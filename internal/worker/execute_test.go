package worker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type providerFunc func(context.Context, app.GenerationRequest) app.GenerationOutcome

func (f providerFunc) Generate(ctx context.Context, req app.GenerationRequest) app.GenerationOutcome {
	return f(ctx, req)
}

type executionFake struct {
	job                                         app.GenerationJob
	input                                       app.GenerationContext
	attempt                                     *app.GenerationAttempt
	contextErr, reserveErr, publishErr          error
	admissionReason                             string
	normalize                                   app.GenerationFailure
	probes, nilClaims, calls, completed, denied int
	status                                      app.GenerationJobStatus
	settled                                     app.GenerationOutcome
	requestID                                   string
	renew                                       func(context.Context) error
	cleanup                                     func(context.Context)
	mu                                          sync.Mutex
	events                                      []string
}

func executeFixture(t *testing.T) *executionFake {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	id := app.NewID()
	lease := now.Add(3 * time.Minute)
	job := app.GenerationJob{ID: id, AgentID: app.NewID(), PersonaVersion: 1, TriggerKind: app.TriggerScheduled, TriggerKey: "test", OutputKind: app.OutputPost, RootJobID: id, MaxChainJobs: 1, Status: app.JobRunning, LeaseVersion: 1, LeaseExpiresAt: &lease, AvailableAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	input, err := app.BuildGenerationContext(job, app.Persona{AgentID: job.AgentID, Version: 1, Instructions: "Discuss Go.", TopicTags: []string{"go"}, CreatedAt: now}, app.PublicGenerationContext{})
	if err != nil {
		t.Fatal(err)
	}
	return &executionFake{job: job, input: input, status: app.JobRunning}
}

func (s *executionFake) event(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, value)
}
func (s *executionFake) Ready(context.Context) error { s.event("ready"); return nil }
func (s *executionFake) RecoverGenerationAttempts(context.Context) (int, error) {
	s.event("recover")
	return 0, nil
}
func (s *executionFake) ExpireGenerationJobs(context.Context) (int, error) {
	s.event("expire")
	return 0, nil
}
func (s *executionFake) ClaimGeneration(context.Context) (*app.GenerationJob, error) {
	s.probes++
	if s.probes != s.nilClaims+1 {
		return nil, nil
	}
	s.event("claim")
	return &s.job, nil
}
func (s *executionFake) LatestGenerationAttempt(context.Context, app.ID) (*app.GenerationAttempt, error) {
	s.event("latest")
	return s.attempt, nil
}
func (s *executionFake) GenerationContext(context.Context, app.ID, int64) (app.GenerationContext, error) {
	s.event("context")
	return s.input, s.contextErr
}
func (s *executionFake) ReserveGeneration(context.Context, app.ID, int64, app.GenerationContext) (app.GenerationAdmission, error) {
	s.event("reserve")
	if s.reserveErr != nil || s.admissionReason != "" {
		return app.GenerationAdmission{Reason: s.admissionReason}, s.reserveErr
	}
	s.attempt = &app.GenerationAttempt{ID: app.NewID(), JobID: s.job.ID, LeaseVersion: s.job.LeaseVersion, Status: app.AttemptReserved}
	return app.GenerationAdmission{Attempt: s.attempt}, nil
}
func (s *executionFake) RenewGeneration(ctx context.Context, _ app.ID, _ int64) (time.Time, error) {
	s.event("renew")
	if s.renew != nil {
		return time.Time{}, s.renew(ctx)
	}
	return time.Now().Add(90 * time.Second), nil
}
func (s *executionFake) SettleGeneration(ctx context.Context, _ app.GenerationAttempt, out app.GenerationOutcome, id string) (bool, error) {
	s.event("settle")
	if s.cleanup != nil {
		s.cleanup(ctx)
	}
	s.settled, s.requestID = out, id
	copy := *s.attempt
	copy.Status, copy.ErrorCode = app.AttemptFailed, string(out.Failure)
	if out.Result.Digest() != "" {
		copy.Status, copy.Decision, copy.OutputDigest = app.AttemptSucceeded, out.Result.Decision(), out.Result.Digest()
	}
	if s.normalize != "" {
		copy.Status, copy.ErrorCode, copy.Decision, copy.OutputDigest = app.AttemptFailed, string(s.normalize), "", ""
	}
	s.attempt = &copy
	return true, nil
}
func (s *executionFake) CompleteGeneration(context.Context, app.ID, int64, app.ID, time.Duration) (app.GenerationJobStatus, error) {
	s.event("complete")
	s.completed++
	if s.attempt.Status != app.AttemptSucceeded {
		s.status = app.JobFailed
	}
	return s.status, nil
}
func (s *executionFake) PublishGeneration(context.Context, app.ID, int64, app.ID, app.GenerationResult) (app.GenerationJob, error) {
	s.event("publish")
	if s.publishErr != nil {
		return app.GenerationJob{}, s.publishErr
	}
	return app.GenerationJob{Status: app.JobSucceeded}, nil
}
func (s *executionFake) DenyGeneration(_ context.Context, _ app.ID, _ int64, d app.GenerationDenial) (app.GenerationJobStatus, error) {
	s.event("deny")
	s.denied++
	status, _ := d.TerminalStatus()
	return status, nil
}

func fakeSuccess(t *testing.T, s *executionFake) app.GenerationOutcome {
	t.Helper()
	result, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A bounded Go example."}`), s.job)
	if err != nil {
		t.Fatal(err)
	}
	return app.GenerationOutcome{Result: result, ProviderRequestID: "safe-request.1"}
}

func TestExecuteSequenceAndNilProbe(t *testing.T) {
	s := executeFixture(t)
	s.nilClaims = 2
	result, err := Execute(context.Background(), s, providerFunc(func(ctx context.Context, _ app.GenerationRequest) app.GenerationOutcome {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > providerTimeout {
			t.Fatal("provider call is not bounded")
		}
		if s.attempt == nil || s.attempt.Status != app.AttemptReserved {
			t.Fatal("call before commit")
		}
		s.event("call")
		return fakeSuccess(t, s)
	}))
	if err != nil || result.Calls != 1 || result.Published != 1 || result.Probes != 8 || s.requestID != "safe-request.1" {
		t.Fatal(result, err)
	}
	want := []string{"ready", "recover", "expire", "claim", "latest", "context", "reserve", "call", "settle", "latest", "complete", "publish"}
	if !reflect.DeepEqual(s.events, want) {
		t.Fatal(s.events)
	}
}

func TestExecuteDenialsAndSafeFailures(t *testing.T) {
	for _, name := range []string{"ambiguous_commit", "already_admitted", "recovery_required", "budget_exhausted", "context", "prompt", "deleted", "publication", "publication_conflict", "publication_output", "storage", "arbitrary_code"} {
		t.Run(name, func(t *testing.T) {
			s := executeFixture(t)
			calls := 0
			switch name {
			case "ambiguous_commit":
				s.reserveErr = errors.New("secret commit error")
			case "already_admitted", "recovery_required", "budget_exhausted":
				s.admissionReason = name
			case "context":
				s.contextErr = app.ErrGenerationOutput
			case "prompt":
				s.input = app.GenerationContext{}
			case "deleted":
				s.contextErr = app.ErrDeleted
			case "publication":
				s.publishErr = app.ErrForbidden
			case "publication_conflict":
				s.publishErr = app.ErrConflict
			case "publication_output":
				s.publishErr = app.ErrGenerationOutput
			case "storage":
				s.contextErr = errors.New("secret storage error")
			case "arbitrary_code":
				s.contextErr = ExecutionError("secret arbitrary code")
			}
			_, err := Execute(context.Background(), s, providerFunc(func(context.Context, app.GenerationRequest) app.GenerationOutcome { calls++; return fakeSuccess(t, s) }))
			wantCalls := 0
			if strings.HasPrefix(name, "publication") {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatal(calls)
			}
			if name == "ambiguous_commit" || name == "storage" || name == "arbitrary_code" {
				if err != ExecutionStorage || strings.Contains(err.Error(), "secret") {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if name == "context" || name == "prompt" || name == "deleted" || strings.HasPrefix(name, "publication") {
				if s.denied != 1 {
					t.Fatal("unacknowledged denial")
				}
			}
		})
	}
}

func TestExecuteDurableStopsAndHistory(t *testing.T) {
	for _, failure := range []app.GenerationFailure{app.GenerationCredentials, app.GenerationConfiguration, app.GenerationAccountingUnsupported} {
		for _, mode := range []string{"outcome", "normalized", "history", "acknowledged"} {
			t.Run(string(failure)+"/"+mode, func(t *testing.T) {
				s := executeFixture(t)
				if mode == "normalized" {
					s.normalize = failure
				}
				if mode == "history" || mode == "acknowledged" {
					finish := s.job.AvailableAt.Add(time.Second)
					s.attempt = &app.GenerationAttempt{ID: app.NewID(), LeaseVersion: 1, Status: app.AttemptFailed, ErrorCode: string(failure), FinishedAt: &finish}
					if mode == "acknowledged" {
						s.job.LeaseVersion = 2
						s.job.AvailableAt = finish.Add(time.Second)
					}
				}
				result, err := Execute(context.Background(), s, providerFunc(func(context.Context, app.GenerationRequest) app.GenerationOutcome {
					if mode == "normalized" {
						return fakeSuccess(t, s)
					}
					return app.GenerationOutcome{Failure: failure}
				}))
				if err == nil || err.Error() != string(failure) || result.Probes != 1 || result.Published != 0 {
					t.Fatal(result, err)
				}
				// "history" and every unacknowledged fatal outcome (including
				// unsupported accounting) stop before any call. "acknowledged"
				// proves the operator-retry exception: credentials and
				// configuration failures call again after the operator retry,
				// while unsupported accounting never has a repair ack and keeps
				// its stop.
				if mode == "history" && result.Calls != 0 {
					t.Fatal("historical call")
				}
				if mode == "acknowledged" && failure == app.GenerationAccountingUnsupported && result.Calls != 0 {
					t.Fatalf("accounting ack stopped: %+v", result)
				}
				if mode == "acknowledged" && failure != app.GenerationAccountingUnsupported && result.Calls != 1 {
					t.Fatalf("operator retry did not call: %+v", result)
				}
			})
		}
	}
	for _, status := range []app.GenerationAttemptStatus{app.AttemptReserved, app.AttemptSucceeded} {
		s := executeFixture(t)
		s.attempt = &app.GenerationAttempt{ID: app.NewID(), Status: status}
		result, err := Execute(context.Background(), s, providerFunc(func(context.Context, app.GenerationRequest) app.GenerationOutcome {
			t.Fatal("historical call")
			return app.GenerationOutcome{}
		}))
		if err != nil || result.Calls != 0 {
			t.Fatal(result, err)
		}
		if status == app.AttemptSucceeded && s.denied != 1 {
			t.Fatal("lost payload not denied")
		}
	}
}

// TestExecuteOperatorRetryAcknowledgement reproduces the exact persisted retry
// state of a failed credentials job after a trusted operator retry: the job was
// claimed with a fresh lease availability (acknowledged via availability-after-
// finish) with the fatal attempt still retained in history. The pass calls
// again with a fresh reservation; repaired providers succeed and publish.
func TestExecuteOperatorRetryAcknowledgement(t *testing.T) {
	s := executeFixture(t)
	finish := s.job.AvailableAt.Add(time.Second)
	s.attempt = &app.GenerationAttempt{ID: app.NewID(), LeaseVersion: 1, Status: app.AttemptFailed,
		ErrorCode: string(app.GenerationCredentials), FinishedAt: &finish}
	s.job.LeaseVersion = 2
	s.job.AvailableAt = finish.Add(time.Second)
	want, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A repaired credentials retry result."}`), s.job)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Execute(context.Background(), s, providerFunc(func(_ context.Context, req app.GenerationRequest) app.GenerationOutcome {
		if s.attempt.Status != app.AttemptReserved {
			t.Fatal("call before reservation")
		}
		s.event("call")
		return app.GenerationOutcome{Result: want, ProviderRequestID: "retry-request.1"}
	}))
	if err != nil || result.Calls != 1 || result.Published != 1 || result.Retried != 0 {
		t.Fatalf("operator retry pass: %+v %v", result, err)
	}
	if s.requestID != "retry-request.1" {
		t.Fatalf("request id: %q", s.requestID)
	}
	wantEvents := []string{"ready", "recover", "expire", "claim", "latest", "context", "reserve", "call", "settle", "latest", "complete", "publish"}
	if !reflect.DeepEqual(s.events, wantEvents) {
		t.Fatalf("retry events: %v", s.events)
	}
}

func TestExecuteRenewalLossShutdownAndLateSuccess(t *testing.T) {
	for _, mode := range []string{"renewed", "lease_loss", "renew_storage", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			s := executeFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			renewed := make(chan struct{}, 1)
			s.renew = func(context.Context) error {
				select {
				case renewed <- struct{}{}:
				default:
				}
				if mode == "lease_loss" {
					return app.ErrConflict
				}
				if mode == "renew_storage" {
					return errors.New("secret")
				}
				return nil
			}
			if mode != "renewed" {
				s.cleanup = func(cleanup context.Context) {
					deadline, ok := cleanup.Deadline()
					if !ok || time.Until(deadline) > cleanupTimeout || cleanup.Err() != nil {
						t.Error("cleanup not independently bounded")
					}
				}
			}
			result, err := execute(ctx, s, providerFunc(func(callCtx context.Context, _ app.GenerationRequest) app.GenerationOutcome {
				select {
				case <-renewed:
				case <-time.After(time.Second):
					t.Fatal("renewal missing")
				}
				if mode == "shutdown" {
					cancel()
				}
				if mode != "renewed" {
					<-callCtx.Done()
				}
				return fakeSuccess(t, s) // Deliberately ignores cancellation on return.
			}), time.Millisecond)
			if mode == "renewed" {
				if err != nil || result.Published != 1 {
					t.Fatal(result, err)
				}
			} else {
				if result.Published != 0 || s.settled.Failure != app.GenerationCancelled || s.settled.InputTokens != nil || s.settled.Result.Digest() != "" {
					t.Fatal(result, s.settled, err)
				}
				if mode == "shutdown" && (err != ExecutionStopped || result.Cancelled != 1) {
					t.Fatal(result, err)
				}
				if mode == "lease_loss" && (err != nil || s.completed != 0 || s.denied != 0) {
					t.Fatal(result, err)
				}
				if mode == "renew_storage" && err != ExecutionStorage {
					t.Fatal(err)
				}
			}
			// All renewal is joined, so this snapshot is stable and race-free.
			settled := false
			for _, event := range s.events {
				if event == "settle" {
					settled = true
				}
				if settled && event == "renew" {
					t.Fatal("renewed after final mutation")
				}
			}
		})
	}
}
