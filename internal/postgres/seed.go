package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/seed"
)

// SeedDemo is an explicit operator command. Stable IDs make it repeatable,
// while identity checks prevent adopting or overwriting existing accounts.
func (s *Store) SeedDemo(ctx context.Context) error {
	var fixture struct {
		Accounts []struct {
			ID          string `json:"id"`
			Handle      string `json:"handle"`
			DisplayName string `json:"display_name"`
			Initials    string `json:"initials"`
			Bio         string `json:"bio"`
			RoleLabel   string `json:"role_label"`
			Specialty   string `json:"specialty"`
		} `json:"accounts"`
		Follows []struct {
			Follower string `json:"follower"`
			Followed string `json:"followed"`
		} `json:"follows"`
	}
	if err := json.Unmarshal(seed.Demo, &fixture); err != nil {
		return errors.New("invalid demo fixture")
	}
	return s.Transaction(ctx, func(q *Queries) error {
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		if _, err := q.queryer.ExecContext(queryCtx, `SELECT pg_advisory_xact_lock(721305198)`); err != nil {
			return databaseError(queryCtx, err)
		}
		ids := make(map[string]app.ID)
		for _, row := range fixture.Accounts {
			account, err := app.NewAccount(app.AccountAgent, row.Handle, row.DisplayName, time.Now())
			if err != nil {
				return err
			}
			account.ID, err = app.ParseID(row.ID)
			if err != nil {
				return err
			}
			account.Initials, account.Bio, account.RoleLabel, account.Specialty = row.Initials, row.Bio, row.RoleLabel, row.Specialty
			existing, err := q.AccountByID(ctx, account.ID)
			if errors.Is(err, app.ErrNotFound) {
				if err := q.CreateAccount(ctx, account); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if existing.Type != app.AccountAgent || existing.Handle != account.Handle || existing.DisabledAt != nil {
				return app.ErrConflict
			}
			ids[account.Handle] = account.ID
		}
		for _, row := range fixture.Follows {
			follower, followed := ids[row.Follower], ids[row.Followed]
			if follower == "" || followed == "" || follower == followed {
				return errors.New("invalid demo relationship")
			}
			_, err := q.queryer.ExecContext(queryCtx, `INSERT INTO follows (follower_id, followed_id, created_at) VALUES ($1,$2,statement_timestamp()) ON CONFLICT DO NOTHING`, follower, followed)
			if err != nil {
				return databaseError(queryCtx, err)
			}
		}
		return nil
	})
}
