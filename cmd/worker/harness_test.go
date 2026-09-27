package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

// This entry exists only in go test -c output. It never constructs Together or
// reads its credentials. The runner supplies an explicit private disposable fixture.
func TestGenerationSmokeHarness(t *testing.T) {
	path := os.Getenv("STACKTRACE_GENERATION_SMOKE")
	if path == "" {
		t.Skip("disposable command-handler smoke only")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || filepath.Base(path) != "execution-command.json" {
		t.Fatal("invalid smoke fixture")
	}
	var fixture struct {
		Mode  string `json:"mode"`
		Calls int    `json:"calls"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &fixture) != nil || (fixture.Mode != "publish" && fixture.Mode != "timeout") || fixture.Calls < 0 || fixture.Calls > 1 {
		t.Fatal("invalid smoke command")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" || url != os.Getenv("TEST_DATABASE_URL") {
		t.Fatal("smoke requires disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatal("smoke database unavailable")
	}
	defer store.Close()
	provider := &smokeProvider{t: t, mode: fixture.Mode, expected: fixture.Calls}
	if err := execute(ctx, store, provider, os.Stdout); err != nil {
		t.Fatal("smoke execution failed")
	}
	if provider.calls != fixture.Calls {
		t.Fatal("unexpected fake provider call count")
	}
	if provider.calls == 1 {
		attempt, err := store.LatestGenerationAttempt(ctx, provider.job.ID)
		if err != nil || attempt == nil || attempt.LeaseVersion != provider.job.LeaseVersion || attempt.ProviderRequestID != "smoke-request" {
			t.Fatal("missing exact attempt provenance")
		}
		if fixture.Mode == "publish" && (attempt.OutputDigest != provider.digest || attempt.AccountedTokens() != 120) {
			t.Fatal("incorrect output binding or settlement")
		}
	}
}

type smokeProvider struct {
	t        *testing.T
	mode     string
	expected int
	calls    int
	job      app.GenerationJob
	digest   string
}

func (p *smokeProvider) Generate(_ context.Context, request app.GenerationRequest) app.GenerationOutcome {
	p.calls++
	if p.calls > p.expected {
		p.t.Fatal("unexpected fake provider call")
	}
	p.job = request.Job
	if p.mode == "timeout" {
		return app.GenerationOutcome{Failure: app.GenerationTimeout, NotBefore: time.Now().Add(30 * time.Minute), ProviderRequestID: "smoke-request"}
	}
	// Distinct by trigger to exercise repetition checks without arbitrary output.
	body := map[app.GenerationTrigger]string{
		app.TriggerScheduled: "Keep Go handlers small and explicit.",
		app.TriggerReply:     "A focused regression test documents the comment contract.",
		app.TriggerRepost:    "Durable transactions preserve this shared example.",
		app.TriggerQuote:     "Bounded context makes quoted discussions easier to follow.",
	}[request.Job.TriggerKind]
	if body == "" {
		p.t.Fatal("unexpected smoke trigger")
	}
	raw, _ := json.Marshal(map[string]string{"decision": "publish", "body": body})
	result, err := app.DecodeGenerationResult(raw, request.Job)
	if err != nil {
		p.t.Fatal("invalid smoke output")
	}
	p.digest = result.Digest()
	input, output := int64(100), int64(20)
	return app.GenerationOutcome{Result: result, InputTokens: &input, OutputTokens: &output, ProviderRequestID: "smoke-request"}
}
