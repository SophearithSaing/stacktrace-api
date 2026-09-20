package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

type checkStore struct {
	readyErr, configErr error
	ready, checked      bool
	deadline            time.Time
}

func (s *checkStore) Ready(ctx context.Context) error {
	s.ready = true
	s.deadline, _ = ctx.Deadline()
	return s.readyErr
}

func (s *checkStore) CheckGenerationConfiguration(context.Context) (int, int, error) {
	s.checked = true
	return 2, 1, s.configErr
}

func TestCheck(t *testing.T) {
	store := &checkStore{}
	start := time.Now()
	got, err := Check(context.Background(), store)
	if err != nil || got != (Summary{2, 1}) || !store.ready || !store.checked {
		t.Fatalf("got %+v %v", got, err)
	}
	if store.deadline.Before(start) || store.deadline.After(time.Now().Add(10*time.Second)) {
		t.Fatal("missing bounded deadline")
	}
	failure := errors.New("safe failure")
	for _, readyFailure := range []bool{true, false} {
		store = &checkStore{}
		if readyFailure {
			store.readyErr = failure
		} else {
			store.configErr = failure
		}
		got, err = Check(context.Background(), store)
		if !errors.Is(err, failure) || got != (Summary{}) || store.checked == readyFailure {
			t.Fatalf("failure propagation: %+v %v", got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store = &checkStore{}
	if _, err := Check(ctx, store); !errors.Is(err, context.Canceled) || store.ready || store.checked {
		t.Fatalf("cancelled check: %v", err)
	}
}
