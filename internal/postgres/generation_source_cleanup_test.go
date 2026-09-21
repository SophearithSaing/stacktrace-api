package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationSourceDeletion(t *testing.T) {
	for _, kind := range []app.GenerationTrigger{app.TriggerHumanPost, app.TriggerReply, app.TriggerRepost} {
		t.Run(string(kind), func(t *testing.T) {
			store := schedulingStore(t)
			agent := socialAgent(t, store, "cleanup_agent", nil)
			_, session := contentTestActor(t, store, "cleanup_human")
			parent, _ := socialPost(t, store, agent.AgentID, "source")
			var action app.ID
			ctx := context.Background()
			switch kind {
			case app.TriggerHumanPost:
				creation, _ := app.NewPostCreation("@cleanup_agent", nil, nil)
				post, err := store.CreatePost(ctx, session, "source", creation)
				if err != nil {
					t.Fatal(err)
				}
				action = post.ID
			case app.TriggerReply:
				creation, _ := app.NewReplyCreation(parent, "reply")
				reply, err := store.CreateReply(ctx, session, "source", creation)
				if err != nil {
					t.Fatal(err)
				}
				action = reply.Reply.ID
			case app.TriggerRepost:
				if _, err := store.SetRepost(ctx, session, parent, true); err != nil {
					t.Fatal(err)
				}
			}
			before := socialJobs(t, store)[0]
			terminal, err := generationCloneJob(store, before.ID, map[string]any{"status": "cancelled", "reason_code": "original_reason", "finished_at": before.CreatedAt})
			if err != nil {
				t.Fatal(err)
			}
			generationSQL(t, store, `UPDATE generation_jobs SET status='running',lease_version=4,lease_expires_at=available_at+interval '1 microsecond' WHERE id=$1`, before.ID)
			switch kind {
			case app.TriggerHumanPost:
				err = store.DeletePost(ctx, session, action)
			case app.TriggerReply:
				err = store.DeleteReply(ctx, session, action)
			case app.TriggerRepost:
				_, err = store.SetRepost(ctx, session, parent, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := store.GenerationJobByID(ctx, before.ID)
			if err != nil || after.Status != app.JobCancelled || after.ReasonCode != "source_removed" || after.LeaseVersion != 4 || after.LeaseExpiresAt != nil || !after.ExpiresAt.Equal(before.ExpiresAt) || after.CooldownKey != before.CooldownKey || *after.TriggerActorID != *before.TriggerActorID {
				t.Fatalf("source cleanup: %+v %v", after, err)
			}
			if kind == app.TriggerRepost && after.SourceRepostID != nil {
				t.Fatal("repost FK survived removal")
			}
			retained, err := store.GenerationJobByID(ctx, terminal)
			if err != nil || retained.ReasonCode != "original_reason" {
				t.Fatal("terminal changed", err)
			}
		})
	}
}

func TestGenerationSourceBoundedRollback(t *testing.T) {
	store := schedulingStore(t)
	socialAgent(t, store, "cleanup_agent", nil)
	_, session := contentTestActor(t, store, "cleanup_human")
	creation, _ := app.NewPostCreation("@cleanup_agent", nil, nil)
	post, err := store.CreatePost(context.Background(), session, "source", creation)
	if err != nil {
		t.Fatal(err)
	}
	job := socialJobs(t, store)[0]
	for range generationSourceCleanupBatch + 2 {
		if _, err := generationCloneJob(store, job.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	abort := errors.New("rollback invalidation")
	err = store.Transaction(context.Background(), func(q *Queries) error {
		if err := q.lockVisiblePost(context.Background(), post.ID); err != nil {
			return err
		}
		if _, err := q.queryer.ExecContext(context.Background(), `UPDATE posts SET deleted_at=clock_timestamp() WHERE id=$1`, post.ID); err != nil {
			return err
		}
		count, err := q.cancelRemovedGenerationSourceJobs(context.Background(), post.ID)
		if err != nil {
			return err
		}
		if count != generationSourceCleanupBatch {
			t.Fatalf("batch=%d", count)
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if _, err := store.PostByID(context.Background(), post.ID, ""); err != nil {
		t.Fatal("source deletion escaped rollback", err)
	}
	for _, job := range socialJobs(t, store) {
		if job.Status != app.JobPending {
			t.Fatal("cancellation escaped rollback")
		}
	}
	if err := store.DeletePost(context.Background(), session, post.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, pending := 0, 0
	for _, job := range socialJobs(t, store) {
		if job.Status == app.JobCancelled {
			cancelled++
		}
		if job.Status == app.JobPending {
			pending++
		}
	}
	if cancelled != generationSourceCleanupBatch || pending != 3 {
		t.Fatalf("unbounded cleanup: cancelled=%d pending=%d", cancelled, pending)
	}
	if _, err := store.PostByID(context.Background(), post.ID, ""); !errors.Is(err, app.ErrDeleted) {
		t.Fatal("remainder source stayed visible", err)
	}
	now := job.ExpiresAt.Add(time.Second)
	count, err := store.expireGenerationJobs(context.Background(), &now)
	if err != nil || count != 3 {
		t.Fatalf("remainder did not expire: %d %v", count, err)
	}
}

func TestGenerationRepostRecreationRetainsAllowance(t *testing.T) {
	for _, limit := range []string{"human", "cooldown"} {
		t.Run(limit, func(t *testing.T) {
			store := schedulingStore(t)
			agent := socialAgent(t, store, "repost_agent", func(p *app.GenerationPolicy) {
				if limit == "human" {
					p.HumanTriggerCapPerWindow = 1
				} else {
					p.CooldownSeconds = 3600
				}
			})
			actor, session := contentTestActor(t, store, "repost_human")
			post, _ := socialPost(t, store, agent.AgentID, "source")
			if _, err := store.SetRepost(context.Background(), session, post, true); err != nil {
				t.Fatal(err)
			}
			first := socialJobs(t, store)[0]
			if _, err := store.SetRepost(context.Background(), session, post, false); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetRepost(context.Background(), session, post, true); err != nil {
				t.Fatal(err)
			}
			var action app.ID
			if err := store.db.QueryRow(`SELECT id FROM reposts WHERE account_id=$1 AND post_id=$2`, actor, post).Scan(&action); err != nil {
				t.Fatal(err)
			}
			now := first.CreatedAt.Add(2 * time.Second)
			count, err := socialAdmit(store, session, app.TriggerRepost, action, &now, schedulingDraw)
			if err != nil || count != 0 || len(socialJobs(t, store)) != 1 {
				t.Fatalf("recreated bypassed %s: %d %v", limit, count, err)
			}
		})
	}
}
