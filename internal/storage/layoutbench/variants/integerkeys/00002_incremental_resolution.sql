-- +goose Up
CREATE TABLE dirty_owners (
    owner_file TEXT PRIMARY KEY
);

-- node_key holds the surrogate the node currently carries, resolved at write
-- time, so the dirty-node enqueue arms can match facts by surrogate as well
-- as by unresolved textual id. The same retire/heal parity that keeps facts
-- and edges on live surrogates applies here: deleting a node zeroes the key
-- in its dirty row, and the node's re-insert heals it to the new surrogate,
-- so a stored nonzero key is always the referenced node's current surrogate.
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

-- Without these two arms a replace that deletes and re-inserts a surviving
-- node id in one transaction would leave the dirty row holding the deleted
-- surrogate (INSERT OR IGNORE on mark keeps the first-written key), and the
-- enqueue arms would miss every fact the heal triggers re-keyed to the new
-- surrogate.
-- +goose StatementBegin
CREATE TRIGGER dirty_nodes_retire_node_key_after_delete AFTER DELETE ON nodes
BEGIN
    UPDATE dirty_nodes SET node_key = 0 WHERE node_key = OLD.node_key;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dirty_nodes_resolve_node_key_after_nodes_insert AFTER INSERT ON nodes
BEGIN
    UPDATE dirty_nodes SET node_key = NEW.node_key
    WHERE node_key = 0 AND node_id = NEW.id;
END;
-- +goose StatementEnd

CREATE TABLE dirty_targets (
    target TEXT NOT NULL,
    target_kind TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (target, target_kind)
);

CREATE INDEX facts_target ON facts(target, target_kind);
-- Composite with the textual id so the heal trigger's target arm and the
-- enqueue OR-arms seek instead of walking the whole 0-key range that
-- name-resolved facts (empty target_id) permanently occupy.
CREATE INDEX facts_target_id ON facts(target_key, target_id);

-- +goose Down
DROP INDEX IF EXISTS facts_target_id;
DROP INDEX IF EXISTS facts_target;
DROP TRIGGER IF EXISTS dirty_nodes_resolve_node_key_after_nodes_insert;
DROP TRIGGER IF EXISTS dirty_nodes_retire_node_key_after_delete;
DROP TRIGGER IF EXISTS dirty_nodes_resolve_node_key_after_insert;
DROP TABLE IF EXISTS dirty_targets;
DROP TABLE IF EXISTS dirty_nodes;
DROP TABLE IF EXISTS dirty_owners;
