package postgres

import (
	"context"
)

const maxConfiguredAgents = 1000

// CheckGenerationConfiguration validates all selected personas and settings,
// including paused agents. Counts describe configuration, not execution readiness
// or eligibility. The snapshot is read-only, bounded, and never returns prompts.
func (s *Store) CheckGenerationConfiguration(ctx context.Context) (configured, enabled int, err error) {
	err = s.readSnapshot(ctx, func(q *Queries) error {
		var innerErr error
		configured, enabled, innerErr = countGenerationConfiguration(ctx, q)
		return innerErr
	})
	if err != nil {
		return 0, 0, err
	}
	return configured, enabled, nil
}
