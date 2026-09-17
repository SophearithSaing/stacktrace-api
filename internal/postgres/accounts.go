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

func (q *Queries) ProfileByID(ctx context.Context, id app.ID, viewer app.ID) (app.AccountProfile, error) {
	return q.profile(ctx, "a.id = $1", string(id), viewer)
}

func (q *Queries) ProfileByHandle(ctx context.Context, handle string, viewer app.ID) (app.AccountProfile, error) {
	return q.profile(ctx, "a.handle = $1", handle, viewer)
}

// Independent subqueries avoid multiplying counts; one statement gives a
// consistent snapshot. PostCount stays zero until the content schema arrives.
func (q *Queries) profile(ctx context.Context, predicate, value string, viewer app.ID) (app.AccountProfile, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	var profile app.AccountProfile
	a := &profile.Account
	err := q.queryer.QueryRowContext(ctx, `SELECT a.id, a.type, a.handle, a.display_name, a.initials, a.bio,
		a.role_label, a.status_text, a.specialty, a.appearance_key, a.verified_at, a.created_at, a.updated_at,
		(SELECT count(*) FROM follows WHERE followed_id = a.id),
		(SELECT count(*) FROM follows WHERE follower_id = a.id),
		CASE WHEN $2::uuid IS NULL THEN NULL ELSE EXISTS (
			SELECT 1 FROM follows WHERE follower_id = $2 AND followed_id = a.id) END
		FROM accounts a WHERE `+predicate+` AND a.disabled_at IS NULL`, value, nullableID(viewer)).Scan(
		&a.ID, &a.Type, &a.Handle, &a.DisplayName, &a.Initials, &a.Bio, &a.RoleLabel, &a.StatusText,
		&a.Specialty, &a.AppearanceKey, &a.VerifiedAt, &a.CreatedAt, &a.UpdatedAt,
		&profile.FollowerCount, &profile.FollowingCount, &profile.ViewerFollowing)
	return profile, databaseError(ctx, err)
}

func nullableID(id app.ID) any {
	if id == "" {
		return nil
	}
	return id
}

// SetFollow derives the actor from a live human session again inside the write
// transaction. Locks serialize revocation/disabling with the mutation.
func (s *Store) SetFollow(ctx context.Context, sessionHash string, target app.ID, follow bool) (app.AccountProfile, error) {
	var profile app.AccountProfile
	err := s.Transaction(ctx, func(q *Queries) error {
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		var actor app.ID
		err := q.queryer.QueryRowContext(queryCtx, `SELECT a.id FROM sessions s JOIN accounts a ON a.id = s.account_id
			WHERE s.token_hash = $1 AND s.expires_at > statement_timestamp() AND a.type = 'human' AND a.disabled_at IS NULL
			FOR SHARE OF a, s`, sessionHash).Scan(&actor)
		if err != nil {
			return authenticationError(databaseError(queryCtx, err))
		}
		if actor == target {
			return &app.ValidationError{Fields: map[string]string{"account_id": "Cannot follow yourself"}}
		}
		var targetID app.ID
		err = q.queryer.QueryRowContext(queryCtx, `SELECT id FROM accounts WHERE id = $1 AND disabled_at IS NULL FOR SHARE`, target).Scan(&targetID)
		if err != nil {
			return databaseError(queryCtx, err)
		}
		if follow {
			_, err = q.queryer.ExecContext(queryCtx, `INSERT INTO follows (follower_id, followed_id, created_at) VALUES ($1,$2,statement_timestamp()) ON CONFLICT DO NOTHING`, actor, target)
		} else {
			_, err = q.queryer.ExecContext(queryCtx, `DELETE FROM follows WHERE follower_id = $1 AND followed_id = $2`, actor, target)
		}
		if err != nil {
			return databaseError(queryCtx, err)
		}
		profile, err = q.ProfileByID(ctx, target, actor)
		return err
	})
	return profile, err
}
