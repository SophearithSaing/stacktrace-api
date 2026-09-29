"""Disposable built-worker/HTTP integration fixtures, never development seeds."""

import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

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


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def probe(port, path):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(f"http://127.0.0.1:{port}{path}", timeout=2) as response:
            return response.status
    except urllib.error.HTTPError as error:
        return error.code
    except Exception:
        return None


def wait_ready(port, expected, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if probe(port, "/readyz") == expected:
            return
        time.sleep(0.2)
    raise AssertionError(f"readiness did not become {expected}")


def wait_sql(statement, expected="1", timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if sql(statement) == expected:
            return
        time.sleep(0.25)
    raise AssertionError("timed out waiting for a SQL fixture state")


def wait_started(started, job_id, timeout=60):
    """Wait for the blocked provider call, failing early with the job's reason."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if started.exists():
            return
        job_state = sql(f"SELECT status||':'||COALESCE(reason_code,'-') FROM generation_jobs WHERE id='{job_id}'")
        if job_state.split(':')[0] not in ("pending", "running"):
            raise AssertionError("blocked trigger terminal before provider call: " + job_state)
        time.sleep(0.25)
    raise AssertionError("blocked trigger never reached the provider")


def admin(*args):
    env = {key: value for key, value in os.environ.items() if key != "TOGETHER_API_KEY"}
    result = subprocess.run([os.environ["STACKTRACE_ADMIN"], *args], env=env, text=True, capture_output=True, timeout=30)
    assert result.returncode == 0, f"admin {' '.join(args)}: unexpected exit"
    assert os.environ["DATABASE_URL"] not in result.stdout + result.stderr
    return json.loads(result.stdout)


def write_serve_control(path, mode="publish", block=False):
    write_fixture(path, dict(mode=mode, block=block))


def start_serve(control, started, release):
    port = free_port()
    env = dict(
        os.environ,
        WORKER_HTTP_ADDR=f"127.0.0.1:{port}",
        STACKTRACE_SERVE_SMOKE=str(control),
        STACKTRACE_SERVE_STARTED=str(started),
        STACKTRACE_SERVE_RELEASE=str(release),
        TOGETHER_API_KEY="",
    )
    env.pop("STACKTRACE_GENERATION_SMOKE", None)
    log = open(state / f"serve-{port}.log", "w")
    process = subprocess.Popen(
        [str(state / "worker.test"), "-test.run=^TestServeSmokeHarness$", "-test.count=1", "-test.timeout=300s"],
        env=env, text=True, stdout=log, stderr=subprocess.STDOUT,
    )
    return process, log, port


def stop_serve(process, log):
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)
    log.close()
    assert process.returncode == 0, f"serve harness exited {process.returncode}"


def serve_publications(agent, jobs):
    ids = ",".join(f"'{job}'" for job in jobs)
    rows = json.loads(sql(f"""SELECT json_agg(row_to_json(p) ORDER BY trigger_kind) FROM (
        SELECT j.id AS job_id,j.trigger_kind,j.result_post_id,j.result_reply_id,j.source_post_id,
            j.published_attempt_id,a.output_digest,
            COALESCE(p.body,r.body) AS body,
            (a.job_id=j.id AND a.lease_version=j.lease_version AND a.status='succeeded'
             AND a.decision='publish' AND a.input_tokens=100 AND a.output_tokens=20
             AND a.reserved_tokens=132096 AND a.provider_request_id='serve-request'
             AND COALESCE(p.author_id,r.author_id)=j.agent_id
             AND (r.id IS NULL OR r.post_id=j.source_post_id)) AS exact_provenance
        FROM generation_jobs j JOIN generation_attempts a ON a.id=j.published_attempt_id
        LEFT JOIN posts p ON p.id=j.result_post_id LEFT JOIN replies r ON r.id=j.result_reply_id
        WHERE j.status='succeeded' AND j.agent_id='{agent}' AND j.id IN ({ids})) p"""))
    assert len(rows) == 4 and {row["trigger_kind"] for row in rows} == {"scheduled", "reply", "repost", "quote"}
    assert all(row["exact_provenance"] and re.fullmatch(r"generation_output_v1:[0-9a-f]{64}", row["output_digest"]) for row in rows)
    return rows


def serve_phase():
    fixture = json.loads(fixture_path.read_text())
    agent = fixture["scheduled"]
    control = state / "serve-command.json"
    started = state / "serve-started"
    release = state / "serve-release"
    baseline = set(json.loads(sql("SELECT COALESCE(json_agg(id),'[]') FROM generation_jobs WHERE status='succeeded'")))
    phase_start = sql("SELECT clock_timestamp()")
    before_scheduled = int(sql(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='scheduled' AND status='succeeded'"))

    sql(f"UPDATE agent_settings SET enabled=false WHERE agent_id<>'{agent}'")
    # Repair the other seed's intentionally invalid fixture so bounded status reads
    # remain valid, and give this agent a genuinely new due slot with headroom above
    # the scheduled post it already published earlier in the run.
    sql(f"UPDATE agent_settings SET policy=jsonb_set(policy,'{{timezone}}','\"UTC\"') WHERE agent_id<>'{agent}'")
    sql(f"""UPDATE agent_settings SET enabled=true, policy=policy || jsonb_build_object(
        'timezone','Etc/GMT','active_start','00:00','active_end','23:59',
        'scheduled_min_per_day',2,'scheduled_max_per_day',2,'scheduled_post_cap_per_day',3,
        'min_spacing_seconds',1,'response_min_delay_seconds',0,'response_max_delay_seconds',0,
        'human_post_probability_bps',0,'reply_probability_bps',10000,'repost_probability_bps',10000,
        'quote_probability_bps',10000,'continuation_probability_bps',0,'cooldown_seconds',1,
        'reply_cap_per_day',20,'reply_cap_per_conversation',10,'max_agents_per_trigger',1,
        'human_trigger_cap_per_window',50,'human_trigger_window_seconds',1,
        'source_max_age_seconds',7200,'daily_token_budget',5000000
    ) WHERE agent_id='{agent}';
    UPDATE agent_settings SET schedule_date=(clock_timestamp() AT TIME ZONE (policy->>'timezone'))::date,
        remaining_slots=1,next_post_at=clock_timestamp()+interval '1 second' WHERE agent_id='{agent}';""")

    write_serve_control(control, "publish")
    process, log, port = start_serve(control, started, release)
    (state / f"serve-{port}.pid").write_text(str(process.pid))
    try:
        wait_ready(port, 200)

        # The service schedules and publishes one post without any manual execute.
        wait_sql(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='scheduled' AND status='succeeded'", str(before_scheduled + 1), timeout=120)
        post_id = sql(f"SELECT result_post_id FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='scheduled' AND status='succeeded' ORDER BY created_at DESC, id DESC LIMIT 1")

        request, _ = client(os.environ["API_PUBLIC_ORIGIN"])
        request("/api/v1/auth/login", "POST", dict(username="generation_smoke", password="smoke-password-123"))
        csrf = request("/api/v1/me")["csrf_token"]

        # Human reply/repost/quote triggers are also claimed and published by the
        # service. Create each only after the previous publication settles, so the
        # agent's own finite admission spacing is respected by real timing.
        for kind in ("reply", "repost", "quote"):
            time.sleep(1.1)
            before_kind = int(sql(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND status='succeeded' AND trigger_kind='{kind}'"))
            if kind == "reply":
                path, method, body = f"/api/v1/posts/{post_id}/replies", "POST", dict(body="How should the continuous worker be tested?")
            elif kind == "repost":
                path, method, body = f"/api/v1/posts/{post_id}/repost", "PUT", None
            else:
                path, method, body = "/api/v1/posts", "POST", dict(body="A continuous example for discussion.", quoted_post_id=post_id)
            kwargs = dict(csrf=csrf, key=f"serve-{kind}", expected=200 if kind == "repost" else 201)
            request(path, method, body, **kwargs)
            wait_sql(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND status='succeeded' AND trigger_kind='{kind}'", str(before_kind + 1), timeout=90)
        current = set(json.loads(sql(f"SELECT COALESCE(json_agg(id),'[]') FROM generation_jobs WHERE status='succeeded' AND agent_id='{agent}'")))
        new_jobs = sorted(current - baseline)
        assert len(new_jobs) == 4, "autonomous service publication count mismatch"
        rows = serve_publications(agent, new_jobs)
        verify_http(request, rows)

        # Bounded operator reports from the separately built admin CLI.
        status = admin("status")
        assert set(status) == {"agents", "queue", "today_usage"} and status["agents"]["enabled"] == 1
        listed = admin("job", "list", "--agent", agent, "--limit", "3")
        assert len(listed["jobs"]) <= 3
        assert admin("job", "inspect", rows[0]["job_id"])["complete"] is True
        assert admin("usage")["charged_tokens"] >= 4 * 120
        # Built admin mutation against the live service: re-apply the current
        # (identical) policy, which preserves publication history and schedule.
        policy_file = state / "serve-policy.json"
        write_fixture(policy_file, json.loads(sql(f"SELECT policy FROM agent_settings WHERE agent_id='{agent}'")))
        assert admin("policy", "set", agent, str(policy_file))["policy_set"]["agent"] == agent

        # Pause during a blocked call: the admitted result must not publish. This
        # runs before the expensive unknown-accounting outcomes so the fixed fleet
        # token budget can still admit the later conservative-usage evidence.
        write_serve_control(control, "publish", block=True)
        started.unlink(missing_ok=True)
        release.unlink(missing_ok=True)
        time.sleep(1.1)
        request(f"/api/v1/posts/{post_id}/replies", "POST", dict(body="Pause must stop publication."), csrf, key="serve-pause", expected=201)
        pause_job = sql(f"SELECT id FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='reply' ORDER BY created_at DESC, id DESC LIMIT 1")
        wait_started(started, pause_job, timeout=60)
        assert admin("agent", "pause", agent)["action"] == "agent pause"
        write_serve_control(control, "publish")
        release.write_text("release")
        wait_sql(f"SELECT (status NOT IN ('pending','running'))::text FROM generation_jobs WHERE id='{pause_job}'", "true", timeout=120)
        assert sql(f"SELECT count(*) FROM generation_jobs WHERE id='{pause_job}' AND (status='succeeded' OR result_reply_id IS NOT NULL OR result_post_id IS NOT NULL)") == "0"
        assert admin("agent", "resume", agent)["action"] == "agent resume"
        release.unlink(missing_ok=True)

        # Provider degradation: unready, HTTP stays usable, then recovery on a
        # later real success. The timeout settles as conservative unknown usage.
        write_serve_control(control, "timeout")
        time.sleep(1.1)
        request(f"/api/v1/posts/{post_id}/replies", "POST", dict(body="Is the API usable while degraded?"), csrf, key="serve-degraded", expected=201)
        wait_sql("SELECT count(*) FROM generation_attempts WHERE error_code='provider_timeout' AND provider_request_id IS NULL AND status='unknown'", "1", timeout=120)
        wait_ready(port, 503, timeout=60)
        healthy = request("/api/v1/posts", "POST", dict(body="HTTP remains usable during provider failure."), csrf, key="serve-healthy", expected=201)
        assert request(f"/api/v1/posts/{healthy['id']}")["body"] == healthy["body"]
        write_serve_control(control, "publish")
        wait_ready(port, 200, timeout=120)

        # Remove the source reply during a blocked call: nothing may publish. This is
        # the last spending action so the fleet budget cannot starve a needed retry.
        write_serve_control(control, "publish", block=True)
        started.unlink(missing_ok=True)
        release.unlink(missing_ok=True)
        time.sleep(1.1)
        removed_reply = request(f"/api/v1/posts/{post_id}/replies", "POST", dict(body="Remove this reply while generating."), csrf, key="serve-remove", expected=201)["reply"]["id"]
        remove_job = sql(f"SELECT id FROM generation_jobs WHERE agent_id='{agent}' AND source_reply_id='{removed_reply}' ORDER BY created_at DESC, id DESC LIMIT 1")
        wait_started(started, remove_job, timeout=60)
        assert admin("reply", "remove", removed_reply)["removed"] is True
        write_serve_control(control, "publish")
        release.write_text("release")
        wait_sql(f"SELECT (status NOT IN ('pending','running'))::text FROM generation_jobs WHERE id='{remove_job}'", "true", timeout=120)
        assert sql(f"SELECT count(*) FROM generation_jobs WHERE id='{remove_job}' AND (status='succeeded' OR result_reply_id IS NOT NULL OR result_post_id IS NOT NULL)") == "0"
        release.unlink(missing_ok=True)

        # Unknown accounting (timeouts always, and cancellation when it races)
        # is charged the full reservation, never understated.
        unknown = int(sql("SELECT count(*) FROM generation_attempts WHERE status='unknown' AND reserved_tokens=132096 AND input_tokens IS NULL AND output_tokens IS NULL"))
        assert unknown >= 2, f"expected execute + service unknown timeouts, got {unknown}"
        assert admin("usage")["charged_tokens"] >= 2 * 132096

        # Pause, settle, then a graceful stop/restart must not replay durable work.
        assert admin("agent", "pause", agent)["action"] == "agent pause"
        time.sleep(6)
        leftover = sql(f"SELECT COALESCE(string_agg(trigger_kind||':'||status||':'||COALESCE(reason_code,'-'),'|'),'') FROM generation_jobs WHERE agent_id='{agent}' AND created_at >= '{phase_start}'::timestamptz AND status IN ('pending','retry_wait','running')")
        assert leftover == "", "leftover non-terminal jobs before restart: " + leftover
        before = execution_snapshot()
        stop_serve(process, log)
        process = log = None
        process, log, port = start_serve(control, started, release)
        wait_ready(port, 200)
        time.sleep(6)
        assert execution_snapshot() == before, "restart replayed or duplicated durable state"

        # Trusted account disable revokes authority without erasing provenance.
        assert admin("account", "disable", agent)["disabled"] is True
        assert sql(f"SELECT (disabled_at IS NOT NULL)::text FROM accounts WHERE id='{agent}'") == "true"
        print("Continuous service smoke passed: autonomous publications, admin controls, degradation recovery, conservative charging, pause/remove barriers and restart durability.")
    finally:
        if process is not None and process.poll() is None:
            process.send_signal(signal.SIGTERM)
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)
        if log is not None:
            log.close()
        sql(f"UPDATE agent_settings SET enabled=false WHERE agent_id='{agent}'")


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
elif phase == "serve":
    serve_phase()
else:
    raise AssertionError("Unknown generation smoke phase")
