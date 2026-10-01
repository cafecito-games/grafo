-- +goose Up
-- Every edge is materialized from exactly one fact, and the reconciler copies
-- that fact's producer and location onto the edge verbatim. Those columns are
-- therefore derivable and stop being stored. Migrating an index whose edges do
-- not satisfy the invariant would silently rewrite evidence, so such an index
-- is rejected and must be rebuilt instead. A NOT NULL column with a
-- CHECK (... IS NULL) constraint turns "the guard query matched a row" into a
-- migration failure that Goose rolls back.
CREATE TABLE edge_missing_fact_guard (reason TEXT NOT NULL CHECK (reason IS NULL));

INSERT INTO edge_missing_fact_guard(reason)
SELECT 'an edge references a fact that no longer exists; rebuild this index with grafo index --force'
FROM edges
WHERE NOT EXISTS (SELECT 1 FROM facts WHERE facts.id = edges.fact_id)
LIMIT 1;

DROP TABLE edge_missing_fact_guard;

CREATE TABLE edge_fact_divergence_guard (reason TEXT NOT NULL CHECK (reason IS NULL));

INSERT INTO edge_fact_divergence_guard(reason)
SELECT 'an edge producer or location diverges from its originating fact; rebuild this index with grafo index --force'
FROM edges
JOIN facts ON facts.id = edges.fact_id
WHERE edges.path <> facts.path
   OR edges.line <> facts.line
   OR edges.column_no <> facts.column_no
   OR edges.end_line <> facts.end_line
   OR edges.producer <> facts.producer
LIMIT 1;

DROP TABLE edge_fact_divergence_guard;

-- paths interns every owner key a fact can carry: repository-relative file
-- paths and the indexer's synthetic workspace owner. A fact's location path and
-- its owner key are interned in the same table because they draw from the same
-- repository-relative namespace.
CREATE TABLE paths (
    id INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE
);

INSERT INTO paths(path)
SELECT path FROM (SELECT path FROM facts UNION SELECT owner_file FROM facts)
ORDER BY path;

CREATE TABLE facts_interned (
    id TEXT PRIMARY KEY,
    from_id TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    source_kind TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    producer TEXT NOT NULL DEFAULT '',
    target_id TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL DEFAULT '',
    target_kind TEXT NOT NULL DEFAULT '',
    path_id INTEGER NOT NULL,
    line INTEGER NOT NULL DEFAULT 0,
    column_no INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    properties TEXT NOT NULL DEFAULT '{}',
    owner_path_id INTEGER NOT NULL
);

INSERT INTO facts_interned(
    id, from_id, source, source_kind, kind, producer, target_id, target, target_kind,
    path_id, line, column_no, end_line, properties, owner_path_id
)
SELECT facts.id, facts.from_id, facts.source, facts.source_kind, facts.kind, facts.producer,
       facts.target_id, facts.target, facts.target_kind, location_paths.id, facts.line,
       facts.column_no, facts.end_line, facts.properties, owner_paths.id
FROM facts
JOIN paths AS location_paths ON location_paths.path = facts.path
JOIN paths AS owner_paths ON owner_paths.path = facts.owner_file;

DROP TABLE facts;

ALTER TABLE facts_interned RENAME TO facts;

CREATE INDEX facts_owner ON facts(owner_path_id);
CREATE INDEX facts_path ON facts(path_id);
CREATE INDEX facts_from_id ON facts(from_id);
CREATE INDEX facts_source ON facts(source, source_kind);
CREATE INDEX facts_target ON facts(target, target_kind);
CREATE INDEX facts_target_id ON facts(target_id);

CREATE TABLE edges_derived (
    id TEXT PRIMARY KEY,
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    properties TEXT NOT NULL DEFAULT '{}'
);

INSERT INTO edges_derived(id, fact_id, from_id, to_id, kind, properties)
SELECT id, fact_id, from_id, to_id, kind, properties FROM edges;

DROP TABLE edges;

ALTER TABLE edges_derived RENAME TO edges;

CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);
CREATE INDEX edges_fact ON edges(fact_id);
CREATE INDEX edges_kind ON edges(kind);

-- +goose Down
CREATE TABLE edges_denormalized (
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
    producer TEXT NOT NULL DEFAULT ''
);

INSERT INTO edges_denormalized(
    id, fact_id, from_id, to_id, kind, path, line, column_no, end_line, properties, producer
)
SELECT edges.id, edges.fact_id, edges.from_id, edges.to_id, edges.kind, paths.path,
       facts.line, facts.column_no, facts.end_line, edges.properties, facts.producer
FROM edges
JOIN facts ON facts.id = edges.fact_id
JOIN paths ON paths.id = facts.path_id;

DROP TABLE edges;

ALTER TABLE edges_denormalized RENAME TO edges;

CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);
CREATE INDEX edges_fact ON edges(fact_id);
CREATE INDEX edges_kind ON edges(kind);

CREATE TABLE facts_denormalized (
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
    source TEXT NOT NULL DEFAULT '',
    source_kind TEXT NOT NULL DEFAULT '',
    producer TEXT NOT NULL DEFAULT ''
);

INSERT INTO facts_denormalized(
    id, from_id, kind, target_id, target, target_kind, path, line, column_no, end_line,
    properties, owner_file, source, source_kind, producer
)
SELECT facts.id, facts.from_id, facts.kind, facts.target_id, facts.target, facts.target_kind,
       location_paths.path, facts.line, facts.column_no, facts.end_line, facts.properties,
       owner_paths.path, facts.source, facts.source_kind, facts.producer
FROM facts
JOIN paths AS location_paths ON location_paths.id = facts.path_id
JOIN paths AS owner_paths ON owner_paths.id = facts.owner_path_id;

DROP TABLE facts;

ALTER TABLE facts_denormalized RENAME TO facts;

CREATE INDEX facts_owner ON facts(owner_file);
CREATE INDEX facts_from_id ON facts(from_id);
CREATE INDEX facts_source ON facts(source, source_kind);
CREATE INDEX facts_target ON facts(target, target_kind);
CREATE INDEX facts_target_id ON facts(target_id);

DROP TABLE paths;
