package postgres

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationScheduleConcurrentPasses(t *testing.T) {
	store := schedulingStore(t)
	settings := schedulingSettings()
	schedulingAdd(t, store, settings)
	now := schedulingTime()
	var workers sync.WaitGroup
	counts := make(chan int, 8)
	start := make(chan struct{})
	for range 8 {
		workers.Go(func() {
			<-start
			result, err := store.scheduleGeneration(context.Background(), &now, schedulingDraw)
			if err != nil {
				t.Errorf("concurrent schedule: %v", err)
				return
			}
			counts <- result.JobsEnqueued
		})
	}
	close(start)
	workers.Wait()
	close(counts)
	enqueued := 0
	for count := range counts {
		enqueued += count
	}
	var stored int
	if err := store.db.QueryRow(`SELECT count(*) FROM generation_jobs`).Scan(&stored); err != nil || enqueued != 1 || stored != 1 {
		t.Fatalf("duplicate schedule: enqueued=%d stored=%d %v", enqueued, stored, err)
	}
	saved := schedulingRead(t, store, settings.AgentID)
	if saved.RemainingSlots != 2 || !saved.NextPostAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("concurrent advancement: %+v", saved)
	}
}

func TestGenerationScheduleFairDiscovery(t *testing.T) {
	store := schedulingStore(t)
	var validIDs, invalidIDs []app.ID
	// Future timestamps must not leave valid or invalid configurations stranded
	// behind rows repeatedly touched with the current database clock.
	original := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range generationScheduleBatch*2 + 3 {
		settings := schedulingSettings()
		settings.UpdatedAt = original
		settings.Policy.ScheduledMinPerDay, settings.Policy.ScheduledMaxPerDay, settings.Policy.ScheduledPostCapPerDay = 0, 0, 0
		if i%4 == 1 {
			settings.ScheduleDate = "2026-09-19"
		}
		if i%4 == 2 {
			settings.ScheduleDate = "2026-09-20"
		}
		schedulingAdd(t, store, settings)
		if i%4 == 3 {
			generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{timezone}','"invalid"') WHERE agent_id=$1`, settings.AgentID)
			invalidIDs = append(invalidIDs, settings.AgentID)
		} else {
			validIDs = append(validIDs, settings.AgentID)
		}
	}
	now := schedulingTime()
	for day := range 2 {
		for range 3 {
			result, err := store.scheduleGeneration(context.Background(), &now, schedulingDraw)
			if err != nil || result.AgentsVisited != generationScheduleBatch || result.JobsEnqueued != 0 {
				t.Fatalf("bounded pass: %+v %v", result, err)
			}
		}
		for _, id := range validIDs {
			saved := schedulingRead(t, store, id)
			if saved.ScheduleDate != now.Format(time.DateOnly) || saved.UpdatedAt.Equal(original) || saved.RemainingSlots != 0 {
				t.Fatalf("starved agent day=%d: %+v", day, saved)
			}
		}
		for _, id := range invalidIDs {
			var inspected time.Time
			if err := store.db.QueryRow(`SELECT updated_at FROM agent_settings WHERE agent_id=$1`, id).Scan(&inspected); err != nil || inspected.Equal(original) {
				t.Fatalf("invalid config stranded: %v", err)
			}
		}
		now = now.Add(24 * time.Hour)
	}
}

func TestGenerationScheduleFleetBound(t *testing.T) {
	store := schedulingStore(t)
	policy, err := json.Marshal(generationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	generationSQL(t, store, `INSERT INTO accounts(id,type,handle,display_name,created_at,updated_at)
		SELECT md5(n::text)::uuid,'agent','agent_'||n,'Agent',now(),now() FROM generate_series(1,$1) n`, maxConfiguredAgents+1)
	generationSQL(t, store, `INSERT INTO agent_personas SELECT id,1,'Technical AI persona',ARRAY['go'],created_at FROM accounts`)
	generationSQL(t, store, `INSERT INTO agent_settings(agent_id,persona_version,enabled,policy,remaining_slots,updated_at)
		SELECT id,1,true,$1,0,created_at FROM accounts WHERE handle<>'agent_1001'`, policy)
	ids, err := store.generationScheduleCandidates(context.Background())
	if err != nil || len(ids) != generationScheduleBatch {
		t.Fatalf("fleet boundary: count=%d %v", len(ids), err)
	}
	generationSQL(t, store, `INSERT INTO agent_settings(agent_id,persona_version,enabled,policy,remaining_slots,updated_at)
		SELECT id,1,true,$1,0,created_at FROM accounts WHERE handle='agent_1001'`, policy)
	result, err := store.ScheduleGeneration(context.Background())
	if err == nil || result.AgentsVisited != 0 || result.JobsEnqueued != 0 {
		t.Fatalf("over-fleet pass mutated: %+v %v", result, err)
	}
}

func TestGenerationScheduleClockAfterLocks(t *testing.T) {
	for _, table := range []string{"accounts", "agent_settings"} {
		t.Run(table, func(t *testing.T) {
			store := schedulingStore(t)
			settings := schedulingSettings()
			var before time.Time
			if err := store.db.QueryRow(`SELECT clock_timestamp()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			settings.Policy.ActiveStart, settings.Policy.ActiveEnd = "00:00", "23:59"
			// Keep this live-clock test away from the unsupported final minute of
			// a same-day HH:MM window, regardless of when the suite executes.
			if before.UTC().Hour() == 23 {
				settings.Policy.Timezone = "Etc/GMT+12"
			}
			location, err := time.LoadLocation(settings.Policy.Timezone)
			if err != nil {
				t.Fatal(err)
			}
			settings.ScheduleDate, settings.RemainingSlots, settings.NextPostAt = before.In(location).Format(time.DateOnly), 1, &before
			schedulingAdd(t, store, settings)
			ctx := context.Background()
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			column := "id"
			if table == "agent_settings" {
				column = "agent_id"
			}
			if _, err := tx.ExecContext(ctx, `SELECT `+column+` FROM `+table+` WHERE `+column+`=$1 FOR UPDATE`, settings.AgentID); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				result, err := store.ScheduleGeneration(ctx)
				if err == nil && result.JobsEnqueued != 1 {
					t.Errorf("production schedule counts: %+v", result)
				}
				done <- err
			}()
			waitForDatabaseBlock(t, store, pid)
			var released time.Time
			if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&released); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			job := schedulingJob(t, store, settings.AgentID)
			if job.CreatedAt.Before(released) || !job.AvailableAt.Equal(job.CreatedAt) || !job.ExpiresAt.Equal(before.Add(time.Hour)) {
				t.Fatalf("clock obtained before lock wait: before=%v released=%v job=%+v", before, released, job)
			}
		})
	}
}
