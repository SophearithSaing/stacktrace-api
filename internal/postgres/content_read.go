package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func (s *Store) PostByID(ctx context.Context, id, viewer app.ID) (app.Post, error) {
	parsed, err := app.ParseID(string(id))
	if err != nil {
		return app.Post{}, err
	}
	if viewer != "" {
		if _, err := app.ParseID(string(viewer)); err != nil {
			return app.Post{}, err
		}
	}
	var result app.Post
	err = s.readSnapshot(ctx, func(q *Queries) error {
		posts, err := q.hydratePosts(ctx, []app.ID{parsed}, viewer)
		if err != nil {
			return err
		}
		if len(posts) != 0 {
			result = posts[0]
			return nil
		}
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		var deleted *time.Time
		err = q.queryer.QueryRowContext(queryCtx, `SELECT deleted_at FROM posts WHERE id=$1`, parsed).Scan(&deleted)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		return app.ErrDeleted
	})
	return result, err
}

// hydratePosts is the bounded batch projection primitive shared by single-post
// reads and future page readers. It never recursively expands quote posts.
func (q *Queries) hydratePosts(ctx context.Context, ids []app.ID, viewer app.ID) ([]app.Post, error) {
	if len(ids) == 0 {
		return []app.Post{}, nil
	}
	// Events may share a canonical post. Hydrate each post once (especially the
	// lateral reply preview), while retaining the caller's requested output order.
	stringIDs := make([]string, 0, len(ids))
	seen := make(map[app.ID]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			stringIDs = append(stringIDs, string(id))
		}
	}
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT p.id,p.body,p.code_language,p.code_filename,p.code_source,p.is_spicy,p.created_at,
		a.id,a.type,a.handle,a.display_name,a.initials,a.bio,a.role_label,a.status_text,a.specialty,a.appearance_key,a.verified_at,a.created_at,a.updated_at,a.disabled_at,
		qp.id,qp.deleted_at,qa.id,qa.type,qa.handle,qa.display_name,qa.initials,qa.bio,qa.role_label,qa.status_text,qa.specialty,qa.appearance_key,qa.verified_at,qa.created_at,qa.updated_at,qa.disabled_at,qp.body
		FROM posts p JOIN accounts a ON a.id=p.author_id
		LEFT JOIN posts qp ON qp.id=p.quoted_post_id LEFT JOIN accounts qa ON qa.id=qp.author_id
		WHERE p.id=ANY($1::uuid[]) AND p.deleted_at IS NULL`, stringIDs)
	if err != nil {
		return nil, databaseError(queryCtx, err)
	}
	defer rows.Close()
	posts := make(map[app.ID]*app.Post, len(ids))
	for rows.Next() {
		var p app.Post
		var language, filename, source sql.NullString
		var quoteID sql.NullString
		var quoteDeleted sql.NullTime
		var qa accountNulls
		var quoteBody sql.NullString
		err = rows.Scan(&p.ID, &p.Content.Body, &language, &filename, &source, &p.IsSpicy, &p.CreatedAt,
			&p.Author.ID, &p.Author.Type, &p.Author.Handle, &p.Author.DisplayName, &p.Author.Initials, &p.Author.Bio, &p.Author.RoleLabel, &p.Author.StatusText, &p.Author.Specialty, &p.Author.AppearanceKey, &p.Author.VerifiedAt, &p.Author.CreatedAt, &p.Author.UpdatedAt, &p.Author.DisabledAt,
			&quoteID, &quoteDeleted, &qa.id, &qa.accountType, &qa.handle, &qa.displayName, &qa.initials, &qa.bio, &qa.roleLabel, &qa.statusText, &qa.specialty, &qa.appearanceKey, &qa.verifiedAt, &qa.createdAt, &qa.updatedAt, &qa.disabledAt, &quoteBody)
		if err != nil {
			return nil, databaseError(queryCtx, err)
		}
		p.CreatedAt = p.CreatedAt.UTC()
		if language.Valid {
			p.Content.Code = &app.Code{Language: language.String, Filename: filename.String, Source: source.String}
		}
		if quoteID.Valid {
			p.Quote = &app.QuotePost{ID: app.ID(quoteID.String)}
			if quoteDeleted.Valid {
				p.Quote.Availability = app.ContentDeleted
			} else {
				p.Quote.Availability = app.ContentAvailable
				author := qa.account()
				p.Quote.Author = &author
				p.Quote.Body = quoteBody.String
			}
		}
		posts[p.ID] = &p
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, databaseError(queryCtx, err)
	}
	if len(posts) == 0 {
		return []app.Post{}, nil
	}
	visibleIDs := make([]string, 0, len(posts))
	for _, id := range stringIDs {
		if posts[app.ID(id)] != nil {
			visibleIDs = append(visibleIDs, id)
		}
	}
	if err = q.hydrateTags(ctx, visibleIDs, posts); err != nil {
		return nil, err
	}
	if err = q.hydrateCounts(ctx, visibleIDs, posts); err != nil {
		return nil, err
	}
	if err = q.hydrateViewer(ctx, visibleIDs, viewer, posts); err != nil {
		return nil, err
	}
	if err = q.hydrateReplyPreviews(ctx, visibleIDs, posts); err != nil {
		return nil, err
	}
	result := make([]app.Post, 0, len(posts))
	for _, id := range ids {
		if post := posts[id]; post != nil {
			result = append(result, *post)
		}
	}
	return result, nil
}

type accountNulls struct {
	id, accountType, handle, displayName, initials, bio, roleLabel, statusText, specialty sql.NullString
	appearanceKey                                                                         sql.NullString
	verifiedAt, createdAt, updatedAt, disabledAt                                          sql.NullTime
}

func (a accountNulls) account() app.Account {
	var appearance *string
	if a.appearanceKey.Valid {
		v := a.appearanceKey.String
		appearance = &v
	}
	var verified, disabled *time.Time
	if a.verifiedAt.Valid {
		v := a.verifiedAt.Time.UTC()
		verified = &v
	}
	if a.disabledAt.Valid {
		v := a.disabledAt.Time.UTC()
		disabled = &v
	}
	return app.Account{ID: app.ID(a.id.String), Type: app.AccountType(a.accountType.String), Handle: a.handle.String, DisplayName: a.displayName.String, Initials: a.initials.String, Bio: a.bio.String, RoleLabel: a.roleLabel.String, StatusText: a.statusText.String, Specialty: a.specialty.String, AppearanceKey: appearance, VerifiedAt: verified, CreatedAt: a.createdAt.Time.UTC(), UpdatedAt: a.updatedAt.Time.UTC(), DisabledAt: disabled}
}

func (q *Queries) hydrateTags(ctx context.Context, ids []string, posts map[app.ID]*app.Post) error {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT pt.post_id,t.slug,t.display_name FROM post_tags pt JOIN tags t ON t.id=pt.tag_id WHERE pt.post_id=ANY($1::uuid[])`, ids)
	if err != nil {
		return databaseError(queryCtx, err)
	}
	defer rows.Close()
	canonical := map[app.ID]map[string]string{}
	for rows.Next() {
		var id app.ID
		var slug, name string
		if err = rows.Scan(&id, &slug, &name); err != nil {
			rows.Close()
			return databaseError(queryCtx, err)
		}
		if canonical[id] == nil {
			canonical[id] = map[string]string{}
		}
		canonical[id][slug] = name
	}
	if err = rows.Err(); err != nil {
		return databaseError(queryCtx, err)
	}
	for id, p := range posts {
		for _, extracted := range app.ExtractTags(p.Content.Body) {
			if display, ok := canonical[id][extracted.Slug]; ok {
				p.Content.Tags = append(p.Content.Tags, app.Tag{Slug: extracted.Slug, DisplayName: display})
			}
		}
	}
	return nil
}

func (q *Queries) hydrateCounts(ctx context.Context, ids []string, posts map[app.ID]*app.Post) error {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT p.id,
		(SELECT count(*) FROM replies x WHERE x.post_id=p.id AND x.deleted_at IS NULL),(SELECT count(*) FROM reposts x WHERE x.post_id=p.id),
		(SELECT count(*) FROM reactions x WHERE x.post_id=p.id),
		(SELECT count(*) FROM reactions x WHERE x.post_id=p.id AND kind='useful'),(SELECT count(*) FROM reactions x WHERE x.post_id=p.id AND kind='agree'),
		(SELECT count(*) FROM reactions x WHERE x.post_id=p.id AND kind='brilliant'),(SELECT count(*) FROM reactions x WHERE x.post_id=p.id AND kind='spicy'),(SELECT count(*) FROM reactions x WHERE x.post_id=p.id AND kind='ship')
		FROM posts p WHERE p.id=ANY($1::uuid[])`, ids)
	if err != nil {
		return databaseError(queryCtx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id app.ID
		var c app.PostCounts
		if err = rows.Scan(&id, &c.Replies, &c.Reposts, &c.ReactionsTotal, &c.Reactions.Useful, &c.Reactions.Agree, &c.Reactions.Brilliant, &c.Reactions.Spicy, &c.Reactions.Ship); err != nil {
			rows.Close()
			return databaseError(queryCtx, err)
		}
		if post := posts[id]; post != nil {
			post.Counts = c
		}
	}
	return databaseError(queryCtx, rows.Err())
}

func (q *Queries) hydrateViewer(ctx context.Context, ids []string, viewer app.ID, posts map[app.ID]*app.Post) error {
	if viewer == "" {
		return nil
	}
	for _, p := range posts {
		p.Viewer = &app.PostViewer{}
	}
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT p.id,r.kind,rp.id,rp.created_at,(b.account_id IS NOT NULL) FROM posts p LEFT JOIN reactions r ON r.post_id=p.id AND r.account_id=$2 LEFT JOIN reposts rp ON rp.post_id=p.id AND rp.account_id=$2 LEFT JOIN bookmarks b ON b.post_id=p.id AND b.account_id=$2 WHERE p.id=ANY($1::uuid[])`, ids, viewer)
	if err != nil {
		return databaseError(queryCtx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id app.ID
		var reaction, repostID sql.NullString
		var repostAt sql.NullTime
		var bookmarked bool
		if err = rows.Scan(&id, &reaction, &repostID, &repostAt, &bookmarked); err != nil {
			rows.Close()
			return databaseError(queryCtx, err)
		}
		post := posts[id]
		if post == nil {
			continue
		}
		v := post.Viewer
		v.Bookmarked = bookmarked
		if reaction.Valid {
			k := app.ReactionKind(reaction.String)
			v.Reaction = &k
		}
		if repostID.Valid {
			v.Reposted = true
			v.ViewerRepost = &app.Repost{ID: app.ID(repostID.String), PostID: id, AccountID: viewer, CreatedAt: repostAt.Time.UTC()}
		}
	}
	return databaseError(queryCtx, rows.Err())
}

func (q *Queries) hydrateReplyPreviews(ctx context.Context, ids []string, posts map[app.ID]*app.Post) error {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	var ceiling time.Time
	if err := q.queryer.QueryRowContext(queryCtx, `SELECT statement_timestamp()`).Scan(&ceiling); err != nil {
		return databaseError(queryCtx, err)
	}
	ceiling = ceiling.UTC()
	for _, post := range posts {
		post.ReplyPreview.Ceiling = ceiling
	}
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT r.id,r.post_id,r.body,r.created_at,r.n,
		a.id,a.type,a.handle,a.display_name,a.initials,a.bio,a.role_label,a.status_text,a.specialty,a.appearance_key,a.verified_at,a.created_at,a.updated_at,a.disabled_at
		FROM unnest($1::uuid[]) requested(id)
		CROSS JOIN LATERAL (
			SELECT limited.*,row_number() OVER (ORDER BY limited.created_at,limited.id) n
			FROM (SELECT rr.id,rr.post_id,rr.author_id,rr.body,rr.created_at FROM replies rr
				WHERE rr.post_id=requested.id AND rr.deleted_at IS NULL AND rr.created_at<=$2
				ORDER BY rr.created_at,rr.id LIMIT 3) limited
		) r JOIN accounts a ON a.id=r.author_id ORDER BY r.post_id,r.created_at,r.id`, ids, ceiling)
	if err != nil {
		return databaseError(queryCtx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var r app.Reply
		var n int
		if err = rows.Scan(&r.ID, &r.PostID, &r.Body, &r.CreatedAt, &n, &r.Author.ID, &r.Author.Type, &r.Author.Handle, &r.Author.DisplayName, &r.Author.Initials, &r.Author.Bio, &r.Author.RoleLabel, &r.Author.StatusText, &r.Author.Specialty, &r.Author.AppearanceKey, &r.Author.VerifiedAt, &r.Author.CreatedAt, &r.Author.UpdatedAt, &r.Author.DisabledAt); err != nil {
			rows.Close()
			return databaseError(queryCtx, err)
		}
		p := posts[r.PostID]
		if p == nil {
			continue
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if n <= 2 {
			p.ReplyPreview.Items = append(p.ReplyPreview.Items, r)
		} else {
			last := p.ReplyPreview.Items[len(p.ReplyPreview.Items)-1]
			p.ReplyPreview.NextPosition = &app.KeysetPosition{Timestamp: last.CreatedAt, ID: last.ID}
		}
	}
	if err = rows.Err(); err != nil {
		return databaseError(queryCtx, err)
	}
	return nil
}

func (s *Store) ListReplies(ctx context.Context, postID app.ID, window app.ReadWindow) (app.ReplyPage, error) {
	parsed, err := app.ParseID(string(postID))
	if err != nil {
		return app.ReplyPage{}, err
	}
	window, err = validateReadWindow(window)
	if err != nil {
		return app.ReplyPage{}, err
	}
	var page app.ReplyPage
	// A continuation ceiling bounds later pages, while each request gets its own
	// repeatable-read snapshot. Commits between page requests can therefore make
	// totals change, but cannot inject replies newer than the initial ceiling.
	err = s.readSnapshot(ctx, func(q *Queries) error {
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		var deleted *time.Time
		if err := q.queryer.QueryRowContext(queryCtx, `SELECT deleted_at FROM posts WHERE id=$1`, parsed).Scan(&deleted); err != nil {
			return databaseError(queryCtx, err)
		}
		if deleted != nil {
			return app.ErrDeleted
		}
		ceiling := window.InitialCeiling
		if ceiling.IsZero() {
			if err := q.queryer.QueryRowContext(queryCtx, `SELECT statement_timestamp()`).Scan(&ceiling); err != nil {
				return databaseError(queryCtx, err)
			}
		}
		page.Ceiling = ceiling.UTC()
		if err := q.visibleReplyTotal(ctx, parsed, &page.ReplyTotal); err != nil {
			return err
		}
		op, order := ">", "ASC"
		if window.Sort == app.ReplySortNewest {
			op = "<"
			order = "DESC"
		}
		positionTime := time.Time{}
		positionID := app.ID("00000000-0000-0000-0000-000000000000")
		hasPosition := window.Position != nil
		if hasPosition {
			positionTime = window.Position.Timestamp
			positionID = window.Position.ID
		}
		query := fmt.Sprintf(`SELECT r.id,r.post_id,r.body,r.created_at,a.id,a.type,a.handle,a.display_name,a.initials,a.bio,a.role_label,a.status_text,a.specialty,a.appearance_key,a.verified_at,a.created_at,a.updated_at,a.disabled_at FROM replies r JOIN accounts a ON a.id=r.author_id WHERE r.post_id=$1 AND r.deleted_at IS NULL AND r.created_at<=$2 AND (NOT $3 OR (r.created_at,r.id) %s ($4,$5)) ORDER BY r.created_at %s,r.id %s LIMIT $6`, op, order, order)
		rows, err := q.queryer.QueryContext(queryCtx, query, parsed, ceiling, hasPosition, positionTime, positionID, window.Limit+1)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		defer rows.Close()
		for rows.Next() {
			var r app.Reply
			if err = rows.Scan(&r.ID, &r.PostID, &r.Body, &r.CreatedAt, &r.Author.ID, &r.Author.Type, &r.Author.Handle, &r.Author.DisplayName, &r.Author.Initials, &r.Author.Bio, &r.Author.RoleLabel, &r.Author.StatusText, &r.Author.Specialty, &r.Author.AppearanceKey, &r.Author.VerifiedAt, &r.Author.CreatedAt, &r.Author.UpdatedAt, &r.Author.DisabledAt); err != nil {
				rows.Close()
				return databaseError(queryCtx, err)
			}
			r.CreatedAt = r.CreatedAt.UTC()
			page.Items = append(page.Items, r)
		}
		if err = rows.Err(); err != nil {
			return databaseError(queryCtx, err)
		}
		if len(page.Items) > window.Limit {
			page.Items = page.Items[:window.Limit]
			last := page.Items[len(page.Items)-1]
			page.NextPosition = &app.KeysetPosition{Timestamp: last.CreatedAt, ID: last.ID}
		}
		return nil
	})
	return page, err
}

func validateReadWindow(window app.ReadWindow) (app.ReadWindow, error) {
	if window.Sort == "" {
		window.Sort = app.ReplySortOldest
	}
	if window.Sort != app.ReplySortOldest && window.Sort != app.ReplySortNewest {
		return window, &app.ValidationError{Fields: map[string]string{"sort": "Must be oldest or newest"}}
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
