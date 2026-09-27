package postgres

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func controlSchedulingFixture(t *testing.T) (*Store, app.AgentSettings, app.GenerationJob) {
	t.Helper()
	store := schedulingStore(t)
	settings := schedulingSettings()
	schedulingAdd(t, store, settings)
	ctx := context.Background()
	now := schedulingTime()
	result, err := store.scheduleGeneration(ctx, &now, schedulingDraw)
	if err != nil || result.JobsEnqueued != 1 {
		t.Fatalf("fixture schedule: %+v %v", result, err)
	}
	before := schedulingRead(t, store, settings.AgentID)
	if before.ScheduleDate != "2026-09-20" || before.RemainingSlots != 2 || before.NextPostAt == nil {
		t.Fatalf("unexpected sampled progress: %+v", before)
	}
	return store, before, schedulingJob(t, store, settings.AgentID)
}

func TestGenerationControlsAtomicPersonaSelection(t *testing.T) {
	store, persona, job := generationSetup(t)
	ctx := context.Background()
	// An atomic create+select rolls back the immutable insert when selection
	// cannot commit: here the settings row does not exist yet.
	second := persona
	second.Version, second.Instructions = 2, "Never claim live access."
	if err := store.CreateAndSelectPersona(ctx, second); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing settings selection: %v", err)
	}
	if _, err := store.PersonaByVersion(ctx, persona.AgentID, 2); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("atomic rollback: %v", err)
	}
	if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: persona.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: job.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAndSelectPersona(ctx, second); err != nil {
		t.Fatal(err)
	}
	if saved, err := store.PersonaByVersion(ctx, persona.AgentID, 2); err != nil || app.ValidatePersonaUnchanged(second, saved) != nil {
		t.Fatalf("create+select round trip: %+v %v", saved, err)
	}
	if got := schedulingRead(t, store, persona.AgentID); got.PersonaVersion != 2 {
		t.Fatalf("selection not persisted: %+v", got)
	}
	duplicate := persona
	duplicate.Version, duplicate.Instructions = 2, "A rewritten immutable version"
	if err := store.CreateAndSelectPersona(ctx, duplicate); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("duplicate version: %v", err)
	}
	if saved, err := store.PersonaByVersion(ctx, persona.AgentID, 2); err != nil || saved.Instructions != second.Instructions {
		t.Fatalf("duplicate overwrote version: %+v %v", saved, err)
	}
	if err := store.SelectAgentPersona(ctx, persona.AgentID, 99); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing version selection: %v", err)
	}
	if got := schedulingRead(t, store, persona.AgentID); got.PersonaVersion != 2 {
		t.Fatalf("failed selection changed state: %+v", got)
	}
	if err := store.SelectAgentPersona(ctx, persona.AgentID, 1); err != nil || schedulingRead(t, store, persona.AgentID).PersonaVersion != 1 {
		t.Fatalf("existing version selection failed: %v", err)
	}
	// A human account never gains persona storage.
	human, _ := contentTestActor(t, store, "persona_human")
	if err := store.CreateAndSelectPersona(ctx, app.Persona{AgentID: human, Version: 1, Instructions: "H", TopicTags: []string{"go"}, CreatedAt: second.CreatedAt}); !errors.As(err, new(*app.ValidationError)) {
		t.Fatalf("human persona accepted: %v", err)
	}
}

func TestGenerationControlsStrictPolicyEdit(t *testing.T) {
	store, settings, _ := controlSchedulingFixture(t)
	ctx := context.Background()
	current := schedulingRead(t, store, settings.AgentID)
	// A strict replace of a policy whose scheduling fields equal the saved ones
	// keeps the day's sampled progress and never resamples.
	relaxed := current.Policy
	relaxed.ReplyCapPerDay = 7
	relaxed.DailyTokenBudget = 12345
	jobsBefore := socialJobs(t, store)
	if err := store.SetAgentPolicy(ctx, settings.AgentID, relaxed); err != nil {
		t.Fatal(err)
	}
	after := schedulingRead(t, store, settings.AgentID)
	if !reflect.DeepEqual(after.Policy, relaxed) || after.Policy.ReplyCapPerDay != 7 || after.Policy.DailyTokenBudget != 12345 {
		t.Fatalf("policy replace mismatch: %+v", after.Policy)
	}
	if after.ScheduleDate != current.ScheduleDate || after.RemainingSlots != current.RemainingSlots || !after.NextPostAt.Equal(*current.NextPostAt) {
		t.Fatalf("non-scheduling edit reset schedule state: %+v -> %+v", current, after)
	}
	if len(socialJobs(t, store)) != len(jobsBefore) || len(jobsBefore) == 0 {
		t.Fatal("history fixture produced nothing")
	}
	// Publication history and existing reservations survive any policy edit.
	generationSQL(t, store, `UPDATE agent_settings SET last_published_at=clock_timestamp() WHERE agent_id=$1`, settings.AgentID)
	published := schedulingRead(t, store, settings.AgentID).LastPublishedAt
	tighter := after.Policy
	tighter.MinSpacingSeconds = 1800
	if err := store.SetAgentPolicy(ctx, settings.AgentID, tighter); err != nil {
		t.Fatal(err)
	}
	edited := schedulingRead(t, store, settings.AgentID)
	if edited.RemainingSlots != 0 || edited.NextPostAt != nil {
		t.Fatalf("scheduling edit kept a resampled day: %+v", edited)
	}
	if edited.LastPublishedAt == nil || !edited.LastPublishedAt.Equal(*published) || len(socialJobs(t, store)) != len(jobsBefore) {
		t.Fatalf("history erased by policy edit: %+v", edited)
	}
	now := schedulingTime()
	if result, err := store.scheduleGeneration(ctx, &now, schedulingDraw); err != nil || result.JobsEnqueued != 0 {
		t.Fatalf("retired day resampled: %+v %v", result, err)
	}
	// Invalid complete policies are rejected atomically by validation.
	invalid := edited.Policy
	invalid.Timezone = "Invalid/Timezone"
	if validation := store.SetAgentPolicy(ctx, settings.AgentID, invalid); !errors.As(validation, new(*app.ValidationError)) {
		t.Fatalf("invalid policy accepted: %v", validation)
	}
	malformed := edited.Policy
	malformed.ActiveStart = "9:00"
	if validation := store.SetAgentPolicy(ctx, settings.AgentID, malformed); !errors.As(validation, new(*app.ValidationError)) {
		t.Fatalf("malformed policy accepted: %v", validation)
	}
	last := schedulingRead(t, store, settings.AgentID)
	if !reflect.DeepEqual(last.Policy, edited.Policy) {
		t.Fatalf("rejected edit leaked: %+v", last.Policy)
	}
}

func TestGenerationControlsPauseFenceViaControls(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "single"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			ctx := context.Background()
			var pause, resume func() error
			pause = func() error {
				if all {
					count, err := store.PauseAllAgents(ctx)
					if err != nil || count != 1 {
						t.Fatalf("pause all: %d %v", count, err)
					}
					return err
				}
				return store.PauseAgent(ctx, job.AgentID)
			}
			resume = func() error {
				if all {
					count, err := store.ResumeAllAgents(ctx)
					if err != nil || count != 1 {
						t.Fatalf("resume all: %d %v", count, err)
					}
					return err
				}
				return store.ResumeAgent(ctx, job.AgentID)
			}
			attempt := admitSpend(t, store, job, input)
			if err := pause(); err != nil {
				t.Fatal(err)
			}
			if got := schedulingRead(t, store, job.AgentID); got.Enabled || got.PauseRevision != 1 {
				t.Fatalf("pause semantics: %+v", got)
			}
			if err := resume(); err != nil {
				t.Fatal(err)
			}
			resumed := schedulingRead(t, store, job.AgentID)
			if !resumed.Enabled || resumed.PauseRevision != 1 {
				t.Fatalf("resume semantics: %+v", resumed)
			}
			// Re-enabling does not restore the paused authority: the admitted
			// call may still settle, but its pre-pause revision can never publish.
			output, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A post-pause fence result"}`), job)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Result: output}, ""); err != nil || !ok {
				t.Fatalf("settlement after pause/resume: %v %v", ok, err)
			}
			before := publicationCounts(t, store)
			if _, pubErr := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output); !errors.Is(pubErr, app.ErrForbidden) {
				t.Fatalf("paused authority published: %v", pubErr)
			}
			if publicationCounts(t, store) != before || storedSpend(t, store, attempt.ID).AccountedTokens() != attempt.ReservedTokens {
				t.Fatal("denial changed usage accounting")
			}
			// A fresh admission after resume carries the new revision. The old
			// fence applies only to stale reservations, not to all future work;
			// an attempt with the current revision satisfies the exact publication
			// match (publication success itself is exercised by the existing
			// publication suite, which publishes revision-matched attempts).
			clock := time.Now().UTC()
			freshJob := contextJob(t, store, job, map[string]any{"created_at": clock.Add(-4000 * time.Second), "available_at": clock.Add(-4000 * time.Second), "lease_expires_at": clock.Add(time.Hour)})
			freshInput, _ := readGenerationContext(t, store, freshJob)
			fresh := admitSpend(t, store, freshJob, freshInput)
			if fresh.PauseRevision == nil || *fresh.PauseRevision != 1 {
				t.Fatalf("fresh admission revision: %v", fresh.PauseRevision)
			}
			freshOutput, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A post-resume fresh fence result"}`), freshJob)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := store.SettleGeneration(ctx, fresh, app.GenerationOutcome{Result: freshOutput}, ""); err != nil || !ok {
				t.Fatalf("fresh settlement: %v %v", ok, err)
			}
		})
	}
}

func TestGenerationControlsPauseStopsAdmission(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	if err := store.PauseAgent(ctx, job.AgentID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReserveGeneration(ctx, job.ID, job.LeaseVersion, input); err != nil || got.Attempt != nil || got.Reason != "policy_disabled" {
		t.Fatalf("pause did not stop admission: %+v %v", got, err)
	}
	if count := spendAttemptCount(t, store); count != 0 {
		t.Fatalf("paused admission reserved %d", count)
	}
}

func TestGenerationControlsSingleResumeValidation(t *testing.T) {
	store, _, root := generationSetup(t)
	ctx := context.Background()
	peer := socialAgent(t, store, "resume_peer", nil)
	// A valid, enabled agent resumes as a no-op success.
	if err := store.ResumeAgent(ctx, peer.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := store.PauseAgent(ctx, peer.AgentID); err != nil {
		t.Fatal(err)
	}
	if got := schedulingRead(t, store, peer.AgentID); got.Enabled || got.PauseRevision != 1 {
		t.Fatalf("pause semantics: %+v", got)
	}
	// Disabled accounts are never implicitly re-enabled by resume.
	generationSQL(t, store, `UPDATE accounts SET disabled_at=clock_timestamp() WHERE id=$1`, peer.AgentID)
	if err := store.ResumeAgent(ctx, peer.AgentID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("disabled account resume: %v", err)
	}
	if got := schedulingRead(t, store, peer.AgentID); got.Enabled {
		t.Fatal("disabled agent enabled")
	}
	// Pausing needs no settings validation and still advances the fence.
	generationSQL(t, store, `UPDATE agent_settings SET policy='{"version":1}' WHERE agent_id=$1`, peer.AgentID)
	if err := store.PauseAgent(ctx, peer.AgentID); err != nil {
		t.Fatal(err)
	}
	var corruptRevision int64
	if err := store.db.QueryRow(`SELECT pause_revision FROM agent_settings WHERE agent_id=$1`, peer.AgentID).Scan(&corruptRevision); err != nil || corruptRevision != 2 {
		t.Fatalf("corrupt pause revision: %d %v", corruptRevision, err)
	}
	// With the account active again, corrupt settings fail closed for the
	// trusted single resume.
	generationSQL(t, store, `UPDATE accounts SET disabled_at=NULL WHERE id=$1`, peer.AgentID)
	if validation := store.ResumeAgent(ctx, peer.AgentID); !errors.As(validation, new(*app.ValidationError)) {
		t.Fatalf("corrupt settings resume: %v", validation)
	}
	// Missing agents fail safely too.
	if err := store.PauseAgent(ctx, root.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing agent: %v", err)
	}
}

func TestGenerationControlsAllFleetSemantics(t *testing.T) {
	store, _, _ := generationSetup(t)
	ctx := context.Background()
	first := socialAgent(t, store, "fleet_first", nil)
	second := socialAgent(t, store, "fleet_second", nil)
	// An initially disabled seed is still a configured agent for --all.
	seed := schedulingSettings()
	seed.Enabled = false
	schedulingAdd(t, store, seed)

	// Pause targets every current settings row, including the seed.
	if count, err := store.PauseAllAgents(ctx); err != nil || count != 3 {
		t.Fatalf("pause all: %d %v", count, err)
	}
	for _, id := range []app.ID{first.AgentID, second.AgentID, seed.AgentID} {
		if got := schedulingRead(t, store, id); got.Enabled || got.PauseRevision != 1 {
			t.Fatalf("pause all settings: %+v", got)
		}
	}
	// A disabled account and a corrupt settings row survive the fleet pass.
	generationSQL(t, store, `UPDATE accounts SET disabled_at=clock_timestamp() WHERE id=$1`, second.AgentID)
	generationSQL(t, store, `UPDATE agent_settings SET policy='{"version":1}' WHERE agent_id=$1`, second.AgentID)
	if count, err := store.ResumeAllAgents(ctx); err != nil || count != 2 {
		t.Fatalf("resume all: %d %v", count, err)
	}
	if got := schedulingRead(t, store, first.AgentID); !got.Enabled || got.PauseRevision != 1 {
		t.Fatalf("resume all valid: %+v", got)
	}
	// The initially disabled seed is enabled by an explicit fleet resume too.
	if got := schedulingRead(t, store, seed.AgentID); !got.Enabled || got.PauseRevision != 1 {
		t.Fatalf("resume all seed: %+v", got)
	}
	// The corrupt settings row of the disabled account stays paused, its
	// revision does not move on resume, and the whole pass still succeeds.
	var frozen bool
	var frozenRevision int64
	if err := store.db.QueryRow(`SELECT enabled,pause_revision FROM agent_settings WHERE agent_id=$1`, second.AgentID).Scan(&frozen, &frozenRevision); err != nil || frozen || frozenRevision != 1 {
		t.Fatalf("resume all corrupt/disabled: %v %d %v", frozen, frozenRevision, err)
	}
	// Every targeted pause advances the revision, even already paused or corrupt
	// agents. The corrupted policy makes trusted reads unavailable, so the final
	// state is read through SQL directly.
	if count, err := store.PauseAllAgents(ctx); err != nil || count != 3 {
		t.Fatalf("second pause all: %d %v", count, err)
	}
	for _, id := range []app.ID{first.AgentID, seed.AgentID} {
		if got := schedulingRead(t, store, id); got.Enabled || got.PauseRevision != 2 {
			t.Fatalf("second pause revision: %+v", got)
		}
	}
	var secondPause int64
	if err := store.db.QueryRow(`SELECT pause_revision FROM agent_settings WHERE agent_id=$1`, second.AgentID).Scan(&secondPause); err != nil || secondPause != 2 {
		t.Fatalf("second pause corrupt revision: %d %v", secondPause, err)
	}
}

func TestGenerationControlsLockOrderAndCancellation(t *testing.T) {
	accountSQL := `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`
	settingsSQL := `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`
	jobSQL := `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`
	for _, stage := range []string{"account", "settings", "fleet_settings"} {
		t.Run(stage, func(t *testing.T) {
			store, agent, _ := controlSchedulingFixture(t)
			peer := socialAgent(t, store, "lock_peer", nil)
			ctx := context.Background()
			// The fleet pass and single-agent paths never touch jobs; provable
			// via `jobSQL` probes. A schedule pass job is available to lock.
			scheduled := socialJobs(t, store)[0]
			blocker, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			var run func(callCtx context.Context) error
			fleet := stage == "fleet_settings"
			switch stage {
			case "account":
				run = func(callCtx context.Context) error { return store.PauseAgent(callCtx, agent.AgentID) }
				if _, err := blocker.ExecContext(ctx, accountSQL, agent.AgentID); err != nil {
					t.Fatal(err)
				}
			case "settings":
				run = func(callCtx context.Context) error { return store.PauseAgent(callCtx, agent.AgentID) }
				if _, err := blocker.ExecContext(ctx, settingsSQL, agent.AgentID); err != nil {
					t.Fatal(err)
				}
			default:
				run = func(callCtx context.Context) error { _, err := store.PauseAllAgents(callCtx); return err }
				// Hold the settings row that sorts first — the earliest settings
				// acquisition the fleet pass reaches.
				firstID, secondID := agent.AgentID, peer.AgentID
				if secondID < firstID {
					firstID, secondID = secondID, firstID
				}
				if _, err := blocker.ExecContext(ctx, settingsSQL, firstID); err != nil {
					t.Fatal(err)
				}
				agent.AgentID, peer.AgentID = secondID, secondID
			}
			var pid int
			if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(callCtx) }()
			waitForDatabaseBlock(t, store, pid)
			if stage == "settings" {
				// While waiting for settings, the control holds the account
				// (EARLIER order) SHARE and no job lock (LATER order).
				assertEligibilityLock(t, store, accountSQL, agent.AgentID, false)
				assertEligibilityLock(t, store, jobSQL, scheduled.ID, true)
				assertEligibilityLock(t, store, `SELECT agent_id FROM agent_settings WHERE agent_id=$1 FOR UPDATE`, peer.AgentID, true)
			}
			if stage == "account" {
				// Settings are never locked while an earlier-order account waits.
				assertEligibilityLock(t, store, settingsSQL, agent.AgentID, true)
				assertEligibilityLock(t, store, jobSQL, scheduled.ID, true)
			}
			if fleet {
				// All accounts precede all settings in the fleet pass: the
				// later settings row is still free while the earlier one waits.
				assertEligibilityLock(t, store, accountSQL, agent.AgentID, false)
				assertEligibilityLock(t, store, settingsSQL, agent.AgentID, true)
				assertEligibilityLock(t, store, jobSQL, scheduled.ID, true)
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
				return
			}
			// The single-agent paths serialize and finish once released.
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if stage == "account" {
				if err := <-done; err != nil {
					t.Fatalf("waited account stage: %v", err)
				}
				if got := schedulingRead(t, store, agent.AgentID); got.Enabled || got.PauseRevision != 1 {
					t.Fatalf("post-wait pause: %+v", got)
				}
				return
			}
			// The settings stage waits behind the blocker, commits to enabled=false
			// with an advanced revision, showing settings serialization.
			if err := <-done; err != nil {
				t.Fatalf("waited settings stage: %v", err)
			}
			if got := schedulingRead(t, store, agent.AgentID); got.Enabled || got.PauseRevision != 1 {
				t.Fatalf("post-wait settings state: %+v", got)
			}
		})
	}
}

func TestGenerationControlsConcurrentPauseRevisions(t *testing.T) {
	store, agent, _ := controlSchedulingFixture(t)
	ctx := context.Background()
	const workers = 4
	var group sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		group.Go(func() {
			<-start
			if err := store.PauseAgent(ctx, agent.AgentID); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	group.Wait()
	if got := schedulingRead(t, store, agent.AgentID); got.Enabled || got.PauseRevision != workers {
		t.Fatalf("concurrent pauses lost or miscounted revisions: %+v", got)
	}
	if err := store.ResumeAgent(ctx, agent.AgentID); err != nil {
		t.Fatal(err)
	}
	if got := schedulingRead(t, store, agent.AgentID); !got.Enabled || got.PauseRevision != workers {
		t.Fatalf("resume changed revision: %+v", got)
	}
}
