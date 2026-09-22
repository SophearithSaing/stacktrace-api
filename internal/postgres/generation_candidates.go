package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const generationCandidateLimit = 32

type generationCandidate struct {
	settings app.AgentSettings
	rank     int
}

func generationCandidateRank(id app.ID, handle string, tags []string, source generationSource) int {
	if id == source.actor {
		return -1
	}
	if id == source.priorityAuthor {
		return 0
	}
	for index, mention := range app.GenerationMentions(source.body) {
		if mention == handle {
			return index + 1
		}
	}
	for _, tag := range app.ExtractTags(source.body) {
		if slices.Contains(tags, tag.Slug) {
			return app.MaxGenerationMentions + 1
		}
	}
	return -1
}

// Discovery scans only bounded IDs/handles/topics, never full persona prompts.
// Optional oversized fleets fail closed for triggers, not the human write.
func (q *Queries) generationCandidateIDs(ctx context.Context, source generationSource) ([]app.ID, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(ctx, `WITH fleet AS MATERIALIZED (SELECT agent_id FROM agent_settings ORDER BY agent_id LIMIT $1)
		SELECT a.id,a.handle,a.type,s.enabled,a.disabled_at IS NOT NULL,
		CASE WHEN octet_length(to_json(p.topic_tags)::text)<=4096 THEN to_json(p.topic_tags) END
		FROM fleet f JOIN accounts a ON a.id=f.agent_id JOIN agent_settings s ON s.agent_id=a.id
		JOIN agent_personas p ON p.agent_id=s.agent_id AND p.version=s.persona_version ORDER BY a.id`, maxConfiguredAgents+1)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	defer rows.Close()
	var candidates []generationCandidate
	count := 0
	for rows.Next() {
		var id app.ID
		var handle string
		var accountType app.AccountType
		var enabled, disabled bool
		var data []byte
		if err := rows.Scan(&id, &handle, &accountType, &enabled, &disabled, &data); err != nil {
			return nil, databaseError(ctx, err)
		}
		count++
		if count > maxConfiguredAgents {
			return nil, nil
		}
		if !enabled || disabled || accountType != app.AccountAgent {
			continue
		}
		var tags []string
		if json.Unmarshal(data, &tags) != nil {
			continue
		}
		if rank := generationCandidateRank(id, handle, tags, source); rank >= 0 {
			candidates = append(candidates, generationCandidate{settings: app.AgentSettings{AgentID: id}, rank: rank})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(ctx, err)
	}
	sortGenerationCandidates(candidates)
	var ids []app.ID
	for _, candidate := range candidates[:min(len(candidates), generationCandidateLimit)] {
		ids = append(ids, candidate.settings.AgentID)
	}
	return ids, nil
}

func sortGenerationCandidates(candidates []generationCandidate) {
	slices.SortFunc(candidates, func(a, b generationCandidate) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		if a.settings.AgentID < b.settings.AgentID {
			return -1
		}
		if a.settings.AgentID > b.settings.AgentID {
			return 1
		}
		return 0
	})
}

// Acquire ALL agent account locks in ID order before ANY settings lock. The
// continuation actor is included with SHARE, never an exclusive actor upgrade.
func (q *Queries) lockGenerationCandidates(ctx context.Context, source generationSource, continuation bool) ([]generationCandidate, error) {
	ids, err := q.generationCandidateIDs(ctx, source)
	if err != nil {
		return nil, err
	}
	if continuation {
		ids = append(ids, source.actor)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	handles := make(map[app.ID]string)
	for _, id := range ids {
		var handle string
		err := q.queryer.QueryRowContext(ctx, `SELECT handle FROM accounts WHERE id=$1 AND type='agent' AND disabled_at IS NULL FOR SHARE`, id).Scan(&handle)
		if errors.Is(databaseError(ctx, err), app.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, databaseError(ctx, err)
		}
		handles[id] = handle
	}
	if continuation && handles[source.actor] == "" {
		return nil, nil
	}
	var candidates []generationCandidate
	for _, id := range ids {
		if id == source.actor || handles[id] == "" {
			continue
		}
		var settings app.AgentSettings
		var policy, tags []byte
		var persona app.Persona
		err := q.queryer.QueryRowContext(ctx, `SELECT s.agent_id,s.persona_version,s.enabled,
			CASE WHEN octet_length(s.policy::text)<=8192 THEN s.policy END,s.next_post_at,
			COALESCE(to_char(s.schedule_date,'YYYY-MM-DD'),''),s.remaining_slots,s.last_published_at,s.updated_at,
			p.instructions,CASE WHEN octet_length(to_json(p.topic_tags)::text)<=4096 THEN to_json(p.topic_tags) END,p.created_at
			FROM agent_settings s JOIN agent_personas p ON p.agent_id=s.agent_id AND p.version=s.persona_version
			WHERE s.agent_id=$1 FOR UPDATE OF s`, id).Scan(&settings.AgentID, &settings.PersonaVersion, &settings.Enabled, &policy, &settings.NextPostAt,
			&settings.ScheduleDate, &settings.RemainingSlots, &settings.LastPublishedAt, &settings.UpdatedAt, &persona.Instructions, &tags, &persona.CreatedAt)
		if errors.Is(databaseError(ctx, err), app.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, databaseError(ctx, err)
		}
		settings.Policy, err = app.DecodeGenerationPolicy(policy)
		persona.AgentID, persona.Version = id, settings.PersonaVersion
		if err != nil || !settings.Enabled || settings.Validate() != nil || json.Unmarshal(tags, &persona.TopicTags) != nil || persona.Validate() != nil {
			continue
		}
		if rank := generationCandidateRank(id, handles[id], persona.TopicTags, source); rank >= 0 {
			candidates = append(candidates, generationCandidate{settings: settings, rank: rank})
		}
	}
	sortGenerationCandidates(candidates)
	return candidates, nil
}
