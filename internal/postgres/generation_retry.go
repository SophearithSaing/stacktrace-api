package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// retryGenerationReason marks a failed job as explicitly re-opened by a trusted
// operator retry. A fresh claim re-acknowledges availability via the existing
// availability-after-finish contract; ordinary claims cannot reach this state
// for a failed job, so an acknowledged provider credentials/configuration
// attempt still proves the operator retried it after repair. No
// unsupported-accounting repair acknowledgement exists in this scope.
const retryGenerationReason = "operator_retry"

// RetryGeneration re-opens one failed job from the trusted persistence menu.
// It retains identity, expiry, lease version, attempts, quota rank and charged
// usage (nothing is refunded or erased) and grants neither admission nor
// publication authority. Availability is a fresh locked-time floor that keeps a
// newer provider Retry-After hint; a hint can delay, never extend work past the
// immutable job expiry, and always lands strictly after the latest finished
// attempt at PostgreSQL microsecond precision. Eligibility is re-checked under
// the standard lock order (source -> agent account -> settings -> job -> policy
// allowance -> fresh clock) before any write: removal, account disable, pause,
// corrupt settings, zero quotas, stale sources/windows or a tightened chain
// deny the retry instead of queueing unavailable work. Succeeded, skipped,
// cancelled, final, exhausted, unsupported-accounting, unsafe or repeated work
// stays closed; denial errors are ErrNotFound, ErrConflict, ErrDeleted,
// app.ErrForbidden or a ValidationError.
func (s *Store) RetryGeneration(ctx context.Context, id app.ID) (app.GenerationJob, error) {
	if _, err := app.ParseID(string(id)); err != nil {
		return app.GenerationJob{}, invalidGeneration("job_id")
	}
	var retried app.GenerationJob
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		// Locking happens on valid references only; this first read is unlisted
		// because eligibility locks and later re-verification re-check everything.
		read, err := q.GenerationJobByID(ctx, id)
		if err != nil {
			return err
		}
		policy, lastPublished, denial, err := q.lockExecutionEligibility(ctx, read)
		if err != nil {
			return err
		}
		switch denial {
		case "":
		case "source_removed":
			return app.ErrDeleted
		default:
			return app.ErrForbidden
		}
		// executionPolicyAllowed requires the root lock before the job/attempt
		// lock while the same transaction keeps the root to the end.
		if read.RootJobID != read.ID {
			if _, err := q.lockExecutionJob(ctx, read.RootJobID); err != nil {
				return err
			}
		}
		job, err := q.lockExecutionJob(ctx, id)
		if err != nil {
			return err
		}
		if err := s.retriedGeneration(q, ctx, job, policy, lastPublished, &retried); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return app.GenerationJob{}, err
	}
	return retried, nil
}

func (s *Store) retriedGeneration(q *Queries, ctx context.Context, job app.GenerationJob,
	policy app.GenerationPolicy, lastPublished *time.Time, out *app.GenerationJob) error {
	if job.Status != app.JobFailed {
		return app.ErrConflict // Never re-open non-failed work.
	}
	attempts, err := q.executionAttempts(ctx, job.ID)
	if err != nil {
		return err
	}
	// The whole retained history closes closed doors: unresolved reservations,
	// unsupported accounting and overturned/regenerated caps never re-open.
	if len(attempts) >= app.MaxGenerationAttempts {
		return app.ErrForbidden // Final/exhausted candidate.
	}
	invalid := 0
	for _, attempt := range attempts {
		switch {
		case attempt.Status == app.AttemptReserved:
			return app.ErrForbidden // Unresolved observation.
		case attempt.ErrorCode == string(app.GenerationAccountingUnsupported):
			return app.ErrForbidden // The accounting stop persists: no repair ack.
		case attempt.ErrorCode == string(app.GenerationInvalidOutput):
			invalid++
		}
	}
	// Align the threshold with admission's regenerations (> cap denied).
	if invalid > app.MaxGenerationInvalidRegenerations {
		return app.ErrForbidden
	}
	last := app.GenerationAttempt{Status: app.AttemptFailed}
	if len(attempts) > 0 {
		last = attempts[len(attempts)-1]
	}
	if last.Status == app.AttemptReserved { // Latest reservation never settles.
		return app.ErrForbidden
	}
	if last.Status == app.AttemptSucceeded {
		// Lost publish payload and legacy undecoded successes are retirable
		// only through their explicit reason; skip decisions belong to skipped
		// work, never to a failed row.
		if job.ReasonCode != "outcome_unavailable" || last.Decision == app.GenerationSkip {
			return app.ErrForbidden
		}
	} else {
		// A failed row re-opens only for provider credentials/configuration
		// repair acks or the retryable population. Final unsafe/repeated
		// observations, execution cancellation and unrecognized reason codes
		// never re-open, even with successful history.
		failure := app.GenerationFailure(job.ReasonCode)
		if !failure.Valid() || failure == app.GenerationUnsafeOutput ||
			failure == app.GenerationRepeatedOutput || failure == app.GenerationCancelled {
			return app.ErrForbidden
		}
	}
	now, err := q.generationClock(ctx, nil) // Fresh locked time after every lock.
	if err != nil {
		return err
	}
	if !now.Before(job.ExpiresAt) {
		return app.ErrForbidden
	}
	allowed, err := q.executionPolicyAllowed(ctx, job, policy, lastPublished, now)
	if err != nil {
		return err
	}
	if !allowed {
		return app.ErrForbidden // Tightened source/policy/quotas could never admit.
	}
	available := now // A retained provider Retry-After can only delay availability.
	if len(attempts) > 0 && attempts[len(attempts)-1].NotBefore != nil && attempts[len(attempts)-1].NotBefore.After(available) {
		available = *attempts[len(attempts)-1].NotBefore
	}
	if last.FinishedAt != nil && !available.After(app.GenerationInstant(*last.FinishedAt)) {
		available = app.GenerationInstant(*last.FinishedAt).Add(time.Microsecond)
	}
	if !available.Before(job.ExpiresAt) {
		return app.ErrForbidden
	}
	after := job
	after.Status, after.ReasonCode = app.JobRetryWait, retryGenerationReason
	after.AvailableAt, after.FinishedAt, after.LeaseExpiresAt = available, nil, nil
	if app.ValidateGenerationJobTransition(job, after, job.LeaseVersion, now, nil) != nil {
		return app.ErrConflict // Raced with another queued/terminal write.
	}
	result, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status='retry_wait',reason_code=$2,available_at=$3,
		finished_at=NULL,lease_expires_at=NULL WHERE id=$1 AND lease_version=$4 AND status='failed'`,
		job.ID, after.ReasonCode, available, job.LeaseVersion)
	if err := generationClaimMutation(ctx, result, err); err != nil {
		return err
	}
	*out = after
	return nil
}
