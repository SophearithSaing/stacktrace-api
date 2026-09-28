package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/api"
	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

const (
	serveCycleInterval       = 5 * time.Second
	serveFailureBackoff      = 30 * time.Second
	serveProgressStaleness   = worker.ExecutionTimeout + 30*time.Second + serveCycleInterval
	serveReadyDBTimeout      = 5 * time.Second
	serveShutdownHTTPTimeout = 10 * time.Second
)

// serveState tracks readiness for the infrastructure probe. It is safe for
// concurrent access from the HTTP handler and the cycle goroutine.
type serveState struct {
	mu               sync.RWMutex
	ready            bool
	shuttingDown     bool
	lastHealthyCycle time.Time
	providerHealthy  bool
}

func (s *serveState) setShuttingDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuttingDown = true
	s.ready = false
}

func (s *serveState) recordHealthyCycle(now time.Time, providerSuccess bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHealthyCycle = now
	s.ready = true
	if providerSuccess {
		s.providerHealthy = true
	}
}

func (s *serveState) setProviderDegraded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerHealthy = false
}

func (s *serveState) readyLocked() bool {
	if s.shuttingDown || !s.ready || s.lastHealthyCycle.IsZero() {
		return false
	}
	return time.Since(s.lastHealthyCycle) <= serveProgressStaleness
}

func (s *serveState) Ready(ctx context.Context, store *postgres.Store) bool {
	s.mu.RLock()
	if !s.readyLocked() {
		s.mu.RUnlock()
		return false
	}
	s.mu.RUnlock()

	dbCtx, cancel := context.WithTimeout(ctx, serveReadyDBTimeout)
	defer cancel()
	if err := store.Ready(dbCtx); err != nil {
		return false
	}
	return true
}

// serve runs bounded schedule/execute cycles with a separate infrastructure
// listener. It returns only after shutdown has joined the cycle and HTTP server
// and closed the store. Fatal provider errors stop the service; transient
// storage errors use a bounded backoff and remain observable through readiness.
func serve(ctx context.Context, store *postgres.Store, provider app.GenerationProvider, addr string, output io.Writer) error {
	state := &serveState{}

	infra := &api.Infrastructure{
		Ready: func(rctx context.Context) bool {
			return state.Ready(rctx, store)
		},
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           infra.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if err != nil && errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErr <- err
	}()

	cycleErr := make(chan error, 1)
	cycleCtx, stopCycles := context.WithCancel(ctx)
	go func() {
		cycleErr <- serveCycles(cycleCtx, store, provider, output, state)
	}()

	var cycleResult error
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		fmt.Fprintf(output, "serve: infrastructure listener failed: %v\n", err)
		stopCycles()
	case err := <-cycleErr:
		fmt.Fprintf(output, "serve: cycle loop stopped: %v\n", err)
		cycleResult = err
		state.setShuttingDown()
	}

	state.setShuttingDown()
	stopCycles()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownHTTPTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(output, "serve: http shutdown: %v\n", err)
	}

	if cycleResult == nil {
		select {
		case cycleResult = <-cycleErr:
		case <-time.After(serveShutdownHTTPTimeout):
			fmt.Fprintln(output, "serve: cycle shutdown timed out")
		}
	}

	serverResult := <-serverErr
	if serverResult != nil {
		return fmt.Errorf("serve: infrastructure listener: %w", serverResult)
	}
	if cycleResult != nil {
		var safe worker.ExecutionError
		if errors.As(cycleResult, &safe) {
			return safe
		}
		return worker.ExecutionStorage
	}
	return nil
}

func serveCycles(ctx context.Context, store *postgres.Store, provider app.GenerationProvider, output io.Writer, state *serveState) error {
	for {
		if err := ctx.Err(); err != nil {
			return worker.ExecutionStopped
		}

		start := time.Now()
		cycleErr := runServeCycle(ctx, store, provider, output, state, start)
		if cycleErr == nil {
			select {
			case <-time.After(serveCycleInterval):
				continue
			case <-ctx.Done():
				return worker.ExecutionStopped
			}
		}

		var fatal worker.ExecutionError
		if errors.As(cycleErr, &fatal) {
			switch fatal {
			case worker.ExecutionCredentials, worker.ExecutionConfiguration, worker.ExecutionAccounting:
				fmt.Fprintf(output, "serve: fatal provider error %s; stopping\n", fatal)
				return fatal
			}
		}

		fmt.Fprintf(output, "serve: transient cycle error %v; backing off %s\n", cycleErr, serveFailureBackoff)
		state.setProviderDegraded()
		select {
		case <-time.After(serveFailureBackoff):
			continue
		case <-ctx.Done():
			return worker.ExecutionStopped
		}
	}
}

func runServeCycle(ctx context.Context, store *postgres.Store, provider app.GenerationProvider, output io.Writer, state *serveState, start time.Time) error {
	scheduleCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	result, err := schedule(scheduleCtx, store)
	cancel()
	if err != nil {
		return err
	}
	reportSchedule(output, result, nil)

	execCtx, cancel := context.WithTimeout(ctx, worker.ExecutionTimeout)
	summary, execErr := worker.Execute(execCtx, store, provider)
	cancel()
	reportExecution(output, summary, execErr)

	providerSuccess := summary.CallsSucceeded > 0
	if execErr != nil {
		var safe worker.ExecutionError
		if errors.As(execErr, &safe) {
			switch safe {
			case worker.ExecutionCredentials, worker.ExecutionConfiguration, worker.ExecutionAccounting:
				return safe
			case worker.ExecutionStorage, worker.ExecutionStopped:
				return safe
			}
		}
		return execErr
	}
	if summary.Calls > 0 && summary.CallsSucceeded == 0 {
		state.setProviderDegraded()
	}
	state.recordHealthyCycle(start, providerSuccess)
	return nil
}
