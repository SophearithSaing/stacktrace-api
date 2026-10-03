package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// CreateAndSelectPersona atomically creates an immutable version and selects it
// for future jobs. Failure (including missing settings) rolls back the insert.
func (s *Store) CreateAndSelectPersona(ctx context.Context, persona app.Persona) error {
	return s.Transaction(ctx, func(q *Queries) error {
		if err := q.CreatePersona(ctx, persona); err != nil {
			return err
		}
		return q.SelectAgentPersona(ctx, persona.AgentID, persona.Version)
	})
}

// SelectAgentPersona atomically selects an existing persona as the agent's active version.
func (s *Store) SelectAgentPersona(ctx context.Context, agentID app.ID, version int) error {
	return s.Transaction(ctx, func(q *Queries) error { return q.SelectAgentPersona(ctx, agentID, version) })
}

// SelectAgentPersona atomically selects an existing persona as the agent's active version.
func (q *Queries) SelectAgentPersona(ctx context.Context, agentID app.ID, version int) error {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	if _, err := q.lockControlledAgent(ctx, agentID); err != nil {
		return err
	}
	if err := q.lockControlledSettings(ctx, agentID); err != nil {
		return err
	}
	if _, err := q.executionPersona(ctx, app.GenerationJob{AgentID: agentID, PersonaVersion: version}); err != nil {
		return err
	}
	result, err := q.queryer.ExecContext(ctx, `UPDATE agent_settings SET persona_version=$2,updated_at=clock_timestamp() WHERE agent_id=$1`, agentID, version)
	return generationClaimMutation(ctx, result, err)
}

// SetAgentPolicy accepts a complete validated policy, never a partial JSON merge.
// JSON callers must use app.DecodeGenerationPolicy before this boundary.
func (s *Store) SetAgentPolicy(ctx context.Context, agentID app.ID, policy app.GenerationPolicy) error {
	if policy.Validate() != nil {
		return invalidGeneration("policy")
	}
	return s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		if _, err := q.lockControlledAgent(ctx, agentID); err != nil {
			return err
		}
		if err := q.lockControlledSettings(ctx, agentID); err != nil {
			return err
		}
		settings, err := q.readAgentSettings(ctx, agentID)
		if err != nil {
			return err
		}
		now, err := q.generationClock(ctx, nil)
		if err != nil {
			return err
		}
		changed, err := app.ChangeGenerationPolicy(settings, policy, now)
		if err != nil {
			return invalidGeneration("policy")
		}
		encoded, err := json.Marshal(policy)
		if err != nil {
			return invalidGeneration("policy")
		}
		result, err := q.queryer.ExecContext(ctx, `UPDATE agent_settings SET policy=$2,next_post_at=$3,
			schedule_date=NULLIF($4,'')::date,remaining_slots=$5,updated_at=$6 WHERE agent_id=$1`,
			agentID, encoded, changed.NextPostAt, changed.ScheduleDate, changed.RemainingSlots, now)
		return generationClaimMutation(ctx, result, err)
	})
}

// PauseAgent disables generation for one agent.
func (s *Store) PauseAgent(ctx context.Context, agentID app.ID) error {
	_, err := s.setAgentsEnabled(ctx, &agentID, false)
	return err
}

// ResumeAgent enables generation for one validly configured agent.
func (s *Store) ResumeAgent(ctx context.Context, agentID app.ID) error {
	_, err := s.setAgentsEnabled(ctx, &agentID, true)
	return err
}

// PauseAllAgents affects existing configured agents, not future fleet members.
// Every targeted pause advances the revision, even if already paused.
func (s *Store) PauseAllAgents(ctx context.Context) (int, error) {
	return s.setAgentsEnabled(ctx, nil, false)
}

// ResumeAllAgents explicitly enables every valid, non-disabled configured agent,
// INCLUDING initially disabled seeds. It is not restoration of a previous set.
// The result counts targeted valid settings, including already enabled ones.
func (s *Store) ResumeAllAgents(ctx context.Context) (int, error) {
	return s.setAgentsEnabled(ctx, nil, true)
}

// setAgentsEnabled atomically changes generation enablement for one agent or the fleet.
func (s *Store) setAgentsEnabled(ctx context.Context, agentID *app.ID, enabled bool) (int, error) {
	count := 0
	err := s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		var ids []app.ID
		if agentID != nil {
			ids = []app.ID{*agentID}
		} else {
			rows, err := q.queryer.QueryContext(ctx, `SELECT agent_id FROM agent_settings ORDER BY agent_id LIMIT $1`, maxConfiguredAgents+1)
			if err != nil {
				return databaseError(ctx, err)
			}
			ids, err = contextIDs(ctx, rows)
			if err != nil {
				return err
			}
			if len(ids) > maxConfiguredAgents {
				return invalidGeneration("agents")
			}
		}
		// ALL accounts first, then ALL settings in the same deterministic ID order
		// as publication. Never acquire a source, job, budget or attempt lock here.
		disabled := make(map[app.ID]bool, len(ids))
		for _, id := range ids {
			var err error
			disabled[id], err = q.lockControlledAgent(ctx, id)
			if err != nil {
				return err
			}
		}
		for _, id := range ids {
			if err := q.lockControlledSettings(ctx, id); err != nil {
				return err
			}
			if enabled {
				if disabled[id] {
					if agentID != nil {
						return app.ErrForbidden
					}
					continue
				}
				if err := q.validateControlledResume(ctx, id); err != nil {
					var invalid *app.ValidationError
					if agentID == nil && (errors.As(err, &invalid) || errors.Is(err, app.ErrGenerationOutput) || errors.Is(err, app.ErrNotFound)) {
						continue
					}
					return err
				}
			}
			result, err := q.queryer.ExecContext(ctx, `UPDATE agent_settings SET enabled=$2,
				pause_revision=pause_revision+CASE WHEN $2 THEN 0 ELSE 1 END,updated_at=clock_timestamp() WHERE agent_id=$1`, id, enabled)
			if err := generationClaimMutation(ctx, result, err); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err // Never report partial/ambiguous changes as committed.
	}
	return count, nil
}

// lockControlledAgent locks an agent account and reports whether it is active.
func (q *Queries) lockControlledAgent(ctx context.Context, id app.ID) (bool, error) {
	if q.lifetime == nil {
		return false, errGenerationTransaction
	}
	parsed, err := app.ParseID(string(id))
	if err != nil || parsed != id || id == "00000000-0000-0000-0000-000000000000" {
		return false, invalidGeneration("agent_id")
	}
	var accountType app.AccountType
	var disabled bool
	if err := q.queryer.QueryRowContext(ctx, `SELECT type,disabled_at IS NOT NULL FROM accounts WHERE id=$1 FOR SHARE`, id).Scan(&accountType, &disabled); err != nil {
		return false, databaseError(ctx, err)
	}
	if accountType != app.AccountAgent {
		return false, invalidGeneration("agent_id")
	}
	return disabled, nil
}

// lockControlledSettings locks and verifies an agent settings row.
func (q *Queries) lockControlledSettings(ctx context.Context, id app.ID) error {
	var locked app.ID
	err := q.queryer.QueryRowContext(ctx, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, id).Scan(&locked)
	return databaseError(ctx, err)
}

// validateControlledResume checks that a locked agent can safely resume generation.
func (q *Queries) validateControlledResume(ctx context.Context, id app.ID) error {
	settings, err := q.readAgentSettings(ctx, id)
	if err != nil {
		return err
	}
	if settings.RemainingSlots > 0 && settings.NextPostAt == nil {
		return invalidGeneration("settings")
	}
	_, err = q.executionPersona(ctx, app.GenerationJob{AgentID: id, PersonaVersion: settings.PersonaVersion})
	return err
}
