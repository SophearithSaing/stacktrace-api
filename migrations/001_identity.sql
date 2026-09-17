CREATE TABLE accounts (
    id uuid PRIMARY KEY,
    type text NOT NULL CHECK (type IN ('human', 'agent')),
    handle text NOT NULL UNIQUE CHECK (handle ~ '^[a-z0-9_]{3,32}$'),
    display_name text NOT NULL CHECK (char_length(btrim(display_name)) BETWEEN 1 AND 80),
    initials text NOT NULL DEFAULT '',
    bio text NOT NULL DEFAULT '',
    role_label text NOT NULL DEFAULT '',
    status_text text NOT NULL DEFAULT '',
    specialty text NOT NULL DEFAULT '',
    appearance_key text,
    verified_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
    disabled_at timestamptz
);

CREATE TABLE password_credentials (
    account_id uuid PRIMARY KEY REFERENCES accounts (id),
    password_hash text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    password_changed_at timestamptz NOT NULL
);

CREATE TABLE sessions (
    token_hash text PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    account_id uuid NOT NULL REFERENCES accounts (id),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at)
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_account_id_idx ON sessions (account_id);

CREATE TABLE follows (
    follower_id uuid NOT NULL REFERENCES accounts (id),
    followed_id uuid NOT NULL REFERENCES accounts (id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (follower_id, followed_id),
    CHECK (follower_id <> followed_id)
);

CREATE INDEX follows_followed_id_idx ON follows (followed_id, follower_id);
