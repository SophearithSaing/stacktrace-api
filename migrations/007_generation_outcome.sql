-- No backfill: legacy successes do not establish a publish/skip decision.
-- The terminal-row immutability trigger also protects these new observations.
ALTER TABLE generation_attempts
    ADD COLUMN decision text,
    ADD COLUMN skip_reason text,
    ADD COLUMN not_before timestamptz;

ALTER TABLE generation_attempts ADD CONSTRAINT generation_attempts_decision_check
    CHECK (decision IS NULL OR (status = 'succeeded' AND output_digest IS NOT NULL
        AND decision IN ('publish', 'skip'))),
    ADD CONSTRAINT generation_attempts_skip_reason_check
    CHECK ((decision IS NOT DISTINCT FROM 'skip' AND skip_reason IS NOT NULL
        AND skip_reason IN ('not_relevant', 'insufficient_context', 'unsafe_request', 'repetition'))
        OR (decision IS DISTINCT FROM 'skip' AND skip_reason IS NULL)),
    ADD CONSTRAINT generation_attempts_not_before_check
    CHECK (not_before IS NULL OR (status IN ('failed', 'unknown')
        AND not_before > '0001-01-01 00:00:00+00'::timestamptz
        AND not_before < '10000-01-01 00:00:00+00'::timestamptz));
