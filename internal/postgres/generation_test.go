package postgres

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

func generationPolicy() app.GenerationPolicy {
	return app.GenerationPolicy{
		Version: 1, Timezone: "UTC", ActiveStart: "09:00", ActiveEnd: "17:00",
		ScheduledMinPerDay: 1, ScheduledMaxPerDay: 2, MinSpacingSeconds: 3600,
		ResponseMinDelaySeconds: 60, ResponseMaxDelaySeconds: 300, SourceMaxAgeSeconds: 3600,
		ReplyProbabilityBPS: 2500, RepostProbabilityBPS: 1000, QuoteProbabilityBPS: 2000,
		HumanPostProbabilityBPS: 500, ContinuationProbabilityBPS: 500, CooldownSeconds: 1800,
		ScheduledPostCapPerDay: 2, ReplyCapPerDay: 10, ReplyCapPerConversation: 2,
		MaxAgentsPerTrigger: 2, HumanTriggerCapPerWindow: 5, HumanTriggerWindowSeconds: 3600,
		MaxChainDepth: 2, MaxChainJobs: 5, DailyTokenBudget: 50000,
	}
}

func generationSetup(t *testing.T) (*Store, app.Persona, app.GenerationJob) {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	agentID := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: agentID, Type: app.AccountAgent, Handle: "generation_agent", DisplayName: "Agent", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	persona := app.Persona{AgentID: agentID, Version: 1, Instructions: "Discuss practical Go.", TopicTags: []string{"go", "databases"}, CreatedAt: now}
	if err := store.CreatePersona(ctx, persona); err != nil {
		t.Fatal(err)
	}
	id := app.NewID()
	job := app.GenerationJob{ID: id, AgentID: agentID, PersonaVersion: 1, TriggerKind: app.TriggerScheduled, TriggerKey: "scheduled:2026-09-20:1",
		OutputKind: app.OutputPost, RootJobID: id, Status: app.JobPending, AvailableAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := store.CreateGenerationJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	return store, persona, job
}

func TestGenerationStoreRoundTripAndInitialization(t *testing.T) {
	store, persona, job := generationSetup(t)
	ctx := context.Background()
	savedPersona, err := store.PersonaByVersion(ctx, persona.AgentID, persona.Version)
	if err != nil || app.ValidatePersonaUnchanged(persona, savedPersona) != nil {
		t.Fatalf("persona round trip: %+v %v", savedPersona, err)
	}
	savedJob, err := store.GenerationJobByID(ctx, job.ID)
	if err != nil || savedJob.ID != job.ID || savedJob.TriggerKey != job.TriggerKey || !savedJob.ExpiresAt.Equal(job.ExpiresAt) || savedJob.Status != app.JobPending {
		t.Fatalf("job round trip: %+v %v", savedJob, err)
	}
	persona.Instructions = "Do not overwrite me"
	if err := store.CreatePersona(ctx, persona); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("duplicate persona = %v", err)
	}
	settings := app.AgentSettings{AgentID: persona.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: job.CreatedAt}
	if _, err := store.InitializeAgentSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	// Operator changed selected version, enabled/pause state, policy and sampled
	// schedule. Reinitialization must return every field without overwriting it.
	persona.Version = 2
	if err := store.CreatePersona(ctx, persona); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE agent_settings SET persona_version=2,enabled=true,
		policy=jsonb_set(policy,'{daily_token_budget}','12345'),next_post_at='2026-09-21 10:00:00Z',
		schedule_date='2026-09-21',remaining_slots=2,last_published_at='2026-09-20 10:00:00Z',updated_at='2026-09-20 11:00:00Z'
		WHERE agent_id=$1`, persona.AgentID); err != nil {
		t.Fatal(err)
	}
	want, err := store.AgentSettingsByID(ctx, persona.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.InitializeAgentSettings(ctx, settings)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("initialization overwrote operator state: got=%+v want=%+v err=%v", got, want, err)
	}
	if got.PersonaVersion != 2 || got.Policy.DailyTokenBudget != 12345 || !got.Enabled {
		t.Fatal("operator changes were not read")
	}
	if _, err := store.PersonaByVersion(ctx, persona.AgentID, 99); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing persona = %v", err)
	}
	if _, err := store.GenerationJobByID(ctx, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing job = %v", err)
	}
	if _, err := store.GenerationAttemptByID(ctx, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing attempt = %v", err)
	}
}

func TestGenerationStoreValidationAndRollback(t *testing.T) {
	store, persona, job := generationSetup(t)
	ctx := context.Background()
	human, _ := contentTestActor(t, store, "generation_human")
	badPersona := persona
	badPersona.AgentID = human
	var validation *app.ValidationError
	if err := store.CreatePersona(ctx, badPersona); !errors.As(err, &validation) {
		t.Fatalf("human persona accepted: %v", err)
	}
	badPersona = persona
	badPersona.TopicTags = []string{"not a slug"}
	if err := store.CreatePersona(ctx, badPersona); !errors.As(err, &validation) {
		t.Fatalf("invalid persona accepted: %v", err)
	}
	settings := app.AgentSettings{AgentID: human, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: job.CreatedAt}
	if _, err := store.InitializeAgentSettings(ctx, settings); !errors.As(err, &validation) {
		t.Fatalf("human settings accepted: %v", err)
	}
	settings.AgentID = persona.AgentID
	settings.Policy.Version = 2
	if _, err := store.InitializeAgentSettings(ctx, settings); !errors.As(err, &validation) {
		t.Fatalf("invalid policy accepted: %v", err)
	}
	badJob := job
	badJob.AgentID = human
	if err := store.CreateGenerationJob(ctx, badJob); !errors.As(err, &validation) {
		t.Fatalf("human job accepted: %v", err)
	}
	badJob = job
	badJob.Status = app.JobRunning
	badJob.LeaseVersion = 1
	lease := job.CreatedAt.Add(time.Minute)
	badJob.LeaseExpiresAt = &lease
	if err := store.CreateGenerationJob(ctx, badJob); !errors.As(err, &validation) {
		t.Fatalf("noninitial job accepted: %v", err)
	}
	persona.Version = 2
	settings.PersonaVersion, settings.Policy = 2, generationPolicy()
	job.ID = app.NewID()
	job.RootJobID, job.PersonaVersion, job.TriggerKey = job.ID, 2, "rolled_back"
	rollback := errors.New("rollback fixture")
	err := store.Transaction(ctx, func(q *Queries) error {
		if err := q.CreatePersona(ctx, persona); err != nil {
			return err
		}
		if _, err := q.PersonaByVersion(ctx, persona.AgentID, 2); err != nil {
			return err
		}
		if _, err := q.InitializeAgentSettings(ctx, settings); err != nil {
			return err
		}
		if err := q.CreateGenerationJob(ctx, job); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err := store.PersonaByVersion(ctx, persona.AgentID, 2); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("persona escaped rollback: %v", err)
	}
	if _, err := store.AgentSettingsByID(ctx, persona.AgentID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("settings escaped rollback: %v", err)
	}
	if _, err := store.GenerationJobByID(ctx, job.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("job escaped rollback: %v", err)
	}
	if err := store.Queries.CreatePersona(ctx, persona); !errors.Is(err, errGenerationTransaction) {
		t.Fatalf("pool-bound write accepted: %v", err)
	}
}

func TestGenerationStoreConcurrentTriggerAndSettings(t *testing.T) {
	store, persona, original := generationSetup(t)
	ctx := context.Background()
	settings := app.AgentSettings{AgentID: persona.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: original.CreatedAt}
	const workers = 8
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			if _, err := store.InitializeAgentSettings(ctx, settings); err != nil {
				errorsFound <- err
				return
			}
			job := original
			job.ID = app.NewID()
			job.RootJobID, job.TriggerKey = job.ID, "concurrent-slot"
			errorsFound <- store.CreateGenerationJob(ctx, job)
		})
	}
	group.Wait()
	close(errorsFound)
	successes, conflicts := 0, 0
	for err := range errorsFound {
		if err == nil {
			successes++
		} else if errors.Is(err, app.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != workers-1 {
		t.Fatalf("success=%d conflicts=%d", successes, conflicts)
	}
	// Changed expiry/identity cannot convert the stable original trigger to a
	// fresh job, even when submitted after completion in a later enqueue path.
	changed := original
	changed.ID = app.NewID()
	changed.RootJobID, changed.ExpiresAt = changed.ID, original.ExpiresAt.Add(time.Hour)
	if err := store.CreateGenerationJob(ctx, changed); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("trigger replacement = %v", err)
	}
	saved, err := store.GenerationJobByID(ctx, original.ID)
	if err != nil || !saved.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatalf("trigger changed: %+v %v", saved, err)
	}
}

func TestGenerationStoreRejectsCorruptReads(t *testing.T) {
	store, persona, job := generationSetup(t)
	ctx := context.Background()
	settings := app.AgentSettings{AgentID: persona.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: job.CreatedAt}
	if _, err := store.InitializeAgentSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{`{"version":1}`, `{"version":1,"secret":"never report me"}`} {
		if _, err := store.db.ExecContext(ctx, `UPDATE agent_settings SET policy=$1 WHERE agent_id=$2`, policy, persona.AgentID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AgentSettingsByID(ctx, persona.AgentID); !errors.Is(err, app.ErrUnavailable) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe corrupt policy read: %v", err)
		}
		if _, err := store.InitializeAgentSettings(ctx, settings); !errors.Is(err, app.ErrUnavailable) {
			t.Fatalf("initialization ignored existing corruption: %v", err)
		}
	}
	// SQL intentionally checks array shape rather than duplicating every domain
	// slug rule. A legacy/direct SQL bad persona must fail safely on reads.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agent_personas(agent_id,version,instructions,topic_tags,created_at)
		VALUES($1,2,'private instructions',ARRAY['Bad Tag'],now())`, persona.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PersonaByVersion(ctx, persona.AgentID, 2); !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("invalid persona read = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE accounts SET type='human' WHERE id=$1`, persona.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PersonaByVersion(ctx, persona.AgentID, 1); !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("human persona read = %v", err)
	}
	if _, err := store.GenerationJobByID(ctx, job.ID); !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("human job read = %v", err)
	}
}
