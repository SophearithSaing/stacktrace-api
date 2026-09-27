package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/llm"
)

func spendFixture(t *testing.T, budget int64) (*Store, app.GenerationJob, app.GenerationContext) {
	t.Helper()
	store, _, root := generationSetup(t)
	actor, post, _, _ := generationSocial(t, store, root)
	generationSQL(t, store, `UPDATE posts SET created_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, post)
	job := contextJob(t, store, root, contextSocialChanges(actor, post, app.TriggerHumanPost))
	p := eligibilityPolicy()
	p.DailyTokenBudget = budget
	if _, err := store.InitializeAgentSettings(context.Background(), app.AgentSettings{AgentID: job.AgentID, PersonaVersion: 1, Enabled: true, Policy: p, UpdatedAt: job.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	input, _ := readGenerationContext(t, store, job)
	return store, job, input
}

func spendAttemptCount(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM generation_attempts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func admitSpend(t *testing.T, store *Store, job app.GenerationJob, input app.GenerationContext) app.GenerationAttempt {
	t.Helper()
	got, err := store.ReserveGeneration(context.Background(), job.ID, job.LeaseVersion, input)
	if err != nil || got.Attempt == nil {
		t.Fatalf("reserve: %+v %v", got, err)
	}
	return *got.Attempt
}

func TestGenerationSpendMetadataAndDuplicate(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	prompt, err := llm.BuildPrompt(app.GenerationRequest{Job: job, Context: input})
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ReservedTokens != 132096 || attempt.ContextHash != prompt.Hash() || attempt.Provider != llm.Provider || attempt.Model != llm.Model || attempt.ContextBuilderVersion != llm.ContextBuilderVersion || attempt.BudgetDay != attempt.StartedAt.UTC().Format(time.DateOnly) {
		t.Fatalf("wrong metadata: %+v", attempt)
	}
	got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
	if err != nil || got.Attempt != nil || got.Reason != "already_admitted" || spendAttemptCount(t, store) != 1 {
		t.Fatalf("duplicate: %+v %v", got, err)
	}
	if !sameClaimJob(job, claimJob(t, store, job.ID)) {
		t.Fatal("admission changed job authority")
	}
}

func TestGenerationSpendContextBindingAndLocalBound(t *testing.T) {
	for _, invalid := range []string{"empty", "wrong_job", "persona", "oversized"} {
		t.Run(invalid, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			switch invalid {
			case "empty":
				input = app.GenerationContext{}
			case "wrong_job":
				other := contextJob(t, store, job, nil)
				input, _ = readGenerationContext(t, store, other)
			case "persona", "oversized":
				persona, err := store.PersonaByVersion(context.Background(), job.AgentID, 1)
				if err != nil {
					t.Fatal(err)
				}
				persona.Version = 2
				if invalid == "oversized" {
					persona.Instructions = strings.Repeat("x", 7000)
				}
				if err := store.CreatePersona(context.Background(), persona); err != nil {
					t.Fatal(err)
				}
				other := contextJob(t, store, job, map[string]any{"persona_version": 2, "created_at": job.CreatedAt.Add(2 * time.Second), "available_at": job.AvailableAt.Add(2 * time.Second), "cooldown_key": string(app.NewID())})
				input, _ = readGenerationContext(t, store, other)
				if invalid == "oversized" {
					job = other
				}
			}
			got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
			if err == nil || got.Attempt != nil || spendAttemptCount(t, store) != 0 {
				t.Fatalf("invalid preparation: %+v %v", got, err)
			}
		})
	}
}

func TestGenerationSpendConcurrentBudgets(t *testing.T) {
	for _, differentAgents := range []bool{false, true} {
		t.Run(fmt.Sprint(differentAgents), func(t *testing.T) {
			budget, want := int64(264192), 2
			if differentAgents {
				budget, want = 500000, 3
			}
			store, job, input := spendFixture(t, budget)
			jobs, inputs := []app.GenerationJob{job}, []app.GenerationContext{input}
			for i := 1; i < 4; i++ {
				changes := map[string]any{"created_at": job.CreatedAt.Add(time.Duration(i) * 2 * time.Second), "available_at": job.AvailableAt.Add(time.Duration(i) * 2 * time.Second), "cooldown_key": string(app.NewID())}
				if differentAgents {
					peer := socialAgent(t, store, fmt.Sprintf("spend_peer%d", i), nil)
					p := eligibilityPolicy()
					p.DailyTokenBudget = budget
					encoded, _ := json.Marshal(p)
					generationSQL(t, store, `UPDATE agent_settings SET enabled=true,policy=$2 WHERE agent_id=$1`, peer.AgentID, encoded)
					changes["agent_id"] = peer.AgentID
				}
				other := contextJob(t, store, job, changes)
				built, _ := readGenerationContext(t, store, other)
				jobs, inputs = append(jobs, other), append(inputs, built)
			}
			var group sync.WaitGroup
			results := make(chan app.GenerationAdmission, 4)
			for i := range jobs {
				group.Add(1)
				go func() {
					defer group.Done()
					got, err := store.ReserveGeneration(context.Background(), jobs[i].ID, 1, inputs[i])
					if err != nil {
						t.Error(err)
					}
					results <- got
				}()
			}
			group.Wait()
			close(results)
			admitted := 0
			for result := range results {
				if result.Attempt != nil {
					admitted++
				} else if result.Reason != "budget_exhausted" {
					t.Errorf("unexpected denial: %+v", result)
				}
			}
			if admitted != want || spendAttemptCount(t, store) != want {
				t.Fatalf("admitted=%d want=%d", admitted, want)
			}
		})
	}
}

func TestGenerationSpendDenials(t *testing.T) {
	for _, why := range []string{"seed_budget", "pause", "disabled", "source", "stale", "lease"} {
		t.Run(why, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			want, status := "budget_exhausted", app.JobSkipped
			switch why {
			case "seed_budget":
				generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{daily_token_budget}','50000')`)
			case "pause":
				generationSQL(t, store, `UPDATE agent_settings SET enabled=false`)
				want, status = "policy_disabled", app.JobCancelled
			case "disabled":
				generationSQL(t, store, `UPDATE accounts SET disabled_at=clock_timestamp() WHERE id=$1`, job.AgentID)
				want, status = "policy_disabled", app.JobCancelled
			case "source":
				generationSQL(t, store, `UPDATE posts SET deleted_at=clock_timestamp() WHERE id=$1`, job.SourcePostID)
				want, status = "source_removed", app.JobCancelled
			case "stale", "lease":
				at := job.ExpiresAt
				if why == "lease" {
					at = *job.LeaseExpiresAt
				}
				got, err := store.reserveGeneration(context.Background(), job.ID, 1, input, &at)
				if why == "lease" {
					if !errors.Is(err, app.ErrConflict) || got.Attempt != nil {
						t.Fatalf("expired lease: %+v %v", got, err)
					}
					return
				}
				if err != nil || got.Reason != "stale_trigger" || got.Attempt != nil || claimJob(t, store, job.ID).ReasonCode != "stale_trigger" {
					t.Fatalf("stale: %+v %v", got, err)
				}
				return
			}
			got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
			if err != nil || got.Attempt != nil || got.Reason != want || claimJob(t, store, job.ID).Status != status || spendAttemptCount(t, store) != 0 {
				t.Fatalf("denial: %+v %v", got, err)
			}
		})
	}
}

func TestGenerationSpendRollback(t *testing.T) {
	for _, operation := range []string{"insert", "denial"} {
		for _, failure := range []string{"zero_rows", "after_write", "commit"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				budget := int64(500000)
				if operation == "denial" {
					budget = 50000
				}
				store, job, input := spendFixture(t, budget)
				table, event := "generation_attempts", "INSERT"
				if operation == "denial" {
					table, event = "generation_jobs", "UPDATE"
				}
				timing, body := "BEFORE", "RETURN NULL;"
				if failure != "zero_rows" {
					timing, body = "AFTER", "RAISE EXCEPTION 'secret failure detail';"
				}
				generationSQL(t, store, `CREATE FUNCTION reject_spend() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
				prefix, deferred := "", ""
				if failure == "commit" {
					prefix, deferred = "CONSTRAINT ", " DEFERRABLE INITIALLY DEFERRED"
				}
				generationSQL(t, store, `CREATE `+prefix+`TRIGGER reject_spend `+timing+` `+event+` ON `+table+deferred+` FOR EACH ROW EXECUTE FUNCTION reject_spend()`)
				got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input)
				if !errors.Is(err, app.ErrUnavailable) || got.Attempt != nil || got.Reason != "" || spendAttemptCount(t, store) != 0 || !sameClaimJob(job, claimJob(t, store, job.ID)) {
					t.Fatalf("rollback: %+v %v", got, err)
				}
			})
		}
	}
}

// Change only the clock projection; all admission statements run on real PG.
type spendClockQueryer struct {
	queryer
	clocks []time.Time
	calls  int
}

func (q *spendClockQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "clock_timestamp()") {
		at := q.clocks[min(q.calls, len(q.clocks)-1)]
		q.calls++
		return q.queryer.QueryRowContext(ctx, `SELECT $1::timestamptz`, at)
	}
	return q.queryer.QueryRowContext(ctx, query, args...)
}

func TestGenerationSpendMidnightAccounting(t *testing.T) {
	store, job, _ := spendFixture(t, 500000)
	start := time.Date(2026, 9, 25, 23, 59, 59, 0, time.UTC)
	job = contextJob(t, store, job, map[string]any{"created_at": start.Add(-time.Minute), "available_at": start.Add(-time.Minute), "expires_at": start.Add(time.Hour), "lease_expires_at": start.Add(time.Minute)})
	generationSQL(t, store, `UPDATE posts SET created_at=$2 WHERE id=$1`, job.SourcePostID, start.Add(-time.Minute))
	var input app.GenerationContext
	err := store.readSnapshot(context.Background(), func(q *Queries) error {
		q.queryer = contextClockQueryer{queryer: q.queryer, at: start}
		var err error
		input, err = q.generationContext(context.Background(), job.ID, 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = store.Transaction(context.Background(), func(q *Queries) error {
		q.queryer = &spendClockQueryer{queryer: q.queryer, clocks: []time.Time{start, start.Add(time.Second)}}
		_, err := q.reserveGeneration(context.Background(), job.ID, 1, input, nil)
		return err
	})
	if !errors.Is(err, app.ErrConflict) || spendAttemptCount(t, store) != 0 {
		t.Fatalf("midnight: %v", err)
	}
	start = start.Add(time.Second)
	got, err := store.reserveGeneration(context.Background(), job.ID, 1, input, &start)
	if err != nil || got.Attempt == nil || got.Attempt.BudgetDay != "2026-09-26" {
		t.Fatalf("new day: %+v %v", got, err)
	}
}
