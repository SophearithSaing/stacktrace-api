-- Policy semantics and AccountAgent ownership are validated at the trusted Go
-- boundary. SQL protects shape, references, durable identities and reservations.
CREATE TABLE agent_personas (
    agent_id uuid NOT NULL REFERENCES accounts (id),
    version integer NOT NULL CHECK (version > 0),
    instructions text NOT NULL CHECK (octet_length(instructions) BETWEEN 1 AND 16000 AND instructions !~ '^[[:space:]]*$'),
    topic_tags text[] NOT NULL CHECK (cardinality(topic_tags) BETWEEN 1 AND 20 AND array_position(topic_tags, NULL) IS NULL),
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    PRIMARY KEY (agent_id, version)
);

CREATE TABLE agent_settings (
    agent_id uuid PRIMARY KEY REFERENCES accounts (id),
    persona_version integer NOT NULL,
    enabled boolean NOT NULL,
    policy jsonb NOT NULL CHECK ((jsonb_typeof(policy) = 'object' AND policy -> 'version' = '1'::jsonb) IS TRUE),
    next_post_at timestamptz CHECK (isfinite(next_post_at)),
    schedule_date date CHECK (isfinite(schedule_date) AND schedule_date BETWEEN DATE '0001-01-01' AND DATE '9999-12-31'),
    remaining_slots integer NOT NULL CHECK (remaining_slots BETWEEN 0 AND 100),
    last_published_at timestamptz CHECK (isfinite(last_published_at)),
    updated_at timestamptz NOT NULL CHECK (isfinite(updated_at)),
    FOREIGN KEY (agent_id, persona_version) REFERENCES agent_personas (agent_id, version),
    CHECK (remaining_slots = 0 OR schedule_date IS NOT NULL),
    CHECK (next_post_at IS NULL OR (remaining_slots > 0 AND schedule_date IS NOT NULL))
);

CREATE INDEX agent_settings_enabled_schedule_idx ON agent_settings (next_post_at) WHERE enabled;

CREATE TABLE generation_jobs (
    id uuid PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    agent_id uuid NOT NULL,
    persona_version integer NOT NULL,
    trigger_kind text NOT NULL CHECK (trigger_kind IN ('scheduled', 'reply', 'repost', 'quote', 'human_post', 'continuation')),
    trigger_key text NOT NULL CHECK (octet_length(trigger_key) BETWEEN 1 AND 256 AND trigger_key !~ '^[[:space:]]*$'),
    trigger_actor_id uuid REFERENCES accounts (id),
    cooldown_key text CHECK (octet_length(cooldown_key) BETWEEN 1 AND 256 AND cooldown_key !~ '^[[:space:]]*$'),
    source_post_id uuid REFERENCES posts (id),
    source_reply_id uuid REFERENCES replies (id),
    source_repost_id uuid REFERENCES reposts (id) ON DELETE SET NULL,
    output_kind text NOT NULL CHECK (output_kind IN ('post', 'quote', 'reply')),
    root_job_id uuid NOT NULL REFERENCES generation_jobs (id),
    chain_depth integer NOT NULL CHECK (chain_depth BETWEEN 0 AND 10),
    status text NOT NULL CHECK (status IN ('pending', 'running', 'retry_wait', 'succeeded', 'skipped', 'cancelled', 'failed')),
    available_at timestamptz NOT NULL CHECK (isfinite(available_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at)),
    lease_version bigint NOT NULL CHECK (lease_version >= 0),
    lease_expires_at timestamptz CHECK (isfinite(lease_expires_at)),
    result_post_id uuid UNIQUE REFERENCES posts (id),
    result_reply_id uuid UNIQUE REFERENCES replies (id),
    published_attempt_id uuid UNIQUE,
    reason_code text CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    finished_at timestamptz CHECK (isfinite(finished_at)),
    UNIQUE (agent_id, trigger_key),
    FOREIGN KEY (agent_id, persona_version) REFERENCES agent_personas (agent_id, version),
    CHECK ((chain_depth = 0) = (root_job_id = id)),
    CHECK (
        (trigger_kind = 'scheduled' AND trigger_actor_id IS NULL AND cooldown_key IS NULL
            AND source_post_id IS NULL AND source_reply_id IS NULL AND source_repost_id IS NULL
            AND output_kind = 'post' AND chain_depth = 0)
        OR
        (trigger_kind <> 'scheduled' AND trigger_actor_id IS NOT NULL AND trigger_actor_id <> agent_id
            AND cooldown_key IS NOT NULL AND source_post_id IS NOT NULL AND output_kind IN ('quote', 'reply')
            AND (
                (trigger_kind = 'reply' AND source_reply_id IS NOT NULL AND source_repost_id IS NULL AND chain_depth = 0)
                OR (trigger_kind = 'repost' AND source_reply_id IS NULL AND chain_depth = 0)
                OR (trigger_kind IN ('quote', 'human_post') AND source_reply_id IS NULL AND source_repost_id IS NULL AND chain_depth = 0)
                OR (trigger_kind = 'continuation' AND source_repost_id IS NULL AND chain_depth > 0)
            ))
    ),
    CHECK (available_at >= created_at AND expires_at > available_at),
    CHECK (status <> 'pending' OR lease_version = 0),
    CHECK (status NOT IN ('running', 'retry_wait', 'succeeded') OR lease_version > 0),
    CHECK ((status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at > available_at)
        OR (status <> 'running' AND lease_expires_at IS NULL)),
    CHECK ((status IN ('succeeded', 'skipped', 'cancelled', 'failed') AND finished_at IS NOT NULL AND finished_at >= created_at)
        OR (status IN ('pending', 'running', 'retry_wait') AND finished_at IS NULL)),
    CHECK (
        (status = 'succeeded' AND published_attempt_id IS NOT NULL AND finished_at >= available_at AND finished_at < expires_at
            AND ((output_kind = 'reply' AND result_reply_id IS NOT NULL AND result_post_id IS NULL)
                OR (output_kind IN ('post', 'quote') AND result_post_id IS NOT NULL AND result_reply_id IS NULL)))
        OR (status <> 'succeeded' AND result_post_id IS NULL AND result_reply_id IS NULL AND published_attempt_id IS NULL)
    ),
    CHECK ((status IN ('skipped', 'cancelled', 'failed', 'retry_wait') AND reason_code IS NOT NULL)
        OR (status IN ('pending', 'running', 'succeeded') AND reason_code IS NULL))
);

CREATE INDEX generation_jobs_due_idx ON generation_jobs (available_at, id) WHERE status IN ('pending', 'retry_wait');
CREATE INDEX generation_jobs_leases_idx ON generation_jobs (lease_expires_at, id) WHERE status = 'running';
CREATE INDEX generation_jobs_chain_idx ON generation_jobs (root_job_id, status);
CREATE INDEX generation_jobs_actor_idx ON generation_jobs (trigger_actor_id, created_at) WHERE trigger_actor_id IS NOT NULL;
CREATE INDEX generation_jobs_cooldown_idx ON generation_jobs (cooldown_key, created_at) WHERE cooldown_key IS NOT NULL;

CREATE TABLE generation_attempts (
    id uuid PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    job_id uuid NOT NULL REFERENCES generation_jobs (id),
    attempt_number integer NOT NULL CHECK (attempt_number > 0),
    lease_version bigint NOT NULL CHECK (lease_version > 0),
    provider text NOT NULL CHECK (provider ~ '^[a-z0-9_]{1,64}$'),
    model text NOT NULL CHECK (octet_length(model) BETWEEN 1 AND 128 AND model !~ '^[[:space:]]*$'),
    provider_request_id text CHECK (octet_length(provider_request_id) BETWEEN 1 AND 256 AND provider_request_id !~ '^[[:space:]]*$'),
    context_hash text NOT NULL CHECK (context_hash ~ '^[0-9a-f]{64}$'),
    context_builder_version text NOT NULL CHECK (context_builder_version ~ '^[a-z0-9_]{1,64}$'),
    budget_day date NOT NULL CHECK (budget_day BETWEEN DATE '0001-01-01' AND DATE '9999-12-31'),
    reserved_tokens bigint NOT NULL CHECK (reserved_tokens > 0),
    input_tokens bigint,
    output_tokens bigint,
    status text NOT NULL CHECK (status IN ('reserved', 'succeeded', 'failed', 'unknown')),
    error_code text CHECK (error_code ~ '^[a-z0-9_]{1,64}$'),
    started_at timestamptz NOT NULL CHECK (isfinite(started_at)),
    finished_at timestamptz CHECK (isfinite(finished_at)),
    UNIQUE (job_id, attempt_number),
    UNIQUE (job_id, id),
    CHECK (budget_day = (started_at AT TIME ZONE 'UTC')::date),
    CHECK ((input_tokens IS NULL AND output_tokens IS NULL)
        OR (input_tokens IS NOT NULL AND output_tokens IS NOT NULL AND input_tokens >= 0 AND output_tokens >= 0
            AND input_tokens <= reserved_tokens AND output_tokens <= reserved_tokens - input_tokens)),
    CHECK ((status = 'reserved' AND finished_at IS NULL AND input_tokens IS NULL AND error_code IS NULL AND provider_request_id IS NULL)
        OR (status <> 'reserved' AND finished_at IS NOT NULL AND finished_at >= started_at)),
    CHECK ((status IN ('reserved', 'succeeded') AND error_code IS NULL)
        OR (status IN ('failed', 'unknown') AND error_code IS NOT NULL)),
    CHECK (status <> 'unknown' OR input_tokens IS NULL)
);

ALTER TABLE generation_jobs ADD CONSTRAINT generation_jobs_published_attempt_fk
    FOREIGN KEY (id, published_attempt_id) REFERENCES generation_attempts (job_id, id);

CREATE INDEX generation_attempts_budget_idx ON generation_attempts (budget_day, job_id);

CREATE FUNCTION retain_generation_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'generation records are retained' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER agent_personas_immutable BEFORE UPDATE OR DELETE ON agent_personas
    FOR EACH ROW EXECUTE FUNCTION retain_generation_record();
CREATE TRIGGER generation_jobs_retained BEFORE DELETE ON generation_jobs
    FOR EACH ROW EXECUTE FUNCTION retain_generation_record();
CREATE TRIGGER generation_attempts_retained BEFORE DELETE ON generation_attempts
    FOR EACH ROW EXECUTE FUNCTION retain_generation_record();

-- This is deliberately not a claim/publication state machine. Runtime writes
-- must lock the before-row and validate domain transitions, fences, policy,
-- sources, budgets and the exact successful attempt/lease before publishing.
CREATE FUNCTION protect_generation_job() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.root_job_id <> NEW.id AND NOT EXISTS (
            SELECT 1 FROM generation_jobs WHERE id = NEW.root_job_id AND chain_depth = 0
        ) THEN
            RAISE EXCEPTION 'chain must reference a root job' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.id, NEW.agent_id, NEW.persona_version, NEW.trigger_kind, NEW.trigger_key,
        NEW.trigger_actor_id, NEW.cooldown_key, NEW.source_post_id, NEW.source_reply_id,
        NEW.output_kind, NEW.root_job_id, NEW.chain_depth, NEW.expires_at, NEW.created_at)
        IS DISTINCT FROM ROW(OLD.id, OLD.agent_id, OLD.persona_version, OLD.trigger_kind, OLD.trigger_key,
        OLD.trigger_actor_id, OLD.cooldown_key, OLD.source_post_id, OLD.source_reply_id,
        OLD.output_kind, OLD.root_job_id, OLD.chain_depth, OLD.expires_at, OLD.created_at) THEN
        RAISE EXCEPTION 'job identity and expiry are immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.source_repost_id IS DISTINCT FROM OLD.source_repost_id THEN
        -- Only the FK action after actual removal may clear this reference.
        -- In particular, neither a direct clear of a live source nor replacement
        -- with another repost can erase/change attribution or terminal provenance.
        IF OLD.source_repost_id IS NULL OR NEW.source_repost_id IS NOT NULL
            OR EXISTS (SELECT 1 FROM reposts WHERE id = OLD.source_repost_id)
            OR (to_jsonb(NEW) - 'source_repost_id') IS DISTINCT FROM (to_jsonb(OLD) - 'source_repost_id') THEN
            RAISE EXCEPTION 'repost source is immutable until deletion' USING ERRCODE = '23514';
        END IF;
    END IF;
    IF OLD.status IN ('succeeded', 'skipped', 'cancelled')
        AND (to_jsonb(NEW) - 'source_repost_id') IS DISTINCT FROM (to_jsonb(OLD) - 'source_repost_id') THEN
        RAISE EXCEPTION 'completed job is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER generation_jobs_identity BEFORE INSERT OR UPDATE ON generation_jobs
    FOR EACH ROW EXECUTE FUNCTION protect_generation_job();

CREATE FUNCTION protect_generation_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'reserved' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'attempt outcome is immutable' USING ERRCODE = '23514';
    END IF;
    IF ROW(NEW.id, NEW.job_id, NEW.attempt_number, NEW.lease_version, NEW.provider, NEW.model,
        NEW.context_hash, NEW.context_builder_version, NEW.budget_day, NEW.reserved_tokens, NEW.started_at)
        IS DISTINCT FROM ROW(OLD.id, OLD.job_id, OLD.attempt_number, OLD.lease_version, OLD.provider, OLD.model,
        OLD.context_hash, OLD.context_builder_version, OLD.budget_day, OLD.reserved_tokens, OLD.started_at) THEN
        RAISE EXCEPTION 'attempt identity and reservation are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER generation_attempts_identity BEFORE UPDATE ON generation_attempts
    FOR EACH ROW EXECUTE FUNCTION protect_generation_attempt();
