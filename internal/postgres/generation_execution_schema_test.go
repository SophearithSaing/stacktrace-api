package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/migrations"
)

func executionIndexes(t *testing.T, store *Store) {
	t.Helper()
	for name, want := range map[string]struct{ table, columns, predicate, options string }{
		"replies_visible_author_newest_idx":      {"replies", "author_id, created_at, id", "(deleted_at IS NULL)", "0 3 3"},
		"generation_attempts_reserved_idx":       {"generation_attempts", "started_at, id", "(status = 'reserved'::text)", "0 0"},
		"generation_jobs_action_reservation_idx": {"generation_jobs", "trigger_key, created_at, id", "", "0 0 0"},
	} {
		var table, method, columns, predicate, options string
		var valid, ready, unique bool
		err := store.db.QueryRow(`SELECT tab.relname,am.amname,
			(SELECT string_agg(pg_get_indexdef(i.indexrelid,n,true),', ' ORDER BY n) FROM generate_series(1,i.indnkeyatts) n),
			COALESCE(pg_get_expr(i.indpred,i.indrelid),''),i.indisvalid,i.indisready,i.indisunique,i.indoption::text
			FROM pg_index i JOIN pg_class idx ON idx.oid=i.indexrelid
			JOIN pg_class tab ON tab.oid=i.indrelid JOIN pg_am am ON am.oid=idx.relam
			WHERE idx.relnamespace=current_schema()::regnamespace AND idx.relname=$1`, name).Scan(&table, &method, &columns, &predicate, &valid, &ready, &unique, &options)
		if err != nil || table != want.table || method != "btree" || columns != want.columns || predicate != want.predicate || options != want.options || !valid || !ready || unique {
			t.Fatalf("index %s: table=%s method=%s columns=%s predicate=%s options=%s valid=%v ready=%v unique=%v error=%v", name, table, method, columns, predicate, options, valid, ready, unique, err)
		}
	}
}

func TestMigrateGenerationExecutionFresh(t *testing.T) {
	store := testStore(t)
	for range 2 {
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		executionIndexes(t, store)
		if err := store.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateGenerationExecutionFromOutput(t *testing.T) {
	store, root := schedulingLegacySetup(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, catalog[:5]); err != nil {
		t.Fatal(err)
	}
	root = claimJob(t, store, root.ID)
	reserved := generationAttempt(t, store, root, 1)
	succeeded := generationAttempt(t, store, root, 2)
	failed := generationAttempt(t, store, root, 3)
	unknown := generationAttempt(t, store, root, 4)
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,output_digest='generation_output_v1:'||repeat('a',64) WHERE id=$1`, succeeded)
	generationSQL(t, store, `UPDATE generation_attempts SET status='failed',error_code='test',finished_at=started_at WHERE id=$1`, failed)
	generationSQL(t, store, `UPDATE generation_attempts SET status='unknown',error_code='test',finished_at=started_at WHERE id=$1`, unknown)
	_, post, _, _ := generationSocial(t, store, root)
	generationSQL(t, store, `UPDATE generation_jobs SET status='succeeded',lease_version=1,result_post_id=$2,published_attempt_id=$3,finished_at=created_at WHERE id=$1`, root.ID, post, succeeded)
	history := func() map[string]string {
		result := map[string]string{}
		for _, table := range []string{"accounts", "posts", "replies", "reposts", "agent_personas", "generation_jobs", "generation_attempts"} {
			var data string
			if err := store.db.QueryRow(`SELECT jsonb_agg(to_jsonb(row) ORDER BY to_jsonb(row)::text)::text FROM ` + table + ` row`).Scan(&data); err != nil {
				t.Fatal(err)
			}
			result[table] = data
		}
		return result
	}
	before := history()
	var oldLedger string
	if err := store.db.QueryRow(`SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&oldLedger); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("005 readiness: %v", err)
	}
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := store.Ready(ctx); err != nil {
			t.Fatal(err)
		}
		executionIndexes(t, store)
		if !reflect.DeepEqual(before, history()) {
			t.Fatal("migration rewrote history")
		}
		var retainedLedger string
		if err := store.db.QueryRow(`SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<=5`).Scan(&retainedLedger); err != nil || retainedLedger != oldLedger {
			t.Fatalf("migration ledger rewritten: %v", err)
		}
	}
	for _, id := range []app.ID{reserved, succeeded, failed, unknown} {
		if _, err := store.GenerationAttemptByID(ctx, id); err != nil {
			t.Fatal(err)
		}
		if id != reserved {
			generationReject(t, store, "23514", `DELETE FROM generation_attempts WHERE id=$1`, id)
		}
	}
	generationReject(t, store, "23514", `DELETE FROM generation_jobs WHERE id=$1`, root.ID)
}
