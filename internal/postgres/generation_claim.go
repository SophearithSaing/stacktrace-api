package postgres

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const generationLeaseDuration = 90 * time.Second

// ClaimGeneration examines at most one due unlocked job. It returns nil when
// there is no candidate or that candidate is cancelled/skipped instead. Claims
// take job locks only. Recovery/accounting is always a separate transaction.
func (s *Store) ClaimGeneration(ctx context.Context) (*app.GenerationJob, error) {
	return s.claimGeneration(ctx, nil)
}

func (s *Store) claimGeneration(ctx context.Context, override *time.Time) (*app.GenerationJob, error) {
	var claimed *app.GenerationJob
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		now, err := q.generationClock(ctx, override)
		if err != nil {
			return err
		}
		var id app.ID
		err = q.queryer.QueryRowContext(ctx, `SELECT id FROM generation_jobs
			WHERE expires_at>$1 AND available_at<=$1 AND
			(status IN ('pending','retry_wait') OR (status='running' AND lease_expires_at<=$1))
			ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&id)
		if errors.Is(databaseError(ctx, err), app.ErrNotFound) {
			return nil
		}
		if err != nil {
			return databaseError(ctx, err)
		}
		before, err := q.GenerationJobByID(ctx, id)
		if err != nil {
			return err
		}
		now, err = q.generationClock(ctx, override)
		if err != nil {
			return err
		}
		if !now.Before(before.ExpiresAt) {
			return q.skipExpiredGenerationJob(ctx, before, now)
		}
		if before.TriggerKind == app.TriggerRepost && before.SourceRepostID == nil {
			// FK SET NULL is durable proof of removal under this job lock. Do not
			// acquire a source lock here: deletion takes source locks before jobs.
			after := before
			after.Status, after.ReasonCode, after.FinishedAt, after.LeaseExpiresAt = app.JobCancelled, "source_removed", &now, nil
			if app.ValidateGenerationSourceInvalidation(before, after, before.LeaseVersion, now) != nil {
				return app.ErrUnavailable
			}
			result, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs
				SET status='cancelled',reason_code='source_removed',finished_at=$3,lease_expires_at=NULL
				WHERE id=$1 AND lease_version=$2 AND status=$4 AND source_repost_id IS NULL`, id, before.LeaseVersion, now, before.Status)
			return generationClaimMutation(ctx, result, err)
		}
		if before.LeaseVersion == math.MaxInt64 {
			return app.ErrUnavailable
		}
		after := before
		expires := now.Add(generationLeaseDuration)
		after.Status, after.LeaseVersion, after.LeaseExpiresAt, after.ReasonCode = app.JobRunning, before.LeaseVersion+1, &expires, ""
		if app.ValidateGenerationJobTransition(before, after, before.LeaseVersion, now, nil) != nil {
			return app.ErrConflict
		}
		result, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status='running',lease_version=$2,lease_expires_at=$3,reason_code=NULL
			WHERE id=$1 AND lease_version=$4 AND status=$5`, id, after.LeaseVersion, expires, before.LeaseVersion, before.Status)
		if err := generationClaimMutation(ctx, result, err); err != nil {
			return err
		}
		claimed = &after
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// RenewGeneration cannot revive an expired lease, even if no other owner has
// claimed it. The returned time is the committed lease expiry.
func (s *Store) RenewGeneration(ctx context.Context, id app.ID, version int64) (time.Time, error) {
	return s.renewGeneration(ctx, id, version, nil)
}

func (s *Store) renewGeneration(ctx context.Context, id app.ID, version int64, override *time.Time) (time.Time, error) {
	var expiry time.Time
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		job, err := q.lockExecutionJob(ctx, id)
		if err != nil {
			return err
		}
		now, err := q.generationClock(ctx, override)
		if err != nil {
			return err
		}
		if job.ValidateLease(version, now) != nil {
			return app.ErrConflict
		}
		if job.TriggerKind == app.TriggerRepost && job.SourceRepostID == nil {
			return app.ErrConflict
		}
		expiry = now.Add(generationLeaseDuration)
		if !expiry.After(*job.LeaseExpiresAt) {
			expiry = *job.LeaseExpiresAt
			return nil
		}
		after := job
		after.LeaseExpiresAt = &expiry
		if app.ValidateGenerationJobTransition(job, after, version, now, nil) != nil {
			return app.ErrConflict
		}
		result, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET lease_expires_at=$2
			WHERE id=$1 AND lease_version=$3 AND status='running' AND lease_expires_at=$4`, id, expiry, version, job.LeaseExpiresAt)
		return generationClaimMutation(ctx, result, err)
	})
	if err != nil {
		return time.Time{}, err
	}
	return expiry, nil
}

func generationClaimMutation(ctx context.Context, result sql.Result, err error) error {
	if err != nil {
		return databaseError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return databaseError(ctx, err)
	}
	if count != 1 {
		return app.ErrUnavailable
	}
	return nil
}

// Caller must acquire any source/account/settings/budget/root locks BEFORE this.
// Job-only operations must never acquire those earlier-order locks afterward.
func (q *Queries) lockExecutionJob(ctx context.Context, id app.ID) (app.GenerationJob, error) {
	if q.lifetime == nil {
		return app.GenerationJob{}, errGenerationTransaction
	}
	var locked app.ID
	if err := q.queryer.QueryRowContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		return app.GenerationJob{}, databaseError(ctx, err)
	}
	return q.GenerationJobByID(ctx, id)
}
