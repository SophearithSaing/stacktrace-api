package postgres

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// ListFeed uses the validated public viewer for projections, like PostByID.
// Following additionally requires a live session for that same viewer, checked
// inside the selection's snapshot; a viewer ID alone grants no access.
func (s *Store) ListFeed(ctx context.Context, viewer app.ID, sessionHash string, query app.FeedQuery) (app.FeedPage, error) {
	query, err := query.Normalize()
	if err != nil {
		return app.FeedPage{}, err
	}
	if viewer != "" {
		viewer, err = app.ParseID(string(viewer))
		if err != nil {
			return app.FeedPage{}, err
		}
	}
	var page app.FeedPage
	err = s.readSnapshot(ctx, func(q *Queries) error {
		if query.View == app.FeedViewFollowing {
			account, err := q.SessionAccount(ctx, sessionHash)
			if err != nil {
				return err
			}
			if account.ID != viewer {
				return app.ErrUnauthenticated
			}
		}
		var err error
		page, err = q.listFeed(ctx, viewer, "", query)
		return err
	})
	return page, err
}

// ListAccountFeed returns authored posts and repost events newest first. A
// disabled/unknown target is unavailable, but disabled authors' existing public
// content remains visible in other feeds, matching PostByID.
func (s *Store) ListAccountFeed(ctx context.Context, accountID, viewer app.ID, window app.FeedWindow) (app.FeedPage, error) {
	accountID, err := app.ParseID(string(accountID))
	if err != nil {
		return app.FeedPage{}, err
	}
	if viewer != "" {
		viewer, err = app.ParseID(string(viewer))
		if err != nil {
			return app.FeedPage{}, err
		}
	}
	window, err = window.Normalize(app.FeedSortNewest)
	if err != nil {
		return app.FeedPage{}, err
	}
	var page app.FeedPage
	err = s.readSnapshot(ctx, func(q *Queries) error {
		account, err := q.AccountByID(ctx, accountID)
		if err != nil {
			return err
		}
		if account.DisabledAt != nil {
			return app.ErrNotFound
		}
		page, err = q.listFeed(ctx, viewer, accountID, app.FeedQuery{View: app.FeedViewForYou, Sort: app.FeedSortNewest, Window: window})
		return err
	})
	return page, err
}

type feedEvent struct {
	position app.FeedPosition
	postID   app.ID
	actorID  app.ID
}

// listFeed requires a normalized query and transaction-bound Queries. Event
// selection, live scores, canonical projections and reposters share a snapshot.
func (q *Queries) listFeed(ctx context.Context, viewer, accountID app.ID, query app.FeedQuery) (app.FeedPage, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	page := app.FeedPage{Items: []app.FeedEntry{}, Ceiling: query.Window.InitialCeiling}
	if page.Ceiling.IsZero() {
		if err := q.queryer.QueryRowContext(queryCtx, `SELECT statement_timestamp()`).Scan(&page.Ceiling); err != nil {
			return page, databaseError(queryCtx, err)
		}
	}
	page.Ceiling = page.Ceiling.UTC()
	position := app.FeedPosition{ID: "00000000-0000-0000-0000-000000000000", Timestamp: time.Time{}, Kind: app.FeedKindPost}
	if query.Window.Position != nil {
		position = *query.Window.Position
	}
	rows, err := q.queryer.QueryContext(queryCtx, `WITH events AS (
		SELECT p.id,'post'::text COLLATE "C" AS kind,p.created_at AS occurred_at,p.id AS post_id,p.author_id AS actor_id
		FROM posts p WHERE p.deleted_at IS NULL
		UNION ALL
		SELECT r.id,'repost'::text COLLATE "C",r.created_at,r.post_id,r.account_id FROM reposts r
	), ranked AS (
		SELECT e.*,CASE WHEN $1 THEN (SELECT count(*) FROM reactions r WHERE r.post_id=p.id) ELSE 0 END AS score
		FROM events e JOIN posts p ON p.id=e.post_id
		WHERE p.deleted_at IS NULL AND e.occurred_at<=$2
		AND ($3::uuid IS NULL OR e.actor_id=$3)
		AND (NOT $4 OR e.actor_id=$5::uuid OR EXISTS (SELECT 1 FROM follows f WHERE f.follower_id=$5 AND f.followed_id=e.actor_id))
		AND (NOT $6 OR p.is_spicy)
		AND ($7='' OR EXISTS (SELECT 1 FROM post_tags pt JOIN tags t ON t.id=pt.tag_id WHERE pt.post_id=p.id AND t.slug=$7))
	)
	SELECT id,kind,occurred_at,post_id,actor_id,score FROM ranked
	WHERE NOT $8 OR (score,occurred_at,kind,id)<($9::bigint,$10::timestamptz,$11::text COLLATE "C",$12::uuid)
	ORDER BY score DESC,occurred_at DESC,kind DESC,id DESC LIMIT $13`,
		query.Sort == app.FeedSortReacted, page.Ceiling, nullableID(accountID), query.View == app.FeedViewFollowing,
		nullableID(viewer), query.View == app.FeedViewSpicy, query.Tag, query.Window.Position != nil,
		position.Score, position.Timestamp, position.Kind, position.ID, query.Window.Limit+1)
	if err != nil {
		return page, databaseError(queryCtx, err)
	}
	var events []feedEvent
	for rows.Next() {
		var event feedEvent
		if err = rows.Scan(&event.position.ID, &event.position.Kind, &event.position.Timestamp, &event.postID, &event.actorID, &event.position.Score); err != nil {
			rows.Close()
			return page, databaseError(queryCtx, err)
		}
		event.position.Timestamp = event.position.Timestamp.UTC()
		events = append(events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, databaseError(queryCtx, err)
	}
	if len(events) > query.Window.Limit {
		events = events[:query.Window.Limit]
		last := events[len(events)-1].position
		page.NextPosition = &last
	}
	// Deduplicate canonical projection inputs, never ordered event rows.
	ids := make([]app.ID, 0, len(events))
	seen := make(map[app.ID]bool, len(events))
	reposterIDs := make(map[app.ID]bool)
	for _, event := range events {
		if !seen[event.postID] {
			seen[event.postID] = true
			ids = append(ids, event.postID)
		}
		if event.position.Kind == app.FeedKindRepost {
			reposterIDs[event.actorID] = true
		}
	}
	posts, err := q.hydratePosts(ctx, ids, viewer)
	if err != nil {
		return page, err
	}
	canonical := make(map[app.ID]app.Post, len(posts))
	for _, post := range posts {
		canonical[post.ID] = post
	}
	reposters, err := q.feedReposters(ctx, reposterIDs)
	if err != nil {
		return page, err
	}
	for _, event := range events {
		entry := app.FeedEntry{ID: event.position.ID, Kind: event.position.Kind, OccurredAt: event.position.Timestamp, Post: canonical[event.postID]}
		if entry.Kind == app.FeedKindRepost {
			entry.Reposter = reposters[event.actorID]
		}
		page.Items = append(page.Items, entry)
	}
	return page, nil
}

func (q *Queries) feedReposters(ctx context.Context, ids map[app.ID]bool) (map[app.ID]*app.Account, error) {
	accounts := make(map[app.ID]*app.Account, len(ids))
	if len(ids) == 0 {
		return accounts, nil
	}
	stringIDs := make([]string, 0, len(ids))
	for id := range ids {
		stringIDs = append(stringIDs, string(id))
	}
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT id,type,handle,display_name,initials,bio,role_label,status_text,specialty,appearance_key,verified_at,created_at,updated_at,disabled_at FROM accounts WHERE id=ANY($1::uuid[])`, stringIDs)
	if err != nil {
		return nil, databaseError(queryCtx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var a accountNulls
		if err = rows.Scan(&a.id, &a.accountType, &a.handle, &a.displayName, &a.initials, &a.bio, &a.roleLabel, &a.statusText, &a.specialty, &a.appearanceKey, &a.verifiedAt, &a.createdAt, &a.updatedAt, &a.disabledAt); err != nil {
			return nil, databaseError(queryCtx, err)
		}
		account := a.account()
		accounts[account.ID] = &account
	}
	return accounts, databaseError(queryCtx, rows.Err())
}
