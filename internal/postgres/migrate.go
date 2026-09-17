package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"time"

	"github.com/SophearithSaing/stacktrace-api/migrations"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrSchemaMismatch = errors.New("database schema is incompatible; run the matching db migrate release")

const migrationLock int64 = 0x535441434b4442

type migration struct {
	version             int
	name, checksum, sql string
}

func loadMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil || len(names) == 0 {
		return nil, errors.New("no embedded migrations found")
	}
	pattern := regexp.MustCompile(`^([0-9]{3})_[a-z0-9_]+\.sql$`)
	result := make([]migration, 0, len(names))
	for index, name := range names {
		match := pattern.FindStringSubmatch(name)
		if match == nil {
			return nil, errors.New("invalid migration filename")
		}
		version, _ := strconv.Atoi(match[1])
		if version != index+1 {
			return nil, errors.New("migration versions must be consecutive starting at 001")
		}
		contents, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, errors.New("could not read embedded migration")
		}
		checksum := sha256.Sum256(contents)
		result = append(result, migration{version, name, hex.EncodeToString(checksum[:]), string(contents)})
	}
	return result, nil
}

// Migrate applies all pending SQL and their history records atomically under a
// transaction-scoped advisory lock. It is called only by the explicit db CLI.
func (s *Store) Migrate(ctx context.Context) error {
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		return err
	}
	return s.migrate(ctx, catalog)
}

func (s *Store) migrate(ctx context.Context, catalog []migration) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(ctx, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return databaseError(ctx, err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY CHECK (version > 0),
		name text NOT NULL UNIQUE,
		checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return databaseError(ctx, err)
	}
	applied, err := checkHistory(ctx, tx, catalog)
	if err != nil {
		return err
	}
	for _, script := range catalog[applied:] {
		if _, err := tx.ExecContext(ctx, script.sql); err != nil {
			return fmt.Errorf("migration %03d failed: %w", script.version, databaseError(ctx, err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1,$2,$3)`, script.version, script.name, script.checksum); err != nil {
			return databaseError(ctx, err)
		}
	}
	return databaseError(ctx, tx.Commit())
}

// Ready checks exact schema version/checksum compatibility without creating or
// altering anything. Missing, edited, or newer migrations make this binary unready.
func (s *Store) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		return err
	}
	applied, err := checkHistory(ctx, s.db, catalog)
	if err != nil {
		return err
	}
	if applied != len(catalog) {
		return ErrSchemaMismatch
	}
	return nil
}

func checkHistory(ctx context.Context, connection queryer, catalog []migration) (int, error) {
	rows, err := connection.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "42P01" {
			return 0, ErrSchemaMismatch
		}
		return 0, databaseError(ctx, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return 0, databaseError(ctx, err)
		}
		if count >= len(catalog) || version != catalog[count].version || name != catalog[count].name || checksum != catalog[count].checksum {
			return 0, ErrSchemaMismatch
		}
		count++
	}
	return count, databaseError(ctx, rows.Err())
}
