package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// GenerationContext reads one bounded repeatable-read public snapshot, not spend
// authority. Reservation must recheck the lease, source and current policy.
func (s *Store) GenerationContext(ctx context.Context, id app.ID, version int64) (app.GenerationContext, error) {
	var built app.GenerationContext
	err := s.readSnapshot(ctx, func(q *Queries) error {
		var err error
		built, err = q.generationContext(ctx, id, version)
		return err
	})
	if err != nil {
		return app.GenerationContext{}, err
	}
	return built, nil
}

func (q *Queries) generationContext(ctx context.Context, id app.ID, version int64) (app.GenerationContext, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	job, err := q.GenerationJobByID(ctx, id)
	if err != nil {
		return app.GenerationContext{}, err
	}
	if err := q.contextLease(ctx, job, version); err != nil {
		return app.GenerationContext{}, err
	}
	valid, err := q.executionSourceValid(ctx, job, false)
	if err != nil {
		return app.GenerationContext{}, err
	}
	if !valid {
		return app.GenerationContext{}, app.ErrDeleted
	}
	persona, err := q.executionPersona(ctx, job)
	if err != nil {
		return app.GenerationContext{}, err
	}
	var public app.PublicGenerationContext
	if job.TriggerKind == app.TriggerRepost {
		var actor app.GenerationContextAuthor
		err := q.queryer.QueryRowContext(ctx, `SELECT CASE WHEN octet_length(handle)<=32 THEN handle ELSE '' END,
			CASE WHEN octet_length(type)<=5 THEN type ELSE '' END
			FROM accounts WHERE id=$1 AND disabled_at IS NULL`, job.TriggerActorID).Scan(&actor.Handle, &actor.Type)
		if err != nil {
			return app.GenerationContext{}, databaseError(ctx, err)
		}
		public.RepostActor = &actor
	}
	// Bound each indexed history branch before hydration. At most 32 rows cross
	// the wire, with byte guards on every variable public field. The trigger is
	// selected by its pinned ID, independently of the ten-reply history window.
	rows, err := q.queryer.QueryContext(ctx, `WITH refs AS (
		SELECT 'source' AS section,id,created_at,false AS is_reply FROM posts WHERE id=$1
		UNION ALL SELECT 'trigger',id,created_at,true FROM replies WHERE id=$2
		UNION ALL (SELECT 'replies',r.id,r.created_at,true FROM replies r JOIN accounts a ON a.id=r.author_id
			WHERE r.post_id=$1 AND r.deleted_at IS NULL AND a.disabled_at IS NULL
			ORDER BY r.created_at DESC,r.id DESC LIMIT $4)
		UNION ALL SELECT 'recent',id,created_at,is_reply FROM (
			(SELECT p.id,p.created_at,false AS is_reply FROM posts p JOIN accounts a ON a.id=p.author_id
			 WHERE p.author_id=$3 AND p.deleted_at IS NULL AND a.disabled_at IS NULL
			 ORDER BY p.created_at DESC,p.id DESC LIMIT $5)
			UNION ALL
			(SELECT r.id,r.created_at,true FROM replies r JOIN posts p ON p.id=r.post_id
			 JOIN accounts a ON a.id=r.author_id JOIN accounts parent_author ON parent_author.id=p.author_id
			 WHERE r.author_id=$3 AND r.deleted_at IS NULL AND p.deleted_at IS NULL
			 AND a.disabled_at IS NULL AND parent_author.disabled_at IS NULL
			 ORDER BY r.created_at DESC,r.id DESC LIMIT $5)
			ORDER BY created_at DESC,id DESC,is_reply DESC LIMIT $5
		) recent
	), items AS (
		SELECT refs.*,a.handle,a.type,CASE WHEN refs.is_reply THEN 'reply'
			WHEN p.quoted_post_id IS NULL THEN 'post' ELSE 'quote' END AS kind,
			CASE WHEN refs.is_reply THEN r.body ELSE p.body END AS body,
			p.code_language,p.code_filename,p.code_source
		FROM refs LEFT JOIN posts p ON NOT refs.is_reply AND p.id=refs.id
		LEFT JOIN replies r ON refs.is_reply AND r.id=refs.id
		JOIN accounts a ON a.id=COALESCE(p.author_id,r.author_id)
	)
	SELECT section,CASE WHEN octet_length(handle)<=32 THEN handle ELSE '' END,
		CASE WHEN octet_length(type)<=5 THEN type ELSE '' END,kind,
		CASE WHEN octet_length(body)<=1280 THEN body END,
		CASE WHEN octet_length(code_language)<=32 THEN code_language END,
		CASE WHEN octet_length(code_filename)<=1020 THEN code_filename END,
		CASE WHEN octet_length(code_source)<=20480 THEN code_source END,
		COALESCE(octet_length(code_language)>32 OR octet_length(code_filename)>1020 OR octet_length(code_source)>20480,false)
	FROM items ORDER BY created_at,id,is_reply,section`, job.SourcePostID, job.SourceReplyID, job.AgentID,
		app.MaxGenerationContextReplies, app.MaxGenerationRecentContent)
	if err != nil {
		return app.GenerationContext{}, databaseError(ctx, err)
	}
	defer rows.Close()
	type historyItem struct {
		section string
		item    app.GenerationContextItem
	}
	var history []historyItem
	for rows.Next() {
		var section string
		var item app.GenerationContextItem
		var body, language, filename, source sql.NullString
		var oversized bool
		if err := rows.Scan(&section, &item.Author.Handle, &item.Author.Type, &item.Kind, &body, &language, &filename, &source, &oversized); err != nil {
			return app.GenerationContext{}, databaseError(ctx, err)
		}
		if oversized || !body.Valid || language.Valid != filename.Valid || language.Valid != source.Valid {
			return app.GenerationContext{}, app.ErrGenerationOutput
		}
		var code *app.Code
		if language.Valid {
			code = &app.Code{Language: language.String, Filename: filename.String, Source: source.String}
		}
		// Validate all selected items before dropping optional history. A corrupt
		// old row must not disappear merely because the context is large.
		item.Content, err = app.NewContent(body.String, code)
		if err != nil || item.Kind != app.OutputReply && len(item.Content.Tags) > app.MaxTagsPerPost {
			return app.GenerationContext{}, app.ErrGenerationOutput
		}
		item.Author.Handle, err = app.NormalizeHandle(item.Author.Handle)
		if err != nil || item.Author.Type != app.AccountHuman && item.Author.Type != app.AccountAgent || section == "recent" && item.Author.Type != app.AccountAgent {
			return app.GenerationContext{}, app.ErrGenerationOutput
		}
		switch section {
		case "source":
			public.Source = &item
		case "trigger":
			public.TriggerReply = &item
		default:
			history = append(history, historyItem{section, item})
		}
	}
	if err := rows.Err(); err != nil {
		return app.GenerationContext{}, databaseError(ctx, err)
	}
	rows.Close()
	// Required source, trigger and pinned persona must fit unchanged on their
	// own. Drop optional history globally oldest-first (time, ID, kind, section
	// ties) until the authoritative app builder accepts the byte budget.
	if _, err := app.BuildGenerationContext(job, persona, public); err != nil {
		return app.GenerationContext{}, err
	}
	for {
		public.Replies, public.RecentAgentContent = nil, nil
		for _, entry := range history {
			if entry.section == "replies" {
				public.Replies = append(public.Replies, entry.item)
			} else {
				public.RecentAgentContent = append(public.RecentAgentContent, entry.item)
			}
		}
		built, err := app.BuildGenerationContext(job, persona, public)
		if err == nil {
			if err := q.contextLease(ctx, job, version); err != nil {
				return app.GenerationContext{}, err
			}
			return built, nil
		}
		if len(history) == 0 {
			return app.GenerationContext{}, err
		}
		history = history[1:]
	}
}

func (q *Queries) contextLease(ctx context.Context, job app.GenerationJob, version int64) error {
	now, err := q.generationClock(ctx, nil)
	if err != nil {
		return err
	}
	if job.ValidateLease(version, now) != nil {
		return app.ErrConflict
	}
	return nil
}

// Retained for the interrupted recovery/transition drafts; context hydration no
// longer needs reference-by-reference reads.
func contextIDs(ctx context.Context, rows *sql.Rows) ([]app.ID, error) {
	defer rows.Close()
	var ids []app.ID
	for rows.Next() {
		var id app.ID
		if err := rows.Scan(&id); err != nil {
			return nil, databaseError(ctx, err)
		}
		ids = append(ids, id)
	}
	return ids, databaseError(ctx, rows.Err())
}

func (q *Queries) executionPersona(ctx context.Context, job app.GenerationJob) (app.Persona, error) {
	persona := app.Persona{AgentID: job.AgentID, Version: job.PersonaVersion}
	var instructions sql.NullString
	var tags []byte
	err := q.queryer.QueryRowContext(ctx, `SELECT CASE WHEN octet_length(instructions)<=16000 THEN instructions END,
		CASE WHEN octet_length(to_json(topic_tags)::text)<=4096 THEN to_json(topic_tags) END,created_at
		FROM agent_personas WHERE agent_id=$1 AND version=$2`, job.AgentID, job.PersonaVersion).Scan(&instructions, &tags, &persona.CreatedAt)
	if err != nil {
		return persona, databaseError(ctx, err)
	}
	persona.Instructions = instructions.String
	if !instructions.Valid || json.Unmarshal(tags, &persona.TopicTags) != nil || persona.Validate() != nil {
		return app.Persona{}, app.ErrGenerationOutput
	}
	return persona, nil
}

// Lock only existing canonical sources, post before reply/repost. The caller
// must not already hold job/settings/budget locks. Account state is observed,
// not locked here; later admission owns main-agent/settings authority.
func (q *Queries) executionSourceValid(ctx context.Context, job app.GenerationJob, lock bool) (bool, error) {
	if lock && q.lifetime == nil {
		return false, errGenerationTransaction
	}
	if job.Validate() != nil {
		return false, app.ErrGenerationOutput
	}
	if job.SourcePostID == nil {
		return true, nil
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	suffix := ""
	if lock {
		suffix = " FOR UPDATE OF p"
	}
	var author app.ID
	var quote, deleted bool
	err := q.queryer.QueryRowContext(ctx, `SELECT p.author_id,p.quoted_post_id IS NOT NULL,p.deleted_at IS NOT NULL OR a.disabled_at IS NOT NULL
		FROM posts p JOIN accounts a ON a.id=p.author_id WHERE p.id=$1`+suffix, job.SourcePostID).Scan(&author, &quote, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseError(ctx, err)
	}
	if deleted {
		return false, nil
	}
	if job.SourceReplyID != nil {
		if lock {
			suffix = " FOR UPDATE OF r"
		}
		var post app.ID
		err = q.queryer.QueryRowContext(ctx, `SELECT r.post_id,r.author_id,r.deleted_at IS NOT NULL OR a.disabled_at IS NOT NULL
			FROM replies r JOIN accounts a ON a.id=r.author_id WHERE r.id=$1`+suffix, job.SourceReplyID).Scan(&post, &author, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, databaseError(ctx, err)
		}
		if post != *job.SourcePostID || deleted {
			return false, nil
		}
	} else if job.TriggerKind == app.TriggerRepost {
		if job.SourceRepostID == nil {
			return false, nil
		}
		if lock {
			suffix = " FOR UPDATE OF r"
		}
		var post app.ID
		err = q.queryer.QueryRowContext(ctx, `SELECT r.post_id,r.account_id,a.disabled_at IS NOT NULL
			FROM reposts r JOIN accounts a ON a.id=r.account_id WHERE r.id=$1`+suffix, job.SourceRepostID).Scan(&post, &author, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, databaseError(ctx, err)
		}
		if post != *job.SourcePostID || deleted {
			return false, nil
		}
	} else if job.TriggerKind == app.TriggerHumanPost && quote || job.TriggerKind == app.TriggerQuote && !quote {
		return false, nil
	}
	return author == *job.TriggerActorID, nil
}
