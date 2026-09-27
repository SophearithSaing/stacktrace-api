"""Disposable built-worker/HTTP integration fixtures, never development seeds."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

from smoke import client


state = Path(sys.argv[1])
phase = sys.argv[2]
worker = state / "worker"
psql = sys.argv[3:]


def sql(statement):
    result = subprocess.run(
        psql + ["-X", "-qAt", "-v", "ON_ERROR_STOP=1"],
        input=statement, text=True, capture_output=True, timeout=15,
    )
    assert result.returncode == 0, "Disposable generation fixture SQL failed"
    return result.stdout.strip()


def command(name="schedule", success=True, key=""):
    env = dict(os.environ, TOGETHER_API_KEY=key)
    env.pop("STACKTRACE_GENERATION_SMOKE", None)
    result = subprocess.run([str(worker), name], env=env, text=True, capture_output=True, timeout=45)
    assert (result.returncode == 0) == success, f"worker {name}: unexpected exit"
    assert os.environ["DATABASE_URL"] not in result.stdout + result.stderr
    if success:
        print(result.stdout.strip())
    return result


def snapshot():
    return sql("""SELECT json_build_object(
        'jobs',(SELECT json_agg(j ORDER BY id) FROM generation_jobs j),
        'settings',(SELECT json_agg(s ORDER BY agent_id) FROM
            (SELECT agent_id,enabled,policy,next_post_at,schedule_date,remaining_slots
             FROM agent_settings) s))""")


def write_fixture(path, fixture):
    with open(path, "w", opener=lambda name, flags: os.open(name, flags, 0o600)) as file:
        json.dump(fixture, file)
    assert path.stat().st_mode & 0o777 == 0o600


def execute(mode="publish", calls=0, published=0, retried=0):
    path = state / "execution-command.json"
    write_fixture(path, dict(mode=mode, calls=calls))
    env = dict(os.environ, STACKTRACE_GENERATION_SMOKE=str(path), TOGETHER_API_KEY="")
    result = subprocess.run(
        [str(state / "worker.test"), "-test.run=^TestGenerationSmokeHarness$", "-test.count=1", "-test.timeout=40s"],
        env=env, text=True, capture_output=True, timeout=45,
    )
    # Never print test failure diagnostics or arbitrary provider/SQL text.
    assert result.returncode == 0, "Fake-provider command harness failed"
    expected = (f"Execution pass complete: probes=8 claimed={calls} calls={calls} recovered=0 expired=0 "
                f"published={published} skipped=0 cancelled=0 failed=0 retried={retried} denied=0")
    assert result.stdout == expected + "\nPASS\n" and result.stderr == "", "Unsafe or unexpected execution report"
    print(expected)


def execution_snapshot():
    return sql("""SELECT json_build_object(
        'jobs',(SELECT json_agg(j ORDER BY id) FROM generation_jobs j),
        'attempts',(SELECT json_agg(a ORDER BY id) FROM generation_attempts a),
        'settings',(SELECT json_agg(s ORDER BY agent_id) FROM agent_settings s),
        'posts',(SELECT json_agg(p ORDER BY id) FROM posts p),
        'replies',(SELECT json_agg(r ORDER BY id) FROM replies r))""")


def publications():
    rows = json.loads(sql("""SELECT json_agg(row_to_json(p) ORDER BY trigger_kind) FROM (
        SELECT j.id AS job_id,j.trigger_kind,j.result_post_id,j.result_reply_id,j.source_post_id,
            j.published_attempt_id,a.output_digest,
            COALESCE(p.body,r.body) AS body,
            (a.job_id=j.id AND a.lease_version=j.lease_version AND a.status='succeeded'
             AND a.decision='publish' AND a.input_tokens=100 AND a.output_tokens=20
             AND a.reserved_tokens=132096 AND a.provider_request_id='smoke-request'
             AND COALESCE(p.author_id,r.author_id)=j.agent_id
             AND (r.id IS NULL OR r.post_id=j.source_post_id)) AS exact_provenance
        FROM generation_jobs j JOIN generation_attempts a ON a.id=j.published_attempt_id
        LEFT JOIN posts p ON p.id=j.result_post_id LEFT JOIN replies r ON r.id=j.result_reply_id
        WHERE j.status='succeeded') p"""))
    assert len(rows) == 4 and {row["trigger_kind"] for row in rows} == {"scheduled", "reply", "repost", "quote"}
    assert all(row["exact_provenance"] and re.fullmatch(r"generation_output_v1:[0-9a-f]{64}", row["output_digest"]) for row in rows)
    return rows


def verify_http(request, rows):
    forbidden = ["generation_output_v1", "context_hash", "persona_version", "trigger_key", "lease_version",
                 "published_attempt_id", "provider_request_id", "provider_timeout", "smoke-request"]
    for row in rows:
        forbidden.extend([row["job_id"], row["published_attempt_id"]])

    def safe(response):
        encoded = json.dumps(response)
        assert not any(value in encoded for value in forbidden), "HTTP exposed generation diagnostics"
        return response

    for row in rows:
        if row["result_post_id"]:
            content = safe(request(f"/api/v1/posts/{row['result_post_id']}"))
            assert content["id"] == row["result_post_id"]
        else:
            source = safe(request(f"/api/v1/posts/{row['source_post_id']}"))
            page = safe(request(f"/api/v1/posts/{row['source_post_id']}/replies"))
            content = next(item for item in page["items"] if item["id"] == row["result_reply_id"])
            # Detail preview shares provenance with the full reply page.
            for preview in source["reply_preview"]["items"]:
                full = next(item for item in page["items"] if item["id"] == preview["id"])
                assert preview["is_generated"] == full["is_generated"]
        assert content["is_generated"] is True and content["body"] == row["body"]
    safe(request("/api/v1/feed"))


fixture_path = state / "generation-fixture.json"
execution_path = state / "execution-fixture.json"
if phase == "unmigrated":
    before = sql("SELECT count(*) FROM pg_tables WHERE schemaname='public'")
    result = command(success=False)
    assert "Schedule pass complete" not in result.stdout
    assert sql("SELECT count(*) FROM pg_tables WHERE schemaname='public'") == before == "0"
    assert command("execute", success=False).stderr.strip() == "provider_configuration"
    assert command("execute", success=False, key="fake-smoke-key").stderr.strip() == "execution_storage"
    assert sql("SELECT count(*) FROM pg_tables WHERE schemaname='public'") == "0"
    print("Schedule refused unmigrated database without DDL.")
elif phase == "empty":
    assert "visited=0 invalid=0 enqueued=0 denied=0 expired=0" in command().stdout
    assert sql("SELECT count(*) FROM generation_jobs") == "0"
    # No jobs exist, so this production command cannot reach the provider.
    assert "claimed=0 calls=0" in command("execute", key="fake-smoke-key").stdout
elif phase == "seeded":
    assert sql("SELECT count(*) FROM agent_settings WHERE enabled") == "0"
    before = snapshot()
    assert "enqueued=0" in command().stdout
    assert snapshot() == before, "Disabled seeds changed"
    agents = json.loads(sql("SELECT json_agg(agent_id ORDER BY agent_id) FROM agent_settings"))
    assert len(agents) == 2
    scheduled, social = agents
    # Put the current local clock around noon, away from midnight/window edges.
    # Persist two slots: exactly one is due, the next must survive a new process.
    sql(f"""UPDATE agent_settings SET enabled=true,
        policy=policy || jsonb_build_object(
            'timezone','Etc/GMT' || CASE WHEN extract(hour from clock_timestamp() AT TIME ZONE 'UTC')-12>=0 THEN '+' ELSE '' END ||
                (extract(hour from clock_timestamp() AT TIME ZONE 'UTC')-12)::int::text,
            'active_start','00:00','active_end','23:59',
            'scheduled_min_per_day',2,'scheduled_max_per_day',2,
            'scheduled_post_cap_per_day',2,'min_spacing_seconds',3600,
            'source_max_age_seconds',7200)
        WHERE agent_id='{scheduled}';
        UPDATE agent_settings SET schedule_date=(clock_timestamp() AT TIME ZONE (policy->>'timezone'))::date,
            remaining_slots=2,next_post_at=clock_timestamp()-interval '1 second'
        WHERE agent_id='{scheduled}';""")
    # Invalid optional settings must not block healthy agents (unlike check).
    sql(f"UPDATE agent_settings SET enabled=true,policy=jsonb_set(policy,'{{timezone}}','\"invalid-zone\"') WHERE agent_id='{social}'")
    original_social = json.loads(before)["settings"][1]["policy"]
    assert "visited=2 invalid=1 enqueued=1" in command().stdout
    assert sql("SELECT count(*) FROM generation_jobs WHERE trigger_kind='scheduled' AND status='pending'") == "1"
    assert sql(f"SELECT remaining_slots=1 AND next_post_at>clock_timestamp() FROM agent_settings WHERE agent_id='{scheduled}'") == "t"
    after = snapshot()
    assert "enqueued=0" in command().stdout
    assert snapshot() == after, "Replay moved slot or duplicated job"
    # Preserve the fixture for verification after API restart too.
    write_fixture(fixture_path, dict(scheduled=scheduled, social=social, snapshot=after, policy=original_social))
    print("Schedule persisted exactly one job and a future slot across worker processes.")
elif phase == "http":
    fixture = json.loads(fixture_path.read_text())
    social = fixture["social"]
    policy = json.dumps(fixture["policy"]).replace("'", "''")
    sql(f"""UPDATE agent_settings SET policy='{policy}'::jsonb ||
        '{{"scheduled_min_per_day":0,"scheduled_max_per_day":0,"min_spacing_seconds":1,
        "human_post_probability_bps":0,"repost_probability_bps":10000,"reply_cap_per_day":10,"reply_cap_per_conversation":10,
        "max_agents_per_trigger":1,"human_trigger_cap_per_window":10,"cooldown_seconds":3600,
        "response_min_delay_seconds":0,"response_max_delay_seconds":0}}'::jsonb
        WHERE agent_id='{social}';""")
    handle = sql(f"SELECT handle FROM accounts WHERE id='{social}'")
    request, _ = client(os.environ["API_PUBLIC_ORIGIN"])
    request("/api/v1/auth/register", "POST", dict(username="generation_smoke", display_name="Generation Smoke", password="smoke-password-123"), expected=201)
    csrf = request("/api/v1/me")["csrf_token"]
    # Original-post probability remains zero; repost body mentions select only this agent.
    body = dict(body=f"smoke @{handle}")
    post = request("/api/v1/posts", "POST", body, csrf, key="generation-smoke", expected=201)
    retry = request("/api/v1/posts", "POST", body, csrf, key="generation-smoke", expected=201)
    assert post["id"] == retry["id"]
    assert sql("SELECT count(*) FROM generation_jobs") == "1"
    path = f"/api/v1/posts/{post['id']}/repost"
    first = request(path, "PUT", csrf=csrf)
    again = request(path, "PUT", csrf=csrf)
    assert first["repost_entry_id"] == again["repost_entry_id"]
    assert sql("SELECT count(*) FROM generation_jobs") == "2"
    assert sql("SELECT count(*) FROM generation_jobs WHERE trigger_kind='repost' AND status='pending'") == "1"
    request(path, "DELETE", csrf=csrf)
    assert sql("SELECT count(*) FROM generation_jobs WHERE trigger_kind='repost' AND status='cancelled' AND reason_code='source_removed' AND source_repost_id IS NULL") == "1"
    # Restore invalid fixture for unchanged scheduler snapshot comparisons below.
    sql(f"UPDATE agent_settings SET policy=jsonb_set(policy,'{{timezone}}','\"invalid-zone\"'),enabled=false WHERE agent_id='{social}'")
    fixture["snapshot"] = snapshot()
    write_fixture(fixture_path, fixture)
    print("Live trigger smoke passed: authenticated retries, repost no-op and removal cancellation.")
elif phase == "verify":
    fixture = json.loads(fixture_path.read_text())
    assert "enqueued=0" in command().stdout
    assert snapshot() == fixture["snapshot"], "Restart lost schedule/job persistence"
    sql("UPDATE agent_settings SET enabled=false")
    before = snapshot()
    assert "enqueued=0" in command().stdout
    assert snapshot() == before
    print("Schedule restart verification passed; disposable agents disabled again.")
elif phase == "execute":
    fixture = json.loads(fixture_path.read_text())
    agent = fixture["scheduled"]
    # Reuse the actual scheduled job only after all old persistence assertions.
    # Other seeds stay disabled; no autonomous continuations or random selection.
    sql(f"""UPDATE agent_settings SET enabled=true,policy=policy ||
        '{{"min_spacing_seconds":1,"response_min_delay_seconds":0,"response_max_delay_seconds":0,
        "reply_probability_bps":10000,"repost_probability_bps":10000,"quote_probability_bps":10000,
        "human_post_probability_bps":0,"continuation_probability_bps":0,"cooldown_seconds":1,
        "reply_cap_per_day":20,"reply_cap_per_conversation":10,"max_agents_per_trigger":1,
        "human_trigger_cap_per_window":20,"daily_token_budget":500000}}'::jsonb WHERE agent_id='{agent}'""")
    execute(calls=1, published=1)
    post_id = sql("SELECT result_post_id FROM generation_jobs WHERE trigger_kind='scheduled'")
    request, _ = client(os.environ["API_PUBLIC_ORIGIN"])
    request("/api/v1/auth/login", "POST", dict(username="generation_smoke", password="smoke-password-123"))
    csrf = request("/api/v1/me")["csrf_token"]
    for kind in ("reply", "repost", "quote"):
        time.sleep(1.1)  # Real finite publication/admission spacing, not SQL clock rewrites.
        if kind == "reply":
            path, method, body = f"/api/v1/posts/{post_id}/replies", "POST", dict(body="How should this handler be tested?")
        elif kind == "repost":
            path, method, body = f"/api/v1/posts/{post_id}/repost", "PUT", None
        else:
            path, method, body = "/api/v1/posts", "POST", dict(body="A useful example for discussion.", quoted_post_id=post_id)
        kwargs = dict(csrf=csrf, key=f"execution-{kind}", expected=200 if kind == "repost" else 201)
        first = request(path, method, body, **kwargs)
        again = request(path, method, body, **kwargs)
        if kind == "reply":
            first, again = first["reply"], again["reply"]
        field = "repost_entry_id" if kind == "repost" else "id"
        assert first[field] == again[field]
        if kind != "repost":
            assert first["is_generated"] is False
        assert sql(f"SELECT count(*) FROM generation_jobs WHERE trigger_kind='{kind}' AND status='pending'") == "1"
        execute(calls=1, published=1)
    rows = publications()
    verify_http(request, rows)
    before = execution_snapshot()
    execute()
    assert execution_snapshot() == before, "Another worker changed published provenance"

    time.sleep(1.1)
    failure = request(f"/api/v1/posts/{post_id}/replies", "POST", dict(body="Can you explain the failure case?"), csrf, key="execution-failure", expected=201)
    assert failure["reply"]["is_generated"] is False
    execute(mode="timeout", calls=1, retried=1)
    assert sql("""SELECT count(*) FROM generation_jobs j JOIN generation_attempts a ON a.job_id=j.id
        WHERE j.status='retry_wait' AND j.reason_code='provider_timeout' AND a.status='unknown'
        AND a.error_code='provider_timeout' AND a.reserved_tokens=132096
        AND a.input_tokens IS NULL AND a.output_tokens IS NULL AND a.output_digest IS NULL
        AND j.available_at>=a.not_before AND j.available_at>clock_timestamp()+interval '20 minutes'
        AND j.result_reply_id IS NULL AND j.published_attempt_id IS NULL""") == "1"
    # Provider unavailability cannot take down unrelated authenticated HTTP writes.
    healthy = request("/api/v1/posts", "POST", dict(body="HTTP remains usable during provider failure."), csrf, key="execution-healthy", expected=201)
    assert healthy["is_generated"] is False
    assert request(f"/api/v1/posts/{healthy['id']}")["body"] == healthy["body"]
    verify_http(request, rows)
    assert sql("SELECT count(*) FROM generation_attempts") == "5"
    assert sql("SELECT count(*) FROM generation_jobs") == "6"
    write_fixture(execution_path, dict(rows=rows, snapshot=execution_snapshot(), healthy=healthy["id"]))
    print("Fake-provider execution passed: scheduled post, human comment/repost/quote replies, exact provenance, timeout isolation.")
elif phase == "execute-verify":
    fixture = json.loads(execution_path.read_text())
    assert execution_snapshot() == fixture["snapshot"], "API restart changed durable execution state"
    execute()
    assert execution_snapshot() == fixture["snapshot"], "Worker replay changed durable execution state"
    assert publications() == fixture["rows"]
    request, _ = client(os.environ["API_PUBLIC_ORIGIN"])
    verify_http(request, fixture["rows"])
    assert request(f"/api/v1/posts/{fixture['healthy']}")["is_generated"] is False
    sql("UPDATE agent_settings SET enabled=false")
    before = execution_snapshot()
    execute()
    assert execution_snapshot() == before
    assert sql("SELECT count(*) FROM agent_settings WHERE enabled") == "0"
    print("Execution verified after API restart: persisted provenance, no extra calls; disposable agents disabled.")
else:
    raise AssertionError("Unknown generation smoke phase")
