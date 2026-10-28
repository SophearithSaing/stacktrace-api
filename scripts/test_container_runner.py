"""Offline failure/ownership regressions for check_container; no Docker or cloud access."""

import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import check_container


class FakeDocker:
    def __init__(self):
        self.calls = []
        self.containers = {}
        self.network = None
        self.image = None
        self.owner = None
        self.migrated = False
        self.fail = None
        self.wrong_owner = False

    def run(self, command, **kwargs):
        args = command[1:]
        self.calls.append((args, kwargs["env"]))
        if self.fail is not None:
            self.fail(args)
        output, code = "", 0
        if args[0] == "build":
            self.image = args[args.index("--tag") + 1]
            self.owner = args[args.index("--label") + 1].split("=", 1)[1]
        elif args[:2] == ["image", "inspect"]:
            if self.image is None:
                output, code = "No such image", 1
            elif "--format" in args:
                output = self.owner
            else:
                output = json.dumps([{"Architecture": "amd64", "Os": "linux", "Config": {
                    "User": "65532:65532", "Entrypoint": ["/app/server"],
                }}])
        elif args[:2] == ["image", "rm"]:
            self.image = None
        elif args[:2] == ["network", "create"]:
            self.network = args[-1]
        elif args[:2] == ["network", "inspect"]:
            output = self.owner
        elif args[:2] == ["network", "rm"]:
            self.network = None
        elif args[0] == "create":
            name = args[args.index("--name") + 1]
            self.containers[name] = args
            if args[-1] == "migrate":
                self.migrated = True
        elif args[0] == "inspect":
            if args[-1] not in self.containers:
                output, code = "No such container", 1
            elif "State.Running" in args[2]:
                output = "true"
            else:
                output = "another-owner" if self.wrong_owner else self.owner
        elif args[0] == "wait":
            output = "0\n"
        elif args[0] == "exec" and "psql" in args:
            output = "0\n"
        elif args[0] == "exec" and "bash" in args:
            output = "503\n" if args[-1] == "/readyz" and not self.migrated else "200\n"
        elif args[0] == "rm":
            del self.containers[args[-1]]
        return subprocess.CompletedProcess(command, code, stdout=output)

class ContainerRunnerTests(unittest.TestCase):
    def setUp(self):
        root = "/tmp/opencode" if Path("/tmp/opencode").is_dir() else None
        temporary = tempfile.TemporaryDirectory(prefix="container-runner.", dir=root)
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.docker = FakeDocker()

    def run_check(self):
        with (mock.patch.object(check_container, "ROOT", self.root),
              mock.patch.object(check_container.subprocess, "run", side_effect=self.docker.run),
              mock.patch.object(check_container.time, "sleep"),
              contextlib.redirect_stdout(io.StringIO())):
            check_container.main()

    def retained_logs(self):
        return list(self.root.glob(".dev/runs/container-check.*/container-check.log"))

    def test_success_is_isolated_and_removes_owned_resources(self):
        with mock.patch.dict(check_container.os.environ, {
            "DATABASE_URL": "ambient-database-secret", "TOGETHER_API_KEY": "ambient-provider-secret",
        }):
            self.run_check()
        self.assertEqual(self.docker.containers, {})
        self.assertIsNone(self.docker.network)
        self.assertIsNone(self.docker.image)
        self.assertEqual(self.retained_logs(), [])
        network = next(args for args, _ in self.docker.calls if args[:2] == ["network", "create"])
        self.assertIn("--internal", network)
        for args, env in self.docker.calls:
            self.assertNotIn("-p", args)
            self.assertNotIn("ambient-database-secret", env["DATABASE_URL"])
            self.assertNotEqual(env["TOGETHER_API_KEY"], "ambient-provider-secret")
            self.assertNotIn(env["POSTGRES_PASSWORD"], " ".join(args))

    def test_daemon_failure_does_not_build_or_create_resources(self):
        def fail(args):
            if args[0] == "info":
                raise subprocess.TimeoutExpired("docker", 20)
        self.docker.fail = fail
        with self.assertRaises(subprocess.TimeoutExpired):
            self.run_check()
        self.assertEqual(len(self.docker.calls), 1)
        self.assertEqual(self.docker.containers, {})
        logs = self.retained_logs()
        self.assertEqual(len(logs), 1)
        self.assertEqual(logs[0].read_text(), "Docker info\n")

    def test_failed_build_does_not_hide_the_original_failure(self):
        def fail(args):
            if args[0] == "build":
                raise subprocess.TimeoutExpired("docker", 600)
        self.docker.fail = fail
        with self.assertRaises(subprocess.TimeoutExpired):
            self.run_check()
        self.assertEqual(self.docker.containers, {})
        self.assertEqual(len(self.retained_logs()), 1)

    def test_interruption_after_create_cleans_partial_resources(self):
        def fail(args):
            if args[0] == "start" and args[-1].endswith("-worker"):
                raise KeyboardInterrupt()
        self.docker.fail = fail
        with self.assertRaises(KeyboardInterrupt):
            self.run_check()
        self.assertEqual(self.docker.containers, {})
        self.assertIsNone(self.docker.network)
        self.assertIsNone(self.docker.image)
        self.assertEqual(len(self.retained_logs()), 1)

    def test_cleanup_failure_is_a_blocker_and_preserves_diagnostics(self):
        def fail(args):
            if args[0] == "rm":
                raise subprocess.TimeoutExpired("docker", 30)
        self.docker.fail = fail
        with self.assertRaisesRegex(RuntimeError, "cleanup failed"):
            self.run_check()
        self.assertTrue(self.docker.containers)
        self.assertEqual(len(self.retained_logs()), 1)

    def test_ownership_mismatch_never_removes_other_containers(self):
        self.docker.wrong_owner = True
        with self.assertRaisesRegex(RuntimeError, "cleanup failed"):
            self.run_check()
        self.assertTrue(self.docker.containers)
        self.assertFalse(any(args[0] == "rm" for args, _ in self.docker.calls))
        self.assertEqual(len(self.retained_logs()), 1)


if __name__ == "__main__":
    unittest.main()
