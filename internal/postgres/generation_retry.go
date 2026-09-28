package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// retryGenerationReason marks a failed job as explicitly re-opened by a trusted
// operator retry. A fresh claim re-acknowledges availability via the existing
// availability-after-finish contract; ordinary claims cannot reach this state
// for a failed job, so an acknowledged fatal attempt still proves the operator
// retried it after repair.
const retryGenerationReason = "operator_retry"

// RetryGeneration re-opens one failed job from the trusted persistence menu.
// It retains identity, expiry, lease version, attempts, quota rank and charged
// usage (nothing is refunded or erased) and grants neither admission nor
// publication authority. Availability is a fresh locked-time floor that keeps a
// newer provider Retry-After hint; a hint can delay, never extend work past the
// immutable job expiry. Eligibility is re-checked under the standard lock order
// (source -> agent account -> settings -> job) before any write: removal,
// account disable, pause, corrupt settings or a changed policy deny the retry
// instead of queueing unavailable work. Terminal, unsupported-accounting,
// attempt- or invalid-output-exhausted candidates stay closed; denial errors are
// ErrNotFound, ErrConflict, ErrDeleted, app.ErrForbidden or a ValidationError.
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
		_, _, denial, err := q.lockExecutionEligibility(ctx, read)
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
		job, err := q.lockExecutionJob(ctx, id)
		if err != nil {
			return err
		}
		if err := s.retriedGeneration(q, ctx, job, &retried); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return app.GenerationJob{}, err
	}
	return retried, nil
}

func (s *Store) retriedGeneration(q *Queries, ctx context.Context, job app.GenerationJob, out *app.GenerationJob) error {
	if job.Status != app.JobFailed {
		return app.ErrConflict // Never re-open non-failed work.
	}
	if job.ReasonCode == string(app.GenerationAccountingUnsupported) {
		return app.ErrForbidden // The accounting stop persists until a repair ack.
	}
	attempts, err := q.executionAttempts(ctx, job.ID)
	if err != nil {
		return err
	}
	if len(attempts) >= app.MaxGenerationAttempts {
		return app.ErrForbidden
	}
	invalid := 0
	for _, attempt := range attempts {
		if attempt.ErrorCode == string(app.GenerationInvalidOutput) {
			invalid++
		}
	}
	if invalid >= app.MaxGenerationInvalidRegenerations {
		return app.ErrForbidden
	}
	var hint time.Time // A retained provider Retry-After can only delay availability.
	if len(attempts) > 0 && attempts[len(attempts)-1].NotBefore != nil {
		hint = *attempts[len(attempts)-1].NotBefore
	}
	now, err := q.generationClock(ctx, nil) // Fresh locked time after every lock.
	if err != nil {
		return err
	}
	if !now.Before(job.ExpiresAt) {
		return app.ErrForbidden
	}
	available := now
	if hint.After(available) {
		available = hint
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
