// Package postgres implements PostgreSQL persistence using parameterized SQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db *sql.DB
}

// Open establishes a bounded connection pool and verifies connectivity within
// the caller's deadline. Schema changes are never a startup side effect.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	store := &Store{db: db}
	if err := store.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Driver errors may include connection details; keep them out of CLI/API logs.
		return errors.New("PostgreSQL is unavailable")
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}
