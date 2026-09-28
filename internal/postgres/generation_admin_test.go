package postgres

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestAdminListGenerationJobsBoundsAndTies(t *testing.T) {
	store, _, root := generationSetup(t)
	ctx := context.Background()
	// Three more jobs of the same agent with stable, close timestamps.
	for index := 1; index <= 3; index++ {
		before := root.AvailableAt.Add(time.Duration(index) * time.Second)
		additional := contextJob(t, store, root, map[string]any{"created_at": before, "available_at": before})
		_ = additional
	}
	page, next, err := store.ListGenerationJobs(ctx, AdminJobFilter{Limit: 2, AgentID: root.AgentID})
	if err != nil || len(page) != 2 || next == nil {
		t.Fatalf("first page: %d %v %v", len(page), next, err)
	}
	if page[0].CreatedAt.Before(page[1].CreatedAt) || page[0].CreatedAt.Equal(page[1].CreatedAt) {
		t.Fatal("page is not newest first with stable ties")
	}
	continuation, err := DecodeAdminCursor(*next)
	if err != nil {
		t.Fatal(err)
	}
	second, forward, err := store.ListGenerationJobs(ctx, AdminJobFilter{
		Limit: 64, AgentID: root.AgentID, Cursor: &AdminJobCursor{CreatedAt: page[1].CreatedAt, JobID: page[1].ID}})
	if err != nil || len(second) != 2 {
		t.Fatalf("second page: %d %v", len(second), err)
	}
	if forward != nil {
		t.Fatalf("unexpected cursor on the last page: %v", *forward)
	}
	// The cursor continues exactly after the page's last row.
	if second[0].CreatedAt.After(continuation.CreatedAt) || second[0].CreatedAt.Equal(continuation.CreatedAt) && second[0].ID != continuation.JobID {
		t.Fatal("cursor continued to stale rows")
	}
	for _, job := range second {
		if job.AgentID != root.AgentID {
			t.Fatal("filter leaked another agent")
		}
	}
	if _, _, err := store.ListGenerationJobs(ctx, AdminJobFilter{Status: "weird"}); err == nil {
		t.Fatal("invalid status set accepted")
	}
}

func TestAdminListGenerationJobsExactFiltersAndEqualTimeTies(t *testing.T) {
	store, _, root := generationSetup(t)
	ctx := context.Background()
	now := root.CreatedAt
	// Create several jobs at the exact same timestamp with different agents.
	otherAgent := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: otherAgent, Type: app.AccountAgent, Handle: "other_agent", DisplayName: "Other", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePersona(ctx, app.Persona{AgentID: otherAgent, Version: 1, Instructions: "Other.", TopicTags: []string{"go"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var ids []app.ID
	for i := 0; i < 5; i++ {
		agent := root.AgentID
		if i%2 == 1 {
			agent = otherAgent
		}
		id := app.NewID()
		ids = append(ids, id)
		job := app.GenerationJob{ID: id, AgentID: agent, PersonaVersion: 1, TriggerKind: app.TriggerScheduled, TriggerKey: string(app.NewID()),
			OutputKind: app.OutputPost, RootJobID: id, MaxChainDepth: 2, MaxChainJobs: 5, Status: app.JobPending,
			AvailableAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
		if err := store.CreateGenerationJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := store.ListGenerationJobs(ctx, AdminJobFilter{Limit: 2, AgentID: root.AgentID})
	if err != nil || len(page) != 2 || next == nil {
		t.Fatalf("agent filter page: %d %v %v", len(page), next, err)
	}
	// Equal-time pages must not leak the other agent and must not duplicate IDs.
	seen := make(map[app.ID]bool)
	for _, job := range page {
		if job.AgentID != root.AgentID {
			t.Fatalf("agent filter leaked: %v", job.AgentID)
		}
		if seen[job.ID] {
			t.Fatalf("duplicate job: %v", job.ID)
		}
		seen[job.ID] = true
	}
}

func TestAdminCursorValidation(t *testing.T) {
	token := AdminJobCursor{
		CreatedAt: app.GenerationInstant(time.Date(2026, 9, 28, 12, 0, 0, 123000, time.UTC)),
		JobID:     app.NewID(),
	}
	cursor, err := DecodeAdminCursor(token.token())
	if err != nil || !cursor.CreatedAt.Equal(token.CreatedAt) || cursor.JobID != token.JobID {
		t.Fatalf("cursor round trip: %+v %v", cursor, err)
	}
	if _, err := DecodeAdminCursor(""); err == nil {
		t.Fatal("empty cursor accepted")
	}
	if _, err := DecodeAdminCursor("bad|token"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	if _, err := DecodeAdminCursor("0001-01-01T00:00:00Z|00000000-0000-4000-8000-000000000001"); err == nil {
		t.Fatal("zero-time cursor accepted")
	}
}

func TestAdminInspectGenerationBounded(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	_ = admitSpend(t, store, job, input)
	// A fabricated extra attempt beyond the cap stays in history in one page.
	var startedAt time.Time
	var budgetDay string
	if err := store.db.QueryRow(`SELECT started_at,(started_at AT TIME ZONE 'UTC')::date
		FROM generation_attempts WHERE job_id=$1`, job.ID).Scan(&startedAt, &budgetDay); err != nil {
		t.Fatal(err)
	}
	foreignID := app.NewID()
	generationSQL(t, store, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
		context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,finished_at,error_code)
		VALUES($1,$2,2,2,'together','test-model',$3,'v1',$4,1000,'failed',$5,$6,'unsupported_accounting')`,
		foreignID, job.ID, strings.Repeat("a", 64), budgetDay, startedAt, startedAt)
	inspection, err := store.InspectGeneration(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Job.ID != job.ID || len(inspection.Attempts) != 2 || !inspection.Complete {
		t.Fatalf("inspect: %+v", inspection)
	}
	if _, err := store.InspectGeneration(ctx, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing inspection: %v", err)
	}
}

func TestAdminInspectGenerationTruncation(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	attempt := admitSpend(t, store, job, input)
	// Fabricate attempts up to and beyond the page cap.
	var startedAt time.Time
	if err := store.db.QueryRow(`SELECT started_at FROM generation_attempts WHERE id=$1`, attempt.ID).Scan(&startedAt); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= app.MaxGenerationAttempts+1; i++ {
		id := app.NewID()
		generationSQL(t, store, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
			context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,finished_at,error_code)
			VALUES($1,$2,$3,2,'together','test-model',$4,'v1',$5,1000,'failed',$6,$6,'provider_credentials')`,
			id, job.ID, i, strings.Repeat("a", 64), attempt.BudgetDay, startedAt)
	}
	inspection, err := store.InspectGeneration(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Attempts) != app.MaxGenerationAttempts {
		t.Fatalf("expected %d attempts, got %d", app.MaxGenerationAttempts, len(inspection.Attempts))
	}
	if inspection.Complete {
		t.Fatal("complete should be false when history exceeds the page")
	}
}

// TestAdminDayUsageConservative checks the exact cost rules of admission: full
// reservation while any call is unresolved or unknowable, exact tokens after a
// settled call, and separate fleet rows with the bounded agent count.
func TestAdminDayUsageConservative(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	attempt := admitSpend(t, store, job, input)
	// reserved/unknown full-cost accounting, matching admission.
	usage, err := store.DayGenerationUsage(ctx, attempt.BudgetDay, "")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Day != attempt.BudgetDay || usage.Attempts != 1 || usage.KnownTokens != 0 || usage.ChargedTokens != attempt.ReservedTokens {
		t.Fatalf("usage: %+v", usage)
	}
	// A settled successful call with counts switches to known tokens.
	output, err := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A prudent usage report."}`), job)
	if err != nil {
		t.Fatal(err)
	}
	inputTokens, outputTokens := int64(4096), int64(512)
	if ok, err := store.SettleGeneration(ctx, attempt, app.GenerationOutcome{Result: output, InputTokens: &inputTokens, OutputTokens: &outputTokens}, "usage.1"); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	usage, err = store.DayGenerationUsage(ctx, attempt.BudgetDay, "")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Day != attempt.BudgetDay || usage.Attempts != 1 || usage.KnownTokens != inputTokens+outputTokens || usage.ChargedTokens != inputTokens+outputTokens {
		t.Fatalf("known usage: %+v", usage)
	}
	if len(usage.Agents) != 1 || usage.Agents[0].AgentID != job.AgentID {
		t.Fatalf("usage rows: %+v", usage.Agents)
	}
	if _, err := store.DayGenerationUsage(ctx, "not-a-day", ""); err == nil {
		t.Fatal("invalid day accepted")
	}
}

func TestAdminDayUsageFleetOverflow(t *testing.T) {
	store, persona, job1 := generationSetup(t)
	ctx := context.Background()
	agent1 := persona.AgentID
	// Create a second agent and job directly so the fleet total spans two rows.
	agent2 := app.NewID()
	now := time.Now().UTC()
	day := now.Format(time.DateOnly)
	if err := store.CreateAccount(ctx, app.Account{ID: agent2, Type: app.AccountAgent, Handle: "overflow_agent", DisplayName: "Overflow", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePersona(ctx, app.Persona{AgentID: agent2, Version: 1, Instructions: "Overflow.", TopicTags: []string{"go"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job2ID := app.NewID()
	generationSQL(t, store, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,output_kind,root_job_id,chain_depth,max_chain_depth,max_chain_jobs,status,available_at,expires_at,lease_version,created_at)
		VALUES($1,$2,1,'scheduled',$3,'post',$1,0,2,5,'pending',$4,$5,0,$4)`,
		job2ID, agent2, string(app.NewID()), now, now.Add(time.Hour))
	half := int64(math.MaxInt64 / 2)
	attempt1ID := app.NewID()
	generationSQL(t, store, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
		context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,finished_at,error_code)
		VALUES($1,$2,1,1,'together','test-model',$3,'v1',$4,$5,'unknown',$6,$6,'provider_timeout')`,
		attempt1ID, job1.ID, strings.Repeat("a", 64), day, half+1000, now)
	attempt2ID := app.NewID()
	generationSQL(t, store, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
		context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,finished_at,error_code)
		VALUES($1,$2,1,1,'together','test-model',$3,'v1',$4,$5,'unknown',$6,$6,'provider_timeout')`,
		attempt2ID, job2ID, strings.Repeat("a", 64), day, half+1000, now)
	if _, err := store.DayGenerationUsage(ctx, day, ""); err == nil {
		t.Fatal("overflowing fleet totals accepted")
	}
	// Per-agent filter should still work because it does not aggregate across agents.
	usage, err := store.DayGenerationUsage(ctx, day, agent1)
	if err != nil {
		t.Fatal(err)
	}
	if usage.ChargedTokens != half+1000 {
		t.Fatalf("agent filter total: %d", usage.ChargedTokens)
	}
}

// TestAdminStatusQueueAndFleetBounds reads one consistent snapshot through the
// whole fleet and the bounded queue/failure counts.
func TestAdminStatusQueueAndFleetBounds(t *testing.T) {
	store, job, input := spendFixture(t, 500000)
	ctx := context.Background()
	_ = admitSpend(t, store, job, input)
	generationSQL(t, store, `UPDATE generation_jobs SET status='failed',reason_code='provider_credentials',
		finished_at=available_at,lease_version=1,lease_expires_at=NULL WHERE id=$1`, job.ID)
	status, err := store.GenerationStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Agent.Configured < 1 || status.Queue.Failed != 1 {
		t.Fatalf("status: %+v", status)
	}
	if status.Queue.OldestFailed == nil {
		t.Fatal("failed queue age missing")
	}
	if status.TodayUsage.Day != time.Now().UTC().Format(time.DateOnly) {
		t.Fatalf("today usage day: %+v", status.TodayUsage)
	}
}

func TestAdminStatusOldestDueIgnoresFutureWork(t *testing.T) {
	store, persona, _ := generationSetup(t)
	ctx := context.Background()
	agentID := persona.AgentID
	now := time.Now()
	futureID := app.NewID()
	generationSQL(t, store, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,output_kind,root_job_id,chain_depth,max_chain_depth,max_chain_jobs,status,available_at,expires_at,lease_version,created_at)
		VALUES($1,$2,1,'scheduled',$3,'post',$1,0,2,5,'pending',$4,$5,0,$6)`,
		futureID, agentID, string(app.NewID()), now.Add(time.Hour), now.Add(2*time.Hour), now)
	dueID := app.NewID()
	dueAt := now.Add(-time.Minute)
	generationSQL(t, store, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,output_kind,root_job_id,chain_depth,max_chain_depth,max_chain_jobs,status,available_at,expires_at,lease_version,created_at)
		VALUES($1,$2,1,'scheduled',$3,'post',$1,0,2,5,'pending',$4,$5,0,$6)`,
		dueID, agentID, string(app.NewID()), dueAt, now.Add(time.Hour), dueAt)
	status, err := store.GenerationStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Queue.OldestDue == nil {
		t.Fatal("oldest due missing")
	}
	if !status.Queue.OldestDue.Equal(dueAt.Truncate(time.Microsecond)) && !status.Queue.OldestDue.Before(now) {
		t.Fatalf("oldest due should be the already-due job, got %v", status.Queue.OldestDue)
	}
	// Cancel the due job; only the future job remains and must not be reported as due.
	generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',finished_at=created_at,reason_code='removed' WHERE id=$1`, dueID)
	status, err = store.GenerationStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Queue.OldestDue != nil {
		t.Fatalf("future work reported as due: %v", status.Queue.OldestDue)
	}
}
