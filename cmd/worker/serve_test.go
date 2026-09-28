package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
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

func TestServeProviderDegradation(t *testing.T) {
	state := &serveState{}
	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{Calls: 1, CallsSucceeded: 1})
	if state.providerDegradedFlag() {
		t.Fatal("degraded after success-only pass")
	}
	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{Calls: 2, CallsSucceeded: 1})
	if !state.providerDegradedFlag() {
		t.Fatal("not degraded after mixed pass")
	}
	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{})
	if !state.providerDegradedFlag() {
		t.Fatal("empty pass cleared degradation")
	}
	state.recordCompletedCycle(time.Now(), worker.ExecutionSummary{Calls: 1, CallsSucceeded: 1})
	if state.providerDegradedFlag() {
		t.Fatal("degraded after later success-only pass")
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
	ctx := serveTestCtx(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	agentID := makeAgentAndPolicy(t, store)
	makeHumanPostJob(t, store, agentID)

	provider := &fakeProvider{}
	var output bytes.Buffer
	addr := ephemeralAddr(t)
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
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

	select {
	case <-ready:
		cancel()
	case <-time.After(30 * time.Second):
		t.Fatal("serve never became ready")
		cancel()
	}
	serveWg.Wait()
	wg.Wait()

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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	ctx := serveTestCtx(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	select {
	case <-started:
		cancel()
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("provider call never started")
	}
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
