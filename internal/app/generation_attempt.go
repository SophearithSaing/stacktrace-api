package app

import (
	"encoding/hex"
	"fmt"
	"time"
)

type GenerationAttemptStatus string

const (
	AttemptReserved  GenerationAttemptStatus = "reserved"
	AttemptSucceeded GenerationAttemptStatus = "succeeded"
	AttemptFailed    GenerationAttemptStatus = "failed"
	AttemptUnknown   GenerationAttemptStatus = "unknown"
)

// GenerationAttempt records one durable reservation before one provider call.
// Unknown outcomes retain the entire reservation; retrying creates a new attempt
// on the same job. Usage is nullable even for successful output.
type GenerationAttempt struct {
	ID                    ID
	JobID                 ID
	AttemptNumber         int
	LeaseVersion          int64
	Provider              string
	Model                 string
	ProviderRequestID     string
	ContextHash           string
	ContextBuilderVersion string
	BudgetDay             string
	ReservedTokens        int64
	InputTokens           *int64
	OutputTokens          *int64
	Status                GenerationAttemptStatus
	ErrorCode             string
	StartedAt             time.Time
	FinishedAt            *time.Time
}

func (a GenerationAttempt) Validate() error {
	if !validGenerationID(a.ID) || !validGenerationID(a.JobID) || a.AttemptNumber < 1 || a.AttemptNumber > 2147483647 || a.LeaseVersion < 1 {
		return fmt.Errorf("invalid attempt identity")
	}
	if !validGenerationCode(a.Provider) || !validGenerationText(a.Model, 128) || !validGenerationCode(a.ContextBuilderVersion) || len(a.ProviderRequestID) > 256 || a.ProviderRequestID != "" && !validGenerationText(a.ProviderRequestID, 256) {
		return fmt.Errorf("invalid attempt provider or context metadata")
	}
	hash, err := hex.DecodeString(a.ContextHash)
	if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != a.ContextHash {
		return fmt.Errorf("context hash must be lowercase SHA-256 hex")
	}
	if a.StartedAt.IsZero() || !validGenerationDate(a.BudgetDay) || a.BudgetDay != a.StartedAt.UTC().Format(time.DateOnly) || a.ReservedTokens <= 0 {
		return fmt.Errorf("invalid attempt reservation or UTC budget day")
	}
	if (a.InputTokens == nil) != (a.OutputTokens == nil) {
		return fmt.Errorf("usage requires both input and output tokens")
	}
	if a.InputTokens != nil && (*a.InputTokens < 0 || *a.OutputTokens < 0 || *a.InputTokens > a.ReservedTokens || *a.OutputTokens > a.ReservedTokens-*a.InputTokens) {
		return fmt.Errorf("usage exceeds reservation or is negative")
	}
	switch a.Status {
	case AttemptReserved:
		if a.FinishedAt != nil || a.InputTokens != nil || a.ErrorCode != "" || a.ProviderRequestID != "" {
			return fmt.Errorf("reserved attempt cannot have an outcome")
		}
	case AttemptSucceeded, AttemptFailed, AttemptUnknown:
		if a.FinishedAt == nil || a.FinishedAt.Before(a.StartedAt) {
			return fmt.Errorf("completed attempt requires a finish time")
		}
		if a.Status == AttemptSucceeded {
			if a.ErrorCode != "" {
				return fmt.Errorf("successful attempt cannot have an error")
			}
		} else if !validGenerationCode(a.ErrorCode) {
			return fmt.Errorf("unsuccessful attempt requires a bounded error code")
		}
		if a.Status == AttemptUnknown && a.InputTokens != nil {
			return fmt.Errorf("unknown outcome must retain reservation without settled usage")
		}
	default:
		return fmt.Errorf("invalid attempt status")
	}
	return nil
}

func (a GenerationAttempt) AccountedTokens() int64 {
	if a.Status == AttemptReserved || a.Status == AttemptUnknown || a.InputTokens == nil || a.OutputTokens == nil || a.Validate() != nil {
		return a.ReservedTokens
	}
	return *a.InputTokens + *a.OutputTokens
}

// Attempt outcomes are append-only observations. This does not authorize any
// job mutation; publication separately checks the current job's live lease.
func ValidateGenerationAttemptTransition(before, after GenerationAttempt) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if before.Status != AttemptReserved || after.Status == AttemptReserved {
		return fmt.Errorf("only reserved attempts may finish")
	}
	if before.ID != after.ID || before.JobID != after.JobID || before.AttemptNumber != after.AttemptNumber || before.LeaseVersion != after.LeaseVersion || before.Provider != after.Provider || before.Model != after.Model || before.ContextHash != after.ContextHash || before.ContextBuilderVersion != after.ContextBuilderVersion || before.BudgetDay != after.BudgetDay || before.ReservedTokens != after.ReservedTokens || !before.StartedAt.Equal(after.StartedAt) {
		return fmt.Errorf("attempt identity and reservation are immutable")
	}
	return nil
}
