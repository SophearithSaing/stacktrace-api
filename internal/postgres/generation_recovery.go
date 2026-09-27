package postgres

import (
	"context"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// RecoverGenerationAttempts conservatively closes at most 32 abandoned reserved
// observations. Budget -> attempt locks ONLY; job state is read without locks.
// Reconciliation is a separate transaction under the current job fence. Unknown
// observations retain their full reservation and cannot become late successes.
func (s *Store) RecoverGenerationAttempts(ctx context.Context) (int, error) {
	count := 0
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		if err := q.lockGenerationBudget(ctx); err != nil {
			return err
		}
		now, err := q.generationClock(ctx, nil)
		if err != nil {
			return err
		}
		rows, err := q.queryer.QueryContext(ctx, `SELECT t.id FROM generation_attempts t JOIN generation_jobs j ON j.id=t.job_id
			WHERE t.status='reserved' AND (j.status<>'running' OR j.lease_version<>t.lease_version OR j.lease_expires_at<=$1 OR j.expires_at<=$1)
			ORDER BY t.started_at,t.id LIMIT $2 FOR UPDATE OF t SKIP LOCKED`, now, generationCleanupBatch)
		if err != nil {
			return databaseError(ctx, err)
		}
		ids, err := contextIDs(ctx, rows)
		if err != nil {
			return err
		}
		for _, id := range ids {
			before, err := q.GenerationAttemptByID(ctx, id)
			if err != nil {
				return err
			}
			job, err := q.GenerationJobByID(ctx, before.JobID)
			if err != nil {
				return err
			}
			now, err := q.generationClock(ctx, nil)
			if err != nil {
				return err
			}
			if before.Status != app.AttemptReserved || (job.Status == app.JobRunning && job.LeaseVersion == before.LeaseVersion &&
				job.LeaseExpiresAt != nil && now.Before(*job.LeaseExpiresAt) && now.Before(job.ExpiresAt)) {
				continue
			}
			after := before
			after.Status, after.ErrorCode, after.FinishedAt = app.AttemptUnknown, "lease_lost", &now
			if app.ValidateGenerationAttemptTransition(before, after) != nil {
				return app.ErrConflict
			}
			updated, err := q.queryer.ExecContext(ctx, `UPDATE generation_attempts SET status='unknown',error_code='lease_lost',finished_at=$2
				WHERE id=$1 AND status='reserved'`, id, now)
			if err := generationClaimMutation(ctx, updated, err); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
