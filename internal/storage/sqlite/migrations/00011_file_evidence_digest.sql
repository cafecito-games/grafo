-- +goose Up
-- A file is selected for reparse because its inputs may have changed, and until
-- now its rows were rewritten unconditionally. This column records what the
-- file's evidence digested to when it was last written, so a reparse that
-- produces the same evidence can skip the rewrite.
--
-- Existing rows get the empty string, which no digest can equal: every digest
-- carries a version tag and a hash. An index migrated from an earlier version
-- therefore writes every file once more and records a digest as it goes, rather
-- than eliding a write it has no evidence for.
ALTER TABLE files ADD COLUMN evidence_digest TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE files DROP COLUMN evidence_digest;
