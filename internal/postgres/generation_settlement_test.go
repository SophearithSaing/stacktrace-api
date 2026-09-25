package postgres

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func settlementOutcome(t *testing.T, job app.GenerationJob) app.GenerationOutcome {
	t.Helper()
	result, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A practical Go example."}`), job)
	if err != nil {
		t.Fatal(err)
	}
	input, output := int64(8192), int64(1024)
	return app.GenerationOutcome{Result: result, InputTokens: &input, OutputTokens: &output}
}

func storedSpend(t *testing.T, store *Store, id app.ID) app.GenerationAttempt {
	t.Helper()
	a, err := store.GenerationAttemptByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGenerationSettlementExactRepeatAndConflict(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	outcome := settlementOutcome(t, job)
	// PostgreSQL may decode Local while the reservation returned UTC. Equality is
	// by instant, not by Go's location/monotonic implementation details.
	attempt.StartedAt = attempt.StartedAt.In(time.FixedZone("other", -7*3600))
	for range 2 {
		if ok, err := store.SettleGeneration(context.Background(), attempt, outcome, "req-1"); err != nil || !ok {
			t.Fatalf("settle: %v %v", ok, err)
		}
	}
	saved := storedSpend(t, store, attempt.ID)
	if saved.Status != app.AttemptSucceeded || saved.OutputDigest != outcome.Result.Digest() || saved.AccountedTokens() != 9216 || saved.ProviderRequestID != "req-1" {
		t.Fatalf("settled: %+v", saved)
	}
	*outcome.InputTokens = 1
	for _, conflict := range []app.GenerationOutcome{outcome, {Failure: app.GenerationTimeout}, {Failure: app.GenerationTransient}} {
		if ok, err := store.SettleGeneration(context.Background(), attempt, conflict, "req-1"); err != nil || ok {
			t.Fatalf("conflict: %v %v", ok, err)
		}
	}
	if storedSpend(t, store, attempt.ID).AccountedTokens() != 9216 || !sameClaimJob(job, claimJob(t, store, job.ID)) {
		t.Fatal("alias or job authority changed")
	}
	if got, err := store.ReserveGeneration(context.Background(), job.ID, 1, input); err != nil || got.Attempt != nil || got.Reason != "already_admitted" {
		t.Fatalf("settlement replay authorized call: %+v %v", got, err)
	}
}

func TestGenerationSettlementReservationIdentity(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	for name, mutate := range map[string]func(*app.GenerationAttempt){
		"job":      func(a *app.GenerationAttempt) { a.JobID = app.NewID() },
		"lease":    func(a *app.GenerationAttempt) { a.LeaseVersion++ },
		"number":   func(a *app.GenerationAttempt) { a.AttemptNumber++ },
		"model":    func(a *app.GenerationAttempt) { a.Model = "other" },
		"provider": func(a *app.GenerationAttempt) { a.Provider = "other" },
		"builder":  func(a *app.GenerationAttempt) { a.ContextBuilderVersion = "other" },
		"hash":     func(a *app.GenerationAttempt) { a.ContextHash = strings.Repeat("f", 64) },
		"reserve":  func(a *app.GenerationAttempt) { a.ReservedTokens++ },
		"started":  func(a *app.GenerationAttempt) { a.StartedAt = a.StartedAt.Add(time.Microsecond) },
	} {
		t.Run(name, func(t *testing.T) {
			altered := attempt
			mutate(&altered)
			if ok, err := store.SettleGeneration(context.Background(), altered, settlementOutcome(t, job), "req"); !errors.Is(err, app.ErrConflict) || ok {
				t.Fatalf("identity: %v %v", ok, err)
			}
		})
	}
	if storedSpend(t, store, attempt.ID).Status != app.AttemptReserved {
		t.Fatal("identity mismatch settled")
	}
	wrongKind := job
	wrongKind.OutputKind = app.OutputQuote
	if ok, err := store.SettleGeneration(context.Background(), attempt, settlementOutcome(t, wrongKind), "req"); err == nil || ok {
		t.Fatalf("wrong kind: %v %v", ok, err)
	}
	for _, id := range []string{" ", "request\nsecret", strings.Repeat("a", 257), "réquest"} {
		if ok, err := store.SettleGeneration(context.Background(), attempt, settlementOutcome(t, job), id); err == nil || ok {
			t.Fatalf("unsafe id: %v %v", ok, err)
		}
	}
}

func TestGenerationSettlementConservativeUsage(t *testing.T) {
	for _, test := range []struct {
		name    string
		in, out *int64
		failure app.GenerationFailure
		status  app.GenerationAttemptStatus
		code    string
	}{
		{"missing", nil, nil, "", app.AttemptSucceeded, ""},
		{"partial", spendInt(100), nil, "", app.AttemptSucceeded, ""},
		{"zero", spendInt(0), spendInt(0), "", app.AttemptSucceeded, ""},
		{"negative", spendInt(-1), spendInt(1), "", app.AttemptSucceeded, ""},
		{"negative_output", spendInt(1), spendInt(-1), "", app.AttemptSucceeded, ""},
		{"over_input", spendInt(8193), spendInt(0), "", app.AttemptFailed, "unsupported_accounting"},
		{"over_output", spendInt(1), spendInt(1025), "", app.AttemptFailed, "unsupported_accounting"},
		{"overflow", spendInt(math.MaxInt64), spendInt(math.MaxInt64), "", app.AttemptFailed, "unsupported_accounting"},
		{"partial_overflow", nil, spendInt(math.MaxInt64), "", app.AttemptFailed, "unsupported_accounting"},
		{"timeout", spendInt(100), spendInt(1), app.GenerationTimeout, app.AttemptUnknown, "provider_timeout"},
		{"cancel", spendInt(math.MaxInt64), spendInt(1), app.GenerationCancelled, app.AttemptUnknown, "execution_cancelled"},
		{"unsupported", spendInt(100), spendInt(1), app.GenerationAccountingUnsupported, app.AttemptFailed, "unsupported_accounting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			outcome := settlementOutcome(t, job)
			outcome.InputTokens, outcome.OutputTokens = test.in, test.out
			if test.failure != "" {
				outcome.Result, outcome.Failure = app.GenerationResult{}, test.failure
			}
			for range 2 {
				if ok, err := store.SettleGeneration(context.Background(), attempt, outcome, ""); err != nil || !ok {
					t.Fatalf("settle: %v %v", ok, err)
				}
			}
			saved := storedSpend(t, store, attempt.ID)
			if saved.Status != test.status || saved.ErrorCode != test.code || saved.InputTokens != nil || saved.OutputTokens != nil || saved.AccountedTokens() != 132096 || (saved.OutputDigest != "") != (test.status == app.AttemptSucceeded) {
				t.Fatalf("usage: %+v", saved)
			}
			if test.status != app.AttemptSucceeded {
				if ok, err := store.SettleGeneration(context.Background(), attempt, settlementOutcome(t, job), ""); err != nil || ok {
					t.Fatalf("late success: %v %v", ok, err)
				}
			}
		})
	}
}

func spendInt(value int64) *int64 { return &value }

func TestGenerationSettlementRollback(t *testing.T) {
	for _, failure := range []string{"zero_rows", "after_write", "commit"} {
		t.Run(failure, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			timing, body, prefix, deferred := "BEFORE", "RETURN NULL;", "", ""
			if failure != "zero_rows" {
				timing, body = "AFTER", "RAISE EXCEPTION 'secret provider text';"
			}
			if failure == "commit" {
				prefix, deferred = "CONSTRAINT ", " DEFERRABLE INITIALLY DEFERRED"
			}
			generationSQL(t, store, `CREATE FUNCTION reject_settlement() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
			generationSQL(t, store, `CREATE `+prefix+`TRIGGER reject_settlement `+timing+` UPDATE ON generation_attempts`+deferred+` FOR EACH ROW EXECUTE FUNCTION reject_settlement()`)
			if ok, err := store.SettleGeneration(context.Background(), attempt, settlementOutcome(t, job), "req"); !errors.Is(err, app.ErrUnavailable) || ok {
				t.Fatalf("rollback: %v %v", ok, err)
			}
			if storedSpend(t, store, attempt.ID).Status != app.AttemptReserved {
				t.Fatal("failed settlement committed")
			}
		})
	}
}

func TestGenerationSettlementNeverLocksJob(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	attempt := admitSpend(t, store, job, input)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, query := range []string{`SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`} {
		id := job.AgentID
		if strings.Contains(query, "generation_jobs") {
			id = job.ID
		}
		if _, err := tx.Exec(query, id); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if ok, err := store.SettleGeneration(ctx, attempt, settlementOutcome(t, job), "req"); err != nil || !ok {
		t.Fatalf("job lock dependence: %v %v", ok, err)
	}
}
