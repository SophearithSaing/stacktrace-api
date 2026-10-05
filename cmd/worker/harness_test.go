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
		app.TriggerHumanPost: "Use a context to bound the worker loop and join it during shutdown.",
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
	if _, ok := readServeControl(path); !ok {
		t.Fatal("unreadable serve smoke control")
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
		control:    path,
		started:    os.Getenv("STACKTRACE_SERVE_STARTED"),
		release:    os.Getenv("STACKTRACE_SERVE_RELEASE"),
		countsFile: os.Getenv("STACKTRACE_SERVE_COUNTS"),
	}
	if err := serve(ctx, store, provider, addr, os.Stdout); err != nil && !errors.Is(err, worker.ExecutionStopped) {
		t.Fatal("serve smoke failed")
	}
}

type serveSmokeControl struct {
	Mode  string `json:"mode"`
	Block bool   `json:"block"`
}

// readServeControl fails closed: a missing, malformed, or unsupported control
// file never defaults to a spend/publish behavior.
func readServeControl(path string) (serveSmokeControl, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return serveSmokeControl{}, false
	}
	var control serveSmokeControl
	if json.Unmarshal(data, &control) != nil {
		return serveSmokeControl{}, false
	}
	switch control.Mode {
	case "publish", "timeout":
		return control, true
	default:
		return serveSmokeControl{}, false
	}
}

type serveSmokeProvider struct {
	control    string
	started    string
	release    string
	countsFile string
	mu         sync.Mutex
	calls      int
}

// nextCount returns the next per-trigger body index, persisting it to a shared
// file so a mid-scenario process restart does not reset body sequencing (which
// could otherwise repeat an already-published body and trip repetition checks).
func (p *serveSmokeProvider) nextCount(kind app.GenerationTrigger) int {
	counts := map[string]int{}
	if p.countsFile != "" {
		if data, err := os.ReadFile(p.countsFile); err == nil {
			_ = json.Unmarshal(data, &counts)
		}
	}
	index := counts[string(kind)]
	counts[string(kind)] = index + 1
	if p.countsFile != "" {
		if data, err := json.Marshal(counts); err == nil {
			temporary := p.countsFile + ".tmp"
			if os.WriteFile(temporary, data, 0o600) == nil {
				_ = os.Rename(temporary, p.countsFile)
			}
		}
	}
	return index
}

func (p *serveSmokeProvider) Generate(ctx context.Context, request app.GenerationRequest) app.GenerationOutcome {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	kindCount := p.nextCount(request.Job.TriggerKind)

	control, ok := readServeControl(p.control)
	if !ok {
		// Fail closed: never publish or spend on an unreadable control file.
		return app.GenerationOutcome{Failure: app.GenerationConfiguration}
	}
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
	input, output := int64(100), int64(20)
	return app.GenerationOutcome{Result: result, InputTokens: &input, OutputTokens: &output, ProviderRequestID: "serve-request"}
}

// TestServeSmokeControlFailsClosed proves missing, malformed, and unsupported
// controls are rejected rather than defaulting to publish.
func TestServeSmokeControlFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readServeControl(filepath.Join(dir, "missing.json")); ok {
		t.Fatal("missing control accepted")
	}
	for name, payload := range map[string]string{
		"malformed":   "{",
		"empty":       "",
		"unsupported": `{"mode":"unknown"}`,
		"nomode":      `{"block":true}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readServeControl(path); ok {
			t.Fatalf("%s control accepted", name)
		}
	}
	for _, mode := range []string{"publish", "timeout"} {
		path := filepath.Join(dir, mode+".json")
		if err := os.WriteFile(path, []byte(`{"mode":"`+mode+`","block":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		control, ok := readServeControl(path)
		if !ok || control.Mode != mode || !control.Block {
			t.Fatalf("valid %s control rejected: %+v %v", mode, control, ok)
		}
	}
}

// TestServeSmokeControlAtomicUpdates proves the same-directory temp+rename update
// protocol never exposes a partial or invalid control to a concurrent reader.
func TestServeSmokeControlAtomicUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.json")
	writeAtomic := func(mode string) error {
		tmp := filepath.Join(dir, "control.tmp")
		if err := os.WriteFile(tmp, []byte(`{"mode":"`+mode+`"}`), 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	if err := writeAtomic("publish"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			mode := "publish"
			if i%2 == 1 {
				mode = "timeout"
			}
			if err := writeAtomic(mode); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 5000; i++ {
		if _, ok := readServeControl(path); !ok {
			close(stop)
			wg.Wait()
			t.Fatal("atomic update exposed an invalid control")
		}
	}
	close(stop)
	wg.Wait()
}

// TestServeSmokeBodySequencingPersists proves per-trigger body indices continue
// across provider instances sharing the counts file, so a restart cannot repeat
// a published body.
func TestServeSmokeBodySequencingPersists(t *testing.T) {
	countsFile := filepath.Join(t.TempDir(), "counts.json")
	first := &serveSmokeProvider{countsFile: countsFile}
	second := &serveSmokeProvider{countsFile: countsFile}
	firstIndex := first.nextCount(app.TriggerReply)
	if secondIndex := second.nextCount(app.TriggerReply); secondIndex != firstIndex+1 {
		t.Fatalf("counts did not persist: first=%d second=%d", firstIndex, secondIndex)
	}
	if string(serveSmokeBody(app.TriggerReply, firstIndex)) == string(serveSmokeBody(app.TriggerReply, firstIndex+1)) {
		t.Fatal("consecutive reply bodies repeated")
	}
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
