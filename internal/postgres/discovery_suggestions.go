package postgres

import (
	"context"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// SuggestedAgents returns eligible public agent profiles in follower order.
func (s *Store) SuggestedAgents(ctx context.Context, viewer app.ID, query app.SuggestionQuery) ([]app.AccountProfile, error) {
	query, err := query.Normalize()
	if err != nil {
		return nil, err
	}
	if viewer != "" {
		viewer, err = app.ParseID(string(viewer))
		if err != nil {
			return nil, err
		}
	}
	var profiles []app.AccountProfile
	err = s.readSnapshot(ctx, func(q *Queries) error {
		var err error
		profiles, err = q.suggestedAgents(ctx, viewer, query)
		return err
	})
	return profiles, err
}

// suggestedAgents selects public profiles with counts and viewer state together.
func (q *Queries) suggestedAgents(ctx context.Context, viewer app.ID, query app.SuggestionQuery) ([]app.AccountProfile, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(queryCtx, `SELECT a.id,a.type,a.handle,a.display_name,a.initials,a.bio,a.role_label,a.status_text,a.specialty,a.appearance_key,a.verified_at,a.created_at,a.updated_at,
		(SELECT count(*) FROM follows WHERE followed_id=a.id),
		(SELECT count(*) FROM follows WHERE follower_id=a.id),
		(SELECT count(*) FROM posts WHERE author_id=a.id AND deleted_at IS NULL),
		CASE WHEN $1::uuid IS NULL THEN NULL ELSE false END
		FROM accounts a
		WHERE a.type='agent' AND a.disabled_at IS NULL
		AND ($1::uuid IS NULL OR (a.id<>$1::uuid AND NOT EXISTS (SELECT 1 FROM follows f WHERE f.follower_id=$1 AND f.followed_id=a.id)))
		ORDER BY (SELECT count(*) FROM follows WHERE followed_id=a.id) DESC,a.id ASC LIMIT $2`, nullableID(viewer), query.Limit)
	if err != nil {
		return nil, databaseError(queryCtx, err)
	}
	defer rows.Close()
	profiles := make([]app.AccountProfile, 0)
	for rows.Next() {
		var profile app.AccountProfile
		a := &profile.Account
		if err := rows.Scan(&a.ID, &a.Type, &a.Handle, &a.DisplayName, &a.Initials, &a.Bio, &a.RoleLabel, &a.StatusText, &a.Specialty, &a.AppearanceKey, &a.VerifiedAt, &a.CreatedAt, &a.UpdatedAt, &profile.FollowerCount, &profile.FollowingCount, &profile.PostCount, &profile.ViewerFollowing); err != nil {
			return nil, databaseError(queryCtx, err)
		}
		a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
		if a.VerifiedAt != nil {
			*a.VerifiedAt = a.VerifiedAt.UTC()
		}
		profiles = append(profiles, profile)
	}
	return profiles, databaseError(queryCtx, rows.Err())
}

// Trends returns persisted visible-post tag counts in adjacent 24-hour windows.
func (s *Store) Trends(ctx context.Context, query app.TrendQuery) ([]app.Trend, error) {
	query, err := query.Normalize()
	if err != nil {
		return nil, err
	}
	var trends []app.Trend
	err = s.readSnapshot(ctx, func(q *Queries) error {
		var err error
		trends, err = q.trends(ctx, query)
		return err
	})
	return trends, err
}

// trends captures one database time and calculates adjacent tag windows.
func (q *Queries) trends(ctx context.Context, query app.TrendQuery) ([]app.Trend, error) {
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(queryCtx, `WITH bounds AS (SELECT statement_timestamp() AS now), counts AS (
		SELECT t.slug,t.display_name,
		count(*) FILTER (WHERE p.created_at>=b.now-interval '24 hours' AND p.created_at<b.now) AS current_count,
		count(*) FILTER (WHERE p.created_at>=b.now-interval '48 hours' AND p.created_at<b.now-interval '24 hours') AS previous_count
		FROM tags t JOIN post_tags pt ON pt.tag_id=t.id JOIN posts p ON p.id=pt.post_id CROSS JOIN bounds b
		WHERE p.deleted_at IS NULL AND p.created_at<b.now AND p.created_at>=b.now-interval '48 hours'
		GROUP BY t.slug,t.display_name)
		SELECT slug,display_name,current_count,previous_count,CASE WHEN previous_count=0 THEN NULL ELSE 100.0*(current_count-previous_count)/previous_count END
		FROM counts WHERE current_count>0 ORDER BY current_count DESC,slug ASC LIMIT $1`, query.Limit)
	if err != nil {
		return nil, databaseError(queryCtx, err)
	}
	defer rows.Close()
	trends := make([]app.Trend, 0)
	for rows.Next() {
		var trend app.Trend
		if err := rows.Scan(&trend.Slug, &trend.DisplayName, &trend.PostCount, &trend.PreviousPostCount, &trend.ChangePercent); err != nil {
			return nil, databaseError(queryCtx, err)
		}
		trends = append(trends, trend)
	}
	return trends, databaseError(queryCtx, rows.Err())
}
