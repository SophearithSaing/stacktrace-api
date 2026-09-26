package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationPublicationSourceCleanupRemainder(t *testing.T) {
	for _, removal := range []string{"post", "reply", "repost", "pause"} {
		t.Run(removal, func(t *testing.T) {
			store, original, _ := spendFixture(t, 500000)
			ctx := context.Background()
			var reply, repost app.ID
			var session string
			if err := store.db.QueryRow(`SELECT token_hash FROM sessions WHERE account_id=$1`, original.TriggerActorID).Scan(&session); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow(`SELECT id FROM replies WHERE post_id=$1`, original.SourcePostID).Scan(&reply); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow(`SELECT id FROM reposts WHERE post_id=$1`, original.SourcePostID).Scan(&repost); err != nil {
				t.Fatal(err)
			}
			old := original.CreatedAt.Add(-5 * time.Minute)
			generationSQL(t, store, `UPDATE posts SET created_at=$2 WHERE id=$1`, original.SourcePostID, old)
			generationSQL(t, store, `UPDATE replies SET created_at=$2 WHERE id=$1`, reply, old)
			generationSQL(t, store, `UPDATE reposts SET created_at=$2 WHERE id=$1`, repost, old)
			var job app.GenerationJob
			for i := range generationSourceCleanupBatch + 3 {
				at := original.CreatedAt.Add(-time.Duration(i+1) * 2 * time.Second)
				changes := map[string]any{"created_at": at, "available_at": at, "cooldown_key": string(app.NewID())}
				if removal == "reply" {
					changes["trigger_kind"], changes["source_reply_id"] = app.TriggerReply, reply
				}
				if removal == "repost" {
					changes["trigger_kind"], changes["source_repost_id"] = app.TriggerRepost, repost
				}
				candidate := contextJob(t, store, original, changes)
				if candidate.ID > job.ID {
					job = candidate // cleanup orders by ID; this one is beyond its batch
				}
			}
			input, _ := readGenerationContext(t, store, job)
			attempt, output := publicationSettle(t, store, job, input, "A response awaiting source validation")
			var err error
			switch removal {
			case "post":
				err = store.DeletePost(ctx, session, *job.SourcePostID)
			case "reply":
				err = store.DeleteReply(ctx, session, reply)
			case "repost":
				_, err = store.SetRepost(ctx, session, *job.SourcePostID, false)
			case "pause":
				generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, job.AgentID)
			}
			if err != nil {
				t.Fatal(err)
			}
			remainder := claimJob(t, store, job.ID)
			if remainder.Status != app.JobRunning {
				t.Fatalf("fixture was not a cleanup remainder: %+v", remainder)
			}
			before := publicationCounts(t, store)
			got, err := store.PublishGeneration(ctx, job.ID, 1, attempt.ID, output)
			want := app.ErrDeleted
			if removal == "pause" {
				want = app.ErrForbidden
			}
			if !errors.Is(err, want) || got.ID != "" || publicationCounts(t, store) != before || !sameClaimJob(remainder, claimJob(t, store, job.ID)) {
				t.Fatalf("cleanup remainder published: %+v %v", got, err)
			}
		})
	}
}
