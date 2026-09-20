package app

import (
	"strings"
	"testing"
	"time"
)

func testGenerationAttempt(job GenerationJob) GenerationAttempt {
	return GenerationAttempt{ID: NewID(), JobID: job.ID, AttemptNumber: 1, LeaseVersion: job.LeaseVersion, Provider: "together", Model: "selected-model", ContextHash: strings.Repeat("a", 64), ContextBuilderVersion: "v1", BudgetDay: job.CreatedAt.UTC().Format(time.DateOnly), ReservedTokens: 10000, Status: AttemptReserved, StartedAt: job.CreatedAt}
}

func TestGenerationAttemptValidation(t *testing.T) {
	a := testGenerationAttempt(testRunningJob())
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := a.AccountedTokens(); got != a.ReservedTokens {
		t.Fatalf("reserved cost = %d", got)
	}
	for name, change := range map[string]func(*GenerationAttempt){
		"identity":              func(a *GenerationAttempt) { a.JobID = "" },
		"number":                func(a *GenerationAttempt) { a.AttemptNumber = 0 },
		"lease":                 func(a *GenerationAttempt) { a.LeaseVersion = 0 },
		"provider":              func(a *GenerationAttempt) { a.Provider = "bad provider" },
		"model":                 func(a *GenerationAttempt) { a.Model = "" },
		"hash":                  func(a *GenerationAttempt) { a.ContextHash = "invalid" },
		"builder":               func(a *GenerationAttempt) { a.ContextBuilderVersion = "" },
		"budget day":            func(a *GenerationAttempt) { a.BudgetDay = "2026-09-19" },
		"reservation":           func(a *GenerationAttempt) { a.ReservedTokens = 0 },
		"status":                func(a *GenerationAttempt) { a.Status = "other" },
		"outcome before finish": func(a *GenerationAttempt) { a.Status = AttemptSucceeded },
		"finished reserved":     func(a *GenerationAttempt) { a.FinishedAt = &a.StartedAt },
		"usage reserved":        func(a *GenerationAttempt) { a.InputTokens, a.OutputTokens = new(int64), new(int64) },
	} {
		t.Run(name, func(t *testing.T) {
			next := a
			change(&next)
			if err := next.Validate(); err == nil {
				t.Fatal("accepted invalid attempt")
			}
		})
	}
	// Admission days always use UTC, even when the caller's clock carries a zone.
	a.StartedAt = a.StartedAt.In(time.FixedZone("offset", -12*3600))
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationAttemptOutcomesAndAccounting(t *testing.T) {
	reserved := testGenerationAttempt(testRunningJob())
	finish := reserved.StartedAt.Add(time.Second)
	for _, status := range []GenerationAttemptStatus{AttemptSucceeded, AttemptFailed, AttemptUnknown} {
		outcome := reserved
		outcome.Status, outcome.FinishedAt = status, &finish
		if status != AttemptSucceeded {
			outcome.ErrorCode = "timeout"
		}
		if err := ValidateGenerationAttemptTransition(reserved, outcome); err != nil {
			t.Fatal(err)
		}
		if got := outcome.AccountedTokens(); got != reserved.ReservedTokens {
			t.Fatalf("missing usage charged %d", got)
		}
		if err := ValidateGenerationAttemptTransition(outcome, reserved); err == nil {
			t.Fatal("reopened attempt")
		}
		outcome.ReservedTokens++
		if err := ValidateGenerationAttemptTransition(reserved, outcome); err == nil {
			t.Fatal("changed reservation")
		}
	}
	complete := reserved
	complete.Status, complete.FinishedAt = AttemptSucceeded, &finish
	input, output := int64(400), int64(100)
	complete.InputTokens, complete.OutputTokens = &input, &output
	if err := complete.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := complete.AccountedTokens(); got != 500 {
		t.Fatalf("known usage = %d", got)
	}
	for name, change := range map[string]func(*GenerationAttempt){
		"partial usage":    func(a *GenerationAttempt) { a.OutputTokens = nil },
		"negative usage":   func(a *GenerationAttempt) { n := int64(-1); a.InputTokens = &n },
		"overflow usage":   func(a *GenerationAttempt) { n := int64(1<<63 - 1); a.OutputTokens = &n },
		"over reservation": func(a *GenerationAttempt) { n := a.ReservedTokens; a.OutputTokens = &n },
		"unknown usage":    func(a *GenerationAttempt) { a.Status, a.ErrorCode = AttemptUnknown, "timeout" },
	} {
		t.Run(name, func(t *testing.T) {
			a := complete
			change(&a)
			if err := a.Validate(); err == nil {
				t.Fatal("accepted inconsistent usage")
			}
			if got := a.AccountedTokens(); got != a.ReservedTokens {
				t.Fatalf("released uncertain reservation: %d", got)
			}
		})
	}
}
