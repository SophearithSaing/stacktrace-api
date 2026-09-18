package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// ListBookmarks returns only the authenticated session owner's bookmarks. The
// selection and batched post projection share one repeatable-read snapshot.
func (s *Store) ListBookmarks(ctx context.Context, sessionHash string, window app.ReadWindow) (app.PostPage, error) {
	window, err := validateBookmarkWindow(window)
	if err != nil {
		return app.PostPage{}, err
	}

	var page app.PostPage
	err = s.readSnapshot(ctx, func(q *Queries) error {
		account, err := q.SessionAccount(ctx, sessionHash)
		if err != nil {
			return err
		}

		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		ceiling := window.InitialCeiling
		if ceiling.IsZero() {
			if err = q.queryer.QueryRowContext(queryCtx, `SELECT statement_timestamp()`).Scan(&ceiling); err != nil {
				return databaseError(queryCtx, err)
			}
		}
		page.Ceiling = ceiling.UTC()

		positionTime := time.Time{}
		positionID := app.ID("00000000-0000-0000-0000-000000000000")
		hasPosition := window.Position != nil
		if hasPosition {
			positionTime = window.Position.Timestamp
			positionID = window.Position.ID
		}
		rows, err := q.queryer.QueryContext(queryCtx, `SELECT b.post_id,b.created_at
			FROM bookmarks b JOIN posts p ON p.id=b.post_id
			WHERE b.account_id=$1 AND p.deleted_at IS NULL AND b.created_at<=$2
			AND (NOT $3 OR (b.created_at,b.post_id)<($4,$5))
			ORDER BY b.created_at DESC,b.post_id DESC LIMIT $6`, account.ID, ceiling, hasPosition, positionTime, positionID, window.Limit+1)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		var ids []app.ID
		var createdAt []time.Time
		for rows.Next() {
			var id app.ID
			var timestamp time.Time
			if err = rows.Scan(&id, &timestamp); err != nil {
				rows.Close()
				return databaseError(queryCtx, err)
			}
			ids = append(ids, id)
			createdAt = append(createdAt, timestamp.UTC())
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return databaseError(queryCtx, err)
		}
		if err = rows.Close(); err != nil {
			return databaseError(queryCtx, err)
		}

		hasMore := len(ids) > window.Limit
		if hasMore {
			ids = ids[:window.Limit]
			createdAt = createdAt[:window.Limit]
		}
		page.Items, err = q.hydratePosts(ctx, ids, account.ID)
		if err != nil {
			return err
		}
		if hasMore {
			last := len(ids) - 1
			page.NextPosition = &app.KeysetPosition{Timestamp: createdAt[last], ID: ids[last]}
		}
		return nil
	})
	return page, err
}

func validateBookmarkWindow(window app.ReadWindow) (app.ReadWindow, error) {
	if window.Sort == "" {
		window.Sort = app.ReplySortNewest
	}
	if window.Sort != app.ReplySortNewest {
		return window, &app.ValidationError{Fields: map[string]string{"sort": "Must be newest"}}
	}
	if window.Limit == 0 {
		window.Limit = app.DefaultReadLimit
	}
	if window.Limit < 1 || window.Limit > app.MaxReadLimit {
		return window, &app.ValidationError{Fields: map[string]string{"limit": "Must be between 1 and 50"}}
	}
	if window.Position != nil {
		if _, err := app.ParseID(string(window.Position.ID)); err != nil {
			return window, err
		}
		if window.Position.Timestamp.IsZero() {
			return window, &app.ValidationError{Fields: map[string]string{"position": "Must include a timestamp"}}
		}
		if window.InitialCeiling.IsZero() {
			return window, &app.ValidationError{Fields: map[string]string{"ceiling": "Required with a position"}}
		}
		if window.Position.Timestamp.After(window.InitialCeiling) {
			return window, &app.ValidationError{Fields: map[string]string{"position": "Must not be after the ceiling"}}
		}
	}
	return window, nil
}
