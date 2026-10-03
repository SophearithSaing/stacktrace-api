package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// lockExecutionEligibility locks and loads source, account, and settings eligibility state.
// Acquire source -> agent account SHARE -> settings UPDATE. Job references are
// read without locking first; callers acquire budget/root/job locks afterward.
// This single-agent preparation is NOT the publisher's multi-agent locking path:
// publication must lock all candidate/publisher accounts, then settings, sorted.
func (q *Queries) lockExecutionEligibility(ctx context.Context, job app.GenerationJob) (app.GenerationPolicy, *time.Time, string, error) {
	if q.lifetime == nil {
		return app.GenerationPolicy{}, nil, "", errGenerationTransaction
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	valid, err := q.executionSourceValid(ctx, job, true)
	if err != nil {
		return app.GenerationPolicy{}, nil, "", err
	}
	if !valid {
		return app.GenerationPolicy{}, nil, "source_removed", nil
	}
	var accountType app.AccountType
	var disabled bool
	if err := q.queryer.QueryRowContext(ctx, `SELECT type,disabled_at IS NOT NULL FROM accounts WHERE id=$1 FOR SHARE`, job.AgentID).Scan(&accountType, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return app.GenerationPolicy{}, nil, "policy_disabled", nil
		}
		return app.GenerationPolicy{}, nil, "", databaseError(ctx, err)
	}
	var settings app.AgentSettings
	var encoded []byte
	if err := q.queryer.QueryRowContext(ctx, `SELECT agent_id,persona_version,enabled,
		CASE WHEN octet_length(policy::text)<=8192 THEN policy END,next_post_at,
		COALESCE(to_char(schedule_date,'YYYY-MM-DD'),''),remaining_slots,last_published_at,updated_at
		FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, job.AgentID).Scan(&settings.AgentID, &settings.PersonaVersion, &settings.Enabled, &encoded,
		&settings.NextPostAt, &settings.ScheduleDate, &settings.RemainingSlots, &settings.LastPublishedAt, &settings.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return app.GenerationPolicy{}, nil, "policy_disabled", nil
		}
		return app.GenerationPolicy{}, nil, "", databaseError(ctx, err)
	}
	settings.Policy, err = app.DecodeGenerationPolicy(encoded)
	if err != nil || disabled || !settings.Enabled || accountType != app.AccountAgent || settings.Validate() != nil || settings.RemainingSlots > 0 && settings.NextPostAt == nil {
		return app.GenerationPolicy{}, nil, "policy_disabled", nil
	}
	// Settings may select a new version. Already enqueued work keeps its immutable
	// persona; selection changes alone do not repin or invalidate existing work.
	if _, err := q.executionPersona(ctx, job); err != nil {
		return app.GenerationPolicy{}, nil, "", err
	}
	return settings.Policy, settings.LastPublishedAt, "", nil
}

// executionPolicyAllowed rechecks quota, timing, and policy eligibility for a locked job.
// Retained reservations are ranked by (created_at,id), including terminal jobs.
// Quotas use the enqueue instant's local day under the current policy timezone,
// NOT publication's day; midnight cannot refund or double-consume a reservation.
// Earlier reservations win stricter caps/cooldowns. Later work cannot invalidate
// a legitimate earlier member. Actual publication spacing still uses today's
// clock and last_published_at, independently of the reservation day.
// Caller holds source/account/settings and then budget/root/job locks, and must
// sample now AFTER those locks. This does not grant lease or financial authority.
func (q *Queries) executionPolicyAllowed(ctx context.Context, job app.GenerationJob, p app.GenerationPolicy, lastPublished *time.Time, now time.Time) (bool, error) {
	if q.lifetime == nil {
		return false, errGenerationTransaction
	}
	if job.Validate() != nil || p.Validate() != nil || now.IsZero() || now.Before(job.CreatedAt) || !now.Before(job.ExpiresAt) {
		return false, nil
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	if p.DailyTokenBudget == 0 || lastPublished != nil && now.Before(lastPublished.Add(time.Duration(p.MinSpacingSeconds)*time.Second)) {
		return false, nil
	}
	var blocked bool
	err := q.queryer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM generation_jobs
		WHERE agent_id=$1 AND output_kind IN ('post','reply','quote') AND (created_at,id)<($2,$3)
		AND created_at>$4)`, job.AgentID, job.CreatedAt, job.ID, job.CreatedAt.Add(-time.Duration(p.MinSpacingSeconds)*time.Second)).Scan(&blocked)
	if err != nil || blocked {
		return false, databaseError(ctx, err)
	}
	location, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return false, nil
	}
	start, end := app.GenerationLocalDayBounds(job.CreatedAt, location)
	cap := p.ReplyCapPerDay
	if job.OutputKind == app.OutputPost {
		cap = min(p.ScheduledPostCapPerDay, p.ScheduledMaxPerDay)
	}
	if cap == 0 {
		return false, nil
	}
	var count int
	// Quotes are social responses too: reply and quote reservations share both
	// response quotas. Enqueue currently emits only replies; keep future quote
	// admission aligned with this execution contract.
	err = q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs
		WHERE agent_id=$1 AND (($2::text='post' AND output_kind='post') OR ($2::text<>'post' AND output_kind IN ('reply','quote')))
		AND created_at >= $3 AND created_at<$4 AND (created_at,id)<=($5,$6) LIMIT $7) reserved`, job.AgentID, job.OutputKind, start, end, job.CreatedAt, job.ID, cap+1).Scan(&count)
	if err != nil || count > cap {
		return false, databaseError(ctx, err)
	}
	if job.TriggerKind == app.TriggerScheduled {
		return executionTriggerTimeAllowed(job, p, time.Time{}, now), nil
	}
	if generationProbability(p, job.TriggerKind) == 0 || p.MaxAgentsPerTrigger == 0 || p.ReplyCapPerConversation == 0 {
		return false, nil
	}
	err = q.queryer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM generation_jobs
		WHERE cooldown_key=$1 AND (created_at,id)<($2,$3) AND created_at >= $4)`, job.CooldownKey, job.CreatedAt, job.ID, job.CreatedAt.Add(-time.Duration(p.CooldownSeconds)*time.Second)).Scan(&blocked)
	if err != nil || blocked {
		return false, databaseError(ctx, err)
	}
	err = q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs
		WHERE agent_id=$1 AND source_post_id=$2 AND output_kind IN ('reply','quote') AND (created_at,id)<=($3,$4) LIMIT $5) reserved`, job.AgentID, job.SourcePostID, job.CreatedAt, job.ID, p.ReplyCapPerConversation+1).Scan(&count)
	if err != nil || count > p.ReplyCapPerConversation {
		return false, databaseError(ctx, err)
	}
	// Source age changes can tighten eligibility but never extend pinned expiry.
	sourceCreated, err := q.executionSourceCreatedAt(ctx, job)
	if err != nil {
		return false, err
	}
	if !executionTriggerTimeAllowed(job, p, sourceCreated, now) {
		return false, nil
	}
	// Direct actions count only direct roots; continuations count only their own
	// chain's action. Neither scope can consume the other's reservation.
	err = q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs
		WHERE trigger_key=$1 AND (($2::integer=0 AND chain_depth=0) OR ($2::integer>0 AND chain_depth>0 AND root_job_id=$3))
		AND (created_at,id)<=($4,$5) LIMIT $6) reserved`, job.TriggerKey, job.ChainDepth, job.RootJobID, job.CreatedAt, job.ID, p.MaxAgentsPerTrigger+1).Scan(&count)
	if err != nil || count > p.MaxAgentsPerTrigger {
		return false, databaseError(ctx, err)
	}
	if job.TriggerKind != app.TriggerContinuation {
		if p.HumanTriggerCapPerWindow == 0 {
			return false, nil
		}
		err = q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs
			WHERE trigger_actor_id=$1 AND trigger_kind IN ('reply','repost','quote','human_post')
			AND created_at >= $2 AND (created_at,id)<=($3,$4) LIMIT $5) reserved`, job.TriggerActorID, job.CreatedAt.Add(-time.Duration(p.HumanTriggerWindowSeconds)*time.Second), job.CreatedAt, job.ID, p.HumanTriggerCapPerWindow+1).Scan(&count)
		return err == nil && count <= p.HumanTriggerCapPerWindow, databaseError(ctx, err)
	}
	root, err := q.GenerationJobByID(ctx, job.RootJobID)
	if err != nil {
		return false, err
	}
	if root.ChainDepth != 0 || root.MaxChainDepth != job.MaxChainDepth || root.MaxChainJobs != job.MaxChainJobs || root.CreatedAt.After(job.CreatedAt) || job.ChainDepth > min(root.MaxChainDepth, p.MaxChainDepth) {
		return false, nil
	}
	cap = min(root.MaxChainJobs, p.MaxChainJobs)
	// The causal root always consumes capacity, even when a child's UUID sorts
	// first at the same timestamp. Other members keep stable reservation rank.
	err = q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs
		WHERE root_job_id=$1 AND (id=$1 OR (created_at,id)<=($2,$3)) LIMIT $4) reserved`, job.RootJobID, job.CreatedAt, job.ID, cap+1).Scan(&count)
	return err == nil && count <= cap, databaseError(ctx, err)
}

// executionSourceCreatedAt returns the locked generation source's creation time.
// Read before the final admission clock. The source rows are already locked.
func (q *Queries) executionSourceCreatedAt(ctx context.Context, job app.GenerationJob) (time.Time, error) {
	var created time.Time
	err := q.queryer.QueryRowContext(ctx, `SELECT COALESCE(r.created_at,rp.created_at,p.created_at)
		FROM posts p LEFT JOIN replies r ON r.id=$2 LEFT JOIN reposts rp ON rp.id=$3 WHERE p.id=$1`, job.SourcePostID, job.SourceReplyID, job.SourceRepostID).Scan(&created)
	return created, databaseError(ctx, err)
}

// executionTriggerTimeAllowed reports whether current trigger timing satisfies policy.
// Recheck the time-sensitive trigger restrictions without more SQL after the
// final clock. Retained quota/cooldown ranks depend on CreatedAt, not this clock.
func executionTriggerTimeAllowed(job app.GenerationJob, p app.GenerationPolicy, sourceCreated, now time.Time) bool {
	if job.TriggerKind == app.TriggerScheduled {
		return app.ScheduledGenerationExecutionAllowed(job.TriggerKey, p, job.CreatedAt, job.ExpiresAt, now)
	}
	return !sourceCreated.IsZero() && !now.Before(sourceCreated) && now.Before(sourceCreated.Add(time.Duration(p.SourceMaxAgeSeconds)*time.Second))
}
