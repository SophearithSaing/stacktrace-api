package llm

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func generationRequest(t *testing.T, instructions string) app.GenerationRequest {
	t.Helper()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	id := app.NewID()
	job := app.GenerationJob{ID: id, AgentID: app.NewID(), PersonaVersion: 1, TriggerKind: app.TriggerScheduled, TriggerKey: "test", OutputKind: app.OutputPost, RootJobID: id, MaxChainDepth: 0, MaxChainJobs: 1, Status: app.JobPending, AvailableAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	persona := app.Persona{AgentID: job.AgentID, Version: 1, Instructions: instructions, TopicTags: []string{"go"}, CreatedAt: now}
	context, err := app.BuildGenerationContext(job, persona, app.PublicGenerationContext{})
	if err != nil {
		t.Fatal(err)
	}
	return app.GenerationRequest{Job: job, Context: context}
}

func TestGenerationPrompt(t *testing.T) {
	request := generationRequest(t, "Discuss Go.")
	first, err := BuildPrompt(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPrompt(request)
	if err != nil || first != second || len(first.Hash()) != 64 {
		t.Fatal("unstable prompt", err)
	}
	if !json.Valid([]byte(first.Schema())) || !strings.Contains(first.System(), first.Schema()) || !strings.Contains(first.System(), request.Context.Instructions()) || !strings.Contains(first.User(), request.Context.PublicJSON()) {
		t.Fatal("missing prompt section")
	}
	changed, err := BuildPrompt(generationRequest(t, "Discuss PostgreSQL."))
	if err != nil || changed.Hash() == first.Hash() {
		t.Fatal("unbound persona", err)
	}
	// Exact UTF-8 byte growth, not runes/4. JSON marshaling is already included.
	multibyte, err := BuildPrompt(generationRequest(t, "Discuss Go.界"))
	if err != nil || multibyte.ReferenceInputTokenBound()-first.ReferenceInputTokenBound() != 3 {
		t.Fatal("not a byte bound", err)
	}
	if first.ReferenceInputTokenBound() != int64(len(first.System())+len(first.User())+len(first.Schema())+34) {
		t.Fatal("framing or schema missing")
	}
	if reservation, err := first.AdmissionReservation(); reservation != 0 || !errors.Is(err, ErrUnverifiedInputBound) {
		t.Fatal("unverified hosted bound enabled calls")
	}
	if _, err := (Prompt{}).AdmissionReservation(); !errors.Is(err, ErrUnverifiedInputBound) {
		t.Fatal("zero prompt enabled calls")
	}
	request.Job.ID = app.NewID()
	if _, err := BuildPrompt(request); err == nil {
		t.Fatal("accepted unrelated context")
	}
}

func TestGenerationProviderClassification(t *testing.T) {
	for status, want := range map[int]app.GenerationFailure{401: app.GenerationCredentials, 403: app.GenerationCredentials, 400: app.GenerationConfiguration, 404: app.GenerationConfiguration, 422: app.GenerationConfiguration, 408: app.GenerationTimeout, 504: app.GenerationTimeout, 429: app.GenerationRateLimited, 500: app.GenerationTransient, 502: app.GenerationTransient, 503: app.GenerationTransient, 501: app.GenerationPermanent, 418: app.GenerationPermanent, 200: app.GenerationPermanent} {
		if got := ClassifyHTTPFailure(status); got != want {
			t.Fatalf("%d: %s", status, got)
		}
	}
	for _, finish := range []string{"stop", "eos", "length", "tool_calls", "function_call", "", "unknown"} {
		failure := CompletionFailure(finish, false, false)
		if (failure == "") != (finish == "stop" || finish == "eos") {
			t.Fatal("unsafe finish", finish, failure)
		}
		if CompletionFailure(finish, true, false) != app.GenerationInvalidOutput {
			t.Fatal("tools accepted")
		}
		if CompletionFailure(finish, false, true) != app.GenerationAccountingUnsupported {
			t.Fatal("reasoning accepted")
		}
	}
}

func TestGenerationUsageAssessment(t *testing.T) {
	for name, test := range map[string]struct {
		raw         string
		accounted   int64
		known, stop bool
	}{
		"known":                 {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`, 120, true, false},
		"zero output":           {`{"prompt_tokens":100,"completion_tokens":0,"total_tokens":100}`, 100, true, false},
		"missing":               {`{}`, 300, false, false},
		"null":                  {`null`, 300, false, false},
		"partial":               {`{"prompt_tokens":100}`, 300, false, false},
		"null count":            {`{"prompt_tokens":null,"completion_tokens":20,"total_tokens":120}`, 300, false, false},
		"negative":              {`{"prompt_tokens":-1,"completion_tokens":20,"total_tokens":19}`, 300, false, false},
		"inconsistent":          {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":121}`, 300, false, false},
		"input over own bound":  {`{"prompt_tokens":101,"completion_tokens":20,"total_tokens":121}`, 300, false, true},
		"output over own bound": {`{"prompt_tokens":90,"completion_tokens":201,"total_tokens":291}`, 300, false, true},
		"partial over bound":    {`{"total_tokens":301}`, 300, false, true},
		"overflow":              {`{"prompt_tokens":9223372036854775807,"completion_tokens":9223372036854775807,"total_tokens":-2}`, 300, false, true},
		"reasoning":             {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"reasoning_tokens":1}`, 300, false, true},
		"case alias":            {`{"Prompt_tokens":100}`, 300, false, true},
		"duplicate":             {`{"prompt_tokens":100,"prompt_tokens":1}`, 300, false, true},
		"trailing":              {`{} {}`, 300, false, true},
		"fraction":              {`{"prompt_tokens":1.5}`, 300, false, true},
		"unknown null":          {`{"unknown":null}`, 300, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			usage, unsupported := DecodeUsage([]byte(test.raw))
			got := AssessUsage(100, 200, usage, false, unsupported)
			if got.AccountedTokens != test.accounted || (got.InputTokens != nil && got.OutputTokens != nil) != test.known || got.StopExecution != test.stop {
				t.Fatalf("assessment=%+v", got)
			}
			got = AssessUsage(100, 200, usage, true, unsupported)
			if got.AccountedTokens != 300 || got.InputTokens != nil || got.OutputTokens != nil {
				t.Fatal("uncertain outcome settled usage")
			}
		})
	}
	if !AssessUsage(0, 200, nil, false, false).StopExecution {
		t.Fatal("invalid reservation accepted")
	}
}
