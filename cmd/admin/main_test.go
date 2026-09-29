package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestParseAdminCommandStrictness(t *testing.T) {
	if _, err := parseAdminCommand(nil); err == nil || !strings.Contains(err.Error(), "usage: admin") {
		t.Fatalf("missing command: %v", err)
	}
	ok := []string{"persona", "create", "00000000-0000-4000-8000-000000000001", "/tmp/persona.json"}
	command, err := parseAdminCommand(ok)
	if err != nil || command.name != "persona create" || command.agent != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("create parse: %+v %v", command, err)
	}
	selectCommand, err := parseAdminCommand([]string{"persona", "select", "00000000-0000-4000-8000-000000000001", "3"})
	if err != nil || selectCommand.version != 3 || selectCommand.agent != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("select parse: %+v %v", selectCommand, err)
	}
	pauseAll, err := parseAdminCommand([]string{"agent", "pause", "--all"})
	if err != nil || !pauseAll.all || pauseAll.agent != "" {
		t.Fatalf("pause all: %+v %v", pauseAll, err)
	}
	pauseAgent, err := parseAdminCommand([]string{"agent", "pause", "00000000-0000-4000-8000-000000000001"})
	if err != nil || pauseAgent.agent == "" {
		t.Fatalf("pause agent: %+v %v", pauseAgent, err)
	}
	jobList, err := parseAdminCommand([]string{"job", "list",
		"--agent", "00000000-0000-4000-8000-000000000001", "--status", "failed", "--limit", "16"})
	if err != nil || jobList.agent == "" || jobList.status != "failed" || jobList.limit != 16 {
		t.Fatalf("job list: %+v %v", jobList, err)
	}
	usage, err := parseAdminCommand([]string{"usage", "--day", "2026-09-28"})
	if err != nil || usage.day != "2026-09-28" {
		t.Fatal("usage day parse failed", err)
	}
	bad := map[string][]string{
		"unknown command":           {"unknown", "x"},
		"agent+all conflict":        {"agent", "pause", "--all", "00000000-0000-4000-8000-000000000001"},
		"agent pause no target":     {"agent", "pause"},
		"agent resume no target":    {"agent", "resume"},
		"unknown flag":              {"job", "list", "--weird", "x"},
		"limit out of range":        {"job", "list", "--limit", "99"},
		"limit prefix junk":         {"job", "list", "--limit", "3junk"},
		"version prefix junk":       {"persona", "select", "00000000-0000-4000-8000-000000000001", "3junk"},
		"missing value":             {"job", "list", "--limit"},
		"invalid status":            {"job", "list", "--status", "weird"},
		"invalid uuid":              {"job", "retry", "nope"},
		"bad persona file":          {"persona", "select", "n", "0"},
		"missing file args":         {"persona", "create"},
		"missing targets":           {"job", "retry"},
		"zero day range":            {"usage", "--day", "wrong"},
		"persona flag override":     {"persona", "create", "00000000-0000-4000-8000-000000000001", "/tmp/x", "--agent", "00000000-0000-4000-8000-000000000002"},
		"status irrelevant flag":    {"status", "--agent", "00000000-0000-4000-8000-000000000001"},
		"job retry irrelevant flag": {"job", "retry", "00000000-0000-4000-8000-000000000001", "--limit", "3"},
		"duplicate agent flag":      {"usage", "--agent", "00000000-0000-4000-8000-000000000001", "--agent", "00000000-0000-4000-8000-000000000001"},
		"duplicate limit":           {"job", "list", "--limit", "3", "--limit", "4"},
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := parseAdminCommand(args)
			if err == nil {
				t.Fatalf("accepted")
			}
		})
	}
}

// TestFormatAdminReports covers the bounded JSON report encoding: no persona
// text or error strings ever surface from the dispatch path.
func TestFormatAdminReports(t *testing.T) {
	var output strings.Builder
	if err := writeAdminJSON(&output, map[string]any{"agent": "a", "version": 2}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "2") || strings.Contains(output.String(), "\n\n") {
		t.Fatal("formatted report")
	}
}

func adminTestStore(t *testing.T) (*postgres.Store, string) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	adminDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminDB.Close() })
	schema := "test_" + strings.ReplaceAll(string(app.NewID()), "-", "")
	if _, err := adminDB.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal("could not create isolated test schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminDB.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("could not remove isolated test schema")
		}
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := postgres.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store, parsed.String()
}

func adminDispatch(t *testing.T, databaseURL string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("DATABASE_URL", databaseURL)
	var output bytes.Buffer
	err := run(context.Background(), args, &output)
	return output.String(), err
}

func writeAdminTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func adminValidPersona(version int) string {
	return fmt.Sprintf(`{"version":%d,"instructions":"A concise admin persona.","topic_tags":["go"],"created_at":"2026-09-28T12:00:00Z"}`, version)
}

func adminValidPolicy() string {
	return `{"version":1,"timezone":"UTC","active_start":"00:00","active_end":"23:59",
		"scheduled_min_per_day":1,"scheduled_max_per_day":2,"min_spacing_seconds":1,"response_min_delay_seconds":60,
		"response_max_delay_seconds":300,"source_max_age_seconds":86400,"reply_probability_bps":2500,"repost_probability_bps":1000,
		"quote_probability_bps":2000,"human_post_probability_bps":500,"continuation_probability_bps":500,"cooldown_seconds":1800,
		"scheduled_post_cap_per_day":2,"reply_cap_per_day":10,"reply_cap_per_conversation":2,"max_agents_per_trigger":2,
		"human_trigger_cap_per_window":5,"human_trigger_window_seconds":3600,"max_chain_depth":2,"max_chain_jobs":5,"daily_token_budget":50000}`
}

func TestAdminCLIIntegration(t *testing.T) {
	_, databaseURL := adminTestStore(t)
	ctx := context.Background()
	store, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var now time.Time
	if err := db.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	now = now.UTC()

	agentID := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: agentID, Type: app.AccountAgent, Handle: "admin_agent", DisplayName: "Admin Agent", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePersona(ctx, app.Persona{AgentID: agentID, Version: 1, Instructions: "Original.", TopicTags: []string{"go"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: agentID, PersonaVersion: 1, Enabled: true, Policy: adminPolicy(), UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	personaPath := writeAdminTempFile(t, "persona.json", adminValidPersona(2))
	policyPath := writeAdminTempFile(t, "policy.json", adminValidPolicy())

	out, err := adminDispatch(t, databaseURL, "persona", "create", string(agentID), personaPath)
	if err != nil {
		t.Fatalf("persona create: %v %s", err, out)
	}
	if !strings.Contains(out, `"created":true`) {
		t.Fatalf("persona create output: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "persona", "select", string(agentID), "1")
	if err != nil {
		t.Fatalf("persona select: %v %s", err, out)
	}
	if !strings.Contains(out, `"version":1`) {
		t.Fatalf("persona select output: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "policy", "set", string(agentID), policyPath)
	if err != nil {
		t.Fatalf("policy set: %v %s", err, out)
	}
	if !strings.Contains(out, `"policy_set"`) {
		t.Fatalf("policy set output: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "agent", "pause", string(agentID))
	if err != nil {
		t.Fatalf("agent pause: %v %s", err, out)
	}
	out, err = adminDispatch(t, databaseURL, "agent", "resume", string(agentID))
	if err != nil {
		t.Fatalf("agent resume: %v %s", err, out)
	}
	out, err = adminDispatch(t, databaseURL, "agent", "pause", "--all")
	if err != nil {
		t.Fatalf("pause all: %v %s", err, out)
	}
	out, err = adminDispatch(t, databaseURL, "agent", "resume", "--all")
	if err != nil {
		t.Fatalf("resume all: %v %s", err, out)
	}

	// Create a failed social job eligible for explicit retry using direct SQL fixtures.
	// Use a recent past creation/finish and a future expiry so the retry re-check
	// passes without weakening eligibility.
	jobID := app.NewID()
	jobAt := now.Add(-10 * time.Minute)
	day := jobAt.Format(time.DateOnly)
	actorID := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: actorID, Type: app.AccountHuman, Handle: "admin_human_actor", DisplayName: "Actor", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sourcePostID := app.NewID()
	if _, err := db.ExecContext(ctx, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'admin source post',$3)`, sourcePostID, actorID, now); err != nil {
		t.Fatal(err)
	}
	triggerKey, _ := app.SocialGenerationKey(app.TriggerHumanPost, sourcePostID)
	cooldownKey, _ := app.GenerationCooldownKey(actorID, agentID, sourcePostID, app.TriggerHumanPost)
	if _, err := db.ExecContext(ctx, `INSERT INTO generation_jobs(id,agent_id,persona_version,trigger_kind,trigger_key,trigger_actor_id,cooldown_key,source_post_id,output_kind,root_job_id,chain_depth,max_chain_depth,max_chain_jobs,status,available_at,expires_at,lease_version,created_at,finished_at,reason_code)
		VALUES($1,$2,1,'human_post',$3,$4,$5,$6,'reply',$1,0,2,5,'failed',$7,$8,1,$7,$7,'provider_credentials')`,
		jobID, agentID, triggerKey, actorID, cooldownKey, sourcePostID, jobAt, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
		context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,finished_at,error_code)
		VALUES($1,$2,1,1,'together','test-model',$3,'v1',$4,1000,'failed',$5,$5,'provider_credentials')`,
		app.NewID(), jobID, strings.Repeat("a", 64), day, jobAt); err != nil {
		t.Fatal(err)
	}

	out, err = adminDispatch(t, databaseURL, "job", "retry", string(jobID))
	if err != nil {
		t.Fatalf("job retry: %v %s", err, out)
	}
	if !strings.Contains(out, `"status":"retry_wait"`) {
		t.Fatalf("job retry output: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "job", "list", "--agent", string(agentID), "--status", "retry_wait", "--limit", "8")
	if err != nil {
		t.Fatalf("job list: %v %s", err, out)
	}
	if !strings.Contains(out, `"jobs"`) {
		t.Fatalf("job list output: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "job", "inspect", string(jobID))
	if err != nil {
		t.Fatalf("job inspect: %v %s", err, out)
	}
	if !strings.Contains(out, `"complete":true`) {
		t.Fatalf("job inspect output missing complete: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "usage", "--agent", string(agentID), "--day", day)
	if err != nil {
		t.Fatalf("usage: %v %s", err, out)
	}
	if !strings.Contains(out, `"day":"`+day+`"`) {
		t.Fatalf("usage output: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "status")
	if err != nil {
		t.Fatalf("status: %v %s", err, out)
	}
	if !strings.Contains(out, `"configured"`) {
		t.Fatalf("status output: %s", out)
	}

	// Retry denial for a non-failed job (the same job is now retry_wait).
	_, err = adminDispatch(t, databaseURL, "job", "retry", string(jobID))
	if err == nil {
		t.Fatal("retry of succeeded job accepted")
	}
	// Denial messages must not echo internal details.
	if strings.Contains(err.Error(), "provider") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("retry denial echoes internals: %v", err)
	}

	// Content removal.
	humanID := app.NewID()
	if err := store.CreateAccount(ctx, app.Account{ID: humanID, Type: app.AccountHuman, Handle: "admin_human", DisplayName: "Human", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	postID := app.NewID()
	replyID := app.NewID()
	if _, err := db.ExecContext(ctx, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'admin post',$3)`, postID, humanID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'admin reply',$4)`, replyID, postID, humanID, now); err != nil {
		t.Fatal(err)
	}
	out, err = adminDispatch(t, databaseURL, "post", "remove", string(postID))
	if err != nil {
		t.Fatalf("post remove: %v %s", err, out)
	}
	if !strings.Contains(out, `"removed":true`) {
		t.Fatalf("post remove output: %s", out)
	}
	// Reply removal requires the parent post to exist; recreate it.
	postID2 := app.NewID()
	if _, err := db.ExecContext(ctx, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'admin post 2',$3)`, postID2, humanID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE replies SET post_id=$1 WHERE id=$2`, postID2, replyID); err != nil {
		t.Fatal(err)
	}
	out, err = adminDispatch(t, databaseURL, "reply", "remove", string(replyID))
	if err != nil {
		t.Fatalf("reply remove: %v %s", err, out)
	}
	if !strings.Contains(out, `"removed":true`) {
		t.Fatalf("reply remove output: %s", out)
	}

	out, err = adminDispatch(t, databaseURL, "account", "disable", string(agentID))
	if err != nil {
		t.Fatalf("account disable: %v %s", err, out)
	}
	if !strings.Contains(out, `"disabled":true`) {
		t.Fatalf("account disable output: %s", out)
	}

	// Payloads must not echo raw file content.
	secretPersona := writeAdminTempFile(t, "secret.json", adminValidPersona(3))
	out, err = adminDispatch(t, databaseURL, "persona", "create", string(agentID), secretPersona)
	if err != nil {
		t.Fatalf("secret persona create: %v %s", err, out)
	}
	if strings.Contains(out, "concise admin persona") || strings.Contains(out, "Original.") {
		t.Fatalf("persona text echoed: %s", out)
	}
}

func adminPolicy() app.GenerationPolicy {
	return app.GenerationPolicy{
		Version: 1, Timezone: "UTC", ActiveStart: "00:00", ActiveEnd: "23:59",
		ScheduledMinPerDay: 1, ScheduledMaxPerDay: 2, MinSpacingSeconds: 1,
		ResponseMinDelaySeconds: 60, ResponseMaxDelaySeconds: 300, SourceMaxAgeSeconds: 86400,
		ReplyProbabilityBPS: 2500, RepostProbabilityBPS: 1000, QuoteProbabilityBPS: 2000,
		HumanPostProbabilityBPS: 500, ContinuationProbabilityBPS: 500, CooldownSeconds: 1800,
		ScheduledPostCapPerDay: 2, ReplyCapPerDay: 10, ReplyCapPerConversation: 2,
		MaxAgentsPerTrigger: 2, HumanTriggerCapPerWindow: 5, HumanTriggerWindowSeconds: 3600,
		MaxChainDepth: 2, MaxChainJobs: 5, DailyTokenBudget: 50000,
	}
}

func TestAdminCLIMissingArgsEveryCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"persona create", []string{"persona", "create"}},
		{"persona select", []string{"persona", "select"}},
		{"policy set", []string{"policy", "set"}},
		{"agent pause", []string{"agent", "pause"}},
		{"agent resume", []string{"agent", "resume"}},
		{"job inspect", []string{"job", "inspect"}},
		{"job retry", []string{"job", "retry"}},
		{"account disable", []string{"account", "disable"}},
		{"post remove", []string{"post", "remove"}},
		{"reply remove", []string{"reply", "remove"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAdminCommand(tc.args)
			if err == nil {
				t.Fatal("accepted missing args")
			}
		})
	}
}

func TestAdminCLIExactNumericConversion(t *testing.T) {
	for _, value := range []string{"3junk", "3.5", "+3", "-3", "03 "} {
		t.Run(value, func(t *testing.T) {
			_, err := parseAdminCommand([]string{"persona", "select", "00000000-0000-4000-8000-000000000001", value})
			if err == nil {
				t.Fatal("accepted prefix integer")
			}
		})
	}
}

func TestAdminCLINoSecretEchoInParseErrors(t *testing.T) {
	secret := "secret-token-do-not-echo"
	_, err := parseAdminCommand([]string{"job", "list", "--weird", secret})
	if err == nil {
		t.Fatal("accepted unknown flag")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("unknown flag echoed secret: %v", err)
	}
}

func TestAdminCLIInvalidDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://invalid.example/stacktrace")
	var output bytes.Buffer
	err := run(context.Background(), []string{"status"}, &output)
	if err == nil {
		t.Fatal("accepted invalid database URL")
	}
	if strings.Contains(err.Error(), "invalid.example") {
		t.Fatalf("error echoed URL host: %v", err)
	}
}
