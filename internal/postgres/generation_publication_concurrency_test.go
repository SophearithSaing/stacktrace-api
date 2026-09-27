package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationPublicationWaitRevalidation(t *testing.T) {
	for _, stage := range []string{"source", "account", "settings", "policy", "lease", "attempt"} {
		t.Run(stage, func(t *testing.T) {
			store, job, attempt, output := publicationFixture(t, app.OutputQuote, "Waited #Provisional output")
			ctx := context.Background()
			blocker, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			query, id, want := `UPDATE posts SET deleted_at=clock_timestamp() WHERE id=$1`, *job.SourcePostID, app.ErrDeleted
			switch stage {
			case "account":
				query, id, want = `UPDATE accounts SET disabled_at=clock_timestamp() WHERE id=$1`, job.AgentID, app.ErrForbidden
			case "settings":
				query, id, want = `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, job.AgentID, app.ErrForbidden
			case "policy":
				query, id, want = `UPDATE agent_settings SET policy=jsonb_set(policy,'{reply_cap_per_day}','0') WHERE agent_id=$1`, job.AgentID, app.ErrForbidden
			case "lease":
				query, id, want = `UPDATE generation_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID, app.ErrConflict
			case "attempt":
				query, id = `SELECT id FROM generation_attempts WHERE id=$1 FOR UPDATE`, attempt.ID
			}
			if _, err := blocker.ExecContext(ctx, query, id); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			before := publicationCounts(t, store)
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := store.PublishGeneration(callCtx, job.ID, 1, attempt.ID, output)
				done <- err
			}()
			waitForDatabaseBlock(t, store, pid)
			if stage == "source" || stage == "account" || stage == "settings" || stage == "policy" {
				if _, err := blocker.Exec(`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE NOWAIT`, job.ID); err != nil {
					t.Fatalf("job locked before preparation: %v", err)
				}
			}
			if stage == "source" {
				if _, err := blocker.Exec(`SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE NOWAIT`, job.AgentID); err != nil {
					t.Fatalf("settings before source: %v", err)
				}
			}
			if stage == "attempt" {
				assertEligibilityLock(t, store, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, job.ID, false)
				cancel()
				want = context.Canceled
			} else if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, want) || publicationCounts(t, store) != before {
				t.Fatalf("wait denial: %v want=%v", err, want)
			}
		})
	}
}

func TestGenerationPublicationTagWaitPrecedesAuthorityLocks(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputPost, "Sorted #Conflicting #Tags")
	ctx := context.Background()
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`INSERT INTO tags(id,slug,display_name) VALUES($1,'conflicting','Conflicting')`, app.NewID()); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := store.PublishGeneration(callCtx, job.ID, 1, attempt.ID, output); done <- err }()
	waitForDatabaseBlock(t, store, pid)
	for _, lock := range []struct {
		query string
		id    app.ID
	}{
		{`SELECT id FROM accounts WHERE id=$1 FOR UPDATE NOWAIT`, job.AgentID},
		{`SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE NOWAIT`, job.AgentID},
		{`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE NOWAIT`, job.ID},
	} {
		if _, err := blocker.Exec(lock.query, lock.id); err != nil {
			t.Fatalf("authority lock before tag conflict: %v", err)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGenerationPublicationRootWaitPrecedesParentLock(t *testing.T) {
	store, root, rootAttempt, rootOutput := publicationFixture(t, app.OutputPost, "@root_lock_child a real continuation")
	childAgent := socialAgent(t, store, "root_lock_child", func(p *app.GenerationPolicy) { p.DailyTokenBudget = 500000 })
	ctx := context.Background()
	if _, err := store.PublishGeneration(ctx, root.ID, 1, rootAttempt.ID, rootOutput); err != nil {
		t.Fatal(err)
	}
	var job app.GenerationJob
	for _, candidate := range socialJobs(t, store) {
		if candidate.RootJobID == root.ID && candidate.AgentID == childAgent.AgentID {
			job = candidate
		}
	}
	if job.ID == "" {
		t.Fatal("missing real continuation")
	}
	generationSQL(t, store, `UPDATE generation_jobs SET status='running',lease_version=1,lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, job.ID)
	job = claimJob(t, store, job.ID)
	input, _ := readGenerationContext(t, store, job)
	attempt, output := publicationSettle(t, store, job, input, "A continuation waiting for its root")
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, root.ID); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := store.PublishGeneration(callCtx, job.ID, 1, attempt.ID, output); done <- err }()
	waitForDatabaseBlock(t, store, pid)
	if _, err := blocker.Exec(`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE NOWAIT`, job.ID); err != nil {
		t.Fatalf("parent before root: %v", err)
	}
	if _, err := blocker.Exec(`SELECT id FROM generation_attempts WHERE id=$1 FOR UPDATE NOWAIT`, attempt.ID); err != nil {
		t.Fatalf("attempt before root: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGenerationPublicationFinalClockRollback(t *testing.T) {
	for _, boundary := range []string{"lease", "expiry", "source_age", "backwards"} {
		t.Run(boundary, func(t *testing.T) {
			store, job, attempt, output := publicationFixture(t, app.OutputQuote, "A final clock #Rollback @clock_child")
			socialAgent(t, store, "clock_child", nil)
			now := time.Now().UTC()
			final := *job.LeaseExpiresAt
			want := app.ErrConflict
			switch boundary {
			case "expiry":
				final = job.ExpiresAt
			case "source_age":
				generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{source_max_age_seconds}','180') WHERE agent_id=$1`, job.AgentID)
				generationSQL(t, store, `UPDATE posts SET created_at=$2 WHERE id=$1`, job.SourcePostID, now.Add(-170*time.Second))
				final, want = now.Add(10*time.Second), app.ErrForbidden
			case "backwards":
				final = now.Add(-time.Microsecond)
			}
			before := publicationCounts(t, store)
			err := store.Transaction(context.Background(), func(q *Queries) error {
				// Initial authority, child admission, and final authority respectively.
				q.queryer = &spendClockQueryer{queryer: q.queryer, clocks: []time.Time{now, now, final}}
				_, err := q.publishGeneration(context.Background(), job.ID, 1, attempt.ID, output)
				return err
			})
			if !errors.Is(err, want) || publicationCounts(t, store) != before || !sameClaimJob(job, claimJob(t, store, job.ID)) {
				t.Fatalf("final boundary: %v want=%v", err, want)
			}
		})
	}
}

type publicationDiscoveryQueryer struct {
	queryer
	onAccounts         func()
	discoveries        int
	accounts, settings []app.ID
}

func (q *publicationDiscoveryQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "WITH fleet AS MATERIALIZED") {
		q.discoveries++
	}
	return q.queryer.QueryContext(ctx, query, args...)
}

func (q *publicationDiscoveryQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "SELECT handle FROM accounts") {
		if q.onAccounts != nil {
			q.onAccounts()
			q.onAccounts = nil
		}
		q.accounts = append(q.accounts, args[0].(app.ID))
	}
	if strings.Contains(query, "WHERE s.agent_id=$1 FOR UPDATE OF s") {
		q.settings = append(q.settings, args[0].(app.ID))
	}
	return q.queryer.QueryRowContext(ctx, query, args...)
}

func TestGenerationPublicationStableCandidatesAndPinnedPersona(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputPost, "@stable_child @late_child #Candidate discovery")
	stable := socialAgent(t, store, "stable_child", nil)
	late := socialAgent(t, store, "late_child", nil)
	generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, late.AgentID)
	persona, err := store.PersonaByVersion(context.Background(), job.AgentID, 1)
	if err != nil {
		t.Fatal(err)
	}
	persona.Version, persona.Instructions = 2, "A newly selected persona does not repin in-flight work."
	if err := store.CreatePersona(context.Background(), persona); err != nil {
		t.Fatal(err)
	}
	var recorder *publicationDiscoveryQueryer
	err = store.Transaction(context.Background(), func(q *Queries) error {
		recorder = &publicationDiscoveryQueryer{queryer: q.queryer, onAccounts: func() {
			// Both changes happen AFTER discovery and before any agent lock.
			generationSQL(t, store, `UPDATE agent_settings SET enabled=true WHERE agent_id=$1`, late.AgentID)
			generationSQL(t, store, `UPDATE agent_settings SET persona_version=2 WHERE agent_id=$1`, job.AgentID)
		}}
		q.queryer = recorder
		_, err := q.publishGeneration(context.Background(), job.ID, 1, attempt.ID, output)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []app.ID{job.AgentID, stable.AgentID}
	slices.Sort(want)
	if recorder.discoveries != 1 || !slices.Equal(recorder.accounts, want) || !slices.Equal(recorder.settings, want) {
		t.Fatalf("lock sets: discoveries=%d accounts=%v settings=%v", recorder.discoveries, recorder.accounts, recorder.settings)
	}
	children := 0
	for _, child := range socialJobs(t, store) {
		if child.RootJobID == job.ID && child.ID != job.ID {
			children++
			if child.AgentID != stable.AgentID {
				t.Fatal("rediscovered newly eligible candidate")
			}
		}
	}
	if children != 1 || claimJob(t, store, job.ID).PersonaVersion != 1 {
		t.Fatal("wrong continuation/pinned persona")
	}
}

func TestGenerationPublicationConcurrentOverlappingPublishers(t *testing.T) {
	for _, tags := range []bool{false, true} {
		name := "accounts"
		if tags {
			name = "tag_conflicts"
		}
		t.Run(name, func(t *testing.T) {
			suffix := ""
			if tags {
				suffix = " #Shared #Conflicts"
			}
			store, first, attempt, output := publicationFixture(t, app.OutputPost, "First publisher @other_publisher"+suffix)
			other := socialAgent(t, store, "other_publisher", nil)
			settings, _ := store.AgentSettingsByID(context.Background(), first.AgentID)
			encoded, _ := json.Marshal(settings.Policy)
			generationSQL(t, store, `UPDATE agent_settings SET policy=$2 WHERE agent_id=$1`, other.AgentID, encoded)
			second := contextJob(t, store, first, map[string]any{"agent_id": other.AgentID, "trigger_key": first.TriggerKey, "created_at": first.CreatedAt, "available_at": first.AvailableAt})
			input, _ := readGenerationContext(t, store, second)
			secondAttempt, secondOutput := publicationSettle(t, store, second, input, "Second publisher @generation_agent"+suffix)
			jobs, attempts, outputs := []app.GenerationJob{first, second}, []app.GenerationAttempt{attempt, secondAttempt}, []app.GenerationResult{output, secondOutput}
			var workers sync.WaitGroup
			start := make(chan struct{})
			for i := range jobs {
				workers.Go(func() {
					<-start
					if _, err := store.PublishGeneration(context.Background(), jobs[i].ID, 1, attempts[i].ID, outputs[i]); err != nil {
						t.Errorf("overlapping publisher: %v", err)
					}
				})
			}
			close(start)
			workers.Wait()
			for _, job := range jobs {
				published := claimJob(t, store, job.ID)
				if published.ResultPostID == nil {
					t.Fatal("missing publication")
				}
				post, err := store.PostByID(context.Background(), *published.ResultPostID, "")
				if err != nil || !post.IsGenerated || tags && len(post.Content.Tags) != 2 {
					t.Fatalf("overlap result: %+v %v", post, err)
				}
			}
		})
	}
}

func TestGenerationPublicationConcurrentExactReplay(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputPost, "@replay_child #Replay race")
	socialAgent(t, store, "replay_child", nil)
	results := make(chan app.GenerationJob, 2)
	var workers sync.WaitGroup
	start := make(chan struct{})
	for range 2 {
		workers.Go(func() {
			<-start
			got, err := store.PublishGeneration(context.Background(), job.ID, 1, attempt.ID, output)
			if err != nil {
				t.Error(err)
			}
			results <- got
		})
	}
	close(start)
	workers.Wait()
	first, second := <-results, <-results
	if first.ID == "" || !sameClaimJob(first, second) {
		t.Fatalf("different replay results: %+v %+v", first, second)
	}
	count := 0
	for _, child := range socialJobs(t, store) {
		if child.RootJobID == job.ID {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("duplicate continuations: %d", count)
	}
}
