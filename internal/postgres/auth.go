package postgres

import (
	"context"
	"errors"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func (s *Store) RegisterHuman(ctx context.Context, account app.Account, passwordHash string, session app.Session, previousHash string) error {
	if account.Type != app.AccountHuman || account.DisabledAt != nil || session.AccountID != account.ID {
		return app.ErrForbidden
	}
	return s.Transaction(ctx, func(q *Queries) error {
		if err := q.CreateAccount(ctx, account); err != nil {
			return err
		}
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		_, err := q.queryer.ExecContext(queryCtx, `INSERT INTO password_credentials (account_id, password_hash, password_changed_at) VALUES ($1,$2,$3)`, account.ID, passwordHash, account.CreatedAt)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		return q.replaceSession(ctx, session, previousHash)
	})
}

func (q *Queries) CredentialByHandle(ctx context.Context, handle string) (app.Account, string, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var account app.Account
	var hash string
	err := q.queryer.QueryRowContext(ctx, `SELECT a.id, a.type, a.handle, a.display_name, a.initials, a.status_text,
		a.appearance_key, a.verified_at, a.disabled_at, c.password_hash
		FROM accounts a JOIN password_credentials c ON c.account_id = a.id
		WHERE a.handle = $1 AND a.type = 'human'`, handle).Scan(
		&account.ID, &account.Type, &account.Handle, &account.DisplayName, &account.Initials, &account.StatusText,
		&account.AppearanceKey, &account.VerifiedAt, &account.DisabledAt, &hash)
	return account, hash, databaseError(ctx, err)
}

func (s *Store) RotateSession(ctx context.Context, session app.Session, previousHash string) error {
	return s.Transaction(ctx, func(q *Queries) error {
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		var id app.ID
		err := q.queryer.QueryRowContext(queryCtx, `SELECT id FROM accounts WHERE id = $1 AND type = 'human' AND disabled_at IS NULL FOR UPDATE`, session.AccountID).Scan(&id)
		if err != nil {
			return authenticationError(databaseError(queryCtx, err))
		}
		return q.replaceSession(ctx, session, previousHash)
	})
}

func (q *Queries) replaceSession(ctx context.Context, session app.Session, previousHash string) error {
	if err := q.RevokeSession(ctx, previousHash); err != nil {
		return err
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	_, err := q.queryer.ExecContext(ctx, `INSERT INTO sessions (token_hash, account_id, created_at, expires_at) VALUES ($1,$2,$3,$4)`, session.TokenHash, session.AccountID, session.CreatedAt, session.ExpiresAt)
	return databaseError(ctx, err)
}

func (q *Queries) SessionAccount(ctx context.Context, hash string) (app.Account, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var account app.Account
	err := q.queryer.QueryRowContext(ctx, `SELECT a.id, a.type, a.handle, a.display_name, a.initials, a.bio,
		a.role_label, a.status_text, a.specialty, a.appearance_key, a.verified_at, a.created_at, a.updated_at
		FROM sessions s JOIN accounts a ON a.id = s.account_id
		WHERE s.token_hash = $1 AND s.expires_at > statement_timestamp() AND a.disabled_at IS NULL AND a.type = 'human'`, hash).Scan(
		&account.ID, &account.Type, &account.Handle, &account.DisplayName, &account.Initials, &account.Bio,
		&account.RoleLabel, &account.StatusText, &account.Specialty, &account.AppearanceKey, &account.VerifiedAt, &account.CreatedAt, &account.UpdatedAt)
	return account, authenticationError(databaseError(ctx, err))
}

func (q *Queries) RevokeSession(ctx context.Context, hash string) error {
	if hash == "" {
		return nil
	}
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	_, err := q.queryer.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash)
	return databaseError(ctx, err)
}

func authenticationError(err error) error {
	if errors.Is(err, app.ErrNotFound) {
		return app.ErrUnauthenticated
	}
	return err
}
