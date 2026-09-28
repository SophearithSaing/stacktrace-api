package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func retryFixture(t *testing.T, failure app.GenerationFailure) (*Store, app.GenerationJob, app.GenerationAttempt) {
	t.Helper()
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: failure}, ""); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return store, claimJob(t, store, job.ID), attempt
}

func validReason(job app.GenerationJob) bool {
	return job.Status == app.JobFailed && job.ReasonCode == string(app.GenerationCredentials)
}

func TestGenerationRetryRetention(t *testing.T) {
	store, failed, attempt := retryFixture(t, app.GenerationCredentials)
	ctx := context.Background()
	if !validReason(failed) || failed.LeaseVersion != 1 {
		t.Fatalf("retry fixture job: %+v", failed)
	}
	retried, err := store.RetryGeneration(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != app.JobRetryWait || retried.ReasonCode != "operator_retry" ||
		retried.LeaseVersion != failed.LeaseVersion || !retried.ExpiresAt.Equal(failed.ExpiresAt) ||
		retried.FinishedAt != nil || retried.LeaseExpiresAt != nil ||
		!retried.AvailableAt.After(*failed.FinishedAt) || retried.AvailableAt.Before(failed.CreatedAt) {
		t.Fatalf("retained identity/expiry: %+v", retried)
	}
	if stored := storedSpend(t, store, attempt.ID); stored.AccountedTokens() != attempt.ReservedTokens || stored.Status != app.AttemptFailed {
		t.Fatalf("usage/history erased: %+v", stored)
	}
	// The queued job re-enters ordinary claiming; a fresh claim must still pass
	// complete admission for its new attempt. Retry grants no spend authority.
	got, err := store.claimGeneration(ctx, nil)
	if err != nil || got == nil {
		t.Fatalf("claim: %+v %v", got, err)
	}
	claimed := claimJob(t, store, failed.ID)
	if claimed.Status != app.JobRunning || claimed.LeaseVersion != 2 {
		t.Fatalf("claim: %+v", claimed)
	}
	input, _ := readGenerationContext(t, store, claimed)
	if admission, err := store.ReserveGeneration(ctx, claimed.ID, claimed.LeaseVersion, input); err != nil || admission.Attempt == nil {
		t.Fatalf("retried admission: %+v %v", admission, err)
	}
	// A retried job is no longer a retry candidate.
	if got, err := store.RetryGeneration(ctx, retried.ID); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("re-retry: %+v %v", got, err)
	}
}

func TestGenerationRetryDenials(t *testing.T) {
	t.Run("accounting_stop", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationAccountingUnsupported)
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("accounting retry: %+v %v", got, err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		store, job, input := spendFixture(t, 500000)
		now := time.Now().UTC()
		job = contextJob(t, store, job, map[string]any{"expires_at": now.Add(900 * time.Millisecond), "lease_expires_at": now.Add(400 * time.Millisecond), "created_at": now.Add(-90 * time.Second), "available_at": now.Add(-90 * time.Second), "cooldown_key": string(app.NewID())})
		ctx := context.Background()
		input, _ = readGenerationContext(t, store, job)
		attempt := admitSpend(t, store, job, input)
		if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationCredentials}, ""); err != nil || !ok {
			t.Fatalf("settle: %v %v", ok, err)
		}
		if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if got, err := store.RetryGeneration(ctx, job.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("expired retry: %+v %v", got, err)
		}
	})
	t.Run("attempt_limit", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		for range app.MaxGenerationAttempts - 1 {
			generationSQL(t, store, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
				context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,finished_at,error_code)
				SELECT $1,job_id,attempt_number+1,lease_version,provider,model,context_hash,context_builder_version,budget_day,reserved_tokens,'failed',started_at,finished_at,error_code
				FROM generation_attempts WHERE job_id=$2 ORDER BY attempt_number DESC LIMIT 1`, app.NewID(), failed.ID)
		}
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("attempt-limited retry: %+v %v", got, err)
		}
	})
	t.Run("invalid_output_limit", func(t *testing.T) {
		store, job, input := spendFixture(t, 500000)
		ctx := context.Background()
		for round := 0; round < 2; round++ {
			admission, err := store.ReserveGeneration(ctx, job.ID, job.LeaseVersion, input)
			if err != nil || admission.Attempt == nil {
				t.Fatalf("reserve: %+v %v", admission, err)
			}
			attempt := *admission.Attempt
			if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationInvalidOutput}, ""); err != nil || !ok {
				t.Fatalf("settle: %v %v", ok, err)
			}
			if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
				t.Fatalf("complete: %v", err)
			}
			if existing := claimJob(t, store, job.ID); existing.Status == app.JobRetryWait {
				// The auto backoff delay is a real-clock availability; no override.
				time.Sleep(2200 * time.Millisecond)
				got, err := store.ClaimGeneration(ctx)
				if err != nil || got == nil || got.ID != job.ID {
					t.Fatalf("claim: %+v %v", got, err)
				}
				job = claimJob(t, store, job.ID)
				read, cntErr := store.GenerationContext(ctx, job.ID, job.LeaseVersion)
				if cntErr != nil {
					t.Fatal(cntErr)
				}
				input = read
			}
		}
		failed := claimJob(t, store, job.ID)
		if failed.Status != app.JobFailed || failed.ReasonCode != string(app.GenerationInvalidOutput) {
			t.Fatalf("invalid exhausted: %+v", failed)
		}
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("invalid output retry: %+v %v", got, err)
		}
	})
	t.Run("source_removed", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		generationSQL(t, store, `UPDATE posts SET deleted_at=now() WHERE id=$1`, failed.SourcePostID)
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrDeleted) {
			t.Fatalf("removed source retry: %+v %v", got, err)
		}
	})
	t.Run("account_disabled", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		if err := store.DisableAccount(context.Background(), failed.AgentID); err != nil {
			t.Fatal(err)
		}
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("disabled agent retry: %+v %v", got, err)
		}
	})
	t.Run("paused", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		if err := store.PauseAgent(context.Background(), failed.AgentID); err != nil {
			t.Fatal(err)
		}
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("paused agent retry: %+v %v", got, err)
		}
	})
	t.Run("missing_job", func(t *testing.T) {
		store, _, _ := retryFixture(t, app.GenerationCredentials)
		if got, err := store.RetryGeneration(context.Background(), app.NewID()); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("missing retry: %+v %v", got, err)
		}
	})
	t.Run("non_failed", func(t *testing.T) {
		store, job, input := spendFixture(t, 500000)
		ctx := context.Background()
		_ = admitSpend(t, store, job, input)
		if got, err := store.RetryGeneration(ctx, job.ID); !errors.Is(err, app.ErrConflict) {
			t.Fatalf("running retry: %+v %v", got, err)
		}
	})
}

// TestGenerationRetryReAdmission proves a re-queued failed job admits fresh
// work with an incremented attempt number without refunding charged usage.
func TestGenerationRetryReAdmission(t *testing.T) {
	store, failed, oldAttempt := retryFixture(t, app.GenerationCredentials)
	ctx := context.Background()
	if retried, err := store.RetryGeneration(ctx, failed.ID); err != nil || retried.Status != app.JobRetryWait {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	if got, err := store.claimGeneration(ctx, nil); err != nil || got == nil {
		t.Fatalf("claim: %+v %v", got, err)
	}
	claimed := claimJob(t, store, failed.ID)
	input, _ := readGenerationContext(t, store, claimed)
	admission, err := store.ReserveGeneration(ctx, claimed.ID, claimed.LeaseVersion, input)
	if err != nil || admission.Attempt == nil {
		t.Fatalf("fresh admission: %+v %v", admission, err)
	}
	if admission.Attempt.AttemptNumber != 2 {
		t.Fatalf("attempt continuation: %+v", admission.Attempt)
	}
	if stored := storedSpend(t, store, oldAttempt.ID); stored.AccountedTokens() != oldAttempt.ReservedTokens || stored.Status != app.AttemptFailed {
		t.Fatalf("retained accounting: %+v", stored)
	}
}

// TestGenerationRetryHintDenial proves a settled Retry-After beyond the
// immutable job expiry can delay but never extend queued work.
func TestGenerationRetryHintDenial(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	hint := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationRateLimited, NotBefore: hint}, ""); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
		t.Fatal(err)
	}
	failed := claimJob(t, store, job.ID)
	if failed.Status != app.JobFailed || failed.ReasonCode != string(app.GenerationRateLimited) {
		t.Fatalf("hint fixture job: %+v", failed)
	}
	if storedSpend(t, store, attempt.ID).NotBefore == nil {
		t.Fatal("hint not retained in the failed attempt")
	}
	if got, err := store.RetryGeneration(ctx, failed.ID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("hint beyond expiry: %+v %v", got, err)
	}
}

func funcer(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, app.ErrConflict):
		return "conflict"
	case errors.Is(err, app.ErrForbidden):
		return "forbidden"
	case errors.Is(err, app.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, app.ErrDeleted):
		return "deleted"
	case errors.Is(err, app.ErrNotFound):
		return "notfound"
	default:
		return err.Error()
	}
}

func TestGenerationRetryConcurrency(t *testing.T) {
	store, failed, _ := retryFixture(t, app.GenerationCredentials)
	ctx := context.Background()
	results := make(chan app.GenerationJob, 4)
	errs := make(chan error, 4)
	var group sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		group.Go(func() {
			<-start
			got, err := store.RetryGeneration(ctx, failed.ID)
			results <- got
			errs <- err
		})
	}
	close(start)
	group.Wait()
	queued, invalid := 0, ""
	for range 4 {
		got, err := <-results, <-errs
		switch {
		case err == nil && got.Status == app.JobRetryWait:
			queued++
		case errors.Is(err, app.ErrConflict) && got.ID == "":
		default:
			invalid = funcer(err) + " " + string(got.Status)
		}
	}
	if queued != 1 || invalid != "" || claimJob(t, store, failed.ID).Status != app.JobRetryWait {
		t.Fatalf("concurrent retried states: %d queued %q", queued, invalid)
	}
}

func TestGenerationRetryPauseRace(t *testing.T) {
	store, failed, _ := retryFixture(t, app.GenerationCredentials)
	ctx := context.Background()
	results := make(chan error, 2)
	retried := make(chan app.GenerationJob, 2)
	var group sync.WaitGroup
	start := make(chan struct{})
	group.Go(func() {
		<-start
		got, err := store.RetryGeneration(ctx, failed.ID)
		results <- err
		retried <- got
	})
	group.Go(func() {
		<-start
		count, err := store.PauseAllAgents(ctx)
		if err != nil || count != 1 {
			t.Errorf("pause: %d %v", count, err)
		}
	})
	close(start)
	group.Wait()
	// Both orders are safe and valid: either the operator retry re-queued the
	// failed job and the pause then disabled the agent, or the pause won and
	// the retry was denied eligibility. No store error escapes.
	rate := <-results
	final := claimJob(t, store, failed.ID)
	proceed := <-retried
	switch {
	case rate == nil:
		if final.Status != app.JobRetryWait || proceed.Status != app.JobRetryWait {
			t.Fatalf("retry winner state: %+v", final)
		}
	case errors.Is(rate, app.ErrForbidden), errors.Is(rate, app.ErrConflict):
		if final.Status != app.JobFailed || proceed.ID != "" {
			t.Fatalf("pause winner state: %+v %+v", final, proceed)
		}
	default:
		t.Fatalf("retry error: %v", rate)
	}
}

func TestGenerationRetryRollback(t *testing.T) {
	store, failed, _ := retryFixture(t, app.GenerationCredentials)
	generationSQL(t, store, `CREATE FUNCTION reject_retry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'secret retry failure'; END $$;
		CREATE TRIGGER reject_retry BEFORE UPDATE ON generation_jobs FOR EACH ROW WHEN (NEW.status='retry_wait') EXECUTE FUNCTION reject_retry()`)
	if _, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("rollback error: %v", err)
	}
	fresh := claimJob(t, store, failed.ID)
	if fresh.Status != app.JobFailed || !fresh.AvailableAt.Equal(failed.AvailableAt) || fresh.ReasonCode != failed.ReasonCode {
		t.Fatalf("rollback changed state: %+v", fresh)
	}
}
