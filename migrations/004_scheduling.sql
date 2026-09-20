-- Every job carries the root's immutable policy limits. New callers must supply
-- them explicitly: there is no permissive default or dependency on live policy.
ALTER TABLE generation_jobs
    ADD COLUMN max_chain_depth integer,
    ADD COLUMN max_chain_jobs integer;

-- Legacy chains have no trustworthy historical policy snapshot. Freeze their
-- allowance at the observed depth/count (root-only => 0/1), not today's policy.
-- The migration's transaction and ALTER TABLE lock protect this one-time update,
-- including terminal rows; restore the original provenance trigger immediately.
-- Historically malformed/over-ceiling chains fail the checks below and require
-- explicit operator review rather than silently expanding/truncating history.
ALTER TABLE generation_jobs DISABLE TRIGGER generation_jobs_identity;
WITH chain_limits AS (
    SELECT root_job_id, max(chain_depth) AS depth, count(*) AS jobs
    FROM generation_jobs GROUP BY root_job_id
)
UPDATE generation_jobs AS job
SET max_chain_depth = limits.depth, max_chain_jobs = limits.jobs
FROM chain_limits AS limits WHERE job.root_job_id = limits.root_job_id;
ALTER TABLE generation_jobs ENABLE TRIGGER generation_jobs_identity;

ALTER TABLE generation_jobs
    ALTER COLUMN max_chain_depth SET NOT NULL,
    ALTER COLUMN max_chain_jobs SET NOT NULL,
    ADD CONSTRAINT generation_jobs_chain_limits_check CHECK (
        max_chain_depth BETWEEN 0 AND 10 AND max_chain_jobs BETWEEN 1 AND 100
        AND max_chain_depth < max_chain_jobs AND chain_depth <= max_chain_depth
    );

-- Keep the existing identity/retention/repost-deletion behavior unchanged.
-- Equality is safe without a root write lock because the root limits themselves
-- cannot change. Runtime capacity admission still needs a root-row lock.
CREATE FUNCTION protect_generation_chain_limits() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF ROW(NEW.max_chain_depth, NEW.max_chain_jobs)
            IS DISTINCT FROM ROW(OLD.max_chain_depth, OLD.max_chain_jobs) THEN
            RAISE EXCEPTION 'chain limits are immutable' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.root_job_id <> NEW.id AND NOT EXISTS (
        SELECT 1 FROM generation_jobs AS root
        WHERE root.id = NEW.root_job_id AND root.chain_depth = 0
            AND root.max_chain_depth = NEW.max_chain_depth
            AND root.max_chain_jobs = NEW.max_chain_jobs
    ) THEN
        RAISE EXCEPTION 'chain limits must match root' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER generation_jobs_chain_limits BEFORE INSERT OR UPDATE ON generation_jobs
    FOR EACH ROW EXECUTE FUNCTION protect_generation_chain_limits();

-- Reservations count in every status; cancellation must not refund quotas.
-- UTC range predicates on created_at support daily admission without date casts.
CREATE INDEX generation_jobs_agent_day_idx ON generation_jobs (agent_id, output_kind, created_at);
CREATE INDEX generation_jobs_agent_conversation_idx ON generation_jobs (agent_id, source_post_id)
    WHERE output_kind = 'reply';

-- Bounded stale/source cleanup; running jobs still require lease-aware fencing.
CREATE INDEX generation_jobs_expiry_idx ON generation_jobs (expires_at, id)
    WHERE status IN ('pending', 'retry_wait', 'running');
CREATE INDEX generation_jobs_source_post_idx ON generation_jobs (source_post_id, id)
    WHERE source_post_id IS NOT NULL AND status IN ('pending', 'retry_wait', 'running');
CREATE INDEX generation_jobs_source_reply_idx ON generation_jobs (source_reply_id, id)
    WHERE source_reply_id IS NOT NULL AND status IN ('pending', 'retry_wait', 'running');
-- Include terminal rows here: the repost FK's SET NULL must find all references.
CREATE INDEX generation_jobs_source_repost_idx ON generation_jobs (source_repost_id)
    WHERE source_repost_id IS NOT NULL;
