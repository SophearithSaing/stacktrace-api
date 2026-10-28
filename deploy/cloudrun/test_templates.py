"""Offline safety contracts for the Cloud Run examples; no Google Cloud access."""

from pathlib import Path
import unittest

import yaml


ROOT = Path(__file__).resolve().parent


def template(name):
    return yaml.safe_load((ROOT / f"{name}.yaml").read_text())


def revision(document):
    if document["kind"] == "Service":
        return document["spec"]["template"]["spec"]
    return document["spec"]["template"]["spec"]["template"]["spec"]


class CloudRunTemplatesTests(unittest.TestCase):
    def test_roles_use_same_digest_and_separate_identities(self):
        images, identities, databases = set(), set(), set()
        commands = {"api": ["/app/server"], "worker": ["/app/worker"],
                    "migrate": ["/app/db"], "admin": ["/app/admin"]}
        for name, command in commands.items():
            with self.subTest(name=name):
                document = template(name)
                self.assertEqual(document["metadata"]["namespace"], "PROJECT_NUMBER")
                spec = revision(document)
                self.assertEqual(len(spec["containers"]), 1)
                container = spec["containers"][0]
                self.assertEqual(container["command"], command)
                images.add(container["image"])
                identities.add(spec["serviceAccountName"])
                env = {item["name"]: item for item in container["env"]}
                self.assertEqual(env["APP_ENV"]["value"], "production")
                databases.add(env["DATABASE_URL"]["valueFrom"]["secretKeyRef"]["name"])
        self.assertEqual(images, {"REGION-docker.pkg.dev/PROJECT_ID/REPOSITORY/stacktrace@sha256:IMAGE_DIGEST"})
        self.assertEqual(len(identities), 4)
        self.assertEqual(len(databases), 4)

    def test_external_postgres_does_not_require_cloud_sql(self):
        for name in ("api", "worker", "migrate", "admin"):
            with self.subTest(name=name):
                document = template(name)
                for metadata in (document["metadata"], document["spec"]["template"].get("metadata", {})):
                    self.assertNotIn("run.googleapis.com/cloudsql-instances", metadata.get("annotations", {}))
                spec = revision(document)
                for volume in spec.get("volumes", []):
                    self.assertNotIn("cloudSqlInstance", volume)
                container = spec["containers"][0]
                for mount in container.get("volumeMounts", []):
                    self.assertFalse(mount["mountPath"].startswith("/cloudsql"))

    def test_credentials_are_versioned_role_scoped_secret_references(self):
        expected = {
            "api": {"DATABASE_URL", "CSRF_SIGNING_KEY", "CURSOR_SIGNING_KEY"},
            "worker": {"DATABASE_URL", "TOGETHER_API_KEY"},
            "migrate": {"DATABASE_URL"}, "admin": {"DATABASE_URL"},
        }
        for name, secret_names in expected.items():
            with self.subTest(name=name):
                container = revision(template(name))["containers"][0]
                secrets = {item["name"] for item in container["env"] if "valueFrom" in item}
                self.assertEqual(secrets, secret_names)
                for item in container["env"]:
                    if item["name"] in secret_names:
                        self.assertNotIn("value", item)
                        self.assertEqual(item["valueFrom"]["secretKeyRef"]["key"], "SECRET_VERSION")

    def test_worker_runs_privately_between_requests(self):
        document = template("worker")
        annotations = document["metadata"]["annotations"]
        self.assertEqual(annotations["run.googleapis.com/ingress"], "internal")
        self.assertEqual(annotations["run.googleapis.com/invoker-iam-disabled"], "false")
        self.assertEqual(annotations["run.googleapis.com/minScale"], "1")
        self.assertEqual(annotations["run.googleapis.com/maxScale"], "1")
        annotations = document["spec"]["template"]["metadata"]["annotations"]
        self.assertEqual(annotations["run.googleapis.com/cpu-throttling"], "false")
        self.assertEqual(annotations["autoscaling.knative.dev/maxScale"], "1")
        container = revision(document)["containers"][0]
        self.assertEqual(container["args"], ["serve"])
        self.assertEqual(container["startupProbe"]["httpGet"]["path"], "/healthz")
        self.assertEqual(container["livenessProbe"]["httpGet"]["path"], "/healthz")

    def test_api_schema_startup_and_same_site_origins(self):
        document = template("api")
        self.assertEqual(document["metadata"]["annotations"]["run.googleapis.com/maxScale"], "3")
        container = revision(document)["containers"][0]
        self.assertNotIn("args", container)
        self.assertEqual(container["startupProbe"]["httpGet"]["path"], "/readyz")
        self.assertEqual(container["livenessProbe"]["httpGet"]["path"], "/healthz")
        env = {item["name"]: item.get("value") for item in container["env"]}
        self.assertEqual(env["API_PUBLIC_ORIGIN"], "https://api.DOMAIN")
        self.assertEqual(env["CLIENT_ORIGINS"], "https://app.DOMAIN")

    def test_platform_port_and_probes_agree_without_listener_overrides(self):
        for name in ("api", "worker"):
            with self.subTest(name=name):
                container = revision(template(name))["containers"][0]
                port = container["ports"][0]["containerPort"]
                env_names = {item["name"] for item in container["env"]}
                self.assertTrue(env_names.isdisjoint({"PORT", "HTTP_ADDR", "WORKER_HTTP_ADDR"}))
                for probe_name in ("startupProbe", "livenessProbe"):
                    probe = container[probe_name]
                    self.assertEqual(probe["httpGet"]["port"], port)
                    self.assertLessEqual(probe["timeoutSeconds"], probe["periodSeconds"])
                probe = container["startupProbe"]
                self.assertLessEqual(probe["periodSeconds"] * probe["failureThreshold"], 240)

    def test_jobs_are_explicit_non_retried_and_safe_by_default(self):
        for name, args in (("migrate", ["migrate"]), ("admin", ["status"])):
            with self.subTest(name=name):
                document = template(name)
                self.assertEqual(document["kind"], "Job")
                spec = document["spec"]["template"]["spec"]
                self.assertEqual(spec["taskCount"], 1)
                self.assertEqual(spec["parallelism"], 1)
                task = revision(document)
                self.assertEqual(task["maxRetries"], 0)
                self.assertLessEqual(int(task["timeoutSeconds"]), 120)
                self.assertEqual(task["containers"][0]["args"], args)


if __name__ == "__main__":
    unittest.main()
