package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// Direct SQL represents retained legacy/operator history, not call permission.
func spendHistory(t *testing.T, store *Store, job app.GenerationJob, number int, lease int64, at time.Time, status app.GenerationAttemptStatus, reserved int64, in, out *int64) app.ID {
	t.Helper()
	id := app.NewID()
	var finish *time.Time
	code := ""
	if status != app.AttemptReserved {
		finish = &at
	}
	if status == app.AttemptUnknown {
		code = "lease_lost"
	}
	if status == app.AttemptFailed {
		code = "provider_transient"
	}
	generationSQL(t, store, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,context_hash,context_builder_version,budget_day,reserved_tokens,status,input_tokens,output_tokens,error_code,started_at,finished_at)
		VALUES($1,$2,$3,$4,'together','legacy',$5,'v1',$6,$7,$8,$9,$10,NULLIF($11,''),$12,$13)`, id, job.ID, number, lease, strings.Repeat("a", 64), at.UTC().Format(time.DateOnly), reserved, status, in, out, code, at, finish)
	return id
}

func TestGenerationSpendAccountingBoundaries(t *testing.T) {
	for _, extra := range []int64{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			history := contextJob(t, store, job, map[string]any{"created_at": job.CreatedAt.Add(-24 * time.Hour), "available_at": job.AvailableAt.Add(-24 * time.Hour), "cooldown_key": string(app.NewID())})
			at := job.CreatedAt.Add(30 * time.Second)
			spendHistory(t, store, history, 1, 1, at, app.AttemptSucceeded, 132096, spendInt(8192), spendInt(1024))
			spendHistory(t, store, history, 2, 2, at, app.AttemptUnknown, 132096, nil, nil)
			spendHistory(t, store, history, 3, 3, at, app.AttemptReserved, 132096, nil, nil)
			spendHistory(t, store, history, 4, 4, at, app.AttemptSucceeded, 94496+extra, nil, nil)
			// Same local time on a different UTC day must not count.
			spendHistory(t, store, history, 5, 5, at.Add(-24*time.Hour), app.AttemptUnknown, 500000, nil, nil)
			spendHistory(t, store, history, 6, 6, at.Add(24*time.Hour), app.AttemptUnknown, 500000, nil, nil)
			got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
			if err != nil || (got.Attempt != nil) != (extra == 0) || extra == 1 && got.Reason != "budget_exhausted" {
				t.Fatalf("exact budget: %+v %v", got, err)
			}
		})
	}
}

func TestGenerationSpendAccountingOverflow(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	history := contextJob(t, store, job, map[string]any{"created_at": job.CreatedAt.Add(-24 * time.Hour), "available_at": job.AvailableAt.Add(-24 * time.Hour), "cooldown_key": string(app.NewID())})
	for n := 1; n <= 2; n++ {
		spendHistory(t, store, history, n, int64(n), job.CreatedAt, app.AttemptUnknown, math.MaxInt64, nil, nil)
	}
	got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
	if !errors.Is(err, app.ErrUnavailable) || got.Attempt != nil || got.Reason != "" || !sameClaimJob(job, claimJob(t, store, job.ID)) {
		t.Fatalf("overflow: %+v %v", got, err)
	}
}

func TestGenerationSpendAllLeaseHistory(t *testing.T) {
	for _, test := range []struct {
		name         string
		count        int
		status       app.GenerationAttemptStatus
		acknowledged bool
		want         string
	}{
		{"unresolved", 1, app.AttemptReserved, false, "recovery_required"},
		{"unacknowledged_success", 1, app.AttemptSucceeded, false, "recovery_required"},
		{"acknowledged_unknown", 1, app.AttemptUnknown, true, ""},
		{"explicit_success_retry", 1, app.AttemptSucceeded, true, ""},
		{"third_attempt", 2, app.AttemptFailed, true, ""},
		{"limit", 3, app.AttemptFailed, true, "attempt_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, base, _ := spendFixture(t, 500000)
			job := contextJob(t, store, base, map[string]any{"created_at": base.CreatedAt.Add(2 * time.Second), "available_at": base.AvailableAt.Add(30 * time.Second), "lease_version": 4, "cooldown_key": string(app.NewID())})
			input, _ := readGenerationContext(t, store, job)
			at := job.AvailableAt
			if test.acknowledged {
				at = at.Add(-time.Second)
			}
			for n := 1; n <= test.count; n++ {
				spendHistory(t, store, job, n, int64(n), at, test.status, 132096, nil, nil)
			}
			before := claimJob(t, store, job.ID)
			got, err := store.ReserveGeneration(context.Background(), job.ID, 4, input)
			if err != nil || got.Reason != test.want || (got.Attempt != nil) != (test.want == "") {
				t.Fatalf("history: %+v %v", got, err)
			}
			if got.Attempt != nil && (got.Attempt.AttemptNumber != test.count+1 || got.Attempt.LeaseVersion != 4) {
				t.Fatal("reset attempt history")
			}
			if test.want == "recovery_required" && !sameClaimJob(before, claimJob(t, store, job.ID)) {
				t.Fatal("C1 invented recovery transition")
			}
		})
	}
}

func TestGenerationSpendConcurrentSameLease(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	var group sync.WaitGroup
	results := make(chan app.GenerationAdmission, 6)
	for range 6 {
		group.Add(1)
		go func() {
			defer group.Done()
			got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
			if err != nil {
				t.Error(err)
			}
			results <- got
		}()
	}
	group.Wait()
	close(results)
	admitted := 0
	for got := range results {
		if got.Attempt != nil {
			admitted++
		} else if got.Reason != "already_admitted" {
			t.Errorf("repeat: %+v", got)
		}
	}
	if admitted != 1 || spendAttemptCount(t, store) != 1 {
		t.Fatalf("permissions=%d", admitted)
	}
}

func TestGenerationSpendBudgetWaitFreshClock(t *testing.T) {
	for _, operation := range []string{"fresh", "expired_lease", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
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
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				admission app.GenerationAdmission
				err       error
			}
			done := make(chan result, 1)
			go func() { got, err := store.ReserveGeneration(ctx, job.ID, 1, input); done <- result{got, err} }()
			waitForDatabaseBlock(t, store, pid)
			// Budget must precede job: while admission waits on budget this lock
			// remains available. Updating lease expiry simulates expiry during wait.
			if _, err := tx.Exec(`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE NOWAIT`, job.ID); err != nil {
				t.Fatal(err)
			}
			if operation == "expired_lease" {
				if _, err := tx.Exec(`UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID); err != nil {
					t.Fatal(err)
				}
			}
			var released time.Time
			if err := tx.QueryRow(`SELECT clock_timestamp()`).Scan(&released); err != nil {
				t.Fatal(err)
			}
			if operation == "cancel" {
				cancel()
				got := <-done
				if !errors.Is(got.err, context.Canceled) || got.admission.Attempt != nil {
					t.Fatalf("cancel: %+v", got)
				}
				return
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if operation == "expired_lease" {
				if !errors.Is(got.err, app.ErrConflict) || got.admission.Attempt != nil {
					t.Fatalf("expired: %+v", got)
				}
				return
			}
			if got.err != nil || got.admission.Attempt == nil || got.admission.Attempt.StartedAt.Before(released) {
				t.Fatalf("old clock: %+v", got)
			}
		})
	}
}

type spendOrderQueryer struct {
	queryer
	order []string
}

func (q *spendOrderQueryer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q.order = append(q.order, query)
	return q.queryer.ExecContext(ctx, query, args...)
}
func (q *spendOrderQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.order = append(q.order, query)
	return q.queryer.QueryContext(ctx, query, args...)
}
func (q *spendOrderQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.order = append(q.order, query)
	return q.queryer.QueryRowContext(ctx, query, args...)
}

func TestGenerationSpendContinuationLockOrder(t *testing.T) {
	store, root, _ := spendFixture(t, 500000)
	var jobs []app.GenerationJob
	var inputs []app.GenerationContext
	for i := 0; i < 2; i++ {
		peer := socialAgent(t, store, fmt.Sprintf("chain_spender%d", i), nil)
		p := eligibilityPolicy()
		p.DailyTokenBudget = 500000
		encoded, _ := json.Marshal(p)
		generationSQL(t, store, `UPDATE agent_settings SET enabled=true,policy=$2 WHERE agent_id=$1`, peer.AgentID, encoded)
		job := contextJob(t, store, root, map[string]any{"agent_id": peer.AgentID, "trigger_kind": "continuation", "root_job_id": root.ID, "chain_depth": 1, "cooldown_key": string(app.NewID()), "created_at": root.CreatedAt.Add(2 * time.Second), "available_at": root.AvailableAt.Add(2 * time.Second)})
		input, _ := readGenerationContext(t, store, job)
		jobs = append(jobs, job)
		inputs = append(inputs, input)
	}
	var group sync.WaitGroup
	for i := range jobs {
		group.Add(1)
		go func() {
			defer group.Done()
			err := store.Transaction(context.Background(), func(q *Queries) error {
				trace := &spendOrderQueryer{queryer: q.queryer}
				q.queryer = trace
				got, err := q.reserveGeneration(context.Background(), jobs[i].ID, 1, inputs[i], nil)
				if err != nil {
					return err
				}
				if got.Attempt == nil {
					t.Errorf("chain denial: %+v", got)
				}
				var locks []string
				for _, statement := range trace.order {
					if strings.Contains(statement, "FOR UPDATE") || strings.Contains(statement, "FOR SHARE") || strings.Contains(statement, "pg_advisory_xact_lock") {
						locks = append(locks, statement)
					}
				}
				if len(locks) != 7 || !strings.Contains(locks[0], "posts") || !strings.Contains(locks[1], "accounts") || !strings.Contains(locks[2], "agent_settings") || !strings.Contains(locks[3], "pg_advisory_xact_lock") || !strings.Contains(locks[4], "generation_jobs") || !strings.Contains(locks[5], "generation_jobs") || !strings.Contains(locks[6], "generation_attempts") {
					t.Errorf("lock order: %v", locks)
				}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if spendAttemptCount(t, store) != 2 {
		t.Fatal("same-chain cross-agent admission failed")
	}
}

func TestGenerationSpendSourceCleanupRemainder(t *testing.T) {
	for _, repost := range []bool{false, true} {
		t.Run(fmt.Sprint(repost), func(t *testing.T) {
			store, base, _ := spendFixture(t, 500000)
			var sourceRepost app.ID
			if err := store.db.QueryRow(`SELECT id FROM reposts WHERE post_id=$1`, base.SourcePostID).Scan(&sourceRepost); err != nil {
				t.Fatal(err)
			}
			inputs := map[app.ID]app.GenerationContext{}
			for range 33 {
				changes := map[string]any{}
				if repost {
					changes["trigger_kind"], changes["source_repost_id"] = "repost", sourceRepost
				}
				job := contextJob(t, store, base, changes)
				input, _ := readGenerationContext(t, store, job)
				inputs[job.ID] = input
			}
			err := store.Transaction(context.Background(), func(q *Queries) error {
				if err := q.lockVisiblePost(context.Background(), *base.SourcePostID); err != nil {
					return err
				}
				if repost {
					if _, err := q.queryer.ExecContext(context.Background(), `DELETE FROM reposts WHERE id=$1`, sourceRepost); err != nil {
						return err
					}
					if _, err := q.queryer.ExecContext(context.Background(), `INSERT INTO reposts(id,post_id,account_id,created_at) VALUES($1,$2,$3,clock_timestamp())`, app.NewID(), base.SourcePostID, base.TriggerActorID); err != nil {
						return err
					}
				} else if _, err := q.queryer.ExecContext(context.Background(), `UPDATE posts SET deleted_at=clock_timestamp() WHERE id=$1`, base.SourcePostID); err != nil {
					return err
				}
				count, err := q.cancelRemovedGenerationSourceJobs(context.Background(), *base.SourcePostID)
				if count != 32 {
					t.Errorf("cleanup=%d", count)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for id, input := range inputs {
				job := claimJob(t, store, id)
				if job.Status != app.JobRunning {
					continue
				}
				got, err := store.ReserveGeneration(context.Background(), id, 1, input)
				if err != nil || got.Attempt != nil || got.Reason != "source_removed" || claimJob(t, store, id).Status != app.JobCancelled {
					t.Fatalf("remainder: %+v %v", got, err)
				}
				checked++
			}
			if checked == 0 || spendAttemptCount(t, store) != 0 {
				t.Fatal("no remainder exercised")
			}
		})
	}
}

func TestGenerationSpendSettingsWait(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(fmt.Sprint(account), func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			tx, err := store.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			query := `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`
			if account {
				query = `UPDATE accounts SET disabled_at=clock_timestamp() WHERE id=$1`
			}
			if _, err := tx.Exec(query, job.AgentID); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
				if err == nil && (got.Attempt != nil || got.Reason != "policy_disabled") {
					err = fmt.Errorf("unexpected admission: %+v", got)
				}
				done <- err
			}()
			waitForDatabaseBlock(t, store, pid)
			var budgetUnlocked bool
			if err := tx.QueryRow(`SELECT pg_try_advisory_xact_lock($1)`, generationBudgetLock).Scan(&budgetUnlocked); err != nil || !budgetUnlocked {
				t.Fatalf("budget before settings: %v", err)
			}
			if _, err := tx.Exec(`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE NOWAIT`, job.ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGenerationSpendExpiryDuringAccounting(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	err := store.Transaction(context.Background(), func(q *Queries) error {
		q.queryer = &spendClockQueryer{queryer: q.queryer, clocks: []time.Time{job.AvailableAt, job.ExpiresAt}}
		got, err := q.reserveGeneration(context.Background(), job.ID, 1, input, nil)
		if err == nil && (got.Attempt != nil || got.Reason != "stale_trigger") {
			t.Errorf("expiry: %+v", got)
		}
		return err
	})
	if err != nil || spendAttemptCount(t, store) != 0 || claimJob(t, store, job.ID).ReasonCode != "stale_trigger" {
		t.Fatalf("expiry: %v", err)
	}
}
