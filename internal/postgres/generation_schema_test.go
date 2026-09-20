package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/jackc/pgx/v5/pgconn"
)

func generationSQL(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func generationReject(t *testing.T, store *Store, code, query string, args ...any) {
	t.Helper()
	_, err := store.db.ExecContext(context.Background(), query, args...)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != code {
		t.Fatalf("SQL error=%v, want %s", err, code)
	}
}

// Clone an existing valid row with explicit overrides to exercise database
// constraints independently of the trusted store's input validation.
func generationCloneJob(store *Store, source app.ID, changes map[string]any) (app.ID, error) {
	id := app.NewID()
	values := map[string]any{"id": id, "root_job_id": id, "trigger_key": string(id)}
	for key, value := range changes {
		values[key] = value
	}
	data, err := json.Marshal(values)
	if err != nil {
		return id, err
	}
	_, err = store.db.ExecContext(context.Background(), `INSERT INTO generation_jobs
		SELECT (jsonb_populate_record(NULL::generation_jobs,to_jsonb(j)||$2::jsonb)).*
		FROM generation_jobs j WHERE id=$1`, source, data)
	return id, err
}

func generationSocial(t *testing.T, store *Store, job app.GenerationJob) (app.ID, app.ID, app.ID, app.ID) {
	t.Helper()
	actor, _ := contentTestActor(t, store, "generation_actor")
	post, reply, repost := app.NewID(), app.NewID(), app.NewID()
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'source post',$3)`, post, actor, job.CreatedAt)
	generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'source reply',$4)`, reply, post, actor, job.CreatedAt)
	generationSQL(t, store, `INSERT INTO reposts(id,post_id,account_id,created_at) VALUES($1,$2,$3,$4)`, repost, post, actor, job.CreatedAt)
	return actor, post, reply, repost
}

func generationAttempt(t *testing.T, store *Store, job app.GenerationJob, number int) app.ID {
	t.Helper()
	id := app.NewID()
	generationSQL(t, store, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,
		context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at)
		VALUES($1,$2,$3,1,'together','test-model',$4,'v1','2026-09-20',1000,'reserved',$5)`,
		id, job.ID, number, strings.Repeat("a", 64), job.AvailableAt)
	return id
}

func TestGenerationSchemaJobChecks(t *testing.T) {
	store, _, job := generationSetup(t)
	actor, post, reply, repost := generationSocial(t, store, job)
	for name, changes := range map[string]map[string]any{
		"status":                  {"status": "invented"},
		"output":                  {"output_kind": "invented"},
		"trigger":                 {"trigger_kind": "invented"},
		"empty trigger":           {"trigger_key": " "},
		"pending leased":          {"lease_version": 1},
		"negative lease":          {"lease_version": -1},
		"pending expiry":          {"lease_expires_at": job.ExpiresAt},
		"pending finish":          {"finished_at": job.CreatedAt},
		"pending reason":          {"reason_code": "bad"},
		"pending post result":     {"result_post_id": post},
		"pending reply result":    {"result_reply_id": reply},
		"pending attempt":         {"published_attempt_id": app.NewID()},
		"availability":            {"available_at": job.CreatedAt.Add(-time.Second)},
		"expiry equality":         {"expires_at": job.AvailableAt},
		"infinite expiry":         {"expires_at": "infinity"},
		"running version":         {"status": "running", "lease_expires_at": job.ExpiresAt},
		"running expiry":          {"status": "running", "lease_version": 1},
		"running expiry equality": {"status": "running", "lease_version": 1, "lease_expires_at": job.AvailableAt},
		"retry version":           {"status": "retry_wait", "reason_code": "retry"},
		"retry reason":            {"status": "retry_wait", "lease_version": 1},
		"cancelled finish":        {"status": "cancelled", "reason_code": "removed"},
		"skipped reason":          {"status": "skipped", "finished_at": job.CreatedAt},
		"failed early finish":     {"status": "failed", "reason_code": "error", "finished_at": job.CreatedAt.Add(-time.Second)},
		"failed unsafe reason":    {"status": "failed", "reason_code": "secret error body!", "finished_at": job.CreatedAt},
		"success missing result":  {"status": "succeeded", "lease_version": 1, "finished_at": job.CreatedAt},
		"scheduled actor":         {"trigger_actor_id": actor},
		"scheduled cooldown":      {"cooldown_key": "cooldown"},
		"scheduled source":        {"source_post_id": post},
		"scheduled reply":         {"source_reply_id": reply},
		"scheduled repost":        {"source_repost_id": repost},
		"scheduled quote":         {"output_kind": "quote"},
		"self child":              {"chain_depth": 1},
		"other root at zero":      {"root_job_id": job.ID},
		"depth overflow":          {"chain_depth": 11},
		"social missing actor":    {"trigger_kind": "human_post", "source_post_id": post, "cooldown_key": "cooldown", "output_kind": "reply"},
		"social missing cooldown": {"trigger_kind": "human_post", "source_post_id": post, "trigger_actor_id": actor, "output_kind": "reply"},
		"social self actor":       {"trigger_kind": "human_post", "source_post_id": post, "trigger_actor_id": job.AgentID, "cooldown_key": "cooldown", "output_kind": "reply"},
		"social missing source":   {"trigger_kind": "human_post", "trigger_actor_id": actor, "cooldown_key": "cooldown", "output_kind": "reply"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := generationCloneJob(store, job.ID, changes)
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "23514" {
				t.Fatalf("constraint error = %v", err)
			}
		})
	}
	for _, trigger := range []string{"reply", "repost", "quote", "human_post", "continuation"} {
		t.Run("valid "+trigger, func(t *testing.T) {
			changes := map[string]any{"trigger_kind": trigger, "trigger_actor_id": actor, "source_post_id": post, "cooldown_key": "durable", "output_kind": "reply"}
			if trigger == "reply" {
				changes["source_reply_id"] = reply
			}
			if trigger == "repost" {
				changes["source_repost_id"] = repost
			}
			if trigger == "continuation" {
				changes["root_job_id"], changes["chain_depth"] = job.ID, 1
			}
			id, err := generationCloneJob(store, job.ID, changes)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.GenerationJobByID(context.Background(), id); err != nil {
				t.Fatalf("valid stored job: %v", err)
			}
			if trigger == "continuation" {
				changes["root_job_id"], changes["chain_depth"] = id, 2
				if _, err := generationCloneJob(store, job.ID, changes); err == nil {
					t.Fatal("child referenced nonroot job")
				}
			}
		})
	}
}

func TestGenerationSchemaForeignKeysAndSettingsShape(t *testing.T) {
	store, persona, job := generationSetup(t)
	ctx := context.Background()
	actor, post, _, _ := generationSocial(t, store, job)
	other := app.NewID()
	generationSQL(t, store, `INSERT INTO accounts(id,type,handle,display_name,created_at,updated_at) VALUES($1,'agent','other_agent','Other',now(),now())`, other)
	for name, changes := range map[string]map[string]any{
		"cross agent persona":   {"agent_id": other},
		"missing persona":       {"persona_version": 2},
		"missing actor":         {"trigger_kind": "human_post", "trigger_actor_id": app.NewID(), "source_post_id": app.NewID(), "cooldown_key": "key", "output_kind": "reply"},
		"missing source post":   {"trigger_kind": "human_post", "trigger_actor_id": actor, "source_post_id": app.NewID(), "cooldown_key": "key", "output_kind": "reply"},
		"missing source reply":  {"trigger_kind": "reply", "trigger_actor_id": actor, "source_post_id": post, "source_reply_id": app.NewID(), "cooldown_key": "key", "output_kind": "reply"},
		"missing source repost": {"trigger_kind": "repost", "trigger_actor_id": actor, "source_post_id": post, "source_repost_id": app.NewID(), "cooldown_key": "key", "output_kind": "reply"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := generationCloneJob(store, job.ID, changes)
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "23503" {
				t.Fatalf("FK error = %v", err)
			}
		})
	}
	settingsSQL := `INSERT INTO agent_settings(agent_id,persona_version,enabled,policy,remaining_slots,updated_at) VALUES($1,1,false,$2,0,now())`
	generationReject(t, store, "23503", settingsSQL, other, `{"version":1}`)
	for _, policy := range []string{`{}`, `[]`, `null`, `{"version":null}`, `{"version":2}`, `{"version":"1"}`} {
		generationReject(t, store, "23514", settingsSQL, persona.AgentID, policy)
	}
	settings := app.AgentSettings{AgentID: persona.AgentID, PersonaVersion: 1, Policy: generationPolicy(), UpdatedAt: job.CreatedAt}
	if _, err := store.InitializeAgentSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	for _, update := range []string{"remaining_slots=-1", "remaining_slots=101", "remaining_slots=1", "next_post_at=now()", "schedule_date='infinity'", "policy='{}'"} {
		generationReject(t, store, "23514", `UPDATE agent_settings SET `+update+` WHERE agent_id=$1`, persona.AgentID)
	}
	for _, update := range []string{"version=2", "instructions='changed'", "topic_tags=ARRAY['other']", "created_at=created_at+interval '1 second'"} {
		generationReject(t, store, "23514", `UPDATE agent_personas SET `+update+` WHERE agent_id=$1`, persona.AgentID)
	}
	generationReject(t, store, "23514", `DELETE FROM agent_personas WHERE agent_id=$1`, persona.AgentID)
	generationReject(t, store, "23503", `DELETE FROM accounts WHERE id=$1`, persona.AgentID)
}

func TestGenerationSchemaPublicationProvenance(t *testing.T) {
	store, _, job := generationSetup(t)
	ctx := context.Background()
	actor, post, reply, _ := generationSocial(t, store, job)
	attempt := generationAttempt(t, store, job, 1)
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at+interval '1 second' WHERE id=$1`, attempt)
	otherJob := job
	otherJob.ID = app.NewID()
	otherJob.RootJobID, otherJob.TriggerKey = otherJob.ID, "other"
	if err := store.CreateGenerationJob(ctx, otherJob); err != nil {
		t.Fatal(err)
	}
	otherAttempt := generationAttempt(t, store, otherJob, 1)
	publication := `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at+interval '2 seconds',result_post_id=$2,published_attempt_id=$3 WHERE id=$1`
	generationReject(t, store, "23503", publication, job.ID, post, otherAttempt)
	generationReject(t, store, "23503", publication, job.ID, app.NewID(), attempt)
	generationReject(t, store, "23514", publication, job.ID, nil, attempt)
	generationReject(t, store, "23514", publication, job.ID, post, nil)
	generationReject(t, store, "23514", `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=expires_at,result_post_id=$2,published_attempt_id=$3 WHERE id=$1`, job.ID, post, attempt)
	generationReject(t, store, "23514", `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at,result_reply_id=$2,published_attempt_id=$3 WHERE id=$1`, job.ID, reply, attempt)
	generationReject(t, store, "23514", `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at,result_post_id=$2,result_reply_id=$3,published_attempt_id=$4 WHERE id=$1`, job.ID, post, reply, attempt)
	generationSQL(t, store, publication, job.ID, post, attempt)
	generationReject(t, store, "23505", publication, otherJob.ID, post, otherAttempt)
	for _, update := range []string{"result_post_id=NULL", "result_post_id='00000000-0000-0000-0000-000000000099'", "published_attempt_id=NULL", "finished_at=finished_at+interval '1 second'", "status='pending',result_post_id=NULL,published_attempt_id=NULL,finished_at=NULL,lease_version=0"} {
		generationReject(t, store, "23514", `UPDATE generation_jobs SET `+update+` WHERE id=$1`, job.ID)
	}
	generationSQL(t, store, `UPDATE posts SET deleted_at=now() WHERE id=$1`, post)
	saved, err := store.GenerationJobByID(ctx, job.ID)
	if err != nil || saved.ResultPostID == nil || *saved.ResultPostID != post || saved.PublishedAttemptID == nil || *saved.PublishedAttemptID != attempt {
		t.Fatalf("lost deleted result provenance: %+v %v", saved, err)
	}
	generationReject(t, store, "23503", `DELETE FROM posts WHERE id=$1`, post)
	generationReject(t, store, "23514", `DELETE FROM generation_jobs WHERE id=$1`, job.ID)

	// Matching reply results are independently unique and a quote is a post
	// result, never a reply result. Content/author suitability is runtime policy.
	for index, output := range []string{"reply", "reply", "quote"} {
		id, err := generationCloneJob(store, otherJob.ID, map[string]any{"trigger_kind": "human_post", "trigger_actor_id": actor, "source_post_id": post, "cooldown_key": "key", "output_kind": output})
		if err != nil {
			t.Fatal(err)
		}
		child := otherJob
		child.ID = id
		attemptID := generationAttempt(t, store, child, 1)
		if output == "quote" {
			generationReject(t, store, "23514", `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at,result_reply_id=$2,published_attempt_id=$3 WHERE id=$1`, id, reply, attemptID)
			continue
		}
		_, err = store.db.ExecContext(ctx, `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at,result_reply_id=$2,published_attempt_id=$3 WHERE id=$1`, id, reply, attemptID)
		if index == 1 {
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) || pgError.Code != "23505" {
				t.Fatal(err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			generationSQL(t, store, `UPDATE replies SET deleted_at=now() WHERE id=$1`, reply)
		}
	}
}

func TestGenerationSchemaRepostDeletionAndIdentity(t *testing.T) {
	store, _, job := generationSetup(t)
	ctx := context.Background()
	actor, post, _, repost := generationSocial(t, store, job)
	var jobs []app.ID
	for _, status := range []string{"pending", "succeeded", "skipped", "cancelled", "failed"} {
		id, err := generationCloneJob(store, job.ID, map[string]any{"trigger_kind": "repost", "trigger_actor_id": actor, "source_post_id": post, "source_repost_id": repost, "cooldown_key": "durable-cooldown", "output_kind": "quote"})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, id)
		if status == "succeeded" {
			result := app.NewID()
			generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at,quoted_post_id) VALUES($1,$2,'result',$3,$4)`, result, job.AgentID, job.CreatedAt, post)
			copyJob := job
			copyJob.ID = id
			attempt := generationAttempt(t, store, copyJob, 1)
			generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at WHERE id=$1`, attempt)
			generationSQL(t, store, `UPDATE generation_jobs SET status='succeeded',lease_version=1,finished_at=available_at,result_post_id=$2,published_attempt_id=$3 WHERE id=$1`, id, result, attempt)
		} else if status != "pending" {
			generationSQL(t, store, `UPDATE generation_jobs SET status=$2,reason_code='test_outcome',finished_at=available_at WHERE id=$1`, id, status)
		}
		generationReject(t, store, "23514", `UPDATE generation_jobs SET source_repost_id=NULL WHERE id=$1`, id)
		generationReject(t, store, "23514", `UPDATE generation_jobs SET source_repost_id=$2 WHERE id=$1`, id, app.NewID())
	}
	for _, update := range []string{"expires_at=expires_at+interval '1 hour'", "persona_version=2", "trigger_key='replacement'", "created_at=created_at-interval '1 second'", "trigger_actor_id=NULL", "cooldown_key='replacement'", "source_post_id=NULL", "root_job_id=id,chain_depth=1", "output_kind='reply'"} {
		generationReject(t, store, "23514", `UPDATE generation_jobs SET `+update+` WHERE id=$1`, jobs[0])
	}
	generationSQL(t, store, `DELETE FROM reposts WHERE id=$1`, repost)
	for _, id := range jobs {
		saved, err := store.GenerationJobByID(ctx, id)
		if err != nil || saved.SourceRepostID != nil || saved.TriggerActorID == nil || *saved.TriggerActorID != actor || saved.CooldownKey != "durable-cooldown" || !saved.ExpiresAt.Equal(job.ExpiresAt) {
			t.Fatalf("repost deletion lost provenance: %+v %v", saved, err)
		}
		if saved.Status == app.JobSucceeded && (saved.ResultPostID == nil || saved.PublishedAttemptID == nil) {
			t.Fatal("terminal result lost")
		}
		if saved.Status == app.JobSkipped || saved.Status == app.JobCancelled {
			generationReject(t, store, "23514", `UPDATE generation_jobs SET reason_code='replacement' WHERE id=$1`, id)
			generationReject(t, store, "23514", `UPDATE generation_jobs SET status='pending',finished_at=NULL,reason_code=NULL WHERE id=$1`, id)
		}
		generationReject(t, store, "23514", `UPDATE generation_jobs SET source_repost_id=$2 WHERE id=$1`, id, repost)
	}
}

func TestGenerationSchemaAttempts(t *testing.T) {
	store, _, job := generationSetup(t)
	ctx := context.Background()
	attempt := generationAttempt(t, store, job, 1)
	saved, err := store.GenerationAttemptByID(ctx, attempt)
	if err != nil || saved.ReservedTokens != 1000 || saved.Status != app.AttemptReserved || saved.BudgetDay != "2026-09-20" {
		t.Fatalf("attempt read=%+v %v", saved, err)
	}
	for name, changes := range map[string]map[string]any{
		"missing job":              {"job_id": app.NewID()},
		"zero lease":               {"lease_version": 0},
		"negative reservation":     {"reserved_tokens": -1},
		"zero reservation":         {"reserved_tokens": 0},
		"wrong UTC day":            {"budget_day": "2026-09-19"},
		"wrong hash":               {"context_hash": "ABC"},
		"unsafe provider":          {"provider": "secret provider error!"},
		"reserved usage":           {"input_tokens": 1, "output_tokens": 1},
		"partial usage":            {"input_tokens": 1},
		"reserved request":         {"provider_request_id": "id"},
		"reserved finish":          {"finished_at": job.CreatedAt},
		"reserved error":           {"error_code": "error"},
		"succeeded missing finish": {"status": "succeeded"},
		"succeeded error":          {"status": "succeeded", "finished_at": job.CreatedAt, "error_code": "error"},
		"failed missing error":     {"status": "failed", "finished_at": job.CreatedAt},
		"unknown settled usage":    {"status": "unknown", "finished_at": job.CreatedAt, "error_code": "timeout", "input_tokens": 1, "output_tokens": 1},
		"excess usage":             {"status": "succeeded", "finished_at": job.CreatedAt, "input_tokens": 999, "output_tokens": 2},
		"negative usage":           {"status": "succeeded", "finished_at": job.CreatedAt, "input_tokens": -1, "output_tokens": 2},
		"early finish":             {"status": "succeeded", "finished_at": job.CreatedAt.Add(-time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			changes["id"], changes["attempt_number"] = app.NewID(), 2
			data, err := json.Marshal(changes)
			if err != nil {
				t.Fatal(err)
			}
			code := "23514"
			if name == "missing job" {
				code = "23503"
			}
			generationReject(t, store, code, `INSERT INTO generation_attempts SELECT (jsonb_populate_record(NULL::generation_attempts,to_jsonb(a)||$2::jsonb)).* FROM generation_attempts a WHERE id=$1`, attempt, data)
		})
	}
	for _, update := range []string{"reserved_tokens=2000", "lease_version=2", "attempt_number=2", "provider='other'", "model='other'", "context_hash=repeat('b',64)", "context_builder_version='v2'", "budget_day=budget_day+1", "started_at=started_at+interval '1 second'"} {
		generationReject(t, store, "23514", `UPDATE generation_attempts SET `+update+` WHERE id=$1`, attempt)
	}
	generationReject(t, store, "23505", `INSERT INTO generation_attempts SELECT (jsonb_populate_record(NULL::generation_attempts,to_jsonb(a)||jsonb_build_object('id',$2::text))).* FROM generation_attempts a WHERE id=$1`, attempt, app.NewID())
	generationSQL(t, store, `UPDATE generation_attempts SET status='unknown',error_code='timeout',finished_at=started_at+interval '1 second' WHERE id=$1`, attempt)
	saved, err = store.GenerationAttemptByID(ctx, attempt)
	if err != nil || saved.AccountedTokens() != 1000 {
		t.Fatalf("unknown reservation=%+v %v", saved, err)
	}
	for _, update := range []string{"status='succeeded',error_code=NULL", "input_tokens=1,output_tokens=1", "reserved_tokens=999", "finished_at=finished_at+interval '1 second'", "provider_request_id='later'"} {
		generationReject(t, store, "23514", `UPDATE generation_attempts SET `+update+` WHERE id=$1`, attempt)
	}
	generationReject(t, store, "23514", `DELETE FROM generation_attempts WHERE id=$1`, attempt)
	second := generationAttempt(t, store, job, 2)
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',input_tokens=600,output_tokens=100,provider_request_id='request-2',finished_at=started_at WHERE id=$1`, second)
	saved, err = store.GenerationAttemptByID(ctx, second)
	if err != nil || saved.AccountedTokens() != 700 || saved.ProviderRequestID != "request-2" {
		t.Fatalf("settled reservation=%+v %v", saved, err)
	}
	generationReject(t, store, "23514", `UPDATE generation_attempts SET input_tokens=500 WHERE id=$1`, second)
}
