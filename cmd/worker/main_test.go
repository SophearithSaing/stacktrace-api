package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

func TestWorkerUsage(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, args := range [][]string{nil, {"run"}, {"check", "extra"}, {"schedule", "extra"}, {"--help"}} {
		var output bytes.Buffer
		err := run(context.Background(), args, &output)
		if err == nil || !strings.Contains(err.Error(), "usage: worker check") || output.Len() != 0 {
			t.Fatalf("usage: %v", err)
		}
	}
}

func TestWorkerRequiresOnlyDatabaseConfiguration(t *testing.T) {
	for _, command := range []string{"check", "schedule"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "")
			var output bytes.Buffer
			err := run(context.Background(), []string{command}, &output)
			if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || output.Len() != 0 {
				t.Fatalf("configuration: %v", err)
			}
			// A cancelled connection cannot issue SQL or reach any provider. No HTTP,
			// signing, deployment, or provider configuration is needed for this command.
			t.Setenv("DATABASE_URL", "postgres://user:private-password@127.0.0.1:1/database?sslmode=disable")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err = run(ctx, []string{command}, &output)
			if err != context.Canceled || strings.Contains(err.Error(), "private-password") || output.Len() != 0 {
				t.Fatalf("cancelled connection: %v", err)
			}
		})
	}
}

func TestScheduleReportsCommittedCounts(t *testing.T) {
	result := postgres.GenerationScheduleResult{AgentsVisited: 4, InvalidAgents: 1, JobsEnqueued: 2, SlotsDenied: 1, JobsExpired: 3}
	failure := errors.New("safe failure")
	for _, expectedErr := range []error{nil, failure} {
		var output bytes.Buffer
		err := reportSchedule(&output, result, expectedErr)
		state := "complete"
		if expectedErr != nil {
			state = "incomplete"
		}
		want := "Schedule pass " + state + ": visited=4 invalid=1 enqueued=2 denied=1 expired=3; no generation executed\n"
		if !errors.Is(err, expectedErr) || output.String() != want {
			t.Fatalf("report: %q %v", output.String(), err)
		}
	}
}
