-- +goose Up
ALTER TABLE facts ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE facts ADD COLUMN source_kind TEXT NOT NULL DEFAULT '';

-- Composite with the textual id so the heal trigger's from arm and the
-- enqueue OR-arms seek instead of walking the whole 0-key range that
-- not-yet-resolved facts permanently occupy.
CREATE INDEX facts_from_id ON facts(from_key, from_id);
CREATE INDEX facts_source ON facts(source, source_kind);

-- +goose Down
DROP INDEX IF EXISTS facts_source;
DROP INDEX IF EXISTS facts_from_id;
ALTER TABLE facts DROP COLUMN source_kind;
ALTER TABLE facts DROP COLUMN source;
