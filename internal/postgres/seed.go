package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/seed"
)

// SeedDemo is an explicit operator command. Stable IDs make it repeatable,
// while identity checks prevent adopting or overwriting existing accounts.
func (s *Store) SeedDemo(ctx context.Context) error {
	fixture, err := decodeDemoFixture(seed.Demo, seed.Personas)
	if err != nil {
		return err
	}
	return s.Transaction(ctx, func(q *Queries) error {
		queryCtx, cancel := q.queryContext(ctx)
		defer cancel()
		if _, err := q.queryer.ExecContext(queryCtx, `SELECT pg_advisory_xact_lock(721305198)`); err != nil {
			return databaseError(queryCtx, err)
		}
		for _, account := range fixture.accounts {
			var accountType app.AccountType
			var handle string
			var disabledAt *time.Time
			// Keep identity and enablement stable through persona initialization.
			err := databaseError(queryCtx, q.queryer.QueryRowContext(queryCtx,
				`SELECT type,handle,disabled_at FROM accounts WHERE id=$1 FOR UPDATE`, account.ID).Scan(&accountType, &handle, &disabledAt))
			if errors.Is(err, app.ErrNotFound) {
				if err := q.CreateAccount(ctx, account); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if accountType != app.AccountAgent || handle != account.Handle || disabledAt != nil {
				return app.ErrConflict
			}
		}
		for _, pair := range fixture.follows {
			_, err := q.queryer.ExecContext(queryCtx, `INSERT INTO follows (follower_id, followed_id, created_at) VALUES ($1,$2,statement_timestamp()) ON CONFLICT DO NOTHING`, pair[0], pair[1])
			if err != nil {
				return databaseError(queryCtx, err)
			}
		}
		for i, persona := range fixture.personas {
			existing, err := q.PersonaByVersion(ctx, persona.AgentID, persona.Version)
			if errors.Is(err, app.ErrNotFound) {
				if err := q.CreatePersona(ctx, persona); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if app.ValidatePersonaUnchanged(existing, persona) != nil {
				return app.ErrConflict
			}
			settings, err := q.InitializeAgentSettings(ctx, fixture.settings[i])
			if err != nil {
				return err
			}
			// An operator may have selected a newer version. Validate it, but never
			// replace that selection (or the policy, pause state, or schedule).
			if _, err := q.PersonaByVersion(ctx, settings.AgentID, settings.PersonaVersion); err != nil {
				return err
			}
		}
		return nil
	})
}
