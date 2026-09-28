package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
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

// realProviderFixture implements the provider boundary with a fixed outcome;
// a successful publication needs no schedule/lease knobs beyond the fixtures.
type realProviderFixture struct {
	result app.GenerationResult
}

func (p realProviderFixture) Generate(ctx context.Context, req app.GenerationRequest) app.GenerationOutcome {
	return app.GenerationOutcome{Result: p.result, ProviderRequestID: "retry-worker.1"}
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
	t.Run("accounting_retained", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		generationAttempt(t, store, failed, 2)
		generationSQL(t, store, `UPDATE generation_attempts SET error_code='unsupported_accounting' WHERE job_id=$1`, failed.ID)
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("retained accounting retry: %+v %v", got, err)
		}
	})
	t.Run("unrecognized_reason", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		generationSQL(t, store, `UPDATE generation_jobs SET reason_code='arbitrary_final' WHERE id=$1`, failed.ID)
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("unrecognized retry: %+v %v", got, err)
		}
	})
	t.Run("final_observation_reasons", func(t *testing.T) {
		for _, reason := range []string{string(app.GenerationUnsafeOutput), string(app.GenerationRepeatedOutput), string(app.GenerationCancelled)} {
			store, failed, _ := retryFixture(t, app.GenerationCredentials)
			generationSQL(t, store, `UPDATE generation_jobs SET reason_code=$2 WHERE id=$1`, failed.ID, reason)
			if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
				t.Fatalf("%s retained: %+v %v", reason, got, err)
			}
		}
	})
	t.Run("skip_attempt_in_failed_row", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		// A successful skip attempt must not ride the success bypass: its
		// outcome belongs to skipped work.
		generationAttempt(t, store, failed, 2)
		generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,
			output_digest='generation_output_v1:'||repeat('a',64),decision='skip',skip_reason='not_relevant' WHERE job_id=$1`, failed.ID)
		generationSQL(t, store, `UPDATE generation_jobs SET reason_code='outcome_unavailable' WHERE id=$1`, failed.ID)
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("skip attempt in failed row: %+v %v", got, err)
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
		if failed := claimJob(t, store, job.ID); failed.Status != app.JobFailed {
			t.Fatalf("expired fixture: %+v", failed)
		}
		time.Sleep(time.Second)
		if got, err := store.RetryGeneration(ctx, job.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("expired retry: %+v %v", got, err)
		}
	})
	t.Run("attempt_limit", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		for range app.MaxGenerationAttempts - 1 {
			generationAttempt(t, store, failed, 2)
			generationSQL(t, store, `UPDATE generation_attempts SET status='failed',finished_at=started_at,error_code='provider_credentials' WHERE job_id=$1`, failed.ID)
		}
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("attempt-limited retry: %+v %v", got, err)
		}
	})
	t.Run("invalid_output_limit", func(t *testing.T) {
		store, job, input := spendFixture(t, 500000)
		ctx := context.Background()
		for round := 1; round <= 2; round++ {
			attempt := admitSpend(t, store, job, input)
			if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationInvalidOutput}, ""); err != nil || !ok {
				t.Fatalf("settle: %v %v", ok, err)
			}
			if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
				t.Fatalf("complete: %v", err)
			}
			if existing := claimJob(t, store, job.ID); existing.Status == app.JobRetryWait {
				time.Sleep(2200 * time.Millisecond) // Real backoff, no override.
				got, err := store.ClaimGeneration(ctx)
				if err != nil || got == nil || got.ID != job.ID {
					t.Fatalf("claim: %+v %v", got, err)
				}
				job = claimJob(t, store, job.ID)
				input, _ = readGenerationContext(t, store, job)
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

// TestGenerationRetryTerminalDenials covers each final, non-retryable job
// state with the real store: succeeded, skipped and cancelled.
func TestGenerationRetryTerminalDenials(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		store, job, attempt, output := publicationFixture(t, app.OutputReply, "A succeeded retry denial")
		ctx := context.Background()
		published, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output)
		if err != nil || published.Status != app.JobSucceeded {
			t.Fatalf("publish: %+v %v", published, err)
		}
		if got, retryErr := store.RetryGeneration(ctx, job.ID); !errors.Is(retryErr, app.ErrConflict) {
			t.Fatalf("succeeded retry: %+v %v", got, retryErr)
		}
	})
	t.Run("skipped", func(t *testing.T) {
		store, job, input := spendFixture(t, 500000)
		ctx := context.Background()
		attempt := admitSpend(t, store, job, input)
		skip, err := app.DecodeGenerationResult([]byte(`{"decision":"skip","reason":"not_relevant"}`), job)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Result: skip}, ""); err != nil || !ok {
			t.Fatalf("settle: %v %v", ok, err)
		}
		if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
			t.Fatal(err)
		}
		if skipped := claimJob(t, store, job.ID); skipped.Status != app.JobSkipped {
			t.Fatalf("skip fixture: %+v", skipped)
		}
		if got, retryErr := store.RetryGeneration(ctx, job.ID); !errors.Is(retryErr, app.ErrConflict) {
			t.Fatalf("skipped retry: %+v %v", got, retryErr)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		store, job, input := spendFixture(t, 500000)
		ctx := context.Background()
		attempt := admitSpend(t, store, job, input)
		if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationCancelled}, ""); err != nil || !ok {
			t.Fatalf("settle: %v %v", ok, err)
		}
		if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
			t.Fatal(err)
		}
		if cancelled := claimJob(t, store, job.ID); cancelled.Status != app.JobCancelled {
			t.Fatalf("cancel fixture: %+v", cancelled)
		}
		if got, retryErr := store.RetryGeneration(ctx, job.ID); !errors.Is(retryErr, app.ErrConflict) {
			t.Fatalf("cancelled retry: %+v %v", got, retryErr)
		}
	})
}

// TestGenerationRetryTightenedPolicy: a policy or source that could never
// admit new work denies the retry instead of queueing it.
func TestGenerationRetryTightenedPolicy(t *testing.T) {
	t.Run("zero_reply_quota", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{reply_cap_per_day}','0') WHERE agent_id=$1`, failed.AgentID)
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("zero quota retry: %+v %v", got, err)
		}
	})
	t.Run("stale_source_window", func(t *testing.T) {
		store, failed, _ := retryFixture(t, app.GenerationCredentials)
		generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{source_max_age_seconds}','60') WHERE agent_id=$1`, failed.AgentID)
		generationSQL(t, store, `UPDATE posts SET created_at=clock_timestamp()-interval '3 minutes' WHERE id=$1`, failed.SourcePostID)
		if got, err := store.RetryGeneration(context.Background(), failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("stale source retry: %+v %v", got, err)
		}
	})
	t.Run("reduced_chain_policy", func(t *testing.T) {
		store, job, attempt, output := publicationFixture(t, app.OutputPost, "@chain_child_tight a real continuation")
		peer := socialAgent(t, store, "chain_child_tight", func(p *app.GenerationPolicy) { p.DailyTokenBudget = 500000 })
		ctx := context.Background()
		published, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output)
		if err != nil || published.Status != app.JobSucceeded {
			t.Fatalf("root publish: %+v %v", published, err)
		}
		var child app.GenerationJob
		for _, candidate := range socialJobs(t, store) {
			if candidate.RootJobID == job.ID && candidate.ID != job.ID {
				child = candidate
			}
		}
		if child.ID == "" {
			t.Fatal("missing real continuation")
		}
		if child.AgentID != peer.AgentID {
			t.Fatalf("unexpected child: %+v", child)
		}
		generationSQL(t, store, `UPDATE generation_jobs SET status='running',lease_version=1,
			lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, child.ID)
		child = claimJob(t, store, child.ID)
		childInput, _ := readGenerationContext(t, store, child)
		childAttempt := admitSpend(t, store, child, childInput)
		if ok, err := store.SettleGeneration(ctx, childAttempt, app.GenerationOutcome{Failure: app.GenerationCredentials}, ""); err != nil || !ok {
			t.Fatalf("settle: %v %v", ok, err)
		}
		if _, err := store.CompleteGeneration(ctx, child.ID, child.LeaseVersion, childAttempt.ID, 0); err != nil {
			t.Fatal(err)
		}
		failed := claimJob(t, store, child.ID)
		if failed.Status != app.JobFailed {
			t.Fatalf("child fixture: %+v", failed)
		}
		// Reduce the agent's chain policy under the root's immutable limits.
		generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{max_chain_depth}','0') WHERE agent_id=$1`, failed.AgentID)
		if got, err := store.RetryGeneration(ctx, failed.ID); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("tightened chain retry: %+v %v", got, err)
		}
		generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{max_chain_depth}','9') WHERE agent_id=$1`, failed.AgentID)
		if retried, err := store.RetryGeneration(ctx, failed.ID); err != nil || retried.Status != app.JobRetryWait {
			t.Fatalf("restored chain retry: %+v %v", retried, err)
		}
	})
}

// TestGenerationRetryRealExecution proves the whole chain with the real store
// and one fake provider call: failed job -> operator retry -> ordinary claim
// -> acknowledged prior attempt -> fresh reservation -> settlement ->
// publication, with the old charge retained and attempt 1 conserved.
func TestGenerationRetryRealExecution(t *testing.T) {
	store, failed, oldAttempt := retryFixture(t, app.GenerationCredentials)
	if failed.Status != app.JobFailed || failed.ReasonCode != string(app.GenerationCredentials) {
		t.Fatalf("fixture job: %+v", failed)
	}
	ctx := context.Background()
	if retried, err := store.RetryGeneration(ctx, failed.ID); err != nil || retried.Status != app.JobRetryWait {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	// Run the full worker pass against the real store: Ready, recovery,
	// expiry, claim, acknowledged prior attempt, reserve, call, settle,
	// complete and publish. The pass owns the claim itself.
	claimed := claimJob(t, store, failed.ID)
	providerResult, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A repaired retry publication result."}`), claimed)
	if err != nil {
		t.Fatal(err)
	}
	result, execErr := worker.Execute(ctx, store, realProviderFixture{result: providerResult})
	if execErr != nil {
		t.Fatalf("execute: %+v %v", result, execErr)
	}
	final := claimJob(t, store, failed.ID)
	// The pass claimed, called and published once; the expired row is the
	// stale canned-fixture root job, not the retried one.
	if result.Claimed != 1 || result.Calls != 1 || result.Published != 1 {
		t.Fatalf("pass summary: %+v", result)
	}
	if final.Status != app.JobSucceeded || final.PublishedAttemptID == nil || final.ResultReplyID == nil {
		t.Fatalf("published job: %+v", final)
	}
	// Attempt history is conserved, and the old charge is retained exactly.
	attempts, err := store.executionAttempts(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].ID != oldAttempt.ID || attempts[1].Status != app.AttemptSucceeded {
		t.Fatalf("attempt history: %+v", attempts)
	}
	if stored := storedSpend(t, store, oldAttempt.ID); stored.AccountedTokens() != oldAttempt.ReservedTokens || stored.Status != app.AttemptFailed {
		t.Fatalf("retained accounting: %+v", stored)
	}
}

// TestGenerationRetryInvalidRecovery proves a failed job whose first attempt
// was invalid output plus a later provider credentials failure stays within
// the attempt/invalid caps and remains explicitly retirable after repair.
func TestGenerationRetryInvalidRecovery(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationInvalidOutput}, ""); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
		t.Fatal(err)
	}
	if existing := claimJob(t, store, job.ID); existing.Status == app.JobRetryWait {
		time.Sleep(2200 * time.Millisecond) // Real backoff, no override.
		got, err := store.ClaimGeneration(ctx)
		if err != nil || got == nil {
			t.Fatalf("claim: %+v %v", got, err)
		}
		job = claimJob(t, store, job.ID)
		input, _ = readGenerationContext(t, store, job)
		second := admitSpend(t, store, job, input)
		if ok, err := store.SettleGeneration(ctx, second, app.GenerationOutcome{Failure: app.GenerationCredentials}, ""); err != nil || !ok {
			t.Fatalf("settle: %v %v", ok, err)
		}
		if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, second.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	failed := claimJob(t, store, job.ID)
	if failed.Status != app.JobFailed || failed.ReasonCode != string(app.GenerationCredentials) {
		t.Fatalf("fixture: %+v", failed)
	}
	retried, err := store.RetryGeneration(ctx, failed.ID)
	if err != nil || retried.Status != app.JobRetryWait {
		t.Fatalf("still eligible within caps: %+v %v", retried, err)
	}
}

// TestGenerationRetryRetainedHint keeps a valid in-window provider Retry-After
// exactly: credentials failures are never auto-retried, so the job fails while
// the attempt retains its hint; the operator retry then re-uses that hint as an
// availability floor. The 45-minute hint is always inside the fixture's
// ~59-minute expiry, so both instants are deterministic.
func TestGenerationRetryRetainedHint(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	hint := time.Now().UTC().Add(45 * time.Minute).Truncate(time.Microsecond)
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationCredentials, NotBefore: hint}, ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
		t.Fatal(err)
	}
	failed := claimJob(t, store, job.ID)
	if failed.Status != app.JobFailed {
		t.Fatalf("hint fixture job: %+v", failed)
	}
	retried, err := store.RetryGeneration(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.AvailableAt.Equal(app.GenerationInstant(hint)) {
		t.Fatalf("retained hint not honored: %+v vs %v", retried.AvailableAt, hint)
	}
	retained, err := store.LatestGenerationAttempt(ctx, job.ID)
	if err != nil || retained == nil || retained.NotBefore == nil {
		t.Fatalf("attempt hint retained: %v", err)
	}
	if !retained.NotBefore.Equal(app.GenerationInstant(hint)) {
		t.Fatalf("attempt hint erasure: %v", retained.NotBefore)
	}
}

// TestGenerationRetryHintDenial restores the beyond-expiry case: a Retry-After
// can delay but never extend past the immutable job expiry, and the retained
// attempt hint stays untouched for the denial.
func TestGenerationRetryHintDenial(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	hint := job.ExpiresAt.Add(time.Hour).Truncate(time.Microsecond)
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationCredentials, NotBefore: hint}, ""); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
		t.Fatal(err)
	}
	failed := claimJob(t, store, job.ID)
	if failed.Status != app.JobFailed || failed.ReasonCode != string(app.GenerationCredentials) {
		t.Fatalf("hint fixture job: %+v", failed)
	}
	retained, err := store.LatestGenerationAttempt(ctx, job.ID)
	if err != nil || retained == nil || retained.NotBefore == nil || !retained.NotBefore.Equal(app.GenerationInstant(hint)) {
		t.Fatalf("denial lost the retained hint: %+v %v", retained, err)
	}
	if got, retryErr := store.RetryGeneration(ctx, failed.ID); !errors.Is(retryErr, app.ErrForbidden) {
		t.Fatalf("hint beyond expiry: %+v %v", got, retryErr)
	}
}

func TestGenerationRetryAvailabilityFloor(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationCredentials}, ""); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if _, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil {
		t.Fatal(err)
	}
	failed := claimJob(t, store, job.ID)
	retried, err := store.RetryGeneration(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.AvailableAt.After(app.GenerationInstant(*failed.FinishedAt)) {
		t.Fatalf("availability did not strictly exceed the finished attempt: %+v vs %v", retried.AvailableAt, *failed.FinishedAt)
	}
}

// TestGenerationRetryConcurrency: only one of N concurrent retries may re-queue
// the failed job. Results and errors from each caller stay paired in a single
// struct channel so no goroutine can cross another's commit state.
func TestGenerationRetryConcurrency(t *testing.T) {
	store, failed, _ := retryFixture(t, app.GenerationCredentials)
	ctx := context.Background()
	type retryOutcome struct {
		job app.GenerationJob
		err error
	}
	results := make(chan retryOutcome, 4)
	var group sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		group.Go(func() {
			<-start
			got, err := store.RetryGeneration(ctx, failed.ID)
			results <- retryOutcome{job: got, err: err}
		})
	}
	close(start)
	group.Wait()
	queued := 0
	for range 4 {
		got := <-results
		switch {
		case got.err == nil && got.job.Status == app.JobRetryWait:
			queued++
		case got.err == nil:
			t.Fatalf("unexpected success: %+v", got.job)
		case !errors.Is(got.err, app.ErrConflict) || got.job.ID != "":
			t.Fatalf("loser error: %v %v", got.err, got.job)
		}
	}
	if queued != 1 || claimJob(t, store, failed.ID).Status != app.JobRetryWait {
		t.Fatalf("concurrent retried states: %d queued", queued)
	}
}

func TestGenerationRetryPauseRace(t *testing.T) {
	store, failed, _ := retryFixture(t, app.GenerationCredentials)
	ctx := context.Background()
	type pauseOutcome struct {
		job app.GenerationJob
		err error
	}
	results := make(chan pauseOutcome, 2)
	var group sync.WaitGroup
	start := make(chan struct{})
	group.Go(func() {
		<-start
		got, err := store.RetryGeneration(ctx, failed.ID)
		results <- pauseOutcome{job: got, err: err}
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
	switch {
	case rate.err == nil:
		if final.Status != app.JobRetryWait || rate.job.Status != app.JobRetryWait {
			t.Fatalf("retry winner state: %+v", final)
		}
	case errors.Is(rate.err, app.ErrForbidden), errors.Is(rate.err, app.ErrConflict):
		if final.Status != app.JobFailed || rate.job.ID != "" {
			t.Fatalf("pause winner state: %+v %+v", final, rate.job)
		}
	default:
		t.Fatalf("retry error: %v", rate.err)
	}
}

// TestGenerationRetryRootWaitOrder proves the retry acquires the root job
// before the child job (the executionPolicyAllowed contract) and never holds
// the child or attempt lock while the root lock waits.
func TestGenerationRetryRootWaitOrder(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputPost, "@retry_root_child a real continuation")
	peer := socialAgent(t, store, "retry_root_child", func(p *app.GenerationPolicy) { p.DailyTokenBudget = 500000 })
	ctx := context.Background()
	published, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output)
	if err != nil || published.Status != app.JobSucceeded {
		t.Fatalf("root publish: %+v %v", published, err)
	}
	var child app.GenerationJob
	for _, candidate := range socialJobs(t, store) {
		if candidate.RootJobID == job.ID && candidate.ID != job.ID {
			child = candidate
		}
	}
	if child.ID == "" || child.AgentID != peer.AgentID {
		t.Fatalf("missing real continuation: %+v", child)
	}
	// Fail the child with a credentials outcome so it becomes retirable.
	generationSQL(t, store, `UPDATE generation_jobs SET status='running',lease_version=1,
		lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, child.ID)
	child = claimJob(t, store, child.ID)
	childInput, _ := readGenerationContext(t, store, child)
	childAttempt := admitSpend(t, store, child, childInput)
	if ok, err := store.SettleGeneration(ctx, childAttempt, app.GenerationOutcome{Failure: app.GenerationCredentials}, ""); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if _, err := store.CompleteGeneration(ctx, child.ID, child.LeaseVersion, childAttempt.ID, 0); err != nil {
		t.Fatal(err)
	}
	failed := claimJob(t, store, child.ID)
	if failed.Status != app.JobFailed {
		t.Fatalf("child fixture: %+v", failed)
	}
	// The root row lock forces the retry transaction to wait exactly between
	// its root and child acquisitions, mirroring admission's documented order.
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := store.RetryGeneration(callCtx, failed.ID); done <- err }()
	waitForDatabaseBlock(t, store, pid)
	// The child job and its newest attempt are not locked while the root waits.
	probeSQL := `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE NOWAIT`
	if _, probeErr := blocker.ExecContext(ctx, probeSQL, failed.ID); probeErr != nil {
		t.Fatalf("child before root: %v", probeErr)
	}
	if _, probeErr := blocker.ExecContext(ctx, `SELECT id FROM generation_attempts WHERE id=$1 FOR UPDATE NOWAIT`, childAttempt.ID); probeErr != nil {
		t.Fatalf("attempt before root: %v", probeErr)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
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
