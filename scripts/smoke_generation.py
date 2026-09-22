"""Disposable built-worker/HTTP integration fixtures, never development seeds."""

import json
import os
from pathlib import Path
import subprocess
import sys

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


def command(name="schedule", success=True):
    result = subprocess.run([str(worker), name], text=True, capture_output=True, timeout=45)
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


fixture_path = state / "generation-fixture.json"
if phase == "unmigrated":
    before = sql("SELECT count(*) FROM pg_tables WHERE schemaname='public'")
    result = command(success=False)
    assert "Schedule pass complete" not in result.stdout
    assert sql("SELECT count(*) FROM pg_tables WHERE schemaname='public'") == before == "0"
    print("Schedule refused unmigrated database without DDL.")
elif phase == "empty":
    assert "visited=0 invalid=0 enqueued=0 denied=0 expired=0" in command().stdout
    assert sql("SELECT count(*) FROM generation_jobs") == "0"
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
    fixture_path.write_text(json.dumps(dict(scheduled=scheduled, social=social, snapshot=after, policy=original_social)))
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
    fixture_path.write_text(json.dumps(fixture))
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
else:
    raise AssertionError("Unknown generation smoke phase")
