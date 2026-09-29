"""Linux lifecycle regressions against a private checkout copy and real PostgreSQL.

Run directly with python3 scripts/test_runner.py. This intentionally sits outside
make test: invoking that command is part of the behavior under test.
"""

import concurrent.futures
import http.server
import json
import os
import shutil
import signal
import socket
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def running(metadata):
    if not metadata.exists():
        return False
    pid, start, boot = metadata.read_text().split()
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()
        return (
            fields[0] != "Z"
            and fields[19] == start
            and Path("/proc/sys/kernel/random/boot_id").read_text().strip() == boot
        )
    except FileNotFoundError:
        return False


class BackendSelectionTests(unittest.TestCase):
    def test_docker_preference_native_fallback_and_saved_choice(self):
        parent = ROOT / ".dev/runner-checks"
        parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="selection.", dir=parent) as directory:
            root = Path(directory)
            binaries = root / "bin"
            binaries.mkdir()
            for name in ("initdb", "postgres", "pg_isready", "createdb", "psql"):
                path = binaries / name
                path.write_text("#!/bin/sh\necho 'postgres (PostgreSQL) 16.15'\n")
                path.chmod(0o700)
            pg_config = binaries / "pg_config"
            pg_config.write_text('#!/bin/sh\nprintf "%s\\n" "$(dirname "$0")"\n')
            pg_config.chmod(0o700)
            docker = binaries / "docker"
            docker.write_text(
                '#!/bin/sh\n[ "$1" = info ] && [ "$DOCKER_AVAILABLE" = 1 ]\n'
            )
            docker.chmod(0o700)
            env = dict(os.environ, PATH=str(binaries) + ":" + os.environ["PATH"])

            def select(state, available):
                state.mkdir(exist_ok=True)
                subprocess.run(
                    [
                        "bash",
                        "-c",
                        "set -euo pipefail; source scripts/common.sh; state=$1; init_settings",
                        "bash",
                        str(state),
                    ],
                    cwd=ROOT,
                    env=dict(env, DOCKER_AVAILABLE=str(available)),
                    check=True,
                )
                return (state / "backend").read_text().strip()

            self.assertEqual(select(root / "docker-state", 1), "docker")
            self.assertEqual(select(root / "native-state", 0), "native")
            # A saved database must not be silently replaced when Docker appears.
            self.assertEqual(select(root / "native-state", 1), "native")


class RunnerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        parent = ROOT / ".dev/runner-checks"
        parent.mkdir(parents=True, exist_ok=True)
        cls.checkout = Path(tempfile.mkdtemp(prefix="checkout.", dir=parent))
        shutil.copytree(
            ROOT,
            cls.checkout,
            dirs_exist_ok=True,
            ignore=shutil.ignore_patterns(".git", ".dev", "docs", "__pycache__"),
        )
        cls.state = cls.checkout / ".dev/dev"
        cls.env = dict(
            os.environ,
            DEV_PORT=str(free_port()),
            DEV_CLIENT_ORIGINS="http://localhost:5174",
        )
        cls.env.pop("TEST_ARGS", None)
        # Prove managed commands never use ambient deployment/libpq settings.
        cls.env.update(
            APP_ENV="production",
            DATABASE_URL="invalid",
            TEST_DATABASE_URL="invalid",
            HTTP_ADDR="invalid",
            API_PUBLIC_ORIGIN="invalid",
            CLIENT_ORIGINS="invalid",
            CSRF_SIGNING_KEY="invalid",
            CURSOR_SIGNING_KEY="invalid",
            PGSERVICE="invalid",
            TOGETHER_API_KEY="ambient-provider-key-must-not-be-used",
            STACKTRACE_GENERATION_SMOKE="ambient-harness-must-not-be-used",
        )
        print(f"Lifecycle verification checkout: {cls.checkout}", flush=True)

    def command(self, *args, expected=0, env=None):
        result = subprocess.run(
            args,
            cwd=self.checkout,
            env=env or self.env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=600,
            check=False,
        )
        self.assertEqual(result.returncode, expected, result.stdout)
        return result.stdout

    def request(self, path, headers=None):
        origin = (self.state / "api-origin").read_text().strip()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(
            urllib.request.Request(origin + path, headers=headers or {}), timeout=5
        ) as response:
            return json.load(response), response.headers

    def sql(self, statement):
        env = {k: v for k, v in self.env.items() if not k.startswith("PG")}
        if (self.state / "backend").read_text().strip() == "native":
            executable = str(Path((self.state / "pg-bin").read_text().strip()) / "psql")
            env.update(
                PGHOST=(self.state / "socket-dir").read_text().strip(),
                PGPORT="5432",
                PGUSER="stacktrace",
                PGDATABASE="stacktrace",
                PGPASSWORD=(self.state / "password").read_text().strip(),
            )
            args = [executable]
        else:
            owner = (self.state / "owner").read_text().strip()
            args = [
                "docker",
                "exec",
                "stacktrace-" + owner,
                "psql",
                "-U",
                "stacktrace",
                "-d",
                "stacktrace",
            ]
        return self.command(
            *args, "-v", "ON_ERROR_STOP=1", "-Atqc", statement, env=env
        ).strip()

    def tearDown(self):
        self.command("make", "dev-down")

    @classmethod
    def tearDownClass(cls):
        # This checkout's development environment is itself a disposable fixture.
        # Use the runner's ownership-aware cleanup before removing its metadata.
        if cls.state.exists():
            subprocess.run(
                ["make", "dev-down"],
                cwd=cls.checkout,
                env=cls.env,
                check=True,
                stdout=subprocess.DEVNULL,
                timeout=60,
            )
            retained = cls.checkout / ".dev/runs/development"
            retained.parent.mkdir(exist_ok=True)
            cls.state.rename(retained)
            subprocess.run(
                ["bash", "scripts/dev.sh", "clean-run", str(retained)],
                cwd=cls.checkout,
                env=cls.env,
                check=True,
                timeout=60,
            )

    def assert_database_removed(self, state):
        self.assertFalse(running(state / "postgres.pid"))
        self.assertFalse((state / "data").exists())
        if (state / "backend").read_text().strip() == "docker":
            name = "stacktrace-" + (state / "owner").read_text().strip()
            self.command("docker", "container", "inspect", name, expected=1)
            self.command("docker", "volume", "inspect", name, expected=1)

    def assert_serve_cleanup(self, state):
        """Owned children joined before the isolated schema drop, with no leak."""
        cleanup = (state / "serve-cleanup.log").read_text()
        lines = [line for line in cleanup.splitlines() if line]
        self.assertTrue(lines and lines[0] == "joined", cleanup)
        self.assertIn("schema_dropped=true", lines[1:], cleanup)
        pid_files = list(state.glob("serve-*.pid"))
        self.assertTrue(pid_files, "service/API child pid was not recorded")
        for pid_file in pid_files:
            pid = int(pid_file.read_text())
            self.assertFalse(Path(f"/proc/{pid}").exists(), f"owned child leaked: {pid}")

    def test_restart_persistence_overrides_and_failed_build(self):
        self.command("make", "dev-up")
        keys = [
            (self.state / name).read_bytes()
            for name in ("password", "csrf-key", "cursor-key")
        ]
        pid = (self.state / "api.pid").read_text()
        profile, headers = self.request(
            "/api/v1/accounts/by-handle/golang", {"Origin": "http://localhost:5174"}
        )
        self.assertEqual(
            headers["Access-Control-Allow-Origin"], "http://localhost:5174"
        )
        self.assertEqual(profile["handle"], "golang")
        self.sql(
            "UPDATE accounts SET display_name='Edited locally', bio='Keep me' WHERE handle='golang'"
        )
        self.command("make", "dev-up")
        self.assertNotEqual(pid, (self.state / "api.pid").read_text())
        self.command("make", "dev-down")
        self.command("make", "dev-down")
        self.command("make", "dev-up")
        profile, _ = self.request("/api/v1/accounts/by-handle/golang")
        self.assertEqual(
            (profile["display_name"], profile["bio"]), ("Edited locally", "Keep me")
        )
        self.assertEqual(self.sql("SELECT count(*) FROM accounts"), "2")
        self.assertEqual(self.sql("SELECT count(*) FROM follows"), "2")
        self.assertEqual(
            keys,
            [
                (self.state / name).read_bytes()
                for name in ("password", "csrf-key", "cursor-key")
            ],
        )
        fake_bin = self.checkout / "fake-bin"
        fake_bin.mkdir(exist_ok=True)
        fake_go = fake_bin / "go"
        fake_go.write_text("#!/bin/sh\necho 'deliberate build failure' >&2\nexit 77\n")
        fake_go.chmod(0o700)
        pid = (self.state / "api.pid").read_text()
        self.command(
            "make",
            "dev-up",
            expected=2,
            env=dict(self.env, PATH=str(fake_bin) + ":" + self.env["PATH"]),
        )
        self.assertEqual(pid, (self.state / "api.pid").read_text())
        self.assertTrue(running(self.state / "api.pid"))
        self.assertIn("Readiness: ready", self.command("make", "dev-status"))

    def test_startup_failure_cleanup_and_existing_dependency(self):
        bad_env = dict(self.env, DEV_CLIENT_ORIGINS="invalid")
        self.command("make", "dev-up", expected=2, env=bad_env)
        self.assertFalse(running(self.state / "api.pid"))
        if (self.state / "backend").read_text().strip() == "native":
            self.assertFalse(running(self.state / "postgres.pid"))
        else:
            name = "stacktrace-" + (self.state / "owner").read_text().strip()
            self.assertEqual(
                self.command(
                    "docker", "inspect", "--format", "{{.State.Running}}", name
                ).strip(),
                "false",
            )
        self.command("make", "dev-up")
        self.command("make", "dev-up", expected=2, env=bad_env)
        self.assertIn("Database: running", self.command("make", "dev-status"))
        self.assertFalse(running(self.state / "api.pid"))

    def test_stale_metadata_does_not_stop_unrelated_process(self):
        self.command("make", "dev-up")
        if (self.state / "backend").read_text().strip() == "native":
            metadata = self.state / "postgres.pid"
            saved = metadata.read_text()
            try:
                metadata.write_text(saved.split()[0] + " 0 stale-boot\n")
                self.command("make", "dev-down", expected=2)
                self.assertTrue((self.state / "data/postmaster.pid").exists())
            finally:
                metadata.write_text(saved)
            self.assertTrue(running(metadata))
        self.command("make", "dev-down")
        with subprocess.Popen(["sleep", "60"]) as unrelated:
            try:
                (self.state / "api.pid").write_text(f"{unrelated.pid} 0 stale-boot\n")
                self.command("make", "dev-down")
                self.assertIsNone(unrelated.poll())
                self.assertFalse((self.state / "api.pid").exists())
            finally:
                unrelated.terminate()

    def test_busy_port_is_not_mistaken_for_ready_api(self):
        class ReadyHandler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"status":"ready"}')

            def log_message(self, format: str, *args: object) -> None:
                pass

        with http.server.HTTPServer(("127.0.0.1", 0), ReadyHandler) as unrelated:
            thread = threading.Thread(target=unrelated.serve_forever, daemon=True)
            thread.start()
            try:
                port = unrelated.server_address[1]
                self.command(
                    "make", "dev-up", expected=2, env=dict(self.env, DEV_PORT=str(port))
                )
                self.assertFalse(running(self.state / "api.pid"))
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                with opener.open(
                    f"http://127.0.0.1:{port}/readyz", timeout=2
                ) as response:
                    self.assertEqual(response.status, 200)
            finally:
                unrelated.shutdown()
                thread.join()

    def test_concurrent_smoke_and_tests(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            smoke = [pool.submit(self.command, "make", "smoke") for _ in range(2)]
            for result in smoke:
                output = result.result()
                self.assertIn("Smoke passed", output)
                self.assertIn("after API restart", output)
                self.assertIn("Check-only configuration OK: configured=0 enabled=0; no generation executed", output)
                self.assertIn("Check-only configuration OK: configured=2 enabled=0; no generation executed", output)
                self.assertIn("Schedule refused unmigrated database without DDL.", output)
                self.assertIn("visited=2 invalid=1 enqueued=1", output)
                self.assertIn("Live trigger smoke passed", output)
                self.assertIn("Schedule restart verification passed", output)
                self.assertIn("Fake-provider execution passed", output)
                self.assertIn("Execution verified after API restart", output)
                self.assertIn("Continuous service smoke passed", output)
                self.assertEqual(output.count("calls=1 recovered=0 expired=0 published=1"), 4, output)
                self.assertEqual(output.count("retried=1 denied=0"), 1, output)
                self.assertEqual(output.count("Execution pass complete: probes=8 claimed=0 calls=0"), 4, output)
                self.assertNotIn(self.env["TOGETHER_API_KEY"], output)
                self.assertEqual(output.count("API ready:"), 3, output)
            tests = [
                pool.submit(self.command, "make", "test", "TEST_ARGS=-run TestSeed -v")
                for _ in range(2)
            ]
            for result in tests:
                self.assertIn("PASS: TestSeedPreservesProfileEdits", result.result())
        self.assertEqual(list((self.checkout / ".dev/runs").iterdir()), [])

    def test_smoke_restart_verification_failure_cleans_services(self):
        smoke = self.checkout / "scripts/smoke.py"
        original = smoke.read_text()
        try:
            # Fail only after setup and the second API startup, exercising the
            # existing ownership cleanup/trap path for the new verify phase.
            smoke.write_text(
                'import sys\nif len(sys.argv) > 2 and sys.argv[2] == "verify":\n'
                '    raise SystemExit(77)\n'
                + original
            )
            output = self.command("make", "smoke", expected=2)
            self.assertEqual(output.count("API ready:"), 2, output)
            self.assertIn("Smoke setup passed", output)
            runs = list((self.checkout / ".dev/runs").iterdir())
            self.assertEqual(len(runs), 1)
            state = runs[0]
            self.assertFalse(running(state / "api.pid"))
            self.assertFalse(running(state / "job.pid"))
            self.assertFalse((state / "password").exists())
            self.assertFalse((state / "worker").exists())
            self.assertFalse((state / "worker.test").exists())
            self.assertFalse((state / "admin").exists())
            self.assertFalse((state / "generation-fixture.json").exists())
            self.assert_database_removed(state)
            fixture = json.loads((state / "smoke-fixture.json").read_text())
            self.assertEqual(
                set(fixture),
                {
                    "account_id", "followed_id", "follower_count", "tag", "post_id",
                    "body", "code", "counts", "quote_id", "quote_body", "reply_ids", "event_ids",
                },
            )
            self.assertEqual((state / "smoke-fixture.json").stat().st_mode & 0o777, 0o600)
            self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        finally:
            smoke.write_text(original)

    def test_execution_restart_failure_cleans_harness_and_fixtures(self):
        smoke = self.checkout / "scripts/smoke_generation.py"
        original = smoke.read_text()
        try:
            # Reach the third API startup with all new private fixtures present,
            # then fail to exercise the same ownership-aware cleanup path.
            smoke.write_text(
                'import os, sys\nfrom pathlib import Path\n'
                'if len(sys.argv) > 2 and sys.argv[2] == "execute-verify":\n'
                '    state = Path(sys.argv[1])\n'
                '    for name in ("generation-fixture.json", "execution-command.json", "execution-fixture.json"):\n'
                '        assert (state / name).stat().st_mode & 0o777 == 0o600\n'
                '    assert not os.environ.get("TOGETHER_API_KEY")\n'
                '    assert not os.environ.get("STACKTRACE_GENERATION_SMOKE")\n'
                '    assert (state / "worker.test").is_file()\n'
                '    print("Private execution fixtures verified", flush=True)\n'
                '    raise SystemExit(77)\n'
                + original
            )
            output = self.command("make", "smoke", expected=2)
            self.assertEqual(output.count("API ready:"), 3, output)
            self.assertIn("Fake-provider execution passed", output)
            self.assertIn("Private execution fixtures verified", output)
            runs = list((self.checkout / ".dev/runs").iterdir())
            self.assertEqual(len(runs), 1)
            state = runs[0]
            self.assertFalse(running(state / "api.pid"))
            self.assertFalse(running(state / "job.pid"))
            for name in ("password", "worker", "worker.test", "admin", "generation-fixture.json", "execution-command.json", "execution-fixture.json"):
                self.assertFalse((state / name).exists(), name)
            self.assert_database_removed(state)
            self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        finally:
            smoke.write_text(original)

    def test_service_smoke_failure_cleans_child(self):
        smoke = self.checkout / "scripts/smoke_generation.py"
        original = smoke.read_text()
        try:
            # Fail right after the Python-owned continuous service child starts,
            # then verify the child is joined/killed and no run state leaks.
            smoke.write_text(
                original.replace(
                    "        wait_ready(port, 200)\n",
                    "        raise SystemExit(77)\n",
                )
            )
            output = self.command("make", "smoke", expected=2)
            self.assertEqual(output.count("API ready:"), 3, output)
            self.assertIn("Fake-provider execution passed", output)
            runs = list((self.checkout / ".dev/runs").iterdir())
            self.assertEqual(len(runs), 1)
            state = runs[0]
            self.assertFalse(running(state / "api.pid"))
            self.assertFalse(running(state / "job.pid"))
            pid_files = list(state.glob("serve-*.pid"))
            self.assertTrue(pid_files, "service child pid was not recorded")
            for pid_file in pid_files:
                pid = int(pid_file.read_text())
                self.assertFalse(Path(f"/proc/{pid}").exists(), f"service child leaked: {pid}")
            self.assert_database_removed(state)
            self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        finally:
            smoke.write_text(original)

    def test_service_smoke_nonzero_child_exit_fails_and_cleans(self):
        smoke = self.checkout / "scripts/smoke_generation.py"
        original = smoke.read_text()
        try:
            # Kill only the last restarted service child so a nonzero final exit is
            # detected instead of being reported as smoke success.
            patched = original.replace(
                '        # The final graceful shutdown must succeed before success or cleanup is claimed.\n        stop_serve(serve_entry)\n',
                '        serve_entry["process"].kill()\n        serve_entry["process"].wait()\n        stop_serve(serve_entry)\n',
            )
            self.assertNotEqual(patched, original, "smoke patch did not apply")
            smoke.write_text(patched)
            output = self.command("make", "smoke", expected=2)
            self.assertEqual(output.count("API ready:"), 3, output)
            self.assertIn("Fake-provider execution passed", output)
            self.assertNotIn("Continuous service smoke passed", output)
            runs = list((self.checkout / ".dev/runs").iterdir())
            self.assertEqual(len(runs), 1)
            state = runs[0]
            self.assertFalse(running(state / "api.pid"))
            self.assertFalse(running(state / "job.pid"))
            self.assert_serve_cleanup(state)
            self.assert_database_removed(state)
            self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        finally:
            smoke.write_text(original)

    def test_service_smoke_spawn_pid_failure_cleans_child(self):
        smoke = self.checkout / "scripts/smoke_generation.py"
        original = smoke.read_text()
        try:
            # Fail immediately after the first owned child spawns and its pid is
            # recorded: the child must still be reaped by cleanup.
            patched = original.replace(
                '        (state / f"{name}-{process.pid}.pid").write_text(str(process.pid))\n',
                '        (state / f"{name}-{process.pid}.pid").write_text(str(process.pid))\n        raise SystemExit(88)\n',
            )
            self.assertNotEqual(patched, original, "spawn patch did not apply")
            smoke.write_text(patched)
            output = self.command("make", "smoke", expected=2)
            self.assertEqual(output.count("API ready:"), 3, output)
            self.assertIn("Fake-provider execution passed", output)
            runs = list((self.checkout / ".dev/runs").iterdir())
            self.assertEqual(len(runs), 1)
            state = runs[0]
            self.assertFalse(running(state / "api.pid"))
            self.assertFalse(running(state / "job.pid"))
            self.assert_serve_cleanup(state)
            self.assert_database_removed(state)
            self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        finally:
            smoke.write_text(original)

    def test_service_smoke_join_failure_skips_schema_drop(self):
        smoke = self.checkout / "scripts/smoke_generation.py"
        original = smoke.read_text()
        try:
            # Report a child-join failure (children are still terminated first): the
            # schema must not be dropped and no success may be printed.
            patched = original.replace(
                "    return errors\n",
                '    return errors + ["injected join failure"]\n',
            )
            self.assertNotEqual(patched, original, "cleanup patch did not apply")
            smoke.write_text(patched)
            output = self.command("make", "smoke", expected=2)
            self.assertNotIn("Continuous service smoke passed", output)
            runs = list((self.checkout / ".dev/runs").iterdir())
            self.assertEqual(len(runs), 1)
            state = runs[0]
            self.assertFalse(running(state / "api.pid"))
            self.assertFalse(running(state / "job.pid"))
            cleanup = (state / "serve-cleanup.log").read_text()
            lines = [line for line in cleanup.splitlines() if line]
            self.assertTrue(lines and lines[0] == "join_failed", cleanup)
            self.assertIn("schema_dropped=false", lines[1:], cleanup)
            for pid_file in state.glob("serve-*.pid"):
                pid = int(pid_file.read_text())
                self.assertFalse(Path(f"/proc/{pid}").exists(), f"owned child leaked: {pid}")
            self.assert_database_removed(state)
            self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        finally:
            smoke.write_text(original)

    def test_interrupt_while_service_blocked_cleans_child(self):
        runs_dir = self.checkout / ".dev/runs"
        existing = set(runs_dir.glob("smoke.*")) if runs_dir.exists() else set()
        log = self.checkout / "interrupt-serve.log"
        process = None
        try:
            with log.open("w") as stream:
                process = subprocess.Popen(
                    ["bash", "scripts/dev.sh", "smoke"],
                    cwd=self.checkout,
                    env=self.env,
                    stdout=stream,
                    stderr=subprocess.STDOUT,
                )
                # Interrupt only once the continuous service is blocked mid-call,
                # not merely after it spawns.
                marker = None
                deadline = time.monotonic() + 300
                while process.poll() is None and time.monotonic() < deadline:
                    for candidate in runs_dir.glob("smoke.*/serve-started"):
                        if candidate.parent not in existing:
                            marker = candidate
                            break
                    if marker is not None:
                        break
                    time.sleep(0.25)
                self.assertIsNotNone(marker, log.read_text())
                process.send_signal(signal.SIGTERM)
                self.assertEqual(
                    process.wait(timeout=120), 128 + signal.SIGTERM, log.read_text()
                )
        finally:
            if process is not None and process.poll() is None:
                process.terminate()
                process.wait(timeout=60)
        runs = list(runs_dir.iterdir())
        self.assertEqual(len(runs), 1)
        state = runs[0]
        self.assertFalse(running(state / "api.pid"))
        self.assertFalse(running(state / "job.pid"))
        # Observable ordering: owned children joined before the isolated schema drop.
        self.assert_serve_cleanup(state)
        self.assert_database_removed(state)
        self.command("bash", "scripts/dev.sh", "clean-run", str(state))

    def test_test_failure_retains_logs_and_removes_services(self):
        self.command("make", "test", "TEST_ARGS=-run [", expected=2)
        runs = list((self.checkout / ".dev/runs").iterdir())
        self.assertEqual(len(runs), 1)
        state = runs[0]
        self.assertTrue((state / "commands.log").exists())
        self.assertFalse((state / "password").exists())
        self.assert_database_removed(state)
        self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        self.assertFalse(state.exists())

    def test_interrupt_cleans_up_active_test(self):
        probe = self.checkout / "internal/runnerprobe"
        probe.mkdir()
        (probe / "probe_test.go").write_text("""package runnerprobe
import ("os"; "testing"; "time")
func TestRunnerInterrupt(t *testing.T) {
 if err := os.WriteFile(os.Getenv("RUNNER_PROBE_FILE"), []byte("ready"), 0600); err != nil { t.Fatal(err) }
 time.Sleep(time.Minute)
}
""")
        marker = self.checkout / "probe-ready"
        output = self.checkout / "interrupt.log"
        try:
            for sig in (signal.SIGINT, signal.SIGTERM):
                with self.subTest(signal=sig), output.open("w") as log:
                    marker.unlink(missing_ok=True)
                    process = subprocess.Popen(
                        ["bash", "scripts/dev.sh", "test"],
                        cwd=self.checkout,
                        env=dict(
                            self.env,
                            TEST_ARGS="-run TestRunnerInterrupt",
                            RUNNER_PROBE_FILE=str(marker),
                        ),
                        stdout=log,
                        stderr=subprocess.STDOUT,
                    )
                    try:
                        deadline = time.monotonic() + 60
                        while (
                            not marker.exists()
                            and process.poll() is None
                            and time.monotonic() < deadline
                        ):
                            time.sleep(0.1)
                        self.assertTrue(marker.exists(), output.read_text())
                        process.send_signal(sig)
                        self.assertEqual(
                            process.wait(timeout=25), 128 + sig, output.read_text()
                        )
                    finally:
                        if process.poll() is None:
                            process.terminate()
                            process.wait(timeout=25)
                    runs = list((self.checkout / ".dev/runs").iterdir())
                    self.assertEqual(len(runs), 1)
                    state = runs[0]
                    self.assertFalse(running(state / "job.pid"))
                    self.assert_database_removed(state)
                    self.command("bash", "scripts/dev.sh", "clean-run", str(state))
        finally:
            shutil.rmtree(probe)


if __name__ == "__main__":
    result = unittest.main(verbosity=2, exit=False).result
    if hasattr(RunnerTests, "checkout"):
        if result.wasSuccessful():
            shutil.rmtree(RunnerTests.checkout)
        else:
            print(f"Lifecycle diagnostics: {RunnerTests.checkout}", flush=True)
    raise SystemExit(0 if result.wasSuccessful() else 1)
