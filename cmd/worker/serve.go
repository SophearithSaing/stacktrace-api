package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
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
	serveCleanupTimeout      = 5 * time.Second
	serveProgressStaleness   = worker.ExecutionTimeout + 35*time.Second + serveCycleInterval + serveCleanupTimeout + 5*time.Second
	serveHTTPShutdownTimeout = 10 * time.Second
	serveCycleJoinTimeout    = worker.ExecutionTimeout + serveCleanupTimeout + 10*time.Second
	serveStatusLogInterval   = time.Minute
	serveReadyDBTimeout      = 5 * time.Second
)

// lockedWriter serializes writes so the cycle goroutine and lifecycle goroutine
// do not race on the shared output stream.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// serveState tracks readiness for the infrastructure probe. It is safe for
// concurrent access from the HTTP handler and the cycle goroutine.
type serveState struct {
	mu                 sync.RWMutex
	ready              bool
	shuttingDown       bool
	storageHealthy     bool
	providerDegraded   bool
	lastCompletedCycle time.Time
	lastStatusLog      time.Time
}

func (s *serveState) setShuttingDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuttingDown = true
	s.ready = false
}

func (s *serveState) markStorageUnhealthy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storageHealthy = false
	s.ready = false
}

func (s *serveState) recordCompletedCycle(now time.Time, summary worker.ExecutionSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCompletedCycle = now
	s.storageHealthy = true
	if !s.shuttingDown {
		s.ready = true
	}
	if summary.Calls > 0 {
		s.providerDegraded = summary.CallsSucceeded != summary.Calls
	}
}

func (s *serveState) providerDegradedFlag() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providerDegraded
}

func (s *serveState) takeStatusLogSlot(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.lastStatusLog) < serveStatusLogInterval {
		return false
	}
	s.lastStatusLog = now
	return true
}

func (s *serveState) readyLocked(now time.Time) bool {
	if s.shuttingDown || !s.storageHealthy || s.lastCompletedCycle.IsZero() {
		return false
	}
	return now.Sub(s.lastCompletedCycle) <= serveProgressStaleness
}

func (s *serveState) Ready(ctx context.Context, store *postgres.Store) bool {
	s.mu.RLock()
	now := time.Now()
	if !s.readyLocked(now) {
		s.mu.RUnlock()
		return false
	}
	s.mu.RUnlock()

	dbCtx, cancel := context.WithTimeout(ctx, serveReadyDBTimeout)
	defer cancel()
	if err := store.Ready(dbCtx); err != nil {
		return false
	}

	// Recheck local state after the DB probe to avoid reporting ready after a
	// concurrent shutdown or error.
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readyLocked(time.Now())
}

// serve runs bounded schedule/execute cycles with a separate infrastructure
// listener. It returns only after shutdown has joined the cycle and HTTP server.
// Fatal provider errors stop the service; transient storage errors use a bounded
// backoff and remain observable through readiness.
func serve(ctx context.Context, store *postgres.Store, provider app.GenerationProvider, addr string, output io.Writer) error {
	outputWriter := &lockedWriter{w: output}
	logger := slog.New(slog.NewJSONHandler(outputWriter, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

	// Bind synchronously so the caller gets a deterministic, safe failure code.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("serve bind failed", slog.String("error_code", "listener_failure"))
		return worker.ExecutionConfiguration
	}

	serverErr := make(chan error, 1)
	go func() {
		err := server.Serve(ln)
		if err != nil && errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErr <- err
	}()

	cycleCtx, stopCycles := context.WithCancel(ctx)
	cycleErr := make(chan error, 1)
	go func() {
		cycleErr <- serveCycles(cycleCtx, store, provider, logger, state)
	}()

	var cycleResult error
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil {
			logger.Error("serve infrastructure listener failed", slog.String("error_code", "listener_failure"))
		}
		stopCycles()
	case err := <-cycleErr:
		cycleResult = err
		state.setShuttingDown()
		logger.Error("serve cycle loop stopped", slog.String("error_code", safeErrorCode(err)))
	}

	state.setShuttingDown()
	stopCycles()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveHTTPShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("serve http shutdown failed, forcing close", slog.String("error_code", "listener_shutdown"))
		server.Close()
	}

	if cycleResult == nil {
		select {
		case cycleResult = <-cycleErr:
		case <-time.After(serveCycleJoinTimeout):
			logger.Error("serve cycle shutdown timed out", slog.String("error_code", "cycle_join_timeout"))
		}
	}

	serverResult := <-serverErr
	if serverResult != nil {
		logger.Error("serve infrastructure listener returned error", slog.String("error_code", "listener_failure"))
		return worker.ExecutionConfiguration
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

func serveCycles(ctx context.Context, store *postgres.Store, provider app.GenerationProvider, logger *slog.Logger, state *serveState) error {
	for {
		if err := ctx.Err(); err != nil {
			return worker.ExecutionStopped
		}

		cycleErr := runServeCycle(ctx, store, provider, logger, state)
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
				logger.Error("serve fatal provider error, stopping",
					slog.String("error_code", string(fatal)))
				return fatal
			}
		}

		logger.Error("serve transient cycle error, backing off",
			slog.String("error_code", safeErrorCode(cycleErr)),
			slog.Duration("backoff", serveFailureBackoff))
		state.markStorageUnhealthy()
		select {
		case <-time.After(serveFailureBackoff):
			continue
		case <-ctx.Done():
			return worker.ExecutionStopped
		}
	}
}

func runServeCycle(ctx context.Context, store *postgres.Store, provider app.GenerationProvider, logger *slog.Logger, state *serveState) error {
	start := time.Now()
	scheduleCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	result, err := schedule(scheduleCtx, store)
	cancel()
	if err != nil {
		return err
	}
	logger.Info("serve schedule complete",
		slog.Int("agents_visited", result.AgentsVisited),
		slog.Int("invalid_agents", result.InvalidAgents),
		slog.Int("jobs_enqueued", result.JobsEnqueued),
		slog.Int("slots_denied", result.SlotsDenied),
		slog.Int("jobs_expired", result.JobsExpired))

	execCtx, cancel := context.WithTimeout(ctx, worker.ExecutionTimeout)
	summary, execErr := worker.Execute(execCtx, store, provider)
	cancel()
	logger.Info("serve execution result",
		slog.Int("probes", summary.Probes),
		slog.Int("claimed", summary.Claimed),
		slog.Int("calls", summary.Calls),
		slog.Int("calls_succeeded", summary.CallsSucceeded),
		slog.Int("recovered", summary.Recovered),
		slog.Int("expired", summary.Expired),
		slog.Int("published", summary.Published),
		slog.Int("skipped", summary.Skipped),
		slog.Int("cancelled", summary.Cancelled),
		slog.Int("failed", summary.Failed),
		slog.Int("retried", summary.Retried),
		slog.Int("denied", summary.Denied),
		slog.String("error_code", safeErrorCode(execErr)))

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

	state.recordCompletedCycle(time.Now(), summary)
	if state.providerDegradedFlag() {
		logger.Info("serve provider degradation persists",
			slog.Bool("provider_degraded", true),
			slog.String("recovery_rule", "success-only pass clears degradation"))
	}
	if state.takeStatusLogSlot(time.Now()) {
		logGenerationStatus(ctx, store, logger)
	}
	logger.Info("serve cycle complete",
		slog.Duration("duration", time.Since(start)))
	return nil
}

func logGenerationStatus(ctx context.Context, store *postgres.Store, logger *slog.Logger) {
	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := store.GenerationStatus(statusCtx)
	if err != nil {
		logger.Error("serve status snapshot unavailable", slog.String("error_code", "status_snapshot"))
		return
	}
	logger.Info("serve status snapshot",
		slog.Int("configured", status.Agent.Configured),
		slog.Int("enabled", status.Agent.Enabled),
		slog.Int("pending", status.Queue.Pending),
		slog.Int("retry_wait", status.Queue.RetryWait),
		slog.Int("running", status.Queue.Running),
		slog.Int("succeeded", status.Queue.Succeeded),
		slog.Int("skipped", status.Queue.Skipped),
		slog.Int("cancelled", status.Queue.Cancelled),
		slog.Int("failed", status.Queue.Failed),
		slog.String("day", status.TodayUsage.Day),
		slog.Int("attempts", status.TodayUsage.Attempts),
		slog.Int64("known_tokens", status.TodayUsage.KnownTokens),
		slog.Int64("charged_tokens", status.TodayUsage.ChargedTokens))
}

func safeErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var safe worker.ExecutionError
	if errors.As(err, &safe) {
		return string(safe)
	}
	return "execution_storage"
}
