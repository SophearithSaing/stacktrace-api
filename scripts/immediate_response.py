"""One isolated human-post trigger, shared by paid and fake-provider checks."""

from datetime import datetime, timezone
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys

from smoke import client

AGENT = "aacebb3d-a794-4cf6-ae22-c8e0a7ca1a01"
QUESTION = "@golang How would you keep a Go worker loop bounded and shut it down gracefully?"
RESERVATION = 132096


def admin(state, *args):
    """Run a bounded operator command without inheriting provider credentials."""
    env = dict(os.environ)
    env.pop("TOGETHER_API_KEY", None)
    result = subprocess.run(
        [str(state / "admin"), *args], env=env, text=True,
        capture_output=True, timeout=35,
    )
    assert result.returncode == 0, "Operator command failed"
    return json.loads(result.stdout)


def write_private(path, payload):
    """Atomically write safe data so partial evidence is never mistaken for a report."""
    assert not path.exists(), "Refusing to overwrite a private fixture"
    temporary = path.with_name(path.name + ".tmp")
    with open(temporary, "x", opener=lambda name, flags: os.open(name, flags, 0o600)) as output:
        json.dump(payload, output)
    temporary.rename(path)


def check_day(state, day):
    """Refuse UTC rollover risk and inconsistent host/database clocks."""
    now = datetime.now(timezone.utc)
    assert now.date().isoformat() == day, "Database UTC day mismatch"
    if state.name.startswith("live-response."):
        assert now.hour != 23 or now.minute < 40, "Too close to UTC rollover"


def setup(state):
    """Install one finite policy and create its source through authenticated HTTP."""
    admin(state, "agent", "pause", "--all")
    status = admin(state, "status")
    check_day(state, status["today_usage"]["day"])
    assert status["agents"]["configured"] == 2 and status["agents"]["enabled"] == 0
    assert status["today_usage"]["attempts"] == 0
    assert not admin(state, "job", "list")["jobs"]
    policy = {
        "version": 1, "timezone": "UTC", "active_start": "00:00", "active_end": "23:59",
        "scheduled_min_per_day": 0, "scheduled_max_per_day": 0, "min_spacing_seconds": 1,
        "response_min_delay_seconds": 0, "response_max_delay_seconds": 0,
        "source_max_age_seconds": 900, "reply_probability_bps": 0,
        "repost_probability_bps": 0, "quote_probability_bps": 0,
        "human_post_probability_bps": 10000, "continuation_probability_bps": 0,
        "cooldown_seconds": 60, "scheduled_post_cap_per_day": 0,
        "reply_cap_per_day": 1, "reply_cap_per_conversation": 1,
        "max_agents_per_trigger": 1, "human_trigger_cap_per_window": 1,
        "human_trigger_window_seconds": 900, "max_chain_depth": 0,
        "max_chain_jobs": 1, "daily_token_budget": RESERVATION,
    }
    path = state / "response-policy.json"
    write_private(path, policy)
    admin(state, "persona", "select", AGENT, "1")
    admin(state, "policy", "set", AGENT, str(path))
    admin(state, "agent", "resume", AGENT)
    request, _ = client(os.environ["API_PUBLIC_ORIGIN"])
    profile = request("/api/v1/accounts/by-handle/golang")
    assert profile["id"] == AGENT and profile["type"] == "agent"
    request("/api/v1/auth/register", "POST", {
        "username": "probe_" + secrets.token_hex(6),
        "password": secrets.token_hex(24), "display_name": "Inference Probe",
    }, expected=201)
    me = request("/api/v1/me")
    post = request("/api/v1/posts", "POST", {"body": QUESTION},
                   csrf=me["csrf_token"], key=secrets.token_hex(16), expected=201)
    jobs = admin(state, "job", "list")
    assert len(jobs["jobs"]) == 1 and not jobs["next_cursor"]
    job = jobs["jobs"][0]
    assert job["AgentID"] == AGENT and job["PersonaVersion"] == 1
    assert job["TriggerKind"] == "human_post" and job["OutputKind"] == "reply"
    assert job["SourcePostID"] == post["id"] and job["TriggerActorID"] == me["account"]["id"]
    assert job["Status"] == "pending"
    write_private(state / "response-fixture.json", {
        "job_id": job["ID"], "post_id": post["id"], "day": status["today_usage"]["day"],
    })
    # Only the test binary reads this; the live branch always runs the real worker.
    write_private(state / "execution-command.json", {"mode": "publish", "calls": 1})
    preflight(state)
    print("Immediate trigger ready: one pending human-post job; no inference yet.")


def preflight(state):
    """Check fresh, single-job authority immediately before provider execution."""
    fixture = json.loads((state / "response-fixture.json").read_text())
    status = admin(state, "status")
    check_day(state, fixture["day"])
    assert status["today_usage"]["day"] == fixture["day"]
    assert status["agents"]["enabled"] == 1 and status["today_usage"]["attempts"] == 0
    listing = admin(state, "job", "list")
    assert len(listing["jobs"]) == 1 and not listing["next_cursor"]
    job = listing["jobs"][0]
    assert job["ID"] == fixture["job_id"] and job["Status"] == "pending"
    now = datetime.now(timezone.utc)
    assert datetime.fromisoformat(job["AvailableAt"].replace("Z", "+00:00")) <= now
    assert (datetime.fromisoformat(job["ExpiresAt"].replace("Z", "+00:00")) - now).total_seconds() > 120
    inspection = admin(state, "job", "inspect", fixture["job_id"])
    assert inspection["complete"] and not inspection["attempts"]


def report(state):
    """Preserve safe durable evidence, including unresolved interrupted attempts."""
    fixture = json.loads((state / "response-fixture.json").read_text())
    inspection = admin(state, "job", "inspect", fixture["job_id"])
    usage = admin(state, "usage", "--agent", AGENT, "--day", fixture["day"])
    job, attempts = inspection["job"], inspection["attempts"] or []
    record = {
        "job_id": job["ID"], "post_id": fixture["post_id"], "status": job["Status"],
        "reason": job["ReasonCode"], "reply_id": job["ResultReplyID"],
        "published_attempt_id": job["PublishedAttemptID"], "usage": usage,
        "attempts": [{field: attempt[field] for field in (
            "ID", "Status", "ErrorCode", "StartedAt", "FinishedAt", "BudgetDay",
            "ReservedTokens", "InputTokens", "OutputTokens",
        )} for attempt in attempts],
    }
    write_private(state / "response-report.json", record)
    print(json.dumps(record, indent=2))
    return fixture, inspection, usage


def verify(state):
    """Verify one known-accounting publication through a fresh API read, then pause."""
    try:
        fixture, inspection, usage = report(state)
        job, attempts = inspection["job"], inspection["attempts"] or []
        assert inspection["complete"] and len(attempts) == usage["attempts"] == 1
        attempt = attempts[0]
        assert attempt["ReservedTokens"] == RESERVATION and attempt["BudgetDay"] == fixture["day"]
        assert job["Status"] == "succeeded" and job["ResultReplyID"]
        assert job["PublishedAttemptID"] == attempt["ID"] and attempt["JobID"] == job["ID"]
        assert attempt["Status"] == "succeeded" and attempt["Decision"] == "publish"
        assert attempt["LeaseVersion"] == job["LeaseVersion"] and attempt["FinishedAt"]
        request, _ = client(os.environ["API_PUBLIC_ORIGIN"])
        page = request(f'/api/v1/posts/{fixture["post_id"]}/replies?sort=newest')
        assert len(page["items"]) == 1
        reply = page["items"][0]
        assert reply["id"] == job["ResultReplyID"] and reply["is_generated"] is True
        assert reply["author"]["id"] == AGENT
        print("Generated reply: " + json.dumps(reply["body"]))
        assert attempt["InputTokens"] is not None and 0 < attempt["InputTokens"] <= 8192
        assert attempt["OutputTokens"] is not None and 0 <= attempt["OutputTokens"] <= 1024
        assert usage["charged_tokens"] == usage["known_tokens"] == attempt["InputTokens"] + attempt["OutputTokens"]
        listing = admin(state, "job", "list")
        assert len(listing["jobs"]) == 1 and not listing["next_cursor"]
        print("Immediate response passed: one attempt, known usage, persisted generated reply; not MVP acceptance.")
    finally:
        admin(state, "agent", "pause", "--all")


def main():
    """Dispatch only runner-owned setup, preflight, and verification phases."""
    if len(sys.argv) != 3 or sys.argv[2] not in ("setup", "preflight", "verify", "report"):
        raise SystemExit("usage: immediate_response.py STATE setup|preflight|verify|report")
    state = Path(sys.argv[1]).resolve()
    assert state.parent == Path(__file__).resolve().parents[1] / ".dev/runs"
    assert state.name.startswith(("live-response.", "response-check."))
    assert os.environ["DATABASE_URL"] == os.environ["TEST_DATABASE_URL"]
    assert not os.environ.get("TOGETHER_API_KEY")
    {"setup": setup, "preflight": preflight, "verify": verify, "report": report}[sys.argv[2]](state)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        # Never echo HTTP/SQL/subprocess payloads, credentials, or exception text.
        print("Immediate response check incomplete; inspect the safe report/runner diagnostics. No automatic retry.", file=sys.stderr)
        raise SystemExit(1)
