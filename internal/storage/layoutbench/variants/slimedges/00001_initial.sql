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

CREATE TABLE nodes (
    id TEXT PRIMARY KEY,
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
    owner_file TEXT NOT NULL
);

CREATE INDEX facts_owner ON facts(owner_file);

-- edges (slim): identity/resolution/adjacency only. Evidence lives on the fact
-- row and is joined at hydration; derived edges override it per edge below.
CREATE TABLE edges (
    id TEXT PRIMARY KEY,
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL
);

CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);

-- exact evidence for the one derived edge family (rare rows)
CREATE TABLE derived_edge_evidence (
    edge_id TEXT PRIMARY KEY,
    producer TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    line INTEGER NOT NULL DEFAULT 0,
    column_no INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    properties TEXT NOT NULL DEFAULT '{}'
);

-- +goose Down
DROP TABLE IF EXISTS derived_edge_evidence;
DROP TABLE IF EXISTS edges;
DROP TABLE IF EXISTS facts;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS meta;
