package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestWorkerUsage(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, args := range [][]string{nil, {"run"}, {"check", "extra"}, {"--help"}} {
		var output bytes.Buffer
		err := run(context.Background(), args, &output)
		if err == nil || !strings.Contains(err.Error(), "usage: worker check") || output.Len() != 0 {
			t.Fatalf("usage: %v", err)
		}
	}
}

func TestWorkerRequiresOnlyDatabaseConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	var output bytes.Buffer
	err := run(context.Background(), []string{"check"}, &output)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || output.Len() != 0 {
		t.Fatalf("configuration: %v", err)
	}
	// A cancelled connection cannot issue SQL or reach any provider. No HTTP,
	// signing, deployment, or provider configuration is needed for this command.
	t.Setenv("DATABASE_URL", "postgres://user:private-password@127.0.0.1:1/database?sslmode=disable")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = run(ctx, []string{"check"}, &output)
	if err != context.Canceled || strings.Contains(err.Error(), "private-password") || output.Len() != 0 {
		t.Fatalf("cancelled connection: %v", err)
	}
}
