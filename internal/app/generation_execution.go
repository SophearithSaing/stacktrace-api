package app

import (
	"context"
	"time"
)

// GenerationProvider is the worker's provider boundary. Implementations must
// return only classified failures, never transport errors or provider bodies.
// Calls require a committed durable reservation; this interface grants none.
type GenerationProvider interface {
	Generate(context.Context, GenerationRequest) GenerationOutcome
}

type GenerationRequest struct {
	Job     GenerationJob
	Context GenerationContext
}

type GenerationFailure string

const (
	GenerationTransient             GenerationFailure = "provider_transient"
	GenerationRateLimited           GenerationFailure = "provider_rate_limited"
	GenerationTimeout               GenerationFailure = "provider_timeout"
	GenerationInvalidOutput         GenerationFailure = "invalid_output"
	GenerationUnsafeOutput          GenerationFailure = "unsafe_output"
	GenerationRepeatedOutput        GenerationFailure = "repeated_output"
	GenerationCredentials           GenerationFailure = "provider_credentials"
	GenerationConfiguration         GenerationFailure = "provider_configuration"
	GenerationAccountingUnsupported GenerationFailure = "unsupported_accounting"
	GenerationCancelled             GenerationFailure = "execution_cancelled"
	GenerationPermanent             GenerationFailure = "provider_permanent"
)

func (f GenerationFailure) Valid() bool {
	switch f {
	case GenerationTransient, GenerationRateLimited, GenerationTimeout, GenerationInvalidOutput,
		GenerationUnsafeOutput, GenerationRepeatedOutput, GenerationCredentials, GenerationConfiguration,
		GenerationAccountingUnsupported, GenerationCancelled, GenerationPermanent:
		return true
	}
	return false
}

func (f GenerationFailure) StopsExecution() bool {
	return !f.Valid() || f == GenerationCredentials || f == GenerationConfiguration || f == GenerationAccountingUnsupported
}

// Usage is known only when the adapter verified every count against the exact
// request reservation. Unknown usage has nil counts and retains the reservation.
// Unknown execution (timeout/cancellation/crash) always uses unknown usage.
type GenerationOutcome struct {
	Result       GenerationResult
	Failure      GenerationFailure
	NotBefore    time.Time
	InputTokens  *int64
	OutputTokens *int64
}

func (o GenerationOutcome) Validate() error {
	if (o.Result.Digest() != "") == (o.Failure != "") || o.Failure != "" && !o.Failure.Valid() {
		return ErrGenerationOutput
	}
	if (o.InputTokens == nil) != (o.OutputTokens == nil) || o.InputTokens != nil && (*o.InputTokens < 1 || *o.OutputTokens < 0) {
		return ErrGenerationOutput
	}
	if (o.Failure == GenerationTimeout || o.Failure == GenerationCancelled || o.Failure == GenerationAccountingUnsupported) && o.InputTokens != nil {
		return ErrGenerationOutput
	}
	return nil
}

const (
	MaxGenerationAttempts             = 3
	MaxGenerationInvalidRegenerations = 1
	MaxGenerationRetryJitter          = time.Second
)

// NextGenerationRetry uses retained all-lease attempt history, including the
// just-finished attempt. invalidOutputs includes that attempt when invalid.
// A worker must persist the returned availability, not sleep or extend expiry.
// NotBefore is the maximum of all provider retry/reset hints; never clamp it
// earlier to fit a local backoff cap. Jitter is supplied in [0,1s].
func NextGenerationRetry(now, expiresAt time.Time, attempts, invalidOutputs int, failure GenerationFailure, notBefore time.Time, jitter time.Duration) (time.Time, bool) {
	if now.IsZero() || attempts < 1 || attempts >= MaxGenerationAttempts || invalidOutputs < 0 || invalidOutputs > attempts || jitter < 0 || jitter > MaxGenerationRetryJitter {
		return time.Time{}, false
	}
	switch failure {
	case GenerationTransient, GenerationRateLimited, GenerationTimeout:
	case GenerationInvalidOutput:
		if invalidOutputs < 1 || invalidOutputs > MaxGenerationInvalidRegenerations {
			return time.Time{}, false
		}
	default:
		return time.Time{}, false
	}
	next := now.Add((2 * time.Second << (attempts - 1)) + jitter)
	if notBefore.After(next) {
		next = notBefore
	}
	if !next.Before(expiresAt) {
		return time.Time{}, false
	}
	return next, true
}
