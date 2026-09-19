package postgres

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestBatchHydrationRepeatedIDs(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	viewer, session := contentTestActor(t, store, "repeated_hydration")
	creation, _ := app.NewPostCreation("repeated #Go", nil, nil)
	post, err := store.CreatePost(ctx, session, "repeated", creation)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		reply, _ := app.NewReplyCreation(post.ID, fmt.Sprintf("reply %d", i))
		if _, err := store.CreateReply(ctx, session, fmt.Sprintf("reply-%d", i), reply); err != nil {
			t.Fatal(err)
		}
	}
	// Preserve requested output order/multiplicity, but never multiply the
	// lateral preview rows when a canonical ID is requested more than once.
	err = store.readSnapshot(ctx, func(q *Queries) error {
		posts, err := q.hydratePosts(ctx, []app.ID{post.ID, post.ID, app.NewID(), post.ID}, viewer)
		if err != nil {
			return err
		}
		if len(posts) != 3 {
			t.Fatalf("hydrated count=%d", len(posts))
		}
		for _, p := range posts {
			if p.ID != post.ID || p.Counts.Replies != 3 || len(p.Content.Tags) != 1 || len(p.ReplyPreview.Items) != 2 || p.ReplyPreview.Items[0].ID == p.ReplyPreview.Items[1].ID || p.ReplyPreview.NextPosition == nil || !reflect.DeepEqual(p, posts[0]) {
				t.Fatalf("hydrated=%+v", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
