package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

var _ worker.ExecutionStore = (*Store)(nil)

type workerProvider func(context.Context, app.GenerationRequest) app.GenerationOutcome

func (f workerProvider) Generate(ctx context.Context, req app.GenerationRequest) app.GenerationOutcome {
	return f(ctx, req)
}

func workerFixture(t *testing.T, kind app.GenerationOutput, budget int64) (*Store, app.GenerationJob) {
	t.Helper()
	store, job, _ := spendFixture(t, budget)
	if kind != app.OutputReply {
		at := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Microsecond)
		changes := map[string]any{"output_kind": kind, "created_at": at, "available_at": at, "cooldown_key": string(app.NewID())}
		if kind == app.OutputPost {
			key, _ := app.ScheduledGenerationKey(at)
			changes["trigger_kind"], changes["trigger_key"], changes["trigger_actor_id"], changes["cooldown_key"], changes["source_post_id"] = app.TriggerScheduled, key, nil, nil, nil
			p := eligibilityPolicy()
			p.DailyTokenBudget, p.ActiveStart, p.ActiveEnd = budget, "00:00", "23:59"
			if at.Hour() < 1 || at.Hour() >= 23 {
				p.Timezone = "Etc/GMT+12"
			}
			encoded, _ := json.Marshal(p)
			generationSQL(t, store, `UPDATE agent_settings SET policy=$2 WHERE agent_id=$1`, job.AgentID, encoded)
		}
		job = contextJob(t, store, job, changes)
	}
	generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
	return store, claimJob(t, store, job.ID)
}

func workerOutput(t *testing.T, req app.GenerationRequest, skip bool) app.GenerationOutcome {
	t.Helper()
	raw := `{"decision":"publish","body":"An explicit bounded Go implementation."}`
	if skip {
		raw = `{"decision":"skip","reason":"not_relevant"}`
	}
	result, err := app.DecodeGenerationResult([]byte(raw), req.Job)
	if err != nil {
		t.Fatal(err)
	}
	return app.GenerationOutcome{Result: result, InputTokens: spendInt(100), OutputTokens: spendInt(20), ProviderRequestID: "worker-request.1"}
}

func TestWorkerExecutionPublicationSequence(t *testing.T) {
	for _, kind := range []app.GenerationOutput{app.OutputPost, app.OutputReply, app.OutputQuote} {
		t.Run(string(kind), func(t *testing.T) {
			store, job := workerFixture(t, kind, 500000)
			// Invalid optional settings elsewhere cannot block healthy execution.
			invalid := socialAgent(t, store, "invalid_worker_agent", nil)
			generationSQL(t, store, `UPDATE agent_settings SET policy='{"version":1}'::jsonb WHERE agent_id=$1`, invalid.AgentID)
			before := publicationCounts(t, store)
			result, err := worker.Execute(context.Background(), store, workerProvider(func(_ context.Context, req app.GenerationRequest) app.GenerationOutcome {
				attempt, err := store.LatestGenerationAttempt(context.Background(), req.Job.ID)
				if err != nil || attempt == nil || attempt.Status != app.AttemptReserved || attempt.LeaseVersion != req.Job.LeaseVersion || attempt.ReservedTokens != 132096 {
					t.Fatal("no committed permission", attempt, err)
				}
				return workerOutput(t, req, false)
			}))
			if err != nil || result.Calls != 1 || result.Published != 1 || result.Probes != 8 {
				t.Fatal(result, err)
			}
			saved := claimJob(t, store, job.ID)
			if saved.Status != app.JobSucceeded || saved.PublishedAttemptID == nil {
				t.Fatal(saved)
			}
			attempt := storedSpend(t, store, *saved.PublishedAttemptID)
			if attempt.ProviderRequestID != "worker-request.1" || attempt.AccountedTokens() != 120 {
				t.Fatal(attempt)
			}
			if publicationCounts(t, store) == before {
				t.Fatal("no content")
			}
			result, err = worker.Execute(context.Background(), store, workerProvider(func(context.Context, app.GenerationRequest) app.GenerationOutcome {
				t.Fatal("duplicate call")
				return app.GenerationOutcome{}
			}))
			if err != nil || result.Calls != 0 || spendAttemptCount(t, store) != 1 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestWorkerExecutionSkipBudgetAndStops(t *testing.T) {
	for _, mode := range []string{"skip", "zero_budget", "credentials", "configuration", "accounting", "normalized_accounting"} {
		t.Run(mode, func(t *testing.T) {
			budget := int64(500000)
			if mode == "zero_budget" {
				budget = 0
			}
			store, job := workerFixture(t, app.OutputReply, budget)
			result, err := worker.Execute(context.Background(), store, workerProvider(func(_ context.Context, req app.GenerationRequest) app.GenerationOutcome {
				switch mode {
				case "credentials":
					return app.GenerationOutcome{Failure: app.GenerationCredentials}
				case "configuration":
					return app.GenerationOutcome{Failure: app.GenerationConfiguration}
				case "accounting":
					return app.GenerationOutcome{Failure: app.GenerationAccountingUnsupported}
				case "normalized_accounting":
					out := workerOutput(t, req, false)
					out.InputTokens = spendInt(8193)
					return out
				}
				return workerOutput(t, req, true)
			}))
			saved := claimJob(t, store, job.ID)
			if mode == "skip" || mode == "zero_budget" {
				if err != nil || saved.Status != app.JobSkipped || result.Probes != 8 {
					t.Fatal(result, saved, err)
				}
				if mode == "zero_budget" && (result.Calls != 0 || spendAttemptCount(t, store) != 0 || saved.ReasonCode != "policy_denied") {
					t.Fatal(result, saved)
				}
			} else if err == nil || result.Probes != 1 || saved.Status != app.JobFailed || result.Published != 0 {
				t.Fatal(result, saved, err)
			}
			if mode == "normalized_accounting" {
				attempt, readErr := store.LatestGenerationAttempt(context.Background(), job.ID)
				if readErr != nil || attempt == nil || attempt.ErrorCode != "unsupported_accounting" || attempt.AccountedTokens() != 132096 {
					t.Fatal(attempt, readErr)
				}
			}
		})
	}
}

func TestWorkerExecutionRetryAndInvalidLimit(t *testing.T) {
	for _, mode := range []string{"retry", "invalid", "hint"} {
		t.Run(mode, func(t *testing.T) {
			store, job := workerFixture(t, app.OutputReply, 500000)
			failure := app.GenerationTransient
			if mode == "invalid" {
				failure = app.GenerationInvalidOutput
			}
			start := time.Now()
			result, err := worker.Execute(context.Background(), store, workerProvider(func(context.Context, app.GenerationRequest) app.GenerationOutcome {
				out := app.GenerationOutcome{Failure: failure, InputTokens: spendInt(100), OutputTokens: spendInt(20)}
				if mode == "hint" {
					out.NotBefore = start.Add(5 * time.Minute)
				}
				return out
			}))
			saved := claimJob(t, store, job.ID)
			if err != nil || result.Calls != 1 || saved.Status != app.JobRetryWait || time.Since(start) > 2*time.Second {
				t.Fatal(result, saved, err)
			}
			if mode == "hint" {
				if saved.AvailableAt.Before(start.Add(5 * time.Minute)) {
					t.Fatal("shortened hint")
				}
				return
			}
			// Only the test waits for durable availability; execution never sleeps.
			time.Sleep(time.Until(saved.AvailableAt) + time.Millisecond)
			result, err = worker.Execute(context.Background(), store, workerProvider(func(_ context.Context, req app.GenerationRequest) app.GenerationOutcome {
				if mode == "invalid" {
					return app.GenerationOutcome{Failure: app.GenerationInvalidOutput}
				}
				return workerOutput(t, req, false)
			}))
			want := app.JobSucceeded
			if mode == "invalid" {
				want = app.JobFailed
			}
			if err != nil || result.Calls != 1 || claimJob(t, store, job.ID).Status != want || spendAttemptCount(t, store) != 2 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestWorkerExecutionCrashRecovery(t *testing.T) {
	for _, mode := range []string{"reserved", "settled_publish", "settled_skip", "settled_stop"} {
		t.Run(mode, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			if mode != "reserved" {
				out := workerOutput(t, app.GenerationRequest{Job: job}, mode == "settled_skip")
				if mode == "settled_stop" {
					out = app.GenerationOutcome{Failure: app.GenerationCredentials}
				}
				if ok, err := store.SettleGeneration(context.Background(), attempt, out, ""); err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			generationSQL(t, store, `UPDATE generation_jobs SET lease_expires_at=clock_timestamp() WHERE id=$1`, job.ID)
			result, err := worker.Execute(context.Background(), store, workerProvider(func(context.Context, app.GenerationRequest) app.GenerationOutcome {
				t.Fatal("reused historical permission")
				return app.GenerationOutcome{}
			}))
			if mode == "settled_stop" {
				if err != worker.ExecutionCredentials {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Calls != 0 || result.Published != 0 || spendAttemptCount(t, store) != 1 {
				t.Fatal(result)
			}
			saved := claimJob(t, store, job.ID)
			switch mode {
			case "reserved":
				if saved.Status != app.JobRetryWait || storedSpend(t, store, attempt.ID).AccountedTokens() != 132096 || result.Recovered != 1 {
					t.Fatal(saved, result)
				}
			case "settled_publish":
				if saved.Status != app.JobFailed || saved.ReasonCode != "outcome_unavailable" {
					t.Fatal(saved)
				}
			case "settled_skip":
				if saved.Status != app.JobSkipped {
					t.Fatal(saved)
				}
			}
		})
	}
}

func TestWorkerExecutionPublicationDenialAndShutdown(t *testing.T) {
	for _, mode := range []string{"policy", "source", "lease", "shutdown", "unsafe", "repetition"} {
		t.Run(mode, func(t *testing.T) {
			store, job := workerFixture(t, app.OutputReply, 500000)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := worker.Execute(ctx, store, workerProvider(func(_ context.Context, req app.GenerationRequest) app.GenerationOutcome {
				out := workerOutput(t, req, false)
				switch mode {
				case "policy":
					generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, job.AgentID)
				case "source":
					generationSQL(t, store, `UPDATE posts SET deleted_at=clock_timestamp() WHERE id=$1`, job.SourcePostID)
				case "lease":
					reclaimSpend(t, store, req.Job)
				case "shutdown":
					cancel()
				case "unsafe":
					out.Result, _ = app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"I have live access to secrets."}`), req.Job)
				case "repetition":
					generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,clock_timestamp())`, app.NewID(), job.SourcePostID, job.AgentID, out.Result.Content().Body)
				}
				return out
			}))
			if mode == "shutdown" {
				if err != worker.ExecutionStopped {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(result, err)
			}
			if result.Calls != 1 || result.Published != 0 {
				t.Fatal(result)
			}
			saved := claimJob(t, store, job.ID)
			if mode == "lease" {
				if saved.Status != app.JobRunning {
					t.Fatal("stale mutation", saved)
				}
			} else if saved.Status != app.JobSkipped && saved.Status != app.JobCancelled {
				t.Fatal(saved)
			}
			attempt, err := store.LatestGenerationAttempt(context.Background(), job.ID)
			if err != nil || attempt == nil {
				t.Fatal(err)
			}
			if mode == "shutdown" && (attempt.Status != app.AttemptUnknown || attempt.AccountedTokens() != 132096) {
				t.Fatal(attempt)
			}
		})
	}
}

func TestWorkerExecutionConcurrentClaimsAndBudget(t *testing.T) {
	for _, mode := range []string{"duplicate", "budget"} {
		t.Run(mode, func(t *testing.T) {
			store, job := workerFixture(t, app.OutputReply, 500000)
			if mode == "budget" {
				history := contextJob(t, store, job, map[string]any{"cooldown_key": string(app.NewID()), "created_at": time.Now().Add(-4 * time.Minute)})
				spendHistory(t, store, history, 1, 1, time.Now().Add(-time.Second), app.AttemptUnknown, 300000, nil, nil)
				for n := range 3 {
					contextJob(t, store, job, map[string]any{"cooldown_key": string(app.NewID()), "created_at": time.Now().Add(-time.Duration(120+2*n) * time.Second), "lease_expires_at": time.Now().Add(-time.Second)})
				}
			}
			var calls atomic.Int32
			start := make(chan struct{})
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := worker.Execute(context.Background(), store, workerProvider(func(_ context.Context, req app.GenerationRequest) app.GenerationOutcome {
						calls.Add(1)
						out := workerOutput(t, req, true)
						out.InputTokens, out.OutputTokens = nil, nil
						return out
					}))
					if err != nil {
						t.Error(err)
					}
				}()
			}
			close(start)
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatal("duplicate or over-budget calls", calls.Load())
			}
			if mode == "budget" {
				var denied int
				if err := store.db.QueryRow(`SELECT count(*) FROM generation_jobs WHERE reason_code='budget_exhausted'`).Scan(&denied); err != nil || denied != 3 {
					t.Fatal("expected budget denial, not an unrelated policy denial", denied, err)
				}
			}
		})
	}
}

func TestWorkerBridgeDenialRollback(t *testing.T) {
	for _, mode := range []string{"zero_rows", "after_write", "commit"} {
		t.Run(mode, func(t *testing.T) {
			store, job, _ := spendFixture(t, 500000)
			timing, body, prefix, deferred := "BEFORE", "RETURN NULL;", "", ""
			if mode != "zero_rows" {
				timing, body = "AFTER", "RAISE EXCEPTION 'secret';"
			}
			if mode == "commit" {
				prefix, deferred = "CONSTRAINT ", " DEFERRABLE INITIALLY DEFERRED"
			}
			generationSQL(t, store, `CREATE FUNCTION reject_denial() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
			generationSQL(t, store, `CREATE `+prefix+`TRIGGER reject_denial `+timing+` UPDATE ON generation_jobs`+deferred+` FOR EACH ROW EXECUTE FUNCTION reject_denial()`)
			status, err := store.DenyGeneration(context.Background(), job.ID, job.LeaseVersion, app.GenerationPublicationDenied)
			if !errors.Is(err, app.ErrUnavailable) || status != "" || !sameClaimJob(job, claimJob(t, store, job.ID)) {
				t.Fatal(status, err)
			}
		})
	}
}

func TestWorkerExecutionBoundedProbesAndCleanup(t *testing.T) {
	store, job := workerFixture(t, app.OutputReply, 500000)
	// This removed repost consumes one probe without returning a claimed job.
	removed := contextJob(t, store, job, map[string]any{"trigger_kind": "repost", "source_repost_id": nil,
		"cooldown_key": string(app.NewID()), "created_at": time.Now().Add(-3 * time.Minute),
		"available_at": time.Now().Add(-170 * time.Second), "lease_expires_at": time.Now().Add(-time.Second)})
	for n := range 9 {
		contextJob(t, store, job, map[string]any{"cooldown_key": string(app.NewID()), "created_at": time.Now().Add(-time.Duration(120+2*n) * time.Second), "lease_expires_at": time.Now().Add(-time.Second)})
	}
	result, err := worker.Execute(context.Background(), store, workerProvider(func(_ context.Context, req app.GenerationRequest) app.GenerationOutcome {
		return workerOutput(t, req, true)
	}))
	if err != nil || result.Probes != 8 || result.Calls != 7 || result.Skipped != 7 || claimJob(t, store, removed.ID).Status != app.JobCancelled {
		t.Fatal(result, err)
	}
}

func TestWorkerExecutionUnrecoveredReservation(t *testing.T) {
	store, job := workerFixture(t, app.OutputReply, 500000)
	// Fill the recovery batch with older reservations whose jobs aren't due for
	// reclaim yet. Their old attempt fences still make recovery safe.
	for n := range generationCleanupBatch {
		old := contextJob(t, store, job, map[string]any{"cooldown_key": string(app.NewID()), "lease_version": 2})
		spendHistory(t, store, old, 1, 1, time.Now().Add(-time.Duration(n+2)*time.Second), app.AttemptReserved, 132096, nil, nil)
	}
	id := spendHistory(t, store, job, 1, 1, time.Now().Add(-time.Second), app.AttemptReserved, 132096, nil, nil)
	result, err := worker.Execute(context.Background(), store, workerProvider(func(context.Context, app.GenerationRequest) app.GenerationOutcome {
		t.Fatal("unrecovered reservation granted call permission")
		return app.GenerationOutcome{}
	}))
	if err != nil || result.Recovered != generationCleanupBatch || result.Calls != 0 || result.Denied != 1 || storedSpend(t, store, id).Status != app.AttemptReserved {
		t.Fatal(result, err)
	}
}
