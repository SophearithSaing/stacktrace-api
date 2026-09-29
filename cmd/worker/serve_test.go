package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

// workerTestStore opens an isolated test schema and migrates it. The schema is
// dropped on test cleanup, matching the repository's isolation contract.
func workerTestStore(t *testing.T) (*postgres.Store, string) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	adminDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminDB.Close() })
	schema := "test_" + strings.ReplaceAll(string(app.NewID()), "-", "")
	if _, err := adminDB.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal("could not create isolated test schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminDB.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("could not remove isolated test schema")
		}
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := postgres.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store, parsed.String()
}

func ephemeralAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func serveTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func makeHumanPostJob(t *testing.T, store *postgres.Store, agentID app.ID) app.GenerationJob {
	t.Helper()
	ctx := context.Background()
	now := app.GenerationInstant(time.Now().UTC())
	handle := fmt.Sprintf("actor_%d", now.UnixNano())
	auth := app.NewAuth(store)
	account, session, err := auth.Register(ctx, handle, "correct horse battery staple", "Actor", "")
	if err != nil {
		t.Fatal(err)
	}
	actorID := account.ID
	creation, err := app.NewPostCreation("source", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, err := store.CreatePost(ctx, app.SessionHash(session), "post-key", creation)
	if err != nil {
		t.Fatal(err)
	}
	triggerKey, _ := app.SocialGenerationKey(app.TriggerHumanPost, post.ID)
	cooldownKey, _ := app.GenerationCooldownKey(actorID, agentID, post.ID, app.TriggerHumanPost)
	id := app.NewID()
	job := app.GenerationJob{
		ID: id, AgentID: agentID, PersonaVersion: 1, TriggerKind: app.TriggerHumanPost,
		TriggerKey: triggerKey, TriggerActorID: &actorID, CooldownKey: cooldownKey,
		SourcePostID: &post.ID, OutputKind: app.OutputReply, RootJobID: id,
		MaxChainDepth: 2, MaxChainJobs: 5, Status: app.JobPending,
		AvailableAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
	if err := store.CreateGenerationJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job
}

func makeAgentAndPolicy(t *testing.T, store *postgres.Store) app.ID {
	t.Helper()
	ctx := context.Background()
	now := app.GenerationInstant(time.Now().UTC())
	agentID := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: agentID, Type: app.AccountAgent, Handle: fmt.Sprintf("agent_%d", now.UnixNano()), DisplayName: "Agent", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePersona(ctx, app.Persona{AgentID: agentID, Version: 1, Instructions: "x", TopicTags: []string{"go"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	policy := app.GenerationPolicy{
		Version: 1, Timezone: "UTC", ActiveStart: "00:00", ActiveEnd: "23:59",
		ScheduledMinPerDay: 1, ScheduledMaxPerDay: 2, MinSpacingSeconds: 1,
		ResponseMinDelaySeconds: 1, ResponseMaxDelaySeconds: 2, SourceMaxAgeSeconds: 86400,
		ReplyProbabilityBPS: 0, RepostProbabilityBPS: 0, QuoteProbabilityBPS: 0,
		HumanPostProbabilityBPS: 10000, ContinuationProbabilityBPS: 0, CooldownSeconds: 1,
		ScheduledPostCapPerDay: 10, ReplyCapPerDay: 10, ReplyCapPerConversation: 10,
		MaxAgentsPerTrigger: 1, HumanTriggerCapPerWindow: 100, HumanTriggerWindowSeconds: 1,
		MaxChainDepth: 2, MaxChainJobs: 5, DailyTokenBudget: 500000,
	}
	if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: agentID, PersonaVersion: 1, Enabled: true, Policy: policy, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return agentID
}

func TestServeStateReady(t *testing.T) {
	store, _ := workerTestStore(t)
	state := &serveState{}
	if state.Ready(context.Background(), store) {
		t.Fatal("ready before first cycle")
	}
	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{})
	if !state.Ready(context.Background(), store) {
		t.Fatal("not ready after healthy cycle")
	}
	state.recordCompletedCycle(time.Now().Add(-serveProgressStaleness-1), worker.ExecutionSummary{})
	if state.Ready(context.Background(), store) {
		t.Fatal("ready after staleness threshold")
	}
	state.setShuttingDown()
	if state.Ready(context.Background(), store) {
		t.Fatal("ready during shutdown")
	}
}

func TestServeStateStorageUnhealthy(t *testing.T) {
	store, _ := workerTestStore(t)
	state := &serveState{}
	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{})
	if !state.Ready(context.Background(), store) {
		t.Fatal("not ready after healthy cycle")
	}
	state.markStorageUnhealthy()
	if state.Ready(context.Background(), store) {
		t.Fatal("ready after storage error")
	}
}

// TestServeProviderReadiness asserts the actual readiness probe result, not just
// an internal flag: a failure keeps readiness false through empty passes, a mixed
// pass stays degraded, and only a later success-only pass recovers.
func TestServeProviderReadiness(t *testing.T) {
	store, _ := workerTestStore(t)
	ctx := context.Background()
	state := &serveState{}

	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{Calls: 1, CallsSucceeded: 1})
	if !state.Ready(ctx, store) {
		t.Fatal("not ready after success-only pass")
	}

	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{Calls: 1, CallsSucceeded: 0})
	if state.Ready(ctx, store) {
		t.Fatal("ready after failed provider call")
	}

	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{})
	if state.providerDegradedFlag() == false || state.Ready(ctx, store) {
		t.Fatal("empty pass cleared provider degradation")
	}

	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{Calls: 2, CallsSucceeded: 1})
	if state.Ready(ctx, store) {
		t.Fatal("ready after mixed provider outcomes")
	}

	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{Calls: 1, CallsSucceeded: 1})
	if !state.Ready(ctx, store) || state.providerDegradedFlag() {
		t.Fatal("success-only pass did not recover readiness")
	}
}

// TestServeProviderDegradationSurvivesStorageError proves provider evidence
// recorded before a storage failure is retained across a following empty pass.
func TestServeProviderDegradationSurvivesStorageError(t *testing.T) {
	store, _ := workerTestStore(t)
	state := &serveState{}
	state.recordProviderEvidence(worker.ExecutionSummary{Calls: 1, CallsSucceeded: 0})
	state.markStorageUnhealthy()
	// A later empty recovery pass must not clear the earlier provider failure.
	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{})
	if !state.providerDegradedFlag() {
		t.Fatal("storage error lost provider degradation")
	}
	if state.Ready(context.Background(), store) {
		t.Fatal("ready while provider degraded")
	}
}

func TestServeStatusLogCadence(t *testing.T) {
	state := &serveState{}
	now := time.Now()
	if !state.takeStatusLogSlot(now) {
		t.Fatal("first status log slot refused")
	}
	if state.takeStatusLogSlot(now.Add(30 * time.Second)) {
		t.Fatal("status log slot taken too early")
	}
	if !state.takeStatusLogSlot(now.Add(70 * time.Second)) {
		t.Fatal("status log slot refused after interval")
	}
}

// TestServeStatusLogFields covers the actual queue-age fields, null timestamps
// and the safe failure code when the reporting read fails.
func TestServeStatusLogFields(t *testing.T) {
	store, _ := workerTestStore(t)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	if err := logGenerationStatus(context.Background(), store, logger); err != nil {
		t.Fatalf("status snapshot: %v", err)
	}
	line := output.String()
	for _, field := range []string{`"oldest_due"`, `"oldest_failed"`, `"pending"`, `"failed"`, `"charged_tokens"`, `"day"`} {
		if !strings.Contains(line, field) {
			t.Fatalf("status log missing %s: %s", field, line)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := logGenerationStatus(context.Background(), store, logger); err == nil {
		t.Fatal("expected status snapshot failure after close")
	}
	if !strings.Contains(output.String(), codeStatusSnapshot) {
		t.Fatalf("missing safe status failure code: %s", output.String())
	}
}

func TestServeShutdownWithoutCycles(t *testing.T) {
	store, _ := workerTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &fakeProvider{}
	err := serve(ctx, store, provider, ephemeralAddr(t), io.Discard)
	if !errors.Is(err, worker.ExecutionStopped) {
		t.Fatalf("expected stopped, got %v", err)
	}
}

func TestServeBindFailure(t *testing.T) {
	store, _ := workerTestStore(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx := serveTestCtx(t)
	err = serve(ctx, store, &fakeProvider{}, ln.Addr().String(), io.Discard)
	if !errors.Is(err, worker.ExecutionConfiguration) {
		t.Fatalf("expected configuration error for bind failure, got %v", err)
	}
}

func TestServeCycleCallsProviderAndBecomesReady(t *testing.T) {
	store, _ := workerTestStore(t)
	ctx, cancel := context.WithCancel(serveTestCtx(t))

	agentID := makeAgentAndPolicy(t, store)
	makeHumanPostJob(t, store, agentID)

	provider := &fakeProvider{}
	var output bytes.Buffer
	addr := ephemeralAddr(t)
	ready := make(chan struct{})
	var probeWg sync.WaitGroup
	probeWg.Add(1)
	go func() {
		defer probeWg.Done()
		client := &http.Client{Timeout: 2 * time.Second}
		for attempts := 0; attempts < 300; attempts++ {
			resp, err := client.Get("http://" + addr + "/readyz")
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				close(ready)
				return
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("readyz never returned 200")
	}()

	var serveErr error
	var serveWg sync.WaitGroup
	serveWg.Add(1)
	go func() {
		defer serveWg.Done()
		serveErr = serve(ctx, store, provider, addr, &output)
	}()
	// Always cancel and join, even when an assertion fails first.
	t.Cleanup(func() {
		cancel()
		serveWg.Wait()
		probeWg.Wait()
	})

	select {
	case <-ready:
		cancel()
	case <-time.After(30 * time.Second):
		t.Fatal("serve never became ready")
	}
	serveWg.Wait()
	probeWg.Wait()

	if serveErr != nil && !errors.Is(serveErr, worker.ExecutionStopped) {
		t.Fatalf("serve error: %v\noutput:\n%s", serveErr, output.String())
	}
	if provider.calls == 0 {
		t.Fatal("provider was never called")
	}
	if provider.callsSucceeded == 0 {
		t.Fatal("no successful provider outcome recorded")
	}
}

func TestServeFatalProviderErrorStopsService(t *testing.T) {
	store, _ := workerTestStore(t)
	ctx := serveTestCtx(t)

	agentID := makeAgentAndPolicy(t, store)
	makeHumanPostJob(t, store, agentID)

	provider := &fakeProvider{failure: app.GenerationCredentials}
	var output bytes.Buffer
	err := serve(ctx, store, provider, ephemeralAddr(t), &output)
	if !errors.Is(err, worker.ExecutionCredentials) {
		t.Fatalf("expected credentials fatal, got %v\noutput:\n%s", err, output.String())
	}
}

func TestServeCancelDuringProviderCall(t *testing.T) {
	store, _ := workerTestStore(t)
	agentID := makeAgentAndPolicy(t, store)
	job := makeHumanPostJob(t, store, agentID)

	ctx, cancel := context.WithCancel(serveTestCtx(t))

	started := make(chan struct{})
	provider := &fakeProvider{started: started, waitForCancel: true}
	var output bytes.Buffer
	addr := ephemeralAddr(t)
	var serveErr error
	var serveWg sync.WaitGroup
	serveWg.Add(1)
	go func() {
		defer serveWg.Done()
		serveErr = serve(ctx, store, provider, addr, &output)
	}()
	// Cancel and join from cleanup so a failed assertion cannot leak the goroutine.
	t.Cleanup(func() {
		cancel()
		serveWg.Wait()
	})

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("provider call never started")
	}
	cancel()
	serveWg.Wait()

	if serveErr != nil && !errors.Is(serveErr, worker.ExecutionStopped) {
		t.Fatalf("expected stopped, got %v\noutput:\n%s", serveErr, output.String())
	}

	// The cancelled call should have been settled, not left reserved.
	attemptCtx, cancelAttempt := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAttempt()
	attempt, err := store.LatestGenerationAttempt(attemptCtx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt == nil || attempt.Status == app.AttemptReserved {
		t.Fatalf("attempt still reserved after cancellation: %+v", attempt)
	}
	if attempt.ErrorCode != string(app.GenerationCancelled) {
		t.Fatalf("attempt error code after cancellation: got %q want %q", attempt.ErrorCode, app.GenerationCancelled)
	}
}

func TestServeLogsDoNotContainSecrets(t *testing.T) {
	store, databaseURL := workerTestStore(t)
	agentID := makeAgentAndPolicy(t, store)
	makeHumanPostJob(t, store, agentID)

	ctx := serveTestCtx(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	provider := &fakeProvider{failure: app.GenerationCredentials}
	var output bytes.Buffer
	err := serve(ctx, store, provider, ephemeralAddr(t), &output)
	if !errors.Is(err, worker.ExecutionCredentials) {
		t.Fatalf("expected credentials fatal, got %v\noutput:\n%s", err, output.String())
	}
	log := output.String()
	if strings.Contains(log, databaseURL) || strings.Contains(log, "postgres://") {
		t.Fatal("log contains database URL")
	}
	// The provider key is never in scope, but verify no raw error text either.
	if strings.Contains(log, "\xff") || strings.Contains(log, "panic") {
		t.Fatal("log contains raw unsafe text")
	}
	// A raw error field would leak whatever the boundary was handed.
	if strings.Contains(log, `"error":`) {
		t.Fatalf("log contains a raw error field: %s", log)
	}
}

// TestServeSafeErrorCodeWhitelist proves arbitrary typed or raw errors are not
// passed through as codes at the reporting boundary.
func TestServeSafeErrorCodeWhitelist(t *testing.T) {
	for _, code := range []worker.ExecutionError{
		worker.ExecutionStorage, worker.ExecutionStopped, worker.ExecutionCredentials,
		worker.ExecutionConfiguration, worker.ExecutionAccounting,
	} {
		if got := safeErrorCode(code); got != string(code) {
			t.Fatalf("known code %q mapped to %q", code, got)
		}
		if safe, ok := safeExecutionError(code); !ok || safe != code {
			t.Fatalf("known code %q rejected", code)
		}
	}
	if got := safeErrorCode(worker.ExecutionError("sentinel-private-error")); got != string(worker.ExecutionStorage) {
		t.Fatalf("unknown typed code leaked: %q", got)
	}
	if _, ok := safeExecutionError(worker.ExecutionError("sentinel-private-error")); ok {
		t.Fatal("unknown typed code accepted")
	}
	if got := safeErrorCode(errors.New("raw private error")); got != string(worker.ExecutionStorage) {
		t.Fatalf("raw error leaked: %q", got)
	}
	if got := safeErrorCode(nil); got != "" {
		t.Fatalf("nil error mapped to %q", got)
	}
}

// TestServeCycleLogBoundarySanitizesSentinel injects raw and typed sentinel
// errors into the real cycle-log boundary and proves only fixed codes appear.
func TestServeCycleLogBoundarySanitizesSentinel(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logCycleError(logger, worker.ExecutionError("sentinel-private-error"))
	logCycleError(logger, errors.New("raw private provider body"))
	logged := output.String()
	for _, secret := range []string{"sentinel-private-error", "raw private provider body"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("secret leaked into log: %s", logged)
		}
	}
	if !strings.Contains(logged, string(worker.ExecutionStorage)) {
		t.Fatalf("missing fixed code: %s", logged)
	}
}

// TestServeRawStoreErrorIsSanitized closes the store so the cycle hits a raw
// storage error, then proves the loop stops safely without echoing it.
func TestServeRawStoreErrorIsSanitized(t *testing.T) {
	store, _ := workerTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	rawErr := store.Ready(context.Background())
	if rawErr == nil {
		t.Fatal("closed store reported ready")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	writer := &lockedWriter{w: &output}
	logger := slog.New(slog.NewJSONHandler(writer, nil))
	err := serveCycles(ctx, store, &fakeProvider{}, logger, &serveState{}, writer)
	if !errors.Is(err, worker.ExecutionStopped) {
		t.Fatalf("expected stopped, got %v", err)
	}
	logged := output.String()
	if !strings.Contains(logged, string(worker.ExecutionStorage)) {
		t.Fatalf("missing fixed storage code: %s", logged)
	}
	if strings.Contains(logged, rawErr.Error()) {
		t.Fatalf("raw store error leaked: %s", logged)
	}
}

// TestServeStopsWhenLogOutputFails verifies a broken reporter stops the service
// instead of continuing to spend without reporting.
func TestServeStopsWhenLogOutputFails(t *testing.T) {
	store, _ := workerTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output := &lockedWriter{w: failingWriter{err: errors.New("output unavailable")}}
	logger := slog.New(slog.NewJSONHandler(output, nil))
	err := serveCycles(ctx, store, &fakeProvider{}, logger, &serveState{}, output)
	if !errors.Is(err, worker.ExecutionStorage) {
		t.Fatalf("expected storage stop for broken output, got %v", err)
	}
}

// TestServeReportingFailureStopsBeforeProviderCall proves an eligible job is not
// spent on when the reporter fails: zero provider calls and no reserved attempt.
func TestServeReportingFailureStopsBeforeProviderCall(t *testing.T) {
	store, _ := workerTestStore(t)
	agentID := makeAgentAndPolicy(t, store)
	job := makeHumanPostJob(t, store, agentID)

	state := &serveState{}
	output := &lockedWriter{w: failingWriter{err: errors.New("output unavailable")}}
	logger := slog.New(slog.NewJSONHandler(output, nil))
	provider := &fakeProvider{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := serveCycles(ctx, store, provider, logger, state, output)
	if !errors.Is(err, worker.ExecutionStorage) {
		t.Fatalf("expected storage stop for broken output, got %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider called %d times before reporting failed", provider.calls)
	}
	if state.storageHealthyFlag() {
		t.Fatal("reporter failure left service ready")
	}

	attemptCtx, cancelAttempt := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAttempt()
	attempt, attemptErr := store.LatestGenerationAttempt(attemptCtx, job.ID)
	if attemptErr != nil {
		t.Fatal(attemptErr)
	}
	if attempt != nil {
		t.Fatalf("attempt reserved before reporting failed: %+v", attempt)
	}
}

// TestServeCycleStatusFailureReturnsBackoff proves a failed status read is
// returned (so the caller applies storage backoff) and leaves the service unready.
func TestServeCycleStatusFailureReturnsBackoff(t *testing.T) {
	base, _ := workerTestStore(t)
	store := statusFailStore{Store: base}
	state := &serveState{}
	var output bytes.Buffer
	writer := &lockedWriter{w: &output}
	logger := slog.New(slog.NewJSONHandler(writer, nil))

	err := runServeCycle(context.Background(), store, &fakeProvider{}, logger, state, writer)
	if err == nil {
		t.Fatal("status snapshot failure did not propagate")
	}
	if state.storageHealthyFlag() {
		t.Fatal("status snapshot failure left service storage-healthy")
	}
	if state.Ready(context.Background(), store) {
		t.Fatal("ready after status snapshot failure")
	}
}

// TestServeRuntimeListenerFailure closes an already-serving listener to force a
// deterministic runtime Serve failure and proves the service joins safely with a
// fixed error.
func TestServeRuntimeListenerFailure(t *testing.T) {
	store, _ := workerTestStore(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	var serveErr error
	var serveWg sync.WaitGroup
	serveWg.Add(1)
	go func() {
		defer serveWg.Done()
		serveErr = serveListener(ctx, store, &fakeProvider{}, ln, &output)
	}()
	t.Cleanup(func() {
		cancel()
		serveWg.Wait()
	})

	waitForHTTP(t, addr)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, &serveWg, "serve did not join after listener failure")
	if !errors.Is(serveErr, worker.ExecutionConfiguration) {
		t.Fatalf("expected configuration error on listener failure, got %v\noutput:\n%s", serveErr, output.String())
	}
}

// TestServeShutdownLeavesStoreOpen proves serve joins both goroutines and does
// not close the caller-owned store.
func TestServeShutdownLeavesStoreOpen(t *testing.T) {
	store, _ := workerTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := serve(ctx, store, &fakeProvider{}, ephemeralAddr(t), io.Discard)
	if !errors.Is(err, worker.ExecutionStopped) {
		t.Fatalf("expected stopped, got %v", err)
	}
	if err := store.Ready(context.Background()); err != nil {
		t.Fatalf("store unusable after serve returned: %v", err)
	}
}

// TestServeCycleDegradationAndStorageRecovery exercises the real cycle path: a
// failed provider call degrades readiness, and a later empty pass recovers
// storage without clearing provider degradation.
func TestServeCycleDegradationAndStorageRecovery(t *testing.T) {
	store, _ := workerTestStore(t)
	agentID := makeAgentAndPolicy(t, store)
	makeHumanPostJob(t, store, agentID)

	state := &serveState{}
	var output bytes.Buffer
	writer := &lockedWriter{w: &output}
	logger := slog.New(slog.NewJSONHandler(writer, nil))

	if err := runServeCycle(context.Background(), store, &fakeProvider{failure: app.GenerationTimeout}, logger, state, writer); err != nil {
		t.Fatalf("failed cycle: %v", err)
	}
	if !state.providerDegradedFlag() || state.Ready(context.Background(), store) {
		t.Fatal("failed provider call did not degrade readiness")
	}

	state.markStorageUnhealthy()
	if err := runServeCycle(context.Background(), store, &fakeProvider{}, logger, state, writer); err != nil {
		t.Fatalf("recovery cycle: %v", err)
	}
	if !state.storageHealthyFlag() {
		t.Fatal("empty healthy pass did not recover storage")
	}
	if !state.providerDegradedFlag() || state.Ready(context.Background(), store) {
		t.Fatal("storage recovery cleared provider degradation")
	}
}

func waitForHTTP(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("listener never served")
}

func waitGroup(t *testing.T, wg *sync.WaitGroup, message string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal(message)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

// statusFailStore wraps a real store and fails only the bounded status read, so
// the cycle probes the status-failure branch without touching other behavior.
type statusFailStore struct {
	*postgres.Store
}

func (statusFailStore) GenerationStatus(context.Context) (postgres.AdminStatus, error) {
	return postgres.AdminStatus{}, errors.New("status snapshot unavailable")
}

type fakeProvider struct {
	calls          int
	callsSucceeded int
	failure        app.GenerationFailure
	started        chan struct{}
	waitForCancel  bool
}

func (p *fakeProvider) Generate(ctx context.Context, request app.GenerationRequest) app.GenerationOutcome {
	p.calls++
	if p.started != nil {
		close(p.started)
		p.started = nil
	}
	if p.waitForCancel {
		<-ctx.Done()
		return app.GenerationOutcome{Failure: app.GenerationCancelled}
	}
	if p.failure != "" {
		return app.GenerationOutcome{Failure: p.failure}
	}
	raw := []byte(`{"decision":"publish","body":"fake reply"}`)
	result, err := app.DecodeGenerationResult(raw, request.Job)
	if err != nil {
		panic(err)
	}
	p.callsSucceeded++
	input, output := int64(10), int64(10)
	return app.GenerationOutcome{Result: result, InputTokens: &input, OutputTokens: &output, ProviderRequestID: "fake-request"}
}
