package llm

import (
	"encoding/json"
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
	if err != nil || multibyte.LocalInputTokenEstimate()-first.LocalInputTokenEstimate() != 3 {
		t.Fatal("not a byte bound", err)
	}
	if first.LocalInputTokenEstimate() != int64(len(first.System())+len(first.User())+len(first.Schema())+34) {
		t.Fatal("framing or schema missing")
	}
	if reservation, err := first.AdmissionReservation(); reservation != 132096 || err != nil {
		t.Fatal("tiny prompt did not reserve full provider ceiling", reservation, err)
	}
	if _, err := (Prompt{}).AdmissionReservation(); err == nil {
		t.Fatal("zero prompt enabled calls")
	}
	request.Job.ID = app.NewID()
	if _, err := BuildPrompt(request); err == nil {
		t.Fatal("accepted unrelated context")
	}
}

func TestGenerationPromptAdmissionBoundary(t *testing.T) {
	base, err := BuildPrompt(generationRequest(t, "x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{0, 1} {
		instructions := strings.Repeat("x", 1+MaxInputTokens-int(base.LocalInputTokenEstimate())+delta)
		prompt, err := BuildPrompt(generationRequest(t, instructions))
		if err != nil {
			t.Fatal(err)
		}
		if prompt.LocalInputTokenEstimate() != int64(MaxInputTokens+delta) {
			t.Fatal("incorrect construction estimate")
		}
		reservation, err := prompt.AdmissionReservation()
		if delta == 0 && (err != nil || reservation != 132096) || delta == 1 && (err == nil || reservation != 0) {
			t.Fatalf("boundary %d: %d %v", delta, reservation, err)
		}
	}
	// Schema/framing overhead is counted even when bare message text would fit.
	oversized, err := BuildPrompt(generationRequest(t, strings.Repeat("x", MaxInputTokens-int(base.LocalInputTokenEstimate())+2)))
	if err != nil || len(oversized.System())+len(oversized.User()) >= MaxInputTokens {
		t.Fatal("invalid overhead boundary fixture", err)
	}
	if _, err := oversized.AdmissionReservation(); err == nil {
		t.Fatal("ignored framing/schema overhead")
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
		"known":               {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`, 120, true, false},
		"zero output":         {`{"prompt_tokens":100,"completion_tokens":0,"total_tokens":100}`, 100, true, false},
		"local targets":       {`{"prompt_tokens":8192,"completion_tokens":1024,"total_tokens":9216}`, 9216, true, false},
		"above tiny estimate": {`{"prompt_tokens":8000,"completion_tokens":20,"total_tokens":8020}`, 8020, true, false},
		"missing":             {`{}`, 132096, false, false},
		"absent":              {``, 132096, false, false},
		"null":                {`null`, 132096, false, false},
		"partial":             {`{"prompt_tokens":100}`, 132096, false, false},
		"null count":          {`{"prompt_tokens":null,"completion_tokens":20,"total_tokens":120}`, 132096, false, false},
		"negative":            {`{"prompt_tokens":-1,"completion_tokens":20,"total_tokens":19}`, 132096, false, false},
		"negative output":     {`{"prompt_tokens":100,"completion_tokens":-1,"total_tokens":99}`, 132096, false, false},
		"negative total":      {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":-120}`, 132096, false, false},
		"zero prompt":         {`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, 132096, false, false},
		"inconsistent":        {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":121}`, 132096, false, false},
		"input over target":   {`{"prompt_tokens":8193,"completion_tokens":0,"total_tokens":8193}`, 132096, false, true},
		"output over target":  {`{"prompt_tokens":100,"completion_tokens":1025,"total_tokens":1125}`, 132096, false, true},
		"partial over target": {`{"total_tokens":9217}`, 132096, false, true},
		"provider ceiling":    {`{"prompt_tokens":131072,"completion_tokens":1024,"total_tokens":132096}`, 132096, false, true},
		"beyond ceiling":      {`{"prompt_tokens":131073,"completion_tokens":1024,"total_tokens":132097}`, 132096, false, true},
		"overflow":            {`{"prompt_tokens":9223372036854775807,"completion_tokens":9223372036854775807,"total_tokens":-2}`, 132096, false, true},
		"negative extremes":   {`{"prompt_tokens":-9223372036854775808,"completion_tokens":-9223372036854775808,"total_tokens":0}`, 132096, false, false},
		"integer overflow":    {`{"prompt_tokens":9223372036854775808}`, 132096, false, true},
		"reasoning":           {`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"reasoning_tokens":1}`, 132096, false, true},
		"case alias":          {`{"Prompt_tokens":100}`, 132096, false, true},
		"duplicate":           {`{"prompt_tokens":100,"prompt_tokens":1}`, 132096, false, true},
		"trailing":            {`{} {}`, 132096, false, true},
		"fraction":            {`{"prompt_tokens":1.5}`, 132096, false, true},
		"unknown null":        {`{"unknown":null}`, 132096, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			usage, unsupported := DecodeUsage([]byte(test.raw))
			got := AssessUsage(usage, false, unsupported)
			if got.AccountedTokens != test.accounted || (got.InputTokens != nil && got.OutputTokens != nil) != test.known || got.StopExecution != test.stop {
				t.Fatalf("assessment=%+v", got)
			}
			got = AssessUsage(usage, true, unsupported)
			if got.AccountedTokens != 132096 || got.InputTokens != nil || got.OutputTokens != nil || got.StopExecution != test.stop {
				t.Fatal("uncertain outcome settled usage")
			}
			got = AssessUsage(usage, false, true)
			if got.AccountedTokens != 132096 || got.InputTokens != nil || got.OutputTokens != nil || !got.StopExecution {
				t.Fatal("unsupported accounting settled usage")
			}
		})
	}
}
