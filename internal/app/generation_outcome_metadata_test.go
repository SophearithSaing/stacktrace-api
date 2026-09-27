package app

import (
	"strings"
	"testing"
	"time"
)

func TestGenerationAttemptOutcomeMetadata(t *testing.T) {
	reserved := testGenerationAttempt(testRunningJob())
	finished := reserved.StartedAt.Add(time.Second)
	success := reserved
	success.Status, success.FinishedAt = AttemptSucceeded, &finished
	// Both pre-digest and pre-decision legacy successes remain readable.
	if err := success.Validate(); err != nil {
		t.Fatal(err)
	}
	success.OutputDigest = GenerationOutputDigestVersion + ":" + strings.Repeat("a", 64)
	if err := success.Validate(); err != nil {
		t.Fatal(err)
	}
	success.Decision = GenerationPublish
	if err := ValidateGenerationAttemptTransition(reserved, success); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*GenerationAttempt){
		"decision":         func(a *GenerationAttempt) { a.Decision = "invented" },
		"no_digest":        func(a *GenerationAttempt) { a.OutputDigest = "" },
		"publish_reason":   func(a *GenerationAttempt) { a.SkipReason = "not_relevant" },
		"skip_no_reason":   func(a *GenerationAttempt) { a.Decision = GenerationSkip },
		"skip_raw_reason":  func(a *GenerationAttempt) { a.Decision, a.SkipReason = GenerationSkip, "raw_provider_text" },
		"success_hint":     func(a *GenerationAttempt) { a.NotBefore = &finished },
		"failure_decision": func(a *GenerationAttempt) { a.Status, a.ErrorCode = AttemptFailed, "test" },
	} {
		t.Run(name, func(t *testing.T) {
			a := success
			change(&a)
			if a.Validate() == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	success.Decision, success.SkipReason = GenerationSkip, "not_relevant"
	if err := success.Validate(); err != nil {
		t.Fatal(err)
	}
	failure := reserved
	failure.Status, failure.ErrorCode, failure.FinishedAt, failure.NotBefore = AttemptUnknown, "lease_lost", &finished, &finished
	if err := failure.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, hint := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)} {
		failure.NotBefore = &hint
		if failure.Validate() == nil {
			t.Fatal("invalid timestamp accepted")
		}
	}
}

func TestGenerationOutcomeRetryHint(t *testing.T) {
	job := testRunningJob()
	result, err := DecodeGenerationResult([]byte(`{"decision":"skip","reason":"not_relevant"}`), job)
	if err != nil {
		t.Fatal(err)
	}
	hint := job.CreatedAt.Add(time.Minute)
	if (GenerationOutcome{Result: result, NotBefore: hint}).Validate() == nil {
		t.Fatal("success retry hint accepted")
	}
	if err := (GenerationOutcome{Failure: GenerationTransient, NotBefore: hint}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (GenerationOutcome{Failure: GenerationTransient, NotBefore: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}).Validate() == nil {
		t.Fatal("invalid hint accepted")
	}
}
