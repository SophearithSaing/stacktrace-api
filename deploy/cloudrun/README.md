# Cloud Run templates

These are deployment examples, not provisioned resources or an automatic release.
The public API and private continuous worker use separate services and identities;
migrations and administration use explicitly executed jobs.

## Prepare

1. Provision Artifact Registry, Cloud SQL PostgreSQL, four service accounts and
   Secret Manager values in the selected project/region.
2. Build and push the root `Dockerfile` for `linux/amd64`. Use the same immutable
   image digest for all four templates.
3. Copy the YAML files outside the checkout. Replace `PROJECT_NUMBER`, `PROJECT_ID`,
   `REGION`, `SQL_INSTANCE`, `REPOSITORY`, `IMAGE_DIGEST`, `DOMAIN`, and each
   `SECRET_VERSION` with the actual values. Secret versions must be numeric, not
   `latest`; values belong in Secret Manager, never the templates.
4. Restrict each identity to its own secrets and grant Cloud SQL Client access.
   Use different database users: runtime users need appropriate data access, not
   schema ownership; only the migration user owns/applies the schema. Restrict
   job execution/argument overrides to trusted release operators. Do not grant
   public invocation to the worker or jobs.

The Cloud SQL integration mounts `/cloudsql/PROJECT_ID:REGION:SQL_INSTANCE`.
Each database URL secret uses the appropriate user/password and database. Shape:

```text
postgres://USER:ENCODED_PASSWORD@localhost/DATABASE?host=%2Fcloudsql%2FPROJECT_ID%3AREGION%3ASQL_INSTANCE&sslmode=disable
```

Percent-encode credentials and the socket path. `sslmode=disable` here is only
for the local Unix socket: the managed Cloud SQL proxy encrypts the remote
connection. It is not a recommendation for unprotected remote TCP connections.
The hostname remains present for application URL validation; pgx uses the socket
specified in the query parameter.

## Release order

After CI passes, use `gcloud run jobs replace` to register the matching migration
job and `gcloud run jobs execute stacktrace-migrate --wait` to apply it explicitly.
Stop if it fails. Use the selected `--project` and `--region` on every command.
Only then use `gcloud run services replace` for the API and worker. The admin job
defaults to read-only `status`; replacing a job does not execute it. No template
runs demo seeding or enables agents. Enabling paid generation is a separate
operator decision after reviewing personas, policies and finite budgets.

Schema-changing releases need a controlled maintenance window: exact migration
checks make old revisions unready after newer migrations. Do not assume rolling
schema upgrades or image-only rollback; rehearse recovery/restore first.

Cloud Run supplies `PORT`; do not add conflicting listener overrides. Worker CPU
must remain always allocated with minimum one instance. The maximum of one is
not a singleton guarantee during deployments/replacement; database leases and
reservations still govern authority. Check combined connection use (ten per
process, including overlapping revisions and jobs) against Cloud SQL capacity.
Use `/readyz` for operational readiness monitoring, not worker restart policy:
provider degradation is deliberately separate from liveness.

Public rollout still requires same-site HTTPS app/API domains, browser checks,
proxy/client-IP rate-limit validation, IAM/network review, alerts, spend controls,
backups/restore and deployed recovery/load checks. No trusted forwarding-header
policy or cloud provisioning is introduced by these examples.

## Local verification

```sh
make container-check
python3 -B -m unittest discover -s scripts -p test_container_runner.py
make openapi-setup
make cloudrun-check
```

Container checks require Docker and real disposable PostgreSQL, keep all runtime
containers on an internal network, and never seed or make provider calls. They
verify non-root execution, explicit CLI migration, readiness and direct SIGTERM
shutdown; failures retain the printed diagnostics directory. Template tests are
offline safety checks, not proof that Google Cloud accepts a rendered deployment.
