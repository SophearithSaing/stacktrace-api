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
	serveStatusLogInterval   = time.Minute
	serveReadyDBTimeout      = 5 * time.Second

	// Fixed, safe log codes. Unknown errors and payloads never reach the log.
	codeListenerFailure = "listener_failure"
	codeShutdownFailure = "listener_shutdown"
	codeStatusSnapshot  = "status_snapshot"
	codeLogOutput       = "log_output_failure"
)

// lockedWriter serializes writes so the cycle goroutine and lifecycle goroutine
// do not race on the shared output stream. It records the first write failure so
// the service can stop rather than keep spending with a broken reporter.
type lockedWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// Write writes bytes to the wrapped writer while recording the result.
func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	n, err := lw.w.Write(p)
	if err != nil && lw.err == nil {
		lw.err = err
	}
	return n, err
}

// failed reports whether the wrapped writer has encountered an error.
func (lw *lockedWriter) failed() bool {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.err != nil
}

// serveStore is the minimal store boundary the service needs: the executor
// contract plus the one scheduling call and the bounded status read. The real
// *postgres.Store satisfies it; tests inject narrow fakes without a production
// selector or new reporting framework.
type serveStore interface {
	worker.ExecutionStore
	ScheduleGeneration(context.Context) (postgres.GenerationScheduleResult, error)
	GenerationStatus(context.Context) (postgres.AdminStatus, error)
}

// serveState tracks readiness for the infrastructure probe. It is safe for
// concurrent access from the HTTP handler and the cycle goroutine.
type serveState struct {
	mu                 sync.RWMutex
	shuttingDown       bool
	storageHealthy     bool
	providerDegraded   bool
	lastCompletedCycle time.Time
	lastStatusLog      time.Time
}

// setShuttingDown marks the worker as shutting down.
func (s *serveState) setShuttingDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuttingDown = true
}

// markStorageUnhealthy marks worker storage as unhealthy.
func (s *serveState) markStorageUnhealthy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storageHealthy = false
}

// recordCompletedCycle records a successfully completed worker cycle.
func (s *serveState) recordCompletedCycle(now time.Time, summary worker.ExecutionSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCompletedCycle = now
	s.storageHealthy = true
	s.recordProviderEvidenceLocked(summary)
}

// recordProviderEvidence preserves provider degradation even when the pass
// later returned a storage error, so a following empty pass cannot lose it.
func (s *serveState) recordProviderEvidence(summary worker.ExecutionSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordProviderEvidenceLocked(summary)
}

// recordProviderEvidenceLocked updates provider health from a cycle summary while the state lock is held.
func (s *serveState) recordProviderEvidenceLocked(summary worker.ExecutionSummary) {
	if summary.Calls > 0 {
		s.providerDegraded = summary.CallsSucceeded != summary.Calls
	}
}

// providerDegradedFlag reports the worker's provider-degradation state.
func (s *serveState) providerDegradedFlag() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providerDegraded
}

// storageHealthyFlag reports the worker's storage-health state.
func (s *serveState) storageHealthyFlag() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storageHealthy
}

// takeStatusLogSlot claims the current rate-limited status-log slot.
func (s *serveState) takeStatusLogSlot(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.lastStatusLog) < serveStatusLogInterval {
		return false
	}
	s.lastStatusLog = now
	return true
}

// readyLocked reports local readiness. A degraded provider is not ready until a
// later success-only pass clears it; an initially unknown provider (no calls
// observed yet) may be ready after a healthy empty pass.
func (s *serveState) readyLocked(now time.Time) bool {
	if s.shuttingDown || !s.storageHealthy || s.providerDegraded || s.lastCompletedCycle.IsZero() {
		return false
	}
	return now.Sub(s.lastCompletedCycle) <= serveProgressStaleness
}

// Ready reports whether the worker and its storage are ready to serve traffic.
func (s *serveState) Ready(ctx context.Context, store serveStore) bool {
	s.mu.RLock()
	if !s.readyLocked(time.Now()) {
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

// serve binds the infrastructure listener synchronously and runs the bounded
// cycle loop. Binding before any work makes a bad address a safe, deterministic
// failure instead of spending after an unusable listener.
func serve(ctx context.Context, store serveStore, provider app.GenerationProvider, addr string, output io.Writer) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(&lockedWriter{w: output}, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logger.Error("serve bind failed", slog.String("error_code", codeListenerFailure))
		return worker.ExecutionConfiguration
	}
	return serveListener(ctx, store, provider, ln, output)
}

// serveListener runs the bounded cycle loop and infrastructure HTTP server for
// an already-bound listener. It returns only after both the cycle loop and the
// HTTP server have been cancelled and joined, so the caller may close the store.
func serveListener(ctx context.Context, store serveStore, provider app.GenerationProvider, ln net.Listener, output io.Writer) error {
	outputWriter := &lockedWriter{w: output}
	logger := slog.New(slog.NewJSONHandler(outputWriter, &slog.HandlerOptions{Level: slog.LevelInfo}))
	state := &serveState{}

	infra := &api.Infrastructure{
		Ready: func(rctx context.Context) bool {
			return state.Ready(rctx, store)
		},
	}
	server := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           infra.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverResult := make(chan error, 1)
	go func() {
		err := server.Serve(ln)
		if err != nil && errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverResult <- err
	}()

	cycleCtx, stopCycles := context.WithCancel(ctx)
	cycleResult := make(chan error, 1)
	go func() {
		cycleResult <- serveCycles(cycleCtx, store, provider, logger, state, outputWriter)
	}()

	// Receive each goroutine result at most once. Do not read serverResult again
	// in the join below unless it was not already received here.
	var cycleErr, listenerErr error
	var cycleJoined, listenerJoined bool
	select {
	case <-ctx.Done():
	case cycleErr = <-cycleResult:
		cycleJoined = true
		state.setShuttingDown()
		logger.Error("serve cycle loop stopped", slog.String("error_code", safeErrorCode(cycleErr)))
	case listenerErr = <-serverResult:
		listenerJoined = true
		state.setShuttingDown()
		logger.Error("serve infrastructure listener stopped", slog.String("error_code", codeListenerFailure))
	}

	state.setShuttingDown()
	stopCycles()

	shutdownFailed := false
	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveHTTPShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("serve http shutdown failed, forcing close", slog.String("error_code", codeShutdownFailure))
		if closeErr := server.Close(); closeErr != nil {
			logger.Error("serve http close failed", slog.String("error_code", codeShutdownFailure))
		}
		shutdownFailed = true
	}

	// Await the bounded executor cleanup rather than abandoning the cycle. The
	// executor settles uncertainty within its own fixed budget, so this join is
	// finite and lets the caller close the database safely afterwards.
	if !listenerJoined {
		listenerErr = <-serverResult
	}
	if !cycleJoined {
		cycleErr = <-cycleResult
	}

	switch {
	case listenerErr != nil:
		logger.Error("serve infrastructure listener returned error", slog.String("error_code", codeListenerFailure))
		return worker.ExecutionConfiguration
	case shutdownFailed:
		return worker.ExecutionConfiguration
	case cycleErr != nil:
		if safe, ok := safeExecutionError(cycleErr); ok {
			return safe
		}
		return worker.ExecutionStorage
	}
	return nil
}

// serveCycles runs worker cycles until cancellation or a fatal failure.
func serveCycles(ctx context.Context, store serveStore, provider app.GenerationProvider, logger *slog.Logger, state *serveState, output *lockedWriter) error {
	for {
		if err := ctx.Err(); err != nil {
			return worker.ExecutionStopped
		}
		// Never start a new pass with a reporter that already failed.
		if output.failed() {
			return reporterFailure(logger, state)
		}

		cycleErr := runServeCycle(ctx, store, provider, logger, state, output)
		if output.failed() {
			return reporterFailure(logger, state)
		}
		if cycleErr == nil {
			select {
			case <-time.After(serveCycleInterval):
				continue
			case <-ctx.Done():
				return worker.ExecutionStopped
			}
		}

		if safe, ok := safeExecutionError(cycleErr); ok {
			switch safe {
			case worker.ExecutionCredentials, worker.ExecutionConfiguration, worker.ExecutionAccounting:
				logger.Error("serve fatal provider error, stopping",
					slog.String("error_code", string(safe)))
				return safe
			}
		}

		logCycleError(logger, cycleErr)
		// Do not back off and cycle again if that failure line could not be written.
		if output.failed() {
			return reporterFailure(logger, state)
		}
		state.markStorageUnhealthy()
		select {
		case <-time.After(serveFailureBackoff):
			continue
		case <-ctx.Done():
			return worker.ExecutionStopped
		}
	}
}

// reporterFailure marks the service unready and returns the fixed safe code for
// an unusable log reporter, so no further work is scheduled or spent.
func reporterFailure(logger *slog.Logger, state *serveState) error {
	logger.Error("serve log output failed, stopping", slog.String("error_code", codeLogOutput))
	state.markStorageUnhealthy()
	return worker.ExecutionStorage
}

// runServeCycle runs one worker cycle and records its health evidence.
func runServeCycle(ctx context.Context, store serveStore, provider app.GenerationProvider, logger *slog.Logger, state *serveState, output *lockedWriter) error {
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
	// Fail before any provider call if the reporter cannot record this pass.
	if output.failed() {
		state.markStorageUnhealthy()
		return worker.ExecutionStorage
	}

	execCtx, cancel := context.WithTimeout(ctx, worker.ExecutionTimeout)
	summary, execErr := worker.Execute(execCtx, store, provider)
	cancel()

	// Record provider evidence from the executed pass even when a later storage
	// error stops the cycle, so a failed call is never silently forgotten.
	state.recordProviderEvidence(summary)

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
		if safe, ok := safeExecutionError(execErr); ok {
			switch safe {
			case worker.ExecutionCredentials, worker.ExecutionConfiguration, worker.ExecutionAccounting,
				worker.ExecutionStorage, worker.ExecutionStopped:
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
		if err := logGenerationStatus(ctx, store, logger); err != nil {
			// A failed reporting read is a storage degradation: leave the completed
			// cycle unready and return the error so the caller applies the storage
			// backoff rather than a normal inter-cycle wait.
			state.markStorageUnhealthy()
			return err
		}
	}
	logger.Info("serve cycle complete",
		slog.Duration("duration", time.Since(start)))
	return nil
}

// logGenerationStatus writes one bounded queue-age/usage snapshot. It returns a
// non-nil error when the read fails so the caller can surface storage degradation.
func logGenerationStatus(ctx context.Context, store serveStore, logger *slog.Logger) error {
	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := store.GenerationStatus(statusCtx)
	if err != nil {
		logger.Error("serve status snapshot unavailable", slog.String("error_code", codeStatusSnapshot))
		return err
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
		slog.String("oldest_due", formatStatusTime(status.Queue.OldestDue)),
		slog.String("oldest_failed", formatStatusTime(status.Queue.OldestFailed)),
		slog.String("day", status.TodayUsage.Day),
		slog.Int("attempts", status.TodayUsage.Attempts),
		slog.Int64("known_tokens", status.TodayUsage.KnownTokens),
		slog.Int64("charged_tokens", status.TodayUsage.ChargedTokens))
	return nil
}

// formatStatusTime formats an optional status timestamp for structured logging.
func formatStatusTime(stamp *time.Time) string {
	if stamp == nil {
		return ""
	}
	return stamp.UTC().Format(time.RFC3339)
}

// logCycleError is the single boundary where a cycle failure reaches the log.
// Only the fixed code is emitted; raw errors and payloads never cross.
func logCycleError(logger *slog.Logger, err error) {
	logger.Error("serve transient cycle error",
		slog.String("error_code", safeErrorCode(err)),
		slog.Duration("backoff", serveFailureBackoff))
}

// safeExecutionError returns the fixed code only for known executor outcomes.
// Arbitrary ExecutionError strings and raw errors are never returned.
func safeExecutionError(err error) (worker.ExecutionError, bool) {
	var safe worker.ExecutionError
	if !errors.As(err, &safe) {
		return "", false
	}
	switch safe {
	case worker.ExecutionStorage, worker.ExecutionStopped, worker.ExecutionCredentials,
		worker.ExecutionConfiguration, worker.ExecutionAccounting:
		return safe, true
	}
	return "", false
}

// safeErrorCode returns an allowlisted worker error code.
func safeErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if safe, ok := safeExecutionError(err); ok {
		return string(safe)
	}
	return string(worker.ExecutionStorage)
}
