-- +goose Up
CREATE TABLE dirty_owners (
    owner_file TEXT PRIMARY KEY
);

CREATE TABLE dirty_nodes (
    node_id TEXT PRIMARY KEY
);

CREATE TABLE dirty_targets (
    target TEXT NOT NULL,
    target_kind TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (target, target_kind)
);

CREATE INDEX facts_target ON facts(target, target_kind);
CREATE INDEX facts_target_id ON facts(target_id);

-- +goose Down
DROP INDEX IF EXISTS facts_target_id;
DROP INDEX IF EXISTS facts_target;
DROP TABLE IF EXISTS dirty_targets;
DROP TABLE IF EXISTS dirty_nodes;
DROP TABLE IF EXISTS dirty_owners;
