CREATE TABLE posts (
    id uuid PRIMARY KEY,
    author_id uuid NOT NULL REFERENCES accounts (id),
    body text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 320 AND body !~ '^[[:space:]]*$'),
    quoted_post_id uuid REFERENCES posts (id),
    code_language text,
    code_filename text,
    code_source text,
    is_spicy boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    deleted_at timestamptz,
    CHECK (
        (code_language IS NULL AND code_filename IS NULL AND code_source IS NULL)
        OR (
            code_language IS NOT NULL
            AND code_filename IS NOT NULL
            AND code_source IS NOT NULL
            AND code_language ~ '^[A-Za-z0-9_+.-]{1,32}$'
            AND char_length(code_filename) BETWEEN 1 AND 255
            AND octet_length(code_source) BETWEEN 1 AND 20480
            AND code_source !~ '^[[:space:]]*$'
        )
    )
);

CREATE INDEX posts_visible_newest_idx ON posts (created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX posts_visible_author_newest_idx ON posts (author_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX posts_visible_spicy_newest_idx ON posts (created_at DESC, id DESC) WHERE deleted_at IS NULL AND is_spicy;
CREATE INDEX posts_quoted_post_id_idx ON posts (quoted_post_id) WHERE quoted_post_id IS NOT NULL;

CREATE TABLE replies (
    id uuid PRIMARY KEY,
    post_id uuid NOT NULL REFERENCES posts (id),
    author_id uuid NOT NULL REFERENCES accounts (id),
    body text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 320 AND body !~ '^[[:space:]]*$'),
    created_at timestamptz NOT NULL,
    deleted_at timestamptz
);

CREATE INDEX replies_visible_post_created_id_idx ON replies (post_id, created_at, id) WHERE deleted_at IS NULL;

CREATE TABLE reactions (
    account_id uuid NOT NULL REFERENCES accounts (id),
    post_id uuid NOT NULL REFERENCES posts (id),
    kind text NOT NULL CHECK (kind IN ('useful', 'agree', 'brilliant', 'spicy', 'ship')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, post_id)
);

CREATE INDEX reactions_post_kind_idx ON reactions (post_id, kind);

CREATE TABLE reposts (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id),
    post_id uuid NOT NULL REFERENCES posts (id),
    created_at timestamptz NOT NULL,
    UNIQUE (account_id, post_id)
);

CREATE INDEX reposts_newest_idx ON reposts (created_at DESC, id DESC);
CREATE INDEX reposts_account_newest_idx ON reposts (account_id, created_at DESC, id DESC);
CREATE INDEX reposts_post_id_idx ON reposts (post_id);

CREATE TABLE bookmarks (
    account_id uuid NOT NULL REFERENCES accounts (id),
    post_id uuid NOT NULL REFERENCES posts (id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, post_id)
);

CREATE INDEX bookmarks_account_newest_idx ON bookmarks (account_id, created_at DESC, post_id DESC);

CREATE TABLE tags (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9_]{1,64}$'),
    display_name text NOT NULL
);

CREATE TABLE post_tags (
    post_id uuid NOT NULL REFERENCES posts (id),
    tag_id uuid NOT NULL REFERENCES tags (id),
    PRIMARY KEY (post_id, tag_id)
);

CREATE INDEX post_tags_tag_id_post_id_idx ON post_tags (tag_id, post_id);

CREATE TABLE idempotency_keys (
    account_id uuid NOT NULL REFERENCES accounts (id),
    key text NOT NULL CHECK (key ~ '^[A-Za-z0-9._:-]{1,128}$'),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    post_id uuid,
    reply_id uuid,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (account_id, key),
    CHECK ((post_id IS NULL) <> (reply_id IS NULL)),
    FOREIGN KEY (post_id) REFERENCES posts (id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (reply_id) REFERENCES replies (id) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
