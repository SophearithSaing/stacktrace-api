package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// SearchPosts returns canonical visible posts matching their body or author.
func (s *Store) SearchPosts(ctx context.Context, viewer app.ID, query app.SearchQuery) (app.SearchPage, error) {
	if viewer != "" {
		var err error
		viewer, err = app.ParseID(string(viewer))
		if err != nil {
			return app.SearchPage{}, err
		}
	}
	query, err := query.Normalize()
	if err != nil {
		return app.SearchPage{}, err
	}
	var page app.SearchPage
	err = s.readSnapshot(ctx, func(q *Queries) error {
		var err error
		page, err = q.searchPosts(ctx, viewer, query)
		return err
	})
	return page, err
}

// searchPosts selects IDs before hydrating each canonical post once.
func (q *Queries) searchPosts(ctx context.Context, viewer app.ID, query app.SearchQuery) (app.SearchPage, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	page := app.SearchPage{Items: []app.SearchItem{}, Ceiling: query.Window.InitialCeiling}
	if page.Ceiling.IsZero() {
		if err := q.queryer.QueryRowContext(queryCtx, `SELECT statement_timestamp()`).Scan(&page.Ceiling); err != nil {
			return page, databaseError(queryCtx, err)
		}
	}
	page.Ceiling = page.Ceiling.UTC()
	position := app.SearchPosition{CreatedAt: time.Time{}, ID: "00000000-0000-0000-0000-000000000000"}
	if query.Window.Position != nil {
		position = *query.Window.Position
	}
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT p.id,p.created_at FROM posts p JOIN accounts a ON a.id=p.author_id
		WHERE p.deleted_at IS NULL AND p.created_at <= $1
		AND (NOT $2 OR (p.created_at,p.id) < ($3::timestamptz,$4::uuid))
		AND (to_tsvector('simple',p.body) @@ websearch_to_tsquery('simple',$5)
			OR a.display_name ILIKE '%' || replace(replace(replace($5,chr(92),chr(92)||chr(92)), '%', chr(92)||'%'), '_', chr(92)||'_') || '%' ESCAPE chr(92)
			OR a.handle ILIKE '%' || replace(replace(replace($5,chr(92),chr(92)||chr(92)), '%', chr(92)||'%'), '_', chr(92)||'_') || '%' ESCAPE chr(92))
		ORDER BY p.created_at DESC,p.id DESC LIMIT $6`, page.Ceiling, query.Window.Position != nil, position.CreatedAt, position.ID, query.Text, query.Window.Limit+1)
	if err != nil {
		return page, databaseError(queryCtx, err)
	}
	defer rows.Close()
	type selectedPost struct {
		id        app.ID
		createdAt time.Time
	}
	var selected []selectedPost
	for rows.Next() {
		var id app.ID
		var created time.Time
		if err := rows.Scan(&id, &created); err != nil {
			return page, databaseError(queryCtx, err)
		}
		selected = append(selected, selectedPost{id: id, createdAt: created.UTC()})
	}
	if err := rows.Err(); err != nil {
		return page, databaseError(queryCtx, err)
	}
	if len(selected) > query.Window.Limit {
		selected = selected[:query.Window.Limit]
		last := selected[len(selected)-1]
		page.NextPosition = &app.SearchPosition{CreatedAt: last.createdAt, ID: last.id}
	}
	ids := make([]app.ID, 0, len(selected))
	for _, post := range selected {
		ids = append(ids, post.id)
	}
	posts, err := q.hydratePosts(ctx, ids, viewer)
	if err != nil {
		return page, err
	}
	for _, post := range posts {
		page.Items = append(page.Items, app.SearchItem{Post: post, Snippet: app.SearchSnippet(post.Content.Body)})
	}
	return page, nil
}
