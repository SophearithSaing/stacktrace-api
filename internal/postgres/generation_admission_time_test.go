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

// Every policy statement still executes on PostgreSQL. Advance the projected
// DB clock while the last query of the second policy evaluation runs, rather
// than merely assuming admission samples the clock a particular number of times.
type admissionPolicyClock struct {
	queryer
	at, after time.Time
	marker    string
	checks    int
}

func (q *admissionPolicyClock) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "clock_timestamp()") {
		return q.queryer.QueryRowContext(ctx, `SELECT $1::timestamptz`, q.at)
	}
	row := q.queryer.QueryRowContext(ctx, query, args...)
	if strings.Contains(query, q.marker) {
		q.checks++
		if q.checks == 2 {
			q.at = q.after
		}
	}
	return row
}

func TestGenerationSpendFinalPolicyTime(t *testing.T) {
	for _, test := range []struct {
		name      string
		scheduled bool
		want      string
		conflict  bool
	}{
		{"valid", false, "", false},
		{"valid_scheduled", true, "", false},
		{"lease_expiry", false, "", true},
		{"job_expiry", false, "stale_trigger", false},
		{"utc_rollover", false, "", true},
		{"source_age", false, "policy_denied", false},
		{"scheduled_window", true, "policy_denied", false},
		{"scheduled_source_age", true, "policy_denied", false},
		{"backwards_clock", false, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, root := generationSetup(t)
			ctx := context.Background()
			at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
			if test.name == "utc_rollover" {
				at = time.Date(2026, 9, 27, 23, 59, 59, 0, time.UTC)
			}
			if test.name == "scheduled_window" {
				at = time.Date(2026, 9, 27, 16, 59, 59, 0, time.UTC)
			}
			after := at.Add(time.Second)
			if test.name == "backwards_clock" {
				after = at.Add(-time.Second)
			}
			p := eligibilityPolicy()
			p.DailyTokenBudget = 500000
			if test.name == "source_age" || test.name == "scheduled_source_age" {
				p.SourceMaxAgeSeconds = 60
			}
			changes := map[string]any{
				"created_at": at.Add(-30 * time.Second), "available_at": at.Add(-30 * time.Second),
				"expires_at": at.Add(time.Hour), "lease_expires_at": at.Add(time.Minute),
			}
			if test.name == "lease_expiry" {
				changes["lease_expires_at"] = after
			}
			if test.name == "job_expiry" {
				changes["expires_at"] = after
			}
			marker := "WHERE trigger_actor_id=$1 AND trigger_kind IN"
			if test.scheduled {
				key, err := app.ScheduledGenerationKey(at.Add(-59 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
				changes["trigger_key"] = key
				marker = "AND created_at >= $3 AND created_at<$4"
			} else {
				actor, post, _, _ := generationSocial(t, store, root)
				generationSQL(t, store, `UPDATE posts SET created_at=$2 WHERE id=$1`, post, at.Add(-59*time.Second))
				for key, value := range contextSocialChanges(actor, post, app.TriggerHumanPost) {
					changes[key] = value
				}
			}
			job := contextJob(t, store, root, changes)
			if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: job.AgentID, PersonaVersion: 1, Enabled: true, Policy: p, UpdatedAt: job.CreatedAt}); err != nil {
				t.Fatal(err)
			}
			var input app.GenerationContext
			if err := store.readSnapshot(ctx, func(q *Queries) error {
				q.queryer = contextClockQueryer{queryer: q.queryer, at: at}
				var err error
				input, err = q.generationContext(ctx, job.ID, 1)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var got app.GenerationAdmission
			err := store.Transaction(ctx, func(q *Queries) error {
				clock := &admissionPolicyClock{queryer: q.queryer, at: at, after: after, marker: marker}
				q.queryer = clock
				var err error
				got, err = q.reserveGeneration(ctx, job.ID, 1, input, nil)
				if clock.checks != 2 {
					t.Errorf("did not reach both policy evaluations: %d", clock.checks)
				}
				return err
			})
			if test.conflict {
				if !errors.Is(err, app.ErrConflict) || got.Attempt != nil || spendAttemptCount(t, store) != 0 || !sameClaimJob(job, claimJob(t, store, job.ID)) {
					t.Fatalf("expired/unchecked authority: %+v %v", got, err)
				}
				return
			}
			if err != nil || got.Reason != test.want {
				t.Fatalf("admission: %+v %v", got, err)
			}
			if test.want != "" {
				finished := claimJob(t, store, job.ID)
				if got.Attempt != nil || spendAttemptCount(t, store) != 0 || finished.ReasonCode != test.want || finished.FinishedAt == nil || !finished.FinishedAt.Equal(after) {
					t.Fatalf("denial authority/time: %+v %+v", got, finished)
				}
				return
			}
			if got.Attempt == nil || !got.Attempt.StartedAt.Equal(after) || got.Attempt.BudgetDay != after.UTC().Format(time.DateOnly) || got.Attempt.ReservedTokens != 132096 {
				t.Fatalf("stale reservation timestamp: %+v", got)
			}
			persisted := storedSpend(t, store, got.Attempt.ID)
			if !persisted.StartedAt.Equal(after) || spendAttemptCount(t, store) != 1 || !sameClaimJob(job, claimJob(t, store, job.ID)) {
				t.Fatalf("reservation changed: %+v", persisted)
			}
		})
	}
}
