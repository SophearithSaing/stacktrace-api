package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

type scheduleStore struct {
	readyErr, scheduleErr error
	result                postgres.GenerationScheduleResult
	ready, scheduled      bool
	deadline              time.Time
	cancel                context.CancelFunc
}

func (s *scheduleStore) Ready(ctx context.Context) error {
	s.ready = true
	s.deadline, _ = ctx.Deadline()
	if s.cancel != nil {
		s.cancel()
	}
	return s.readyErr
}

func (s *scheduleStore) ScheduleGeneration(context.Context) (postgres.GenerationScheduleResult, error) {
	s.scheduled = true
	return s.result, s.scheduleErr
}

func TestSchedule(t *testing.T) {
	partial := postgres.GenerationScheduleResult{AgentsVisited: 3, InvalidAgents: 1, JobsEnqueued: 1, SlotsDenied: 1, JobsExpired: 2}
	failure := errors.New("safe database failure")
	for _, scheduleErr := range []error{nil, failure} {
		store := &scheduleStore{result: partial, scheduleErr: scheduleErr}
		start := time.Now()
		got, err := Schedule(context.Background(), store)
		if !errors.Is(err, scheduleErr) || got != partial || !store.ready || !store.scheduled {
			t.Fatalf("schedule: %+v %v", got, err)
		}
		if store.deadline.Before(start) || store.deadline.After(time.Now().Add(30*time.Second)) {
			t.Fatal("unbounded pass")
		}
	}
	store := &scheduleStore{readyErr: failure}
	got, err := Schedule(context.Background(), store)
	if !errors.Is(err, failure) || got != (postgres.GenerationScheduleResult{}) || !store.ready || store.scheduled {
		t.Fatal("mutated incompatible schema")
	}
}

func TestScheduleCancellation(t *testing.T) {
	for _, beforeReady := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		store := &scheduleStore{}
		if beforeReady {
			cancel()
		} else {
			store.cancel = cancel
		}
		got, err := Schedule(ctx, store)
		cancel()
		if !errors.Is(err, context.Canceled) || got != (postgres.GenerationScheduleResult{}) || store.scheduled || store.ready == beforeReady {
			t.Fatalf("cancelled pass: %+v %v", got, err)
		}
	}
}
