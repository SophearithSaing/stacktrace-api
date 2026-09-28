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
	"github.com/SophearithSaing/stacktrace-api/internal/llm"
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
	if len(args) != 1 || (args[0] != "check" && args[0] != "schedule" && args[0] != "execute" && args[0] != "serve") {
		return errors.New("usage: worker check | schedule | execute | serve (read-only preflight | bounded enqueue pass | bounded generation pass | continuous serve)")
	}
	// Includes opening the database, not just the preflight queries.
	deadline := 10 * time.Second
	role := config.Database
	if args[0] == "schedule" {
		deadline = 35 * time.Second
	}
	if args[0] == "execute" {
		deadline, role = worker.ExecutionTimeout, config.WorkerExecute
	}
	if args[0] == "serve" {
		role = config.WorkerServe
	}
	if role != config.WorkerServe {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	cfg, err := config.Load(role)
	if err != nil {
		if role == config.WorkerExecute {
			return worker.ExecutionConfiguration
		}
		return err
	}
	// Construction is local and cannot make a request. No other command reads
	// credentials or even constructs the provider.
	var provider *llm.Together
	if role == config.WorkerExecute || role == config.WorkerServe {
		provider, err = llm.NewTogether(cfg.TogetherAPIKey)
		if err != nil {
			return worker.ExecutionConfiguration
		}
	}
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		if role == config.WorkerExecute {
			if ctx.Err() != nil {
				return worker.ExecutionStopped
			}
			return worker.ExecutionStorage
		}
		return err
	}
	defer store.Close()
	if role == config.WorkerExecute {
		return execute(ctx, store, provider, output)
	}
	if role == config.WorkerServe {
		return serve(ctx, store, provider, cfg.HTTPAddr, output)
	}
	if args[0] == "schedule" {
		result, err := schedule(ctx, store)
		return reportSchedule(output, result, err)
	}
	summary, err := worker.Check(ctx, store)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Check-only configuration OK: configured=%d enabled=%d; no generation executed\n", summary.Configured, summary.Enabled)
	return err
}

func reportSchedule(output io.Writer, result postgres.GenerationScheduleResult, scheduleErr error) error {
	state := "complete"
	if scheduleErr != nil {
		state = "incomplete"
	}
	_, outputErr := fmt.Fprintf(output, "Schedule pass %s: visited=%d invalid=%d enqueued=%d denied=%d expired=%d; no generation executed\n",
		state, result.AgentsVisited, result.InvalidAgents, result.JobsEnqueued, result.SlotsDenied, result.JobsExpired)
	return errors.Join(scheduleErr, outputErr)
}
