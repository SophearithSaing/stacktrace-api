package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

func TestWorkerExecuteConfigurationAndCancellation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:private-password@127.0.0.1:1/database?sslmode=disable")
	for _, key := range []string{"", "invalid private key", "fake-key"} {
		t.Setenv("TOGETHER_API_KEY", key)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var output bytes.Buffer
		err := run(ctx, []string{"execute"}, &output)
		want := worker.ExecutionConfiguration
		if key == "fake-key" {
			want = worker.ExecutionStopped
		}
		if err != want || output.Len() != 0 {
			t.Fatal(output.String(), err)
		}
	}
}

// Embedding the boundary makes any unexpected storage method fail immediately.
// There is no real transport, database connection or provider in command tests.
type commandStore struct {
	worker.ExecutionStore
	probes int
}

func (*commandStore) Ready(context.Context) error                            { return nil }
func (*commandStore) RecoverGenerationAttempts(context.Context) (int, error) { return 2, nil }
func (*commandStore) ExpireGenerationJobs(context.Context) (int, error)      { return 3, nil }
func (s *commandStore) ClaimGeneration(ctx context.Context) (*app.GenerationJob, error) {
	deadline, ok := ctx.Deadline()
	if !ok || deadline.IsZero() {
		return nil, errors.New("missing deadline")
	}
	s.probes++
	return nil, nil
}

type forbiddenProvider struct{ t *testing.T }

func (p forbiddenProvider) Generate(context.Context, app.GenerationRequest) app.GenerationOutcome {
	p.t.Fatal("empty pass accessed provider")
	return app.GenerationOutcome{}
}

func TestWorkerExecuteCommandNoProviderAccess(t *testing.T) {
	store := &commandStore{}
	var output bytes.Buffer
	if err := execute(context.Background(), store, forbiddenProvider{t}, &output); err != nil {
		t.Fatal(err)
	}
	if store.probes != 8 || output.String() != "Execution pass complete: probes=8 claimed=0 calls=0 recovered=2 expired=3 published=0 skipped=0 cancelled=0 failed=0 retried=0 denied=0\n" {
		t.Fatal(output.String())
	}
}

func TestWorkerExecuteReportsSafeCodesAndCounts(t *testing.T) {
	for _, failure := range []error{nil, worker.ExecutionCredentials, errors.New("secret provider response"), worker.ExecutionError("secret arbitrary code")} {
		var output bytes.Buffer
		err := reportExecution(&output, worker.ExecutionSummary{Calls: 1, Failed: 1}, failure)
		if strings.Contains(output.String(), "secret") || err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe reporting")
		}
		if failure == nil && err != nil || failure != nil && err == nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "calls=1") || !strings.Contains(output.String(), "failed=1") {
			t.Fatal(output.String())
		}
	}
}
