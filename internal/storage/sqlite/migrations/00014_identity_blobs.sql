-- +goose Up
-- Identity columns are the widest thing in the index. A graph identity is
-- graph.StableID output -- a prefix, a colon, and 20 hex characters -- so it is
-- 22 bytes of text for a one-letter prefix, and half of those bytes are hex
-- digits spelling out 10 bytes of digest. An edge row carries three identities
-- and each of its indexes carries two or three more, so the hex doubling is paid
-- many times per edge.
--
-- This stores the digest raw, as the BLOB encoding in
-- internal/storage/sqlite/identity, which grafo_identity_blob applies here and
-- Key.Value applies on every write after this. Measured on a real 12,744-file
-- index, with both sides vacuumed so free pages do not flatter the result:
--
--     before                1909 MiB
--     after                 1584 MiB   (-325 MiB, -17.0%)
--
-- Where it comes from, by dbstat: the three tables give up 151 MiB, and the six
-- indexes that name an identity column give up the rest. edges and its indexes
-- are the majority of it, because an edge is almost nothing but identities.
-- nodes barely moves: one identity column in a wide row is a small share of it.
--
-- Two bytes of the encoding are overhead and both buy correctness. The separator
-- stays because a digest byte can be an ASCII letter -- 0x6e is 'n' -- so nothing
-- else marks where a prefix ends, and prefixes are not one letter wide: the
-- repository node is "repo:". The tag stays because the encoding has to be total:
-- not every identity is StableID output, and an identity like "a" has to come
-- back unchanged rather than corrupted. Together they cost 72 MiB of the 397 MiB
-- a tagless fixed-width encoding would have saved, and they are what makes the
-- transform reversible for every value in the index rather than for most of them.
--
-- Ordering survives, which identity columns require because they are ORDER BY
-- tiebreaks and a pagination cursor. Compact identities share a tag and compare
-- prefix-first then digest-second, and for equal-length lowercase hex, text order
-- and raw-byte order agree. The empty identity encodes to an empty BLOB, so the
-- "no target" sentinel that 1,474,126 facts carry stays empty and still sorts
-- before every present identity.
--
-- The tables are rebuilt rather than altered because SQLite cannot change a
-- column's type in place, and a rebuild is what applies the encoding to the rows
-- already stored.
--
-- Every call is wrapped in COALESCE because the driver maps a zero-length []byte
-- return to NULL, so the empty identity would arrive as NULL and violate the
-- column's NOT NULL rather than staying the sentinel. Key.Value does not share
-- that problem -- it binds the empty identity as a zero-length blob -- so this
-- guard belongs to the migration path and not to the write path.
CREATE TABLE nodes_identified (
    id BLOB PRIMARY KEY,
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
    external INTEGER NOT NULL DEFAULT 0,
    name_folded TEXT NOT NULL DEFAULT '',
    qualified_name_folded TEXT NOT NULL DEFAULT ''
);

INSERT INTO nodes_identified(id, kind, name, qualified_name, language, path, line, column_no,
    end_line, properties, owner_file, external, name_folded, qualified_name_folded)
SELECT COALESCE(grafo_identity_blob(id), x''), kind, name, qualified_name, language, path, line, column_no,
    end_line, properties, owner_file, external, name_folded, qualified_name_folded
FROM nodes;

DROP TABLE nodes;

ALTER TABLE nodes_identified RENAME TO nodes;

CREATE INDEX nodes_owner ON nodes(owner_file);
CREATE INDEX nodes_external ON nodes(id) WHERE external = 1;
CREATE INDEX nodes_kind ON nodes(kind);
CREATE INDEX nodes_name ON nodes(name COLLATE NOCASE);
CREATE INDEX nodes_name_resolve ON nodes(name, external, kind, id);
CREATE INDEX nodes_qualified ON nodes(qualified_name COLLATE NOCASE);
CREATE INDEX nodes_qualified_resolve ON nodes(qualified_name, external, kind, id);

CREATE TABLE facts_identified (
    id BLOB PRIMARY KEY,
    from_id BLOB NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    source_kind TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    producer TEXT NOT NULL DEFAULT '',
    target_id BLOB NOT NULL DEFAULT x'',
    target TEXT NOT NULL DEFAULT '',
    target_kind TEXT NOT NULL DEFAULT '',
    path_id INTEGER NOT NULL,
    line INTEGER NOT NULL DEFAULT 0,
    column_no INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    properties TEXT NOT NULL DEFAULT '{}',
    owner_path_id INTEGER NOT NULL
);

INSERT INTO facts_identified(id, from_id, source, source_kind, kind, producer, target_id, target,
    target_kind, path_id, line, column_no, end_line, properties, owner_path_id)
SELECT COALESCE(grafo_identity_blob(id), x''), COALESCE(grafo_identity_blob(from_id), x''), source, source_kind, kind, producer,
    COALESCE(grafo_identity_blob(target_id), x''), target, target_kind, path_id, line, column_no, end_line,
    properties, owner_path_id
FROM facts;

DROP TABLE facts;

ALTER TABLE facts_identified RENAME TO facts;

CREATE INDEX facts_owner ON facts(owner_path_id);
CREATE INDEX facts_path ON facts(path_id);
CREATE INDEX facts_from_id ON facts(from_id);
CREATE INDEX facts_source ON facts(source, source_kind);
CREATE INDEX facts_target ON facts(target, target_kind);
CREATE INDEX facts_target_id ON facts(target_id);

CREATE TABLE edges_identified (
    fact_id BLOB NOT NULL,
    from_id BLOB NOT NULL,
    to_id BLOB NOT NULL,
    kind TEXT NOT NULL,
    properties TEXT NOT NULL DEFAULT '{}'
);

INSERT INTO edges_identified(fact_id, from_id, to_id, kind, properties)
SELECT COALESCE(grafo_identity_blob(fact_id), x''), COALESCE(grafo_identity_blob(from_id), x''), COALESCE(grafo_identity_blob(to_id), x''),
    kind, properties
FROM edges;

DROP TABLE edges;

ALTER TABLE edges_identified RENAME TO edges;

CREATE UNIQUE INDEX edges_identity ON edges(fact_id, to_id, kind);
CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_kind ON edges(kind);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);

-- The two reconciliation queues hold identities as well, and they have to be
-- encoded the same way or they stop matching rather than fail. SQLite compares
-- across storage classes by class first, so a BLOB is never equal to a TEXT
-- holding the same characters: dirty_nodes.node_id joined against facts.from_id,
-- and dirty_facts.fact_id against facts.id, would both silently find nothing and
-- reconciliation would quietly resolve no edges.
CREATE TABLE dirty_nodes_identified (
    node_id BLOB PRIMARY KEY
) WITHOUT ROWID;

INSERT INTO dirty_nodes_identified(node_id)
SELECT COALESCE(grafo_identity_blob(node_id), x'') FROM dirty_nodes;

DROP TABLE dirty_nodes;

ALTER TABLE dirty_nodes_identified RENAME TO dirty_nodes;

CREATE TABLE dirty_facts_identified (
    fact_id BLOB PRIMARY KEY,
    owner_file TEXT NOT NULL
) WITHOUT ROWID;

INSERT INTO dirty_facts_identified(fact_id, owner_file)
SELECT COALESCE(grafo_identity_blob(fact_id), x''), owner_file FROM dirty_facts;

DROP TABLE dirty_facts;

ALTER TABLE dirty_facts_identified RENAME TO dirty_facts;

CREATE INDEX dirty_facts_order ON dirty_facts(owner_file, fact_id);

-- +goose Down
-- grafo_identity_text is the exact inverse of grafo_identity_blob, verified
-- against every identity of a real index before this migration was written, so
-- rolling back restores the text the column held rather than an approximation of
-- it.
CREATE TABLE nodes_text (
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
    external INTEGER NOT NULL DEFAULT 0,
    name_folded TEXT NOT NULL DEFAULT '',
    qualified_name_folded TEXT NOT NULL DEFAULT ''
);

INSERT INTO nodes_text(id, kind, name, qualified_name, language, path, line, column_no,
    end_line, properties, owner_file, external, name_folded, qualified_name_folded)
SELECT COALESCE(grafo_identity_text(id), ''), kind, name, qualified_name, language, path, line, column_no,
    end_line, properties, owner_file, external, name_folded, qualified_name_folded
FROM nodes;

DROP TABLE nodes;

ALTER TABLE nodes_text RENAME TO nodes;

CREATE INDEX nodes_owner ON nodes(owner_file);
CREATE INDEX nodes_external ON nodes(id) WHERE external = 1;
CREATE INDEX nodes_kind ON nodes(kind);
CREATE INDEX nodes_name ON nodes(name COLLATE NOCASE);
CREATE INDEX nodes_name_resolve ON nodes(name, external, kind, id);
CREATE INDEX nodes_qualified ON nodes(qualified_name COLLATE NOCASE);
CREATE INDEX nodes_qualified_resolve ON nodes(qualified_name, external, kind, id);

CREATE TABLE facts_text (
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

INSERT INTO facts_text(id, from_id, source, source_kind, kind, producer, target_id, target,
    target_kind, path_id, line, column_no, end_line, properties, owner_path_id)
SELECT COALESCE(grafo_identity_text(id), ''), COALESCE(grafo_identity_text(from_id), ''), source, source_kind, kind, producer,
    COALESCE(grafo_identity_text(target_id), ''), target, target_kind, path_id, line, column_no, end_line,
    properties, owner_path_id
FROM facts;

DROP TABLE facts;

ALTER TABLE facts_text RENAME TO facts;

CREATE INDEX facts_owner ON facts(owner_path_id);
CREATE INDEX facts_path ON facts(path_id);
CREATE INDEX facts_from_id ON facts(from_id);
CREATE INDEX facts_source ON facts(source, source_kind);
CREATE INDEX facts_target ON facts(target, target_kind);
CREATE INDEX facts_target_id ON facts(target_id);

CREATE TABLE edges_text (
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    properties TEXT NOT NULL DEFAULT '{}'
);

INSERT INTO edges_text(fact_id, from_id, to_id, kind, properties)
SELECT COALESCE(grafo_identity_text(fact_id), ''), COALESCE(grafo_identity_text(from_id), ''), COALESCE(grafo_identity_text(to_id), ''),
    kind, properties
FROM edges;

DROP TABLE edges;

ALTER TABLE edges_text RENAME TO edges;

CREATE UNIQUE INDEX edges_identity ON edges(fact_id, to_id, kind);
CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_kind ON edges(kind);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);

CREATE TABLE dirty_nodes_text (
    node_id TEXT PRIMARY KEY
) WITHOUT ROWID;

INSERT INTO dirty_nodes_text(node_id)
SELECT COALESCE(grafo_identity_text(node_id), '') FROM dirty_nodes;

DROP TABLE dirty_nodes;

ALTER TABLE dirty_nodes_text RENAME TO dirty_nodes;

CREATE TABLE dirty_facts_text (
    fact_id TEXT PRIMARY KEY,
    owner_file TEXT NOT NULL
) WITHOUT ROWID;

INSERT INTO dirty_facts_text(fact_id, owner_file)
SELECT COALESCE(grafo_identity_text(fact_id), ''), owner_file FROM dirty_facts;

DROP TABLE dirty_facts;

ALTER TABLE dirty_facts_text RENAME TO dirty_facts;

CREATE INDEX dirty_facts_order ON dirty_facts(owner_file, fact_id);
