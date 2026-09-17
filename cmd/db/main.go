package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/SophearithSaing/stacktrace-api/internal/config"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 || (args[0] != "ping" && args[0] != "migrate") {
		return errors.New("usage: db <ping|migrate>")
	}
	cfg, err := config.Load(config.Database)
	if err != nil {
		return err
	}
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if args[0] == "migrate" {
		if err := store.Migrate(ctx); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Database migrations applied")
		return nil
	}
	fmt.Fprintln(os.Stdout, "PostgreSQL connection OK")
	return nil
}
