package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationScheduleDisabledAndInvalid(t *testing.T) {
	store := schedulingStore(t)
	healthy := schedulingSettings()
	schedulingAdd(t, store, healthy)
	for _, mutation := range []string{
		`policy=jsonb_set(policy,'{timezone}','"Not/AZone"')`,
		`policy=policy-'source_max_age_seconds'`,
		`policy=jsonb_set(policy,'{timezone}',to_jsonb(repeat('x',9000)))`,
		`remaining_slots=1,schedule_date='2026-09-20'`, // No sampled next slot.
	} {
		settings := schedulingSettings()
		schedulingAdd(t, store, settings)
		generationSQL(t, store, `UPDATE agent_settings SET `+mutation+` WHERE agent_id=$1`, settings.AgentID)
	}
	disabledAccount, disabledSettings := schedulingSettings(), schedulingSettings()
	disabledSettings.Enabled = false
	schedulingAdd(t, store, disabledAccount)
	schedulingAdd(t, store, disabledSettings)
	generationSQL(t, store, `UPDATE accounts SET disabled_at=clock_timestamp() WHERE id=$1`, disabledAccount.AgentID)
	now := schedulingTime()
	result, err := store.scheduleGeneration(context.Background(), &now, schedulingDraw)
	if err != nil || result.AgentsVisited != 5 || result.InvalidAgents != 4 || result.JobsEnqueued != 1 {
		t.Fatalf("safe optional config handling: %+v %v", result, err)
	}
	for _, settings := range []app.AgentSettings{disabledAccount, disabledSettings} {
		saved := schedulingRead(t, store, settings.AgentID)
		if saved.NextPostAt != nil || saved.ScheduleDate != "" || !saved.UpdatedAt.Equal(settings.UpdatedAt) {
			t.Fatalf("disabled schedule changed: %+v", saved)
		}
	}
	if job := schedulingJob(t, store, healthy.AgentID); job.AgentID != healthy.AgentID {
		t.Fatal("healthy agent was starved by invalid config")
	}
	// Recheck eligibility in the transaction even when discovery saw enabled.
	for _, settings := range []app.AgentSettings{disabledAccount, disabledSettings} {
		err := store.Transaction(context.Background(), func(q *Queries) error {
			result, err := q.scheduleGenerationAgent(context.Background(), settings.AgentID, &now, schedulingDraw)
			if result.AgentsVisited != 0 || result.JobsEnqueued != 0 {
				t.Fatalf("disabled after discovery: %+v", result)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestGenerationScheduleLocalQuotaIncludesTerminalJobs(t *testing.T) {
	// At 00:30 UTC, Los Angeles is still on the previous local day. A cancelled
	// reservation on the previous UTC date must count, but the prior local day
	// must not. Both created_at boundaries are exercised, including DST midnight.
	for _, tt := range []struct {
		name, zone, now, created string
		want                     int
	}{
		{"same local previous UTC", "America/Los_Angeles", "2026-09-21T00:30:00Z", "2026-09-20T18:00:00Z", 0},
		{"prior local day", "America/Los_Angeles", "2026-09-21T00:30:00Z", "2026-09-20T06:59:59.999999Z", 1},
		{"local start inclusive", "America/Los_Angeles", "2026-09-21T00:30:00Z", "2026-09-20T07:00:00Z", 0},
		{"missing midnight prior day", "America/Sao_Paulo", "2018-11-04T12:00:00Z", "2018-11-04T02:59:59.999999Z", 1},
		{"missing midnight day start", "America/Sao_Paulo", "2018-11-04T12:00:00Z", "2018-11-04T03:00:00Z", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := schedulingStore(t)
			settings := schedulingSettings()
			settings.Policy.Timezone, settings.Policy.ActiveStart, settings.Policy.ActiveEnd = tt.zone, "00:00", "23:59"
			settings.Policy.ScheduledMinPerDay, settings.Policy.ScheduledMaxPerDay, settings.Policy.ScheduledPostCapPerDay = 1, 1, 1
			schedulingAdd(t, store, settings)
			now, err := time.Parse(time.RFC3339Nano, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			created, err := time.Parse(time.RFC3339Nano, tt.created)
			if err != nil {
				t.Fatal(err)
			}
			id := app.NewID()
			job := app.GenerationJob{ID: id, AgentID: settings.AgentID, PersonaVersion: 1, TriggerKind: app.TriggerScheduled,
				TriggerKey: "reserved-slot", OutputKind: app.OutputPost, RootJobID: id, MaxChainDepth: 0, MaxChainJobs: 1,
				Status: app.JobPending, AvailableAt: created, ExpiresAt: created.Add(time.Hour), CreatedAt: created}
			if err := store.CreateGenerationJob(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='source_removed',finished_at=created_at WHERE id=$1`, id)
			result, err := store.scheduleGeneration(context.Background(), &now, schedulingDraw)
			if err != nil || result.JobsEnqueued != tt.want || result.SlotsDenied != 1-tt.want {
				t.Fatalf("local quota: %+v %v", result, err)
			}
			saved := schedulingRead(t, store, settings.AgentID)
			if saved.NextPostAt != nil || saved.RemainingSlots != 0 || saved.ScheduleDate == "" {
				t.Fatalf("denied allowance did not consume slot: %+v", saved)
			}
			result, err = store.scheduleGeneration(context.Background(), &now, nil)
			if err != nil || result.JobsEnqueued != 0 || result.SlotsDenied != 0 {
				t.Fatalf("quota denial rerolled: %+v %v", result, err)
			}
		})
	}
}

func TestGenerationScheduleStaleSlotAndOldDay(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	now := schedulingTime()
	at := now.Add(-time.Duration(settings.Policy.SourceMaxAgeSeconds) * time.Second)
	settings.ScheduleDate, settings.RemainingSlots, settings.NextPostAt = "2026-09-20", 1, &at
	schedulingAdd(t, store, settings)
	result, err := store.scheduleGeneration(context.Background(), &now, nil)
	if err != nil || result.JobsEnqueued != 0 {
		t.Fatalf("expired slot revived: %+v %v", result, err)
	}
	settings = schedulingRead(t, store, settings.AgentID)
	if settings.RemainingSlots != 0 || settings.NextPostAt != nil {
		t.Fatalf("stale slot not consumed: %+v", settings)
	}
	// A multi-day outage initializes only today's remaining interval.
	now = now.Add(72*time.Hour + 6*time.Hour + 30*time.Minute)
	result, err = store.scheduleGeneration(context.Background(), &now, schedulingDraw)
	if err != nil || result.JobsEnqueued != 1 {
		t.Fatalf("multi-day resume: %+v %v", result, err)
	}
	settings = schedulingRead(t, store, settings.AgentID)
	if settings.ScheduleDate != "2026-09-23" || settings.RemainingSlots != 0 || settings.NextPostAt != nil {
		t.Fatalf("backlog appeared: %+v", settings)
	}
}
