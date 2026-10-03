package postgres

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

var errGenerationPublicationReplay = errors.New("generation publication replay")

// PublishGeneration is a trusted, job-bound operation, not a human write API.
// Identity, kind, target and persona come exclusively from persisted state.
// Denial returns a safe domain error and rolls back EVERYTHING, including new
// tags: ErrDeleted (source), ErrForbidden (policy), ErrConflict (lease/state),
// or a generation output/safety error. It does not acknowledge/finish denied
// work. Database/commit errors return no result; an exact retry can resolve an
// ambiguous commit without creating content or reenqueuing continuations.
func (s *Store) PublishGeneration(ctx context.Context, id app.ID, version int64, attemptID app.ID, output app.GenerationResult) (app.GenerationJob, error) {
	var published app.GenerationJob
	err := s.Transaction(ctx, func(q *Queries) error {
		var err error
		published, err = q.publishGeneration(ctx, id, version, attemptID, output)
		return err
	})
	if errors.Is(err, errGenerationPublicationReplay) {
		return published, nil // read-only replay; rollback any provisional tags
	}
	if err != nil {
		return app.GenerationJob{}, err
	}
	return published, nil
}

// publicationReplay validates and returns an idempotent publication replay.
func (q *Queries) publicationReplay(ctx context.Context, job app.GenerationJob, version int64, attemptID app.ID, output app.GenerationResult) (app.GenerationJob, error) {
	if job.LeaseVersion != version || job.PublishedAttemptID == nil || *job.PublishedAttemptID != attemptID {
		return app.GenerationJob{}, app.ErrConflict
	}
	attempt, err := q.GenerationAttemptByID(ctx, attemptID)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if attempt.JobID != job.ID || attempt.LeaseVersion != version || attempt.Status != app.AttemptSucceeded ||
		attempt.Decision != app.GenerationPublish || attempt.OutputDigest == "" || attempt.OutputDigest != output.Digest() ||
		output.Decision() != app.GenerationPublish || output.OutputKind() != job.OutputKind {
		return app.GenerationJob{}, app.ErrGenerationOutput
	}
	return job, errGenerationPublicationReplay
}

// publishGeneration validates authority and atomically publishes generated content.
func (q *Queries) publishGeneration(ctx context.Context, id app.ID, version int64, attemptID app.ID, output app.GenerationResult) (app.GenerationJob, error) {
	if q.lifetime == nil {
		return app.GenerationJob{}, errGenerationTransaction
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	refs, err := q.GenerationJobByID(ctx, id)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if refs.Status == app.JobSucceeded {
		return q.publicationReplay(ctx, refs, version, attemptID, output)
	}
	if refs.Status != app.JobRunning || refs.LeaseVersion != version {
		return app.GenerationJob{}, app.ErrConflict
	}
	if output.Decision() != app.GenerationPublish || output.OutputKind() != refs.OutputKind || output.Digest() == "" {
		return app.GenerationJob{}, app.ErrGenerationOutput
	}
	valid, err := q.executionSourceValid(ctx, refs, true)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if !valid {
		return app.GenerationJob{}, app.ErrDeleted
	}
	content := output.Content()
	resultID := app.NewID()
	source := generationSource{kind: app.TriggerContinuation, action: resultID, actor: refs.AgentID,
		post: resultID, priorityAuthor: refs.AgentID, body: content.Body}
	if refs.OutputKind == app.OutputReply {
		source.post, source.reply = *refs.SourcePostID, &resultID
	}
	if refs.OutputKind == app.OutputQuote {
		source.quoted = refs.SourcePostID
	}
	if refs.OutputKind != app.OutputPost {
		if err := q.queryer.QueryRowContext(ctx, `SELECT author_id FROM posts WHERE id=$1`, refs.SourcePostID).Scan(&source.priorityAuthor); err != nil {
			return app.GenerationJob{}, databaseError(ctx, err)
		}
	}
	var tags []app.ID
	if refs.OutputKind != app.OutputReply {
		tags, err = q.resolveTags(ctx, content.Tags)
		if err != nil {
			return app.GenerationJob{}, err
		}
	}
	ids, err := q.generationCandidateIDs(ctx, source)
	if err != nil {
		return app.GenerationJob{}, err
	}
	candidates, settings, err := q.lockPreparedGenerationCandidates(ctx, source, ids, true, &refs)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if settings == nil {
		return app.GenerationJob{}, app.ErrForbidden
	}
	root, err := q.lockExecutionJob(ctx, refs.RootJobID)
	if err != nil {
		return app.GenerationJob{}, err
	}
	job := root
	if refs.RootJobID != id {
		job, err = q.lockExecutionJob(ctx, id)
		if err != nil {
			return app.GenerationJob{}, err
		}
	}
	if job.Status == app.JobSucceeded {
		return q.publicationReplay(ctx, job, version, attemptID, output)
	}
	if job.Status != app.JobRunning || job.LeaseVersion != version ||
		!sameAttemptSource(refs.SourceRepostID, job.SourceRepostID) {
		return app.GenerationJob{}, app.ErrConflict
	}
	var locked app.ID
	if err := q.queryer.QueryRowContext(ctx, `SELECT id FROM generation_attempts WHERE id=$1 AND job_id=$2 FOR UPDATE`, attemptID, id).Scan(&locked); err != nil {
		return app.GenerationJob{}, databaseError(ctx, err)
	}
	attempt, err := q.GenerationAttemptByID(ctx, locked)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if attempt.Decision != app.GenerationPublish {
		return app.GenerationJob{}, app.ErrGenerationOutput
	}
	if attempt.PauseRevision == nil || *attempt.PauseRevision != settings.PauseRevision {
		return app.GenerationJob{}, app.ErrForbidden
	}
	recent, err := q.publicationRecentContent(ctx, job.AgentID)
	if err != nil {
		return app.GenerationJob{}, err
	}
	now, err := q.generationClock(ctx, nil)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if job.ValidateLease(version, now) != nil {
		return app.GenerationJob{}, app.ErrConflict
	}
	if err := app.ValidateGenerationPublicationOutput(job, attempt, output, recent, now); err != nil {
		return app.GenerationJob{}, err
	}
	allowed, err := q.executionPolicyAllowed(ctx, job, settings.Policy, settings.LastPublishedAt, now)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if !allowed {
		return app.GenerationJob{}, app.ErrForbidden
	}
	var sourceCreated time.Time
	if job.TriggerKind != app.TriggerScheduled {
		sourceCreated, err = q.executionSourceCreatedAt(ctx, job)
		if err != nil {
			return app.GenerationJob{}, err
		}
	}
	// Use one content/finish timestamp; recheck authority after ALL intervening
	// SQL below. A failed final check rolls back content and child reservations.
	after := job
	after.Status, after.FinishedAt, after.LeaseExpiresAt = app.JobSucceeded, &now, nil
	after.PublishedAttemptID = &attemptID
	if job.OutputKind == app.OutputReply {
		after.ResultReplyID = &resultID
	} else {
		after.ResultPostID = &resultID
	}
	if app.ValidateGenerationJobTransition(job, after, version, now, &attempt) != nil {
		return app.GenerationJob{}, app.ErrGenerationOutput
	}
	if err := q.insertGenerationContent(ctx, after, content, resultID, tags, now); err != nil {
		return app.GenerationJob{}, err
	}
	updated, err := q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status='succeeded',result_post_id=$3,result_reply_id=$4,
		published_attempt_id=$5,finished_at=$6,lease_expires_at=NULL
		WHERE id=$1 AND lease_version=$2 AND status='running'`, id, version, after.ResultPostID, after.ResultReplyID, attemptID, now)
	if err := generationClaimMutation(ctx, updated, err); err != nil {
		return app.GenerationJob{}, err
	}
	updated, err = q.queryer.ExecContext(ctx, `UPDATE agent_settings SET last_published_at=$2,updated_at=$2 WHERE agent_id=$1`, job.AgentID, now)
	if err := generationClaimMutation(ctx, updated, err); err != nil {
		return app.GenerationJob{}, err
	}
	source.created = now
	if root.ID == job.ID {
		root = after
	}
	if _, err := q.admitPreparedGenerationSource(ctx, source, &after, root, candidates, nil, rand.Int64N); err != nil {
		return app.GenerationJob{}, err
	}
	// Account visibility can change while earlier source locks are held. Re-read
	// it without acquiring any earlier-order lock before the final clock.
	valid, err = q.executionSourceValid(ctx, job, false)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if !valid {
		return app.GenerationJob{}, app.ErrDeleted
	}
	fresh, err := q.generationClock(ctx, nil)
	if err != nil {
		return app.GenerationJob{}, err
	}
	if fresh.Before(now) || job.ValidateLease(version, fresh) != nil {
		return app.GenerationJob{}, app.ErrConflict
	}
	if !executionTriggerTimeAllowed(job, settings.Policy, sourceCreated, fresh) {
		return app.GenerationJob{}, app.ErrForbidden
	}
	return after, nil
}

// insertGenerationContent inserts generated post or reply content and its provenance.
func (q *Queries) insertGenerationContent(ctx context.Context, job app.GenerationJob, content app.Content, id app.ID, tags []app.ID, now time.Time) error {
	if job.OutputKind == app.OutputReply {
		result, err := q.queryer.ExecContext(ctx, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,$5)`, id, job.SourcePostID, job.AgentID, content.Body, now)
		return generationClaimMutation(ctx, result, err)
	}
	var quote *app.ID
	if job.OutputKind == app.OutputQuote {
		quote = job.SourcePostID
	}
	var language, filename, source any
	if content.Code != nil {
		language, filename, source = content.Code.Language, content.Code.Filename, content.Code.Source
	}
	result, err := q.queryer.ExecContext(ctx, `INSERT INTO posts(id,author_id,body,quoted_post_id,code_language,code_filename,code_source,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, job.AgentID, content.Body, quote, language, filename, source, now)
	if err := generationClaimMutation(ctx, result, err); err != nil {
		return err
	}
	return q.linkTags(ctx, id, tags)
}
