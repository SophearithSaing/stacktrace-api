package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestWorkerBridgeLatestAndTerminalDenial(t *testing.T) {
	ctx := context.Background()
	store, job, input := spendFixture(t, 500000)
	if got, err := store.LatestGenerationAttempt(ctx, job.ID); err != nil || got != nil {
		t.Fatal(got, err)
	}
	attempt := admitSpend(t, store, job, input)
	attempt = storedSpend(t, store, attempt.ID)
	if got, err := store.LatestGenerationAttempt(ctx, job.ID); err != nil || !reflect.DeepEqual(got, &attempt) {
		t.Fatal(got, err)
	}
	for _, denial := range []app.GenerationDenial{"succeeded", "retry_wait", "provider body secret", ""} {
		if _, err := store.DenyGeneration(ctx, job.ID, job.LeaseVersion, denial); !errors.Is(err, app.ErrConflict) {
			t.Fatal(err)
		}
	}
	if _, err := store.DenyGeneration(ctx, job.ID, job.LeaseVersion+1, app.GenerationContextRejected); !errors.Is(err, app.ErrConflict) {
		t.Fatal(err)
	}
	if !sameClaimJob(job, claimJob(t, store, job.ID)) {
		t.Fatal("denial changed job without authority")
	}
	for range 2 {
		if status, err := store.DenyGeneration(ctx, job.ID, job.LeaseVersion, app.GenerationContextRejected); err != nil || status != app.JobSkipped {
			t.Fatal(status, err)
		}
	}
	if !reflect.DeepEqual(attempt, storedSpend(t, store, attempt.ID)) {
		t.Fatal("denial rewrote observation")
	}
	if _, err := store.DenyGeneration(ctx, job.ID, job.LeaseVersion, app.GenerationOutputUnavailable); !errors.Is(err, app.ErrConflict) {
		t.Fatal("conflicting replay", err)
	}
}

func TestWorkerBridgeDenialCleanupAndFence(t *testing.T) {
	for _, mode := range []string{"expired_lease", "reclaimed", "expired_job", "removed_repost"} {
		t.Run(mode, func(t *testing.T) {
			store, job, _ := spendFixture(t, 500000)
			want := app.JobSkipped
			switch mode {
			case "expired_lease":
				generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
			case "reclaimed":
				reclaimSpend(t, store, job)
			case "expired_job":
				job = contextJob(t, store, job, map[string]any{"expires_at": time.Now().Add(-time.Second), "cooldown_key": string(app.NewID())})
			case "removed_repost":
				job = contextJob(t, store, job, map[string]any{"trigger_kind": "repost", "source_repost_id": nil, "lease_expires_at": time.Now().Add(-time.Second), "cooldown_key": string(app.NewID())})
				want = app.JobCancelled
			}
			before := claimJob(t, store, job.ID)
			status, err := store.DenyGeneration(context.Background(), job.ID, job.LeaseVersion, app.GenerationContextRejected)
			if mode == "expired_lease" || mode == "reclaimed" {
				if !errors.Is(err, app.ErrConflict) || !sameClaimJob(before, claimJob(t, store, job.ID)) {
					t.Fatal(status, err)
				}
			} else if err != nil || status != want {
				t.Fatal(status, err)
			}
		})
	}
}

func TestWorkerBridgeExpiryAndLatestHistory(t *testing.T) {
	store, job, _ := spendFixture(t, 500000)
	if _, err := store.ExpireGenerationJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	job = contextJob(t, store, job, map[string]any{"expires_at": time.Now().Add(-time.Second), "cooldown_key": string(app.NewID())})
	first := spendHistory(t, store, job, 1, 1, job.CreatedAt, app.AttemptUnknown, 132096, nil, nil)
	last := spendHistory(t, store, job, 2, 2, job.CreatedAt.Add(time.Second), app.AttemptFailed, 132096, nil, nil)
	got, err := store.LatestGenerationAttempt(context.Background(), job.ID)
	if err != nil || got == nil || got.ID != last || got.ID == first {
		t.Fatal(got, err)
	}
	for _, want := range []int{1, 0} {
		if count, err := store.ExpireGenerationJobs(context.Background()); err != nil || count != want {
			t.Fatal(count, err)
		}
	}
	if saved := claimJob(t, store, job.ID); saved.Status != app.JobSkipped || saved.ReasonCode != "stale_trigger" {
		t.Fatal(saved)
	}
}
