package app

import (
	"testing"
	"time"
)

func testGenerationJob() GenerationJob {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	id := NewID()
	return GenerationJob{ID: id, AgentID: NewID(), PersonaVersion: 1, TriggerKind: TriggerScheduled, TriggerKey: "scheduled:2026-09-20:1", OutputKind: OutputPost, RootJobID: id, Status: JobPending, AvailableAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
}

func testRunningJob() GenerationJob {
	j := testGenerationJob()
	j.Status, j.LeaseVersion = JobRunning, 1
	expiry := j.CreatedAt.Add(time.Minute)
	j.LeaseExpiresAt = &expiry
	return j
}

func TestGenerationJobStructure(t *testing.T) {
	valid := testGenerationJob()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*GenerationJob){
		"id":                    func(j *GenerationJob) { j.ID = "bad" },
		"root":                  func(j *GenerationJob) { j.RootJobID = NewID() },
		"depth":                 func(j *GenerationJob) { j.ChainDepth = -1 },
		"persona":               func(j *GenerationJob) { j.PersonaVersion = 0 },
		"status":                func(j *GenerationJob) { j.Status = "other" },
		"trigger":               func(j *GenerationJob) { j.TriggerKind = "other" },
		"scheduled actor":       func(j *GenerationJob) { id := NewID(); j.TriggerActorID = &id },
		"scheduled output":      func(j *GenerationJob) { j.OutputKind = OutputReply },
		"expiry":                func(j *GenerationJob) { j.ExpiresAt = j.AvailableAt },
		"availability":          func(j *GenerationJob) { j.AvailableAt = j.CreatedAt.Add(-time.Second) },
		"pending lease":         func(j *GenerationJob) { j.LeaseVersion = 1 },
		"running no lease":      func(j *GenerationJob) { j.Status = JobRunning },
		"result before success": func(j *GenerationJob) { id := NewID(); j.ResultPostID = &id },
		"finished pending":      func(j *GenerationJob) { j.FinishedAt = &j.CreatedAt },
		"failed no reason":      func(j *GenerationJob) { j.Status = JobFailed; j.FinishedAt = &j.CreatedAt },
	} {
		t.Run(name, func(t *testing.T) {
			j := valid
			change(&j)
			if err := j.Validate(); err == nil {
				t.Fatal("accepted invalid job")
			}
		})
	}
	actor, source, reply, repost := NewID(), NewID(), NewID(), NewID()
	for _, trigger := range []GenerationTrigger{TriggerReply, TriggerRepost, TriggerQuote, TriggerHumanPost, TriggerContinuation} {
		j := valid
		j.TriggerKind, j.OutputKind, j.TriggerActorID, j.SourcePostID, j.CooldownKey = trigger, OutputReply, &actor, &source, "actor:agent:conversation:class"
		if trigger == TriggerReply {
			j.SourceReplyID = &reply
		}
		if trigger == TriggerRepost {
			j.SourceRepostID = &repost
		}
		if trigger == TriggerContinuation {
			j.RootJobID, j.ChainDepth = NewID(), 1
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("%s: %v", trigger, err)
		}
		j.TriggerActorID = nil
		if err := j.Validate(); err == nil {
			t.Fatalf("%s lost durable actor", trigger)
		}
	}
}

func TestGenerationJobClaimRetryAndFencing(t *testing.T) {
	pending := testGenerationJob()
	running := pending
	running.Status, running.LeaseVersion = JobRunning, 1
	leaseEnd := pending.CreatedAt.Add(time.Minute)
	running.LeaseExpiresAt = &leaseEnd
	now := pending.AvailableAt
	if err := ValidateGenerationJobTransition(pending, running, 0, now, nil); err != nil {
		t.Fatal(err)
	}
	if err := running.ValidateLease(1, now); err != nil {
		t.Fatal(err)
	}
	if err := running.ValidateLease(0, now); err == nil {
		t.Fatal("accepted stale lease")
	}
	if err := running.ValidateLease(1, leaseEnd); err == nil {
		t.Fatal("accepted expired lease")
	}
	for name, change := range map[string]func(*GenerationJob){
		"identity":     func(j *GenerationJob) { j.ID, j.RootJobID = NewID(), NewID() },
		"expiry":       func(j *GenerationJob) { j.ExpiresAt = j.ExpiresAt.Add(time.Hour) },
		"persona":      func(j *GenerationJob) { j.PersonaVersion++ },
		"key":          func(j *GenerationJob) { j.TriggerKey = "another" },
		"lease jump":   func(j *GenerationJob) { j.LeaseVersion = 2 },
		"lease expiry": func(j *GenerationJob) { j.LeaseExpiresAt = &now },
	} {
		t.Run(name, func(t *testing.T) {
			next := running
			change(&next)
			if err := ValidateGenerationJobTransition(pending, next, 0, now, nil); err == nil {
				t.Fatal("accepted invalid claim")
			}
		})
	}
	if err := ValidateGenerationJobTransition(pending, running, 0, now.Add(-time.Second), nil); err == nil {
		t.Fatal("claimed before due")
	}
	renewed := running
	renewal := leaseEnd.Add(time.Minute)
	renewed.LeaseExpiresAt = &renewal
	if err := ValidateGenerationJobTransition(running, renewed, 1, now.Add(time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGenerationJobTransition(running, renewed, 1, leaseEnd, nil); err == nil {
		t.Fatal("renewed expired lease without new fence")
	}
	renewed.LeaseVersion++
	if err := ValidateGenerationJobTransition(running, renewed, 1, leaseEnd, nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := ValidateGenerationJobTransition(running, renewed, 1, now, nil); err == nil {
		t.Fatal("stole live lease")
	}
	retry := running
	retry.Status, retry.LeaseExpiresAt, retry.ReasonCode = JobRetryWait, nil, "provider_timeout"
	retry.AvailableAt = now.Add(10 * time.Second)
	if err := ValidateGenerationJobTransition(running, retry, 1, now.Add(time.Second), nil); err != nil {
		t.Fatal(err)
	}
	second := retry
	second.Status, second.LeaseVersion, second.LeaseExpiresAt, second.ReasonCode = JobRunning, 2, &leaseEnd, ""
	if err := ValidateGenerationJobTransition(retry, second, 1, retry.AvailableAt, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGenerationJobTransition(retry, second, 0, retry.AvailableAt, nil); err == nil {
		t.Fatal("accepted stale fence")
	}
	retry.AvailableAt = retry.ExpiresAt
	if err := retry.Validate(); err == nil {
		t.Fatal("retry extended expiry")
	}
}

func TestGenerationJobPublication(t *testing.T) {
	running := testRunningJob()
	now := running.CreatedAt.Add(time.Second)
	attempt := testGenerationAttempt(running)
	attempt.Status, attempt.FinishedAt = AttemptSucceeded, &now
	success := running
	result := NewID()
	success.Status, success.LeaseExpiresAt, success.FinishedAt = JobSucceeded, nil, &now
	success.ResultPostID, success.PublishedAttemptID = &result, &attempt.ID
	if err := ValidateGenerationJobTransition(running, success, 1, now, &attempt); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*GenerationJob, *GenerationAttempt){
		"no result":         func(j *GenerationJob, a *GenerationAttempt) { j.ResultPostID = nil },
		"two results":       func(j *GenerationJob, a *GenerationAttempt) { j.ResultReplyID = &result },
		"wrong result":      func(j *GenerationJob, a *GenerationAttempt) { j.ResultPostID, j.ResultReplyID = nil, &result },
		"no attempt link":   func(j *GenerationJob, a *GenerationAttempt) { j.PublishedAttemptID = nil },
		"wrong attempt":     func(j *GenerationJob, a *GenerationAttempt) { a.ID = NewID() },
		"other job attempt": func(j *GenerationJob, a *GenerationAttempt) { a.JobID = NewID() },
		"stale attempt":     func(j *GenerationJob, a *GenerationAttempt) { a.LeaseVersion++ },
		"unknown attempt":   func(j *GenerationJob, a *GenerationAttempt) { a.Status, a.ErrorCode = AttemptUnknown, "timeout" },
	} {
		t.Run(name, func(t *testing.T) {
			j, a := success, attempt
			change(&j, &a)
			if err := ValidateGenerationJobTransition(running, j, 1, now, &a); err == nil {
				t.Fatal("accepted invalid publication")
			}
		})
	}
	if err := ValidateGenerationJobTransition(running, success, 1, now, nil); err == nil {
		t.Fatal("published without attempt")
	}
	if err := ValidateGenerationJobTransition(success, success, 1, now, &attempt); err == nil {
		t.Fatal("repeated terminal publication")
	}
	if err := ValidateGenerationJobTransition(success, running, 1, now, nil); err == nil {
		t.Fatal("reopened success")
	}
	// Flat replies and quotes have the same exact-one-result contract.
	for _, output := range []GenerationOutput{OutputReply, OutputQuote} {
		actor, source := NewID(), NewID()
		from := running
		from.TriggerKind, from.OutputKind, from.TriggerActorID, from.SourcePostID, from.CooldownKey = TriggerHumanPost, output, &actor, &source, "scope"
		to := from
		to.Status, to.LeaseExpiresAt, to.FinishedAt, to.PublishedAttemptID = JobSucceeded, nil, &now, &attempt.ID
		if output == OutputReply {
			to.ResultReplyID = &result
		} else {
			to.ResultPostID = &result
		}
		if err := ValidateGenerationJobTransition(from, to, 1, now, &attempt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGenerationJobExpiryAndTerminalStates(t *testing.T) {
	for _, before := range []GenerationJob{testGenerationJob(), testRunningJob()} {
		now := before.ExpiresAt
		skipped := before
		skipped.Status, skipped.ReasonCode, skipped.LeaseExpiresAt, skipped.FinishedAt = JobSkipped, "stale_trigger", nil, &now
		if err := ValidateGenerationJobTransition(before, skipped, before.LeaseVersion, now, nil); err != nil {
			t.Fatalf("stale cleanup: %v", err)
		}
		skipped.ReasonCode = "other"
		if err := ValidateGenerationJobTransition(before, skipped, before.LeaseVersion, now, nil); err == nil {
			t.Fatal("expired without stale reason")
		}
	}
	running := testRunningJob()
	now := running.CreatedAt.Add(time.Second)
	for _, status := range []GenerationJobStatus{JobSkipped, JobCancelled, JobFailed} {
		terminal := running
		terminal.Status, terminal.LeaseExpiresAt, terminal.FinishedAt, terminal.ReasonCode = status, nil, &now, "policy_rejected"
		if err := ValidateGenerationJobTransition(running, terminal, 1, now, nil); err != nil {
			t.Fatal(err)
		}
		retry := terminal
		retry.Status, retry.FinishedAt, retry.AvailableAt = JobRetryWait, nil, now.Add(time.Second)
		err := ValidateGenerationJobTransition(terminal, retry, 1, now, nil)
		if (err == nil) != (status == JobFailed) {
			t.Fatalf("%s retry: %v", status, err)
		}
	}
}

func TestGenerationRemovedRepost(t *testing.T) {
	j := testGenerationJob()
	actor, source := NewID(), NewID()
	j.TriggerKind, j.OutputKind, j.TriggerActorID, j.SourcePostID, j.CooldownKey = TriggerRepost, OutputReply, &actor, &source, "durable_scope"
	if err := j.Validate(); err != nil {
		t.Fatalf("deleted source remains structurally valid: %v", err)
	}
	next := j
	now := j.CreatedAt
	next.Status, next.ReasonCode, next.FinishedAt = JobCancelled, "source_removed", &now
	if err := ValidateGenerationJobTransition(j, next, 0, now, nil); err != nil {
		t.Fatal(err)
	}
	next.TriggerActorID = nil
	if err := next.Validate(); err == nil {
		t.Fatal("lost durable trigger actor")
	}
	next = j
	leaseEnd := now.Add(time.Minute)
	next.Status, next.LeaseVersion, next.LeaseExpiresAt = JobRunning, 1, &leaseEnd
	if err := ValidateGenerationJobTransition(j, next, 0, now, nil); err == nil {
		t.Fatal("claimed removed repost")
	}
}

func TestGenerationRemovedRepostCleanup(t *testing.T) {
	running := testRunningJob()
	actor, source := NewID(), NewID()
	running.TriggerKind, running.OutputKind = TriggerRepost, OutputReply
	running.TriggerActorID, running.SourcePostID, running.CooldownKey = &actor, &source, "durable_scope"
	// SourceRepostID was SET NULL while the job was running.
	for _, now := range []time.Time{running.LeaseExpiresAt.Add(-time.Second), *running.LeaseExpiresAt, running.LeaseExpiresAt.Add(time.Second)} {
		cancelled := running
		cancelled.Status, cancelled.ReasonCode, cancelled.LeaseExpiresAt, cancelled.FinishedAt = JobCancelled, "source_removed", nil, &now
		if err := ValidateGenerationJobTransition(running, cancelled, running.LeaseVersion, now, nil); err != nil {
			t.Errorf("removed repost cleanup at %s: %v", now, err)
		}
		if err := ValidateGenerationJobTransition(running, cancelled, running.LeaseVersion+1, now, nil); err == nil {
			t.Error("cleanup accepted mismatched lease version")
		}
		cancelled.ExpiresAt = cancelled.ExpiresAt.Add(time.Hour)
		if err := ValidateGenerationJobTransition(running, cancelled, running.LeaseVersion, now, nil); err == nil {
			t.Error("cleanup changed immutable expiry")
		}
	}
	now := *running.LeaseExpiresAt
	for _, change := range []func(*GenerationJob, *GenerationJob){
		func(before, after *GenerationJob) {
			id := NewID()
			before.SourceRepostID, after.SourceRepostID = &id, &id
		},
		func(before, after *GenerationJob) {
			before.TriggerKind, after.TriggerKind = TriggerHumanPost, TriggerHumanPost
		},
		func(before, after *GenerationJob) { after.ReasonCode = "operator_cancelled" },
		func(before, after *GenerationJob) { after.AgentID = NewID() },
		func(before, after *GenerationJob) { after.LeaseVersion++ },
	} {
		before, after := running, running
		after.Status, after.ReasonCode, after.LeaseExpiresAt, after.FinishedAt = JobCancelled, "source_removed", nil, &now
		change(&before, &after)
		if err := ValidateGenerationJobTransition(before, after, running.LeaseVersion, now, nil); err == nil {
			t.Error("cleanup authorized unrelated cancellation or changed identity/fence")
		}
	}
	for _, status := range []GenerationJobStatus{JobRunning, JobRetryWait, JobSucceeded} {
		after := running
		after.Status = status
		var attempt *GenerationAttempt
		switch status {
		case JobRunning:
			leaseEnd := now.Add(time.Minute)
			after.LeaseVersion++
			after.LeaseExpiresAt = &leaseEnd
		case JobRetryWait:
			after.LeaseExpiresAt, after.ReasonCode, after.AvailableAt = nil, "retry", now
		case JobSucceeded:
			completed := testGenerationAttempt(running)
			completed.Status, completed.FinishedAt = AttemptSucceeded, &now
			attempt = &completed
			result := NewID()
			after.LeaseExpiresAt, after.FinishedAt, after.ResultReplyID, after.PublishedAttemptID = nil, &now, &result, &completed.ID
		}
		if err := after.Validate(); err != nil {
			t.Fatalf("invalid %s test setup: %v", status, err)
		}
		if err := ValidateGenerationJobTransition(running, after, running.LeaseVersion, now, attempt); err == nil {
			t.Errorf("removed repost cleanup permitted %s", status)
		}
	}
	for _, now := range []time.Time{running.ExpiresAt, running.ExpiresAt.Add(time.Second)} {
		after := running
		after.Status, after.ReasonCode, after.LeaseExpiresAt, after.FinishedAt = JobCancelled, "source_removed", nil, &now
		if err := ValidateGenerationJobTransition(running, after, running.LeaseVersion, now, nil); err == nil {
			t.Error("cancelled expired job instead of skipping stale trigger")
		}
		after.Status, after.ReasonCode = JobSkipped, "stale_trigger"
		if err := ValidateGenerationJobTransition(running, after, running.LeaseVersion, now, nil); err != nil {
			t.Errorf("stale removed repost cleanup: %v", err)
		}
		if err := ValidateGenerationJobTransition(running, after, running.LeaseVersion+1, now, nil); err == nil {
			t.Error("stale cleanup accepted mismatched lease version")
		}
	}
}

func TestGenerationJobTimingBoundaries(t *testing.T) {
	running := testRunningJob()
	running.AvailableAt = *running.LeaseExpiresAt
	if err := running.Validate(); err == nil {
		t.Fatal("lease expired before job became due")
	}
	running = testRunningJob()
	now := running.CreatedAt.Add(30 * time.Second)
	attempt := testGenerationAttempt(running)
	attempt.Status, attempt.FinishedAt = AttemptSucceeded, &now
	success := running
	result := NewID()
	success.Status, success.LeaseExpiresAt, success.FinishedAt = JobSucceeded, nil, &now
	success.ResultPostID, success.PublishedAttemptID = &result, &attempt.ID
	for _, expiredAt := range []time.Time{*running.LeaseExpiresAt, running.ExpiresAt} {
		success.FinishedAt = &expiredAt
		if err := ValidateGenerationJobTransition(running, success, 1, expiredAt, &attempt); err == nil {
			t.Fatal("published on expiry boundary")
		}
	}
	success.FinishedAt = &now
	success.AvailableAt = now.Add(time.Second)
	if err := success.Validate(); err == nil {
		t.Fatal("published before job became due")
	}
	failed := running
	failed.Status, failed.LeaseExpiresAt, failed.FinishedAt, failed.ReasonCode = JobFailed, nil, &now, "provider_failed"
	retry := failed
	retry.Status, retry.FinishedAt, retry.AvailableAt = JobRetryWait, nil, now
	if err := ValidateGenerationJobTransition(failed, retry, 1, now.Add(-time.Second), nil); err == nil {
		t.Fatal("retried before recorded failure")
	}
}
