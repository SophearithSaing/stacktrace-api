package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const maxConfiguredAgents = 1000

// CheckGenerationConfiguration validates all selected personas and settings,
// including paused agents. Counts describe configuration, not execution readiness
// or eligibility. The snapshot is read-only, bounded, and never returns prompts.
func (s *Store) CheckGenerationConfiguration(ctx context.Context) (configured, enabled int, err error) {
	err = s.readSnapshot(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		rows, err := q.queryer.QueryContext(ctx, `SELECT s.agent_id,s.persona_version,s.enabled,
			CASE WHEN octet_length(s.policy::text)<=8192 THEN s.policy END,s.next_post_at,
			COALESCE(to_char(s.schedule_date,'YYYY-MM-DD'),''),s.remaining_slots,s.last_published_at,s.updated_at,
			p.instructions,CASE WHEN octet_length(to_json(p.topic_tags)::text)<=4096 THEN to_json(p.topic_tags) END,p.created_at,a.type
			FROM agent_settings s LEFT JOIN agent_personas p ON p.agent_id=s.agent_id AND p.version=s.persona_version
			LEFT JOIN accounts a ON a.id=s.agent_id ORDER BY s.agent_id LIMIT $1`, maxConfiguredAgents+1)
		if err != nil {
			return databaseError(ctx, err)
		}
		defer rows.Close()
		for rows.Next() {
			if configured == maxConfiguredAgents {
				return errors.New("generation configuration exceeds 1000 agents")
			}
			var settings app.AgentSettings
			var persona app.Persona
			var policy, tags []byte
			var accountType app.AccountType
			if err := rows.Scan(&settings.AgentID, &settings.PersonaVersion, &settings.Enabled, &policy,
				&settings.NextPostAt, &settings.ScheduleDate, &settings.RemainingSlots, &settings.LastPublishedAt, &settings.UpdatedAt,
				&persona.Instructions, &tags, &persona.CreatedAt, &accountType); err != nil {
				return databaseError(ctx, err)
			}
			persona.AgentID, persona.Version = settings.AgentID, settings.PersonaVersion
			settings.Policy, err = app.DecodeGenerationPolicy(policy)
			if err != nil || json.Unmarshal(tags, &persona.TopicTags) != nil || persona.Validate() != nil || settings.Validate() != nil || accountType != app.AccountAgent {
				return app.ErrUnavailable
			}
			configured++
			if settings.Enabled {
				enabled++
			}
		}
		return databaseError(ctx, rows.Err())
	})
	if err != nil {
		return 0, 0, err
	}
	return configured, enabled, nil
}
