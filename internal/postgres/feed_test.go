package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func feedTestStore(t *testing.T) *Store {
	t.Helper()
	store := testStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func feedExec(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func feedPost(t *testing.T, store *Store, session, key, body string) app.Post {
	t.Helper()
	creation, err := app.NewPostCreation(body, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, err := store.CreatePost(context.Background(), session, key, creation)
	if err != nil {
		t.Fatal(err)
	}
	return post
}

func feedRead(t *testing.T, store *Store, viewer app.ID, session string, query app.FeedQuery) app.FeedPage {
	t.Helper()
	page, err := store.ListFeed(context.Background(), viewer, session, query)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func feedKeys(page app.FeedPage) []string {
	keys := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		keys = append(keys, string(item.Kind)+":"+string(item.ID))
	}
	return keys
}

func TestFeedViewsTagsAndAccounts(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	viewer, session := contentTestActor(t, store, "feed_viewer")
	followed, followedSession := contentTestActor(t, store, "feed_followed")
	outsider, outsiderSession := contentTestActor(t, store, "feed_outsider")
	selfPost := feedPost(t, store, session, "self", "self #Go")
	followedPost := feedPost(t, store, followedSession, "followed", "followed #Rust")
	outsidePost := feedPost(t, store, outsiderSession, "outside", "outside #Go")
	reactedPost := feedPost(t, store, outsiderSession, "reacted", "reaction only #Rust")
	feedExec(t, store, `UPDATE posts SET is_spicy=true WHERE id IN ($1,$2)`, followedPost.ID, outsidePost.ID)
	if _, err := store.SetFollow(ctx, session, followed, true); err != nil {
		t.Fatal(err)
	}
	followedRepost, selfRepost, outsideRepost := app.NewID(), app.NewID(), app.NewID()
	feedExec(t, store, `INSERT INTO reposts(id,account_id,post_id,created_at) VALUES($1,$2,$3,statement_timestamp()),($4,$5,$6,statement_timestamp()),($7,$8,$9,statement_timestamp())`,
		followedRepost, followed, outsidePost.ID, selfRepost, viewer, reactedPost.ID, outsideRepost, outsider, selfPost.ID)
	spicy := app.ReactionSpicy
	if _, err := store.SetReaction(ctx, session, reactedPost.ID, &spicy); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		query app.FeedQuery
		want  map[string]bool
	}{
		{"following", app.FeedQuery{View: app.FeedViewFollowing}, map[string]bool{"post:" + string(selfPost.ID): true, "post:" + string(followedPost.ID): true, "repost:" + string(followedRepost): true, "repost:" + string(selfRepost): true}},
		{"following tag", app.FeedQuery{View: app.FeedViewFollowing, Tag: "GO"}, map[string]bool{"post:" + string(selfPost.ID): true, "repost:" + string(followedRepost): true}},
		{"spicy", app.FeedQuery{View: app.FeedViewSpicy}, map[string]bool{"post:" + string(followedPost.ID): true, "post:" + string(outsidePost.ID): true, "repost:" + string(followedRepost): true}},
		{"spicy tag", app.FeedQuery{View: app.FeedViewSpicy, Tag: "go"}, map[string]bool{"post:" + string(outsidePost.ID): true, "repost:" + string(followedRepost): true}},
		{"global tag", app.FeedQuery{Tag: "Go"}, map[string]bool{"post:" + string(selfPost.ID): true, "post:" + string(outsidePost.ID): true, "repost:" + string(followedRepost): true, "repost:" + string(outsideRepost): true}},
		{"unknown tag", app.FeedQuery{Tag: "unknown"}, map[string]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := feedRead(t, store, viewer, session, tc.query)
			if len(page.Items) != len(tc.want) {
				t.Fatalf("got=%v want=%v", feedKeys(page), tc.want)
			}
			for _, key := range feedKeys(page) {
				if !tc.want[key] {
					t.Fatalf("unexpected %s", key)
				}
			}
		})
	}
	global := feedRead(t, store, "", "", app.FeedQuery{})
	if len(global.Items) != 7 {
		t.Fatalf("global=%v", feedKeys(global))
	}
	for _, item := range global.Items {
		if item.Post.Viewer != nil || (item.Kind == app.FeedKindRepost) != (item.Reposter != nil) {
			t.Fatalf("projection=%+v", item)
		}
	}
	account, err := store.ListAccountFeed(ctx, followed, viewer, app.FeedWindow{Limit: 1})
	if err != nil || len(account.Items) != 1 || account.Items[0].ID != followedRepost || account.Items[0].Reposter.ID != followed || account.NextPosition == nil {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	next, err := store.ListAccountFeed(ctx, followed, viewer, app.FeedWindow{Limit: 1, Position: account.NextPosition, InitialCeiling: account.Ceiling})
	if err != nil || len(next.Items) != 1 || next.Items[0].Post.ID != followedPost.ID || next.NextPosition != nil {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	if _, err := store.ListAccountFeed(ctx, app.NewID(), "", app.FeedWindow{}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown=%v", err)
	}
	empty, _ := contentTestActor(t, store, "feed_empty")
	emptyPage, err := store.ListAccountFeed(ctx, empty, "", app.FeedWindow{})
	if err != nil || emptyPage.Items == nil || len(emptyPage.Items) != 0 || emptyPage.NextPosition != nil {
		t.Fatalf("empty=%+v err=%v", emptyPage, err)
	}
	feedExec(t, store, `UPDATE accounts SET disabled_at=statement_timestamp() WHERE id=$1`, outsider)
	if _, err := store.ListAccountFeed(ctx, outsider, "", app.FeedWindow{}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("disabled=%v", err)
	}
	if page := feedRead(t, store, "", "", app.FeedQuery{}); len(page.Items) != 7 {
		t.Fatalf("disabled public content disappeared: %v", feedKeys(page))
	}
}

func TestFeedFollowingAuthorization(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	viewer, session := contentTestActor(t, store, "feed_auth")
	other, _ := contentTestActor(t, store, "feed_other")
	feedPost(t, store, session, "auth", "auth")
	query := app.FeedQuery{View: app.FeedViewFollowing}
	for _, tc := range []struct {
		viewer app.ID
		hash   string
	}{{viewer, ""}, {viewer, "invalid"}, {other, session}, {"", session}} {
		if _, err := store.ListFeed(ctx, tc.viewer, tc.hash, query); !errors.Is(err, app.ErrUnauthenticated) {
			t.Fatalf("viewer=%s session=%q error=%v", tc.viewer, tc.hash, err)
		}
	}
	if page := feedRead(t, store, viewer, session, query); len(page.Items) != 1 {
		t.Fatal("self post absent")
	}
	feedExec(t, store, `UPDATE sessions SET created_at=statement_timestamp()-interval '1 hour',expires_at=statement_timestamp()-interval '1 second' WHERE token_hash=$1`, session)
	if _, err := store.ListFeed(ctx, viewer, session, query); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("expired=%v", err)
	}
	feedExec(t, store, `UPDATE sessions SET expires_at=statement_timestamp()+interval '1 hour' WHERE token_hash=$1`, session)
	feedExec(t, store, `UPDATE accounts SET disabled_at=statement_timestamp() WHERE id=$1`, viewer)
	if _, err := store.ListFeed(ctx, viewer, session, query); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("disabled=%v", err)
	}
	feedExec(t, store, `UPDATE accounts SET disabled_at=NULL WHERE id=$1`, viewer)
	if err := store.RevokeSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListFeed(ctx, viewer, session, query); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("revoked=%v", err)
	}
	// Public reads retain the existing validated-viewer projection boundary.
	feedRead(t, store, "", "", app.FeedQuery{})
}

func TestFeedEqualTimestampKindAndUUIDKeyset(t *testing.T) {
	store := feedTestStore(t)
	actor, session := contentTestActor(t, store, "feed_ties")
	low := app.ID("00000000-0000-0000-0000-000000000001")
	high := app.ID("00000000-0000-0000-0000-000000000002")
	feedExec(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$3,'low','2020-01-01'),($2,$3,'high','2020-01-01')`, low, high, actor)
	feedExec(t, store, `INSERT INTO reposts(id,account_id,post_id,created_at) VALUES($1,$3,$1,'2020-01-01'),($2,$3,$2,'2020-01-01')`, low, high, actor)
	want := []string{"repost:" + string(high), "repost:" + string(low), "post:" + string(high), "post:" + string(low)}
	for _, sort := range []app.FeedSort{app.FeedSortNewest, app.FeedSortRelevant, app.FeedSortReacted} {
		query := app.FeedQuery{Sort: sort, Window: app.FeedWindow{Limit: 1}}
		var got []string
		for range 4 {
			page := feedRead(t, store, actor, session, query)
			if len(page.Items) != 1 {
				t.Fatalf("sort=%s page=%+v", sort, page)
			}
			got = append(got, feedKeys(page)...)
			query.Window.Position, query.Window.InitialCeiling = page.NextPosition, page.Ceiling
		}
		if !reflect.DeepEqual(got, want) || query.Window.Position != nil {
			t.Fatalf("sort=%s got=%v want=%v next=%+v", sort, got, want, query.Window.Position)
		}
	}
}

func TestFeedReactedTotalsAndMovingScores(t *testing.T) {
	store := feedTestStore(t)
	actor, session := contentTestActor(t, store, "feed_rank")
	first := feedPost(t, store, session, "first", "first")
	second := feedPost(t, store, session, "second", "second")
	third := feedPost(t, store, session, "third", "third")
	for i, kind := range []app.ReactionKind{app.ReactionUseful, app.ReactionAgree, app.ReactionBrilliant, app.ReactionSpicy, app.ReactionShip} {
		reactor, _ := contentTestActor(t, store, fmt.Sprintf("feed_reactor_%d", i))
		feedExec(t, store, `INSERT INTO reactions(account_id,post_id,kind,created_at,updated_at) VALUES($1,$2,$3,now(),now())`, reactor, first.ID, kind)
		if i < 2 {
			feedExec(t, store, `INSERT INTO reactions(account_id,post_id,kind,created_at,updated_at) VALUES($1,$2,$3,now(),now())`, reactor, second.ID, kind)
		}
	}
	query := app.FeedQuery{Sort: app.FeedSortReacted, Window: app.FeedWindow{Limit: 2}}
	page := feedRead(t, store, "", "", query)
	if len(page.Items) != 2 || page.Items[0].Post.ID != first.ID || page.Items[1].Post.ID != second.ID || page.NextPosition == nil || page.NextPosition.Score != 2 {
		t.Fatalf("ranking=%+v", page)
	}
	if counts := page.Items[0].Post.Counts; counts.ReactionsTotal != 5 || counts.Reactions != (app.ReactionCounts{Useful: 1, Agree: 1, Brilliant: 1, Spicy: 1, Ship: 1}) {
		t.Fatalf("totals=%+v", counts)
	}
	// The first entry drops below the boundary, while the unvisited third moves
	// above it: a duplicate and an omission are expected for live-score paging.
	feedExec(t, store, `DELETE FROM reactions WHERE post_id=$1`, first.ID)
	feedExec(t, store, `INSERT INTO reactions(account_id,post_id,kind,created_at,updated_at) SELECT id,$1,'agree',now(),now() FROM accounts`, third.ID)
	query.Window.Position, query.Window.InitialCeiling = page.NextPosition, page.Ceiling
	next := feedRead(t, store, "", "", query)
	if len(next.Items) != 1 || next.Items[0].Post.ID != first.ID || next.Items[0].Post.Counts.ReactionsTotal != 0 || next.NextPosition != nil {
		t.Fatalf("moving scores=%+v", next)
	}
	if _, err := store.SetRepost(context.Background(), session, third.ID, true); err != nil {
		t.Fatal(err)
	}
	fresh := feedRead(t, store, actor, session, app.FeedQuery{Sort: app.FeedSortReacted})
	if len(fresh.Items) != 4 || fresh.Items[0].Kind != app.FeedKindRepost || fresh.Items[0].Post.ID != third.ID || fresh.Items[1].Post.ID != third.ID || fresh.Items[0].Post.Counts != fresh.Items[1].Post.Counts {
		t.Fatalf("shared score=%+v", fresh)
	}
}

func TestFeedChangingEventsAndCeiling(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	viewer, session := contentTestActor(t, store, "feed_changes")
	followed, followedSession := contentTestActor(t, store, "feed_old_follow")
	newFollow, newSession := contentTestActor(t, store, "feed_new_follow")
	old := feedPost(t, store, followedSession, "old", "old")
	newlyEligible := feedPost(t, store, newSession, "eligible", "eligible")
	latest := feedPost(t, store, session, "latest", "latest")
	if _, err := store.SetFollow(ctx, session, followed, true); err != nil {
		t.Fatal(err)
	}
	query := app.FeedQuery{View: app.FeedViewFollowing, Window: app.FeedWindow{Limit: 1}}
	page := feedRead(t, store, viewer, session, query)
	if len(page.Items) != 1 || page.Items[0].Post.ID != latest.ID || page.NextPosition == nil {
		t.Fatalf("initial=%+v", page)
	}
	if _, err := store.SetFollow(ctx, session, followed, false); err != nil {
		t.Fatal(err)
	}
	query.Window.Position, query.Window.InitialCeiling = page.NextPosition, page.Ceiling
	if next := feedRead(t, store, viewer, session, query); len(next.Items) != 0 {
		t.Fatalf("unfollowed=%v", feedKeys(next))
	}
	if _, err := store.SetFollow(ctx, session, newFollow, true); err != nil {
		t.Fatal(err)
	}
	if next := feedRead(t, store, viewer, session, query); len(next.Items) != 1 || next.Items[0].Post.ID != newlyEligible.ID {
		t.Fatalf("new follow=%v", feedKeys(next))
	}
	newPost := feedPost(t, store, session, "after", "after ceiling")
	if !newPost.CreatedAt.After(page.Ceiling) {
		t.Fatal("fixture must be after initial ceiling")
	}
	bounded := feedRead(t, store, "", "", app.FeedQuery{Window: app.FeedWindow{InitialCeiling: page.Ceiling}})
	if len(bounded.Items) != 3 {
		t.Fatalf("ceiling=%v", feedKeys(bounded))
	}
	firstRepost, err := store.SetRepost(ctx, session, old.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	repostPage := feedRead(t, store, "", "", app.FeedQuery{})
	if _, err := store.SetRepost(ctx, session, old.ID, false); err != nil {
		t.Fatal(err)
	}
	recreated, err := store.SetRepost(ctx, session, old.ID, true)
	if err != nil || recreated.Viewer.ViewerRepost.ID == firstRepost.Viewer.ViewerRepost.ID || !recreated.Viewer.ViewerRepost.CreatedAt.After(repostPage.Ceiling) {
		t.Fatalf("recreated=%+v error=%v", recreated, err)
	}
	bounded = feedRead(t, store, "", "", app.FeedQuery{Window: app.FeedWindow{InitialCeiling: repostPage.Ceiling}})
	if len(bounded.Items) != 4 {
		t.Fatalf("recreated repost crossed ceiling: %v", feedKeys(bounded))
	}
	if err := store.DeletePost(ctx, followedSession, old.ID); err != nil {
		t.Fatal(err)
	}
	for _, item := range feedRead(t, store, "", "", app.FeedQuery{}).Items {
		if item.Post.ID == old.ID {
			t.Fatal("deleted source retained a feed event")
		}
	}
}

func TestFeedRepeatedPostProjectionAndPrivacy(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	viewer, session := contentTestActor(t, store, "feed_projection")
	other, otherSession := contentTestActor(t, store, "feed_projection_other")
	post := feedPost(t, store, session, "projection", "projection #Go #Rust")
	for _, hash := range []string{session, otherSession} {
		if _, err := store.SetRepost(ctx, hash, post.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		creation, _ := app.NewReplyCreation(post.ID, fmt.Sprintf("reply %d", i))
		if _, err := store.CreateReply(ctx, session, fmt.Sprintf("reply-%d", i), creation); err != nil {
			t.Fatal(err)
		}
	}
	agree := app.ReactionAgree
	if _, err := store.SetReaction(ctx, session, post.ID, &agree); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBookmark(ctx, session, post.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []app.ID{viewer, other, ""} {
		page := feedRead(t, store, id, "", app.FeedQuery{})
		if len(page.Items) != 3 {
			t.Fatalf("events=%v", feedKeys(page))
		}
		for _, item := range page.Items {
			p := item.Post
			if p.ID != post.ID || p.Counts.Replies != 4 || p.Counts.Reposts != 2 || p.Counts.ReactionsTotal != 1 || len(p.Content.Tags) != 2 || len(p.ReplyPreview.Items) != 2 || p.ReplyPreview.NextPosition == nil || p.ReplyPreview.Items[0].ID == p.ReplyPreview.Items[1].ID {
				t.Fatalf("canonical=%+v", p)
			}
			if !reflect.DeepEqual(p, page.Items[0].Post) {
				t.Fatal("events disagree on canonical projection")
			}
			if id == "" {
				if p.Viewer != nil {
					t.Fatal("anonymous viewer state")
				}
			} else if p.Viewer == nil || !p.Viewer.Reposted || p.Viewer.Bookmarked != (id == viewer) || (p.Viewer.Reaction != nil) != (id == viewer) {
				t.Fatalf("private viewer state: id=%s viewer=%+v", id, p.Viewer)
			}
		}
	}
}

func TestFeedLimitsAndValidation(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	actor, session := contentTestActor(t, store, "feed_limits")
	for range 51 {
		feedExec(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'bounded',statement_timestamp())`, app.NewID(), actor)
	}
	for _, limit := range []int{0, 1, 50} {
		page := feedRead(t, store, "", "", app.FeedQuery{Window: app.FeedWindow{Limit: limit}})
		want := limit
		if want == 0 {
			want = 20
		}
		if len(page.Items) != want || page.NextPosition == nil {
			t.Fatalf("limit=%d page length=%d next=%+v", limit, len(page.Items), page.NextPosition)
		}
	}
	for _, query := range []app.FeedQuery{{View: "forged"}, {Sort: "score"}, {Tag: "#go"}, {Window: app.FeedWindow{Limit: 51}}, {Window: app.FeedWindow{Limit: -1}}, {Window: app.FeedWindow{Position: &app.FeedPosition{ID: app.NewID(), Kind: app.FeedKindPost, Timestamp: time.Now()}}}} {
		if _, err := store.ListFeed(ctx, actor, session, query); err == nil {
			t.Fatalf("accepted %+v", query)
		}
	}
	if _, err := store.ListFeed(ctx, "invalid", "", app.FeedQuery{}); !errors.Is(err, app.ErrInvalidID) {
		t.Fatalf("viewer=%v", err)
	}
	if _, err := store.ListAccountFeed(ctx, "invalid", "", app.FeedWindow{}); !errors.Is(err, app.ErrInvalidID) {
		t.Fatalf("account=%v", err)
	}
	if _, err := store.ListAccountFeed(ctx, actor, "", app.FeedWindow{Limit: 51}); err == nil {
		t.Fatal("accepted unbounded account feed")
	}
}

// The queryer seam verifies bounded statement counts and permits a committed
// writer between selection and hydration, without production test hooks.
type feedObservedQueries struct {
	queryer
	count  int
	before func(string)
}

func (q *feedObservedQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.count++
	if q.before != nil {
		q.before(query)
	}
	return q.queryer.QueryContext(ctx, query, args...)
}

func (q *feedObservedQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.count++
	if q.before != nil {
		q.before(query)
	}
	return q.queryer.QueryRowContext(ctx, query, args...)
}

func TestFeedSnapshotAndBoundedQueries(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	actor, session := contentTestActor(t, store, "feed_snapshot")
	post := feedPost(t, store, session, "snapshot", "snapshot")
	for i := range 5 {
		reposter, _ := contentTestActor(t, store, fmt.Sprintf("snapshot_reposter_%d", i))
		feedExec(t, store, `INSERT INTO reposts(id,account_id,post_id,created_at) VALUES($1,$2,$3,statement_timestamp())`, app.NewID(), reposter, post.ID)
	}
	var counts []int
	for _, limit := range []int{1, 50} {
		err := store.readSnapshot(ctx, func(q *Queries) error {
			observed := &feedObservedQueries{queryer: q.queryer}
			q.queryer = observed
			query, _ := (app.FeedQuery{Window: app.FeedWindow{Limit: limit}}).Normalize()
			page, err := q.listFeed(ctx, actor, "", query)
			if err == nil && len(page.Items) != min(limit, 6) {
				t.Fatalf("page=%+v", page)
			}
			counts = append(counts, observed.count)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if counts[0] != counts[1] || counts[1] != 9 {
		t.Fatalf("statement counts=%v, want fixed 9", counts)
	}
	var snapshot app.FeedPage
	wrote := false
	err := store.readSnapshot(ctx, func(q *Queries) error {
		q.queryer = &feedObservedQueries{queryer: q.queryer, before: func(query string) {
			if wrote || !strings.Contains(query, "SELECT p.id,p.body") {
				return
			}
			wrote = true
			// All of these commits are after event selection but before any post
			// or reposter projection. A read-committed implementation would mix them.
			feedExec(t, store, `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, post.ID)
			feedExec(t, store, `INSERT INTO reactions(account_id,post_id,kind,created_at,updated_at) VALUES($1,$2,'ship',now(),now())`, actor, post.ID)
			feedExec(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'late',statement_timestamp())`, app.NewID(), post.ID, actor)
			feedExec(t, store, `UPDATE accounts SET display_name='changed'`)
		}}
		query, _ := (app.FeedQuery{Sort: app.FeedSortReacted, Window: app.FeedWindow{Limit: 5}}).Normalize()
		var err error
		snapshot, err = q.listFeed(ctx, actor, "", query)
		return err
	})
	if err != nil || !wrote || len(snapshot.Items) != 5 || snapshot.NextPosition == nil || snapshot.NextPosition.Score != 0 {
		t.Fatalf("snapshot=%+v wrote=%v err=%v", snapshot, wrote, err)
	}
	for _, item := range snapshot.Items {
		if item.Post.ID != post.ID || item.Post.Counts.ReactionsTotal != 0 || item.Post.Counts.Replies != 0 || len(item.Post.ReplyPreview.Items) != 0 || item.Post.Author.DisplayName == "changed" || (item.Reposter != nil && item.Reposter.DisplayName == "changed") {
			t.Fatalf("mixed snapshot=%+v", item)
		}
	}
	if fresh := feedRead(t, store, "", "", app.FeedQuery{}); len(fresh.Items) != 0 {
		t.Fatalf("fresh snapshot failed: %v", feedKeys(fresh))
	}
}

func TestFeedFollowingSnapshotReauthorization(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	viewer, session := contentTestActor(t, store, "following_snapshot")
	followed, followedSession := contentTestActor(t, store, "following_snapshot_target")
	feedPost(t, store, followedSession, "first", "first")
	feedPost(t, store, followedSession, "second", "second")
	if _, err := store.SetFollow(ctx, session, followed, true); err != nil {
		t.Fatal(err)
	}
	query, _ := (app.FeedQuery{View: app.FeedViewFollowing, Window: app.FeedWindow{Limit: 1}}).Normalize()
	var page app.FeedPage
	err := store.readSnapshot(ctx, func(q *Queries) error {
		account, err := q.SessionAccount(ctx, session)
		if err != nil {
			return err
		}
		// Revocation and unfollow commit after snapshot authorization. This read
		// consistently sees the original state; the next request must reauthorize.
		if err := store.RevokeSession(ctx, session); err != nil {
			return err
		}
		feedExec(t, store, `DELETE FROM follows WHERE follower_id=$1`, viewer)
		page, err = q.listFeed(ctx, account.ID, "", query)
		return err
	})
	if err != nil || len(page.Items) != 1 || page.NextPosition == nil {
		t.Fatalf("authorized snapshot=%+v err=%v", page, err)
	}
	query.Window.Position, query.Window.InitialCeiling = page.NextPosition, page.Ceiling
	if _, err := store.ListFeed(ctx, viewer, session, query); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("continuation after revocation=%v", err)
	}
}
