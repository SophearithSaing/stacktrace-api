package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestExecutionSourceRelationships(t *testing.T) {
	store, _, root := generationSetup(t)
	actor, post, reply, repost := generationSocial(t, store, root)
	other, _ := contentTestActor(t, store, "other_source_actor")
	otherPost, otherReply, otherRepost, quote := app.NewID(), app.NewID(), app.NewID(), app.NewID()
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'other post',now())`, otherPost, other)
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,quoted_post_id,created_at) VALUES($1,$2,'quote',$3,now())`, quote, actor, post)
	generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'other reply',now())`, otherReply, otherPost, actor)
	generationSQL(t, store, `INSERT INTO reposts(id,post_id,account_id,created_at) VALUES($1,$2,$3,now())`, otherRepost, otherPost, actor)
	missing := app.NewID()
	for _, test := range []struct {
		name          string
		kind          app.GenerationTrigger
		post, actor   app.ID
		reply, repost *app.ID
		valid         bool
	}{
		{"post", app.TriggerHumanPost, post, actor, nil, nil, true},
		{"quote", app.TriggerQuote, quote, actor, nil, nil, true},
		{"reply", app.TriggerReply, post, actor, &reply, nil, true},
		{"repost", app.TriggerRepost, post, actor, nil, &repost, true},
		{"wrong post actor", app.TriggerHumanPost, post, other, nil, nil, false},
		{"wrong reply actor", app.TriggerReply, post, other, &reply, nil, false},
		{"wrong repost actor", app.TriggerRepost, post, other, nil, &repost, false},
		{"wrong reply parent", app.TriggerReply, post, actor, &otherReply, nil, false},
		{"wrong repost target", app.TriggerRepost, post, actor, nil, &otherRepost, false},
		{"quote as original", app.TriggerHumanPost, quote, actor, nil, nil, false},
		{"original as quote", app.TriggerQuote, post, actor, nil, nil, false},
		{"removed repost", app.TriggerRepost, post, actor, nil, nil, false},
		{"missing post", app.TriggerHumanPost, missing, actor, nil, nil, false},
		{"missing reply", app.TriggerReply, post, actor, &missing, nil, false},
		{"missing repost", app.TriggerRepost, post, actor, nil, &missing, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := root
			job.TriggerKind, job.TriggerActorID, job.SourcePostID, job.SourceReplyID, job.SourceRepostID, job.OutputKind, job.CooldownKey = test.kind, &test.actor, &test.post, test.reply, test.repost, app.OutputReply, "source"
			for _, lock := range []bool{false, true} {
				err := store.Transaction(context.Background(), func(q *Queries) error {
					valid, err := q.executionSourceValid(context.Background(), job, lock)
					if err != nil || valid != test.valid {
						t.Fatalf("valid=%v want=%v lock=%v err=%v", valid, test.valid, lock, err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			// Valid persisted foreign keys need not establish relational correctness.
			if test.post != missing && test.reply != &missing && test.repost != &missing {
				changes := contextSocialChanges(test.actor, test.post, test.kind)
				changes["source_reply_id"], changes["source_repost_id"] = test.reply, test.repost
				persisted := contextJob(t, store, root, changes)
				_, err := store.GenerationContext(context.Background(), persisted.ID, 1)
				if test.valid && err != nil || !test.valid && !errors.Is(err, app.ErrDeleted) {
					t.Fatalf("context source validity: %v", err)
				}
			}
		})
	}
	if valid, err := store.executionSourceValid(context.Background(), root, false); err != nil || !valid {
		t.Fatalf("original: %v %v", valid, err)
	}
	if _, err := store.executionSourceValid(context.Background(), root, true); !errors.Is(err, errGenerationTransaction) {
		t.Fatalf("unbound locking: %v", err)
	}
}

func TestExecutionSourceDeletionAndDisabledAuthors(t *testing.T) {
	for _, kind := range []app.GenerationTrigger{app.TriggerHumanPost, app.TriggerReply, app.TriggerRepost} {
		for _, invalid := range []string{"post deleted", "post author disabled", "actor disabled", "reply deleted", "repost recreated"} {
			if invalid == "reply deleted" && kind != app.TriggerReply || invalid == "repost recreated" && kind != app.TriggerRepost {
				continue
			}
			t.Run(string(kind)+"/"+invalid, func(t *testing.T) {
				store, _, root := generationSetup(t)
				author, post, reply, repost := generationSocial(t, store, root)
				actor := author
				if kind != app.TriggerHumanPost {
					actor, _ = contentTestActor(t, store, "distinct_trigger_actor")
					generationSQL(t, store, `UPDATE replies SET author_id=$2 WHERE id=$1`, reply, actor)
					generationSQL(t, store, `UPDATE reposts SET account_id=$2 WHERE id=$1`, repost, actor)
				}
				changes := contextSocialChanges(actor, post, kind)
				if kind == app.TriggerReply {
					changes["source_reply_id"] = reply
				}
				if kind == app.TriggerRepost {
					changes["source_repost_id"] = repost
				}
				job := contextJob(t, store, root, changes)
				readGenerationContext(t, store, job)
				switch invalid {
				case "post deleted":
					generationSQL(t, store, `UPDATE posts SET deleted_at=now() WHERE id=$1`, post)
				case "post author disabled":
					generationSQL(t, store, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, author)
				case "actor disabled":
					generationSQL(t, store, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, actor)
				case "reply deleted":
					generationSQL(t, store, `UPDATE replies SET deleted_at=now() WHERE id=$1`, reply)
				case "repost recreated":
					generationSQL(t, store, `DELETE FROM reposts WHERE id=$1`, repost)
					generationSQL(t, store, `INSERT INTO reposts(id,post_id,account_id,created_at) VALUES($1,$2,$3,now())`, app.NewID(), post, actor)
				}
				if valid, err := store.executionSourceValid(context.Background(), job, false); err != nil || valid {
					t.Fatalf("old reference replaced: %v %v", valid, err)
				}
				if got, err := store.GenerationContext(context.Background(), job.ID, 1); !errors.Is(err, app.ErrDeleted) || got.PublicJSON() != "" {
					t.Fatalf("unavailable source: %v", err)
				}
			})
		}
	}
}

func TestExecutionSourceLockOrderAndCancellation(t *testing.T) {
	for _, kind := range []app.GenerationTrigger{app.TriggerReply, app.TriggerRepost} {
		t.Run(string(kind), func(t *testing.T) {
			store, _, root := generationSetup(t)
			actor, post, reply, repost := generationSocial(t, store, root)
			changes := contextSocialChanges(actor, post, kind)
			table, child := "replies", reply
			if kind == app.TriggerReply {
				changes["source_reply_id"] = reply
			} else {
				changes["source_repost_id"] = repost
				table, child = "reposts", repost
			}
			job := contextJob(t, store, root, changes)
			ctx := context.Background()
			if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: job.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: job.CreatedAt}); err != nil {
				t.Fatal(err)
			}
			blocker, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM posts WHERE id=$1 FOR UPDATE`, post); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- store.Transaction(callCtx, func(q *Queries) error { _, err := q.executionSourceValid(callCtx, job, true); return err })
			}()
			waitForDatabaseBlock(t, store, pid)
			// While the helper waits on the post, the child and job must still be
			// lockable: it cannot have taken either one before the canonical post.
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM `+table+` WHERE id=$1 FOR UPDATE NOWAIT`, child); err != nil {
				t.Fatalf("child locked before post: %v", err)
			}
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE NOWAIT`, job.ID); err != nil {
				t.Fatalf("job locked before post: %v", err)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("blocked cancellation: %v", err)
			}
			if err := blocker.Rollback(); err != nil {
				t.Fatal(err)
			}
			// Conversely, a wait on the child must retain the already-acquired
			// post lock, and cancellation must release that lock as well.
			blocker, err = store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM `+table+` WHERE id=$1 FOR UPDATE`, child); err != nil {
				t.Fatal(err)
			}
			if err := blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			callCtx, cancel = context.WithCancel(ctx)
			defer cancel()
			go func() {
				done <- store.Transaction(callCtx, func(q *Queries) error { _, err := q.executionSourceValid(callCtx, job, true); return err })
			}()
			waitForDatabaseBlock(t, store, pid)
			probe, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = probe.ExecContext(ctx, `SELECT id FROM posts WHERE id=$1 FOR UPDATE NOWAIT`, post)
			probe.Rollback()
			if err == nil {
				t.Fatal("waiting on child without retaining post lock")
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("child wait cancellation: %v", err)
			}
			// database/sql may return cancellation while pgx is still closing
			// the discarded connection. Allow bounded server-side rollback cleanup.
			releasedCtx, releaseCancel := context.WithTimeout(ctx, time.Second)
			defer releaseCancel()
			if _, err := blocker.ExecContext(releasedCtx, `SELECT id FROM posts WHERE id=$1 FOR UPDATE`, post); err != nil {
				t.Fatalf("cancel retained post lock: %v", err)
			}
			if err := blocker.Rollback(); err != nil {
				t.Fatal(err)
			}
			// Source locking must not acquire job/account/settings locks at all.
			blocker, err = store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM accounts WHERE id IN ($1,$2) FOR UPDATE`, actor, job.AgentID); err != nil {
				t.Fatal(err)
			}
			if _, err := blocker.ExecContext(ctx, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, job.AgentID); err != nil {
				t.Fatal(err)
			}
			rollback := errors.New("rollback source locks")
			err = store.Transaction(ctx, func(q *Queries) error {
				valid, err := q.executionSourceValid(ctx, job, true)
				if err != nil || !valid {
					t.Fatalf("source acquired unrelated lock: %v %v", valid, err)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM posts WHERE id=$1 FOR UPDATE NOWAIT`, post); err != nil {
				t.Fatalf("post lock not rolled back: %v", err)
			}
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM `+table+` WHERE id=$1 FOR UPDATE NOWAIT`, child); err != nil {
				t.Fatalf("child lock not rolled back: %v", err)
			}
		})
	}
}
