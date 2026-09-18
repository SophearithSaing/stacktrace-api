package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	contentAccountID = "10000000-0000-0000-0000-000000000001"
	contentPostID    = "20000000-0000-0000-0000-000000000001"
	contentReplyID   = "30000000-0000-0000-0000-000000000001"
)

func TestContentSchemaConstraints(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO accounts (id, type, handle, display_name, created_at, updated_at)
		VALUES ($1, 'human', 'content_owner', 'Content owner', now(), now())`, contentAccountID); err != nil {
		t.Fatal(err)
	}

	reject := func(wantCode, query string, arguments ...any) {
		t.Helper()
		_, err := store.db.ExecContext(ctx, query, arguments...)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != wantCode {
			t.Fatalf("query error code = %v, want %s: %s", err, wantCode, query)
		}
	}
	post := func(id, body string) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, `INSERT INTO posts (id, author_id, body, created_at) VALUES ($1, $2, $3, now())`, id, contentAccountID, body); err != nil {
			t.Fatal(err)
		}
	}

	post(contentPostID, strings.Repeat("😀", 320))
	reject("23514", `INSERT INTO posts (id, author_id, body, created_at) VALUES ('20000000-0000-0000-0000-000000000002', $1, $2, now())`, contentAccountID, strings.Repeat("😀", 321))
	reject("23514", `INSERT INTO posts (id, author_id, body, created_at) VALUES ('20000000-0000-0000-0000-000000000003', $1, E'\t\n', now())`, contentAccountID)
	reject("23503", `INSERT INTO posts (id, author_id, body, quoted_post_id, created_at) VALUES ('20000000-0000-0000-0000-000000000009', $1, 'body', '20000000-0000-0000-0000-000000000099', now())`, contentAccountID)
	reject("23514", `INSERT INTO posts (id, author_id, body, code_language, created_at) VALUES ('20000000-0000-0000-0000-000000000004', $1, 'body', 'go', now())`, contentAccountID)
	reject("23514", `INSERT INTO posts (id, author_id, body, code_language, code_filename, code_source, created_at) VALUES ('20000000-0000-0000-0000-000000000005', $1, 'body', 'bad language!', 'x.go', 'package x', now())`, contentAccountID)
	reject("23514", `INSERT INTO posts (id, author_id, body, code_language, code_filename, code_source, created_at) VALUES ('20000000-0000-0000-0000-000000000015', $1, 'body', $2, 'x.go', 'package x', now())`, contentAccountID, strings.Repeat("x", 33))
	reject("23514", `INSERT INTO posts (id, author_id, body, code_language, code_filename, code_source, created_at) VALUES ('20000000-0000-0000-0000-000000000006', $1, 'body', 'go', '', 'package x', now())`, contentAccountID)
	reject("23514", `INSERT INTO posts (id, author_id, body, code_language, code_filename, code_source, created_at) VALUES ('20000000-0000-0000-0000-000000000016', $1, 'body', 'go', $2, 'package x', now())`, contentAccountID, strings.Repeat("x", 256))
	reject("23514", `INSERT INTO posts (id, author_id, body, code_language, code_filename, code_source, created_at) VALUES ('20000000-0000-0000-0000-000000000007', $1, 'body', 'go', 'x.go', $2, now())`, contentAccountID, strings.Repeat("x", 20481))
	reject("23514", `INSERT INTO posts (id, author_id, body, code_language, code_filename, code_source, created_at) VALUES ('20000000-0000-0000-0000-000000000008', $1, 'body', 'go', 'x.go', E' \n', now())`, contentAccountID)
	codeSource := strings.Repeat("x", 20480)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO posts (id, author_id, body, code_language, code_filename, code_source, created_at) VALUES ('20000000-0000-0000-0000-000000000017', $1, 'body', 'go', 'x.go', $2, now())`, contentAccountID, codeSource); err != nil {
		t.Fatal(err)
	}
	var savedSource string
	if err := store.db.QueryRowContext(ctx, `SELECT code_source FROM posts WHERE id = '20000000-0000-0000-0000-000000000017'`).Scan(&savedSource); err != nil || savedSource != codeSource {
		t.Fatalf("valid code was not preserved: error=%v", err)
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO replies (id, post_id, author_id, body, created_at) VALUES ($1, $2, $3, 'reply', now())`, contentReplyID, contentPostID, contentAccountID); err != nil {
		t.Fatal(err)
	}
	reject("23514", `INSERT INTO replies (id, post_id, author_id, body, created_at) VALUES ('30000000-0000-0000-0000-000000000002', $1, $2, ' ', now())`, contentPostID, contentAccountID)
	reject("23503", `INSERT INTO replies (id, post_id, author_id, body, created_at) VALUES ('30000000-0000-0000-0000-000000000003', '20000000-0000-0000-0000-000000000099', $1, 'reply', now())`, contentAccountID)
	reject("23514", `INSERT INTO reactions (account_id, post_id, kind, created_at, updated_at) VALUES ($1, $2, 'nope', now(), now())`, contentAccountID, contentPostID)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO reactions (account_id, post_id, kind, created_at, updated_at) VALUES ($1, $2, 'useful', now(), now())`, contentAccountID, contentPostID); err != nil {
		t.Fatal(err)
	}
	reject("23505", `INSERT INTO reactions (account_id, post_id, kind, created_at, updated_at) VALUES ($1, $2, 'ship', now(), now())`, contentAccountID, contentPostID)

	if _, err := store.db.ExecContext(ctx, `INSERT INTO reposts (id, account_id, post_id, created_at) VALUES ('40000000-0000-0000-0000-000000000001', $1, $2, now())`, contentAccountID, contentPostID); err != nil {
		t.Fatal(err)
	}
	reject("23505", `INSERT INTO reposts (id, account_id, post_id, created_at) VALUES ('40000000-0000-0000-0000-000000000002', $1, $2, now())`, contentAccountID, contentPostID)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO bookmarks (account_id, post_id, created_at) VALUES ($1, $2, now())`, contentAccountID, contentPostID); err != nil {
		t.Fatal(err)
	}
	reject("23505", `INSERT INTO bookmarks (account_id, post_id, created_at) VALUES ($1, $2, now())`, contentAccountID, contentPostID)

	if _, err := store.db.ExecContext(ctx, `INSERT INTO tags (id, slug, display_name) VALUES ('50000000-0000-0000-0000-000000000001', 'go_lang', 'Go Lang')`); err != nil {
		t.Fatal(err)
	}
	reject("23514", `INSERT INTO tags (id, slug, display_name) VALUES ('50000000-0000-0000-0000-000000000002', 'Go', 'Go')`)
	reject("23505", `INSERT INTO tags (id, slug, display_name) VALUES ('50000000-0000-0000-0000-000000000003', 'go_lang', 'Other')`)
	reject("23514", `INSERT INTO tags (id, slug, display_name) VALUES ('50000000-0000-0000-0000-000000000004', $1, 'Long')`, strings.Repeat("a", 65))
	if _, err := store.db.ExecContext(ctx, `INSERT INTO post_tags (post_id, tag_id) VALUES ($1, '50000000-0000-0000-0000-000000000001')`, contentPostID); err != nil {
		t.Fatal(err)
	}
	reject("23505", `INSERT INTO post_tags (post_id, tag_id) VALUES ($1, '50000000-0000-0000-0000-000000000001')`, contentPostID)
}

func TestIdempotencyKeysDeferredResults(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO accounts (id, type, handle, display_name, created_at, updated_at)
		VALUES ($1, 'human', 'idempotency_owner', 'Idempotency owner', now(), now())`, contentAccountID); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO posts (id, author_id, body, created_at) VALUES ($1, $2, 'parent post', now())`, contentPostID, contentAccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO replies (id, post_id, author_id, body, created_at) VALUES ($1, $2, $3, 'existing reply', now())`, contentReplyID, contentPostID, contentAccountID); err != nil {
		t.Fatal(err)
	}

	reject := func(wantCode, query string, arguments ...any) {
		t.Helper()
		_, err := store.db.ExecContext(ctx, query, arguments...)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != wantCode {
			t.Fatalf("query error code = %v, want %s: %s", err, wantCode, query)
		}
	}
	reject("23514", `INSERT INTO idempotency_keys (account_id, key, request_hash, created_at, expires_at) VALUES ($1, 'missing-result', $2, now(), now() + interval '1 day')`, contentAccountID, hash)
	reject("23514", `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, reply_id, created_at, expires_at) VALUES ($1, 'both-results', $2, $3, $4, now(), now() + interval '1 day')`, contentAccountID, hash, contentPostID, contentReplyID)
	reject("23514", `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, 'valid-hash-key', 'ABC', $2, now(), now() + interval '1 day')`, contentAccountID, contentPostID)
	reject("23514", `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, 'invalid key', $2, $3, now(), now() + interval '1 day')`, contentAccountID, hash, contentPostID)
	reject("23514", `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, 'expired-key', $2, $3, now(), now())`, contentAccountID, hash, contentPostID)
	key128 := strings.Repeat("k", 128)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, $2, $3, $4, now(), now() + interval '1 day')`, contentAccountID, key128, hash, contentPostID); err != nil {
		t.Fatal(err)
	}
	reject("23514", `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, $2, $3, $4, now(), now() + interval '1 day')`, contentAccountID, strings.Repeat("k", 129), hash, contentPostID)

	reservedPostID := "20000000-0000-0000-0000-000000000010"
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, 'reserve-post', $2, $3, now(), now() + interval '1 day')`, contentAccountID, hash, reservedPostID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO posts (id, author_id, body, created_at) VALUES ($1, $2, 'created after reservation', now())`, reservedPostID, contentAccountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("deferred result reservation did not commit: %v", err)
	}
	reject("23505", `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, 'reserve-post', $2, $3, now(), now() + interval '1 day')`, contentAccountID, hash, reservedPostID)

	reservedReplyID := "30000000-0000-0000-0000-000000000010"
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (account_id, key, request_hash, reply_id, created_at, expires_at) VALUES ($1, 'reserve-reply', $2, $3, now(), now() + interval '1 day')`, contentAccountID, hash, reservedReplyID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO replies (id, post_id, author_id, body, created_at) VALUES ($1, $2, $3, 'created after reservation', now())`, reservedReplyID, contentPostID, contentAccountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("deferred reply reservation did not commit: %v", err)
	}

	missingPostID := "20000000-0000-0000-0000-000000000011"
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (account_id, key, request_hash, post_id, created_at, expires_at) VALUES ($1, 'missing-result', $2, $3, now(), now() + interval '1 day')`, contentAccountID, hash, missingPostID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("missing deferred result committed")
	} else {
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23503" {
			t.Fatalf("missing result commit error code = %v, want 23503", err)
		}
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = 'missing-result'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed deferred commit retained key: count=%d error=%v", count, err)
	}
}
