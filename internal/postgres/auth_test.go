package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestRegistrationAndRotationRollback(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	account, err := app.NewAccount(app.AccountHuman, "original", "Original", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	session := app.Session{TokenHash: strings.Repeat("a", 64), AccountID: account.ID, CreatedAt: account.CreatedAt, ExpiresAt: account.CreatedAt.Add(time.Hour)}
	if err := store.RegisterHuman(ctx, account, "$argon2id$test", session, ""); err != nil {
		t.Fatal(err)
	}
	newAccount, err := app.NewAccount(app.AccountHuman, "new_user", "New", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	invalid := app.Session{TokenHash: "invalid", AccountID: newAccount.ID, CreatedAt: newAccount.CreatedAt, ExpiresAt: newAccount.CreatedAt.Add(time.Hour)}
	if err := store.RegisterHuman(ctx, newAccount, "$argon2id$test", invalid, session.TokenHash); err == nil {
		t.Fatal("invalid session accepted")
	}
	if _, err := store.AccountByID(ctx, newAccount.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatal("registration left an orphan account")
	}
	if _, err := store.SessionAccount(ctx, session.TokenHash); err != nil {
		t.Fatal("failed registration revoked previous session")
	}
	invalid.AccountID = account.ID
	if err := store.RotateSession(ctx, invalid, session.TokenHash); err == nil {
		t.Fatal("invalid rotation accepted")
	}
	if _, err := store.SessionAccount(ctx, session.TokenHash); err != nil {
		t.Fatal("failed rotation revoked previous session")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RotateSession(ctx, session, ""); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("disabled account rotation: %v", err)
	}
	if _, err := store.SetFollow(ctx, session.TokenHash, newAccount.ID, true); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("mutation skipped session recheck: %v", err)
	}
}

func TestSeedCollisionRollsBackWithoutAdoptingHuman(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	account, err := app.NewAccount(app.AccountHuman, "postgresql", "Human", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedDemo(ctx); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("seed handle collision: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM accounts`).Scan(&count); err != nil || count != 1 {
		t.Fatal("seed was not atomic")
	}
	if existing, err := store.AccountByID(ctx, account.ID); err != nil || existing.Type != app.AccountHuman {
		t.Fatal("seed adopted an existing human")
	}
}

func TestSeedPreservesProfileEdits(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedDemo(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET display_name='Local name', bio='Local bio' WHERE handle='golang'`); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedDemo(ctx); err != nil {
		t.Fatal(err)
	}
	var displayName, bio string
	if err := store.db.QueryRowContext(ctx, `SELECT display_name, bio FROM accounts WHERE handle='golang'`).Scan(&displayName, &bio); err != nil {
		t.Fatal(err)
	}
	if displayName != "Local name" || bio != "Local bio" {
		t.Fatal("repeated seed overwrote local profile edits")
	}
	var accounts, follows int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM accounts), (SELECT count(*) FROM follows)`).Scan(&accounts, &follows); err != nil {
		t.Fatal(err)
	}
	if accounts != 2 || follows != 2 {
		t.Fatalf("repeated seed duplicated fixtures: %d accounts, %d follows", accounts, follows)
	}
}
