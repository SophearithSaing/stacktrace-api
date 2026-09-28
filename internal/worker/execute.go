package worker

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/llm"
)

const (
	ExecutionTimeout = 10 * time.Minute
	maxClaimProbes   = 8
	renewInterval    = 20 * time.Second
	providerTimeout  = 45 * time.Second
	cleanupTimeout   = 5 * time.Second
)

// ExecutionError contains fixed codes only. Storage/provider errors and payloads
// never cross the reporting boundary.
type ExecutionError string

func (e ExecutionError) Error() string { return string(e) }

const (
	ExecutionStorage       ExecutionError = "execution_storage"
	ExecutionStopped       ExecutionError = "execution_stopped"
	ExecutionCredentials   ExecutionError = "provider_credentials"
	ExecutionConfiguration ExecutionError = "provider_configuration"
	ExecutionAccounting    ExecutionError = "unsupported_accounting"
)

// ExecutionStore is worker-owned. Historical reads grant no call authority;
// ReserveGeneration must return only a NEW, committed reservation.
type ExecutionStore interface {
	Ready(context.Context) error
	RecoverGenerationAttempts(context.Context) (int, error)
	ExpireGenerationJobs(context.Context) (int, error)
	ClaimGeneration(context.Context) (*app.GenerationJob, error)
	LatestGenerationAttempt(context.Context, app.ID) (*app.GenerationAttempt, error)
	GenerationContext(context.Context, app.ID, int64) (app.GenerationContext, error)
	ReserveGeneration(context.Context, app.ID, int64, app.GenerationContext) (app.GenerationAdmission, error)
	RenewGeneration(context.Context, app.ID, int64) (time.Time, error)
	SettleGeneration(context.Context, app.GenerationAttempt, app.GenerationOutcome, string) (bool, error)
	CompleteGeneration(context.Context, app.ID, int64, app.ID, time.Duration) (app.GenerationJobStatus, error)
	PublishGeneration(context.Context, app.ID, int64, app.ID, app.GenerationResult) (app.GenerationJob, error)
	DenyGeneration(context.Context, app.ID, int64, app.GenerationDenial) (app.GenerationJobStatus, error)
}

type ExecutionSummary struct {
	Probes, Claimed, Calls, Recovered, Expired             int
	Published, Skipped, Cancelled, Failed, Retried, Denied int
}

// Execute performs one bounded pass, never sleeps through backoff, and makes at
// most one local provider call at a time. A provider must honor context deadlines.
func Execute(ctx context.Context, store ExecutionStore, provider app.GenerationProvider) (ExecutionSummary, error) {
	return execute(ctx, store, provider, renewInterval)
}

func execute(ctx context.Context, store ExecutionStore, provider app.GenerationProvider, interval time.Duration) (ExecutionSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, ExecutionTimeout)
	defer cancel()
	r := execution{store: store, provider: provider, interval: interval}
	err := r.run(ctx)
	if err != nil {
		var safe ExecutionError
		if errors.As(err, &safe) {
			switch safe {
			case ExecutionStorage, ExecutionStopped, ExecutionCredentials, ExecutionConfiguration, ExecutionAccounting:
				return r.summary, safe
			}
		}
		if ctx.Err() != nil {
			return r.summary, ExecutionStopped
		}
		return r.summary, ExecutionStorage
	}
	return r.summary, nil
}

type execution struct {
	store    ExecutionStore
	provider app.GenerationProvider
	interval time.Duration
	summary  ExecutionSummary
}

func (r *execution) run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.store.Ready(ctx); err != nil {
		return err
	}
	var err error
	r.summary.Recovered, err = r.store.RecoverGenerationAttempts(ctx)
	if err != nil {
		return err
	}
	r.summary.Expired, err = r.store.ExpireGenerationJobs(ctx)
	if err != nil {
		return err
	}
	for range maxClaimProbes {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.summary.Probes++
		job, err := r.store.ClaimGeneration(ctx)
		if err != nil {
			return err
		}
		if job == nil {
			continue
		} // May have cleaned one invalid candidate.
		r.summary.Claimed++
		if err := r.job(ctx, *job); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func durableStop(attempt *app.GenerationAttempt) error {
	if attempt == nil || attempt.Status == app.AttemptReserved || attempt.Status == app.AttemptSucceeded {
		return nil
	}
	switch app.GenerationFailure(attempt.ErrorCode) {
	case app.GenerationCredentials:
		return ExecutionCredentials
	case app.GenerationConfiguration:
		return ExecutionConfiguration
	case app.GenerationAccountingUnsupported:
		return ExecutionAccounting
	}
	return nil
}

func (r *execution) job(ctx context.Context, job app.GenerationJob) error {
	prior, err := r.store.LatestGenerationAttempt(ctx, job.ID)
	if err != nil {
		return err
	}
	if prior != nil {
		if prior.Status == app.AttemptReserved {
			r.summary.Denied++ // Beyond this pass's recovery batch; never call again.
			return nil
		}
		// Previously acknowledged retries remain eligible; don't re-acknowledge.
		acknowledged := prior.LeaseVersion < job.LeaseVersion && prior.FinishedAt != nil && job.AvailableAt.After(*prior.FinishedAt)
		if !acknowledged {
			status, err := r.complete(ctx, job, prior.ID)
			if err != nil {
				return err
			}
			if stop := durableStop(prior); stop != nil {
				return stop
			}
			if status == app.JobRunning {
				// Historical successful output has no process-local payload.
				return r.deny(ctx, job, app.GenerationOutputUnavailable)
			}
			return nil
		}
		// An acknowledged fatal attempt can only exist after a trusted operator
		// retry re-opened the failed job; unavailable claims cannot move a failed
		// job's availability. The operator ack is the repaired environment, so
		// these workers may call again with a fresh reservation. An ordinary
		// restart of unacknowledged fatal attempts keeps stopping above.
	}
	input, err := r.store.GenerationContext(ctx, job.ID, job.LeaseVersion)
	if err != nil {
		return r.rejection(ctx, job, err, false)
	}
	request := app.GenerationRequest{Job: job, Context: input}
	prompt, err := llm.BuildPrompt(request)
	if err == nil {
		_, err = prompt.AdmissionReservation()
	}
	if err != nil {
		return r.deny(ctx, job, app.GenerationContextRejected)
	}
	admission, err := r.store.ReserveGeneration(ctx, job.ID, job.LeaseVersion, input)
	if err != nil {
		return r.rejection(ctx, job, err, false)
	}
	if admission.Attempt == nil {
		r.summary.Denied++ // Includes already_admitted and recovery_required.
		return nil
	}
	if err := ctx.Err(); err != nil {
		return r.cancelled(job, *admission.Attempt, true)
	}
	r.summary.Calls++
	outcome, leaseErr := r.call(ctx, request)
	if ctx.Err() != nil || leaseErr != nil {
		err := r.cancelled(job, *admission.Attempt, leaseErr == nil)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ExecutionStopped
		}
		if errors.Is(leaseErr, app.ErrConflict) {
			return nil
		}
		return leaseErr
	}
	err = r.finish(ctx, job, *admission.Attempt, outcome)
	if ctx.Err() != nil {
		if cleanupErr := r.cancelled(job, *admission.Attempt, true); cleanupErr != nil {
			return cleanupErr
		}
		return ExecutionStopped
	}
	return err
}

// call joins renewal before any settlement/job mutation. No detached provider
// goroutine: the adapter's own deadline bounds the synchronous network call.
func (r *execution) call(ctx context.Context, request app.GenerationRequest) (app.GenerationOutcome, error) {
	callCtx, cancelCall := context.WithTimeout(ctx, providerTimeout)
	defer cancelCall()
	renewCtx, stopRenew := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				_, err := r.store.RenewGeneration(renewCtx, request.Job.ID, request.Job.LeaseVersion)
				if err != nil {
					if renewCtx.Err() != nil && errors.Is(err, context.Canceled) {
						done <- nil
						return
					}
					cancelCall()
					done <- err
					return
				}
			}
		}
	}()
	outcome := r.provider.Generate(callCtx, request)
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		outcome = app.GenerationOutcome{Failure: app.GenerationTimeout}
	}
	stopRenew()
	return outcome, <-done
}

// Cancellation settlement has one separate total cleanup budget. An uncertain
// call always retains full cost, even if a late provider returns success/usage.
func (r *execution) cancelled(job app.GenerationJob, attempt app.GenerationAttempt, acknowledge bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, err := r.store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Failure: app.GenerationCancelled}, "")
	if err != nil {
		return ExecutionStorage
	}
	if acknowledge {
		err = r.deny(ctx, job, app.GenerationExecutionCancelled)
		if err != nil {
			return ExecutionStorage
		}
	}
	return nil
}

func (r *execution) finish(ctx context.Context, job app.GenerationJob, reserved app.GenerationAttempt, outcome app.GenerationOutcome) error {
	accepted, err := r.store.SettleGeneration(ctx, reserved, outcome, outcome.ProviderRequestID)
	if err != nil {
		return err
	}
	durable, err := r.store.LatestGenerationAttempt(ctx, job.ID)
	if err != nil {
		return err
	}
	if durable == nil {
		return ExecutionStorage
	}
	status, err := r.complete(ctx, job, durable.ID)
	if err != nil {
		return err
	}
	if stop := durableStop(durable); stop != nil {
		return stop
	}
	if status != app.JobRunning {
		return nil
	}
	if !accepted || durable.ID != reserved.ID || durable.LeaseVersion != job.LeaseVersion || durable.Status != app.AttemptSucceeded || durable.Decision != app.GenerationPublish || durable.OutputDigest == "" || durable.OutputDigest != outcome.Result.Digest() {
		return r.deny(ctx, job, app.GenerationOutputUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	published, err := r.store.PublishGeneration(ctx, job.ID, job.LeaseVersion, durable.ID, outcome.Result)
	if err != nil {
		return r.rejection(ctx, job, err, true)
	}
	r.count(published.Status)
	return nil
}

func (r *execution) complete(ctx context.Context, job app.GenerationJob, attemptID app.ID) (app.GenerationJobStatus, error) {
	status, err := r.store.CompleteGeneration(ctx, job.ID, job.LeaseVersion, attemptID, time.Duration(rand.Int64N(int64(app.MaxGenerationRetryJitter)+1)))
	if errors.Is(err, app.ErrConflict) {
		r.summary.Denied++
		return "", nil
	}
	if err == nil {
		r.count(status)
	}
	return status, err
}

func (r *execution) deny(ctx context.Context, job app.GenerationJob, reason app.GenerationDenial) error {
	status, err := r.store.DenyGeneration(ctx, job.ID, job.LeaseVersion, reason)
	if errors.Is(err, app.ErrConflict) {
		r.summary.Denied++
		return nil
	}
	if err == nil {
		r.count(status)
	}
	return err
}

func (r *execution) rejection(ctx context.Context, job app.GenerationJob, err error, publication bool) error {
	switch {
	case errors.Is(err, app.ErrDeleted), errors.Is(err, app.ErrNotFound):
		return r.deny(ctx, job, app.GenerationSourceRemoved)
	case errors.Is(err, app.ErrForbidden):
		return r.deny(ctx, job, app.GenerationPolicyDenied)
	case errors.Is(err, app.ErrConflict):
		if publication {
			return r.deny(ctx, job, app.GenerationPublicationDenied)
		}
		r.summary.Denied++
		return nil
	case errors.Is(err, app.ErrGenerationOutput), errors.Is(err, app.ErrGenerationUnsafe), errors.Is(err, app.ErrGenerationRepetition):
		if publication {
			return r.deny(ctx, job, app.GenerationPublicationDenied)
		}
		return r.deny(ctx, job, app.GenerationContextRejected)
	default:
		return err
	}
}

func (r *execution) count(status app.GenerationJobStatus) {
	switch status {
	case app.JobSucceeded:
		r.summary.Published++
	case app.JobSkipped:
		r.summary.Skipped++
	case app.JobCancelled:
		r.summary.Cancelled++
	case app.JobFailed:
		r.summary.Failed++
	case app.JobRetryWait:
		r.summary.Retried++
	}
}
