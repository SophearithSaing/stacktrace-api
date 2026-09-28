package postgres

import (
	"context"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// DisableAccount performs the trusted, irreversible soft disable. It is
// idempotent: already disabled accounts are left fully intact but still lose
// every remaining session; only the first disable stamps disabled_at and
// updated_at, preserving the original timestamps on repeats. The agent-
// settings row and all authored content stay until separately removed;
// generation admission, publication and human session resolution already
// observe disabled_at, so the write is a single fenced flip. Lock order
// matches the existing account-first convention (pause, content mutation):
// account FOR SHARE/UPDATE precedes sessions.
func (s *Store) DisableAccount(ctx context.Context, id app.ID) error {
	if _, err := app.ParseID(string(id)); err != nil {
		return invalidGeneration("account_id")
	}
	return s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		var disabled bool
		if err := q.queryer.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM accounts WHERE id=$1 FOR UPDATE`, id).Scan(&disabled); err != nil {
			return databaseError(ctx, err)
		}
		if !disabled {
			result, err := q.queryer.ExecContext(ctx, `UPDATE accounts SET disabled_at=clock_timestamp(),
				updated_at=clock_timestamp() WHERE id=$1 AND disabled_at IS NULL`, id)
			if err := generationClaimMutation(ctx, result, err); err != nil {
				return err
			}
		}
		result, err := q.queryer.ExecContext(ctx, `DELETE FROM sessions WHERE account_id=$1`, id)
		if err != nil {
			return databaseError(ctx, err)
		}
		if _, err := result.RowsAffected(); err != nil {
			return databaseError(ctx, err)
		}
		return nil
	})
}

// RemovePost is the trusted operator soft-delete with no session authority:
// the caller must hold database credentials (human authorization is unchanged
// and no HTTP route reaches this in this checkpoint). It reuses the exact
// removal barriers of the human deletion path: lock the post row, set a
// soft-delete provenance stamp, then cancel remaining generation-source jobs
// under their job locks. History, provenance and source attribution persist.
func (s *Store) RemovePost(ctx context.Context, id app.ID) error {
	if _, err := app.ParseID(string(id)); err != nil {
		return invalidGeneration("post_id")
	}
	return s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		var deleted bool
		if err := q.queryer.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM posts WHERE id=$1 FOR UPDATE`, id).Scan(&deleted); err != nil {
			return databaseError(ctx, err)
		}
		if deleted {
			return app.ErrDeleted // Idempotent refusal; provenance was untouched.
		}
		if _, err := q.queryer.ExecContext(ctx, `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, id); err != nil {
			return databaseError(ctx, err)
		}
		_, err := q.cancelRemovedGenerationSourceJobs(ctx, id)
		return err
	})
}

// RemoveReply mirrors the trusted post removal for a bounded soft-deleted
// reply. The parent post is locked first and must still exist, matching the
// established author-removal contract.
func (s *Store) RemoveReply(ctx context.Context, id app.ID) error {
	if _, err := app.ParseID(string(id)); err != nil {
		return invalidGeneration("reply_id")
	}
	return s.Transaction(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		var parent app.ID
		if err := q.queryer.QueryRowContext(ctx, `SELECT post_id FROM replies WHERE id=$1`, id).Scan(&parent); err != nil {
			return databaseError(ctx, err)
		}
		var parentDeleted bool
		if err := q.queryer.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM posts WHERE id=$1 FOR UPDATE`, parent).Scan(&parentDeleted); err != nil {
			return databaseError(ctx, err)
		}
		if parentDeleted {
			return app.ErrDeleted
		}
		var deleted bool
		if err := q.queryer.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM replies WHERE id=$1 AND post_id=$2 FOR UPDATE`, id, parent).Scan(&deleted); err != nil {
			return databaseError(ctx, err)
		}
		if deleted {
			return app.ErrDeleted
		}
		if _, err := q.queryer.ExecContext(ctx, `UPDATE replies SET deleted_at=statement_timestamp() WHERE id=$1`, id); err != nil {
			return databaseError(ctx, err)
		}
		_, err := q.cancelRemovedGenerationSourceJobs(ctx, parent)
		return err
	})
}
