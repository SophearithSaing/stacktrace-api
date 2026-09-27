package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationRecoveryAbandonment(t *testing.T) {
	for _, name := range []string{"live", "expired_lease", "new_fence", "terminal_job", "expired_job"} {
		t.Run(name, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			switch name {
			case "expired_lease":
				generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
			case "new_fence":
				generationSQL(t, store, `UPDATE generation_jobs SET lease_version=2 WHERE id=$1`, job.ID)
			case "terminal_job":
				generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='source_removed',lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, job.ID)
			case "expired_job":
				// Immutable expiry: create historical work rather than editing identity.
				job = contextJob(t, store, job, map[string]any{"created_at": job.CreatedAt.Add(-time.Hour), "available_at": job.AvailableAt.Add(-time.Hour), "expires_at": job.CreatedAt, "cooldown_key": string(app.NewID())})
				id := spendHistory(t, store, job, 1, 1, job.CreatedAt, app.AttemptReserved, 132096, nil, nil)
				attempt = storedSpend(t, store, id)
			}
			before := claimJob(t, store, job.ID)
			want := 1
			if name == "live" {
				want = 0
			}
			if count, err := store.RecoverGenerationAttempts(context.Background()); err != nil || count != want {
				t.Fatal(count, err)
			}
			if !sameClaimJob(before, claimJob(t, store, job.ID)) {
				t.Fatal("recovery changed job")
			}
			saved := storedSpend(t, store, attempt.ID)
			if name == "live" {
				if saved.Status != app.AttemptReserved {
					t.Fatal(saved)
				}
				return
			}
			if saved.Status != app.AttemptUnknown || saved.ErrorCode != "lease_lost" || saved.AccountedTokens() != 132096 || saved.NotBefore != nil || saved.Decision != "" {
				t.Fatal(saved)
			}
			if ok, err := store.SettleGeneration(context.Background(), attempt, settlementOutcome(t, job), ""); err != nil || ok {
				t.Fatal("late success overwrote unknown", ok, err)
			}
			if count, err := store.RecoverGenerationAttempts(context.Background()); err != nil || count != 0 {
				t.Fatal("repeated recovery", count, err)
			}
		})
	}
}

func TestGenerationRecoveryBoundAndSkipLocked(t *testing.T) {
	store, _, root := generationSetup(t)
	job := contextJob(t, store, root, map[string]any{"lease_expires_at": time.Now().Add(-time.Second)})
	var first app.ID
	for n := 1; n <= 35; n++ {
		id := spendHistory(t, store, job, n, int64(n), job.CreatedAt.Add(time.Duration(n)*time.Microsecond), app.AttemptReserved, 132096, nil, nil)
		if n == 1 {
			first = id
		}
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT id FROM generation_attempts WHERE id=$1 FOR UPDATE`, first); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverGenerationAttempts(context.Background()); err != nil || count != 32 {
		t.Fatal(count, err)
	}
	if storedSpend(t, store, first).Status != app.AttemptReserved {
		t.Fatal("stole locked attempt")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverGenerationAttempts(context.Background()); err != nil || count != 3 {
		t.Fatal(count, err)
	}
}

func TestGenerationRecoveryRollback(t *testing.T) {
	for _, failure := range []string{"zero_rows", "after_write", "commit"} {
		t.Run(failure, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
			timing, body, prefix, deferred := "BEFORE", "RETURN NULL;", "", ""
			if failure != "zero_rows" {
				timing, body = "AFTER", "RAISE EXCEPTION 'secret';"
			}
			if failure == "commit" {
				prefix, deferred = "CONSTRAINT ", " DEFERRABLE INITIALLY DEFERRED"
			}
			generationSQL(t, store, `CREATE FUNCTION reject_recovery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
			generationSQL(t, store, `CREATE `+prefix+`TRIGGER reject_recovery `+timing+` UPDATE ON generation_attempts`+deferred+` FOR EACH ROW EXECUTE FUNCTION reject_recovery()`)
			count, err := store.RecoverGenerationAttempts(context.Background())
			if !errors.Is(err, app.ErrUnavailable) || count != 0 || storedSpend(t, store, attempt.ID).Status != app.AttemptReserved {
				t.Fatal(count, err)
			}
		})
	}
}

func TestGenerationRecoverySettlementRace(t *testing.T) {
	for range 6 {
		store, job, input := spendFixture(t, 500000)
		attempt := admitSpend(t, store, job, input)
		generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
		var group sync.WaitGroup
		group.Add(2)
		var count int
		var accepted bool
		go func() {
			defer group.Done()
			var err error
			count, err = store.RecoverGenerationAttempts(context.Background())
			if err != nil {
				t.Error(err)
			}
		}()
		outcome := settlementOutcome(t, job)
		go func() {
			defer group.Done()
			var err error
			accepted, err = store.SettleGeneration(context.Background(), attempt, outcome, "")
			if err != nil {
				t.Error(err)
			}
		}()
		group.Wait()
		saved := storedSpend(t, store, attempt.ID)
		if accepted {
			if count != 0 || saved.Status != app.AttemptSucceeded || saved.AccountedTokens() != 9216 {
				t.Fatal(count, saved)
			}
		} else if count != 1 || saved.Status != app.AttemptUnknown || saved.AccountedTokens() != 132096 {
			t.Fatal(count, saved)
		}
	}
}

func TestGenerationRecoveryNoJobLocksAndRenewal(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	// A committed renewal remains live and cannot be recovered.
	if _, err := store.RenewGeneration(context.Background(), job.ID, job.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverGenerationAttempts(context.Background()); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, query := range []string{`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`} {
		id := job.AgentID
		if query == `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE` {
			id = job.ID
		}
		if _, err := tx.Exec(query, id); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if count, err := store.RecoverGenerationAttempts(ctx); err != nil || count != 1 {
		t.Fatal("earlier locks acquired", count, err)
	}
	if storedSpend(t, store, attempt.ID).Status != app.AttemptUnknown {
		t.Fatal("not recovered")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if expiry, err := store.RenewGeneration(context.Background(), job.ID, job.LeaseVersion); !errors.Is(err, app.ErrConflict) || !expiry.IsZero() {
		t.Fatal("expired renewal", expiry, err)
	}
}

func TestGenerationCompletionNoBudgetOrSettingsLocks(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	if ok, err := store.SettleGeneration(context.Background(), attempt, app.GenerationOutcome{Failure: app.GenerationTransient}, ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, generationBudgetLock); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, job.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, job.AgentID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if status, err := store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, 0); err != nil || status != app.JobRetryWait {
		t.Fatal(status, err)
	}
}

func TestGenerationRecoveryBudgetWaitAndRenewal(t *testing.T) {
	for _, renew := range []bool{false, true} {
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
		type result struct {
			count int
			err   error
		}
		done := make(chan result, 1)
		go func() {
			count, err := store.RecoverGenerationAttempts(context.Background())
			done <- result{count, err}
		}()
		waitForDatabaseBlock(t, store, pid)
		if _, err := tx.Exec(`SELECT id FROM generation_attempts WHERE id=$1 FOR UPDATE NOWAIT`, attempt.ID); err != nil {
			t.Fatal("attempt locked before budget", err)
		}
		if renew {
			if _, err := store.RenewGeneration(context.Background(), job.ID, 1); err != nil {
				t.Fatal(err)
			}
		} else {
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
		want := 1
		if renew {
			want = 0
		}
		if got.err != nil || got.count != want {
			t.Fatal(got)
		}
		if !renew && storedSpend(t, store, attempt.ID).FinishedAt.Before(released) {
			t.Fatal("used pre-budget clock")
		}
	}
}
