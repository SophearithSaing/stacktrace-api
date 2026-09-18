package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func contentTestActor(t *testing.T, store *Store, handle string) (app.ID, string) {
	t.Helper()
	id := app.NewID()
	sum := sha256.Sum256([]byte("session-" + handle))
	token := hex.EncodeToString(sum[:])
	_, err := store.db.ExecContext(context.Background(), `INSERT INTO accounts(id,type,handle,display_name,created_at,updated_at) VALUES($1,'human',$2,$2,now(),now())`, id, handle)
	if err == nil {
		_, err = store.db.ExecContext(context.Background(), `INSERT INTO sessions(token_hash,account_id,created_at,expires_at) VALUES($1,$2,now(),now()+interval '1 hour')`, token, id)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id, token
}

func TestContentCreateProjectRetryDelete(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	actor, session := contentTestActor(t, store, "writer")
	code := &app.Code{Language: " go ", Filename: " main.go ", Source: "package main\n"}
	creation, err := app.NewPostCreation("  hello #Go #go  ", nil, code)
	if err != nil {
		t.Fatal(err)
	}
	post, err := store.CreatePost(ctx, session, "post-key", creation)
	if err != nil {
		t.Fatal(err)
	}
	if post.Author.ID != actor || post.Content.Body != "hello #Go #go" || post.Content.Code == nil || post.Content.Code.Language != "go" || len(post.Content.Tags) != 1 || post.Content.Tags[0].DisplayName != "Go" {
		t.Fatalf("unexpected post: %+v", post)
	}
	retry, err := store.CreatePost(ctx, session, "post-key", app.PostCreation{Content: app.Content{Body: "hello #Go #go", Tags: []app.Tag{{Slug: "forged", DisplayName: "forged"}}, Code: code}})
	if err != nil || retry.ID != post.ID {
		t.Fatalf("retry=%+v error=%v", retry, err)
	}
	quoteCreation, _ := app.NewPostCreation("quote", &post.ID, nil)
	quote, err := store.CreatePost(ctx, session, "quote-key", quoteCreation)
	if err != nil || quote.Quote == nil || quote.Quote.Body != post.Content.Body {
		t.Fatalf("quote=%+v error=%v", quote, err)
	}
	replyCreation, _ := app.NewReplyCreation(post.ID, " reply ")
	reply, err := store.CreateReply(ctx, session, "reply-key", replyCreation)
	if err != nil || reply.ReplyTotal != 1 || reply.Reply.Body != "reply" {
		t.Fatalf("reply=%+v error=%v", reply, err)
	}
	read, err := store.PostByID(ctx, post.ID, actor)
	if err != nil || read.Counts.Replies != 1 || len(read.ReplyPreview.Items) != 1 || read.Viewer == nil {
		t.Fatalf("read=%+v error=%v", read, err)
	}
	if err := store.DeletePost(ctx, session, post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostByID(ctx, post.ID, ""); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("deleted read=%v", err)
	}
	quote, err = store.PostByID(ctx, quote.ID, "")
	if err != nil || quote.Quote == nil || quote.Quote.Availability != app.ContentDeleted || quote.Quote.Body != "" || quote.Quote.Author != nil {
		t.Fatalf("redacted quote=%+v error=%v", quote, err)
	}
	if _, err := store.CreatePost(ctx, session, "post-key", creation); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("deleted retry=%v", err)
	}
	if _, err := store.CreateReply(ctx, session, "reply-key", replyCreation); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("reply retry with deleted parent=%v", err)
	}
}

func TestReplyPageEqualTimestampKeyset(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "pager")
	creation, _ := app.NewPostCreation("parent", nil, nil)
	post, err := store.CreatePost(ctx, session, "parent", creation)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	ids := []app.ID{app.NewID(), app.NewID(), app.NewID()}
	for _, id := range ids {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO replies(id,post_id,author_id,body,created_at) SELECT $1,$2,account_id,'same',$3 FROM sessions WHERE token_hash=$4`, id, post.ID, stamp, session); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.ListReplies(ctx, post.ID, app.ReadWindow{Sort: app.ReplySortOldest, Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextPosition == nil || page.ReplyTotal != 3 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	next, err := store.ListReplies(ctx, post.ID, app.ReadWindow{Sort: app.ReplySortOldest, Limit: 2, Position: page.NextPosition, InitialCeiling: page.Ceiling})
	if err != nil || len(next.Items) != 1 {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	newest, err := store.ListReplies(ctx, post.ID, app.ReadWindow{Sort: app.ReplySortNewest, Limit: 2})
	if err != nil || len(newest.Items) != 2 || newest.Items[0].ID != next.Items[0].ID {
		t.Fatalf("newest=%+v err=%v", newest, err)
	}
}

func TestConcurrentIdempotentPostCreation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "concurrent")
	creation, _ := app.NewPostCreation("same normalized body", nil, nil)
	start := make(chan struct{})
	results := make(chan app.Post, 2)
	errorsCh := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			post, err := store.CreatePost(ctx, session, "concurrent-key", creation)
			results <- post
			errorsCh <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id app.ID
	for post := range results {
		if id == "" {
			id = post.ID
		} else if post.ID != id {
			t.Fatalf("different retry results: %s and %s", id, post.ID)
		}
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM posts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("post count=%d error=%v", count, err)
	}
	replyCreation, _ := app.NewReplyCreation(id, "reply")
	if _, err := store.CreateReply(ctx, session, "concurrent-key", replyCreation); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("cross-operation key error=%v", err)
	}
}

func TestBatchHydrationSkipsUnavailableIDs(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "batcher")
	creation, _ := app.NewPostCreation("visible", nil, nil)
	visible, err := store.CreatePost(ctx, session, "visible", creation)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.CreatePost(ctx, session, "deleted", creation)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DeletePost(ctx, session, deleted.ID); err != nil {
		t.Fatal(err)
	}
	unknown := app.NewID()
	var hydrated []app.Post
	err = store.readSnapshot(ctx, func(q *Queries) error {
		var err error
		hydrated, err = q.hydratePosts(ctx, []app.ID{deleted.ID, visible.ID, unknown}, "")
		return err
	})
	if err != nil || len(hydrated) != 1 || hydrated[0].ID != visible.ID {
		t.Fatalf("hydrated=%+v error=%v", hydrated, err)
	}
}

func TestContentOwnershipCountsAndRetryStates(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	owner, ownerSession := contentTestActor(t, store, "content_owner2")
	_, otherSession := contentTestActor(t, store, "content_other2")
	baseCreation, _ := app.NewPostCreation("base", nil, nil)
	base, err := store.CreatePost(ctx, ownerSession, "base", baseCreation)
	if err != nil {
		t.Fatal(err)
	}
	replyCreation, _ := app.NewReplyCreation(base.ID, "answer")
	reply, err := store.CreateReply(ctx, ownerSession, "answer", replyCreation)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DeletePost(ctx, otherSession, base.ID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("nonowner post delete=%v", err)
	}
	if err = store.DeleteReply(ctx, otherSession, reply.Reply.ID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("nonowner reply delete=%v", err)
	}
	profile, err := store.ProfileByID(ctx, owner, "")
	if err != nil || profile.PostCount != 1 {
		t.Fatalf("profile=%+v error=%v", profile, err)
	}
	if err = store.DeleteReply(ctx, ownerSession, reply.Reply.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateReply(ctx, ownerSession, "answer", replyCreation); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("deleted reply retry=%v", err)
	}
	if err = store.DeletePost(ctx, ownerSession, base.ID); err != nil {
		t.Fatal(err)
	}
	profile, err = store.ProfileByID(ctx, owner, "")
	if err != nil || profile.PostCount != 0 {
		t.Fatalf("deleted profile=%+v error=%v", profile, err)
	}
	if err = store.DeletePost(ctx, ownerSession, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown post delete=%v", err)
	}
	if err = store.DeleteReply(ctx, ownerSession, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown reply delete=%v", err)
	}
}

func TestQuoteRetryAndOneLevelProjection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "quoter")
	baseCreation, _ := app.NewPostCreation("source", nil, nil)
	base, err := store.CreatePost(ctx, session, "source", baseCreation)
	if err != nil {
		t.Fatal(err)
	}
	quoteCreation, _ := app.NewPostCreation("first quote", &base.ID, nil)
	quote, err := store.CreatePost(ctx, session, "quote-retry", quoteCreation)
	if err != nil {
		t.Fatal(err)
	}
	quoteOfQuoteCreation, _ := app.NewPostCreation("second quote", &quote.ID, nil)
	nested, err := store.CreatePost(ctx, session, "quote-two", quoteOfQuoteCreation)
	if err != nil {
		t.Fatal(err)
	}
	if nested.Quote == nil || nested.Quote.ID != quote.ID || nested.Quote.Body != "first quote" {
		t.Fatalf("nested quote=%+v", nested)
	}
	if err = store.DeletePost(ctx, session, base.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := store.CreatePost(ctx, session, "quote-retry", quoteCreation)
	if err != nil || retry.ID != quote.ID || retry.Quote == nil || retry.Quote.Availability != app.ContentDeleted {
		t.Fatalf("redacted retry=%+v error=%v", retry, err)
	}
	if err = store.DeletePost(ctx, session, quote.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreatePost(ctx, session, "quote-retry", quoteCreation); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("deleted quote result retry=%v", err)
	}
}

func TestFailedCreationRollbackAndSessionAuthorization(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	human, session := contentTestActor(t, store, "authorized")
	missing := app.NewID()
	badQuote, _ := app.NewPostCreation("quote", &missing, nil)
	if _, err := store.CreatePost(ctx, session, "reusable", badQuote); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing source=%v", err)
	}
	good, _ := app.NewPostCreation("corrected", nil, nil)
	if _, err := store.CreatePost(ctx, session, "reusable", good); err != nil {
		t.Fatalf("rolled-back key not reusable: %v", err)
	}
	different, _ := app.NewPostCreation("different", nil, nil)
	if _, err := store.CreatePost(ctx, session, "reusable", different); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("different payload=%v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE author_id=$1`, human).Scan(&count); err != nil || count != 1 {
		t.Fatalf("post count=%d error=%v", count, err)
	}
	if _, err := store.CreatePost(ctx, strings.Repeat("0", 64), "none", good); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("unknown session=%v", err)
	}
	_, expired := contentTestActor(t, store, "expired_user")
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET expires_at=created_at+interval '1 microsecond' WHERE token_hash=$1`, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePost(ctx, expired, "expired", good); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("expired session=%v", err)
	}
	disabled, disabledSession := contentTestActor(t, store, "disabled_user")
	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, disabled); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePost(ctx, disabledSession, "disabled", good); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("disabled session=%v", err)
	}
	agent := app.NewID()
	agentHash := strings.Repeat("a", 64)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO accounts(id,type,handle,display_name,created_at,updated_at) VALUES($1,'agent','agent_writer','Agent',now(),now())`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,account_id,created_at,expires_at) VALUES($1,$2,now(),now()+interval '1 hour')`, agentHash, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePost(ctx, agentHash, "agent", good); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("agent session=%v", err)
	}
}
