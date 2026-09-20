package app

import (
	"fmt"
	"math"
	"time"
)

type GenerationTrigger string

const (
	TriggerScheduled    GenerationTrigger = "scheduled"
	TriggerReply        GenerationTrigger = "reply"
	TriggerRepost       GenerationTrigger = "repost"
	TriggerQuote        GenerationTrigger = "quote"
	TriggerHumanPost    GenerationTrigger = "human_post"
	TriggerContinuation GenerationTrigger = "continuation"
)

type GenerationOutput string

const (
	OutputPost  GenerationOutput = "post"
	OutputQuote GenerationOutput = "quote"
	OutputReply GenerationOutput = "reply"
)

type GenerationJobStatus string

const (
	JobPending   GenerationJobStatus = "pending"
	JobRunning   GenerationJobStatus = "running"
	JobRetryWait GenerationJobStatus = "retry_wait"
	JobSucceeded GenerationJobStatus = "succeeded"
	JobSkipped   GenerationJobStatus = "skipped"
	JobCancelled GenerationJobStatus = "cancelled"
	JobFailed    GenerationJobStatus = "failed"
)

type GenerationJob struct {
	ID             ID
	AgentID        ID
	PersonaVersion int
	TriggerKind    GenerationTrigger
	TriggerKey     string
	TriggerActorID *ID
	CooldownKey    string
	SourcePostID   *ID
	SourceReplyID  *ID
	SourceRepostID *ID
	OutputKind     GenerationOutput
	RootJobID      ID
	ChainDepth     int
	// Root policy snapshots, inherited exactly by every child. A responding
	// agent's current stricter policy is an additional admission limit, never a
	// reason to mutate these or expand the root's allowance.
	MaxChainDepth      int
	MaxChainJobs       int
	Status             GenerationJobStatus
	AvailableAt        time.Time
	ExpiresAt          time.Time
	LeaseVersion       int64
	LeaseExpiresAt     *time.Time
	ResultPostID       *ID
	ResultReplyID      *ID
	PublishedAttemptID *ID
	ReasonCode         string
	CreatedAt          time.Time
	FinishedAt         *time.Time
}

func (j GenerationJob) Validate() error {
	if !validGenerationID(j.ID) || !validGenerationID(j.AgentID) || !validGenerationID(j.RootJobID) || j.PersonaVersion < 1 || j.PersonaVersion > 2147483647 {
		return fmt.Errorf("invalid job identity")
	}
	for _, id := range []*ID{j.TriggerActorID, j.SourcePostID, j.SourceReplyID, j.SourceRepostID, j.ResultPostID, j.ResultReplyID, j.PublishedAttemptID} {
		if id != nil && !validGenerationID(*id) {
			return fmt.Errorf("invalid job reference")
		}
	}
	if !validGenerationText(j.TriggerKey, 256) || j.ChainDepth < 0 || j.ChainDepth > 10 || (j.ChainDepth == 0) != (j.RootJobID == j.ID) {
		return fmt.Errorf("invalid trigger key or chain identity")
	}
	if j.MaxChainDepth < 0 || j.MaxChainDepth > 10 || j.MaxChainJobs < 1 || j.MaxChainJobs > 100 || j.MaxChainDepth >= j.MaxChainJobs || j.ChainDepth > j.MaxChainDepth {
		return fmt.Errorf("invalid immutable chain limits")
	}
	if j.OutputKind != OutputPost && j.OutputKind != OutputQuote && j.OutputKind != OutputReply {
		return fmt.Errorf("invalid output kind")
	}
	if j.TriggerKind == TriggerScheduled {
		if j.TriggerActorID != nil || j.CooldownKey != "" || j.SourcePostID != nil || j.SourceReplyID != nil || j.SourceRepostID != nil || j.OutputKind != OutputPost || j.ChainDepth != 0 {
			return fmt.Errorf("scheduled jobs must be source-free root posts")
		}
	} else {
		if j.TriggerActorID == nil || *j.TriggerActorID == j.AgentID || !validGenerationText(j.CooldownKey, 256) || j.SourcePostID == nil || j.OutputKind == OutputPost {
			return fmt.Errorf("interaction jobs require another actor, cooldown and source conversation")
		}
		switch j.TriggerKind {
		case TriggerReply:
			if j.SourceReplyID == nil || j.SourceRepostID != nil {
				return fmt.Errorf("reply trigger requires a source reply")
			}
		case TriggerRepost:
			// A hard-deleted repost is SET NULL; durable attribution remains.
			if j.SourceReplyID != nil {
				return fmt.Errorf("repost trigger cannot reference a reply")
			}
		case TriggerQuote, TriggerHumanPost:
			if j.SourceReplyID != nil || j.SourceRepostID != nil {
				return fmt.Errorf("post trigger cannot reference a reply or repost")
			}
		case TriggerContinuation:
			if j.SourceRepostID != nil || j.ChainDepth == 0 {
				return fmt.Errorf("continuation must belong to an existing chain")
			}
		default:
			return fmt.Errorf("invalid trigger kind")
		}
		if j.TriggerKind != TriggerContinuation && j.ChainDepth != 0 {
			return fmt.Errorf("direct triggers must start a chain")
		}
	}
	if j.CreatedAt.IsZero() || j.AvailableAt.Before(j.CreatedAt) || !j.ExpiresAt.After(j.AvailableAt) || j.LeaseVersion < 0 {
		return fmt.Errorf("invalid job timing or lease version")
	}
	terminal := false
	switch j.Status {
	case JobPending:
		if j.LeaseVersion != 0 {
			return fmt.Errorf("pending job cannot have been leased")
		}
	case JobRunning, JobRetryWait:
		if j.LeaseVersion == 0 {
			return fmt.Errorf("running or retry job requires a lease version")
		}
	case JobSucceeded, JobSkipped, JobCancelled, JobFailed:
		terminal = true
	default:
		return fmt.Errorf("invalid job status")
	}
	if j.Status == JobRunning {
		if j.LeaseExpiresAt == nil || !j.LeaseExpiresAt.After(j.AvailableAt) {
			return fmt.Errorf("running job requires a lease expiry")
		}
	} else if j.LeaseExpiresAt != nil {
		return fmt.Errorf("non-running job cannot retain a lease")
	}
	if terminal {
		if j.FinishedAt == nil || j.FinishedAt.Before(j.CreatedAt) {
			return fmt.Errorf("terminal job requires a finish time")
		}
	} else if j.FinishedAt != nil {
		return fmt.Errorf("unfinished job cannot have a finish time")
	}
	if j.Status == JobSucceeded {
		if j.LeaseVersion == 0 || j.PublishedAttemptID == nil || j.FinishedAt.Before(j.AvailableAt) || !j.FinishedAt.Before(j.ExpiresAt) || j.ReasonCode != "" {
			return fmt.Errorf("success requires an attempt and unexpired publication")
		}
		if j.OutputKind == OutputReply {
			if j.ResultReplyID == nil || j.ResultPostID != nil {
				return fmt.Errorf("reply success requires exactly one reply result")
			}
		} else if j.ResultPostID == nil || j.ResultReplyID != nil {
			return fmt.Errorf("post or quote success requires exactly one post result")
		}
	} else if j.ResultPostID != nil || j.ResultReplyID != nil || j.PublishedAttemptID != nil {
		return fmt.Errorf("only succeeded jobs may have publication results")
	}
	if j.Status == JobSkipped || j.Status == JobCancelled || j.Status == JobFailed || j.Status == JobRetryWait {
		if !validGenerationCode(j.ReasonCode) {
			return fmt.Errorf("job requires a bounded reason code")
		}
	} else if j.ReasonCode != "" {
		return fmt.Errorf("job cannot have a reason code in this state")
	}
	return nil
}

// ValidateLease fences owner actions. Expiry is exclusive; a lease never grants
// permission to spend or publish after the immutable job expiry.
func (j GenerationJob) ValidateLease(expectedVersion int64, now time.Time) error {
	if j.Status != JobRunning || expectedVersion <= 0 || j.LeaseVersion != expectedVersion || j.LeaseExpiresAt == nil || !now.Before(*j.LeaseExpiresAt) || !now.Before(j.ExpiresAt) || now.Before(j.AvailableAt) {
		return fmt.Errorf("stale, unavailable or expired generation lease")
	}
	return nil
}

// ValidateGenerationJobTransition is a pure lifecycle contract, not a claim or
// publication operation. Callers must lock/fence storage and recheck policy,
// source visibility, quotas and content in the eventual publication transaction.
// Failed jobs may be explicitly retried; all other terminal states are final.
func ValidateGenerationJobTransition(before, after GenerationJob, expectedVersion int64, now time.Time, attempt *GenerationAttempt) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if expectedVersion != before.LeaseVersion {
		return fmt.Errorf("stale generation lease version")
	}
	if !sameGenerationJobIdentity(before, after) {
		return fmt.Errorf("job identity, provenance and expiry are immutable")
	}
	if before.Status == JobSucceeded || before.Status == JobSkipped || before.Status == JobCancelled {
		return fmt.Errorf("terminal job cannot change")
	}
	if now.Before(before.CreatedAt) || before.FinishedAt != nil && now.Before(*before.FinishedAt) {
		return fmt.Errorf("transition precedes job creation or previous outcome")
	}
	if before.TriggerKind == TriggerRepost && before.SourceRepostID == nil && after.Status != JobCancelled && !(after.Status == JobSkipped && after.ReasonCode == "stale_trigger" && !now.Before(before.ExpiresAt)) {
		return fmt.Errorf("removed repost must cancel, not execute")
	}
	if after.FinishedAt != nil && !after.FinishedAt.Equal(now) {
		return fmt.Errorf("finish time must match transition time")
	}
	claim := after.Status == JobRunning && (before.Status == JobPending || before.Status == JobRetryWait || before.Status == JobRunning && !now.Before(*before.LeaseExpiresAt))
	if claim {
		if before.LeaseVersion == math.MaxInt64 || after.LeaseVersion != before.LeaseVersion+1 || now.Before(before.AvailableAt) || !now.Before(before.ExpiresAt) || !after.LeaseExpiresAt.After(now) {
			return fmt.Errorf("invalid claim or lease fencing increment")
		}
	} else {
		if after.LeaseVersion != before.LeaseVersion {
			return fmt.Errorf("only claims may advance the lease version")
		}
		switch before.Status {
		case JobPending, JobRetryWait:
			if after.Status != JobSkipped && after.Status != JobCancelled && after.Status != JobFailed {
				return fmt.Errorf("invalid queued job transition")
			}
		case JobFailed:
			if after.Status != JobRetryWait || before.LeaseVersion == 0 || !now.Before(before.ExpiresAt) {
				return fmt.Errorf("invalid failed job retry")
			}
		case JobRunning:
			// Fenced cleanup may cancel a removed repost without a live lease,
			// or skip stale work after job expiry. Other actions need ownership.
			staleCleanup := !now.Before(before.ExpiresAt) && after.Status == JobSkipped && after.ReasonCode == "stale_trigger"
			removedRepostCleanup := before.TriggerKind == TriggerRepost && before.SourceRepostID == nil && now.Before(before.ExpiresAt) && after.Status == JobCancelled && after.ReasonCode == "source_removed"
			if !now.Before(*before.LeaseExpiresAt) && !staleCleanup && !removedRepostCleanup {
				return fmt.Errorf("expired owner lease")
			}
			if after.Status != JobRunning && after.Status != JobRetryWait && after.Status != JobSucceeded && after.Status != JobSkipped && after.Status != JobCancelled && after.Status != JobFailed {
				return fmt.Errorf("invalid running job transition")
			}
			if after.Status == JobRunning && (!after.LeaseExpiresAt.After(*before.LeaseExpiresAt) || !now.Before(before.ExpiresAt)) {
				return fmt.Errorf("lease renewal must extend an unexpired job")
			}
		}
	}
	if after.Status == JobRetryWait {
		if after.AvailableAt.Before(now) || !now.Before(before.ExpiresAt) {
			return fmt.Errorf("retry must be future eligible and retain original expiry")
		}
	} else if !after.AvailableAt.Equal(before.AvailableAt) {
		return fmt.Errorf("only retry may change availability")
	}
	if !now.Before(before.ExpiresAt) && (after.Status != JobSkipped || after.ReasonCode != "stale_trigger") {
		return fmt.Errorf("expired job must skip as stale_trigger")
	}
	if after.Status == JobSucceeded {
		if err := before.ValidateLease(expectedVersion, now); err != nil {
			return err
		}
		if attempt == nil {
			return fmt.Errorf("publication requires its successful attempt")
		}
		if err := attempt.Validate(); err != nil {
			return err
		}
		if attempt.ID != *after.PublishedAttemptID || attempt.JobID != after.ID || attempt.LeaseVersion != after.LeaseVersion || attempt.Status != AttemptSucceeded || attempt.StartedAt.Before(before.AvailableAt) || attempt.FinishedAt.After(now) {
			return fmt.Errorf("published attempt must be successful and belong to this job and lease")
		}
	} else if attempt != nil {
		return fmt.Errorf("only publication accepts a published attempt")
	}
	return nil
}

func sameGenerationJobIdentity(a, b GenerationJob) bool {
	return a.ID == b.ID && a.AgentID == b.AgentID && a.PersonaVersion == b.PersonaVersion &&
		a.TriggerKind == b.TriggerKind && a.TriggerKey == b.TriggerKey &&
		sameGenerationID(a.TriggerActorID, b.TriggerActorID) && a.CooldownKey == b.CooldownKey &&
		sameGenerationID(a.SourcePostID, b.SourcePostID) && sameGenerationID(a.SourceReplyID, b.SourceReplyID) &&
		sameGenerationID(a.SourceRepostID, b.SourceRepostID) && a.OutputKind == b.OutputKind &&
		a.RootJobID == b.RootJobID && a.ChainDepth == b.ChainDepth &&
		a.MaxChainDepth == b.MaxChainDepth && a.MaxChainJobs == b.MaxChainJobs &&
		a.ExpiresAt.Equal(b.ExpiresAt) && a.CreatedAt.Equal(b.CreatedAt)
}

// ValidateGenerationContinuation checks lineage and exact root-limit inheritance.
// Admission must additionally lock the root and reserve total chain capacity,
// counting every retained job regardless of outcome, and apply the child's own
// conservative policy limits. This pure check does not reserve that capacity.
func ValidateGenerationContinuation(root, parent, child GenerationJob) error {
	for _, job := range []GenerationJob{root, parent, child} {
		if err := job.Validate(); err != nil {
			return err
		}
	}
	if root.ChainDepth != 0 || parent.RootJobID != root.ID || child.RootJobID != root.ID || child.TriggerKind != TriggerContinuation || child.ChainDepth != parent.ChainDepth+1 {
		return fmt.Errorf("invalid continuation lineage")
	}
	if parent.MaxChainDepth != root.MaxChainDepth || parent.MaxChainJobs != root.MaxChainJobs || child.MaxChainDepth != root.MaxChainDepth || child.MaxChainJobs != root.MaxChainJobs {
		return fmt.Errorf("continuations must inherit immutable root limits")
	}
	return nil
}

func sameGenerationID(a, b *ID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func validGenerationCode(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
