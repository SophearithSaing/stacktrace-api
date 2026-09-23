-- Legacy succeeded attempts keep NULL: the original validated bytes are not
-- available, so no digest may be invented. New publication fails closed on NULL.
ALTER TABLE generation_attempts ADD COLUMN output_digest text;

ALTER TABLE generation_attempts ADD CONSTRAINT generation_attempts_output_digest_check
    CHECK (output_digest IS NULL OR
        (status = 'succeeded' AND output_digest ~ '^generation_output_v1:[0-9a-f]{64}$'));

-- Existing protect_generation_attempt compares the entire completed row, so the
-- new digest is immutable as part of a terminal outcome, including legacy NULL.
-- No execution/context indexes until the actual bounded queries are reviewed.
