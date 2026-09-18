# Project Guide

## Priorities

Write clean, simple, explicit, easy-to-read, idiomatic Go. Prefer the smallest
implementation that satisfies the current requirement. Avoid speculative
abstractions and infrastructure for features that have not been requested.

## Implementation style

- Use standard-library APIs directly. Prefer `http.Server.ListenAndServe` and
  `Shutdown` over custom listener/lifecycle plumbing unless there is a concrete need.
- Register routes explicitly with method-aware `http.ServeMux` patterns. Do not
  build another routing framework around it.
- Use descriptive local names where they improve readability: `origin` rather
  than `u`, `portNumber` rather than `p`. Conventional Go names such as `ctx`,
  `err`, `r`, `w`, and short receiver names are fine.
- Prefer straightforward control flow, early returns, and small focused functions.
  A couple of explicit statements can be clearer than a generic helper or loop.
- Introduce interfaces only at real boundaries. The small shared SQL interface
  for `*sql.DB` and `*sql.Tx` is useful; generic repositories, forwarding service
  layers, dependency-injection containers, and broad options structs are not.
- Preserve actual correctness requirements when simplifying: validation, bounded
  work, transaction isolation, migration integrity, authorization, and safe errors.

## Configuration

- Environment variables are for deployment-specific values: URLs, listener
  addresses, origins, deployment mode, and credentials/keys.
- Keep pool sizes, timeouts, body/header limits, rate policy, and logging defaults
  as literals or named constants near the code that uses them.
- Do not add configuration structs or pass fixed defaults through multiple layers.
  Constructors should take the dependencies they actually need.
- `.env.example` documents setup; application commands read the process environment.
  Never include real credentials or log connection strings, cookies, or passwords.

## Project boundaries

- This is a backend-only Go module. The frontend is a separate project.
- Use `net/http`, `database/sql`, PostgreSQL through `pgx`, and handwritten SQL.
- Keep HTTP/JSON in `internal/api`, domain types/rules in `internal/app`, and SQL
  in `internal/postgres`. Domain code must not depend on HTTP or database drivers.
- `cmd/server` runs the API. `cmd/db` performs explicit database commands.
  Neither command hosts PostgreSQL; both connect through `DATABASE_URL`.
- Run migrations explicitly with `cmd/db migrate`, never on server startup.
- Database-dependent tests use real PostgreSQL with isolated test schemas.
  Preserve the existing `AccountType` naming and repository conventions.

## Documentation and verification

- Keep README concise: setup, start/stop commands, package layout, and tests.
  Put architecture explanations and review guides in `docs/`.
- `docs/` is intentionally Git-ignored in this workspace. Do not change that
  policy or force-add those files without a request.
- The architecture plan and implementation checklist live in `docs/`. Follow the
  requested implementation slice and mark work complete only after verification.
- Keep tests alongside the behavior they verify. Test meaningful contracts and
  failure/concurrency cases; avoid tests that merely repeat implementation details.
- Run `make check` directly for full verification. It runs build, vet, normal
  tests, and race tests with disposable real PostgreSQL and caching disabled.
- Use `make test TEST_ARGS='-run TestName -v'` for focused verification. Tests
  create isolated schemas inside the runner's disposable database.
- Run `make smoke` for live-server checks, in addition to `make check` when
  changing startup, configuration, migrations/seeding, or HTTP/session behavior.
- Use `make dev-up`, `make dev-status`, and `make dev-down` for interactive API
  work. Development data survives shutdown; each checkout owns one instance.
- Use the lifecycle commands and respect ownership. Concurrent agents should
  normally use isolated test/smoke runs. Do not stop another run's processes.
- Treat dependency startup or cleanup failures as blockers. Report the printed
  diagnostics path; do not substitute Go tests that skip PostgreSQL coverage.
- Runner changes also require `python3 scripts/test_runner.py` for lifecycle,
  failure/interruption, and concurrency contracts. See
  `docs/development-testing.md` for recovery and implementation details.
- Use existing OpenCode permissions for the documented commands. Project-specific
  allowances are deferred until needed. Custom agents use their own effective
  permissions; verify execution when changing policies and preserve unrelated
  configuration.
- Ordinary tests and smoke checks must not make paid provider calls.
