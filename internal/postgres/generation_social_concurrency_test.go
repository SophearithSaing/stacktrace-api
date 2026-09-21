package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationSocialConcurrentQuotas(t *testing.T) {
	for _, scope := range []string{"human_across_agents", "agent_across_humans"} {
		t.Run(scope, func(t *testing.T) {
			store := schedulingStore(t)
			socialAgent(t, store, "first_agent", func(p *app.GenerationPolicy) {
				if scope == "human_across_agents" {
					p.HumanTriggerCapPerWindow = 1
				} else {
					p.ReplyCapPerDay = 1
				}
			})
			socialAgent(t, store, "second_agent", func(p *app.GenerationPolicy) { p.HumanTriggerCapPerWindow = 1 })
			_, firstSession := contentTestActor(t, store, "first_human")
			_, secondSession := contentTestActor(t, store, "second_human")
			if scope == "human_across_agents" {
				secondSession = firstSession
			}
			bodies := []string{"@first_agent", "@second_agent"}
			if scope == "agent_across_humans" {
				bodies[1] = "@first_agent"
			}
			var workers sync.WaitGroup
			start := make(chan struct{})
			for i, session := range []string{firstSession, secondSession} {
				workers.Go(func() {
					<-start
					creation, _ := app.NewPostCreation(bodies[i], nil, nil)
					if _, err := store.CreatePost(context.Background(), session, "race-"+strings.TrimPrefix(bodies[i], "@"), creation); err != nil {
						t.Errorf("concurrent content: %v", err)
					}
				})
			}
			close(start)
			workers.Wait()
			if len(socialJobs(t, store)) != 1 {
				t.Fatalf("quota oversubscribed: %d", len(socialJobs(t, store)))
			}
			var posts int
			if err := store.db.QueryRow(`SELECT count(*) FROM posts`).Scan(&posts); err != nil || posts != 2 {
				t.Fatalf("quota denial lost content: %d %v", posts, err)
			}
		})
	}
}

func TestGenerationSocialDeleteWriteRace(t *testing.T) {
	for _, kind := range []string{"reply", "repost"} {
		t.Run(kind, func(t *testing.T) {
			store := schedulingStore(t)
			socialAgent(t, store, "race_agent", nil)
			owner, ownerSession := contentTestActor(t, store, "source_owner")
			_, session := contentTestActor(t, store, "source_actor")
			post, _ := socialPost(t, store, owner, "@race_agent")
			start := make(chan struct{})
			done := make(chan error, 2)
			go func() { <-start; done <- store.DeletePost(context.Background(), ownerSession, post) }()
			go func() {
				<-start
				var err error
				if kind == "reply" {
					creation, _ := app.NewReplyCreation(post, "@race_agent")
					_, err = store.CreateReply(context.Background(), session, "racing-reply", creation)
				} else {
					_, err = store.SetRepost(context.Background(), session, post, true)
				}
				done <- err
			}()
			close(start)
			for range 2 {
				if err := <-done; err != nil && !errors.Is(err, app.ErrDeleted) {
					t.Fatal(err)
				}
			}
			for _, job := range socialJobs(t, store) {
				if job.Status != app.JobCancelled || job.ReasonCode != "source_removed" {
					t.Fatalf("deleted source stayed active: %+v", job)
				}
			}
			if _, err := store.PostByID(context.Background(), post, ""); !errors.Is(err, app.ErrDeleted) {
				t.Fatal("source deletion lost", err)
			}
		})
	}
}

func TestGenerationSocialSessionLockOrder(t *testing.T) {
	for _, operation := range []string{"rotate", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			store := schedulingStore(t)
			socialAgent(t, store, "auth_agent", nil)
			actor, session := contentTestActor(t, store, "auth_human")
			source, _ := socialPost(t, store, actor, "source")
			ctx := context.Background()
			blocker, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM posts WHERE id=$1 FOR UPDATE`, source); err != nil {
				t.Fatal(err)
			}
			var blockerPID int
			if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			written := make(chan error, 1)
			go func() {
				creation, _ := app.NewPostCreation("@auth_agent", &source, nil)
				_, err := store.CreatePost(ctx, session, "locked-write", creation)
				written <- err
			}()
			waitForDatabaseBlock(t, store, blockerPID)
			var writerPID int
			if err := store.db.QueryRow(`SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) LIMIT 1`, blockerPID).Scan(&writerPID); err != nil {
				t.Fatal(err)
			}
			authDone := make(chan error, 1)
			go func() {
				if operation == "revoke" {
					authDone <- store.RevokeSession(ctx, session)
				} else {
					now := time.Now()
					authDone <- store.RotateSession(ctx, app.Session{TokenHash: strings.Repeat("e", 64), AccountID: actor, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, session)
				}
			}()
			waitForDatabaseBlock(t, store, writerPID)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-written; err != nil {
				t.Fatal("writer deadlock/failure", err)
			}
			if err := <-authDone; err != nil {
				t.Fatal("auth deadlock/failure", err)
			}
			if len(socialJobs(t, store)) != 1 {
				t.Fatal("auth race lost atomic job")
			}
			creation, _ := app.NewPostCreation("@auth_agent", nil, nil)
			if _, err := store.CreatePost(ctx, session, "revoked-write", creation); !errors.Is(err, app.ErrUnauthenticated) {
				t.Fatalf("revoked session accepted: %v", err)
			}
		})
	}
}

func TestGenerationSocialExpiredSessionAfterWait(t *testing.T) {
	store := schedulingStore(t)
	actor, session := contentTestActor(t, store, "expiry_human")
	ctx := context.Background()
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, actor); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		creation, _ := app.NewPostCreation("content", nil, nil)
		_, err := store.CreatePost(ctx, session, "expired", creation)
		done <- err
	}()
	waitForDatabaseBlock(t, store, pid)
	generationSQL(t, store, `UPDATE sessions SET expires_at=created_at+interval '1 microsecond' WHERE token_hash=$1`, session)
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("expired waited session accepted: %v", err)
	}
}
