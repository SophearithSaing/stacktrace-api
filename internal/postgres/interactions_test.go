package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func interactionTestPost(t *testing.T, store *Store, session, key string) app.Post {
	t.Helper()
	creation, err := app.NewPostCreation("interaction target", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	post, err := store.CreatePost(context.Background(), session, key, creation)
	if err != nil {
		t.Fatal(err)
	}
	return post
}

func TestReactionDesiredStateAndCanonicalCounts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	actor, session := contentTestActor(t, store, "reactor")
	_, otherSession := contentTestActor(t, store, "other_reactor")
	post := interactionTestPost(t, store, session, "reaction-post")

	useful := app.ReactionUseful
	first, err := store.SetReaction(ctx, session, post.ID, &useful)
	if err != nil || first.Viewer == nil || first.Viewer.Reaction == nil || *first.Viewer.Reaction != useful || first.Counts.ReactionsTotal != 1 || first.Counts.Reactions.Useful != 1 {
		t.Fatalf("first reaction=%+v error=%v", first, err)
	}
	var createdAt, updatedAt time.Time
	if err = store.db.QueryRowContext(ctx, `SELECT created_at,updated_at FROM reactions WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetReaction(ctx, session, post.ID, &useful); err != nil {
		t.Fatal(err)
	}
	var sameCreatedAt, sameUpdatedAt time.Time
	if err = store.db.QueryRowContext(ctx, `SELECT created_at,updated_at FROM reactions WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&sameCreatedAt, &sameUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if !sameCreatedAt.Equal(createdAt) || !sameUpdatedAt.Equal(updatedAt) {
		t.Fatalf("no-op reaction changed timestamps: (%v,%v) -> (%v,%v)", createdAt, updatedAt, sameCreatedAt, sameUpdatedAt)
	}

	start := make(chan struct{})
	kinds := []app.ReactionKind{app.ReactionAgree, app.ReactionBrilliant}
	errs := make(chan error, len(kinds))
	for i := range kinds {
		kind := kinds[i]
		go func() {
			<-start
			_, err := store.SetReaction(ctx, session, post.ID, &kind)
			errs <- err
		}()
	}
	close(start)
	for range kinds {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var storedKind app.ReactionKind
	var rowCount int
	if err = store.db.QueryRowContext(ctx, `SELECT count(*),min(kind) FROM reactions WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&rowCount, &storedKind); err != nil {
		t.Fatal(err)
	}
	projected, err := store.PostByID(ctx, post.ID, actor)
	if err != nil || rowCount != 1 || projected.Viewer == nil || projected.Viewer.Reaction == nil || *projected.Viewer.Reaction != storedKind || projected.Counts.ReactionsTotal != 1 {
		t.Fatalf("rows=%d kind=%q projected=%+v error=%v", rowCount, storedKind, projected, err)
	}

	spicy := app.ReactionSpicy
	if _, err = store.SetReaction(ctx, otherSession, post.ID, &spicy); err != nil {
		t.Fatal(err)
	}
	replyCreation, _ := app.NewReplyCreation(post.ID, "one reply")
	if _, err = store.CreateReply(ctx, otherSession, "count-reply", replyCreation); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetRepost(ctx, otherSession, post.ID, true); err != nil {
		t.Fatal(err)
	}
	projected, err = store.PostByID(ctx, post.ID, actor)
	if err != nil || projected.Counts.Replies != 1 || projected.Counts.Reposts != 1 || projected.Counts.ReactionsTotal != 2 || projected.Counts.Reactions.Spicy != 1 || projected.IsSpicy {
		t.Fatalf("independent counts or spicy flag changed: %+v error=%v", projected, err)
	}

	if _, err = store.SetReaction(ctx, session, post.ID, nil); err != nil {
		t.Fatal(err)
	}
	removed, err := store.SetReaction(ctx, session, post.ID, nil)
	if err != nil || removed.Counts.ReactionsTotal != 1 || removed.Viewer == nil || removed.Viewer.Reaction != nil {
		t.Fatalf("repeated removal=%+v error=%v", removed, err)
	}
	invalid := app.ReactionKind("fire")
	if _, err = store.SetReaction(ctx, session, post.ID, &invalid); !errors.Is(err, app.ErrInvalidReactionKind) {
		t.Fatalf("invalid reaction=%v", err)
	}
}

func TestRepostAndBookmarkStableDesiredState(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	actor, session := contentTestActor(t, store, "relationship_actor")
	post := interactionTestPost(t, store, session, "relationship-post")

	for _, relationship := range []string{"repost", "bookmark"} {
		t.Run(relationship, func(t *testing.T) {
			start := make(chan struct{})
			errs := make(chan error, 2)
			for range 2 {
				go func() {
					<-start
					var err error
					if relationship == "repost" {
						_, err = store.SetRepost(ctx, session, post.ID, true)
					} else {
						_, err = store.SetBookmark(ctx, session, post.ID, true)
					}
					errs <- err
				}()
			}
			close(start)
			for range 2 {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}

			var count int
			var firstID app.ID
			var firstCreated time.Time
			if relationship == "repost" {
				if err := store.db.QueryRowContext(ctx, `SELECT id,created_at FROM reposts WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&firstID, &firstCreated); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM reposts WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				projected, err := store.SetRepost(ctx, session, post.ID, true)
				if err != nil || projected.Viewer == nil || projected.Viewer.ViewerRepost == nil || projected.Viewer.ViewerRepost.ID != firstID || !projected.Viewer.ViewerRepost.CreatedAt.Equal(firstCreated) {
					t.Fatalf("stable repost=%+v error=%v", projected, err)
				}
			} else {
				if err := store.db.QueryRowContext(ctx, `SELECT created_at FROM bookmarks WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&firstCreated); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM bookmarks WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				projected, err := store.SetBookmark(ctx, session, post.ID, true)
				if err != nil || projected.Viewer == nil || !projected.Viewer.Bookmarked {
					t.Fatalf("stable bookmark=%+v error=%v", projected, err)
				}
				var sameCreated time.Time
				if err = store.db.QueryRowContext(ctx, `SELECT created_at FROM bookmarks WHERE account_id=$1 AND post_id=$2`, actor, post.ID).Scan(&sameCreated); err != nil || !sameCreated.Equal(firstCreated) {
					t.Fatalf("bookmark timestamp changed: %v -> %v error=%v", firstCreated, sameCreated, err)
				}
			}
			if count != 1 {
				t.Fatalf("duplicate %s rows=%d", relationship, count)
			}

			if relationship == "repost" {
				if _, err := store.SetRepost(ctx, session, post.ID, false); err != nil {
					t.Fatal(err)
				}
				removed, err := store.SetRepost(ctx, session, post.ID, false)
				if err != nil || removed.Viewer == nil || removed.Viewer.Reposted || removed.Counts.Reposts != 0 {
					t.Fatalf("repost removal=%+v error=%v", removed, err)
				}
				readded, err := store.SetRepost(ctx, session, post.ID, true)
				if err != nil || readded.Viewer == nil || readded.Viewer.ViewerRepost == nil || readded.Viewer.ViewerRepost.ID == firstID {
					t.Fatalf("re-added repost=%+v error=%v", readded, err)
				}
			} else {
				if _, err := store.SetBookmark(ctx, session, post.ID, false); err != nil {
					t.Fatal(err)
				}
				removed, err := store.SetBookmark(ctx, session, post.ID, false)
				if err != nil || removed.Viewer == nil || removed.Viewer.Bookmarked {
					t.Fatalf("bookmark removal=%+v error=%v", removed, err)
				}
			}
		})
	}
}

func TestInteractionAuthorizationTargetsAndDeletionRaces(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "interaction_security")
	missing := app.NewID()
	useful := app.ReactionUseful
	for name, mutate := range map[string]func(app.ID) error{
		"reaction": func(id app.ID) error { _, err := store.SetReaction(ctx, session, id, nil); return err },
		"repost":   func(id app.ID) error { _, err := store.SetRepost(ctx, session, id, false); return err },
		"bookmark": func(id app.ID) error { _, err := store.SetBookmark(ctx, session, id, false); return err },
	} {
		t.Run(name+" unknown", func(t *testing.T) {
			if err := mutate(missing); !errors.Is(err, app.ErrNotFound) {
				t.Fatalf("unknown delete=%v", err)
			}
		})
	}

	deleted := interactionTestPost(t, store, session, "deleted-target")
	if err := store.DeletePost(ctx, session, deleted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetReaction(ctx, session, deleted.ID, nil); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("deleted reaction remove=%v", err)
	}
	if _, err := store.SetRepost(ctx, session, deleted.ID, false); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("deleted repost remove=%v", err)
	}
	if _, err := store.SetBookmark(ctx, session, deleted.ID, false); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("deleted bookmark remove=%v", err)
	}

	securityPost := interactionTestPost(t, store, session, "security-target")
	if _, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=$1`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetReaction(ctx, session, securityPost.ID, &useful); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("revoked reaction=%v", err)
	}
	if _, err := store.SetRepost(ctx, session, securityPost.ID, true); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("revoked repost=%v", err)
	}
	if _, err := store.SetBookmark(ctx, session, securityPost.ID, true); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("revoked bookmark=%v", err)
	}
	disabledActor, disabledSession := contentTestActor(t, store, "disabled_interactor")
	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, disabledActor); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(string) error{
		"reaction": func(hash string) error { _, err := store.SetReaction(ctx, hash, securityPost.ID, &useful); return err },
		"repost":   func(hash string) error { _, err := store.SetRepost(ctx, hash, securityPost.ID, true); return err },
		"bookmark": func(hash string) error { _, err := store.SetBookmark(ctx, hash, securityPost.ID, true); return err },
	} {
		for state, hash := range map[string]string{"unknown": "unknown-session", "disabled": disabledSession} {
			if err := mutate(hash); !errors.Is(err, app.ErrUnauthenticated) {
				t.Fatalf("%s %s session=%v", state, name, err)
			}
		}
	}
	var relationships int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM reactions)+(SELECT count(*) FROM reposts)+(SELECT count(*) FROM bookmarks)`).Scan(&relationships); err != nil || relationships != 0 {
		t.Fatalf("unauthorized relationships=%d error=%v", relationships, err)
	}

	_, raceSession := contentTestActor(t, store, "interaction_racer")
	for _, relationship := range []string{"reaction", "repost", "bookmark"} {
		t.Run(relationship+" deletion race", func(t *testing.T) {
			post := interactionTestPost(t, store, raceSession, "race-"+relationship)
			blocker, pid := lockAndDeletePost(t, store, post.ID)
			result := make(chan error, 1)
			go func() {
				var err error
				switch relationship {
				case "reaction":
					_, err = store.SetReaction(ctx, raceSession, post.ID, &useful)
				case "repost":
					_, err = store.SetRepost(ctx, raceSession, post.ID, true)
				case "bookmark":
					_, err = store.SetBookmark(ctx, raceSession, post.ID, true)
				}
				result <- err
			}()
			waitForDatabaseBlock(t, store, pid)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, app.ErrDeleted) {
				t.Fatalf("blocked mutation=%v", err)
			}
			var count int
			query := `SELECT count(*) FROM reactions WHERE post_id=$1`
			if relationship == "repost" {
				query = `SELECT count(*) FROM reposts WHERE post_id=$1`
			} else if relationship == "bookmark" {
				query = `SELECT count(*) FROM bookmarks WHERE post_id=$1`
			}
			if err := store.db.QueryRowContext(ctx, query, post.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("race rows=%d error=%v", count, err)
			}
		})
	}
}

func TestConcurrentRelationshipDeletesRemainAbsent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, session := contentTestActor(t, store, "delete_concurrently")
	post := interactionTestPost(t, store, session, "delete-concurrently-post")
	useful := app.ReactionUseful
	if _, err := store.SetReaction(ctx, session, post.ID, &useful); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRepost(ctx, session, post.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBookmark(ctx, session, post.ID, true); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			_, err := store.SetReaction(ctx, session, post.ID, nil)
			if err != nil {
				t.Error(err)
			}
		})
		workers.Go(func() {
			_, err := store.SetRepost(ctx, session, post.ID, false)
			if err != nil {
				t.Error(err)
			}
		})
		workers.Go(func() {
			_, err := store.SetBookmark(ctx, session, post.ID, false)
			if err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	projected, err := store.PostByID(ctx, post.ID, "")
	if err != nil || projected.Counts.ReactionsTotal != 0 || projected.Counts.Reposts != 0 {
		t.Fatalf("post-delete counts=%+v error=%v", projected.Counts, err)
	}
}
