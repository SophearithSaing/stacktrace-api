package postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func schedulingTime() time.Time  { return time.Date(2026, 9, 20, 10, 0, 0, 123456000, time.UTC) }
func schedulingDraw(int64) int64 { return 0 }

func schedulingSettings() app.AgentSettings {
	policy := generationPolicy()
	policy.ScheduledMinPerDay, policy.ScheduledMaxPerDay, policy.ScheduledPostCapPerDay = 3, 3, 3
	return app.AgentSettings{AgentID: app.NewID(), PersonaVersion: 1, Enabled: true, Policy: policy, UpdatedAt: schedulingTime()}
}

func schedulingStore(t *testing.T) *Store {
	t.Helper()
	store := testStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func schedulingAdd(t *testing.T, store *Store, settings app.AgentSettings) {
	t.Helper()
	ctx := context.Background()
	err := store.Transaction(ctx, func(q *Queries) error {
		if err := q.CreateAccount(ctx, app.Account{ID: settings.AgentID, Type: app.AccountAgent, Handle: "agent_" + strings.ReplaceAll(string(settings.AgentID), "-", "")[:24], DisplayName: "Agent", CreatedAt: settings.UpdatedAt, UpdatedAt: settings.UpdatedAt}); err != nil {
			return err
		}
		if err := q.CreatePersona(ctx, app.Persona{AgentID: settings.AgentID, Version: 1, Instructions: "Discuss Go.", TopicTags: []string{"go"}, CreatedAt: settings.UpdatedAt}); err != nil {
			return err
		}
		_, err := q.InitializeAgentSettings(ctx, settings)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func schedulingJob(t *testing.T, store *Store, agentID app.ID) app.GenerationJob {
	t.Helper()
	var id app.ID
	if err := store.db.QueryRow(`SELECT id FROM generation_jobs WHERE agent_id=$1 ORDER BY created_at DESC,id LIMIT 1`, agentID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	job, err := store.GenerationJobByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func schedulingRead(t *testing.T, store *Store, agentID app.ID) app.AgentSettings {
	t.Helper()
	settings, err := store.AgentSettingsByID(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestGenerationSchedulePersistenceAndRestart(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	schedulingAdd(t, store, settings)
	ctx := context.Background()
	now := schedulingTime().Add(789 * time.Nanosecond)
	result, err := store.scheduleGeneration(ctx, &now, schedulingDraw)
	if err != nil || result.AgentsVisited != 1 || result.JobsEnqueued != 1 || result.InvalidAgents != 0 {
		t.Fatalf("first pass: %+v %v", result, err)
	}
	job := schedulingJob(t, store, settings.AgentID)
	key, _ := app.ScheduledGenerationKey(now)
	if job.TriggerKey != key || !job.CreatedAt.Equal(app.GenerationInstant(now)) || !job.AvailableAt.Equal(job.CreatedAt) || !job.ExpiresAt.Equal(job.CreatedAt.Add(time.Hour)) || job.MaxChainDepth != settings.Policy.MaxChainDepth || job.MaxChainJobs != settings.Policy.MaxChainJobs {
		t.Fatalf("scheduled job: %+v", job)
	}
	saved := schedulingRead(t, store, settings.AgentID)
	if saved.ScheduleDate != "2026-09-20" || saved.RemainingSlots != 2 || !saved.NextPostAt.Equal(job.CreatedAt.Add(time.Hour)) {
		t.Fatalf("progress: %+v", saved)
	}
	// New Store/Queries object: no in-memory cursor or random state is required.
	restarted := &Store{db: store.db, Queries: &Queries{queryer: store.db}}
	result, err = restarted.scheduleGeneration(ctx, &now, func(int64) int64 { t.Fatal("restart rerolled future slot"); return 0 })
	if err != nil || result.JobsEnqueued != 0 {
		t.Fatalf("restart: %+v %v", result, err)
	}
	again := schedulingRead(t, store, settings.AgentID)
	again.UpdatedAt = saved.UpdatedAt // Fair inspection timestamp is expected to move.
	if !reflect.DeepEqual(saved, again) {
		t.Fatalf("restart changed schedule: %+v %+v", saved, again)
	}
	if duplicate := schedulingJob(t, store, settings.AgentID); duplicate.ID != job.ID {
		t.Fatal("restart duplicated job")
	}
	now = now.Add(24 * time.Hour)
	result, err = store.scheduleGeneration(ctx, &now, schedulingDraw)
	if err != nil || result.JobsEnqueued != 1 || result.JobsExpired != 1 {
		t.Fatalf("rollover: %+v %v", result, err)
	}
	saved = schedulingRead(t, store, settings.AgentID)
	if saved.ScheduleDate != "2026-09-21" || saved.RemainingSlots != 2 {
		t.Fatalf("rollover progress: %+v", saved)
	}
}

func TestGenerationScheduleSampledZero(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	settings.Policy.ScheduledMinPerDay = 0
	schedulingAdd(t, store, settings)
	now := schedulingTime()
	calls := 0
	draw := func(int64) int64 { calls++; return 0 }
	result, err := store.scheduleGeneration(context.Background(), &now, draw)
	if err != nil || calls != 1 || result.JobsEnqueued != 0 {
		t.Fatalf("zero sample: %+v calls=%d %v", result, calls, err)
	}
	saved := schedulingRead(t, store, settings.AgentID)
	if saved.ScheduleDate != "2026-09-20" || saved.NextPostAt != nil || saved.RemainingSlots != 0 {
		t.Fatalf("zero progress: %+v", saved)
	}
	result, err = store.scheduleGeneration(context.Background(), &now, draw)
	if err != nil || calls != 1 || result.JobsEnqueued != 0 {
		t.Fatalf("zero rerolled: %+v calls=%d %v", result, calls, err)
	}
	now = now.Add(24 * time.Hour)
	_, err = store.scheduleGeneration(context.Background(), &now, draw)
	if err != nil || calls != 2 {
		t.Fatalf("zero day never rolled over: calls=%d %v", calls, err)
	}
}

func TestGenerationScheduleAtomicRollback(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	schedulingAdd(t, store, settings)
	now := schedulingTime()
	before := schedulingRead(t, store, settings.AgentID)
	abort := errors.New("abort schedule")
	err := store.Transaction(context.Background(), func(q *Queries) error {
		result, err := q.scheduleGenerationAgent(context.Background(), settings.AgentID, &now, schedulingDraw)
		if err != nil {
			return err
		}
		if result.JobsEnqueued != 1 {
			t.Fatalf("no work to roll back: %+v", result)
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	after := schedulingRead(t, store, settings.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("schedule escaped transaction rollback")
	}
	var jobs int
	if err := store.db.QueryRow(`SELECT count(*) FROM generation_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("jobs escaped rollback: %d %v", jobs, err)
	}
	// A database failure after advancing the schedule also rolls back inspection
	// time/progress. Do not return successful counts from an uncommitted agent.
	generationSQL(t, store, `CREATE FUNCTION reject_schedule_job() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test rejection'; END $$;
		CREATE TRIGGER reject_schedule_job BEFORE INSERT ON generation_jobs FOR EACH ROW EXECUTE FUNCTION reject_schedule_job()`)
	result, err := store.scheduleGeneration(context.Background(), &now, schedulingDraw)
	if !errors.Is(err, app.ErrUnavailable) || result.AgentsVisited != 0 || result.JobsEnqueued != 0 {
		t.Fatalf("failed commit counts: %+v %v", result, err)
	}
	after = schedulingRead(t, store, settings.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed insert consumed schedule")
	}
}

func TestGenerationScheduleDowntimeAndDuplicate(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	settings.Policy.ScheduledMinPerDay, settings.Policy.ScheduledMaxPerDay, settings.Policy.ScheduledPostCapPerDay = 8, 8, 8
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	settings.ScheduleDate, settings.RemainingSlots, settings.NextPostAt = "2026-09-20", 8, &at
	schedulingAdd(t, store, settings)
	now := at.Add(5*time.Hour + 30*time.Minute)
	result, err := store.scheduleGeneration(context.Background(), &now, schedulingDraw)
	if err != nil || result.JobsEnqueued != 1 {
		t.Fatalf("downtime: %+v %v", result, err)
	}
	job := schedulingJob(t, store, settings.AgentID)
	key, _ := app.ScheduledGenerationKey(at.Add(5 * time.Hour))
	if job.TriggerKey != key || !job.CreatedAt.Equal(now) || !job.ExpiresAt.Equal(at.Add(6*time.Hour)) {
		t.Fatalf("original expiry lost: %+v", job)
	}
	saved := schedulingRead(t, store, settings.AgentID)
	if saved.RemainingSlots != 2 || !saved.NextPostAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("catch-up burst: %+v", saved)
	}
	result, err = store.scheduleGeneration(context.Background(), &now, nil)
	if err != nil || result.JobsEnqueued != 0 {
		t.Fatalf("repeated pass burst: %+v %v", result, err)
	}
	// Simulate schedule progress behind an already-reserved slot. Replay must
	// advance, not abort or extend original expiry, even when spacing denies it.
	previous := at.Add(5 * time.Hour)
	generationSQL(t, store, `UPDATE agent_settings SET next_post_at=$2,remaining_slots=3 WHERE agent_id=$1`, settings.AgentID, previous)
	result, err = store.scheduleGeneration(context.Background(), &now, schedulingDraw)
	if err != nil || result.JobsEnqueued != 0 || result.SlotsDenied != 1 {
		t.Fatalf("identity conflict: %+v %v", result, err)
	}
	if same := schedulingJob(t, store, settings.AgentID); same.ID != job.ID || !same.ExpiresAt.Equal(job.ExpiresAt) {
		t.Fatal("duplicate changed expiry")
	}
	saved = schedulingRead(t, store, settings.AgentID)
	if saved.RemainingSlots != 2 || !saved.NextPostAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("conflict did not advance: %+v", saved)
	}
}
