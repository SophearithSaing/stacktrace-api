"""Build and check the release image with owned Docker resources and no inference.

Requires a working Docker daemon; never skips container/database coverage.
Run with make container-check. Runtime logs are retained only on failure.
"""

import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import tempfile
import time


ROOT = Path(__file__).resolve().parents[1]


def main():
    runs = ROOT / ".dev" / "runs"
    runs.mkdir(parents=True, exist_ok=True)
    state = Path(tempfile.mkdtemp(prefix="container-check.", dir=runs))
    print(f"Runtime artifacts: {state}", flush=True)
    owner = secrets.token_hex(12)
    network = f"stacktrace-container-{owner}"
    image = f"stacktrace-container-check:{owner}"
    database = f"{network}-db"
    api = f"{network}-api"
    worker = f"{network}-worker"
    containers = []
    network_created = False
    image_created = False
    success = False
    env = dict(os.environ)
    # Docker receives only the explicit -e names below, never ambient credentials.
    env.update(
        APP_ENV="production", PORT="8080", POSTGRES_USER="stacktrace",
        POSTGRES_DB="stacktrace", POSTGRES_PASSWORD=secrets.token_hex(24),
        API_PUBLIC_ORIGIN="https://api.example.com",
        CLIENT_ORIGINS="https://app.example.com",
        CSRF_SIGNING_KEY=secrets.token_hex(32),
        CURSOR_SIGNING_KEY=secrets.token_hex(32),
        TOGETHER_API_KEY="synthetic-unusable-container-test-key",
    )
    env["DATABASE_URL"] = (
        f"postgres://stacktrace:{env['POSTGRES_PASSWORD']}@{database}/stacktrace?sslmode=disable"
    )

    def interrupted(signum, frame):
        raise KeyboardInterrupt("container check interrupted")

    def docker(*args, timeout=30, check=True):
        with (state / "container-check.log").open("a") as log:
            # Record progress even if a command times out or is interrupted.
            # Arguments can carry deployment data and must not be logged.
            log.write(f"Docker {args[0]}\n")
            log.flush()
            result = subprocess.run(
                ["docker", *args], cwd=ROOT, env=env, text=True,
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout,
            )
            log.write(result.stdout)
        if check and result.returncode:
            # Do not echo Docker command arguments, environment or raw errors.
            raise RuntimeError(f"Docker {args[0]} failed; see {state}/container-check.log")
        return result

    def start(name, *args):
        # Register before create: even a failed/interrupted response may have
        # created the container. Only this unique owner can be removed below.
        containers.append(name)
        docker("create", "--name", name, "--label", f"stacktrace.owner={owner}",
               "--network", network, *args)
        docker("start", name)

    def wait_http(name, path, status):
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            # The PostgreSQL image already includes Bash/coreutils. Probe from
            # there: internal networks need no host port or extra helper image.
            result = docker("exec", database, "timeout", "3", "bash", "-ec",
                            'exec 3<>"/dev/tcp/$1/8080"; '
                            'printf "GET %s HTTP/1.1\\r\\nHost: localhost\\r\\nConnection: close\\r\\n\\r\\n" "$2" >&3; '
                            'IFS=" " read -r protocol status rest <&3; printf "%s\\n" "$status"',
                            "--", name, path, check=False)
            if result.returncode == 0 and result.stdout.strip() == str(status):
                return
            if docker("inspect", "--format", "{{.State.Running}}", name).stdout.strip() != "true":
                raise RuntimeError("release process exited before its expected health result")
            time.sleep(0.1)
        raise RuntimeError("release process health check timed out")

    def cli(binary, *args):
        name = f"{network}-cli-{len(containers)}"
        start(name, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
              "-e", "APP_ENV", "-e", "DATABASE_URL", "--entrypoint", f"/app/{binary}", image, *args)
        result = docker("wait", name, timeout=90).stdout.strip()
        docker("logs", name)
        if result != "0":
            raise RuntimeError(f"packaged {binary} command failed")

    old_term = signal.signal(signal.SIGTERM, interrupted)
    old_int = signal.getsignal(signal.SIGINT)
    try:
        # Full Docker info may contain proxy credentials from the host.
        docker("info", "--format", "{{.ServerVersion}}", timeout=20)
        print("Building Linux amd64 release image", flush=True)
        # Mark before building so an interrupted successful build is also cleaned.
        image_created = True
        docker("build", "--platform", "linux/amd64", "--label", f"stacktrace.owner={owner}", "--tag", image, ".", timeout=600)
        info = json.loads(docker("image", "inspect", image).stdout)[0]
        if info["Architecture"] != "amd64" or info["Os"] != "linux":
            raise RuntimeError("release image must target Linux amd64")
        if info["Config"]["User"] != "65532:65532" or info["Config"]["Entrypoint"] != ["/app/server"]:
            raise RuntimeError("release image must run the API directly as non-root")

        docker("pull", "postgres:18", timeout=180)
        network_created = True
        # No container in this check has internet egress, including the worker.
        docker("network", "create", "--internal", "--label", f"stacktrace.owner={owner}", network)
        start(database, "-e", "POSTGRES_USER", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB", "postgres:18")
        for _ in range(150):
            if docker("exec", database, "pg_isready", "-q", "-h", "127.0.0.1", "-U", "stacktrace", check=False).returncode == 0:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("container PostgreSQL startup timed out")

        common = ("--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
                  "-e", "APP_ENV", "-e", "PORT", "-e", "DATABASE_URL")
        # Startup must not silently migrate or seed a reachable empty database.
        start(api, *common, "-e", "API_PUBLIC_ORIGIN", "-e", "CLIENT_ORIGINS",
              "-e", "CSRF_SIGNING_KEY", "-e", "CURSOR_SIGNING_KEY", image)
        wait_http(api, "/healthz", 200)
        wait_http(api, "/readyz", 503)
        count = docker("exec", database, "psql", "-U", "stacktrace", "-d", "stacktrace", "-Atc",
                       "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'").stdout.strip()
        if count != "0":
            raise RuntimeError("API startup modified the empty database")
        cli("db", "ping")
        cli("db", "migrate")
        cli("db", "migrate")  # Idempotent explicit migration, never seeding.
        wait_http(api, "/readyz", 200)
        cli("worker", "check")
        cli("admin", "status")

        start(worker, *common, "-e", "TOGETHER_API_KEY", "--entrypoint", "/app/worker", image, "serve")
        wait_http(worker, "/healthz", 200)
        wait_http(worker, "/readyz", 200)
        # Direct PID-1 SIGTERM, not docker stop's automatic SIGKILL fallback.
        for name in (worker, api):
            started = time.monotonic()
            docker("kill", "--signal", "TERM", name)
            exit_code = docker("wait", name, timeout=9).stdout.strip()
            if exit_code != "0" or time.monotonic() - started >= 9:
                raise RuntimeError("release process did not exit cleanly within the platform deadline")
            docker("logs", name)
        count = docker("exec", database, "psql", "-U", "stacktrace", "-d", "stacktrace", "-Atc",
                       "SELECT (SELECT count(*) FROM accounts)+(SELECT count(*) FROM generation_attempts)").stdout.strip()
        if count != "0":
            raise RuntimeError("container check seeded data or attempted generation")
        success = True
    finally:
        # An interruption must not interrupt cleanup a second time. Every removal
        # checks ownership; unrelated containers, networks and images are untouched.
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        cleanup_errors = []
        for name in reversed(containers):
            try:
                info = docker("inspect", "--format", '{{index .Config.Labels "stacktrace.owner"}}', name, check=False)
                if info.returncode == 0:
                    if info.stdout.strip() != owner:
                        raise RuntimeError("container ownership mismatch")
                    docker("logs", name, check=False)
                    docker("rm", "--force", "--volumes", name)
                elif "No such" not in info.stdout:
                    raise RuntimeError("container cleanup could not be confirmed")
            except (RuntimeError, subprocess.SubprocessError) as err:
                cleanup_errors.append(type(err).__name__)
        if network_created:
            try:
                info = docker("network", "inspect", "--format", '{{index .Labels "stacktrace.owner"}}', network, check=False)
                if info.returncode == 0:
                    if info.stdout.strip() != owner:
                        raise RuntimeError("network ownership mismatch")
                    docker("network", "rm", network)
                elif "No such" not in info.stdout and f"network {network} not found" not in info.stdout:
                    raise RuntimeError("network cleanup could not be confirmed")
            except (RuntimeError, subprocess.SubprocessError) as err:
                cleanup_errors.append(type(err).__name__)
        if image_created:
            try:
                info = docker("image", "inspect", "--format", '{{index .Config.Labels "stacktrace.owner"}}', image, check=False)
                if info.returncode == 0:
                    if info.stdout.strip() != owner:
                        raise RuntimeError("image ownership mismatch")
                    docker("image", "rm", image)
                elif "No such" not in info.stdout:
                    raise RuntimeError("image cleanup could not be confirmed")
            except (RuntimeError, subprocess.SubprocessError) as err:
                cleanup_errors.append(type(err).__name__)
        signal.signal(signal.SIGTERM, old_term)
        signal.signal(signal.SIGINT, old_int)
        if cleanup_errors:
            raise RuntimeError(f"container cleanup failed; retained diagnostics: {state}")
        if success:
            shutil.rmtree(state)
    print("PASS container: non-root API/worker, explicit db/admin CLIs, no seed/inference, bounded SIGTERM", flush=True)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, subprocess.SubprocessError, OSError, KeyboardInterrupt) as err:
        # Subprocess exceptions can carry arguments. Only safe errors cross this boundary.
        print(str(err) if isinstance(err, RuntimeError) else "Container check failed; see the printed runtime artifacts path.", flush=True)
        raise SystemExit(1)
