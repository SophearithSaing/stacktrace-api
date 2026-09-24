package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestExecutionEligibilityLockOrderAndCancellation(t *testing.T) {
	for _, stage := range []string{"source", "account", "settings"} {
		t.Run(stage, func(t *testing.T) {
			store, job, _ := eligibilityFixture(t)
			ctx := context.Background()
			blocker, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			sourceSQL, accountSQL, settingsSQL, jobSQL := `SELECT id FROM posts WHERE id=$1 FOR UPDATE`, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`
			query, id := sourceSQL, *job.SourcePostID
			if stage == "account" {
				query, id = accountSQL, job.AgentID
			}
			if stage == "settings" {
				query, id = settingsSQL, job.AgentID
			}
			if _, err := blocker.ExecContext(ctx, query, id); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- store.Transaction(callCtx, func(q *Queries) error { _, _, _, err := q.lockExecutionEligibility(callCtx, job); return err })
			}()
			waitForDatabaseBlock(t, store, pid)
			// Later-order locks have not been acquired while an earlier one waits.
			if stage == "source" {
				if _, err := blocker.ExecContext(ctx, accountSQL+` NOWAIT`, job.AgentID); err != nil {
					t.Fatalf("account before source: %v", err)
				}
			}
			if stage != "settings" {
				if _, err := blocker.ExecContext(ctx, settingsSQL+` NOWAIT`, job.AgentID); err != nil {
					t.Fatalf("settings before account/source: %v", err)
				}
			}
			if _, err := blocker.ExecContext(ctx, jobSQL+` NOWAIT`, job.ID); err != nil {
				t.Fatalf("preparation locked job: %v", err)
			}
			// Previously acquired source/account locks are retained until rollback.
			if stage != "source" {
				assertEligibilityLock(t, store, sourceSQL, *job.SourcePostID, false)
			}
			if stage == "settings" {
				assertEligibilityLock(t, store, accountSQL, job.AgentID, false)
				// SHARE, not UPDATE: another SHARE remains compatible.
				assertEligibilityLock(t, store, strings.Replace(accountSQL, "UPDATE", "SHARE", 1), job.AgentID, true)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			released, releaseCancel := context.WithTimeout(ctx, time.Second)
			defer releaseCancel()
			if _, err := blocker.ExecContext(released, sourceSQL, *job.SourcePostID); err != nil {
				t.Fatalf("retained source after cancellation: %v", err)
			}
			if _, err := blocker.ExecContext(released, accountSQL, job.AgentID); err != nil {
				t.Fatalf("retained account after cancellation: %v", err)
			}
		})
	}
}

func assertEligibilityLock(t *testing.T, store *Store, query string, id app.ID, want bool) {
	t.Helper()
	probe, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Rollback()
	_, err = probe.Exec(query+` NOWAIT`, id)
	if (err == nil) != want {
		t.Fatalf("lock availability=%v want=%v: %v", err == nil, want, err)
	}
}

func TestExecutionEligibilityPostLockState(t *testing.T) {
	for _, mutation := range []string{"disable", "pause", "tighten", "delete settings", "delete source"} {
		t.Run(mutation, func(t *testing.T) {
			store, job, _ := eligibilityFixture(t)
			ctx := context.Background()
			blocker, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			query := `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`
			id := job.AgentID
			switch mutation {
			case "disable":
				query = `UPDATE accounts SET disabled_at=now() WHERE id=$1`
			case "tighten":
				query = `UPDATE agent_settings SET policy=jsonb_set(policy,'{reply_cap_per_day}','0') WHERE agent_id=$1`
			case "delete settings":
				query = `DELETE FROM agent_settings WHERE agent_id=$1`
			case "delete source":
				query, id = `UPDATE posts SET deleted_at=now() WHERE id=$1`, *job.SourcePostID
			}
			if _, err := blocker.ExecContext(ctx, query, id); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			type result struct {
				reason string
				policy app.GenerationPolicy
				err    error
			}
			done := make(chan result, 1)
			go func() {
				var r result
				r.err = store.Transaction(ctx, func(q *Queries) error {
					var err error
					r.policy, _, r.reason, err = q.lockExecutionEligibility(ctx, job)
					return err
				})
				done <- r
			}()
			waitForDatabaseBlock(t, store, pid)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			r := <-done
			want := "policy_disabled"
			if mutation == "delete source" {
				want = "source_removed"
			}
			if mutation == "tighten" {
				want = ""
				if r.policy.ReplyCapPerDay != 0 {
					t.Fatal("read stale pre-lock policy")
				}
			}
			if r.err != nil || r.reason != want {
				t.Fatalf("post-lock result: %q want=%q err=%v", r.reason, want, r.err)
			}
		})
	}
}

func TestExecutionEligibilityHoldsAuthorityWithoutJobOrActorLocks(t *testing.T) {
	store, job, _ := eligibilityFixture(t)
	ctx := context.Background()
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	// Human actor is observed, never locked after source. Job authority is later.
	if _, err := blocker.Exec(`SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, job.TriggerActorID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
		t.Fatal(err)
	}
	err = store.Transaction(ctx, func(q *Queries) error {
		_, _, reason, err := q.lockExecutionEligibility(ctx, job)
		if err != nil || reason != "" {
			t.Fatalf("unrelated locks blocked preparation: %q %v", reason, err)
		}
		assertEligibilityLock(t, store, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, job.AgentID, false)
		assertEligibilityLock(t, store, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, job.AgentID, false)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sameClaimJob(job, claimJob(t, store, job.ID)) {
		t.Fatal("eligibility mutated job")
	}
}

type eligibilityErrorQueryer struct{ queryer }

func (q eligibilityErrorQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "FROM agent_settings") || strings.Contains(query, "FROM generation_jobs") {
		return q.queryer.QueryRowContext(ctx, `SELECT secret_column FROM secret_table`)
	}
	return q.queryer.QueryRowContext(ctx, query, args...)
}

func TestExecutionEligibilitySafeDatabaseErrors(t *testing.T) {
	store, job, p := eligibilityFixture(t)
	err := store.Transaction(context.Background(), func(q *Queries) error {
		q.queryer = eligibilityErrorQueryer{q.queryer}
		_, _, _, err := q.lockExecutionEligibility(context.Background(), job)
		return err
	})
	if !errors.Is(err, app.ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe database error: %v", err)
	}
	err = store.Transaction(context.Background(), func(q *Queries) error {
		q.queryer = eligibilityErrorQueryer{q.queryer}
		_, err := q.executionPolicyAllowed(context.Background(), job, p, nil, job.CreatedAt)
		return err
	})
	if !errors.Is(err, app.ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe policy database error: %v", err)
	}
}
