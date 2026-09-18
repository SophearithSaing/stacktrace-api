package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func waitForDatabaseBlock(t *testing.T, store *Store, blockerPID int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := store.db.QueryRowContext(context.Background(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("content operation never reached the source row lock")
}

func lockAndDeletePost(t *testing.T, store *Store, postID app.ID) (*sql.Tx, int) {
	t.Helper()
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	var pid int
	if err = tx.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(context.Background(), `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, postID); err != nil {
		t.Fatal(err)
	}
	return tx, pid
}

func TestSourceDeletionWinsBlockedQuoteAndReplyCreation(t *testing.T) {
	for _, kind := range []string{"quote", "reply"} {
		t.Run(kind, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			_, session := contentTestActor(t, store, "blocked_"+kind)
			parentCreation, _ := app.NewPostCreation("parent", nil, nil)
			parent, err := store.CreatePost(ctx, session, "parent", parentCreation)
			if err != nil {
				t.Fatal(err)
			}
			blocker, pid := lockAndDeletePost(t, store, parent.ID)
			result := make(chan error, 1)
			if kind == "quote" {
				creation, _ := app.NewPostCreation("quote", &parent.ID, nil)
				go func() { _, err := store.CreatePost(ctx, session, "blocked-key", creation); result <- err }()
			} else {
				creation, _ := app.NewReplyCreation(parent.ID, "reply")
				go func() { _, err := store.CreateReply(ctx, session, "blocked-key", creation); result <- err }()
			}
			waitForDatabaseBlock(t, store, pid)
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err = <-result; !errors.Is(err, app.ErrDeleted) {
				t.Fatalf("blocked creation=%v", err)
			}
			var keyCount, contentCount int
			if err = store.db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE key='blocked-key'`).Scan(&keyCount); err != nil {
				t.Fatal(err)
			}
			if kind == "quote" {
				err = store.db.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE body='quote'`).Scan(&contentCount)
			} else {
				err = store.db.QueryRowContext(ctx, `SELECT count(*) FROM replies WHERE body='reply'`).Scan(&contentCount)
			}
			if err != nil || keyCount != 0 || contentCount != 0 {
				t.Fatalf("key=%d content=%d error=%v", keyCount, contentCount, err)
			}
		})
	}
}

func TestCanceledSourceWaitRollsBackReservation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "cancel_wait")
	parentCreation, _ := app.NewPostCreation("parent", nil, nil)
	parent, err := store.CreatePost(ctx, session, "parent", parentCreation)
	if err != nil {
		t.Fatal(err)
	}
	blocker, pid := lockAndDeletePost(t, store, parent.ID)
	creation, _ := app.NewReplyCreation(parent.ID, "reply")
	requestCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { _, err := store.CreateReply(requestCtx, session, "cancel-key", creation); result <- err }()
	waitForDatabaseBlock(t, store, pid)
	cancel()
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled creation=%v", err)
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateReply(ctx, session, "cancel-key", creation); err != nil {
		t.Fatalf("reservation survived cancellation: %v", err)
	}
}

func TestConcurrentReplyIdempotency(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "reply_concurrent")
	postCreation, _ := app.NewPostCreation("parent", nil, nil)
	post, err := store.CreatePost(ctx, session, "parent", postCreation)
	if err != nil {
		t.Fatal(err)
	}
	creation, _ := app.NewReplyCreation(post.ID, "same")
	start := make(chan struct{})
	results := make(chan app.CreateReplyResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			r, err := store.CreateReply(ctx, session, "same-key", creation)
			results <- r
			errs <- err
		}()
	}
	close(start)
	var id app.ID
	for range 2 {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
		r := <-results
		if id == "" {
			id = r.Reply.ID
		} else if r.Reply.ID != id {
			t.Fatalf("different reply IDs")
		}
	}
	left, _ := app.NewReplyCreation(post.ID, "left")
	right, _ := app.NewReplyCreation(post.ID, "right")
	start = make(chan struct{})
	errs = make(chan error, 2)
	go func() { <-start; _, err := store.CreateReply(ctx, session, "different-key", left); errs <- err }()
	go func() { <-start; _, err := store.CreateReply(ctx, session, "different-key", right); errs <- err }()
	close(start)
	conflicts := 0
	for range 2 {
		err = <-errs
		if errors.Is(err, app.ErrConflict) {
			conflicts++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if conflicts != 1 {
		t.Fatalf("conflicts=%d", conflicts)
	}
}
