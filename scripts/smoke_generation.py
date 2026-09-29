"""Disposable built-worker/HTTP integration fixtures, never development seeds."""

import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

from smoke import client


class ServeInterrupted(Exception):
    """Raised from a signal handler so serve cleanup unwinds before exit."""


def _interrupt(signum, _frame):
    raise ServeInterrupted(f"interrupted by signal {signum}")


# Managed TERM/INT must unwind through the phase's finally (terminating and
# joining Python-owned children) instead of killing the interpreter mid-wait.
signal.signal(signal.SIGTERM, _interrupt)
signal.signal(signal.SIGINT, _interrupt)


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


def wait_sql(statement, expected="1", timeout=60, query=None):
    query = query or sql
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if query(statement) == expected:
            return
        time.sleep(0.25)
    raise AssertionError("timed out waiting for a SQL fixture state")


def wait_started(started, job_id, timeout=60, query=None):
    """Wait for the blocked provider call, failing early with the job's reason."""
    query = query or sql
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if started.exists():
            return
        job_state = query(f"SELECT status||':'||COALESCE(reason_code,'-') FROM generation_jobs WHERE id='{job_id}'")
        if job_state.split(':')[0] not in ("pending", "running"):
            raise AssertionError("blocked trigger terminal before provider call: " + job_state)
        time.sleep(0.25)
    raise AssertionError("blocked trigger never reached the provider")


def admin(*args, database_url=None):
    env = {key: value for key, value in os.environ.items() if key != "TOGETHER_API_KEY"}
    if database_url is not None:
        env["DATABASE_URL"] = database_url
    result = subprocess.run([os.environ["STACKTRACE_ADMIN"], *args], env=env, text=True, capture_output=True, timeout=30)
    assert result.returncode == 0, f"admin {' '.join(args)}: unexpected exit"
    assert env["DATABASE_URL"] not in result.stdout + result.stderr
    return json.loads(result.stdout)


def write_serve_control(path, mode="publish", block=False):
    # Same-directory temp + atomic rename so the Go reader never sees a partial
    # or empty control file (which would now fail closed).
    payload = json.dumps(dict(mode=mode, block=block))
    temporary = path.with_name(path.name + f".tmp{os.getpid()}")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w") as file:
        file.write(payload)
    os.replace(temporary, path)


def spawn(children, name, argv, env, log_name):
    """Start an owned child and register it before anything else can fail.

    Signals are blocked across Popen and the ownership/pid record so an
    interruption cannot strand a just-created child.
    """
    previous = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM, signal.SIGINT})
    log = open(state / log_name, "w")
    try:
        process = subprocess.Popen(argv, env=env, text=True, stdout=log, stderr=subprocess.STDOUT)
    except BaseException:
        log.close()
        signal.pthread_sigmask(signal.SIG_SETMASK, previous)
        raise
    entry = {"name": name, "process": process, "log": log, "pid": process.pid}
    children.append(entry)
    try:
        (state / f"{name}-{process.pid}.pid").write_text(str(process.pid))
    finally:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous)
    return entry


def start_serve(children, control, started, release, counts_file, database_url):
    port = free_port()
    env = dict(
        os.environ,
        WORKER_HTTP_ADDR=f"127.0.0.1:{port}",
        STACKTRACE_SERVE_SMOKE=str(control),
        STACKTRACE_SERVE_STARTED=str(started),
        STACKTRACE_SERVE_RELEASE=str(release),
        STACKTRACE_SERVE_COUNTS=str(counts_file),
        DATABASE_URL=database_url,
        TEST_DATABASE_URL=database_url,
        TOGETHER_API_KEY="",
    )
    env.pop("STACKTRACE_GENERATION_SMOKE", None)
    entry = spawn(children, "serve", [str(state / "worker.test"), "-test.run=^TestServeSmokeHarness$", "-test.count=1", "-test.timeout=300s"], env, f"serve-{port}.log")
    return entry, port


def start_schema_api(children, database_url):
    port = free_port()
    origin = f"http://127.0.0.1:{port}"
    env = dict(
        os.environ,
        DATABASE_URL=database_url,
        HTTP_ADDR=f"127.0.0.1:{port}",
        API_PUBLIC_ORIGIN=origin,
        APP_ENV="development",
    )
    env.pop("TOGETHER_API_KEY", None)
    entry = spawn(children, "serve-api", [str(state / "server")], env, f"serve-api-{port}.log")
    return entry, origin


def wait_api(origin, timeout=30):
    deadline = time.monotonic() + timeout
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    while time.monotonic() < deadline:
        try:
            with opener.open(origin + "/readyz", timeout=2) as response:
                if response.status == 200:
                    return
        except Exception:
            pass
        time.sleep(0.2)
    raise AssertionError("isolated serve API did not become ready")


def terminate(process, log, check):
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)
    log.close()
    if check and process.returncode != 0:
        raise AssertionError(f"serve harness exited {process.returncode}")
    return process.returncode


def stop_serve(entry):
    return terminate(entry["process"], entry["log"], check=True)


def terminate_children(children):
    """Boundedly stop owned children, joining service children before the API."""
    errors = []
    ordered = sorted(children, key=lambda child: 0 if child["name"] == "serve" else 1)
    for child in ordered:
        try:
            terminate(child["process"], child["log"], check=False)
        except Exception:
            errors.append(f"{child['name']} child cleanup failed")
    return errors


def schema_sql(schema, statement):
    # SET search_path runs in the same libpq session as the statement, so the
    # psql path targets the disposable schema exactly like the pgx URL does.
    return sql(f"SET search_path TO {schema}; {statement}")


def schema_execution_snapshot(schema):
    return schema_sql(schema, """SELECT json_build_object(
        'jobs',(SELECT json_agg(j ORDER BY id) FROM generation_jobs j),
        'attempts',(SELECT json_agg(a ORDER BY id) FROM generation_attempts a),
        'settings',(SELECT json_agg(s ORDER BY agent_id) FROM agent_settings s),
        'posts',(SELECT json_agg(p ORDER BY id) FROM posts p),
        'replies',(SELECT json_agg(r ORDER BY id) FROM replies r))""")


def serve_publications(agent, jobs, query=None):
    query = query or sql
    ids = ",".join(f"'{job}'" for job in jobs)
    rows = json.loads(query(f"""SELECT json_agg(row_to_json(p) ORDER BY trigger_kind) FROM (
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
    original_origin = os.environ["API_PUBLIC_ORIGIN"]
    base_snapshot = snapshot()
    schema = "serve_" + secrets.token_hex(4)
    control = state / "serve-command.json"
    started = state / "serve-started"
    release = state / "serve-release"
    counts_file = state / "serve-counts.json"
    children = []
    schema_created = False
    scenario_ok = False

    def query(statement):
        return schema_sql(schema, statement)

    try:
        sql(f"CREATE SCHEMA {schema}")
        schema_created = True
        parsed = urllib.parse.urlparse(os.environ["DATABASE_URL"])
        joined = parsed.query + ("&" if parsed.query else "") + "search_path=" + schema
        serve_url = urllib.parse.urlunparse(parsed._replace(query=joined))
        seed_env = dict(os.environ, DATABASE_URL=serve_url)
        for verb in ("migrate", "seed"):
            result = subprocess.run([str(state / "db"), verb], env=seed_env, text=True, capture_output=True, timeout=90)
            assert result.returncode == 0, f"isolated serve schema {verb} failed"

        _, serve_origin = start_schema_api(children, serve_url)
        wait_api(serve_origin)

        agent = query("SELECT agent_id FROM agent_settings ORDER BY agent_id LIMIT 1")
        assert agent, "isolated seed has no agent"
        query(f"UPDATE agent_settings SET enabled=false WHERE agent_id<>'{agent}'")
        # Keep the seeded convention: derive a database-time timezone whose local
        # clock is near noon, so scheduled slots cannot cross a midnight boundary.
        offset = int(query("SELECT extract(hour from clock_timestamp() AT TIME ZONE 'UTC')::int")) - 12
        zone = "Etc/GMT" + ("+" if offset >= 0 else "") + str(offset)
        query(f"""UPDATE agent_settings SET enabled=true, policy=policy || jsonb_build_object(
            'timezone','{zone}','active_start','00:00','active_end','23:59',
            'scheduled_min_per_day',1,'scheduled_max_per_day',1,'scheduled_post_cap_per_day',1,
            'min_spacing_seconds',1,'response_min_delay_seconds',0,'response_max_delay_seconds',0,
            'human_post_probability_bps',0,'reply_probability_bps',10000,'repost_probability_bps',10000,
            'quote_probability_bps',10000,'continuation_probability_bps',0,'cooldown_seconds',1,
            'reply_cap_per_day',20,'reply_cap_per_conversation',10,'max_agents_per_trigger',1,
            'human_trigger_cap_per_window',50,'human_trigger_window_seconds',1,
            'source_max_age_seconds',7200,'daily_token_budget',10000000
        ) WHERE agent_id='{agent}';
        UPDATE agent_settings SET schedule_date=(clock_timestamp() AT TIME ZONE (policy->>'timezone'))::date,
            remaining_slots=1,next_post_at=clock_timestamp()+interval '1 second' WHERE agent_id='{agent}';""")

        request, _ = client(serve_origin)
        request("/api/v1/auth/register", "POST", dict(username="serve_" + secrets.token_hex(4), display_name="Serve Smoke", password="serve-smoke-password-123"), expected=201)
        csrf = request("/api/v1/me")["csrf_token"]
        baseline = set(json.loads(query("SELECT COALESCE(json_agg(id),'[]') FROM generation_jobs WHERE status='succeeded'")))

        write_serve_control(control, "publish")
        serve_entry, port = start_serve(children, control, started, release, counts_file, serve_url)
        wait_ready(port, 200)

        # The service schedules and publishes one post without any manual execute.
        wait_sql(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='scheduled' AND status='succeeded'", "1", timeout=120, query=query)
        post_id = query(f"SELECT result_post_id FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='scheduled' AND status='succeeded' ORDER BY created_at DESC, id DESC LIMIT 1")

        # Human reply/repost/quote triggers are also claimed and published by the
        # service. Create each only after the previous publication settles, so the
        # agent's own finite admission spacing is respected by real timing.
        for kind in ("reply", "repost", "quote"):
            time.sleep(1.1)
            before_kind = int(query(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND status='succeeded' AND trigger_kind='{kind}'"))
            if kind == "reply":
                path, method, body = f"/api/v1/posts/{post_id}/replies", "POST", dict(body="How should the continuous worker be tested?")
            elif kind == "repost":
                path, method, body = f"/api/v1/posts/{post_id}/repost", "PUT", None
            else:
                path, method, body = "/api/v1/posts", "POST", dict(body="A continuous example for discussion.", quoted_post_id=post_id)
            request(path, method, body, csrf=csrf, key=f"serve-{kind}", expected=200 if kind == "repost" else 201)
            wait_sql(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND status='succeeded' AND trigger_kind='{kind}'", str(before_kind + 1), timeout=90, query=query)
        current = set(json.loads(query(f"SELECT COALESCE(json_agg(id),'[]') FROM generation_jobs WHERE status='succeeded' AND agent_id='{agent}'")))
        new_jobs = sorted(current - baseline)
        assert len(new_jobs) == 4, "autonomous service publication count mismatch"
        rows = serve_publications(agent, new_jobs, query=query)
        verify_http(request, rows)

        # Bounded operator reports and mutations from the built admin CLI against
        # the isolated schema.
        status = admin("status", database_url=serve_url)
        assert set(status) == {"agents", "queue", "today_usage"} and status["agents"]["enabled"] == 1
        assert len(admin("job", "list", "--agent", agent, "--limit", "3", database_url=serve_url)["jobs"]) <= 3
        assert admin("job", "inspect", rows[0]["job_id"], database_url=serve_url)["complete"] is True
        assert admin("usage", database_url=serve_url)["charged_tokens"] >= 4 * 120
        policy_file = state / "serve-policy.json"
        write_fixture(policy_file, json.loads(query(f"SELECT policy FROM agent_settings WHERE agent_id='{agent}'")))
        assert admin("policy", "set", agent, str(policy_file), database_url=serve_url)["policy_set"]["agent"] == agent

        # Pause during a blocked call, then supervise a stop: context cancellation
        # settles the in-flight call as conservative unknown usage retaining the
        # full reservation, and the pre-pause result never publishes.
        write_serve_control(control, "publish", block=True)
        started.unlink(missing_ok=True)
        release.unlink(missing_ok=True)
        time.sleep(1.1)
        request(f"/api/v1/posts/{post_id}/replies", "POST", dict(body="Pause then stop must retain the reservation."), csrf, key="serve-pause", expected=201)
        pause_job = query(f"SELECT id FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='reply' ORDER BY created_at DESC, id DESC LIMIT 1")
        wait_started(started, pause_job, timeout=60, query=query)
        assert admin("agent", "pause", agent, database_url=serve_url)["action"] == "agent pause"
        stop_serve(serve_entry)
        wait_sql(f"SELECT (status NOT IN ('pending','running'))::text FROM generation_jobs WHERE id='{pause_job}'", "true", timeout=120, query=query)
        assert query(f"SELECT count(*) FROM generation_jobs WHERE id='{pause_job}' AND (status='succeeded' OR result_reply_id IS NOT NULL OR result_post_id IS NOT NULL)") == "0"
        assert query(f"SELECT count(*) FROM generation_attempts WHERE job_id='{pause_job}' AND status='unknown' AND error_code='execution_cancelled' AND reserved_tokens=132096 AND input_tokens IS NULL") == "1"
        release.unlink(missing_ok=True)

        # Restart preserves the isolated durable snapshot exactly; the persisted
        # fake-body sequence prevents a repeat of an already-published body.
        before = schema_execution_snapshot(schema)
        serve_entry, port = start_serve(children, control, started, release, counts_file, serve_url)
        wait_ready(port, 200)
        assert schema_execution_snapshot(schema) == before, "restart replayed or duplicated isolated durable state"
        assert admin("agent", "resume", agent, database_url=serve_url)["action"] == "agent resume"

        # Provider degradation: unready, isolated HTTP stays usable, then a later
        # real success recovers readiness.
        write_serve_control(control, "timeout")
        time.sleep(1.1)
        request(f"/api/v1/posts/{post_id}/replies", "POST", dict(body="Is the isolated API usable while degraded?"), csrf, key="serve-degraded", expected=201)
        wait_sql("SELECT count(*) FROM generation_attempts WHERE error_code='provider_timeout' AND provider_request_id IS NULL AND status='unknown'", "1", timeout=120, query=query)
        wait_ready(port, 503, timeout=60)
        healthy = request("/api/v1/posts", "POST", dict(body="HTTP remains usable during provider failure."), csrf, key="serve-healthy", expected=201)
        assert request(f"/api/v1/posts/{healthy['id']}")["body"] == healthy["body"]
        write_serve_control(control, "publish")
        wait_ready(port, 200, timeout=120)
        wait_sql(f"SELECT count(*) FROM generation_jobs WHERE agent_id='{agent}' AND trigger_kind='reply' AND status='succeeded'", "2", timeout=120, query=query)

        # Final removal during a blocked call: nothing may publish, and the isolated
        # ledger still had headroom for this last 132,096 reservation.
        write_serve_control(control, "publish", block=True)
        started.unlink(missing_ok=True)
        release.unlink(missing_ok=True)
        time.sleep(1.1)
        removed_reply = request(f"/api/v1/posts/{post_id}/replies", "POST", dict(body="Remove this reply while generating."), csrf, key="serve-remove", expected=201)["reply"]["id"]
        remove_job = query(f"SELECT id FROM generation_jobs WHERE agent_id='{agent}' AND source_reply_id='{removed_reply}' ORDER BY created_at DESC, id DESC LIMIT 1")
        wait_started(started, remove_job, timeout=60, query=query)
        assert admin("reply", "remove", removed_reply, database_url=serve_url)["removed"] is True
        write_serve_control(control, "publish")
        release.write_text("release")
        wait_sql(f"SELECT (status NOT IN ('pending','running'))::text FROM generation_jobs WHERE id='{remove_job}'", "true", timeout=120, query=query)
        assert query(f"SELECT count(*) FROM generation_jobs WHERE id='{remove_job}' AND (status='succeeded' OR result_reply_id IS NOT NULL OR result_post_id IS NOT NULL)") == "0"
        release.unlink(missing_ok=True)

        # Isolated-ledger accounting: pause-cancel and timeout always retain the full
        # reservation, and the total stays inside the fixed daily budget so every
        # required admission (including the final removal) had headroom.
        assert int(query("SELECT count(*) FROM generation_attempts WHERE status='unknown' AND reserved_tokens=132096 AND input_tokens IS NULL")) >= 2
        charged = int(admin("usage", database_url=serve_url)["charged_tokens"])
        assert charged >= 2 * 132096, f"expected conservative unknown charges, got {charged}"
        assert charged < 500000, f"isolated ledger exceeded the fixed budget: {charged}"

        # Trusted account disable revokes authority without erasing provenance.
        assert admin("account", "disable", agent, database_url=serve_url)["disabled"] is True
        assert query(f"SELECT (disabled_at IS NOT NULL)::text FROM accounts WHERE id='{agent}'") == "true"
        # The final graceful shutdown must succeed before success or cleanup is claimed.
        stop_serve(serve_entry)
        scenario_ok = True
    finally:
        # Once unwinding, ignore further signals so cleanup actually completes.
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        cleanup_errors = terminate_children(children)
        dropped = False
        if schema_created:
            try:
                sql(f"DROP SCHEMA IF EXISTS {schema} CASCADE")
                dropped = sql(f"SELECT count(*) FROM information_schema.schemata WHERE schema_name='{schema}'") == "0"
            except Exception:
                dropped = False
            if not dropped:
                cleanup_errors.append("isolated schema drop incomplete")
        try:
            if snapshot() != base_snapshot:
                cleanup_errors.append("original schema snapshot changed")
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            with opener.open(original_origin.rstrip("/") + "/readyz", timeout=2) as response:
                if response.status != 200:
                    cleanup_errors.append("original API unready")
        except Exception:
            cleanup_errors.append("original API unready")
        with (state / "serve-cleanup.log").open("a") as cleanup_log:
            cleanup_log.write("joined\n")
            cleanup_log.write(f"schema_dropped={str(dropped).lower()}\n")
        if cleanup_errors:
            if scenario_ok:
                raise AssertionError("isolated serve cleanup failed: " + "; ".join(cleanup_errors))
            print("isolated serve cleanup incomplete: " + "; ".join(cleanup_errors), file=sys.stderr)
        elif scenario_ok:
            print("Continuous service smoke passed: isolated autonomous publications, admin controls, cancel/timeout unknown accounting, degraded-API separation, recovery and removal barriers.")


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
