package postgres

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func claimJob(t *testing.T, store *Store, id app.ID) app.GenerationJob {
	t.Helper()
	job, err := store.GenerationJobByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func cloneClaimJob(t *testing.T, store *Store, source app.ID, changes map[string]any) app.GenerationJob {
	t.Helper()
	id, err := generationCloneJob(store, source, changes)
	if err != nil {
		t.Fatal(err)
	}
	return claimJob(t, store, id)
}

func sameClaimJob(a, b app.GenerationJob) bool {
	// pgx may decode timestamps in the local zone; compare instants rather than
	// time.Location implementation details while still checking every field.
	for _, job := range []*app.GenerationJob{&a, &b} {
		job.CreatedAt, job.AvailableAt, job.ExpiresAt = job.CreatedAt.UTC(), job.AvailableAt.UTC(), job.ExpiresAt.UTC()
		if job.LeaseExpiresAt != nil {
			expiry := job.LeaseExpiresAt.UTC()
			job.LeaseExpiresAt = &expiry
		}
		if job.FinishedAt != nil {
			finished := job.FinishedAt.UTC()
			job.FinishedAt = &finished
		}
	}
	return reflect.DeepEqual(a, b)
}

func TestGenerationClaimSelection(t *testing.T) {
	store, _, pending := generationSetup(t)
	ctx := context.Background()
	now := pending.AvailableAt.Add(10 * time.Minute)
	retry := cloneClaimJob(t, store, pending.ID, map[string]any{
		"status": "retry_wait", "lease_version": 4, "reason_code": "transient", "available_at": now,
	})
	reclaim := cloneClaimJob(t, store, pending.ID, map[string]any{
		"status": "running", "lease_version": 7, "lease_expires_at": now,
	})
	want := map[app.ID]app.GenerationJob{pending.ID: pending, retry.ID: retry, reclaim.ID: reclaim}
	var untouched []app.GenerationJob
	for _, changes := range []map[string]any{
		{"available_at": now.Add(time.Microsecond)},
		{"status": "retry_wait", "lease_version": 2, "reason_code": "transient", "available_at": now.Add(time.Microsecond)},
		{"expires_at": now},
		{"status": "running", "lease_version": 2, "lease_expires_at": now.Add(time.Microsecond)},
		{"status": "running", "lease_version": 2, "lease_expires_at": now.Add(-time.Second), "expires_at": now},
		{"status": "failed", "lease_version": 2, "reason_code": "original", "finished_at": now},
		{"status": "skipped", "reason_code": "original", "finished_at": now},
		{"status": "cancelled", "reason_code": "original", "finished_at": now},
	} {
		untouched = append(untouched, cloneClaimJob(t, store, pending.ID, changes))
	}
	_, post, _, _ := generationSocial(t, store, pending)
	published := cloneClaimJob(t, store, pending.ID, nil)
	attempt := generationAttempt(t, store, published, 1)
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at WHERE id=$1`, attempt)
	generationSQL(t, store, `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at,result_post_id=$2,published_attempt_id=$3 WHERE id=$1`, published.ID, post, attempt)
	untouched = append(untouched, claimJob(t, store, published.ID))
	for range 3 {
		got, err := store.claimGeneration(ctx, &now)
		if err != nil || got == nil {
			t.Fatalf("claim: %+v %v", got, err)
		}
		before, ok := want[got.ID]
		if !ok {
			t.Fatalf("unexpected or duplicate claim: %+v", got)
		}
		expiry := now.Add(90 * time.Second)
		before.Status, before.LeaseVersion, before.LeaseExpiresAt, before.ReasonCode = app.JobRunning, before.LeaseVersion+1, &expiry, ""
		if !sameClaimJob(*got, before) || !sameClaimJob(claimJob(t, store, got.ID), before) {
			t.Fatalf("claim changed identity or lease: got=%+v want=%+v", got, before)
		}
		delete(want, got.ID)
	}
	if got, err := store.claimGeneration(ctx, &now); err != nil || got != nil {
		t.Fatalf("claimed unavailable work: %+v %v", got, err)
	}
	for _, before := range untouched {
		if got := claimJob(t, store, before.ID); !sameClaimJob(got, before) {
			t.Fatalf("unavailable work changed: %+v", got)
		}
	}
}

func TestGenerationClaimParallelAndSkipLocked(t *testing.T) {
	store, _, pending := generationSetup(t)
	ctx := context.Background()
	now := pending.AvailableAt
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, pending.ID); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	for range workers {
		cloneClaimJob(t, store, pending.ID, nil)
	}
	claimed := make(chan app.ID, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			job, err := store.claimGeneration(ctx, &now)
			if err != nil || job == nil {
				t.Errorf("parallel claim: %+v %v", job, err)
				return
			}
			claimed <- job.ID
		}()
	}
	group.Wait()
	close(claimed)
	seen := map[app.ID]bool{}
	for id := range claimed {
		if seen[id] || id == pending.ID {
			t.Fatalf("duplicate or locked claim: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("claimed %d distinct jobs", len(seen))
	}
	if got, err := store.claimGeneration(ctx, &now); err != nil || got != nil {
		t.Fatalf("locked-only queue: %+v %v", got, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := store.claimGeneration(ctx, &now); err != nil || got == nil || got.ID != pending.ID {
		t.Fatalf("released job: %+v %v", got, err)
	}
}

func TestGenerationClaimRenewalAndFences(t *testing.T) {
	store, _, pending := generationSetup(t)
	ctx := context.Background()
	now := pending.AvailableAt
	job, err := store.claimGeneration(ctx, &now)
	if err != nil || job == nil {
		t.Fatalf("claim: %+v %v", job, err)
	}
	now = now.Add(20 * time.Second)
	expiry, err := store.renewGeneration(ctx, job.ID, job.LeaseVersion, &now)
	if err != nil || !expiry.Equal(now.Add(90*time.Second)) {
		t.Fatalf("renew: %v %v", expiry, err)
	}
	job.LeaseExpiresAt = &expiry
	if got := claimJob(t, store, job.ID); !sameClaimJob(got, *job) {
		t.Fatalf("renew changed fields: %+v", got)
	}
	// An identical/backwards clock must not shorten the committed lease.
	earlier := now.Add(-time.Second)
	if got, err := store.renewGeneration(ctx, job.ID, 1, &earlier); err != nil || !got.Equal(expiry) {
		t.Fatalf("shortened lease: %v %v", got, err)
	}
	for _, version := range []int64{0, 2} {
		if _, err := store.renewGeneration(ctx, job.ID, version, &now); !errors.Is(err, app.ErrConflict) {
			t.Fatalf("wrong fence %d: %v", version, err)
		}
	}
	now = expiry
	if got, err := store.renewGeneration(ctx, job.ID, 1, &now); !errors.Is(err, app.ErrConflict) || !got.IsZero() {
		t.Fatalf("revived expired lease: %v %v", got, err)
	}
	reclaimed, err := store.claimGeneration(ctx, &now)
	if err != nil || reclaimed == nil || reclaimed.ID != job.ID || reclaimed.LeaseVersion != 2 {
		t.Fatalf("exclusive reclaim: %+v %v", reclaimed, err)
	}
	if _, err := store.renewGeneration(ctx, job.ID, 1, &now); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("old owner renewed: %v", err)
	}
	// Reclaim just before immutable job expiry; its lease extends beyond that
	// deadline, but it still confers no authority at or after job expiry.
	now = job.ExpiresAt.Add(-time.Second)
	reclaimed, err = store.claimGeneration(ctx, &now)
	if err != nil || reclaimed == nil || reclaimed.LeaseVersion != 3 {
		t.Fatalf("late reclaim: %+v %v", reclaimed, err)
	}
	now = job.ExpiresAt
	if _, err := store.renewGeneration(ctx, job.ID, 3, &now); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("revived expired job: %v", err)
	}
	if got, err := store.claimGeneration(ctx, &now); err != nil || got != nil {
		t.Fatalf("reclaimed expired job: %+v %v", got, err)
	}
}

func TestGenerationClaimFenceOverflow(t *testing.T) {
	store, _, pending := generationSetup(t)
	now := pending.AvailableAt.Add(time.Minute)
	generationSQL(t, store, `UPDATE generation_jobs SET status='running',lease_version=$2,lease_expires_at=$3 WHERE id=$1`, pending.ID, int64(math.MaxInt64), now)
	before := claimJob(t, store, pending.ID)
	if got, err := store.claimGeneration(context.Background(), &now); !errors.Is(err, app.ErrUnavailable) || got != nil {
		t.Fatalf("overflow claim: %+v %v", got, err)
	}
	if got := claimJob(t, store, pending.ID); !sameClaimJob(got, before) {
		t.Fatalf("overflow mutated job: %+v", got)
	}
}

func TestGenerationClaimRemovedRepost(t *testing.T) {
	for _, status := range []string{"pending", "retry_wait", "running"} {
		t.Run(status, func(t *testing.T) {
			store, _, pending := generationSetup(t)
			ctx := context.Background()
			now := pending.AvailableAt.Add(time.Minute)
			actor, post, _, repost := generationSocial(t, store, pending)
			changes := map[string]any{"trigger_kind": "repost", "trigger_actor_id": actor, "cooldown_key": "repost", "source_post_id": post, "source_repost_id": repost, "output_kind": "reply", "status": status}
			if status != "pending" {
				changes["lease_version"] = 4
			}
			if status == "retry_wait" {
				changes["reason_code"] = "transient"
			}
			if status == "running" {
				changes["lease_expires_at"] = now
			}
			job := cloneClaimJob(t, store, pending.ID, changes)
			generationSQL(t, store, `DELETE FROM reposts WHERE id=$1`, repost)
			before := claimJob(t, store, job.ID)
			generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='test',finished_at=available_at WHERE id=$1`, pending.ID)
			if got, err := store.claimGeneration(ctx, &now); err != nil || got != nil {
				t.Fatalf("removed repost claim: %+v %v", got, err)
			}
			got := claimJob(t, store, job.ID)
			if got.Status != app.JobCancelled || got.ReasonCode != "source_removed" || got.LeaseVersion != before.LeaseVersion || got.LeaseExpiresAt != nil {
				t.Fatalf("removed repost executed: %+v", got)
			}
			if err := app.ValidateGenerationSourceInvalidation(before, got, before.LeaseVersion, *got.FinishedAt); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGenerationClaimJobOnlyLocks(t *testing.T) {
	store, _, pending := generationSetup(t)
	ctx := context.Background()
	now := pending.AvailableAt
	actor, post, _, repost := generationSocial(t, store, pending)
	job := cloneClaimJob(t, store, pending.ID, map[string]any{"trigger_kind": "repost", "trigger_actor_id": actor, "cooldown_key": "repost", "source_post_id": post, "source_repost_id": repost, "output_kind": "reply"})
	generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='test',finished_at=available_at WHERE id=$1`, pending.ID)
	if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: job.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Hold the earlier locks used by deletion/policy changes. Neither claim nor
	// renewal may wait on them after acquiring the job, even for social work.
	for _, statement := range []struct {
		query string
		id    app.ID
	}{
		{`SELECT id FROM posts WHERE id=$1 FOR UPDATE`, post},
		{`SELECT id FROM reposts WHERE id=$1 FOR UPDATE`, repost},
		{`SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, job.AgentID},
		{`SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, job.AgentID},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, statement.id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.claimGeneration(ctx, &now)
	if err != nil || got == nil || got.ID != job.ID {
		t.Fatalf("claim acquired earlier lock: %+v %v", got, err)
	}
	now = now.Add(time.Second)
	if _, err := store.renewGeneration(ctx, job.ID, 1, &now); err != nil {
		t.Fatalf("renewal acquired earlier lock: %v", err)
	}
	// Deletion can subsequently reach the job/FK without a lock-order cycle.
	if _, err := tx.ExecContext(ctx, `DELETE FROM reposts WHERE id=$1`, repost); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.renewGeneration(ctx, job.ID, 1, &now); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("renewed removed repost: %v", err)
	}
}

func TestGenerationClaimFreshClockAfterWait(t *testing.T) {
	for _, operation := range []string{"claim", "renew", "expired_renew", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			store, _, pending := generationSetup(t)
			ctx := context.Background()
			var now time.Time
			if err := store.db.QueryRow(`SELECT clock_timestamp()`).Scan(&now); err != nil {
				t.Fatal(err)
			}
			job := cloneClaimJob(t, store, pending.ID, map[string]any{"available_at": now, "expires_at": now.Add(time.Hour)})
			if operation != "claim" {
				if got, err := store.ClaimGeneration(ctx); err != nil || got == nil || got.ID != job.ID {
					t.Fatalf("live claim: %+v %v", got, err)
				}
			}
			before := claimJob(t, store, job.ID)
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if operation == "claim" {
				_, err = tx.ExecContext(ctx, `LOCK TABLE generation_jobs IN ACCESS EXCLUSIVE MODE`)
			} else {
				_, err = tx.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, job.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "claim" {
					_, err := store.ClaimGeneration(callCtx)
					done <- err
				} else {
					_, err := store.RenewGeneration(callCtx, job.ID, 1)
					done <- err
				}
			}()
			waitForDatabaseBlock(t, store, pid)
			if operation == "cancel" {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled renewal: %v", err)
				}
			}
			if operation == "expired_renew" {
				if _, err := tx.ExecContext(ctx, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID); err != nil {
					t.Fatal(err)
				}
			}
			var released time.Time
			if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&released); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if operation == "cancel" {
				if got := claimJob(t, store, job.ID); !sameClaimJob(got, before) {
					t.Fatalf("cancelled renewal mutated job: %+v", got)
				}
				return
			}
			err = <-done
			if operation == "expired_renew" {
				if !errors.Is(err, app.ErrConflict) {
					t.Fatalf("expired while waiting: %v", err)
				}
				return
			}
			got := claimJob(t, store, job.ID)
			if err != nil || got.LeaseExpiresAt == nil || got.LeaseExpiresAt.Before(released.Add(90*time.Second)) {
				t.Fatalf("pre-lock clock: release=%v job=%+v err=%v", released, got, err)
			}
		})
	}
}

func TestGenerationClaimMutationRollback(t *testing.T) {
	for _, operation := range []string{"claim", "renew", "removed_repost"} {
		for _, failure := range []string{"zero_rows", "database_error"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				store, _, pending := generationSetup(t)
				ctx := context.Background()
				now := pending.AvailableAt
				if operation == "renew" {
					if _, err := store.claimGeneration(ctx, &now); err != nil {
						t.Fatal(err)
					}
					now = now.Add(time.Second)
				}
				if operation == "removed_repost" {
					actor, post, _, _ := generationSocial(t, store, pending)
					pending = cloneClaimJob(t, store, pending.ID, map[string]any{"trigger_kind": "repost", "trigger_actor_id": actor, "cooldown_key": "repost", "source_post_id": post, "output_kind": "reply"})
					generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='test',finished_at=available_at WHERE trigger_kind='scheduled'`)
				}
				before := claimJob(t, store, pending.ID)
				timing, body := "BEFORE", "RETURN NULL;"
				if failure == "database_error" {
					timing, body = "AFTER", "RAISE EXCEPTION 'secret database detail';"
				}
				generationSQL(t, store, `CREATE FUNCTION reject_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
				generationSQL(t, store, `CREATE TRIGGER reject_claim `+timing+` UPDATE ON generation_jobs FOR EACH ROW EXECUTE FUNCTION reject_claim()`)
				if operation == "renew" {
					if expiry, err := store.renewGeneration(ctx, pending.ID, 1, &now); !errors.Is(err, app.ErrUnavailable) || !expiry.IsZero() {
						t.Fatalf("failed renewal: %v %v", expiry, err)
					}
				} else if got, err := store.claimGeneration(ctx, &now); !errors.Is(err, app.ErrUnavailable) || got != nil {
					t.Fatalf("failed claim: %+v %v", got, err)
				}
				if got := claimJob(t, store, pending.ID); !sameClaimJob(got, before) {
					t.Fatalf("failed mutation committed: %+v", got)
				}
			})
		}
	}
}
