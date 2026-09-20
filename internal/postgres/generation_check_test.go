package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

func TestGenerationCheckReadOnly(t *testing.T) {
	store, fixture := seededStore(t)
	ctx := context.Background()
	for _, stage := range []string{"empty", "seeded", "enabled"} {
		want := worker.Summary{}
		switch stage {
		case "seeded":
			if err := store.SeedDemo(ctx); err != nil {
				t.Fatal(err)
			}
			want.Configured = 2
		case "enabled":
			generationSQL(t, store, `UPDATE agent_settings SET enabled=true WHERE agent_id=$1`, fixture.personas[0].AgentID)
			want = worker.Summary{Configured: 2, Enabled: 1}
		}
		before := seedSnapshot(t, store)
		got, err := worker.Check(ctx, store)
		if err != nil || got != want {
			t.Fatalf("%s: got %+v %v", stage, got, err)
		}
		if seedSnapshot(t, store) != before {
			t.Fatal("check wrote data")
		}
	}
	// The actual DB queries must work even when PostgreSQL prohibits writes.
	store.db.SetMaxOpenConns(1)
	generationSQL(t, store, `SET default_transaction_read_only=on`)
	defer generationSQL(t, store, `SET default_transaction_read_only=off`)
	if _, err := worker.Check(ctx, store); err != nil {
		t.Fatalf("read-only database: %v", err)
	}
}

func TestGenerationCheckInvalidConfiguration(t *testing.T) {
	for _, kind := range []string{"policy missing", "policy unknown", "policy bounds", "policy oversized", "selected persona", "human", "schedule"} {
		t.Run(kind, func(t *testing.T) {
			store, fixture := seededStore(t)
			ctx := context.Background()
			if err := store.SeedDemo(ctx); err != nil {
				t.Fatal(err)
			}
			id := fixture.personas[0].AgentID
			switch kind {
			case "policy missing":
				generationSQL(t, store, `UPDATE agent_settings SET policy='{"version":1}' WHERE agent_id=$1`, id)
			case "policy unknown":
				generationSQL(t, store, `UPDATE agent_settings SET policy=policy || '{"secret":"never expose"}' WHERE agent_id=$1`, id)
			case "policy bounds":
				generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{daily_token_budget}','-1') WHERE agent_id=$1`, id)
			case "policy oversized":
				generationSQL(t, store, `UPDATE agent_settings SET policy=policy || jsonb_build_object('secret',repeat('private',2000)) WHERE agent_id=$1`, id)
			case "selected persona":
				generationSQL(t, store, `INSERT INTO agent_personas VALUES($1,2,'private instructions',ARRAY['Bad Tag'],now())`, id)
				generationSQL(t, store, `UPDATE agent_settings SET persona_version=2 WHERE agent_id=$1`, id)
			case "human":
				generationSQL(t, store, `UPDATE accounts SET type='human' WHERE id=$1`, id)
			case "schedule":
				generationSQL(t, store, `UPDATE agent_settings SET next_post_at='2026-09-21 10:00Z',schedule_date='2026-09-22',remaining_slots=1 WHERE agent_id=$1`, id)
			}
			before := seedSnapshot(t, store)
			got, err := worker.Check(ctx, store)
			if !errors.Is(err, app.ErrUnavailable) || got != (worker.Summary{}) {
				t.Fatalf("got %+v %v", got, err)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatal("unsafe configuration error")
			}
			if seedSnapshot(t, store) != before {
				t.Fatal("failed check wrote data")
			}
		})
	}
}

func TestGenerationCheckSchemaAndCancellation(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := worker.Check(ctx, store); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("empty schema: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	generationSQL(t, store, `UPDATE schema_migrations SET checksum=repeat('0',64) WHERE version=3`)
	if _, err := worker.Check(ctx, store); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("mismatched schema: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := store.CheckGenerationConfiguration(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
	// Cancellation while a query is blocked must also unwind its read snapshot.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE agent_settings IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if _, _, err := store.CheckGenerationConfiguration(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked read: %v", err)
	}
}

func TestGenerationCheckBoundedFleet(t *testing.T) {
	store, _ := seededStore(t)
	ctx := context.Background()
	policy, err := json.Marshal(generationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	generationSQL(t, store, `INSERT INTO accounts(id,type,handle,display_name,created_at,updated_at)
		SELECT md5(n::text)::uuid,'agent','agent_'||n,'Agent',now(),now() FROM generate_series(1,$1) n`, maxConfiguredAgents+1)
	generationSQL(t, store, `INSERT INTO agent_personas SELECT id,1,'Technical AI persona',ARRAY['go'],created_at FROM accounts`)
	generationSQL(t, store, `INSERT INTO agent_settings(agent_id,persona_version,enabled,policy,remaining_slots,updated_at)
		SELECT id,1,false,$1,0,created_at FROM accounts WHERE handle<>'agent_1001'`, policy)
	configured, enabled, err := store.CheckGenerationConfiguration(ctx)
	if err != nil || configured != maxConfiguredAgents || enabled != 0 {
		t.Fatalf("boundary: %d %d %v", configured, enabled, err)
	}
	generationSQL(t, store, `INSERT INTO agent_settings(agent_id,persona_version,enabled,policy,remaining_slots,updated_at)
		SELECT id,1,false,$1,0,created_at FROM accounts WHERE handle='agent_1001'`, policy)
	configured, enabled, err = store.CheckGenerationConfiguration(ctx)
	if err == nil || !strings.Contains(err.Error(), "exceeds 1000") || configured != 0 || enabled != 0 {
		t.Fatalf("over bound: %d %d %v", configured, enabled, err)
	}
}
