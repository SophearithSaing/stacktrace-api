package main

import (
	"context"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

type scheduleStore interface {
	Ready(context.Context) error
	ScheduleGeneration(context.Context) (postgres.GenerationScheduleResult, error)
}

// schedule requires compatible migrations before any mutation. Do not use Check
// here: invalid optional agent settings must be skipped by admission, not prevent
// scheduling healthy agents. Preserve committed counts even on partial failure.
func schedule(ctx context.Context, store scheduleStore) (postgres.GenerationScheduleResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return postgres.GenerationScheduleResult{}, err
	}
	if err := store.Ready(ctx); err != nil {
		return postgres.GenerationScheduleResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return postgres.GenerationScheduleResult{}, err
	}
	result, err := store.ScheduleGeneration(ctx)
	if err == nil {
		err = ctx.Err()
	}
	return result, err
}
