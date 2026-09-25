package postgres

import (
	"context"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/migrations"
)

func TestMigrateGenerationOutcomeFromExecution(t *testing.T) {
	store, root := schedulingLegacySetup(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, catalog[:6]); err != nil {
		t.Fatal(err)
	}
	root = claimJob(t, store, root.ID)
	ids := []app.ID{}
	for i, status := range []string{"reserved", "succeeded", "failed", "unknown"} {
		id := generationAttempt(t, store, root, i+1)
		ids = append(ids, id)
		if status == "succeeded" {
			generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,output_digest='generation_output_v1:'||repeat('a',64) WHERE id=$1`, id)
		} else if status != "reserved" {
			generationSQL(t, store, `UPDATE generation_attempts SET status=$2,error_code='test',finished_at=started_at WHERE id=$1`, id, status)
		}
	}
	var before, after string
	if err := store.db.QueryRow(`SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM generation_attempts t`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.db.QueryRow(`SELECT jsonb_agg(to_jsonb(t)-'decision'-'skip_reason'-'not_before' ORDER BY id)::text FROM generation_attempts t`).Scan(&after); err != nil || before != after {
		t.Fatal("legacy history changed", err)
	}
	for _, id := range ids {
		a := storedSpend(t, store, id)
		if a.Decision != "" || a.SkipReason != "" || a.NotBefore != nil {
			t.Fatal("invented metadata", a)
		}
	}
	generationReject(t, store, "23514", `UPDATE generation_attempts SET decision='publish' WHERE id=$1`, ids[1])
	for _, change := range []string{
		`decision='publish'`, `skip_reason='not_relevant'`, `not_before=now()`,
		`status='succeeded',finished_at=started_at,decision='publish'`,
		`status='succeeded',finished_at=started_at,output_digest='generation_output_v1:'||repeat('a',64),decision='skip'`,
		`status='succeeded',finished_at=started_at,output_digest='generation_output_v1:'||repeat('a',64),decision='skip',skip_reason='raw_text'`,
		`status='failed',finished_at=started_at,error_code='test',not_before='infinity'`,
		`status='failed',finished_at=started_at,error_code='test',not_before='0001-01-01 00:00:00+00'`,
	} {
		generationReject(t, store, "23514", `UPDATE generation_attempts SET `+change+` WHERE id=$1`, ids[0])
	}
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,output_digest='generation_output_v1:'||repeat('a',64),decision='skip',skip_reason='not_relevant' WHERE id=$1`, ids[0])
	generationReject(t, store, "23514", `UPDATE generation_attempts SET skip_reason='repetition' WHERE id=$1`, ids[0])
}
