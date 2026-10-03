package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/llm"
)

const generationDailyTokenBudget int64 = 500000
const generationBudgetLock int64 = 783245196031 // shared by admission and settlement

// lockGenerationBudget locks the singleton generation budget row.
func (q *Queries) lockGenerationBudget(ctx context.Context) error {
	if q.lifetime == nil {
		return errGenerationTransaction
	}
	_, err := q.queryer.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, generationBudgetLock)
	return databaseError(ctx, err)
}

// ReserveGeneration commits a fresh attempt before the caller may make ONE
// provider call. An ambiguous commit must NOT be replayed as call permission.
// The opaque context is input, not authority. All prompt metadata and the fixed
// reservation are derived here; callers cannot assert a hash or a local estimate.
func (s *Store) ReserveGeneration(ctx context.Context, id app.ID, version int64, input app.GenerationContext) (app.GenerationAdmission, error) {
	return s.reserveGeneration(ctx, id, version, input, nil)
}

// reserveGeneration validates admission and reserves provider token spend.
func (s *Store) reserveGeneration(ctx context.Context, id app.ID, version int64, input app.GenerationContext, override *time.Time) (app.GenerationAdmission, error) {
	var result app.GenerationAdmission
	err := s.Transaction(ctx, func(q *Queries) error {
		var err error
		result, err = q.reserveGeneration(ctx, id, version, input, override)
		return err
	})
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	return result, nil
}

// reserveGeneration validates admission and reserves provider token spend.
func (q *Queries) reserveGeneration(ctx context.Context, id app.ID, version int64, input app.GenerationContext, override *time.Time) (app.GenerationAdmission, error) {
	if q.lifetime == nil {
		return app.GenerationAdmission{}, errGenerationTransaction
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	refs, err := q.GenerationJobByID(ctx, id) // no job lock before sources
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	policy, lastPublished, denial, err := q.lockExecutionEligibility(ctx, refs)
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	if err := q.lockGenerationBudget(ctx); err != nil {
		return app.GenerationAdmission{}, err
	}
	if refs.RootJobID != id {
		if _, err := q.lockExecutionJob(ctx, refs.RootJobID); err != nil {
			return app.GenerationAdmission{}, err
		}
	}
	job, err := q.lockExecutionJob(ctx, id)
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	// Recheck observed state/fence and source references, including the nullable
	// repost FK. All other references are immutable in storage.
	if refs.Status != job.Status || refs.LeaseVersion != job.LeaseVersion || job.Status != app.JobRunning || job.LeaseVersion != version ||
		!sameAttemptSource(refs.SourcePostID, job.SourcePostID) || !sameAttemptSource(refs.SourceReplyID, job.SourceReplyID) || !sameAttemptSource(refs.SourceRepostID, job.SourceRepostID) {
		return app.GenerationAdmission{}, app.ErrConflict
	}
	attempts, err := q.executionAttempts(ctx, id)
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	now, err := q.generationClock(ctx, override) // AFTER every blocking lock
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	if !now.Before(job.ExpiresAt) {
		return app.GenerationAdmission{Reason: "stale_trigger"}, q.skipExpiredGenerationJob(ctx, job, now)
	}
	if job.ValidateLease(version, now) != nil {
		return app.GenerationAdmission{}, app.ErrConflict
	}
	if denial == "" {
		allowed, err := q.executionPolicyAllowed(ctx, job, policy, lastPublished, now)
		if err != nil {
			return app.GenerationAdmission{}, err
		}
		if !allowed {
			denial = "policy_denied"
		}
	}
	if denial != "" {
		status := app.JobSkipped
		if denial == "source_removed" || denial == "policy_disabled" {
			status = app.JobCancelled
		}
		return app.GenerationAdmission{Reason: denial}, q.finishExecutionJob(ctx, job, status, denial, now, now)
	}
	// Eligibility owns the settings lock until commit; no pause can pass between
	// this snapshot and reservation. Settlement never needs that lock.
	var pauseRevision int64
	if err := q.queryer.QueryRowContext(ctx, `SELECT pause_revision FROM agent_settings WHERE agent_id=$1`, job.AgentID).Scan(&pauseRevision); err != nil {
		return app.GenerationAdmission{}, databaseError(ctx, err)
	}
	prompt, err := llm.BuildPrompt(app.GenerationRequest{Job: job, Context: input})
	if err != nil {
		return app.GenerationAdmission{}, invalidGeneration("context")
	}
	reserved, err := prompt.AdmissionReservation()
	if err != nil {
		return app.GenerationAdmission{}, invalidGeneration("preparation")
	}
	for _, attempt := range attempts {
		if attempt.LeaseVersion == version {
			return app.GenerationAdmission{Reason: "already_admitted"}, nil
		}
	}
	invalidOutputs := 0
	for _, attempt := range attempts {
		if attempt.ErrorCode == string(app.GenerationInvalidOutput) {
			invalidOutputs++
		}
	}
	if invalidOutputs > app.MaxGenerationInvalidRegenerations {
		return app.GenerationAdmission{Reason: "invalid_output"}, q.finishExecutionJob(ctx, job, app.JobFailed, "invalid_output", now, now)
	}
	if len(attempts) >= app.MaxGenerationAttempts {
		return app.GenerationAdmission{Reason: "attempt_limit"}, q.finishExecutionJob(ctx, job, app.JobFailed, "attempt_limit", now, now)
	}
	if len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		// Admission does not recover or infer a retry from a historical observation.
		// A separate fenced transition must acknowledge it by advancing availability.
		// This also permits an explicit failed-job retry after lost successful output;
		// the historical success is NEVER publication authority for the new lease.
		if last.FinishedAt == nil || !job.AvailableAt.After(*last.FinishedAt) {
			return app.GenerationAdmission{Reason: "recovery_required"}, nil
		}
	}
	// SUM(bigint) is numeric; overflow on scan fails closed. Both daily sums use
	// the (budget_day,job_id) index and the existing bounded statement deadline.
	// Old/unsupported accounting must not release budget either.
	day := now.UTC().Format(time.DateOnly)
	var global, agent int64
	err = q.queryer.QueryRowContext(ctx, `SELECT COALESCE(sum(cost),0),COALESCE(sum(cost) FILTER (WHERE agent_id=$2),0)
		FROM (SELECT j.agent_id,CASE WHEN t.status IN ('reserved','unknown') OR t.input_tokens IS NULL OR t.output_tokens IS NULL
		OR t.input_tokens<1 OR t.input_tokens>$3 OR t.output_tokens<0 OR t.output_tokens>$4 OR t.error_code='unsupported_accounting'
		THEN t.reserved_tokens ELSE t.input_tokens+t.output_tokens END AS cost
		FROM generation_attempts t JOIN generation_jobs j ON j.id=t.job_id WHERE t.budget_day=$1::date) spent`, day, job.AgentID, llm.MaxInputTokens, llm.MaxOutputTokens).Scan(&global, &agent)
	if err != nil {
		return app.GenerationAdmission{}, databaseError(ctx, err)
	}
	fresh, err := q.generationClock(ctx, override)
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	if !fresh.Before(job.ExpiresAt) {
		return app.GenerationAdmission{Reason: "stale_trigger"}, q.skipExpiredGenerationJob(ctx, job, fresh)
	}
	// Midnight during accounting restarts safely, never charges the previous day.
	if job.ValidateLease(version, fresh) != nil || fresh.UTC().Format(time.DateOnly) != day {
		return app.GenerationAdmission{}, app.ErrConflict
	}
	allowed, err := q.executionPolicyAllowed(ctx, job, policy, lastPublished, fresh)
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	var sourceCreated time.Time
	if allowed && job.TriggerKind != app.TriggerScheduled {
		sourceCreated, err = q.executionSourceCreatedAt(ctx, job)
		if err != nil {
			return app.GenerationAdmission{}, err
		}
	}
	// Policy SQL can consume the remaining lease/trigger lifetime or cross UTC
	// midnight. Finish every read before sampling final authority and StartedAt.
	// A backwards clock also restarts: it could invalidate publication spacing.
	checkedAt := fresh
	fresh, err = q.generationClock(ctx, override)
	if err != nil {
		return app.GenerationAdmission{}, err
	}
	if !fresh.Before(job.ExpiresAt) {
		return app.GenerationAdmission{Reason: "stale_trigger"}, q.skipExpiredGenerationJob(ctx, job, fresh)
	}
	if fresh.Before(checkedAt) || job.ValidateLease(version, fresh) != nil || fresh.UTC().Format(time.DateOnly) != day {
		return app.GenerationAdmission{}, app.ErrConflict
	}
	if !allowed || !executionTriggerTimeAllowed(job, policy, sourceCreated, fresh) {
		return app.GenerationAdmission{Reason: "policy_denied"}, q.finishExecutionJob(ctx, job, app.JobSkipped, "policy_denied", fresh, fresh)
	}
	if global < 0 || agent < 0 || global > generationDailyTokenBudget-reserved || agent > policy.DailyTokenBudget-reserved {
		return app.GenerationAdmission{Reason: "budget_exhausted"}, q.finishExecutionJob(ctx, job, app.JobSkipped, "budget_exhausted", fresh, fresh)
	}
	attempt := app.GenerationAttempt{ID: app.NewID(), JobID: id, AttemptNumber: len(attempts) + 1, LeaseVersion: version,
		Provider: llm.Provider, Model: llm.Model, ContextHash: prompt.Hash(), ContextBuilderVersion: llm.ContextBuilderVersion,
		BudgetDay: day, ReservedTokens: reserved, Status: app.AttemptReserved, StartedAt: fresh, PauseRevision: &pauseRevision}
	if attempt.Validate() != nil {
		return app.GenerationAdmission{}, invalidGeneration("preparation")
	}
	inserted, err := q.queryer.ExecContext(ctx, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
		context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,pause_revision)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'reserved',$11,$12)`, attempt.ID, id, attempt.AttemptNumber, version, attempt.Provider, attempt.Model, attempt.ContextHash, attempt.ContextBuilderVersion, day, reserved, fresh, pauseRevision)
	if err := generationClaimMutation(ctx, inserted, err); err != nil {
		return app.GenerationAdmission{}, err
	}
	return app.GenerationAdmission{Attempt: &attempt}, nil
}

// sameAttemptSource reports whether two optional attempt source identifiers match.
func sameAttemptSource(a, b *app.ID) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// executionAttempts loads bounded attempt history for a locked generation job.
// Bounded even with legacy/operator history. Caller owns the job lock. Admission
// also owns budget before taking attempt locks; job-only callers do not write them.
func (q *Queries) executionAttempts(ctx context.Context, id app.ID) ([]app.GenerationAttempt, error) {
	rows, err := q.queryer.QueryContext(ctx, `SELECT id FROM generation_attempts WHERE job_id=$1 ORDER BY attempt_number LIMIT $2 FOR UPDATE`, id, app.MaxGenerationAttempts+1)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	ids, err := contextIDs(ctx, rows)
	if err != nil {
		return nil, err
	}
	var attempts []app.GenerationAttempt
	for _, id := range ids {
		attempt, err := q.GenerationAttemptByID(ctx, id)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}
