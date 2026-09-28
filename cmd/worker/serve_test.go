package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

func serveTestNow() time.Time {
	return app.GenerationInstant(time.Now().UTC())
}

func workerTestStore(t *testing.T) *postgres.Store {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestServeStateReady(t *testing.T) {
	store := workerTestStore(t)
	state := &serveState{}
	if state.Ready(context.Background(), store) {
		t.Fatal("ready before first cycle")
	}
	state.recordHealthyCycle(time.Now(), false)
	if !state.Ready(context.Background(), store) {
		t.Fatal("not ready after healthy cycle")
	}
	state.recordHealthyCycle(time.Now().Add(-serveProgressStaleness-1), false)
	if state.Ready(context.Background(), store) {
		t.Fatal("ready after staleness threshold")
	}
	state.setShuttingDown()
	if state.Ready(context.Background(), store) {
		t.Fatal("ready during shutdown")
	}
}

func TestServeShutdownWithoutCycles(t *testing.T) {
	store := workerTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &fakeProvider{}
	err := serve(ctx, store, provider, "127.0.0.1:18081", io.Discard)
	if !errors.Is(err, worker.ExecutionStopped) {
		t.Fatalf("expected stopped, got %v", err)
	}
}

func TestServeCycleCallsProviderAndBecomesReady(t *testing.T) {
	store := workerTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := serveTestNow()
	agentID := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: agentID, Type: app.AccountAgent, Handle: fmt.Sprintf("serve_agent_%d", now.UnixNano()), DisplayName: "Serve", CreatedAt: now, UpdatedAt: now}); err != nil {
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
		HumanPostProbabilityBPS: 0, ContinuationProbabilityBPS: 0, CooldownSeconds: 1,
		ScheduledPostCapPerDay: 10, ReplyCapPerDay: 10, ReplyCapPerConversation: 10,
		MaxAgentsPerTrigger: 1, HumanTriggerCapPerWindow: 100, HumanTriggerWindowSeconds: 1,
		MaxChainDepth: 2, MaxChainJobs: 5, DailyTokenBudget: 500000,
	}
	if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: agentID, PersonaVersion: 1, Enabled: true, Policy: policy, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	id := app.NewID()
	slot := now.Add(-time.Second)
	triggerKey, _ := app.ScheduledGenerationKey(slot)
	job := app.GenerationJob{
		ID: id, AgentID: agentID, PersonaVersion: 1, TriggerKind: app.TriggerScheduled,
		TriggerKey: triggerKey, OutputKind: app.OutputPost, RootJobID: id,
		MaxChainDepth: 2, MaxChainJobs: 5, Status: app.JobPending,
		AvailableAt: slot, ExpiresAt: now.Add(time.Hour), CreatedAt: slot,
	}
	if err := store.CreateGenerationJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{}
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		for attempts := 0; attempts < 60; attempts++ {
			resp, err := client.Get("http://127.0.0.1:18081/readyz")
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				cancel()
				return
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("readyz never returned 200")
		cancel()
	}()

	var output bytes.Buffer
	if err := serve(ctx, store, provider, "127.0.0.1:18081", &output); err != nil && !errors.Is(err, worker.ExecutionStopped) {
		t.Fatalf("serve error: %v\noutput:\n%s", err, output.String())
	}
	if provider.calls == 0 {
		t.Fatal("provider was never called")
	}
}

func TestServeFatalProviderErrorStopsService(t *testing.T) {
	store := workerTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := serveTestNow()
	agentID := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: agentID, Type: app.AccountAgent, Handle: fmt.Sprintf("fatal_agent_%d", now.UnixNano()), DisplayName: "Fatal", CreatedAt: now, UpdatedAt: now}); err != nil {
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
		HumanPostProbabilityBPS: 0, ContinuationProbabilityBPS: 0, CooldownSeconds: 1,
		ScheduledPostCapPerDay: 10, ReplyCapPerDay: 10, ReplyCapPerConversation: 10,
		MaxAgentsPerTrigger: 1, HumanTriggerCapPerWindow: 100, HumanTriggerWindowSeconds: 1,
		MaxChainDepth: 2, MaxChainJobs: 5, DailyTokenBudget: 500000,
	}
	if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: agentID, PersonaVersion: 1, Enabled: true, Policy: policy, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	id := app.NewID()
	slot := now.Add(-time.Second)
	triggerKey, _ := app.ScheduledGenerationKey(slot)
	job := app.GenerationJob{
		ID: id, AgentID: agentID, PersonaVersion: 1, TriggerKind: app.TriggerScheduled,
		TriggerKey: triggerKey, OutputKind: app.OutputPost, RootJobID: id,
		MaxChainDepth: 2, MaxChainJobs: 5, Status: app.JobPending,
		AvailableAt: slot, ExpiresAt: now.Add(time.Hour), CreatedAt: slot,
	}
	if err := store.CreateGenerationJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{failure: app.GenerationCredentials}
	var output bytes.Buffer
	err := serve(ctx, store, provider, "127.0.0.1:18082", &output)
	if !errors.Is(err, worker.ExecutionCredentials) {
		t.Fatalf("expected credentials fatal, got %v\noutput:\n%s", err, output.String())
	}
}

type fakeProvider struct {
	calls   int
	failure app.GenerationFailure
}

func (p *fakeProvider) Generate(_ context.Context, request app.GenerationRequest) app.GenerationOutcome {
	p.calls++
	if p.failure != "" {
		return app.GenerationOutcome{Failure: p.failure}
	}
	raw := []byte(`{"decision":"publish","body":"fake post"}`)
	result, err := app.DecodeGenerationResult(raw, request.Job)
	if err != nil {
		panic(err)
	}
	input, output := int64(10), int64(10)
	return app.GenerationOutcome{Result: result, InputTokens: &input, OutputTokens: &output, ProviderRequestID: "fake-request"}
}
