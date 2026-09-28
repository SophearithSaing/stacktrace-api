package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestDisableAccountRevokesSessions(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	actor, token := contentTestActor(t, store, "disable_authigor")
	var sessions int
	if err := store.db.QueryRow(`SELECT count(*) FROM sessions WHERE account_id=$1`, actor).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("fixture session: %d %v", sessions, err)
	}
	if err := store.DisableAccount(ctx, actor); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sessions WHERE account_id=$1`, actor).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("sessions left: %d %v", sessions, err)
	}
	// Disabled accounts resolve nothing through the human write path.
	if err := store.DeletePost(ctx, token, app.NewID()); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("disabled session write: %v", err)
	}
	// Idempotent: repeated disable stays a no-op with nothing to revoke.
	if err := store.DisableAccount(ctx, actor); err != nil {
		t.Fatal(err)
	}
}

func TestDisableAccountIdempotentAndUnknown(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	schedulingAdd(t, store, settings)
	ctx := context.Background()
	for range 3 {
		if err := store.DisableAccount(ctx, settings.AgentID); err != nil {
			t.Fatal(err)
		}
	}
	// Disabling the account leaves agent settings untouched; stopping the
	// agent's publication is a disable effect, never a settings write.
	if got := schedulingRead(t, store, settings.AgentID); !got.Enabled || got.PauseRevision != 0 {
		t.Fatalf("settings mutated: %+v", got)
	}
	if err := store.DisableAccount(ctx, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown disable: %v", err)
	}
}

func TestDisableAgentStopsPublication(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputReply, "An agent disable barrier example")
	ctx := context.Background()
	if err := store.DisableAccount(ctx, job.AgentID); err != nil {
		t.Fatal(err)
	}
	before := publicationCounts(t, store)
	if _, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("disabled publication: %v", err)
	}
	if got, err := store.RetryGeneration(ctx, job.ID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("disabled status retry: %+v %v", got, err)
	}
	if publicationCounts(t, store) != before {
		t.Fatal("disabled agent published")
	}
}

func TestDisableAccountLockOrder(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	schedulingAdd(t, store, settings)
	peer := socialAgent(t, store, "disable_peer", nil)
	ctx := context.Background()
	accountSQL := `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, accountSQL, peer.AgentID); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- store.DisableAccount(callCtx, peer.AgentID) }()
	waitForDatabaseBlock(t, store, pid)
	// While the disable waits on the earlier-order account lock it must have
	// taken neither a later-order settings lock nor a sessions lock.
	if _, err := blocker.ExecContext(ctx, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE NOWAIT`, peer.AgentID); err != nil {
		t.Fatalf("disable took settings: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestDisableAccountCancellationCleanliness(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	schedulingAdd(t, store, settings)
	ctx := context.Background()
	accountSQL := `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, accountSQL, settings.AgentID); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- store.DisableAccount(callCtx, settings.AgentID) }()
	waitForDatabaseBlock(t, store, pid)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	var disabled bool
	if err := store.db.QueryRow(`SELECT disabled_at IS NOT NULL FROM accounts WHERE id=$1`, settings.AgentID).Scan(&disabled); err != nil || disabled {
		t.Fatalf("cancelled disable committed: %v %v", disabled, err)
	}
}

func TestRemovePostAndReply(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputReply, "A trusted removal example")
	ctx := context.Background()
	// Publish first so the reply row (ResultReplyID) exists to remove.
	if _, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output); err != nil {
		t.Fatal(err)
	}
	published := claimJob(t, store, job.ID)
	// The reply is preserved until the second removal; provenance persists.
	if err := store.RemoveReply(ctx, *published.ResultReplyID); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveReply(ctx, *published.ResultReplyID); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("remove reply again: %v", err)
	}
	if err := store.RemovePost(ctx, *job.SourcePostID); err != nil {
		t.Fatal(err)
	}
	if err := store.RemovePost(ctx, *job.SourcePostID); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("remove post again: %v", err)
	}
	// Generation jobs referencing the removed conversation are history-only
	// (retained provenance, no live rows).
	for _, source := range socialJobs(t, store) {
		if source.SourcePostID == nil || *source.SourcePostID != *job.SourcePostID {
			continue
		}
		if source.Status == app.JobPending || source.Status == app.JobRetryWait || source.Status == app.JobRunning {
			t.Fatal("removed source still has live jobs")
		}
	}
	var sourcePostID string
	if err := store.db.QueryRow(`SELECT id FROM posts WHERE id=$1`, *job.SourcePostID).Scan(&sourcePostID); err != nil || sourcePostID == "" {
		t.Fatalf("soft-deleted row removed: %v", err)
	}
}

func TestRemoveUnknownContent(t *testing.T) {
	store := schedulingStore(t)
	ctx := context.Background()
	if err := store.RemovePost(ctx, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown post removal: %v", err)
	}
	if err := store.RemoveReply(ctx, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown reply removal: %v", err)
	}
}

func TestRemoveSourceStopsPublicationWait(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputReply, "A waited removal race example")
	ctx := context.Background()
	// A publication holding the source lock pushes an operator removal behind
	// it; the removal wins on commit, the publication is valid before it.
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `SELECT id FROM posts WHERE id=$1 FOR UPDATE`, *job.SourcePostID); err != nil {
		t.Fatal(err)
	}
	var pid int
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- store.RemovePost(callCtx, *job.SourcePostID) }()
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	waitForDatabaseBlock(t, store, pid)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation after blocked removal: %v", err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.RemovePost(ctx, *job.SourcePostID); err != nil {
		t.Fatal(err)
	}
	before := publicationCounts(t, store)
	if _, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("post-removal publication: %v", err)
	}
	if publicationCounts(t, store) != before {
		t.Fatal("removal changed content")
	}
}

func TestRemoveGeneratedContentProvenance(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputQuote, "A generated removal example")
	ctx := context.Background()
	if _, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output); err != nil {
		t.Fatal(err)
	}
	published := claimJob(t, store, job.ID)
	if err := store.RemovePost(ctx, *published.ResultPostID); err != nil {
		t.Fatal(err)
	}
	var generated bool
	if err := store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM generation_jobs WHERE result_post_id=$1 AND status='succeeded')`, *published.ResultPostID).Scan(&generated); err != nil || !generated {
		t.Fatalf("provenance lost: %+v %v", generated, err)
	}
	// A post that still owns the cached content is individually removed, but the
	// generation attempt/ledger records always remain.
	var attemptID int
	if err := store.db.QueryRow(`SELECT count(*) FROM generation_attempts WHERE job_id=$1`, job.ID).Scan(&attemptID); err != nil || attemptID != 1 {
		t.Fatalf("attempt history erased: %d %v", attemptID, err)
	}
}
