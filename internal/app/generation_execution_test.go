package app

import (
	"testing"
	"time"
)

func TestGenerationRetry(t *testing.T) {
	now := testGenerationJob().CreatedAt
	expires := now.Add(time.Minute)
	for _, failure := range []GenerationFailure{GenerationTransient, GenerationRateLimited, GenerationTimeout, GenerationInvalidOutput} {
		for attempts := 1; attempts <= 3; attempts++ {
			next, ok := NextGenerationRetry(now, expires, attempts, 1, failure, time.Time{}, time.Second)
			if ok != (attempts < 3) {
				t.Fatalf("%s attempts %d: %v", failure, attempts, ok)
			}
			if ok && next != now.Add((2*time.Second<<(attempts-1))+time.Second) {
				t.Fatal("wrong backoff", next)
			}
		}
	}
	for _, hint := range []time.Time{now.Add(30 * time.Second), expires, expires.Add(time.Hour)} {
		next, ok := NextGenerationRetry(now, expires, 1, 0, GenerationRateLimited, hint, 0)
		if ok != hint.Before(expires) || ok && next.Before(hint) {
			t.Fatal("ignored Retry-After or expiry")
		}
	}
	for _, failure := range []GenerationFailure{GenerationUnsafeOutput, GenerationRepeatedOutput, GenerationCredentials, GenerationConfiguration, GenerationAccountingUnsupported, GenerationCancelled, GenerationPermanent, "unknown", ""} {
		if _, ok := NextGenerationRetry(now, expires, 1, 0, failure, time.Time{}, 0); ok {
			t.Fatal("retried terminal failure", failure)
		}
	}
	for _, counts := range [][2]int{{0, 0}, {-1, 0}, {4, 0}, {1, -1}, {1, 2}, {2, 2}} {
		if _, ok := NextGenerationRetry(now, expires, counts[0], counts[1], GenerationInvalidOutput, time.Time{}, 0); ok {
			t.Fatal("invalid retry count", counts)
		}
	}
	for _, jitter := range []time.Duration{-1, time.Second + 1} {
		if _, ok := NextGenerationRetry(now, expires, 1, 0, GenerationTransient, time.Time{}, jitter); ok {
			t.Fatal("invalid jitter")
		}
	}
	if _, ok := NextGenerationRetry(now, now.Add(2*time.Second), 1, 0, GenerationTransient, time.Time{}, 0); ok {
		t.Fatal("retried at exclusive expiry")
	}
}

func TestGenerationOutcome(t *testing.T) {
	result := decodeTestGeneration(t, "Hello")
	input, output := int64(100), int64(20)
	for _, outcome := range []GenerationOutcome{{Result: result}, {Failure: GenerationTransient}, {Result: result, InputTokens: &input, OutputTokens: &output}} {
		if err := outcome.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, outcome := range []GenerationOutcome{{}, {Result: result, Failure: GenerationTransient}, {Failure: "secret provider error"}, {Result: result, InputTokens: &input}, {Failure: GenerationTimeout, InputTokens: &input, OutputTokens: &output}} {
		if outcome.Validate() == nil {
			t.Fatal("invalid outcome accepted")
		}
	}
	for _, failure := range []GenerationFailure{GenerationCredentials, GenerationConfiguration, GenerationAccountingUnsupported, "unknown"} {
		if !failure.StopsExecution() {
			t.Fatal("did not stop", failure)
		}
	}
}
