package postgres

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// generationSource is server-derived, never a request or model-supplied target.
type generationSource struct {
	kind                                app.GenerationTrigger
	action, actor, post, priorityAuthor app.ID
	reply, repost                       *app.ID
	body                                string
	created                             time.Time
}

// Caller holds the authenticated human account lock, then the source locks.
// Only fresh writes call this hook; retry/no-op paths must not invoke it.
func (q *Queries) enqueueSocialGeneration(ctx context.Context, kind app.GenerationTrigger, action, actor app.ID) error {
	_, err := q.enqueueSocialGenerationAt(ctx, kind, action, actor, nil, rand.Int64N)
	return err
}

func (q *Queries) enqueueSocialGenerationAt(ctx context.Context, kind app.GenerationTrigger, action, actor app.ID, now *time.Time, draw func(int64) int64) (int, error) {
	if q.lifetime == nil {
		return 0, errGenerationTransaction
	}
	source, err := q.generationSocialSource(ctx, kind, action)
	if err != nil {
		return 0, err
	}
	if source.actor != actor {
		return 0, app.ErrForbidden
	}
	return q.admitGenerationSource(ctx, source, nil, now, draw)
}

func (q *Queries) generationSocialSource(ctx context.Context, kind app.GenerationTrigger, action app.ID) (generationSource, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	source := generationSource{kind: kind, action: action, post: action}
	switch kind {
	case app.TriggerReply:
		if err := q.queryer.QueryRowContext(ctx, `SELECT post_id FROM replies WHERE id=$1`, action).Scan(&source.post); err != nil {
			return source, databaseError(ctx, err)
		}
		source.reply = &source.action
	case app.TriggerRepost:
		if err := q.queryer.QueryRowContext(ctx, `SELECT post_id FROM reposts WHERE id=$1`, action).Scan(&source.post); err != nil {
			return source, databaseError(ctx, err)
		}
		source.repost = &source.action
	case app.TriggerHumanPost, app.TriggerQuote:
	default:
		return source, app.ErrForbidden
	}
	var deleted *time.Time
	var quoted *app.ID
	if err := q.queryer.QueryRowContext(ctx, `SELECT author_id,body,created_at,deleted_at,quoted_post_id FROM posts WHERE id=$1 FOR UPDATE`, source.post).Scan(
		&source.actor, &source.body, &source.created, &deleted, &quoted); err != nil {
		return source, databaseError(ctx, err)
	}
	if deleted != nil {
		return source, app.ErrDeleted
	}
	source.priorityAuthor = source.actor
	switch kind {
	case app.TriggerReply:
		if err := q.queryer.QueryRowContext(ctx, `SELECT author_id,body,created_at,deleted_at FROM replies WHERE id=$1 AND post_id=$2 FOR UPDATE`, action, source.post).Scan(
			&source.actor, &source.body, &source.created, &deleted); err != nil {
			return source, databaseError(ctx, err)
		}
		if deleted != nil {
			return source, app.ErrDeleted
		}
	case app.TriggerRepost:
		if err := q.queryer.QueryRowContext(ctx, `SELECT account_id,created_at FROM reposts WHERE id=$1 AND post_id=$2 FOR UPDATE`, action, source.post).Scan(&source.actor, &source.created); err != nil {
			return source, databaseError(ctx, err)
		}
	case app.TriggerQuote:
		if quoted == nil {
			return source, app.ErrForbidden
		}
		// The new quote is the source/target. Its quoted author is only a
		// candidate hint; deletion of the original does not hide the new quote.
		if err := q.queryer.QueryRowContext(ctx, `SELECT author_id FROM posts WHERE id=$1`, quoted).Scan(&source.priorityAuthor); err != nil {
			return source, databaseError(ctx, err)
		}
	case app.TriggerHumanPost:
		if quoted != nil {
			return source, app.ErrForbidden
		}
	}
	return source, nil
}
