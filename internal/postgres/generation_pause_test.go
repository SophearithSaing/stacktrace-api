package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/migrations"
)

func TestGenerationPauseRevisionAuthority(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	attempt := admitSpend(t, store, job, input)
	if attempt.PauseRevision == nil || *attempt.PauseRevision != 0 {
		t.Fatal("missing admission revision", attempt.PauseRevision)
	}
	// Model a completed pause/resume between admission and settlement.
	generationSQL(t, store, `UPDATE agent_settings SET enabled=false,pause_revision=pause_revision+1 WHERE agent_id=$1`, job.AgentID)
	generationSQL(t, store, `UPDATE agent_settings SET enabled=true WHERE agent_id=$1`, job.AgentID)
	output, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A late result"}`), job)
	if err != nil {
		t.Fatal(err)
	}
	forged := attempt
	revision := int64(1)
	forged.PauseRevision = &revision
	if _, err := store.SettleGeneration(ctx, forged, app.GenerationOutcome{Result: output}, ""); !errors.Is(err, app.ErrConflict) {
		t.Fatal("forged reservation accepted", err)
	}
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Result: output}, ""); err != nil || !ok {
		t.Fatal("late settlement denied", ok, err)
	}
	before := publicationCounts(t, store)
	if _, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output); !errors.Is(err, app.ErrForbidden) {
		t.Fatal("stale pause revision published", err)
	}
	if publicationCounts(t, store) != before || storedSpend(t, store, attempt.ID).AccountedTokens() != attempt.ReservedTokens {
		t.Fatal("pause changed content or refunded uncertain usage")
	}
	generationReject(t, store, "23514", `UPDATE generation_attempts SET pause_revision=1 WHERE id=$1`, attempt.ID)
	generationReject(t, store, "23514", `UPDATE agent_settings SET pause_revision=-1 WHERE agent_id=$1`, job.AgentID)
}

func TestMigrateOperatorControlsPreservesHistory(t *testing.T) {
	store, root := schedulingLegacySetup(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, catalog[:7]); err != nil {
		t.Fatal(err)
	}
	root = claimJob(t, store, root.ID)
	reserved := generationAttempt(t, store, root, 1)
	succeeded := generationAttempt(t, store, root, 2)
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,output_digest='generation_output_v1:'||repeat('a',64),decision='publish' WHERE id=$1`, succeeded)
	_, post, _, _ := generationSocial(t, store, root)
	generationSQL(t, store, `UPDATE generation_jobs SET status='succeeded',lease_version=1,result_post_id=$2,published_attempt_id=$3,finished_at=created_at WHERE id=$1`, root.ID, post, succeeded)
	history := func() string {
		var data string
		if err := store.db.QueryRow(`SELECT jsonb_build_object(
			'attempts',(SELECT jsonb_agg(to_jsonb(a)-'pause_revision' ORDER BY id) FROM generation_attempts a),
			'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM generation_jobs j),
			'settings',(SELECT jsonb_agg(to_jsonb(s)-'pause_revision' ORDER BY agent_id) FROM agent_settings s),
			'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<=7))::text`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := history()
	if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatal("old schema accepted", err)
	}
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := store.Ready(ctx); err != nil {
			t.Fatal(err)
		}
		if history() != before {
			t.Fatal("upgrade rewrote history")
		}
	}
	for _, id := range []app.ID{reserved, succeeded} {
		if storedSpend(t, store, id).PauseRevision != nil {
			t.Fatal("upgrade invented publication authority")
		}
		generationReject(t, store, "23514", `UPDATE generation_attempts SET pause_revision=0 WHERE id=$1`, id)
	}
	// Outstanding old calls can still settle without inventing a revision.
	a := storedSpend(t, store, reserved)
	if ok, err := store.SettleGeneration(ctx, a, app.GenerationOutcome{Failure: app.GenerationTimeout}, ""); err != nil || !ok {
		t.Fatal("legacy settlement failed", ok, err)
	}
}
