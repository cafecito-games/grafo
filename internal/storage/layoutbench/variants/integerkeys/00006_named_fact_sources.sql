-- +goose Up
ALTER TABLE facts ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE facts ADD COLUMN source_kind TEXT NOT NULL DEFAULT '';

CREATE INDEX facts_from_id ON facts(from_key);
CREATE INDEX facts_source ON facts(source, source_kind);

-- +goose Down
DROP INDEX IF EXISTS facts_source;
DROP INDEX IF EXISTS facts_from_id;
ALTER TABLE facts DROP COLUMN source_kind;
ALTER TABLE facts DROP COLUMN source;
