CREATE INDEX posts_visible_body_search_idx ON posts USING GIN (to_tsvector('simple', body)) WHERE deleted_at IS NULL;
