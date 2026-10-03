package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/llm"
)

// SettleGeneration records an observation, never job/publication authority. The
// supplied attempt must be the exact committed reservation. Exact normalized
// terminal repeats return true; conflicting/late observations return false.
// Even after lease loss a reserved observation may finish, but a recovered
// unknown can NEVER become successful or release its reservation.
func (s *Store) SettleGeneration(ctx context.Context, reserved app.GenerationAttempt, outcome app.GenerationOutcome, requestID string) (bool, error) {
	if reserved.Validate() != nil || reserved.Status != app.AttemptReserved || !safeGenerationRequestID(requestID) {
		return false, invalidGeneration("outcome")
	}
	// Snapshot caller-owned counts before blocking. No pointer aliases enter the
	// transaction or its comparison state. Invalid accounting retains full cost;
	// over-target accounting is durably classified as execution-stopping failure.
	input, output := outcome.InputTokens, outcome.OutputTokens
	outcome.InputTokens, outcome.OutputTokens = nil, nil
	// Snapshot the caller-owned pause revision too: settlement may wait on the
	// budget/attempt locks, and mutated/caller-moved revisions after that wait
	// must never change the authority comparison.
	frozen := reserved
	if frozen.PauseRevision != nil {
		revision := *frozen.PauseRevision
		frozen.PauseRevision = &revision
	}
	if outcome.Validate() != nil {
		return false, invalidGeneration("outcome")
	}
	if !outcome.NotBefore.IsZero() {
		// PostgreSQL stores microseconds. Round lower bounds up, never shorten
		// the provider's requested delay by truncating sub-microsecond precision.
		normalized := app.GenerationInstant(outcome.NotBefore)
		if normalized.Before(outcome.NotBefore) {
			normalized = normalized.Add(time.Microsecond)
		}
		outcome.NotBefore = normalized
		if outcome.NotBefore.IsZero() || outcome.Validate() != nil {
			return false, invalidGeneration("outcome")
		}
	}
	uncertain := outcome.Failure == app.GenerationTimeout || outcome.Failure == app.GenerationCancelled || outcome.Failure == app.GenerationAccountingUnsupported
	if uncertain {
		// A timeout/cancellation stays unknown regardless of attached counts.
		input, output = nil, nil
	}
	if input != nil && output != nil {
		in, out := *input, *output
		if in > llm.MaxInputTokens || out > llm.MaxOutputTokens {
			outcome.Result, outcome.Failure = app.GenerationResult{}, app.GenerationAccountingUnsupported
		} else if in >= 1 && out >= 0 {
			outcome.InputTokens, outcome.OutputTokens = &in, &out
		}
	} else if input != nil && *input > llm.MaxInputTokens || output != nil && *output > llm.MaxOutputTokens {
		outcome.Result, outcome.Failure = app.GenerationResult{}, app.GenerationAccountingUnsupported
	}
	accepted := false
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		if err := q.lockGenerationBudget(ctx); err != nil {
			return err
		}
		var id app.ID
		if err := q.queryer.QueryRowContext(ctx, `SELECT id FROM generation_attempts WHERE id=$1 FOR UPDATE`, reserved.ID).Scan(&id); err != nil {
			return databaseError(ctx, err)
		}
		before, err := q.GenerationAttemptByID(ctx, id)
		if err != nil {
			return err
		}
		if !sameGenerationReservation(before, frozen) {
			return app.ErrConflict
		}
		// A nonlocking kind read only. NEVER lock a job after budget/attempt.
		if outcome.Result.Digest() != "" {
			var kind app.GenerationOutput
			if err := q.queryer.QueryRowContext(ctx, `SELECT output_kind FROM generation_jobs WHERE id=$1`, before.JobID).Scan(&kind); err != nil {
				return databaseError(ctx, err)
			}
			if outcome.Result.OutputKind() != kind {
				return invalidGeneration("outcome")
			}
		}
		now, err := q.generationClock(ctx, nil)
		if err != nil {
			return err
		}
		after := frozen
		after.Status, after.FinishedAt, after.ProviderRequestID = app.AttemptFailed, &now, requestID
		after.ErrorCode = string(outcome.Failure)
		after.InputTokens, after.OutputTokens = outcome.InputTokens, outcome.OutputTokens
		if !outcome.NotBefore.IsZero() {
			after.NotBefore = &outcome.NotBefore
		}
		if outcome.Result.Digest() != "" {
			after.Status, after.OutputDigest = app.AttemptSucceeded, outcome.Result.Digest()
			after.Decision, after.SkipReason = outcome.Result.Decision(), outcome.Result.Reason()
		}
		if outcome.Failure == app.GenerationTimeout || outcome.Failure == app.GenerationCancelled {
			after.Status = app.AttemptUnknown
		}
		if before.Status != app.AttemptReserved {
			// FinishedAt is assigned by storage, not the caller's replay clock.
			accepted = before.Status == after.Status && before.ErrorCode == after.ErrorCode && before.OutputDigest == after.OutputDigest &&
				before.Decision == after.Decision && before.SkipReason == after.SkipReason && sameSettlementTime(before.NotBefore, after.NotBefore) &&
				before.ProviderRequestID == after.ProviderRequestID && sameSettlementUsage(before.InputTokens, after.InputTokens) && sameSettlementUsage(before.OutputTokens, after.OutputTokens)
			return nil
		}
		if app.ValidateGenerationAttemptTransition(before, after) != nil {
			return invalidGeneration("outcome")
		}
		updated, err := q.queryer.ExecContext(ctx, `UPDATE generation_attempts SET status=$2,error_code=NULLIF($3,''),input_tokens=$4,output_tokens=$5,
			output_digest=NULLIF($6,''),provider_request_id=NULLIF($7,''),finished_at=$8,
			decision=NULLIF($9,''),skip_reason=NULLIF($10,''),not_before=$11 WHERE id=$1 AND status='reserved'`,
			id, after.Status, after.ErrorCode, after.InputTokens, after.OutputTokens, after.OutputDigest, requestID, now, after.Decision, after.SkipReason, after.NotBefore)
		if err := generationClaimMutation(ctx, updated, err); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	return accepted && err == nil, err
}

// sameGenerationReservation reports whether two attempts describe the same reservation.
func sameGenerationReservation(a, b app.GenerationAttempt) bool {
	return a.ID == b.ID && a.JobID == b.JobID && a.AttemptNumber == b.AttemptNumber && a.LeaseVersion == b.LeaseVersion &&
		sameSettlementUsage(a.PauseRevision, b.PauseRevision) &&
		a.Provider == b.Provider && a.Model == b.Model && a.ContextHash == b.ContextHash && a.ContextBuilderVersion == b.ContextBuilderVersion &&
		a.BudgetDay == b.BudgetDay && a.ReservedTokens == b.ReservedTokens && a.StartedAt.Equal(b.StartedAt)
}

// sameSettlementUsage reports whether two optional usage values match exactly.
func sameSettlementUsage(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// sameSettlementTime reports whether two optional settlement times match exactly.
func sameSettlementTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// safeGenerationRequestID validates and bounds a provider request identifier for persistence.
// Absence is allowed (timeouts often have no response). Present IDs are bounded
// opaque tokens, never free-form provider text or control/whitespace characters.
func safeGenerationRequestID(id string) bool {
	if len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
