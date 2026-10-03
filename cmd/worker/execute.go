package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/worker"
)

// execute runs one bounded worker execution pass.
func execute(ctx context.Context, store worker.ExecutionStore, provider app.GenerationProvider, output io.Writer) error {
	result, err := worker.Execute(ctx, store, provider)
	return reportExecution(output, result, err)
}

// reportExecution writes a safe worker execution report.
func reportExecution(output io.Writer, result worker.ExecutionSummary, executionErr error) error {
	state := "complete"
	var safeErr error
	if executionErr != nil {
		state = "incomplete"
		// Defense at the command boundary: never print an unknown/raw error.
		safeErr = worker.ExecutionStorage
		var code worker.ExecutionError
		if errors.As(executionErr, &code) {
			switch code {
			case worker.ExecutionStorage, worker.ExecutionStopped, worker.ExecutionCredentials, worker.ExecutionConfiguration, worker.ExecutionAccounting:
				safeErr = code
			}
		}
	}
	_, err := fmt.Fprintf(output, "Execution pass %s: probes=%d claimed=%d calls=%d recovered=%d expired=%d published=%d skipped=%d cancelled=%d failed=%d retried=%d denied=%d\n",
		state, result.Probes, result.Claimed, result.Calls, result.Recovered, result.Expired, result.Published, result.Skipped, result.Cancelled, result.Failed, result.Retried, result.Denied)
	if err != nil {
		return errors.New("execution_output")
	}
	return safeErr
}
