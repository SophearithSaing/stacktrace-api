package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// Caller owns the job lock and has checked a fresh exclusive lease. This helper
// cannot publish or reopen failed jobs; no account/settings/budget lock follows.
func (q *Queries) finishExecutionJob(ctx context.Context, before app.GenerationJob, status app.GenerationJobStatus, reason string, now, available time.Time) error {
	if q.lifetime == nil {
		return errGenerationTransaction
	}
	if before.Status != app.JobRunning || before.ValidateLease(before.LeaseVersion, now) != nil ||
		(status != app.JobRetryWait && status != app.JobSkipped && status != app.JobCancelled && status != app.JobFailed) {
		return app.ErrConflict
	}
	after := before
	after.Status, after.ReasonCode, after.LeaseExpiresAt = status, reason, nil
	if status == app.JobRetryWait {
		after.AvailableAt, after.FinishedAt = available, nil
	} else {
		after.FinishedAt = &now
	}
	if app.ValidateGenerationJobTransition(before, after, before.LeaseVersion, now, nil) != nil {
		return app.ErrConflict
	}
	result, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status=$2,reason_code=$3,lease_expires_at=NULL,available_at=$4,finished_at=$5
		WHERE id=$1 AND lease_version=$6 AND status='running'`, before.ID, status, reason, after.AvailableAt, after.FinishedAt, before.LeaseVersion)
	return generationClaimMutation(ctx, result, err)
}
