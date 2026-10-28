# Cloud Run templates

These are deployment examples, not provisioned resources or an automatic release.
The public API and private continuous worker use separate services and identities;
migrations and administration use explicitly executed jobs.

## Prepare

1. Use your existing Neon PostgreSQL database. Set up Artifact Registry and
   Secret Manager values in the selected Google Cloud project/region; no Cloud SQL
   instance, socket, proxy or Cloud SQL Client IAM role is needed.
2. Build and push the root `Dockerfile` for `linux/amd64`. Use the same immutable
   image digest for all four templates.
3. Copy the YAML files outside the checkout. Replace `PROJECT_NUMBER`, `PROJECT_ID`,
   `REGION`, `REPOSITORY`, `IMAGE_DIGEST`, `DOMAIN`, and each
   `SECRET_VERSION` with the actual values. Secret versions must be numeric, not
   `latest`; values belong in Secret Manager, never the templates.
4. Set `serviceAccountName` to the Cloud Run identity you will use and grant it
   access to the referenced secrets. The examples show separate identities, but
   creating four custom accounts is not required; an existing service account can
   be used. Restrict job execution/argument overrides to trusted release operators.
   Do not grant public invocation to the worker or jobs.

## Neon connections

Copy the connection strings from Neon's connection dialog into Secret Manager,
not the YAML or repository. All four secrets must target the same Neon branch
and database. Use a Neon role with the appropriate privileges for each command;
separate restricted runtime roles are optional hardening, not a connection
requirement. Keep migration credentials able to create/alter the schema.

| Template | Database URL secret | Neon endpoint |
| --- | --- | --- |
| API | `stacktrace-api-database-url` | Pooled |
| Worker | `stacktrace-worker-database-url` | Pooled |
| Migrate | `stacktrace-migrate-database-url` | Direct (unpooled) |
| Admin | `stacktrace-admin-database-url` | Direct (unpooled) |

Example shapes only; replace the entire URL with the one from Neon:

```text
postgresql://USER:ENCODED_PASSWORD@NEON_POOLED_HOST/DATABASE?sslmode=require
postgresql://USER:ENCODED_PASSWORD@NEON_DIRECT_HOST/DATABASE?sslmode=require
```

Keep TLS enabled and retain the Neon-provided connection security options. Do not
use `sslmode=disable` or a local socket URL. If assembling a URL yourself,
percent-encode credentials. Cloud Run connects over outbound PostgreSQL/TLS;
the existing pgx adapter reads `DATABASE_URL` with no application code change.

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
process, including overlapping revisions and jobs) against your Neon plan's
connection limits.
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
