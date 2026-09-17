package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/jackc/pgx/v5/pgconn"
)

func testAccount(t *testing.T, handle string) app.Account {
	t.Helper()
	account, err := app.NewAccount(app.AccountHuman, handle, "Test account", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestTransactionBoundOperations(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	account := testAccount(t, "transaction_test")
	rollback := errors.New("abort")
	err := store.Transaction(ctx, func(tx *Queries) error {
		if err := tx.CreateAccount(ctx, account); err != nil {
			return err
		}
		if _, err := tx.AccountByID(ctx, account.ID); err != nil {
			t.Errorf("transaction could not read its own write: %v", err)
		}
		if _, err := store.AccountByID(ctx, account.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("uncommitted write escaped transaction: %v", err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err := store.AccountByID(ctx, account.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("rollback retained account: %v", err)
	}
	var finished *Queries
	if err := store.Transaction(ctx, func(tx *Queries) error {
		finished = tx
		return tx.CreateAccount(ctx, account)
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.AccountByID(ctx, account.ID)
	if err != nil || loaded.Type != account.Type || loaded.Handle != account.Handle || loaded.CreatedAt.Location() != time.UTC {
		t.Fatalf("committed account round trip failed: %v", err)
	}
	if err := finished.CreateAccount(ctx, testAccount(t, "late_write")); err == nil {
		t.Fatal("finished transaction silently fell back to pool")
	}
	duplicate := testAccount(t, account.Handle)
	if err := store.CreateAccount(ctx, duplicate); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("duplicate handle should map to domain conflict: %v", err)
	}
}

func TestTransactionPanicAndCancellation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	account := testAccount(t, "panic_rollback")
	func() {
		defer func() {
			if recover() == nil {
				t.Error("transaction swallowed panic")
			}
		}()
		_ = store.Transaction(ctx, func(tx *Queries) error {
			if err := tx.CreateAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			panic("abort transaction")
		})
	}()
	if _, err := store.AccountByID(ctx, account.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatal("panicked transaction was not rolled back")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	err := store.Transaction(cancelCtx, func(tx *Queries) error {
		if err := tx.CreateAccount(cancelCtx, account); err != nil {
			return err
		}
		cancel()
		return nil
	})
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled transaction committed: %v", err)
	}
	if _, err := store.AccountByID(ctx, account.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatal("cancelled transaction retained account")
	}
}

func TestIdentityConstraints(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	first, second := testAccount(t, "first_human"), testAccount(t, "second_human")
	for _, account := range []app.Account{first, second} {
		if err := store.CreateAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	assertSQLCode := func(query, code string, args ...any) {
		t.Helper()
		_, err := store.db.ExecContext(ctx, query, args...)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != code {
			t.Fatalf("expected constraint %s, got %v", code, err)
		}
	}
	for _, invalid := range []struct{ accountType, handle string }{{"robot", "valid_name"}, {"human", "UPPER"}, {"human", "ab"}, {"human", "@name"}} {
		assertSQLCode(`INSERT INTO accounts (id,type,handle,display_name,created_at,updated_at) VALUES ($1,$2,$3,'Name',now(),now())`, "23514", app.NewID(), invalid.accountType, invalid.handle)
	}
	assertSQLCode(`INSERT INTO password_credentials VALUES ($1,'$argon2id$fixture',now())`, "23503", app.NewID())
	assertSQLCode(`INSERT INTO password_credentials VALUES ($1,'plaintext',now())`, "23514", first.ID)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO password_credentials VALUES ($1,'$argon2id$fixture',now())`, first.ID); err != nil {
		t.Fatal(err)
	}
	assertSQLCode(`INSERT INTO password_credentials VALUES ($1,'$argon2id$fixture',now())`, "23505", first.ID)
	assertSQLCode(`INSERT INTO sessions VALUES ('raw-token',$1,now(),now()+interval '1 hour')`, "23514", first.ID)
	assertSQLCode(`INSERT INTO sessions VALUES ($1,$2,now(),now())`, "23514", strings.Repeat("a", 64), first.ID)
	assertSQLCode(`INSERT INTO sessions VALUES ($1,$2,now(),now()+interval '1 hour')`, "23503", strings.Repeat("a", 64), app.NewID())
	if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions VALUES ($1,$2,now(),now()+interval '1 hour')`, strings.Repeat("a", 64), first.ID); err != nil {
		t.Fatal(err)
	}
	assertSQLCode(`INSERT INTO sessions VALUES ($1,$2,now(),now()+interval '1 hour')`, "23505", strings.Repeat("a", 64), first.ID)
	assertSQLCode(`INSERT INTO follows VALUES ($1,$1,now())`, "23514", first.ID)
	assertSQLCode(`INSERT INTO follows VALUES ($1,$2,now())`, "23503", first.ID, app.NewID())
	if _, err := store.db.ExecContext(ctx, `INSERT INTO follows VALUES ($1,$2,now())`, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	assertSQLCode(`INSERT INTO follows VALUES ($1,$2,now())`, "23505", first.ID, second.ID)
}

func TestQueryDeadline(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AccountByID(ctx, app.NewID()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query did not honor its own timeout: %v", err)
	}
}

func TestTransactionDeadlineBindsBackgroundQueries(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	blocker, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	err = store.Transaction(deadline, func(tx *Queries) error {
		_, err := tx.AccountByID(context.Background(), app.NewID())
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transaction lifetime did not bound background query: %v", err)
	}
}
