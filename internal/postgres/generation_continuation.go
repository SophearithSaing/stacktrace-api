package postgres

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// EnqueueGenerationContinuations is a trusted transaction-bound primitive, not
// human authority or publication. Source actor/result/root are derived exclusively
// from the succeeded parent's exact persisted provenance. A future publisher
// already holding a parent job lock MUST first acquire all required earlier
// source, sorted agent-account/settings and root locks before changing that job.
// See docs/scheduling-and-triggers.md; never acquire new earlier locks afterward.
func (q *Queries) EnqueueGenerationContinuations(ctx context.Context, parentID app.ID) (int, error) {
	return q.enqueueGenerationContinuationsAt(ctx, parentID, nil, rand.Int64N)
}

func (q *Queries) enqueueGenerationContinuationsAt(ctx context.Context, parentID app.ID, now *time.Time, draw func(int64) int64) (int, error) {
	if q.lifetime == nil {
		return 0, errGenerationTransaction
	}
	parent, err := q.GenerationJobByID(ctx, parentID) // Read only: no job lock before sources.
	if err != nil {
		return 0, err
	}
	if parent.Status != app.JobSucceeded || parent.PublishedAttemptID == nil {
		return 0, app.ErrForbidden
	}
	attempt, err := q.GenerationAttemptByID(ctx, *parent.PublishedAttemptID)
	if err != nil {
		return 0, err
	}
	if attempt.JobID != parent.ID || attempt.Status != app.AttemptSucceeded || attempt.LeaseVersion != parent.LeaseVersion || attempt.FinishedAt.After(*parent.FinishedAt) || attempt.StartedAt.Before(parent.AvailableAt) {
		return 0, app.ErrForbidden
	}
	kind, action := app.TriggerHumanPost, parent.ResultPostID
	if parent.OutputKind == app.OutputQuote {
		kind = app.TriggerQuote
	}
	if parent.OutputKind == app.OutputReply {
		kind, action = app.TriggerReply, parent.ResultReplyID
	}
	if action == nil {
		return 0, app.ErrForbidden
	}
	source, err := q.generationSocialSource(ctx, kind, *action)
	if err != nil {
		return 0, err
	}
	if source.actor != parent.AgentID || source.created.Before(parent.CreatedAt) || source.created.After(*parent.FinishedAt) {
		return 0, app.ErrForbidden
	}
	// A reply result must remain in its pinned target conversation; post/quote
	// result shape was checked above against the persisted output kind.
	if parent.OutputKind == app.OutputReply && (parent.SourcePostID == nil || source.post != *parent.SourcePostID) {
		return 0, app.ErrForbidden
	}
	if parent.OutputKind == app.OutputQuote && (parent.SourcePostID == nil || source.quoted == nil || *source.quoted != *parent.SourcePostID) {
		return 0, app.ErrForbidden
	}
	source.kind = app.TriggerContinuation
	return q.admitGenerationSource(ctx, source, &parent, now, draw)
}
