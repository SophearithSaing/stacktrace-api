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
            timeout=180,
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
                self.assertIn("Smoke passed", result.result())
            tests = [
                pool.submit(self.command, "make", "test", "TEST_ARGS=-run TestSeed -v")
                for _ in range(2)
            ]
            for result in tests:
                self.assertIn("PASS: TestSeedPreservesProfileEdits", result.result())
        self.assertEqual(list((self.checkout / ".dev/runs").iterdir()), [])

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
