package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationCleanupExpiryAndRetention(t *testing.T) {
	store, _, pending := generationSetup(t)
	ctx := context.Background()
	now := pending.ExpiresAt
	jobs := []app.GenerationJob{pending}
	for _, changes := range []map[string]any{
		{"status": "retry_wait", "lease_version": 7, "reason_code": "retry"},
		{"status": "running", "lease_version": 7, "lease_expires_at": now.Add(-time.Minute)},
		{"status": "running", "lease_version": 9, "lease_expires_at": now.Add(time.Hour)},
	} {
		id, err := generationCloneJob(store, pending.ID, changes)
		if err != nil {
			t.Fatal(err)
		}
		job, err := store.GenerationJobByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	future, err := generationCloneJob(store, pending.ID, map[string]any{"expires_at": now.Add(time.Microsecond)})
	if err != nil {
		t.Fatal(err)
	}
	var terminals []app.ID
	for _, status := range []string{"cancelled", "skipped", "failed"} {
		id, err := generationCloneJob(store, pending.ID, map[string]any{"status": status, "finished_at": pending.CreatedAt, "reason_code": "original_reason"})
		if err != nil {
			t.Fatal(err)
		}
		terminals = append(terminals, id)
	}
	// Successful publication provenance is also untouched by stale cleanup.
	_, post, _, _ := generationSocial(t, store, pending)
	published, err := generationCloneJob(store, pending.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	publishedJob, err := store.GenerationJobByID(ctx, published)
	if err != nil {
		t.Fatal(err)
	}
	attempt := generationAttempt(t, store, publishedJob, 1)
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at WHERE id=$1`, attempt)
	generationSQL(t, store, `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at,result_post_id=$2,published_attempt_id=$3 WHERE id=$1`, published, post, attempt)
	terminals = append(terminals, published)
	before := make(map[app.ID]string)
	for _, id := range terminals {
		var record string
		if err := store.db.QueryRow(`SELECT to_jsonb(j)::text FROM generation_jobs j WHERE id=$1`, id).Scan(&record); err != nil {
			t.Fatal(err)
		}
		before[id] = record
	}
	count, err := store.expireGenerationJobs(ctx, &now)
	if err != nil || count != len(jobs) {
		t.Fatalf("expired=%d err=%v", count, err)
	}
	for _, old := range jobs {
		job, err := store.GenerationJobByID(ctx, old.ID)
		if err != nil || job.Status != app.JobSkipped || job.ReasonCode != "stale_trigger" || job.LeaseExpiresAt != nil || job.LeaseVersion != old.LeaseVersion || !job.ExpiresAt.Equal(old.ExpiresAt) || !job.FinishedAt.Equal(now) {
			t.Fatalf("stale cleanup: %+v %v", job, err)
		}
		if err := app.ValidateGenerationJobTransition(old, job, old.LeaseVersion, now, nil); err != nil {
			t.Fatal("identity/fence changed", err)
		}
	}
	for _, id := range terminals {
		var after string
		if err := store.db.QueryRow(`SELECT to_jsonb(j)::text FROM generation_jobs j WHERE id=$1`, id).Scan(&after); err != nil || after != before[id] {
			t.Fatalf("terminal changed: %s %v", id, err)
		}
	}
	job, err := store.GenerationJobByID(ctx, future)
	if err != nil || job.Status != app.JobPending {
		t.Fatalf("early cleanup: %+v %v", job, err)
	}
	count, err = store.expireGenerationJobs(ctx, &now)
	if err != nil || count != 0 {
		t.Fatalf("repeat cleanup: %d %v", count, err)
	}
	now = now.Add(time.Microsecond)
	count, err = store.expireGenerationJobs(ctx, &now)
	if err != nil || count != 1 {
		t.Fatalf("exclusive expiry: %d %v", count, err)
	}
}

func TestGenerationCleanupBoundedAndLocked(t *testing.T) {
	store, _, pending := generationSetup(t)
	ctx := context.Background()
	now := pending.ExpiresAt
	for range generationCleanupBatch + 3 {
		if _, err := generationCloneJob(store, pending.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, pending.ID); err != nil {
		t.Fatal(err)
	}
	count, err := store.expireGenerationJobs(ctx, &now)
	if err != nil || count != generationCleanupBatch {
		t.Fatalf("bounded cleanup: %d %v", count, err)
	}
	count, err = store.expireGenerationJobs(ctx, &now)
	if err != nil || count != 3 {
		t.Fatalf("remaining unlocked cleanup: %d %v", count, err)
	}
	job, err := store.GenerationJobByID(ctx, pending.ID)
	if err != nil || job.Status != app.JobPending {
		t.Fatalf("locked job changed: %+v %v", job, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	count, err = store.expireGenerationJobs(ctx, &now)
	if err != nil || count != 1 {
		t.Fatalf("unlocked cleanup: %d %v", count, err)
	}
}

func TestGenerationCleanupFenceAndRollback(t *testing.T) {
	store, _, pending := generationSetup(t)
	ctx := context.Background()
	now := pending.ExpiresAt
	abort := errors.New("abort cleanup")
	err := store.Transaction(ctx, func(q *Queries) error {
		if _, err := q.queryer.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, pending.ID); err != nil {
			return err
		}
		if err := q.skipExpiredGenerationJob(ctx, pending, now); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	job, err := store.GenerationJobByID(ctx, pending.ID)
	if err != nil || job.Status != app.JobPending {
		t.Fatalf("cleanup escaped rollback: %+v %v", job, err)
	}
	generationSQL(t, store, `UPDATE generation_jobs SET status='running',lease_version=1,lease_expires_at=expires_at+interval '1 hour' WHERE id=$1`, pending.ID)
	err = store.Transaction(ctx, func(q *Queries) error {
		if _, err := q.queryer.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, pending.ID); err != nil {
			return err
		}
		return q.skipExpiredGenerationJob(ctx, pending, now)
	})
	if !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("stale fence accepted: %v", err)
	}
	job, err = store.GenerationJobByID(ctx, pending.ID)
	if err != nil || job.Status != app.JobRunning || job.LeaseVersion != 1 {
		t.Fatalf("stale fence changed job: %+v %v", job, err)
	}
}
