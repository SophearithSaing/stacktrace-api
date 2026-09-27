-- Settings revisions fence publication across pause followed by immediate resume.
ALTER TABLE agent_settings ADD COLUMN pause_revision bigint NOT NULL DEFAULT 0
    CHECK (pause_revision >= 0);

-- Do not backfill authority for old calls. Historical observations/provenance
-- remain readable, and outstanding legacy calls may settle but cannot publish.
ALTER TABLE generation_attempts ADD COLUMN pause_revision bigint
    CHECK (pause_revision >= 0);

CREATE FUNCTION protect_generation_pause_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.pause_revision IS DISTINCT FROM OLD.pause_revision THEN
        RAISE EXCEPTION 'attempt pause revision is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER generation_attempt_pause_revision_immutable
    BEFORE UPDATE ON generation_attempts
    FOR EACH ROW EXECUTE FUNCTION protect_generation_pause_revision();
