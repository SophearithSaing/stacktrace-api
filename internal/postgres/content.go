package postgres

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func (q *Queries) lockHumanActor(ctx context.Context, sessionHash string) (app.ID, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	var actor app.ID
	// Resolve without locking, then lock the account FIRST, as session rotation
	// does. A joined LockRows plan could otherwise take the session lock first.
	err := q.queryer.QueryRowContext(queryCtx, `SELECT account_id FROM sessions WHERE token_hash=$1`, sessionHash).Scan(&actor)
	if err != nil {
		return "", authenticationError(databaseError(queryCtx, err))
	}
	err = q.queryer.QueryRowContext(queryCtx, `SELECT id FROM accounts WHERE id=$1 AND type='human' AND disabled_at IS NULL FOR NO KEY UPDATE`, actor).Scan(&actor)
	if err != nil {
		return "", authenticationError(databaseError(queryCtx, err))
	}
	// Recheck after any account wait: revoke/rotation may have removed the session.
	err = q.queryer.QueryRowContext(queryCtx, `SELECT account_id FROM sessions
		WHERE token_hash=$1 AND account_id=$2 AND expires_at>clock_timestamp() FOR SHARE`, sessionHash, actor).Scan(&actor)
	return actor, authenticationError(databaseError(queryCtx, err))
}

func normalizePostCreation(creation app.PostCreation) (app.PostCreation, error) {
	return app.NewPostCreation(creation.Content.Body, creation.QuotedPostID, creation.Content.Code)
}

func normalizeReplyCreation(creation app.ReplyCreation) (app.ReplyCreation, error) {
	return app.NewReplyCreation(creation.PostID, creation.Body)
}

// CreatePost reserves the idempotency result before touching a quote source.
// This makes concurrent retries wait on the unique key and then reuse its row.
func (s *Store) CreatePost(ctx context.Context, sessionHash, key string, supplied app.PostCreation) (app.Post, error) {
	if err := app.ValidateIdempotencyKey(key); err != nil {
		return app.Post{}, err
	}
	creation, err := normalizePostCreation(supplied)
	if err != nil {
		return app.Post{}, err
	}
	var actor, resultID app.ID
	err = s.Transaction(ctx, func(q *Queries) error {
		var err error
		actor, err = q.lockHumanActor(ctx, sessionHash)
		if err != nil {
			return err
		}
		id := app.NewID()
		fresh, existingID, err := q.reservePost(ctx, actor, key, creation.RequestHash(), id)
		if err != nil {
			return err
		}
		if !fresh {
			resultID = existingID
			return nil
		}
		if creation.QuotedPostID != nil {
			if err := q.lockVisiblePost(ctx, *creation.QuotedPostID); err != nil {
				return err
			}
		}
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		var language, filename, source any
		if creation.Content.Code != nil {
			language = creation.Content.Code.Language
			filename = creation.Content.Code.Filename
			source = creation.Content.Code.Source
		}
		_, err = q.queryer.ExecContext(queryCtx, `INSERT INTO posts
			(id,author_id,body,quoted_post_id,code_language,code_filename,code_source,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,statement_timestamp())`, id, actor, creation.Content.Body, creation.QuotedPostID, language, filename, source)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		if err := q.insertTags(ctx, id, creation.Content.Tags); err != nil {
			return err
		}
		resultID = id
		kind := app.TriggerHumanPost
		if creation.QuotedPostID != nil {
			kind = app.TriggerQuote
		}
		return q.enqueueSocialGeneration(ctx, kind, id, actor)
	})
	if err != nil {
		return app.Post{}, err
	}
	// Projection refresh intentionally occurs after commit in a coherent read
	// snapshot. A concurrent owner deletion therefore returns ErrDeleted rather
	// than recreating content or returning a stale creation response.
	return s.PostByID(ctx, resultID, actor)
}

func (q *Queries) reservePost(ctx context.Context, actor app.ID, key, hash string, resultID app.ID) (bool, app.ID, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	res, err := q.queryer.ExecContext(queryCtx, `INSERT INTO idempotency_keys
		(account_id,key,request_hash,post_id,created_at,expires_at)
		VALUES ($1,$2,$3,$4,statement_timestamp(),statement_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, actor, key, hash, resultID)
	if err != nil {
		return false, "", databaseError(queryCtx, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, "", databaseError(queryCtx, err)
	}
	if rows == 1 {
		return true, resultID, nil
	}
	var storedHash string
	var id sql.NullString
	var replyID sql.NullString
	err = q.queryer.QueryRowContext(queryCtx, `SELECT request_hash,post_id::text,reply_id::text FROM idempotency_keys WHERE account_id=$1 AND key=$2`, actor, key).Scan(&storedHash, &id, &replyID)
	if err != nil {
		return false, "", databaseError(queryCtx, err)
	}
	if storedHash != hash || !id.Valid || replyID.Valid {
		return false, "", app.ErrConflict
	}
	return false, app.ID(id.String), nil
}

func (q *Queries) insertTags(ctx context.Context, postID app.ID, tags []app.Tag) error {
	tags = append([]app.Tag(nil), tags...)
	sort.Slice(tags, func(i, j int) bool { return tags[i].Slug < tags[j].Slug })
	for _, tag := range tags {
		queryCtx, cancel := q.queryContext(ctx)
		tagID := app.NewID()
		_, err := q.queryer.ExecContext(queryCtx, `INSERT INTO tags(id,slug,display_name) VALUES($1,$2,$3) ON CONFLICT(slug) DO NOTHING`, tagID, tag.Slug, tag.DisplayName)
		if err == nil {
			err = q.queryer.QueryRowContext(queryCtx, `SELECT id FROM tags WHERE slug=$1`, tag.Slug).Scan(&tagID)
		}
		if err == nil {
			_, err = q.queryer.ExecContext(queryCtx, `INSERT INTO post_tags(post_id,tag_id) VALUES($1,$2)`, postID, tagID)
		}
		if err != nil {
			mapped := databaseError(queryCtx, err)
			cancel()
			return mapped
		}
		cancel()
	}
	return nil
}

func (q *Queries) lockVisiblePost(ctx context.Context, id app.ID) error {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	var deleted *time.Time
	err := q.queryer.QueryRowContext(queryCtx, `SELECT deleted_at FROM posts WHERE id=$1 FOR UPDATE`, id).Scan(&deleted)
	if err != nil {
		return databaseError(queryCtx, err)
	}
	if deleted != nil {
		return app.ErrDeleted
	}
	return nil
}

func (s *Store) CreateReply(ctx context.Context, sessionHash, key string, supplied app.ReplyCreation) (app.CreateReplyResult, error) {
	if err := app.ValidateIdempotencyKey(key); err != nil {
		return app.CreateReplyResult{}, err
	}
	creation, err := normalizeReplyCreation(supplied)
	if err != nil {
		return app.CreateReplyResult{}, err
	}
	var resultID app.ID
	err = s.Transaction(ctx, func(q *Queries) error {
		actor, err := q.lockHumanActor(ctx, sessionHash)
		if err != nil {
			return err
		}
		id := app.NewID()
		fresh, existing, err := q.reserveReply(ctx, actor, key, creation.RequestHash(), id)
		if err != nil {
			return err
		}
		if !fresh {
			resultID = existing
			return nil
		}
		if err := q.lockVisiblePost(ctx, creation.PostID); err != nil {
			return err
		}
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		_, err = q.queryer.ExecContext(queryCtx, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,statement_timestamp())`, id, creation.PostID, actor, creation.Body)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		resultID = id
		return q.enqueueSocialGeneration(ctx, app.TriggerReply, id, actor)
	})
	if err != nil {
		return app.CreateReplyResult{}, err
	}
	// As with posts, a deletion between commit and refresh is reported from the
	// current snapshot and never causes a second insert.
	return s.replyResultByID(ctx, resultID)
}

func (s *Store) replyResultByID(ctx context.Context, id app.ID) (app.CreateReplyResult, error) {
	var result app.CreateReplyResult
	err := s.readSnapshot(ctx, func(q *Queries) error {
		reply, parentDeleted, err := q.replyByID(ctx, id)
		if err != nil {
			return err
		}
		if parentDeleted {
			return app.ErrDeleted
		}
		result.Reply = reply
		return q.visibleReplyTotal(ctx, reply.PostID, &result.ReplyTotal)
	})
	return result, err
}

func (q *Queries) reserveReply(ctx context.Context, actor app.ID, key, hash string, resultID app.ID) (bool, app.ID, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	res, err := q.queryer.ExecContext(queryCtx, `INSERT INTO idempotency_keys(account_id,key,request_hash,reply_id,created_at,expires_at) VALUES($1,$2,$3,$4,statement_timestamp(),statement_timestamp()+interval '24 hours') ON CONFLICT DO NOTHING`, actor, key, hash, resultID)
	if err != nil {
		return false, "", databaseError(queryCtx, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, "", databaseError(queryCtx, err)
	}
	if rows == 1 {
		return true, resultID, nil
	}
	var stored string
	var postID, replyID sql.NullString
	err = q.queryer.QueryRowContext(queryCtx, `SELECT request_hash,post_id::text,reply_id::text FROM idempotency_keys WHERE account_id=$1 AND key=$2`, actor, key).Scan(&stored, &postID, &replyID)
	if err != nil {
		return false, "", databaseError(queryCtx, err)
	}
	if stored != hash || postID.Valid || !replyID.Valid {
		return false, "", app.ErrConflict
	}
	return false, app.ID(replyID.String), nil
}

func (q *Queries) replyByID(ctx context.Context, id app.ID) (app.Reply, bool, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	var r app.Reply
	var deleted, parentDeleted *time.Time
	err := q.queryer.QueryRowContext(queryCtx, `SELECT r.id,r.post_id,r.body,r.created_at,r.deleted_at,p.deleted_at,
		a.id,a.type,a.handle,a.display_name,a.initials,a.bio,a.role_label,a.status_text,a.specialty,a.appearance_key,a.verified_at,a.created_at,a.updated_at,a.disabled_at
		FROM replies r JOIN posts p ON p.id=r.post_id JOIN accounts a ON a.id=r.author_id WHERE r.id=$1`, id).Scan(&r.ID, &r.PostID, &r.Body, &r.CreatedAt, &deleted, &parentDeleted, &r.Author.ID, &r.Author.Type, &r.Author.Handle, &r.Author.DisplayName, &r.Author.Initials, &r.Author.Bio, &r.Author.RoleLabel, &r.Author.StatusText, &r.Author.Specialty, &r.Author.AppearanceKey, &r.Author.VerifiedAt, &r.Author.CreatedAt, &r.Author.UpdatedAt, &r.Author.DisabledAt)
	if err != nil {
		return app.Reply{}, false, databaseError(queryCtx, err)
	}
	if deleted != nil {
		return app.Reply{}, false, app.ErrDeleted
	}
	r.CreatedAt = r.CreatedAt.UTC()
	return r, parentDeleted != nil, nil
}

func (q *Queries) visibleReplyTotal(ctx context.Context, postID app.ID, total *int64) error {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	return databaseError(queryCtx, q.queryer.QueryRowContext(queryCtx, `SELECT count(*) FROM replies WHERE post_id=$1 AND deleted_at IS NULL`, postID).Scan(total))
}

func (s *Store) DeletePost(ctx context.Context, sessionHash string, id app.ID) error {
	parsed, err := app.ParseID(string(id))
	if err != nil {
		return err
	}
	return s.Transaction(ctx, func(q *Queries) error {
		actor, err := q.lockHumanActor(ctx, sessionHash)
		if err != nil {
			return err
		}
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		var owner app.ID
		var deleted *time.Time
		err = q.queryer.QueryRowContext(queryCtx, `SELECT author_id,deleted_at FROM posts WHERE id=$1 FOR UPDATE`, parsed).Scan(&owner, &deleted)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		if deleted != nil {
			return app.ErrDeleted
		}
		if owner != actor {
			return app.ErrForbidden
		}
		_, err = q.queryer.ExecContext(queryCtx, `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, parsed)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		_, err = q.cancelRemovedGenerationSourceJobs(ctx, parsed)
		return err
	})
}

func (s *Store) DeleteReply(ctx context.Context, sessionHash string, id app.ID) error {
	parsed, err := app.ParseID(string(id))
	if err != nil {
		return err
	}
	return s.Transaction(ctx, func(q *Queries) error {
		actor, err := q.lockHumanActor(ctx, sessionHash)
		if err != nil {
			return err
		}
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		var parent app.ID
		if err = q.queryer.QueryRowContext(queryCtx, `SELECT post_id FROM replies WHERE id=$1`, parsed).Scan(&parent); err != nil {
			return databaseError(queryCtx, err)
		}
		var parentDeleted *time.Time
		if err = q.queryer.QueryRowContext(queryCtx, `SELECT deleted_at FROM posts WHERE id=$1 FOR UPDATE`, parent).Scan(&parentDeleted); err != nil {
			return databaseError(queryCtx, err)
		}
		if parentDeleted != nil {
			return app.ErrDeleted
		}
		var owner app.ID
		var deleted *time.Time
		if err = q.queryer.QueryRowContext(queryCtx, `SELECT author_id,deleted_at FROM replies WHERE id=$1 AND post_id=$2 FOR UPDATE`, parsed, parent).Scan(&owner, &deleted); err != nil {
			return databaseError(queryCtx, err)
		}
		if deleted != nil {
			return app.ErrDeleted
		}
		if owner != actor {
			return app.ErrForbidden
		}
		_, err = q.queryer.ExecContext(queryCtx, `UPDATE replies SET deleted_at=statement_timestamp() WHERE id=$1`, parsed)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		_, err = q.cancelRemovedGenerationSourceJobs(ctx, parent)
		return err
	})
}
