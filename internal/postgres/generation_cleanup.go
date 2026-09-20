package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const generationCleanupBatch = 32

func (q *Queries) generationClock(ctx context.Context, override *time.Time) (time.Time, error) {
	if override != nil {
		return app.GenerationInstant(*override), nil
	}
	var now time.Time
	if err := q.queryer.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, databaseError(ctx, err)
	}
	return app.GenerationInstant(now), nil
}

// expireGenerationJobs only acquires job locks, never account/settings/source
// locks. Later source cleanup must likewise not acquire earlier-order locks
// while holding a job. Locked jobs are skipped, not waited on or stolen.
func (s *Store) expireGenerationJobs(ctx context.Context, now *time.Time) (int, error) {
	expired := 0
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		cutoff, err := q.generationClock(ctx, now)
		if err != nil {
			return err
		}
		rows, err := q.queryer.QueryContext(ctx, `SELECT id FROM generation_jobs
			WHERE status IN ('pending','retry_wait','running') AND expires_at <= $1
			ORDER BY expires_at,id LIMIT $2 FOR UPDATE SKIP LOCKED`, cutoff, generationCleanupBatch)
		if err != nil {
			return databaseError(ctx, err)
		}
		var ids []app.ID
		for rows.Next() {
			var id app.ID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return databaseError(ctx, err)
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return databaseError(ctx, err)
		}
		clock, err := q.generationClock(ctx, now)
		if err != nil {
			return err
		}
		for _, id := range ids {
			job, err := q.GenerationJobByID(ctx, id)
			if err != nil {
				return err
			}
			if clock.Before(job.ExpiresAt) {
				continue
			} // Backwards DB clock.
			if err := q.skipExpiredGenerationJob(ctx, job, clock); err != nil {
				return err
			}
			expired++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
}

// Caller owns the job lock. Keep the observed fence and all immutable identity,
// attribution and expiry fields; cleanup never renews or increments a lease.
func (q *Queries) skipExpiredGenerationJob(ctx context.Context, before app.GenerationJob, now time.Time) error {
	if q.lifetime == nil {
		return errGenerationTransaction
	}
	after := before
	after.Status, after.ReasonCode, after.FinishedAt, after.LeaseExpiresAt = app.JobSkipped, "stale_trigger", &now, nil
	if app.ValidateGenerationJobTransition(before, after, before.LeaseVersion, now, nil) != nil {
		return app.ErrUnavailable
	}
	updated, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status='skipped',reason_code='stale_trigger',finished_at=$3,lease_expires_at=NULL
		WHERE id=$1 AND lease_version=$2 AND status=$4 AND expires_at <= $3`, before.ID, before.LeaseVersion, now, before.Status)
	if err != nil {
		return databaseError(ctx, err)
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return databaseError(ctx, err)
	}
	if count != 1 {
		return app.ErrUnavailable
	}
	return nil
}
