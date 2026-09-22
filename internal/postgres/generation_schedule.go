package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const generationScheduleBatch = 32

// GenerationScheduleResult counts committed work only. Counts can accompany an
// error: independent earlier agent transactions are not rolled back by a later
// failure. Invalid optional configurations are skipped without exposing content.
type GenerationScheduleResult struct {
	AgentsVisited int
	InvalidAgents int
	JobsEnqueued  int
	SlotsDenied   int
	JobsExpired   int
}

// ScheduleGeneration performs one bounded enqueue/expiry pass, never a provider
// call. Daily original-post reservations use the agent's local calendar; future
// token accounting remains UTC. Disabled settings and accounts are not scheduled.
func (s *Store) ScheduleGeneration(ctx context.Context) (GenerationScheduleResult, error) {
	return s.scheduleGeneration(ctx, nil, rand.Int64N)
}

// The time override and RNG are private deterministic integration-test inputs.
// Production eligibility always obtains database time after acquiring row locks.
func (s *Store) scheduleGeneration(ctx context.Context, now *time.Time, draw func(int64) int64) (GenerationScheduleResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result GenerationScheduleResult
	ids, err := s.generationScheduleCandidates(ctx)
	if err != nil {
		return result, err
	}
	result.JobsExpired, err = s.expireGenerationJobs(ctx, now)
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		var outcome GenerationScheduleResult
		err := s.Transaction(ctx, func(q *Queries) error {
			var err error
			outcome, err = q.scheduleGenerationAgent(ctx, id, now, draw)
			return err
		})
		if err != nil {
			return result, err
		}
		result.AgentsVisited += outcome.AgentsVisited
		result.InvalidAgents += outcome.InvalidAgents
		result.JobsEnqueued += outcome.JobsEnqueued
		result.SlotsDenied += outcome.SlotsDenied
	}
	return result, nil
}

func (s *Store) generationScheduleCandidates(ctx context.Context) ([]app.ID, error) {
	ctx, cancel := s.queryContext(ctx)
	defer cancel()
	// Bound even discovery to the established configuration ceiling. Read no
	// policy/timezone expressions here: malformed optional config cannot break
	// fleet discovery. The primary-key scan needs no new sorting index.
	rows, err := s.db.QueryContext(ctx, `SELECT s.agent_id,s.updated_at,s.enabled,a.disabled_at IS NOT NULL
		FROM agent_settings s JOIN accounts a ON a.id=s.agent_id ORDER BY s.agent_id LIMIT $1`, maxConfiguredAgents+1)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	defer rows.Close()
	type candidate struct {
		id        app.ID
		inspected time.Time
	}
	var candidates []candidate
	count := 0
	for rows.Next() {
		var candidate candidate
		var enabled, disabled bool
		if err := rows.Scan(&candidate.id, &candidate.inspected, &enabled, &disabled); err != nil {
			return nil, databaseError(ctx, err)
		}
		count++
		if count > maxConfiguredAgents {
			return nil, errors.New("generation configuration exceeds 1000 agents")
		}
		if enabled && !disabled {
			candidates = append(candidates, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return nil, databaseError(ctx, err)
	}
	var clock time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&clock); err != nil {
		return nil, databaseError(ctx, err)
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		// Future operator timestamps get one inspection first; resetting them to
		// database time then restores ordinary oldest-first rotation. They cannot
		// remain behind repeatedly inspected rows until a future date arrives.
		if a.inspected.After(clock) != b.inspected.After(clock) {
			if a.inspected.After(clock) {
				return -1
			}
			return 1
		}
		if order := a.inspected.Compare(b.inspected); order != 0 {
			return order
		}
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	ids := make([]app.ID, 0, min(len(candidates), generationScheduleBatch))
	for _, candidate := range candidates[:min(len(candidates), generationScheduleBatch)] {
		ids = append(ids, candidate.id)
	}
	return ids, nil
}

func (q *Queries) scheduleGenerationAgent(ctx context.Context, agentID app.ID, now *time.Time, draw func(int64) int64) (GenerationScheduleResult, error) {
	var result GenerationScheduleResult
	if q.lifetime == nil {
		return result, errGenerationTransaction
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	// Lock order: account SHARE, settings UPDATE, then job insertion. Do not
	// enter the low-level CreateGenerationJob path after taking settings locks.
	var accountType app.AccountType
	var disabled bool
	if err := q.queryer.QueryRowContext(ctx, `SELECT type,disabled_at IS NOT NULL FROM accounts WHERE id=$1 FOR SHARE`, agentID).Scan(&accountType, &disabled); err != nil {
		return result, databaseError(ctx, err)
	}
	var settings app.AgentSettings
	var policy []byte
	if err := q.queryer.QueryRowContext(ctx, `SELECT agent_id,persona_version,enabled,
		CASE WHEN octet_length(policy::text)<=8192 THEN policy END,next_post_at,
		COALESCE(to_char(schedule_date,'YYYY-MM-DD'),''),remaining_slots,last_published_at,updated_at
		FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, agentID).Scan(&settings.AgentID, &settings.PersonaVersion, &settings.Enabled, &policy,
		&settings.NextPostAt, &settings.ScheduleDate, &settings.RemainingSlots, &settings.LastPublishedAt, &settings.UpdatedAt); err != nil {
		return result, databaseError(ctx, err)
	}
	if disabled || !settings.Enabled {
		return result, nil
	}
	clock, err := q.generationClock(ctx, nil)
	if err != nil {
		return result, err
	}
	// Touch even invalid/zero/exhausted/future schedules for durable fair rotation.
	// This inspection timestamp always uses the real DB clock, also in tests.
	if _, err := q.queryer.ExecContext(ctx, `UPDATE agent_settings SET updated_at=$2 WHERE agent_id=$1`, agentID, clock); err != nil {
		return result, databaseError(ctx, err)
	}
	result.AgentsVisited = 1
	settings.Policy, err = app.DecodeGenerationPolicy(policy)
	if err != nil || accountType != app.AccountAgent || settings.Validate() != nil {
		result.InvalidAgents = 1
		return result, nil
	}
	var persona app.Persona
	var tags []byte
	persona.AgentID, persona.Version = agentID, settings.PersonaVersion
	if err := q.queryer.QueryRowContext(ctx, `SELECT instructions,
		CASE WHEN octet_length(to_json(topic_tags)::text)<=4096 THEN to_json(topic_tags) END,created_at
		FROM agent_personas WHERE agent_id=$1 AND version=$2`, agentID, settings.PersonaVersion).Scan(&persona.Instructions, &tags, &persona.CreatedAt); err != nil {
		return result, databaseError(ctx, err)
	}
	if json.Unmarshal(tags, &persona.TopicTags) != nil || persona.Validate() != nil {
		result.InvalidAgents = 1
		return result, nil
	}
	if now != nil {
		clock = app.GenerationInstant(*now)
	}
	advanced, slot, err := app.AdvanceGenerationSchedule(settings, clock, draw)
	if err != nil {
		result.InvalidAgents = 1
		return result, nil
	}
	if _, err := q.queryer.ExecContext(ctx, `UPDATE agent_settings SET next_post_at=$2,schedule_date=NULLIF($3,'')::date,remaining_slots=$4 WHERE agent_id=$1`,
		agentID, advanced.NextPostAt, advanced.ScheduleDate, advanced.RemainingSlots); err != nil {
		return result, databaseError(ctx, err)
	}
	if slot == nil {
		return result, nil
	}
	// Count retained original-post reservations, not successful publications.
	// Limit the count scan to the cap rather than walking an unbounded history.
	location, _ := time.LoadLocation(settings.Policy.Timezone)
	dayStart, dayEnd := app.GenerationLocalDayBounds(clock, location)
	var reserved int
	if err := q.queryer.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM generation_jobs
		WHERE agent_id=$1 AND output_kind='post' AND created_at >= $2 AND created_at < $3 LIMIT $4) reserved`,
		agentID, dayStart, dayEnd, settings.Policy.ScheduledPostCapPerDay).Scan(&reserved); err != nil {
		return result, databaseError(ctx, err)
	}
	if reserved >= settings.Policy.ScheduledPostCapPerDay {
		result.SlotsDenied = 1
		return result, nil
	}
	// Daily rollover or switching between originals and replies must not reset
	// spacing. Retained reservations count before publication and after cancellation.
	var recentlyReserved bool
	if err := q.queryer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM generation_jobs
		WHERE agent_id=$1 AND output_kind IN ('post','reply','quote') AND created_at > $2)`, agentID, clock.Add(-time.Duration(settings.Policy.MinSpacingSeconds)*time.Second)).Scan(&recentlyReserved); err != nil {
		return result, databaseError(ctx, err)
	}
	if recentlyReserved {
		result.SlotsDenied = 1
		return result, nil
	}
	key, err := app.ScheduledGenerationKey(slot.At)
	if err != nil {
		return result, app.ErrUnavailable
	}
	id := app.NewID()
	job := app.GenerationJob{ID: id, AgentID: agentID, PersonaVersion: settings.PersonaVersion,
		TriggerKind: app.TriggerScheduled, TriggerKey: key, OutputKind: app.OutputPost, RootJobID: id,
		MaxChainDepth: settings.Policy.MaxChainDepth, MaxChainJobs: settings.Policy.MaxChainJobs,
		Status: app.JobPending, AvailableAt: clock, ExpiresAt: slot.ExpiresAt, CreatedAt: clock}
	if job.Validate() != nil {
		return result, app.ErrUnavailable
	}
	inserted, err := q.queryer.ExecContext(ctx, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,
		output_kind,root_job_id,chain_depth,max_chain_depth,max_chain_jobs,status,available_at,expires_at,lease_version,created_at)
		VALUES($1,$2,$3,'scheduled',$4,'post',$1,0,$5,$6,'pending',$7,$8,0,$7)
		ON CONFLICT (agent_id,trigger_key) DO NOTHING`, id, agentID, job.PersonaVersion, key, job.MaxChainDepth, job.MaxChainJobs, clock, job.ExpiresAt)
	if err != nil {
		return result, databaseError(ctx, err)
	}
	rows, err := inserted.RowsAffected()
	if err != nil {
		return result, databaseError(ctx, err)
	}
	result.JobsEnqueued, result.SlotsDenied = int(rows), 1-int(rows)
	return result, nil
}
