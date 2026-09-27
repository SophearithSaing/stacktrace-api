package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func completeSpend(t *testing.T, store *Store, job app.GenerationJob, attempt app.GenerationAttempt, want app.GenerationJobStatus) app.GenerationJob {
	t.Helper()
	status, err := store.CompleteGeneration(context.Background(), job.ID, job.LeaseVersion, attempt.ID, 0)
	if err != nil || status != want {
		t.Fatalf("complete: %s %v want %s", status, err, want)
	}
	return claimJob(t, store, job.ID)
}

func reclaimSpend(t *testing.T, store *Store, job app.GenerationJob) app.GenerationJob {
	t.Helper()
	generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
	claimed, err := store.ClaimGeneration(context.Background())
	if err != nil || claimed == nil || claimed.ID != job.ID || claimed.LeaseVersion != job.LeaseVersion+1 {
		t.Fatalf("reclaim: %+v %v", claimed, err)
	}
	return *claimed
}

func TestGenerationCompletionDurableDecisions(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure app.GenerationFailure
		want    app.GenerationJobStatus
		reason  string
	}{
		{"publish", "", app.JobRunning, ""}, {"skip", "", app.JobSkipped, "not_relevant"},
		{"transient", app.GenerationTransient, app.JobRetryWait, "provider_transient"},
		{"rate_limit", app.GenerationRateLimited, app.JobRetryWait, "provider_rate_limited"},
		{"timeout", app.GenerationTimeout, app.JobRetryWait, "provider_timeout"},
		{"invalid", app.GenerationInvalidOutput, app.JobRetryWait, "invalid_output"},
		{"unsafe", app.GenerationUnsafeOutput, app.JobSkipped, "unsafe_output"},
		{"repeat", app.GenerationRepeatedOutput, app.JobSkipped, "repeated_output"},
		{"cancel", app.GenerationCancelled, app.JobCancelled, "execution_cancelled"},
		{"credentials", app.GenerationCredentials, app.JobFailed, "provider_credentials"},
		{"configuration", app.GenerationConfiguration, app.JobFailed, "provider_configuration"},
		{"accounting", app.GenerationAccountingUnsupported, app.JobFailed, "unsupported_accounting"},
		{"normalized_accounting", "", app.JobFailed, "unsupported_accounting"},
		{"permanent", app.GenerationPermanent, app.JobFailed, "provider_permanent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			outcome := settlementOutcome(t, job)
			if test.failure != "" {
				outcome = app.GenerationOutcome{Failure: test.failure}
			}
			if test.name == "skip" {
				var err error
				outcome.Result, err = app.DecodeGenerationResult([]byte(`{"decision":"skip","reason":"not_relevant"}`), job)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "normalized_accounting" {
				outcome.InputTokens = spendInt(8193)
			}
			if test.name == "rate_limit" {
				outcome.NotBefore = app.GenerationInstant(time.Now().Add(time.Minute))
			}
			if ok, err := store.SettleGeneration(context.Background(), attempt, outcome, ""); err != nil || !ok {
				t.Fatal(ok, err)
			}
			// Process-local result is gone. Only the durable normalized observation is used.
			saved := completeSpend(t, store, job, attempt, test.want)
			if saved.ReasonCode != test.reason || !saved.ExpiresAt.Equal(job.ExpiresAt) {
				t.Fatal("wrong transition", saved)
			}
			if test.name == "rate_limit" && !saved.AvailableAt.Equal(outcome.NotBefore) {
				t.Fatal("hint shortened", saved.AvailableAt)
			}
			if test.want != app.JobRunning {
				// Even after expiry and with different jitter, exact replay cannot change anything.
				later := job.ExpiresAt.Add(time.Hour)
				if got, err := store.completeGeneration(context.Background(), job.ID, job.LeaseVersion, attempt.ID, time.Second, &later); err != nil || got != test.want {
					t.Fatal(got, err)
				}
				if !sameClaimJob(saved, claimJob(t, store, job.ID)) {
					t.Fatal("replay changed acknowledgement")
				}
			}
		})
	}
}

func TestGenerationCompletionReclaimedObservation(t *testing.T) {
	for _, name := range []string{"publish", "skip", "failure", "unknown", "legacy_success", "legacy_failure"} {
		t.Run(name, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			want, reason := app.JobFailed, "outcome_unavailable"
			switch name {
			case "legacy_success":
				generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=clock_timestamp() WHERE id=$1`, attempt.ID)
			case "legacy_failure":
				generationSQL(t, store, `UPDATE generation_attempts SET status='failed',error_code='legacy_code',finished_at=clock_timestamp() WHERE id=$1`, attempt.ID)
			case "unknown":
			default:
				outcome := settlementOutcome(t, job)
				if name == "skip" {
					outcome.Result, _ = app.DecodeGenerationResult([]byte(`{"decision":"skip","reason":"repetition"}`), job)
					want, reason = app.JobSkipped, "repetition"
				}
				if name == "failure" {
					outcome = app.GenerationOutcome{Failure: app.GenerationTransient}
					want, reason = app.JobRetryWait, "provider_transient"
				}
				if ok, err := store.SettleGeneration(context.Background(), attempt, outcome, ""); err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			claimed := reclaimSpend(t, store, job)
			if name == "unknown" {
				if count, err := store.RecoverGenerationAttempts(context.Background()); err != nil || count != 1 {
					t.Fatal(count, err)
				}
				want, reason = app.JobRetryWait, "lease_lost"
			}
			if got, err := store.CompleteGeneration(context.Background(), job.ID, job.LeaseVersion, attempt.ID, 0); !errors.Is(err, app.ErrConflict) || got != "" {
				t.Fatal("old fence", got, err)
			}
			saved := completeSpend(t, store, claimed, attempt, want)
			if saved.ReasonCode != reason {
				t.Fatal(saved)
			}
			if name == "unknown" && storedSpend(t, store, attempt.ID).AccountedTokens() != 132096 {
				t.Fatal("released unknown accounting")
			}
		})
	}
}

func TestGenerationCompletionAcknowledgedPriorAllowsAdmission(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	// Retained, previously acknowledged failure predates the retry availability.
	id := spendHistory(t, store, job, 1, 1, job.AvailableAt.Add(-time.Second), app.AttemptFailed, 132096, spendInt(1), spendInt(1))
	generationSQL(t, store, `UPDATE generation_jobs SET lease_version=2 WHERE id=$1`, job.ID)
	job = claimJob(t, store, job.ID)
	prior := storedSpend(t, store, id)
	before := completeSpend(t, store, job, prior, app.JobRunning)
	if !sameClaimJob(job, before) {
		t.Fatal("old failure acknowledged twice")
	}
	admitted := admitSpend(t, store, job, input)
	if admitted.AttemptNumber != 2 || admitted.LeaseVersion != 2 {
		t.Fatal(admitted)
	}
	if got, err := store.CompleteGeneration(context.Background(), job.ID, 2, id, 0); !errors.Is(err, app.ErrConflict) || got != "" {
		t.Fatal("not latest", got, err)
	}
	if got, err := store.CompleteGeneration(context.Background(), job.ID, 2, admitted.ID, 0); !errors.Is(err, app.ErrConflict) || got != "" {
		t.Fatal("reserved accepted", got, err)
	}
}

func TestGenerationCompletionRetryCapsAndExpiry(t *testing.T) {
	for _, name := range []string{"third_attempt", "second_invalid", "hint_at_expiry", "expired_job", "expired_lease", "bad_jitter"} {
		t.Run(name, func(t *testing.T) {
			store, job, _ := spendFixture(t, 500000)
			count := 1
			if name == "third_attempt" {
				count = 3
			}
			if name == "second_invalid" {
				count = 2
			}
			var id app.ID
			for n := 1; n <= count; n++ {
				id = spendHistory(t, store, job, n, int64(n), job.AvailableAt.Add(time.Duration(n)*time.Second), app.AttemptReserved, 132096, nil, nil)
				failure := app.GenerationTransient
				if name == "second_invalid" {
					failure = app.GenerationInvalidOutput
				}
				outcome := app.GenerationOutcome{Failure: failure}
				if name == "hint_at_expiry" {
					outcome.NotBefore = job.ExpiresAt
				}
				if ok, err := store.SettleGeneration(context.Background(), storedSpend(t, store, id), outcome, ""); err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			generationSQL(t, store, `UPDATE generation_jobs SET lease_version=$2 WHERE id=$1`, job.ID, count)
			job = claimJob(t, store, job.ID)
			now := app.GenerationInstant(time.Now())
			jitter := time.Duration(0)
			want := app.JobFailed
			if name == "expired_job" {
				now = job.ExpiresAt
				want = app.JobSkipped
			}
			if name == "expired_lease" {
				now = *job.LeaseExpiresAt
			}
			if name == "bad_jitter" {
				jitter = time.Second + 1
			}
			got, err := store.completeGeneration(context.Background(), job.ID, job.LeaseVersion, id, jitter, &now)
			if name == "expired_lease" || name == "bad_jitter" {
				if err == nil || got != "" || !sameClaimJob(job, claimJob(t, store, job.ID)) {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || got != want {
				t.Fatal(got, err)
			}
			if !claimJob(t, store, job.ID).ExpiresAt.Equal(job.ExpiresAt) {
				t.Fatal("expiry changed")
			}
		})
	}
}

func TestGenerationCompletionRollback(t *testing.T) {
	for _, failure := range []string{"zero_rows", "after_write", "commit"} {
		t.Run(failure, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			if ok, err := store.SettleGeneration(context.Background(), attempt, app.GenerationOutcome{Failure: app.GenerationTransient}, ""); err != nil || !ok {
				t.Fatal(ok, err)
			}
			timing, body, prefix, deferred := "BEFORE", "RETURN NULL;", "", ""
			if failure != "zero_rows" {
				timing, body = "AFTER", "RAISE EXCEPTION 'secret';"
			}
			if failure == "commit" {
				prefix, deferred = "CONSTRAINT ", " DEFERRABLE INITIALLY DEFERRED"
			}
			generationSQL(t, store, `CREATE FUNCTION reject_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
			generationSQL(t, store, `CREATE `+prefix+`TRIGGER reject_completion `+timing+` UPDATE ON generation_jobs`+deferred+` FOR EACH ROW EXECUTE FUNCTION reject_completion()`)
			got, err := store.CompleteGeneration(context.Background(), job.ID, job.LeaseVersion, attempt.ID, 0)
			if !errors.Is(err, app.ErrUnavailable) || got != "" || !sameClaimJob(job, claimJob(t, store, job.ID)) {
				t.Fatal(got, err)
			}
		})
	}
}

func TestGenerationCompletionRemovedRepost(t *testing.T) {
	for _, expired := range []bool{false, true} {
		store, _, root := generationSetup(t)
		actor, post, _, repost := generationSocial(t, store, root)
		changes := contextSocialChanges(actor, post, app.TriggerRepost)
		changes["source_repost_id"] = repost
		job := contextJob(t, store, root, changes)
		id := spendHistory(t, store, job, 1, 1, job.AvailableAt, app.AttemptReserved, 132096, nil, nil)
		attempt := storedSpend(t, store, id)
		if ok, err := store.SettleGeneration(context.Background(), attempt, settlementOutcome(t, job), ""); err != nil || !ok {
			t.Fatal(ok, err)
		}
		generationSQL(t, store, `DELETE FROM reposts WHERE id=$1`, repost)
		now := *job.LeaseExpiresAt // Removed-source cleanup does not require a live lease.
		want, reason := app.JobCancelled, "source_removed"
		if expired {
			now, want, reason = job.ExpiresAt, app.JobSkipped, "stale_trigger"
		}
		if got, err := store.completeGeneration(context.Background(), job.ID, 1, id, 0, &now); err != nil || got != want {
			t.Fatal(got, err)
		}
		if saved := claimJob(t, store, job.ID); saved.ReasonCode != reason {
			t.Fatal(saved)
		}
	}
}

func TestGenerationCompletionFreshAfterBlockingReads(t *testing.T) {
	for _, expired := range []bool{false, true} {
		store, job, input := spendFixture(t, 500000)
		attempt := admitSpend(t, store, job, input)
		if ok, err := store.SettleGeneration(context.Background(), attempt, app.GenerationOutcome{Failure: app.GenerationPermanent}, ""); err != nil || !ok {
			t.Fatal(ok, err)
		}
		tx, err := store.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var pid int
		if err := tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
			t.Fatal(err)
		}
		type result struct {
			status app.GenerationJobStatus
			err    error
		}
		done := make(chan result, 1)
		go func() {
			status, err := store.CompleteGeneration(context.Background(), job.ID, 1, attempt.ID, 0)
			done <- result{status, err}
		}()
		waitForDatabaseBlock(t, store, pid)
		if expired {
			if _, err := tx.Exec(`UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID); err != nil {
				t.Fatal(err)
			}
		}
		var released time.Time
		if err := tx.QueryRow(`SELECT clock_timestamp()`).Scan(&released); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if expired {
			if !errors.Is(got.err, app.ErrConflict) || got.status != "" {
				t.Fatal(got)
			}
			continue
		}
		if got.err != nil || got.status != app.JobFailed {
			t.Fatal(got)
		}
		if claimJob(t, store, job.ID).FinishedAt.Before(released) {
			t.Fatal("used pre-lock clock")
		}
	}
}

func TestGenerationCompletionWaitsForSettlement(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var pid int
	if err := tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, generationBudgetLock); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT id FROM generation_attempts WHERE id=$1 FOR UPDATE`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status app.GenerationJobStatus
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := store.CompleteGeneration(context.Background(), job.ID, 1, attempt.ID, 0)
		done <- result{status, err}
	}()
	waitForDatabaseBlock(t, store, pid)
	if _, err := tx.Exec(`UPDATE generation_attempts SET status='failed',error_code='provider_rate_limited',not_before=clock_timestamp()+interval '1 minute',finished_at=clock_timestamp() WHERE id=$1`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	var released time.Time
	if err := tx.QueryRow(`SELECT clock_timestamp()`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.status != app.JobRetryWait {
		t.Fatal(got)
	}
	if !claimJob(t, store, job.ID).AvailableAt.Equal(*storedSpend(t, store, attempt.ID).NotBefore) || claimJob(t, store, job.ID).AvailableAt.Before(released) {
		t.Fatal("did not use committed hint")
	}
}

func TestGenerationCompletionRetryClaimAdmission(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(context.Background(), attempt, app.GenerationOutcome{Failure: app.GenerationInvalidOutput}, ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	queued := completeSpend(t, store, job, attempt, app.JobRetryWait)
	claimed, err := store.claimGeneration(context.Background(), &queued.AvailableAt)
	if err != nil || claimed == nil || claimed.ID != job.ID || claimed.LeaseVersion != 2 {
		t.Fatal(claimed, err)
	}
	if status, err := store.completeGeneration(context.Background(), job.ID, 2, attempt.ID, 0, &queued.AvailableAt); err != nil || status != app.JobRunning {
		t.Fatal(status, err)
	}
	if !sameClaimJob(*claimed, claimJob(t, store, job.ID)) {
		t.Fatal("acknowledged prior failure twice")
	}
	admitted, err := store.reserveGeneration(context.Background(), job.ID, 2, input, &queued.AvailableAt)
	if err != nil || admitted.Attempt == nil || admitted.Attempt.AttemptNumber != 2 {
		t.Fatal(admitted, err)
	}
	if again, err := store.reserveGeneration(context.Background(), job.ID, 2, input, &queued.AvailableAt); err != nil || again.Attempt != nil || again.Reason != "already_admitted" {
		t.Fatal(again, err)
	}
}

func TestGenerationSpendInvalidRegenerationAcrossLeases(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	for n := 1; n <= 2; n++ {
		id := spendHistory(t, store, job, n, int64(n), job.AvailableAt.Add(-time.Second), app.AttemptReserved, 132096, nil, nil)
		generationSQL(t, store, `UPDATE generation_attempts SET status='failed',error_code='invalid_output',finished_at=started_at WHERE id=$1`, id)
	}
	generationSQL(t, store, `UPDATE generation_jobs SET lease_version=3 WHERE id=$1`, job.ID)
	got, err := store.ReserveGeneration(context.Background(), job.ID, 3, input)
	if err != nil || got.Attempt != nil || got.Reason != "invalid_output" || claimJob(t, store, job.ID).Status != app.JobFailed {
		t.Fatal(got, err)
	}
}
