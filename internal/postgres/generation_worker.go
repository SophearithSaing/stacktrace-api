package postgres

import (
	"context"
	"errors"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// LatestGenerationAttempt is a bounded historical read; it grants no authority.
// No attempts is represented by nil, not an error.
func (q *Queries) LatestGenerationAttempt(ctx context.Context, jobID app.ID) (*app.GenerationAttempt, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var id app.ID
	err := q.queryer.QueryRowContext(ctx, `SELECT id FROM generation_attempts WHERE job_id=$1 ORDER BY attempt_number DESC LIMIT 1`, jobID).Scan(&id)
	if errors.Is(databaseError(ctx, err), app.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	attempt, err := q.GenerationAttemptByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

// ExpireGenerationJobs performs one bounded, job-only cleanup batch.
func (s *Store) ExpireGenerationJobs(ctx context.Context) (int, error) {
	return s.expireGenerationJobs(ctx, nil)
}

// DenyGeneration acknowledges local preparation or publication rejection under
// the current job fence. It never changes attempts, grants retries or succeeds.
// No source, settings, account or budget lock may follow the job lock.
func (s *Store) DenyGeneration(ctx context.Context, id app.ID, version int64, denial app.GenerationDenial) (app.GenerationJobStatus, error) {
	status, valid := denial.TerminalStatus()
	if !valid {
		return "", app.ErrConflict
	}
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		job, err := q.lockExecutionJob(ctx, id)
		if err != nil {
			return err
		}
		if version < 1 || job.LeaseVersion != version {
			return app.ErrConflict
		}
		if job.Status != app.JobRunning {
			if job.Status == status && job.ReasonCode == string(denial) ||
				job.Status == app.JobSkipped && job.ReasonCode == "stale_trigger" ||
				job.Status == app.JobCancelled && job.ReasonCode == "source_removed" {
				status = job.Status
				return nil
			}
			return app.ErrConflict
		}
		now, err := q.generationClock(ctx, nil)
		if err != nil {
			return err
		}
		if !now.Before(job.ExpiresAt) {
			status = app.JobSkipped
			return q.skipExpiredGenerationJob(ctx, job, now)
		}
		if job.TriggerKind == app.TriggerRepost && job.SourceRepostID == nil {
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
		return q.finishExecutionJob(ctx, job, status, string(denial), now, now)
	})
	if err != nil {
		return "", err
	}
	return status, nil
}
