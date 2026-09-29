-- +goose Up
CREATE TABLE dirty_owners (
    owner_file TEXT PRIMARY KEY
);

-- node_key holds the surrogate the node carried when the row was written,
-- resolved at write time, so the dirty-node enqueue arms can match facts by
-- surrogate as well as by unresolved textual id.
CREATE TABLE dirty_nodes (
    node_id TEXT PRIMARY KEY,
    node_key INTEGER NOT NULL DEFAULT 0
);

-- +goose StatementBegin
CREATE TRIGGER dirty_nodes_resolve_node_key_after_insert AFTER INSERT ON dirty_nodes
BEGIN
    UPDATE dirty_nodes SET node_key = COALESCE((SELECT node_key FROM nodes WHERE nodes.id = NEW.node_id), 0)
    WHERE dirty_nodes.node_id = NEW.node_id;
END;
-- +goose StatementEnd

CREATE TABLE dirty_targets (
    target TEXT NOT NULL,
    target_kind TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (target, target_kind)
);

CREATE INDEX facts_target ON facts(target, target_kind);
CREATE INDEX facts_target_id ON facts(target_key);

-- +goose Down
DROP INDEX IF EXISTS facts_target_id;
DROP INDEX IF EXISTS facts_target;
DROP TRIGGER IF EXISTS dirty_nodes_resolve_node_key_after_insert;
DROP TABLE IF EXISTS dirty_targets;
DROP TABLE IF EXISTS dirty_nodes;
DROP TABLE IF EXISTS dirty_owners;
