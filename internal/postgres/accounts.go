package postgres

import (
	"context"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func (q *Queries) CreateAccount(ctx context.Context, account app.Account) error {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	_, err := q.queryer.ExecContext(ctx, `
		INSERT INTO accounts (id, type, handle, display_name, initials, bio,
			role_label, status_text, specialty, appearance_key, verified_at,
			created_at, updated_at, disabled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		account.ID, account.Type, account.Handle, account.DisplayName,
		account.Initials, account.Bio, account.RoleLabel, account.StatusText,
		account.Specialty, account.AppearanceKey, account.VerifiedAt,
		account.CreatedAt, account.UpdatedAt, account.DisabledAt)
	return databaseError(ctx, err)
}

func (q *Queries) AccountByID(ctx context.Context, id app.ID) (app.Account, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var account app.Account
	err := q.queryer.QueryRowContext(ctx, `
		SELECT id, type, handle, display_name, initials, bio, role_label,
			status_text, specialty, appearance_key, verified_at,
			created_at, updated_at, disabled_at
		FROM accounts WHERE id = $1`, id).Scan(
		&account.ID, &account.Type, &account.Handle, &account.DisplayName,
		&account.Initials, &account.Bio, &account.RoleLabel, &account.StatusText,
		&account.Specialty, &account.AppearanceKey, &account.VerifiedAt,
		&account.CreatedAt, &account.UpdatedAt, &account.DisabledAt)
	if err != nil {
		return app.Account{}, databaseError(ctx, err)
	}
	account.CreatedAt = account.CreatedAt.UTC()
	account.UpdatedAt = account.UpdatedAt.UTC()
	if account.VerifiedAt != nil {
		*account.VerifiedAt = account.VerifiedAt.UTC()
	}
	if account.DisabledAt != nil {
		*account.DisabledAt = account.DisabledAt.UTC()
	}
	return account, nil
}
