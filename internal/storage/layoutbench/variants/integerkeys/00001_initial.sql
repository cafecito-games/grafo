-- +goose Up
PRAGMA foreign_keys = OFF;

CREATE TABLE meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE files (
    path TEXT PRIMARY KEY,
    hash TEXT NOT NULL,
    language TEXT NOT NULL,
    size INTEGER NOT NULL,
    modified_ns INTEGER NOT NULL,
    indexed_at TEXT NOT NULL
);

-- node_key is the rowid alias every adjacency and node-id secondary index
-- keys on; the public textual id stays unique and reaches every repository
-- boundary unchanged.
CREATE TABLE nodes (
    node_key INTEGER PRIMARY KEY,
    id TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL,
    language TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    line INTEGER NOT NULL DEFAULT 0,
    column_no INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    properties TEXT NOT NULL DEFAULT '{}',
    owner_file TEXT NOT NULL,
    external INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX nodes_name ON nodes(name COLLATE NOCASE);
CREATE INDEX nodes_qualified ON nodes(qualified_name COLLATE NOCASE);
CREATE INDEX nodes_owner ON nodes(owner_file);
CREATE INDEX nodes_kind ON nodes(kind);

CREATE TABLE facts (
    id TEXT PRIMARY KEY,
    from_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    target_id TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL DEFAULT '',
    target_kind TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    line INTEGER NOT NULL DEFAULT 0,
    column_no INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    properties TEXT NOT NULL DEFAULT '{}',
    owner_file TEXT NOT NULL,
    from_key INTEGER NOT NULL DEFAULT 0,
    target_key INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX facts_owner ON facts(owner_file);

CREATE TABLE edges (
    id TEXT PRIMARY KEY,
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    path TEXT NOT NULL DEFAULT '',
    line INTEGER NOT NULL DEFAULT 0,
    column_no INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    properties TEXT NOT NULL DEFAULT '{}',
    producer TEXT NOT NULL DEFAULT '',
    from_key INTEGER NOT NULL DEFAULT 0,
    to_key INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX edges_from ON edges(from_key, kind, to_key);
CREATE INDEX edges_to ON edges(to_key, kind, from_key);

-- Surrogate translation at write time: the fact's node-id references carry
-- the surrogate the referenced node holds at write time, 0 when that node
-- does not exist yet; reconciliation is the authority and re-derives the
-- edge, whose insert re-resolves. Keys are only refreshed by these triggers
-- and by node deletion below, so a stored nonzero key is always the
-- referenced node's current surrogate — an invariant that holds for every
-- key-bearing row: facts and edges here, and the dirty-node queue the
-- later migrations add under the same retire/heal parity.
-- +goose StatementBegin
CREATE TRIGGER facts_resolve_node_keys_after_insert AFTER INSERT ON facts
BEGIN
    UPDATE facts SET
        from_key = COALESCE((SELECT node_key FROM nodes WHERE nodes.id = NEW.from_id), 0),
        target_key = COALESCE((SELECT node_key FROM nodes WHERE nodes.id = NEW.target_id), 0)
    WHERE facts.id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER facts_resolve_node_keys_after_update AFTER UPDATE OF from_id, target_id ON facts
BEGIN
    UPDATE facts SET
        from_key = COALESCE((SELECT node_key FROM nodes WHERE nodes.id = NEW.from_id), 0),
        target_key = COALESCE((SELECT node_key FROM nodes WHERE nodes.id = NEW.target_id), 0)
    WHERE facts.id = NEW.id;
END;
-- +goose StatementEnd

-- A surrogate is retired with its node: SQLite only reassigns a rowid after
-- its row is deleted, so zeroing every fact and edge that still holds the
-- deleted key leaves each stored key either the referenced node's current
-- surrogate or 0 — exactly the pair the dirty-node enqueue arms match (a
-- delete+reinsert re-keys the node and zeroes its foreign facts, which the
-- arms then catch through the textual id).
-- +goose StatementBegin
CREATE TRIGGER nodes_retire_node_key_after_delete AFTER DELETE ON nodes
BEGIN
    UPDATE facts SET from_key = 0 WHERE from_key = OLD.node_key;
    UPDATE facts SET target_key = 0 WHERE target_key = OLD.node_key;
    UPDATE edges SET from_key = 0 WHERE from_key = OLD.node_key;
    UPDATE edges SET to_key = 0 WHERE to_key = OLD.node_key;
END;
-- +goose StatementEnd

-- The bounded writer flushes each buffer independently on overflow, so an
-- edge or fact row can reach the table while the node it references still
-- sits in the unflushed nodes buffer and resolves to 0. Flushing always
-- writes nodes before facts and edges at commit, so the node's eventual
-- insert heals every such row: by the time the transaction commits, each
-- edge and fact referencing an existing node holds that node's current
-- surrogate.
-- +goose StatementBegin
CREATE TRIGGER nodes_resolve_reference_keys_after_insert AFTER INSERT ON nodes
BEGIN
    UPDATE facts SET from_key = NEW.node_key WHERE from_key = 0 AND from_id = NEW.id;
    UPDATE facts SET target_key = NEW.node_key WHERE target_key = 0 AND target_id = NEW.id;
    UPDATE edges SET from_key = NEW.node_key WHERE from_key = 0 AND from_id = NEW.id;
    UPDATE edges SET to_key = NEW.node_key WHERE to_key = 0 AND to_id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS nodes_resolve_reference_keys_after_insert;
DROP TRIGGER IF EXISTS nodes_retire_node_key_after_delete;
DROP TRIGGER IF EXISTS facts_resolve_node_keys_after_update;
DROP TRIGGER IF EXISTS facts_resolve_node_keys_after_insert;
DROP TABLE IF EXISTS edges;
DROP TABLE IF EXISTS facts;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS meta;
