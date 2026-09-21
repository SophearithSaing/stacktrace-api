package postgres

import (
	"context"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// SetReaction establishes the actor's desired reaction state. A nil kind
// removes the current reaction.
func (s *Store) SetReaction(ctx context.Context, sessionHash string, postID app.ID, kind *app.ReactionKind) (app.Post, error) {
	parsedPostID, err := app.ParseID(string(postID))
	if err != nil {
		return app.Post{}, err
	}
	if kind != nil && !kind.Valid() {
		return app.Post{}, app.ErrInvalidReactionKind
	}

	var actor app.ID
	err = s.Transaction(ctx, func(q *Queries) error {
		actor, err = q.lockHumanActor(ctx, sessionHash)
		if err != nil {
			return err
		}
		if err = q.lockVisiblePost(ctx, parsedPostID); err != nil {
			return err
		}

		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		if kind == nil {
			_, err = q.queryer.ExecContext(queryCtx, `DELETE FROM reactions WHERE account_id=$1 AND post_id=$2`, actor, parsedPostID)
		} else {
			_, err = q.queryer.ExecContext(queryCtx, `INSERT INTO reactions(account_id,post_id,kind,created_at,updated_at)
				VALUES($1,$2,$3,statement_timestamp(),statement_timestamp())
				ON CONFLICT(account_id,post_id) DO UPDATE SET
				kind=EXCLUDED.kind,
				updated_at=CASE WHEN reactions.kind IS DISTINCT FROM EXCLUDED.kind THEN statement_timestamp() ELSE reactions.updated_at END`, actor, parsedPostID, *kind)
		}
		return databaseError(queryCtx, err)
	})
	if err != nil {
		return app.Post{}, err
	}
	return s.PostByID(ctx, parsedPostID, actor)
}

// SetRepost establishes whether the actor has a repost event for the post.
// Repeated creation preserves the existing event identity and timestamp.
func (s *Store) SetRepost(ctx context.Context, sessionHash string, postID app.ID, desired bool) (app.Post, error) {
	parsedPostID, err := app.ParseID(string(postID))
	if err != nil {
		return app.Post{}, err
	}

	var actor app.ID
	err = s.Transaction(ctx, func(q *Queries) error {
		actor, err = q.lockHumanActor(ctx, sessionHash)
		if err != nil {
			return err
		}
		if err = q.lockVisiblePost(ctx, parsedPostID); err != nil {
			return err
		}

		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		if desired {
			id := app.NewID()
			result, insertErr := q.queryer.ExecContext(queryCtx, `INSERT INTO reposts(id,account_id,post_id,created_at)
				VALUES($1,$2,$3,statement_timestamp()) ON CONFLICT(account_id,post_id) DO NOTHING`, id, actor, parsedPostID)
			if insertErr != nil {
				return databaseError(queryCtx, insertErr)
			}
			count, countErr := result.RowsAffected()
			if countErr != nil {
				return databaseError(queryCtx, countErr)
			}
			if count == 1 {
				return q.enqueueSocialGeneration(ctx, app.TriggerRepost, id, actor)
			}
		} else {
			_, err = q.queryer.ExecContext(queryCtx, `DELETE FROM reposts WHERE account_id=$1 AND post_id=$2`, actor, parsedPostID)
		}
		return databaseError(queryCtx, err)
	})
	if err != nil {
		return app.Post{}, err
	}
	return s.PostByID(ctx, parsedPostID, actor)
}

// SetBookmark establishes the actor's private bookmark state.
func (s *Store) SetBookmark(ctx context.Context, sessionHash string, postID app.ID, desired bool) (app.Post, error) {
	parsedPostID, err := app.ParseID(string(postID))
	if err != nil {
		return app.Post{}, err
	}

	var actor app.ID
	err = s.Transaction(ctx, func(q *Queries) error {
		actor, err = q.lockHumanActor(ctx, sessionHash)
		if err != nil {
			return err
		}
		if err = q.lockVisiblePost(ctx, parsedPostID); err != nil {
			return err
		}

		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		if desired {
			_, err = q.queryer.ExecContext(queryCtx, `INSERT INTO bookmarks(account_id,post_id,created_at)
				VALUES($1,$2,statement_timestamp()) ON CONFLICT(account_id,post_id) DO NOTHING`, actor, parsedPostID)
		} else {
			_, err = q.queryer.ExecContext(queryCtx, `DELETE FROM bookmarks WHERE account_id=$1 AND post_id=$2`, actor, parsedPostID)
		}
		return databaseError(queryCtx, err)
	})
	if err != nil {
		return app.Post{}, err
	}
	return s.PostByID(ctx, parsedPostID, actor)
}
