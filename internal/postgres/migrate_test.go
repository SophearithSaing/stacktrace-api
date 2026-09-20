package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/migrations"
)

// Each test owns an isolated schema; existing application tables are untouched.
func testStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := "test_" + strings.ReplaceAll(string(app.NewID()), "-", "")
	if _, err := admin.db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal("could not create isolated test schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.db.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("could not remove isolated test schema")
		}
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestMigrateEmptyAndConcurrent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("empty database readiness = %v", err)
	}
	var tableCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()`).Scan(&tableCount); err != nil || tableCount != 0 {
		t.Fatalf("readiness changed an empty schema: count=%d error=%v", tableCount, err)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			if err := store.Migrate(ctx); err != nil {
				t.Errorf("concurrent migration: %v", err)
			}
		})
	}
	workers.Wait()
	if err := store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&tableCount); err != nil || tableCount != len(catalog) {
		t.Fatalf("history count=%d error=%v", tableCount, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()`).Scan(&tableCount); err != nil || tableCount != 17 {
		t.Fatalf("table count=%d error=%v", tableCount, err)
	}
}

func TestSchemaCompatibility(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE schema_migrations SET checksum = repeat('0', 64)`,
		`UPDATE schema_migrations SET name = '001_changed.sql' WHERE version = 1`,
		`INSERT INTO schema_migrations VALUES (99, '099_newer.sql', repeat('0',64), now())`,
		`UPDATE schema_migrations SET version = 98 WHERE version = 1`,
	} {
		t.Run(mutation, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("readiness accepted incompatible history: %v", err)
			}
			if err := store.Migrate(ctx); !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("migration accepted incompatible history: %v", err)
			}
		})
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	catalog = append(catalog, migration{len(catalog) + 1, fmt.Sprintf("%03d_broken.sql", len(catalog)+1), strings.Repeat("0", 64), "CREATE TABLE partial_work (id integer); SELECT secret_invalid_sql"})
	if err := store.migrate(ctx, catalog); err == nil || strings.Contains(err.Error(), "secret_invalid_sql") {
		t.Fatalf("expected a safe migration failure: %v", err)
	}
	var tableCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()`).Scan(&tableCount); err != nil || tableCount != 0 {
		t.Fatalf("failed migration left partial work: count=%d error=%v", tableCount, err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("lock was not released or schema was not rolled back: %v", err)
	}
}

func TestMigrateUpgradeFromIdentity(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, catalog[:1]); err != nil {
		t.Fatal(err)
	}
	accountID := "00000000-0000-0000-0000-000000000001"
	if _, err := store.db.ExecContext(ctx, `INSERT INTO accounts (id, type, handle, display_name, created_at, updated_at)
		VALUES ($1, 'human', 'retained_account', 'Retained account', now(), now())`, accountID); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var handle string
	if err := store.db.QueryRowContext(ctx, `SELECT handle FROM accounts WHERE id = $1`, accountID).Scan(&handle); err != nil || handle != "retained_account" {
		t.Fatalf("upgrade did not retain account: handle=%q error=%v", handle, err)
	}
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil || applied != len(catalog) {
		t.Fatalf("upgrade history count=%d error=%v", applied, err)
	}
}

func TestMigrateUpgradeFromContent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, catalog[:2]); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("old schema readiness = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO accounts(id,type,handle,display_name,created_at,updated_at)
		VALUES ('10000000-0000-0000-0000-000000000001','agent','retained_agent','Agent',now(),now());
		INSERT INTO posts(id,author_id,body,created_at) VALUES
		('20000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000001','retained post',now());
		INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES
		('30000000-0000-0000-0000-000000000001','20000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000001','retained reply',now());
		INSERT INTO reposts(id,post_id,account_id,created_at) VALUES
		('40000000-0000-0000-0000-000000000001','20000000-0000-0000-0000-000000000001','10000000-0000-0000-0000-000000000001',now())`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := store.Ready(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var post, reply string
	var reposts, jobs, attempts int
	if err := store.db.QueryRowContext(ctx, `SELECT p.body, r.body,
		(SELECT count(*) FROM reposts), (SELECT count(*) FROM generation_jobs),
		(SELECT count(*) FROM generation_attempts) FROM posts p JOIN replies r ON r.post_id=p.id`).Scan(
		&post, &reply, &reposts, &jobs, &attempts); err != nil || post != "retained post" || reply != "retained reply" || reposts != 1 || jobs != 0 || attempts != 0 {
		t.Fatalf("upgrade content/provenance = %q %q %d %d %d, error=%v", post, reply, reposts, jobs, attempts, err)
	}
}

func TestMigrationLockCancellation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := store.Migrate(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked migration did not honor deadline: %v", err)
	}
}

func TestLoadMigrationsRejectsBadOrder(t *testing.T) {
	for _, files := range []fstest.MapFS{
		{},
		{"002_skip.sql": {Data: []byte("SELECT 1")}},
		{"001_a.sql": {}, "001_b.sql": {}},
		{"identity.sql": {}},
	} {
		if _, err := loadMigrations(files); err == nil {
			t.Fatal("accepted invalid migration catalog")
		}
	}
}
