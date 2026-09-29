package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
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

// TestServeSmokeHarness runs the approved continuous serve orchestration against a
// disposable database with a deterministic, file-controlled fake provider. It
// exists only in go test -c output, never constructs Together, and refuses a
// provider key. The runner controls failure mode/blocking and graceful stop via
// the private control file and signal handling.
func TestServeSmokeHarness(t *testing.T) {
	path := os.Getenv("STACKTRACE_SERVE_SMOKE")
	if path == "" {
		t.Skip("disposable serve smoke only")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || filepath.Base(path) != "serve-command.json" {
		t.Fatal("invalid serve smoke control file")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" || url != os.Getenv("TEST_DATABASE_URL") {
		t.Fatal("serve smoke requires disposable database")
	}
	if os.Getenv("TOGETHER_API_KEY") != "" {
		t.Fatal("serve smoke must not receive a provider key")
	}
	addr := os.Getenv("WORKER_HTTP_ADDR")
	if addr == "" {
		t.Fatal("serve smoke requires WORKER_HTTP_ADDR")
	}

	// Graceful stop: a managed TERM cancels the service context and joins both
	// the cycle goroutine and the infrastructure listener.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatal("serve smoke database unavailable")
	}
	defer store.Close()

	provider := &serveSmokeProvider{
		control: path,
		started: os.Getenv("STACKTRACE_SERVE_STARTED"),
		release: os.Getenv("STACKTRACE_SERVE_RELEASE"),
		counts:  map[app.GenerationTrigger]int{},
	}
	if err := serve(ctx, store, provider, addr, os.Stdout); err != nil && !errors.Is(err, worker.ExecutionStopped) {
		t.Fatal("serve smoke failed")
	}
}

type serveSmokeControl struct {
	Mode  string `json:"mode"`
	Block bool   `json:"block"`
}

func readServeControl(path string) serveSmokeControl {
	data, err := os.ReadFile(path)
	if err != nil {
		return serveSmokeControl{Mode: "publish"}
	}
	var control serveSmokeControl
	if json.Unmarshal(data, &control) != nil || control.Mode == "" {
		return serveSmokeControl{Mode: "publish"}
	}
	return control
}

type serveSmokeProvider struct {
	control string
	started string
	release string
	mu      sync.Mutex
	calls   int
	counts  map[app.GenerationTrigger]int
}

func (p *serveSmokeProvider) Generate(ctx context.Context, request app.GenerationRequest) app.GenerationOutcome {
	p.mu.Lock()
	p.calls++
	kindCount := p.counts[request.Job.TriggerKind]
	p.counts[request.Job.TriggerKind] = kindCount + 1
	p.mu.Unlock()

	control := readServeControl(p.control)
	if control.Block {
		if p.started != "" {
			_ = os.WriteFile(p.started, []byte("started"), 0o600)
		}
		for {
			if _, err := os.Stat(p.release); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return app.GenerationOutcome{Failure: app.GenerationCancelled}
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	if control.Mode == "timeout" {
		return app.GenerationOutcome{Failure: app.GenerationTimeout}
	}
	result, err := app.DecodeGenerationResult(serveSmokeBody(request.Job.TriggerKind, kindCount), request.Job)
	if err != nil {
		return app.GenerationOutcome{Failure: app.GenerationInvalidOutput}
	}
	if control.Mode == "unknown" {
		// No usage counts: the reservation is charged conservatively.
		return app.GenerationOutcome{Result: result, ProviderRequestID: "serve-request"}
	}
	input, output := int64(100), int64(20)
	return app.GenerationOutcome{Result: result, InputTokens: &input, OutputTokens: &output, ProviderRequestID: "serve-request"}
}

// serveSmokeBodies are deliberately word-distinct per call so the safety
// repetition heuristic does not reject legitimate repeated replies.
var serveSmokeBodies = map[app.GenerationTrigger][]string{
	app.TriggerScheduled: {"Continuous workers keep bounded passes explicit."},
	app.TriggerReply: {
		"Set the network deadline from the request context.",
		"Keep exactly one provider call in flight per pass.",
		"Persist reservations before any remote work begins.",
		"Join the worker loop before the database closes.",
		"Report readiness from observed provider outcomes.",
		"Snapshot queue age inside one bounded transaction.",
		"Keep supervised restarts from replaying durable voids.",
		"Charge unknown usage at the reserved token ceiling.",
	},
	app.TriggerRepost: {"Durable reservations preserve this shared example."},
	app.TriggerQuote:  {"Bounded context keeps quoted discussions readable."},
}

func serveSmokeBody(trigger app.GenerationTrigger, index int) []byte {
	bodies := serveSmokeBodies[trigger]
	body := bodies[index%len(bodies)]
	raw, _ := json.Marshal(map[string]string{"decision": "publish", "body": body})
	return raw
}
