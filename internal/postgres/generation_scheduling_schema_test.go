package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/migrations"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestGenerationSchemaChainLimits(t *testing.T) {
	store, persona, root := generationSetup(t)
	actor, post, _, _ := generationSocial(t, store, root)
	ctx := context.Background()
	for _, changes := range []map[string]any{
		{"max_chain_depth": -1}, {"max_chain_depth": 11}, {"max_chain_jobs": 0},
		{"max_chain_jobs": 101}, {"max_chain_jobs": 2},
	} {
		_, err := generationCloneJob(store, root.ID, changes)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23514" {
			t.Fatalf("chain bound accepted: %v: %v", changes, err)
		}
	}
	for _, field := range []string{"max_chain_depth", "max_chain_jobs"} {
		_, err := generationCloneJob(store, root.ID, map[string]any{field: nil})
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23502" {
			t.Fatalf("missing snapshot accepted: %s: %v", field, err)
		}
		generationReject(t, store, "23514", `UPDATE generation_jobs SET `+field+`=`+field+`+1 WHERE id=$1`, root.ID)
	}
	child := root
	child.ID, child.TriggerKey = app.NewID(), "child"
	child.TriggerKind, child.OutputKind, child.ChainDepth = app.TriggerContinuation, app.OutputReply, 1
	child.TriggerActorID, child.SourcePostID, child.CooldownKey = &actor, &post, "cooldown"
	if err := store.CreateGenerationJob(ctx, child); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GenerationJobByID(ctx, child.ID)
	if err != nil || saved.MaxChainDepth != root.MaxChainDepth || saved.MaxChainJobs != root.MaxChainJobs {
		t.Fatalf("child snapshot round trip: %+v %v", saved, err)
	}
	for _, changes := range []map[string]any{
		{"max_chain_depth": 3}, {"max_chain_jobs": 6}, {"max_chain_jobs": 4}, {"chain_depth": 3},
	} {
		changes["root_job_id"] = root.ID
		_, err := generationCloneJob(store, child.ID, changes)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23514" {
			t.Fatalf("child snapshot mismatch accepted: %v: %v", changes, err)
		}
	}
	// Increasing mutable policy cannot change old roots or authorize different
	// snapshots on their children. A stricter child policy is checked later.
	settings := app.AgentSettings{AgentID: persona.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: root.CreatedAt}
	if _, err := store.InitializeAgentSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(jsonb_set(policy,'{max_chain_depth}','10'),'{max_chain_jobs}','100')`)
	child.ID, child.TriggerKey, child.MaxChainDepth, child.MaxChainJobs = app.NewID(), "expanded", 10, 100
	if err := store.CreateGenerationJob(ctx, child); !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("mutable policy expanded existing chain: %v", err)
	}
	for _, id := range []app.ID{root.ID, saved.ID} {
		generationReject(t, store, "23514", `UPDATE generation_jobs SET max_chain_jobs=4 WHERE id=$1`, id)
		generationReject(t, store, "23514", `DELETE FROM generation_jobs WHERE id=$1`, id)
	}
}

// Create actual 003 records without relying on the new job write mapping.
func schedulingLegacySetup(t *testing.T) (*Store, app.GenerationJob) {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, catalog[:3]); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	root := app.GenerationJob{ID: app.NewID(), AgentID: app.NewID(), CreatedAt: now}
	generationSQL(t, store, `INSERT INTO accounts(id,type,handle,display_name,created_at,updated_at) VALUES($1,'agent','legacy_agent','Legacy',$2,$2)`, root.AgentID, now)
	if err := store.CreatePersona(ctx, app.Persona{AgentID: root.AgentID, Version: 1, Instructions: "Retain legacy persona.", TopicTags: []string{"go"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	generationSQL(t, store, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,output_kind,root_job_id,chain_depth,status,available_at,expires_at,lease_version,created_at)
		VALUES($1,$2,1,'scheduled','legacy','post',$1,0,'pending',$3::timestamptz,$3::timestamptz+interval '1 hour',0,$3::timestamptz)`, root.ID, root.AgentID, now)
	return store, root
}

func TestMigrateSchedulingFromGeneration(t *testing.T) {
	store, root := schedulingLegacySetup(t)
	ctx := context.Background()
	actor, post, _, _ := generationSocial(t, store, root)
	child, err := generationCloneJob(store, root.ID, map[string]any{
		"trigger_kind": "continuation", "trigger_actor_id": actor, "source_post_id": post,
		"cooldown_key": "legacy-cooldown", "output_kind": "reply", "root_job_id": root.ID, "chain_depth": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	lone, err := generationCloneJob(store, root.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Backfill terminal history without losing its immutability or provenance.
	generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='source_removed',finished_at=created_at WHERE id=$1`, root.ID)
	var before, after string
	if err := store.db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id)::text FROM generation_jobs j`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("003 readiness: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(j)-'max_chain_depth'-'max_chain_jobs' ORDER BY id)::text FROM generation_jobs j`).Scan(&after); err != nil || after != before {
		t.Fatalf("migration changed retained history: %v", err)
	}
	for _, id := range []app.ID{root.ID, child, lone} {
		job, err := store.GenerationJobByID(ctx, id)
		depth, jobs := 1, 2
		if id == lone {
			depth, jobs = 0, 1
		}
		if err != nil || job.MaxChainDepth != depth || job.MaxChainJobs != jobs {
			t.Fatalf("backfill: %+v %v", job, err)
		}
		generationReject(t, store, "23514", `UPDATE generation_jobs SET max_chain_jobs=max_chain_jobs+1 WHERE id=$1`, id)
		generationReject(t, store, "23514", `UPDATE generation_jobs SET expires_at=expires_at+interval '1 hour' WHERE id=$1`, id)
	}
	// Repeat migration does not replace snapshots or re-enable mutable history.
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	generationReject(t, store, "23514", `UPDATE generation_jobs SET reason_code='changed' WHERE id=$1`, root.ID)
}

func TestMigrateSchedulingRejectsMalformedLegacyChain(t *testing.T) {
	store, root := schedulingLegacySetup(t)
	actor, post, _, _ := generationSocial(t, store, root)
	// 003 accepted a depth-10 child without the intermediate history. Do not
	// invent allowance to make such a sparse chain fit the new domain contract.
	if _, err := generationCloneJob(store, root.ID, map[string]any{
		"trigger_kind": "continuation", "trigger_actor_id": actor, "source_post_id": post,
		"cooldown_key": "legacy-cooldown", "output_kind": "reply", "root_job_id": root.ID, "chain_depth": 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err == nil {
		t.Fatal("accepted malformed legacy chain")
	}
	var columns, version int
	if err := store.db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='generation_jobs' AND column_name='max_chain_jobs'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("partial schema: %d %v", columns, err)
	}
	if err := store.db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("partial history: %d %v", version, err)
	}
	generationReject(t, store, "23514", `UPDATE generation_jobs SET expires_at=expires_at+interval '1 hour' WHERE id=$1`, root.ID)
}
