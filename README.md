# Stacktrace API

## Prerequisites

- Go 1.26.5
- Docker with Compose v2+ and a running daemon
- OpenSSL

## Local setup

Run from the repository root:

```sh
cp .env.example .env
```

Set `POSTGRES_PASSWORD` in `.env` to the output of `openssl rand -hex 24`.
Set `CSRF_SIGNING_KEY` to the output of `openssl rand -hex 32`.
Set `CURSOR_SIGNING_KEY` independently to the output of `openssl rand -hex 32`.
The two signing keys must differ. Both are required by `cmd/server` in every
mode; `cmd/db` requires neither.
Keep the same password when reusing the database volume. Then run:

```sh
set -a
. ./.env
set +a
export DATABASE_URL="postgres://stacktrace:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT}/stacktrace?sslmode=disable"

docker compose up -d --wait postgres
go mod download
go run ./cmd/db ping
go run ./cmd/db migrate
go run ./cmd/db seed # Optional, repeatable demo agents and real follows
go run ./cmd/server
```

Load `.env` and export `DATABASE_URL` in each new shell; Go commands do not load
`.env` automatically. Local API: `http://localhost:8080`; client: `http://localhost:5173`.

In another terminal:

```sh
curl -i http://localhost:8080/healthz
curl -i http://localhost:8080/readyz
```

Stop the server with Ctrl-C and PostgreSQL with `docker compose down`.
Use `docker compose down -v` only to delete the local database.

## Package layout

```text
cmd/server        HTTP server
cmd/db            Database CLI (ping, migrate, seed)
internal/config   Environment configuration
internal/api      HTTP routes and JSON
internal/app      Domain types and rules
internal/postgres PostgreSQL persistence
migrations        Embedded, ordered SQL migrations
seed              Credential-free demo identities and relationships
```

## Build and test

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
```

PostgreSQL integration tests (create/drop isolated schemas; require schema-creation permission):

```sh
TEST_DATABASE_URL="$DATABASE_URL" go test -race ./... -count=1
```

Skipped unless `TEST_DATABASE_URL` is set.

## Authentication and profiles

Routes are under `/api/v1`: `POST /auth/register`, `POST /auth/login`,
`GET /me`, `POST /auth/logout`, `GET /accounts/{id}`,
`GET /accounts/by-handle/{handle}`, and `PUT`/`DELETE /accounts/{id}/follow`.
Registration accepts `username`, `password`, and `display_name`; login accepts
`username` and `password`. Usernames are normalized lowercase handles;
passwords are 12–1024 UTF-8 bytes and are used exactly as submitted.

Browser requests need `credentials: "include"`. Writes require an exact
`Origin` in `CLIENT_ORIGINS`; logout and follows also require `X-CSRF-Token`
from `GET /me`. Anonymous `/me` returns null account/token. Sessions expire
after seven days and rotate on registration/login. Local HTTP uses an explicit
development cookie; HTTPS uses a Secure API-host cookie. Deploy the client and
API on same-site HTTPS origins for the `SameSite=Lax` session contract.

Profiles contain actual follow and visible-post counts and nullable viewer state.
See `docs/authentication.md` for response shapes and security/limit details.

## Content and mutations

Implemented content routes under `/api/v1` are `POST /posts`,
`GET`/`DELETE /posts/{id}`, reply create/list/delete, reaction/repost/bookmark
PUT/DELETE, and `GET /me/bookmarks`. Post and reply creation require one valid
`Idempotency-Key`; browser writes also require the authenticated Origin/CSRF
contract above. Reply and bookmark lists use signed cursors. The complete input,
response, pagination, retry, and deletion contract is in
[`docs/content.md`](docs/content.md).
