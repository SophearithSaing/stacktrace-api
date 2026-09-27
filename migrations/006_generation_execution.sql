-- Recent public agent replies have no author/time access path in 002. Bound
-- context history by this index rather than sorting all conversation replies.
CREATE INDEX replies_visible_author_newest_idx ON replies (author_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

-- These remaining indexes establish access paths for subsequent execution work;
-- they do not implement recovery, eligibility or budget behavior.
-- Recovery scans only outstanding reservations, not retained terminal history.
CREATE INDEX generation_attempts_reserved_idx ON generation_attempts (started_at, id)
    WHERE status = 'reserved';

-- Existing uniqueness leads with agent_id, not the shared action. Execution
-- revalidates stricter action caps across agents in deterministic reservation order.
CREATE INDEX generation_jobs_action_reservation_idx ON generation_jobs (trigger_key, created_at, id);
