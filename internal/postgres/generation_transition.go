package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// CompleteGeneration reconciles the latest durable observation under the current
// job fence. It grants neither call nor publication permission. Publish remains
// running only on its original lease; a reclaimed publish has lost its payload
// and fails closed. No caller-replayed output, usage or retry hints are accepted.
// Locks are job -> attempts only, never budget/settings/source afterward.
func (s *Store) CompleteGeneration(ctx context.Context, id app.ID, version int64, attemptID app.ID, jitter time.Duration) (app.GenerationJobStatus, error) {
	return s.completeGeneration(ctx, id, version, attemptID, jitter, nil)
}

// completeGeneration settles a completed attempt and transitions its job transactionally.
func (s *Store) completeGeneration(ctx context.Context, id app.ID, version int64, attemptID app.ID, jitter time.Duration, override *time.Time) (app.GenerationJobStatus, error) {
	if jitter < 0 || jitter > app.MaxGenerationRetryJitter {
		return "", invalidGeneration("jitter")
	}
	var status app.GenerationJobStatus
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		job, err := q.lockExecutionJob(ctx, id)
		if err != nil {
			return err
		}
		attempts, err := q.executionAttempts(ctx, id)
		if err != nil {
			return err
		}
		if len(attempts) == 0 || len(attempts) > app.MaxGenerationAttempts || job.LeaseVersion != version || version < 1 {
			return app.ErrConflict
		}
		attempt := attempts[len(attempts)-1]
		if attempt.ID != attemptID || attempt.LeaseVersion > version || attempt.Status == app.AttemptReserved {
			return app.ErrConflict
		}
		// Terminal or queued acknowledgement replay is a read-only no-op. In
		// particular it cannot shorten retry availability or reopen terminal work.
		if job.Status != app.JobRunning {
			if job.Status == app.JobPending {
				return app.ErrConflict
			}
			status = job.Status
			return nil
		}
		now, err := q.generationClock(ctx, override) // after every potentially blocking read
		if err != nil {
			return err
		}
		if !now.Before(job.ExpiresAt) {
			status = app.JobSkipped
			return q.skipExpiredGenerationJob(ctx, job, now)
		}
		if job.TriggerKind == app.TriggerRepost && job.SourceRepostID == nil {
			// FK removal is proof under the job lock; do not reverse source order.
			after := job
			after.Status, after.ReasonCode, after.FinishedAt, after.LeaseExpiresAt = app.JobCancelled, "source_removed", &now, nil
			if app.ValidateGenerationSourceInvalidation(job, after, version, now) != nil {
				return app.ErrConflict
			}
			updated, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status='cancelled',reason_code='source_removed',finished_at=$3,lease_expires_at=NULL
				WHERE id=$1 AND lease_version=$2 AND status='running' AND source_repost_id IS NULL`, id, version, now)
			status = app.JobCancelled
			return generationClaimMutation(ctx, updated, err)
		}
		if job.ValidateLease(version, now) != nil || now.Before(*attempt.FinishedAt) {
			return app.ErrConflict
		}
		if attempt.LeaseVersion < version && job.AvailableAt.After(*attempt.FinishedAt) {
			// Prior retry already acknowledged. Leave this new lease available
			// for admission rather than replaying the old failure forever.
			status = app.JobRunning
			return nil
		}
		status = app.JobFailed
		reason, available := attempt.ErrorCode, now
		if attempt.Status == app.AttemptSucceeded {
			switch attempt.Decision {
			case app.GenerationSkip:
				status, reason = app.JobSkipped, attempt.SkipReason
			case app.GenerationPublish:
				if attempt.LeaseVersion == version {
					status = app.JobRunning
					return nil
				}
				reason = "outcome_unavailable"
			default:
				reason = "outcome_unavailable" // legacy success, never infer a decision
			}
		} else {
			failure := app.GenerationFailure(attempt.ErrorCode)
			if attempt.Status == app.AttemptUnknown && attempt.ErrorCode == "lease_lost" {
				failure = app.GenerationTimeout // accounting remains unknown/full cost
			}
			if !failure.Valid() {
				reason = "outcome_unavailable"
			} else {
				invalid := 0
				for _, prior := range attempts {
					if prior.ErrorCode == string(app.GenerationInvalidOutput) {
						invalid++
					}
				}
				var hint time.Time
				if attempt.NotBefore != nil {
					hint = *attempt.NotBefore
				}
				if next, retry := app.NextGenerationRetry(now, job.ExpiresAt, len(attempts), invalid, failure, hint, jitter); retry {
					status, available = app.JobRetryWait, next
				} else if failure == app.GenerationCancelled {
					status = app.JobCancelled
				} else if failure == app.GenerationUnsafeOutput || failure == app.GenerationRepeatedOutput {
					status = app.JobSkipped
				}
			}
		}
		return q.finishExecutionJob(ctx, job, status, reason, now, available)
	})
	if err != nil {
		return "", err
	}
	return status, nil
}
