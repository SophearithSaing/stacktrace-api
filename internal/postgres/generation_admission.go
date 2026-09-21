package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func generationProbability(policy app.GenerationPolicy, kind app.GenerationTrigger) int {
	switch kind {
	case app.TriggerReply:
		return policy.ReplyProbabilityBPS
	case app.TriggerRepost:
		return policy.RepostProbabilityBPS
	case app.TriggerQuote:
		return policy.QuoteProbabilityBPS
	case app.TriggerHumanPost:
		return policy.HumanPostProbabilityBPS
	case app.TriggerContinuation:
		return policy.ContinuationProbabilityBPS
	}
	return 0
}

func (q *Queries) admitGenerationSource(ctx context.Context, source generationSource, parent *app.GenerationJob, now *time.Time, draw func(int64) int64) (int, error) {
	candidates, err := q.lockGenerationCandidates(ctx, source, parent != nil)
	if err != nil {
		return 0, err
	}
	// Compute conservative shared action/human limits BEFORE random selection.
	// Zero caps disable that candidate; they never loosen another candidate's cap.
	eligible := candidates[:0]
	actionCap, humanCap, humanWindow := 10, 1000, 0
	for _, candidate := range candidates {
		p := candidate.settings.Policy
		if generationProbability(p, source.kind) == 0 || p.MaxAgentsPerTrigger == 0 || p.ReplyCapPerDay == 0 || p.ReplyCapPerConversation == 0 {
			continue
		}
		if parent == nil && p.HumanTriggerCapPerWindow == 0 || parent != nil && p.MaxChainDepth == 0 {
			continue
		}
		eligible = append(eligible, candidate)
		actionCap = min(actionCap, p.MaxAgentsPerTrigger)
		if parent == nil {
			humanCap = min(humanCap, p.HumanTriggerCapPerWindow)
			humanWindow = max(humanWindow, p.HumanTriggerWindowSeconds)
		}
	}
	if len(eligible) == 0 {
		return 0, nil
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var root app.GenerationJob
	chainJobs := 0
	if parent != nil {
		var id app.ID
		if err := q.queryer.QueryRowContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, parent.RootJobID).Scan(&id); err != nil {
			return 0, databaseError(ctx, err)
		}
		root, err = q.GenerationJobByID(ctx, id)
		if err != nil {
			return 0, err
		}
		if err := q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs WHERE root_job_id=$1 LIMIT $2) chain`, root.ID, root.MaxChainJobs).Scan(&chainJobs); err != nil {
			return 0, databaseError(ctx, err)
		}
	}
	clock, err := q.generationClock(ctx, now)
	if err != nil {
		return 0, err
	}
	if clock.Before(source.created) {
		return 0, nil
	}
	humanJobs := 0
	if parent == nil {
		if err := q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs
			WHERE trigger_actor_id=$1 AND trigger_kind IN ('reply','repost','quote','human_post') AND created_at >= $2 LIMIT $3) reservations`,
			source.actor, clock.Add(-time.Duration(humanWindow)*time.Second), humanCap).Scan(&humanJobs); err != nil {
			return 0, databaseError(ctx, err)
		}
	}
	key, err := app.SocialGenerationKey(source.kind, source.action)
	if err != nil {
		return 0, app.ErrUnavailable
	}
	priorActionJobs := 0
	if parent != nil {
		// Unlike fresh human hooks, this trusted primitive can be retried. The
		// root index bounds this history by the immutable <=100-job chain limit.
		if err := q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs WHERE root_job_id=$1 AND trigger_key=$2 LIMIT $3) reserved`, root.ID, key, actionCap).Scan(&priorActionJobs); err != nil {
			return 0, databaseError(ctx, err)
		}
	}
	enqueued := 0
	for _, candidate := range eligible {
		if enqueued+priorActionJobs >= actionCap || parent == nil && humanJobs >= humanCap {
			break
		}
		s := candidate.settings
		p := s.Policy
		if parent != nil && (parent.ChainDepth+1 > min(root.MaxChainDepth, p.MaxChainDepth) || chainJobs >= min(root.MaxChainJobs, p.MaxChainJobs)) {
			continue
		}
		probability := generationProbability(p, source.kind)
		if probability < 10000 {
			if draw == nil {
				return 0, app.ErrUnavailable
			}
			sample := draw(10000)
			if sample < 0 || sample >= 10000 {
				return 0, app.ErrUnavailable
			}
			if sample >= int64(probability) {
				continue
			}
		}
		available, expires, err := app.GenerationResponseTiming(p, source.created, clock, draw)
		if err != nil {
			continue
		} // Expired source: never extend its age on replay.
		cooldown, err := app.GenerationCooldownKey(source.actor, s.AgentID, source.post, source.kind)
		if err != nil {
			return 0, app.ErrUnavailable
		}
		allowed, err := q.generationReplyAllowance(ctx, s, source.post, cooldown, clock)
		if err != nil {
			return 0, err
		}
		if !allowed {
			continue
		}
		id := app.NewID()
		job := app.GenerationJob{ID: id, AgentID: s.AgentID, PersonaVersion: s.PersonaVersion, TriggerKind: source.kind, TriggerKey: key,
			TriggerActorID: &source.actor, CooldownKey: cooldown, SourcePostID: &source.post, SourceReplyID: source.reply, SourceRepostID: source.repost,
			OutputKind: app.OutputReply, RootJobID: id, MaxChainDepth: p.MaxChainDepth, MaxChainJobs: p.MaxChainJobs,
			Status: app.JobPending, CreatedAt: clock, AvailableAt: available, ExpiresAt: expires}
		if parent != nil {
			job.RootJobID, job.ChainDepth, job.MaxChainDepth, job.MaxChainJobs = root.ID, parent.ChainDepth+1, root.MaxChainDepth, root.MaxChainJobs
			if app.ValidateGenerationContinuation(root, *parent, job) != nil {
				return 0, app.ErrUnavailable
			}
		}
		if job.Validate() != nil {
			return 0, app.ErrUnavailable
		}
		inserted, err := q.insertSocialGenerationJob(ctx, job)
		if err != nil {
			return 0, err
		}
		if inserted {
			enqueued++
			if parent == nil {
				humanJobs++
			} else {
				chainJobs++
			}
		}
	}
	return enqueued, nil
}

func (q *Queries) generationReplyAllowance(ctx context.Context, s app.AgentSettings, post app.ID, cooldown string, now time.Time) (bool, error) {
	p := s.Policy
	if s.LastPublishedAt != nil && now.Before(s.LastPublishedAt.Add(time.Duration(p.MinSpacingSeconds)*time.Second)) {
		return false, nil
	}
	var blocked bool
	if err := q.queryer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM generation_jobs WHERE cooldown_key=$1 AND created_at >= $2)
		OR EXISTS(SELECT 1 FROM generation_jobs WHERE agent_id=$3 AND output_kind IN ('post','reply','quote') AND created_at > $4)`,
		cooldown, now.Add(-time.Duration(p.CooldownSeconds)*time.Second), s.AgentID, now.Add(-time.Duration(p.MinSpacingSeconds)*time.Second)).Scan(&blocked); err != nil {
		return false, databaseError(ctx, err)
	}
	if blocked {
		return false, nil
	}
	location, _ := time.LoadLocation(p.Timezone)
	start, end := app.GenerationLocalDayBounds(now, location)
	var daily, conversation int
	if err := q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs WHERE agent_id=$1 AND output_kind='reply' AND created_at >= $2 AND created_at < $3 LIMIT $4) reserved`, s.AgentID, start, end, p.ReplyCapPerDay).Scan(&daily); err != nil {
		return false, databaseError(ctx, err)
	}
	if daily >= p.ReplyCapPerDay {
		return false, nil
	}
	if err := q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs WHERE agent_id=$1 AND output_kind='reply' AND source_post_id=$2 LIMIT $3) reserved`, s.AgentID, post, p.ReplyCapPerConversation).Scan(&conversation); err != nil {
		return false, databaseError(ctx, err)
	}
	return conversation < p.ReplyCapPerConversation, nil
}

// No account re-lock here: caller already owns every earlier-order lock.
func (q *Queries) insertSocialGenerationJob(ctx context.Context, job app.GenerationJob) (bool, error) {
	result, err := q.queryer.ExecContext(ctx, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,
		trigger_actor_id,cooldown_key,source_post_id,source_reply_id,source_repost_id,output_kind,root_job_id,chain_depth,
		max_chain_depth,max_chain_jobs,status,available_at,expires_at,lease_version,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'reply',$11,$12,$13,$14,'pending',$15,$16,0,$17)
		ON CONFLICT(agent_id,trigger_key) DO NOTHING`, job.ID, job.AgentID, job.PersonaVersion, job.TriggerKind, job.TriggerKey,
		job.TriggerActorID, job.CooldownKey, job.SourcePostID, job.SourceReplyID, job.SourceRepostID, job.RootJobID, job.ChainDepth,
		job.MaxChainDepth, job.MaxChainJobs, job.AvailableAt, job.ExpiresAt, job.CreatedAt)
	if err != nil {
		return false, databaseError(ctx, err)
	}
	count, err := result.RowsAffected()
	return count == 1, databaseError(ctx, err)
}
