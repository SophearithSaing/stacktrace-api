package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/llm"
)

// Admin read reports stay bounded and identity/count based: no persona text,
// prompts, provider payloads or billing claims. Every multi-statement report
// runs through one consistent read snapshot; every list uses capped keyset
// pages with stable (created_at,id) ties and optional exact filters.

const (
	adminJobPageSize     = 64
	adminAttemptPageSize = app.MaxGenerationAttempts + 1 // One page detects overflow.
	adminUsageAgentLimit = maxConfiguredAgents
)

var errAdminStatusSet = invalidGeneration("status")

// AdminJobCursor is an opaque keyset cursor over (created_at,id): operator-only
// cursors stay bounded, tie-safe and do not need browser HMAC signing.
type AdminJobCursor struct {
	CreatedAt time.Time
	JobID     app.ID
}

// DecodeAdminCursor parses an operator cursor token; invalid tokens fail
// closed. Reports list newer work first, and each token re-encodes the whole
// key.
func DecodeAdminCursor(token string) (AdminJobCursor, error) {
	var cursor AdminJobCursor
	parts := strings.SplitN(token, "|", 2)
	if len(parts) != 2 {
		return cursor, invalidGeneration("cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return cursor, invalidGeneration("cursor")
	}
	if _, err := app.ParseID(parts[1]); err != nil {
		return cursor, invalidGeneration("cursor")
	}
	return AdminJobCursor{CreatedAt: at, JobID: app.ID(parts[1])}, nil
}

func (c AdminJobCursor) token() string {
	return c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + string(c.JobID)
}

func validAdminLimit(limit int) int {
	if limit < 1 {
		return adminJobPageSize
	}
	return min(limit, adminJobPageSize)
}

// AdminJobFilter bounds one list page. Agent and status are exact values;
// the cursor continues the page; limits are capped.
type AdminJobFilter struct {
	AgentID app.ID
	Status  string
	Cursor  *AdminJobCursor
	Limit   int
}

// ListGenerationJobs returns one bounded, stable page plus the next cursor
// token when a later page may exist. Snapshot reads grant no authority.
func (s *Store) ListGenerationJobs(ctx context.Context, filter AdminJobFilter) ([]app.GenerationJob, *string, error) {
	var (
		jobs []app.GenerationJob
		next *string
	)
	err := s.readSnapshot(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		if filter.AgentID != "" {
			if _, err := app.ParseID(string(filter.AgentID)); err != nil {
				return invalidGeneration("agent_id")
			}
		}
		// Status filters are exact, operator-provided set members.
		switch filter.Status {
		case "", "pending", "running", "retry_wait", "succeeded", "skipped", "cancelled", "failed":
		default:
			return invalidGeneration("status")
		}
		if filter.Cursor != nil && filter.Cursor.JobID != "" {
			if _, err := app.ParseID(string(filter.Cursor.JobID)); err != nil {
				return invalidGeneration("cursor")
			}
		}
		predicate := `
			WHERE ($1::uuid IS NULL OR j.agent_id=$1)
			AND ($2::text IS NULL OR j.status=$2)
			AND ($3::timestamptz IS NULL OR (j.created_at,j.id) < ($3,$4))
			ORDER BY j.created_at DESC,j.id DESC LIMIT $5`
		var at, cursorID any
		if filter.Cursor != nil {
			at = filter.Cursor.CreatedAt
			cursorID = filter.Cursor.JobID
		}
		var agent any
		if filter.AgentID != "" {
			agent = filter.AgentID
		}
		var status any
		if filter.Status != "" {
			status = filter.Status
		}
		rows, err := q.queryer.QueryContext(ctx, `SELECT j.id FROM generation_jobs j`+predicate,
			agent, status, at, cursorID, validAdminLimit(filter.Limit)+1)
		if err != nil {
			return databaseError(ctx, err)
		}
		ids, err := contextIDs(ctx, rows)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		var more bool
		if len(ids) > validAdminLimit(filter.Limit) {
			ids, more = ids[:validAdminLimit(filter.Limit)], true
		}
		for _, id := range ids {
			job, err := q.GenerationJobByID(ctx, id)
			if err != nil {
				return err
			}
			jobs = append(jobs, job)
		}
		if more {
			token, err := q.adminCursorToken(ctx, ids[len(ids)-1])
			if err != nil {
				return err
			}
			next = &token
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return jobs, next, nil
}

// adminCursorToken re-reads the persisted key of the page's last row so the
// cursor stays tie-safe against UUID ordering and encoded timestamps.
func (q *Queries) adminCursorToken(ctx context.Context, id app.ID) (string, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	job, err := q.GenerationJobByID(ctx, id)
	if err != nil {
		return "", err
	}
	return AdminJobCursor{CreatedAt: job.CreatedAt.UTC(), JobID: job.ID}.token(), nil
}

// AdminJobInspection bounds one job with its full attempt page; the attempted
// history, digest decision markers and pagination identity are operator-only.
type AdminJobInspection struct {
	Job      app.GenerationJob
	Attempts []app.GenerationAttempt
	Complete bool // Attempt pages could exceed MaxGenerationAttempts.
}

// InspectGenerationJob returns one bounded job with at most one page of
// attempts, ordered by attempt number. Reads grant no authority.
func (s *Store) InspectGeneration(ctx context.Context, id app.ID) (AdminJobInspection, error) {
	var inspection AdminJobInspection
	err := s.readSnapshot(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		job, err := q.GenerationJobByID(ctx, id)
		if err != nil {
			return err
		}
		inspection.Job = job
		attempts, err := q.executionAttemptsBounded(ctx, job.ID, adminAttemptPageSize)
		if err != nil {
			return err
		}
		if len(attempts) > app.MaxGenerationAttempts {
			inspection.Attempts = attempts
			return nil // Overflow stays readable history, bounded to the page.
		}
		inspection.Attempts = attempts
		return nil
	})
	if err != nil {
		return AdminJobInspection{}, err
	}
	return inspection, nil
}

// AdminDayUsage distinguishes known token usage from conservative charged
// reservations. The cost rules are admission's, reused verbatim: resolved
// attempts charge actual tokens, while reserved/unknown calls, absent or
// out-of-bound counts and unsupported accounting retain the whole reservation.
// SUM(bigint) is numeric, so overflow on scan fails closed unchanged.
type AdminDayUsage struct {
	Day           string            `json:"day"`
	Attempts      int               `json:"attempts"`
	KnownTokens   int64             `json:"known_tokens"`
	ChargedTokens int64             `json:"charged_tokens"`
	Agents        []AdminAgentUsage `json:"agents,omitempty"`
}

type AdminAgentUsage struct {
	AgentID       app.ID `json:"agent_id"`
	Attempts      int    `json:"attempts"`
	KnownTokens   int64  `json:"known_tokens"`
	ChargedTokens int64  `json:"charged_tokens"`
}

// DayGenerationUsage reports one UTC budget day. Builds only aggregate rows
// into the bounded fleet limit; called from one consistent snapshot.
func (s *Store) DayGenerationUsage(ctx context.Context, day string, agentID app.ID) (AdminDayUsage, error) {
	if day == "" {
		day = time.Now().UTC().Format(time.DateOnly)
	}
	if _, err := time.Parse(time.DateOnly, day); err != nil {
		return AdminDayUsage{}, invalidGeneration("day")
	}
	if agentID != "" {
		if _, err := app.ParseID(string(agentID)); err != nil {
			return AdminDayUsage{}, invalidGeneration("agent_id")
		}
	}
	var usage AdminDayUsage
	err := s.readSnapshot(ctx, func(q *Queries) error {
		ctx, cancel := q.queryContext(ctx)
		defer cancel()
		usage.Day = day
		var agentFilter any
		if agentID != "" {
			agentFilter = agentID
		}
		rows, err := q.queryer.QueryContext(ctx, `WITH spent AS (
			SELECT j.agent_id,
			CASE WHEN t.status IN ('reserved','unknown') OR t.input_tokens IS NULL OR t.output_tokens IS NULL
			OR t.input_tokens<1 OR t.input_tokens>$3 OR t.output_tokens<0 OR t.output_tokens>$4 OR t.error_code='unsupported_accounting'
			THEN NULL ELSE t.input_tokens+t.output_tokens END AS known,
			CASE WHEN t.status IN ('reserved','unknown') OR t.input_tokens IS NULL OR t.output_tokens IS NULL
			OR t.input_tokens<1 OR t.input_tokens>$3 OR t.output_tokens<0 OR t.output_tokens>$4 OR t.error_code='unsupported_accounting'
			THEN t.reserved_tokens ELSE t.input_tokens+t.output_tokens END AS charged
			FROM generation_attempts t JOIN generation_jobs j ON j.id=t.job_id
			WHERE t.budget_day=$2::date)
			SELECT agent_id,count(*)::integer,COALESCE(sum(known),0),COALESCE(sum(charged),0)
			FROM spent WHERE ($1::uuid IS NULL OR agent_id=$1)
			GROUP BY agent_id ORDER BY agent_id LIMIT $5`,
			agentFilter, day, llm.MaxInputTokens, llm.MaxOutputTokens, adminUsageAgentLimit+1)
		if err != nil {
			return databaseError(ctx, err)
		}
		defer rows.Close()
		for rows.Next() {
			var row AdminAgentUsage
			if err := rows.Scan(&row.AgentID, &row.Attempts, &row.KnownTokens, &row.ChargedTokens); err != nil {
				return databaseError(ctx, err)
			}
			if len(usage.Agents) == adminUsageAgentLimit {
				return fmt.Errorf("generation usage exceeds %d agents", adminUsageAgentLimit)
			}
			usage.Agents = append(usage.Agents, row)
			usage.Attempts += row.Attempts
			usage.KnownTokens += row.KnownTokens
			usage.ChargedTokens += row.ChargedTokens
		}
		return databaseError(ctx, rows.Err())
	})
	if err != nil {
		return AdminDayUsage{}, err
	}
	return usage, nil
}

// AdminStatus reports bounded queue, failure and usage signals through one
// consistent snapshot. No provider billing guarantee or configuration leak.
type AdminStatus struct {
	Agent      AdminAgentStatus `json:"agents"`
	Queue      AdminQueueStatus `json:"queue"`
	TodayUsage AdminDayUsage    `json:"today_usage"`
}

type AdminAgentStatus struct {
	Configured int `json:"configured"`
	Enabled    int `json:"enabled"`
}

type AdminQueueStatus struct {
	Pending      int        `json:"pending"`
	RetryWait    int        `json:"retry_wait"`
	Running      int        `json:"running"`
	Succeeded    int        `json:"succeeded"`
	Skipped      int        `json:"skipped"`
	Cancelled    int        `json:"cancelled"`
	Failed       int        `json:"failed"`
	OldestDue    *time.Time `json:"oldest_due,omitempty"`
	OldestFailed *time.Time `json:"oldest_failed,omitempty"`
}

// GenerateStatus aggregates the bounded report; failures and retries simply
// count, never re-open work or claim provider billing.
func (s *Store) GenerationStatus(ctx context.Context) (AdminStatus, error) {
	var status AdminStatus
	err := s.readSnapshot(ctx, func(q *Queries) error {
		qctx, cancel := q.queryContext(ctx)
		defer cancel()
		configured, enabled, err := s.CheckGenerationConfiguration(ctx)
		if err != nil {
			return err
		}
		status.Agent = AdminAgentStatus{Configured: configured, Enabled: enabled}
		var counts struct{ pending, retry, running, succeeded, skipped, cancelled, failed int }
		err = q.queryer.QueryRowContext(qctx, `SELECT count(*) FILTER (WHERE status='pending'),
			count(*) FILTER (WHERE status='retry_wait'),count(*) FILTER (WHERE status='running'),
			count(*) FILTER (WHERE status='succeeded'),count(*) FILTER (WHERE status='skipped'),
			count(*) FILTER (WHERE status='cancelled'),count(*) FILTER (WHERE status='failed')
			FROM generation_jobs`).Scan(&counts.pending, &counts.retry, &counts.running,
			&counts.succeeded, &counts.skipped, &counts.cancelled, &counts.failed)
		if err != nil {
			return databaseError(ctx, err)
		}
		status.Queue = AdminQueueStatus{Pending: counts.pending, RetryWait: counts.retry,
			Running: counts.running, Succeeded: counts.succeeded, Skipped: counts.skipped,
			Cancelled: counts.cancelled, Failed: counts.failed}
		var oldestDue, oldestFailed any
		if err := q.queryer.QueryRowContext(qctx, `SELECT (SELECT min(created_at) FROM generation_jobs
				WHERE status IN ('pending','retry_wait')),
				(SELECT min(created_at) FROM generation_jobs WHERE status='failed')`).Scan(&oldestDue, &oldestFailed); err != nil {
			return databaseError(ctx, err)
		}
		for value, target := range map[any]**time.Time{oldestDue: &status.Queue.OldestDue, oldestFailed: &status.Queue.OldestFailed} {
			if stamp, ok := value.(time.Time); ok {
				*target = &stamp
			}
		}
		return nil
	})
	if err != nil {
		return AdminStatus{}, err
	}
	// The whole fleet reads today's usage in one bounded, id-free row.
	today, err := s.DayGenerationUsage(ctx, "", "")
	if err != nil {
		return AdminStatus{}, err
	}
	status.TodayUsage = AdminDayUsage{Day: today.Day, Attempts: today.Attempts,
		KnownTokens: today.KnownTokens, ChargedTokens: today.ChargedTokens}
	return status, nil
}

// executionAttemptsBounded mirrors the shape of the attempt launch query and
// bounds the listing; the attempts read no lease or user represented identity.
func (q *Queries) executionAttemptsBounded(ctx context.Context, id app.ID, limit int) ([]app.GenerationAttempt, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(ctx, `SELECT id FROM generation_attempts WHERE job_id=$1 ORDER BY attempt_number LIMIT $2`, id, limit)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	ids, err := contextIDs(ctx, rows)
	if err != nil {
		return nil, err
	}
	attempts := make([]app.GenerationAttempt, 0, len(ids))
	for _, attemptID := range ids {
		attempt, err := q.GenerationAttemptByID(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}
