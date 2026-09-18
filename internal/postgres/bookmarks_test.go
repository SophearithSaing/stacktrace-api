package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestBookmarkPagePrivateNewestKeysetAndCeiling(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	owner, ownerSession := contentTestActor(t, store, "bookmark_owner")
	_, otherSession := contentTestActor(t, store, "bookmark_other")

	posts := make([]app.Post, 4)
	for i := range posts {
		posts[i] = interactionTestPost(t, store, ownerSession, "bookmark-post-"+string(rune('a'+i)))
		if _, err := store.SetBookmark(ctx, ownerSession, posts[i].ID, true); err != nil {
			t.Fatal(err)
		}
	}
	tied := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	older := tied.Add(-time.Minute)
	if _, err := store.db.ExecContext(ctx, `UPDATE bookmarks SET created_at=CASE WHEN post_id=ANY($2::uuid[]) THEN $3::timestamptz ELSE $4::timestamptz END WHERE account_id=$1`, owner, []string{string(posts[0].ID), string(posts[1].ID), string(posts[2].ID)}, tied, older); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBookmark(ctx, otherSession, posts[0].ID, true); err != nil {
		t.Fatal(err)
	}

	page, err := store.ListBookmarks(ctx, ownerSession, app.ReadWindow{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextPosition == nil {
		t.Fatalf("first page=%+v error=%v", page, err)
	}
	if string(page.Items[0].ID) < string(page.Items[1].ID) {
		t.Fatalf("equal timestamps not ordered by post ID descending: %s then %s", page.Items[0].ID, page.Items[1].ID)
	}
	for _, post := range page.Items {
		if post.Viewer == nil || !post.Viewer.Bookmarked {
			t.Fatalf("bookmark page lacked owner viewer state: %+v", post)
		}
	}

	late := interactionTestPost(t, store, ownerSession, "bookmark-late")
	if _, err = store.SetBookmark(ctx, ownerSession, late.ID, true); err != nil {
		t.Fatal(err)
	}
	next, err := store.ListBookmarks(ctx, ownerSession, app.ReadWindow{Sort: app.ReplySortNewest, Limit: 2, Position: page.NextPosition, InitialCeiling: page.Ceiling})
	if err != nil || len(next.Items) != 2 || next.NextPosition != nil {
		t.Fatalf("next page=%+v error=%v", next, err)
	}
	for _, post := range next.Items {
		if post.ID == late.ID {
			t.Fatal("continuation included bookmark newer than initial ceiling")
		}
	}

	otherPage, err := store.ListBookmarks(ctx, otherSession, app.ReadWindow{})
	if err != nil || len(otherPage.Items) != 1 || otherPage.Items[0].ID != posts[0].ID {
		t.Fatalf("other private page=%+v error=%v", otherPage, err)
	}
	if otherPage.Items[0].Counts.ReactionsTotal != 0 || otherPage.Items[0].Counts.Reposts != 0 {
		t.Fatalf("bookmark fabricated public aggregates: %+v", otherPage.Items[0].Counts)
	}

	if err = store.DeletePost(ctx, ownerSession, posts[3].ID); err != nil {
		t.Fatal(err)
	}
	visible, err := store.ListBookmarks(ctx, ownerSession, app.ReadWindow{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, post := range visible.Items {
		if post.ID == posts[3].ID {
			t.Fatal("deleted post appeared in bookmarks")
		}
	}
}

func TestBookmarkPageAuthorizationAndWindowValidation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	account, session := contentTestActor(t, store, "bookmark_auth")
	post := interactionTestPost(t, store, session, "bookmark-auth-post")
	if _, err := store.SetBookmark(ctx, session, post.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListBookmarks(ctx, "", app.ReadWindow{}); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("anonymous bookmarks=%v", err)
	}
	if _, err := store.ListBookmarks(ctx, session, app.ReadWindow{Sort: app.ReplySortOldest}); err == nil {
		t.Fatal("oldest bookmark sort accepted")
	}
	if _, err := store.ListBookmarks(ctx, session, app.ReadWindow{Limit: 51}); err == nil {
		t.Fatal("oversized bookmark page accepted")
	}
	position := &app.KeysetPosition{Timestamp: time.Now(), ID: app.NewID()}
	if _, err := store.ListBookmarks(ctx, session, app.ReadWindow{Position: position}); err == nil {
		t.Fatal("continuation without ceiling accepted")
	}
	badPosition := &app.KeysetPosition{Timestamp: time.Now(), ID: app.ID("bad")}
	if _, err := store.ListBookmarks(ctx, session, app.ReadWindow{Position: badPosition, InitialCeiling: time.Now()}); !errors.Is(err, app.ErrInvalidID) {
		t.Fatalf("bad position ID=%v", err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListBookmarks(ctx, session, app.ReadWindow{}); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("disabled account bookmarks=%v", err)
	}
}
