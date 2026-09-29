package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
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
	adminJobCursorMaxLen = 256
)

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
	if len(token) > adminJobCursorMaxLen {
		return cursor, invalidGeneration("cursor")
	}
	parts := strings.SplitN(token, "|", 2)
	if len(parts) != 2 {
		return cursor, invalidGeneration("cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return cursor, invalidGeneration("cursor")
	}
	if at.IsZero() {
		return cursor, invalidGeneration("cursor")
	}
	if _, err := app.ParseID(parts[1]); err != nil {
		return cursor, invalidGeneration("cursor")
	}
	return AdminJobCursor{CreatedAt: at.UTC(), JobID: app.ID(parts[1])}, nil
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
		if filter.Cursor != nil {
			if filter.Cursor.CreatedAt.IsZero() {
				return invalidGeneration("cursor")
			}
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
		limit := validAdminLimit(filter.Limit)
		rows, err := q.queryer.QueryContext(ctx, `SELECT j.id,j.created_at FROM generation_jobs j`+predicate,
			agent, status, at, cursorID, limit+1)
		if err != nil {
			return databaseError(ctx, err)
		}
		defer rows.Close()
		type jobKey struct {
			id        app.ID
			createdAt time.Time
		}
		var keys []jobKey
		for rows.Next() {
			var key jobKey
			if err := rows.Scan(&key.id, &key.createdAt); err != nil {
				return databaseError(ctx, err)
			}
			keys = append(keys, key)
		}
		if err := rows.Err(); err != nil {
			return databaseError(ctx, err)
		}
		if len(keys) == 0 {
			return nil
		}
		var more bool
		if len(keys) > limit {
			keys, more = keys[:limit], true
		}
		jobs = make([]app.GenerationJob, 0, len(keys))
		for _, key := range keys {
			job, err := q.GenerationJobByID(ctx, key.id)
			if err != nil {
				return err
			}
			jobs = append(jobs, job)
		}
		if more {
			last := keys[len(keys)-1]
			token := AdminJobCursor{CreatedAt: last.createdAt.UTC(), JobID: last.id}.token()
			next = &token
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return jobs, next, nil
}

// AdminJobInspection bounds one job with its full attempt page; the attempted
// history, digest decision markers and pagination identity are operator-only.
type AdminJobInspection struct {
	Job      app.GenerationJob       `json:"job"`
	Attempts []app.GenerationAttempt `json:"attempts"`
	Complete bool                    `json:"complete"`
}

// InspectGenerationJob returns one bounded job with at most one page of
// attempts, ordered by attempt number. Reads grant no authority. Complete is
// true only when every attempt fits in the page; otherwise the result is
// truncated to MaxGenerationAttempts and Complete is false.
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
			inspection.Attempts = attempts[:app.MaxGenerationAttempts]
			inspection.Complete = false
		} else {
			inspection.Attempts = attempts
			inspection.Complete = true
		}
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

// DayGenerationUsage reports one UTC budget day through a consistent snapshot.
func (s *Store) DayGenerationUsage(ctx context.Context, day string, agentID app.ID) (AdminDayUsage, error) {
	if day != "" {
		if _, err := time.Parse(time.DateOnly, day); err != nil {
			return AdminDayUsage{}, invalidGeneration("day")
		}
	}
	if agentID != "" {
		if _, err := app.ParseID(string(agentID)); err != nil {
			return AdminDayUsage{}, invalidGeneration("agent_id")
		}
	}
	var usage AdminDayUsage
	err := s.readSnapshot(ctx, func(q *Queries) error {
		var err error
		usage, err = dayGenerationUsage(ctx, q, day, agentID)
		return err
	})
	if err != nil {
		return AdminDayUsage{}, err
	}
	return usage, nil
}

// countGenerationConfiguration is the shared validation/counting logic used by
// CheckGenerationConfiguration and GenerationStatus. It runs inside an existing
// Queries transaction and never starts its own.
func countGenerationConfiguration(ctx context.Context, q *Queries) (configured, enabled int, err error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.queryer.QueryContext(ctx, `SELECT s.agent_id,s.persona_version,s.enabled,
		CASE WHEN octet_length(s.policy::text)<=8192 THEN s.policy END,s.next_post_at,
		COALESCE(to_char(s.schedule_date,'YYYY-MM-DD'),''),s.remaining_slots,s.last_published_at,s.updated_at,
		p.instructions,CASE WHEN octet_length(to_json(p.topic_tags)::text)<=4096 THEN to_json(p.topic_tags) END,p.created_at,a.type
		FROM agent_settings s LEFT JOIN agent_personas p ON p.agent_id=s.agent_id AND p.version=s.persona_version
		LEFT JOIN accounts a ON a.id=s.agent_id ORDER BY s.agent_id LIMIT $1`, maxConfiguredAgents+1)
	if err != nil {
		return 0, 0, databaseError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		if configured == maxConfiguredAgents {
			return 0, 0, errors.New("generation configuration exceeds 1000 agents")
		}
		var settings app.AgentSettings
		var persona app.Persona
		var policy, tags []byte
		var accountType app.AccountType
		if err := rows.Scan(&settings.AgentID, &settings.PersonaVersion, &settings.Enabled, &policy,
			&settings.NextPostAt, &settings.ScheduleDate, &settings.RemainingSlots, &settings.LastPublishedAt, &settings.UpdatedAt,
			&persona.Instructions, &tags, &persona.CreatedAt, &accountType); err != nil {
			return 0, 0, databaseError(ctx, err)
		}
		persona.AgentID, persona.Version = settings.AgentID, settings.PersonaVersion
		settings.Policy, err = app.DecodeGenerationPolicy(policy)
		if err != nil || json.Unmarshal(tags, &persona.TopicTags) != nil || persona.Validate() != nil || settings.Validate() != nil || accountType != app.AccountAgent {
			return 0, 0, app.ErrUnavailable
		}
		configured++
		if settings.Enabled {
			enabled++
		}
	}
	return configured, enabled, databaseError(ctx, rows.Err())
}

func dayGenerationUsage(ctx context.Context, q *Queries, day string, agentID app.ID) (AdminDayUsage, error) {
	ctx, cancel := q.queryContext(ctx)
	defer cancel()
	if day == "" {
		if err := q.queryer.QueryRowContext(ctx, `SELECT to_char((now() AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`).Scan(&day); err != nil {
			return AdminDayUsage{}, databaseError(ctx, err)
		}
	}
	usage := AdminDayUsage{Day: day}
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
		return AdminDayUsage{}, databaseError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row AdminAgentUsage
		if err := rows.Scan(&row.AgentID, &row.Attempts, &row.KnownTokens, &row.ChargedTokens); err != nil {
			return AdminDayUsage{}, databaseError(ctx, err)
		}
		if len(usage.Agents) == adminUsageAgentLimit {
			return AdminDayUsage{}, invalidGeneration("agents")
		}
		usage.Agents = append(usage.Agents, row)
		if err := addInt(&usage.Attempts, row.Attempts); err != nil {
			return AdminDayUsage{}, err
		}
		if err := addInt64(&usage.KnownTokens, row.KnownTokens); err != nil {
			return AdminDayUsage{}, err
		}
		if err := addInt64(&usage.ChargedTokens, row.ChargedTokens); err != nil {
			return AdminDayUsage{}, err
		}
	}
	return usage, databaseError(ctx, rows.Err())
}

func addInt64(target *int64, value int64) error {
	if value > 0 && *target > math.MaxInt64-value {
		return invalidGeneration("usage")
	}
	if value < 0 && *target < math.MinInt64-value {
		return invalidGeneration("usage")
	}
	*target += value
	return nil
}

func addInt(target *int, value int) error {
	if value > 0 && *target > math.MaxInt-value {
		return invalidGeneration("usage")
	}
	if value < 0 && *target < math.MinInt-value {
		return invalidGeneration("usage")
	}
	*target += value
	return nil
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

// GenerationStatus aggregates the bounded report in a single read snapshot.
// Configured/enabled counts reuse the same validation rules as
// CheckGenerationConfiguration. The UTC day is taken from the database so the
// usage report describes the same moment as the queue counts. Failures and
// retries simply count, never re-open work or claim provider billing.
func (s *Store) GenerationStatus(ctx context.Context) (AdminStatus, error) {
	var status AdminStatus
	err := s.readSnapshot(ctx, func(q *Queries) error {
		qctx, cancel := q.queryContext(ctx)
		defer cancel()
		configured, enabled, err := countGenerationConfiguration(ctx, q)
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
		var oldestDue, oldestFailed sql.NullTime
		if err := q.queryer.QueryRowContext(qctx, `SELECT
			(SELECT min(available_at) FROM generation_jobs
			 WHERE status IN ('pending','retry_wait') AND available_at <= now() AND expires_at > now()),
			(SELECT min(created_at) FROM generation_jobs WHERE status='failed')`).Scan(&oldestDue, &oldestFailed); err != nil {
			return databaseError(ctx, err)
		}
		if oldestDue.Valid {
			stamp := oldestDue.Time
			status.Queue.OldestDue = &stamp
		}
		if oldestFailed.Valid {
			stamp := oldestFailed.Time
			status.Queue.OldestFailed = &stamp
		}
		var today string
		if err := q.queryer.QueryRowContext(qctx, `SELECT to_char((now() AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`).Scan(&today); err != nil {
			return databaseError(ctx, err)
		}
		usage, err := dayGenerationUsage(ctx, q, today, "")
		if err != nil {
			return err
		}
		status.TodayUsage = AdminDayUsage{Day: usage.Day, Attempts: usage.Attempts,
			KnownTokens: usage.KnownTokens, ChargedTokens: usage.ChargedTokens}
		return nil
	})
	if err != nil {
		return AdminStatus{}, err
	}
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
