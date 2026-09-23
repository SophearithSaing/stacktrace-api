package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

var errGenerationTransaction = errors.New("generation writes require a transaction")

func invalidGeneration(field string) error {
	return &app.ValidationError{Fields: map[string]string{field: "Invalid generation configuration or record"}}
}

// lockGenerationAgent protects the trusted AccountAgent check for the entire
// write transaction, including initialization's insert-then-read sequence.
func (q *Queries) lockGenerationAgent(ctx context.Context, id app.ID) error {
	if q.lifetime == nil {
		return errGenerationTransaction
	}
	var accountType app.AccountType
	err := q.queryer.QueryRowContext(ctx, `SELECT type FROM accounts WHERE id=$1 FOR SHARE`, id).Scan(&accountType)
	if err != nil {
		return databaseError(ctx, err)
	}
	if accountType != app.AccountAgent {
		return invalidGeneration("agent_id")
	}
	return nil
}

func (s *Store) CreatePersona(ctx context.Context, persona app.Persona) error {
	return s.Transaction(ctx, func(q *Queries) error { return q.CreatePersona(ctx, persona) })
}

// CreatePersona inserts an immutable version; duplicates are conflicts, never
// overwrites. Queries writes require the caller's transaction (e.g. atomic seed).
func (q *Queries) CreatePersona(ctx context.Context, persona app.Persona) error {
	if persona.Validate() != nil {
		return invalidGeneration("persona")
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	if err := q.lockGenerationAgent(ctx, persona.AgentID); err != nil {
		return err
	}
	tags, err := json.Marshal(persona.TopicTags)
	if err != nil {
		return invalidGeneration("persona")
	}
	_, err = q.queryer.ExecContext(ctx, `INSERT INTO agent_personas(agent_id,version,instructions,topic_tags,created_at)
		VALUES ($1,$2,$3,ARRAY(SELECT jsonb_array_elements_text($4::jsonb)),$5)`,
		persona.AgentID, persona.Version, persona.Instructions, tags, persona.CreatedAt)
	return databaseError(ctx, err)
}

func (q *Queries) PersonaByVersion(ctx context.Context, agentID app.ID, version int) (app.Persona, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var persona app.Persona
	var tags []byte
	var accountType app.AccountType
	err := q.queryer.QueryRowContext(ctx, `SELECT p.agent_id,p.version,p.instructions,to_json(p.topic_tags),p.created_at,a.type
		FROM agent_personas p JOIN accounts a ON a.id=p.agent_id WHERE p.agent_id=$1 AND p.version=$2`, agentID, version).Scan(
		&persona.AgentID, &persona.Version, &persona.Instructions, &tags, &persona.CreatedAt, &accountType)
	if err != nil {
		return app.Persona{}, databaseError(ctx, err)
	}
	if json.Unmarshal(tags, &persona.TopicTags) != nil || persona.Validate() != nil || accountType != app.AccountAgent {
		return app.Persona{}, app.ErrUnavailable
	}
	return persona, nil
}

func (s *Store) InitializeAgentSettings(ctx context.Context, settings app.AgentSettings) (app.AgentSettings, error) {
	var saved app.AgentSettings
	err := s.Transaction(ctx, func(q *Queries) error {
		var err error
		saved, err = q.InitializeAgentSettings(ctx, settings)
		return err
	})
	if err != nil {
		return app.AgentSettings{}, err
	}
	return saved, nil
}

// InitializeAgentSettings preserves every existing operator-controlled field.
// A conflict is followed by a fresh statement so concurrent committed settings
// are visible at READ COMMITTED. No UPDATE side effects are used to return a row.
func (q *Queries) InitializeAgentSettings(ctx context.Context, settings app.AgentSettings) (app.AgentSettings, error) {
	if settings.Validate() != nil {
		return app.AgentSettings{}, invalidGeneration("settings")
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	if err := q.lockGenerationAgent(ctx, settings.AgentID); err != nil {
		return app.AgentSettings{}, err
	}
	if _, err := q.PersonaByVersion(ctx, settings.AgentID, settings.PersonaVersion); err != nil {
		return app.AgentSettings{}, err
	}
	policy, err := json.Marshal(settings.Policy)
	if err != nil {
		return app.AgentSettings{}, invalidGeneration("policy")
	}
	_, err = q.queryer.ExecContext(ctx, `INSERT INTO agent_settings(agent_id,persona_version,enabled,policy,next_post_at,
		schedule_date,remaining_slots,last_published_at,updated_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::date,$7,$8,$9)
		ON CONFLICT (agent_id) DO NOTHING`, settings.AgentID, settings.PersonaVersion, settings.Enabled, policy,
		settings.NextPostAt, settings.ScheduleDate, settings.RemainingSlots, settings.LastPublishedAt, settings.UpdatedAt)
	if err != nil {
		return app.AgentSettings{}, databaseError(ctx, err)
	}
	return q.AgentSettingsByID(ctx, settings.AgentID)
}

func (q *Queries) AgentSettingsByID(ctx context.Context, agentID app.ID) (app.AgentSettings, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var settings app.AgentSettings
	var policy []byte
	var accountType app.AccountType
	err := q.queryer.QueryRowContext(ctx, `SELECT s.agent_id,s.persona_version,s.enabled,s.policy,s.next_post_at,
		COALESCE(to_char(s.schedule_date,'YYYY-MM-DD'),''),s.remaining_slots,s.last_published_at,s.updated_at,a.type
		FROM agent_settings s JOIN accounts a ON a.id=s.agent_id WHERE s.agent_id=$1`, agentID).Scan(
		&settings.AgentID, &settings.PersonaVersion, &settings.Enabled, &policy, &settings.NextPostAt,
		&settings.ScheduleDate, &settings.RemainingSlots, &settings.LastPublishedAt, &settings.UpdatedAt, &accountType)
	if err != nil {
		return app.AgentSettings{}, databaseError(ctx, err)
	}
	settings.Policy, err = app.DecodeGenerationPolicy(policy)
	if err != nil || settings.Validate() != nil || accountType != app.AccountAgent {
		return app.AgentSettings{}, app.ErrUnavailable
	}
	return settings, nil
}

func (s *Store) CreateGenerationJob(ctx context.Context, job app.GenerationJob) error {
	return s.Transaction(ctx, func(q *Queries) error { return q.CreateGenerationJob(ctx, job) })
}

// CreateGenerationJob only persists initial pending work. It performs no trigger
// selection, scheduling or quota admission. Future enqueue paths must do those
// checks under their required locks in the same transaction. Duplicate stable
// (agent, trigger_key) identities conflict, even if other submitted fields differ.
func (q *Queries) CreateGenerationJob(ctx context.Context, job app.GenerationJob) error {
	if job.Validate() != nil || job.Status != app.JobPending {
		return invalidGeneration("job")
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	if err := q.lockGenerationAgent(ctx, job.AgentID); err != nil {
		return err
	}
	if _, err := q.PersonaByVersion(ctx, job.AgentID, job.PersonaVersion); err != nil {
		return err
	}
	_, err := q.queryer.ExecContext(ctx, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,
		trigger_actor_id,cooldown_key,source_post_id,source_reply_id,source_repost_id,output_kind,root_job_id,chain_depth,
		status,available_at,expires_at,lease_version,created_at,max_chain_depth,max_chain_jobs)
		VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,'pending',$14,$15,0,$16,$17,$18)`,
		job.ID, job.AgentID, job.PersonaVersion, job.TriggerKind, job.TriggerKey, job.TriggerActorID, job.CooldownKey,
		job.SourcePostID, job.SourceReplyID, job.SourceRepostID, job.OutputKind, job.RootJobID, job.ChainDepth,
		job.AvailableAt, job.ExpiresAt, job.CreatedAt, job.MaxChainDepth, job.MaxChainJobs)
	return databaseError(ctx, err)
}

func (q *Queries) GenerationJobByID(ctx context.Context, id app.ID) (app.GenerationJob, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var job app.GenerationJob
	var accountType app.AccountType
	err := q.queryer.QueryRowContext(ctx, `SELECT j.id,j.agent_id,j.persona_version,j.trigger_kind,j.trigger_key,
		j.trigger_actor_id,COALESCE(j.cooldown_key,''),j.source_post_id,j.source_reply_id,j.source_repost_id,j.output_kind,
		j.root_job_id,j.chain_depth,j.status,j.available_at,j.expires_at,j.lease_version,j.lease_expires_at,
		j.result_post_id,j.result_reply_id,j.published_attempt_id,COALESCE(j.reason_code,''),j.created_at,j.finished_at,
		j.max_chain_depth,j.max_chain_jobs,a.type
		FROM generation_jobs j JOIN accounts a ON a.id=j.agent_id WHERE j.id=$1`, id).Scan(
		&job.ID, &job.AgentID, &job.PersonaVersion, &job.TriggerKind, &job.TriggerKey, &job.TriggerActorID, &job.CooldownKey,
		&job.SourcePostID, &job.SourceReplyID, &job.SourceRepostID, &job.OutputKind, &job.RootJobID, &job.ChainDepth, &job.Status,
		&job.AvailableAt, &job.ExpiresAt, &job.LeaseVersion, &job.LeaseExpiresAt, &job.ResultPostID, &job.ResultReplyID,
		&job.PublishedAttemptID, &job.ReasonCode, &job.CreatedAt, &job.FinishedAt, &job.MaxChainDepth, &job.MaxChainJobs, &accountType)
	if err != nil {
		return app.GenerationJob{}, databaseError(ctx, err)
	}
	if job.Validate() != nil || accountType != app.AccountAgent {
		return app.GenerationJob{}, app.ErrUnavailable
	}
	return job, nil
}

// GenerationAttemptByID exposes historical validated observations only. There is
// intentionally no attempt insertion/outcome API without future budget/lease
// authority and no job transition/publication API in this persistence foundation.
func (q *Queries) GenerationAttemptByID(ctx context.Context, id app.ID) (app.GenerationAttempt, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var attempt app.GenerationAttempt
	var accountType app.AccountType
	err := q.queryer.QueryRowContext(ctx, `SELECT t.id,t.job_id,t.attempt_number,t.lease_version,t.provider,t.model,
		COALESCE(t.provider_request_id,''),t.context_hash,t.context_builder_version,to_char(t.budget_day,'YYYY-MM-DD'),
		t.reserved_tokens,t.input_tokens,t.output_tokens,t.status,COALESCE(t.error_code,''),t.started_at,t.finished_at,
		COALESCE(t.output_digest,''),a.type
		FROM generation_attempts t JOIN generation_jobs j ON j.id=t.job_id JOIN accounts a ON a.id=j.agent_id WHERE t.id=$1`, id).Scan(
		&attempt.ID, &attempt.JobID, &attempt.AttemptNumber, &attempt.LeaseVersion, &attempt.Provider, &attempt.Model,
		&attempt.ProviderRequestID, &attempt.ContextHash, &attempt.ContextBuilderVersion, &attempt.BudgetDay,
		&attempt.ReservedTokens, &attempt.InputTokens, &attempt.OutputTokens, &attempt.Status, &attempt.ErrorCode,
		&attempt.StartedAt, &attempt.FinishedAt, &attempt.OutputDigest, &accountType)
	if err != nil {
		return app.GenerationAttempt{}, databaseError(ctx, err)
	}
	if attempt.Validate() != nil || accountType != app.AccountAgent {
		return app.GenerationAttempt{}, app.ErrUnavailable
	}
	return attempt, nil
}
