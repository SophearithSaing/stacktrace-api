// Package worker provides read-only preflight and bounded enqueue orchestration.
// It does not claim, generate or publish work, and has no provider client.
package worker

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/llm"
)

type Store interface {
	Ready(context.Context) error
	CheckGenerationConfiguration(context.Context) (configured, enabled int, err error)
}

// Summary contains only counts. Enabled counts settings flags, not live account
// eligibility, provider availability, or permission to execute jobs.
type Summary struct{ Configured, Enabled int }

func Check(ctx context.Context, store Store) (Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if err := llm.ValidateModel(llm.Provider, llm.Model); err != nil {
		return Summary{}, err
	}
	if _, err := llm.TokenReservation(llm.MaxInputTokens, llm.MaxOutputTokens); err != nil {
		return Summary{}, err
	}
	if err := store.Ready(ctx); err != nil {
		return Summary{}, err
	}
	configured, enabled, err := store.CheckGenerationConfiguration(ctx)
	if err != nil {
		return Summary{}, err
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	return Summary{Configured: configured, Enabled: enabled}, nil
}
