package postgres

import (
	"context"
	"errors"
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

func TestAdminListGenerationJobsExactFilters(t *testing.T) {
	store, _, root := generationSetup(t)
	ctx := context.Background()
	generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='source_finished',
		finished_at=available_at,lease_version=1,lease_expires_at=NULL WHERE id=$1`, root.ID)
	page, _, err := store.ListGenerationJobs(ctx, AdminJobFilter{Status: "cancelled"})
	if err != nil || len(page) != 1 || page[0].ID != root.ID {
		t.Fatalf("status filter: %d %v", len(page), err)
	}
	if missing, _, err := store.ListGenerationJobs(ctx, AdminJobFilter{Status: "pending", AgentID: root.AgentID}); err != nil || len(missing) != 0 {
		t.Fatalf("mismatched filter: %d %v", len(missing), err)
	}
	if _, _, err := store.ListGenerationJobs(ctx, AdminJobFilter{AgentID: "not-uuid"}); err == nil {
		t.Fatal("invalid agent accepted")
	}
}

func TestAdminCursorRoundTrip(t *testing.T) {
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
	if inspection.Job.ID != job.ID || len(inspection.Attempts) != 2 {
		t.Fatalf("inspect: %+v", inspection)
	}
	if _, err := store.InspectGeneration(ctx, app.NewID()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing inspection: %v", err)
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
