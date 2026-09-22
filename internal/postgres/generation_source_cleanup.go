package postgres

import (
	"context"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const generationSourceCleanupBatch = 32

// Caller owns the canonical post lock and has already invalidated the source
// (including repost FK nulling). Only job locks are acquired here. Deletion is
// synchronous; cancelling arbitrarily large conversation fanout is not. Any
// remainder is source-ineligible and later expiry/spend/publication checks must
// honor that invalidation even if a cancellation batch has not reached it yet.
func (q *Queries) cancelRemovedGenerationSourceJobs(ctx context.Context, postID app.ID) (int, error) {
	if q.lifetime == nil {
		return 0, errGenerationTransaction
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(ctx, `SELECT j.id FROM generation_jobs j
		JOIN posts p ON p.id=j.source_post_id LEFT JOIN replies r ON r.id=j.source_reply_id
		WHERE j.source_post_id=$1 AND j.status IN ('pending','retry_wait','running')
		AND (p.deleted_at IS NOT NULL OR r.deleted_at IS NOT NULL OR (j.trigger_kind='repost' AND j.source_repost_id IS NULL))
		ORDER BY j.id LIMIT $2 FOR UPDATE OF j SKIP LOCKED`, postID, generationSourceCleanupBatch)
	if err != nil {
		return 0, databaseError(ctx, err)
	}
	var ids []app.ID
	for rows.Next() {
		var id app.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, databaseError(ctx, err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, databaseError(ctx, err)
	}
	now, err := q.generationClock(ctx, nil)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		before, err := q.GenerationJobByID(ctx, id)
		if err != nil {
			return 0, err
		}
		after := before
		after.Status, after.ReasonCode, after.FinishedAt, after.LeaseExpiresAt = app.JobCancelled, "source_removed", &now, nil
		if !now.Before(before.ExpiresAt) {
			after.Status, after.ReasonCode = app.JobSkipped, "stale_trigger"
		}
		if app.ValidateGenerationSourceInvalidation(before, after, before.LeaseVersion, now) != nil {
			return 0, app.ErrUnavailable
		}
		result, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status=$3,reason_code=$4,finished_at=$5,lease_expires_at=NULL
			WHERE id=$1 AND lease_version=$2 AND status=$6`, id, before.LeaseVersion, after.Status, after.ReasonCode, now, before.Status)
		if err != nil {
			return 0, databaseError(ctx, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, databaseError(ctx, err)
		}
		if count != 1 {
			return 0, app.ErrUnavailable
		}
	}
	return len(ids), nil
}
