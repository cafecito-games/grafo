-- +goose Up
ALTER TABLE nodes ADD COLUMN name_folded TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN qualified_name_folded TEXT NOT NULL DEFAULT '';

-- Preserve the existing ASCII-insensitive behavior until the semantic-index
-- version rebuild rewrites every node with Go's Unicode-aware lowercase rule.
UPDATE nodes
SET name_folded = lower(name),
    qualified_name_folded = lower(qualified_name);

-- +goose Down
ALTER TABLE nodes DROP COLUMN qualified_name_folded;
ALTER TABLE nodes DROP COLUMN name_folded;
