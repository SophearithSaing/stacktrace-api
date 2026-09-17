// Package postgres implements PostgreSQL persistence using parameterized SQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	*Queries
	db *sql.DB
}

const (
	connectTimeout     = 5 * time.Second
	queryTimeout       = 2 * time.Second
	transactionTimeout = 5 * time.Second
)

type queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Queries is bound either to the connection pool or to one transaction.
// Transaction callbacks receive only the transaction-bound operations.
type Queries struct {
	queryer  queryer
	lifetime context.Context
}

func (q *Queries) queryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(queryTimeout)
	if q.lifetime != nil {
		if transactionDeadline, ok := q.lifetime.Deadline(); ok && transactionDeadline.Before(deadline) {
			deadline = transactionDeadline
		}
	}
	queryCtx, cancel := context.WithDeadline(ctx, deadline)
	if q.lifetime == nil {
		return queryCtx, cancel
	}
	stop := context.AfterFunc(q.lifetime, cancel)
	if q.lifetime.Err() != nil {
		cancel()
	}
	return queryCtx, func() { stop(); cancel() }
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
	store := &Store{db: db, Queries: &Queries{queryer: db}}
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := db.PingContext(connectCtx); err != nil {
		db.Close()
		return nil, databaseError(connectCtx, err)
	}
	return store, nil
}

func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return databaseError(ctx, s.db.PingContext(ctx))
}

// Transaction rolls back on callback failure, cancellation, or panic. The
// callback must use its Queries argument for every operation in the unit of work.
func (s *Store) Transaction(ctx context.Context, fn func(*Queries) error) error {
	ctx, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(ctx, err)
	}
	defer tx.Rollback()
	if err := fn(&Queries{queryer: tx, lifetime: ctx}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return databaseError(ctx, tx.Commit())
}

func databaseError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, sql.ErrNoRows) {
		return app.ErrNotFound
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return app.ErrConflict
	}
	// Driver errors can contain credentials and submitted values.
	return app.ErrUnavailable
}

func (s *Store) Close() error {
	return s.db.Close()
}
