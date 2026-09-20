package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/config"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 1 || args[0] != "check" {
		return errors.New("usage: worker check (read-only configuration check; no generation execution)")
	}
	// Includes opening the database, not just the preflight queries.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, err := config.Load(config.Database)
	if err != nil {
		return err
	}
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	summary, err := worker.Check(ctx, store)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Check-only configuration OK: configured=%d enabled=%d; no generation executed\n", summary.Configured, summary.Enabled)
	return err
}
