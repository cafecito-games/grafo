-- +goose Up
-- edges.id is a pure function of two other columns in its own row. Every edge id
-- is StableID("e", fact_id, to_id), except a structural test edge, which
-- DirectTestEdge derives as StableID("e", fact_id, to_id, "tests") to keep it
-- distinct from the calls or references edge it was built from. Both forms are
-- determined by (fact_id, to_id, kind), so the column carried 22 bytes per row and
-- a primary-key index over 2.5M content hashes to store what the row already said.
--
-- Being a hash, it was also inserted in random page order, so every edge write
-- touched an unpredictable index page. (fact_id, to_id, kind) arrives in near-fact
-- order instead.
--
-- Measured on a real 12.7k-file index, the edges family of table and indexes:
--
--     before                                    818 MiB
--     after                                     747 MiB
--
-- The identity index is wider than the edges_fact it replaces, 150 MiB against
-- 83 MiB, which is why the saving is 71 MiB rather than the 83 MiB of primary-key
-- index plus 56 MiB of column that disappear. It replaces edges_fact rather than
-- joining it because fact_id is its leading column, so it serves every lookup
-- edges_fact served: DeleteEdgesByDirtyFactBatch still plans as
-- SEARCH edges USING COVERING INDEX edges_identity (fact_id=?), which is the plan
-- the per-batch edge delete depends on.
--
-- A WITHOUT ROWID table keyed by the same columns was measured too and rejected at
-- 810 MiB: edges carries three secondary indexes, and each would have to repeat the
-- 52-byte composite key in place of an 8-byte rowid, which gives back nearly
-- everything the merge saves.
--
-- Uniqueness is load-bearing rather than defensive. Because the id is now derived
-- from these three columns, two rows sharing them would derive one id, and the id
-- is what identifies message-flow evidence in query results. A non-unique index
-- would have saved a further 75 MiB and made the derivation ill-defined.
-- (fact_id, to_id, kind) is exactly unique on the reference index: 2,565,131
-- distinct tuples over 2,565,131 rows, where (fact_id, to_id) alone has 88,825
-- duplicates, one per structural test edge.
CREATE TABLE edges_derived (
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    properties TEXT NOT NULL DEFAULT '{}'
);

INSERT INTO edges_derived(fact_id, from_id, to_id, kind, properties)
SELECT fact_id, from_id, to_id, kind, properties FROM edges;

DROP TABLE edges;

ALTER TABLE edges_derived RENAME TO edges;

CREATE UNIQUE INDEX edges_identity ON edges(fact_id, to_id, kind);
CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);
CREATE INDEX edges_kind ON edges(kind);

-- +goose Down
-- The id is recomputed rather than remembered, which is the claim this migration
-- rests on. grafo_stable_id is graph.StableID registered on every connection by
-- internal/storage/sqlitedriver, so the rollback produces the same bytes the Go
-- code produced and not a second definition of identity living in the schema.
--
-- Verified against a real 12.7k-file index before this migration was written: the
-- derivation below reproduces all 2,565,131 stored edge ids with zero mismatches.
CREATE TABLE edges_identified (
    id TEXT PRIMARY KEY,
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    properties TEXT NOT NULL DEFAULT '{}'
);

INSERT INTO edges_identified(id, fact_id, from_id, to_id, kind, properties)
SELECT
    CASE WHEN kind = 'tests'
        THEN grafo_stable_id('e', fact_id, to_id, 'tests')
        ELSE grafo_stable_id('e', fact_id, to_id)
    END,
    fact_id, from_id, to_id, kind, properties
FROM edges;

DROP TABLE edges;

ALTER TABLE edges_identified RENAME TO edges;

CREATE INDEX edges_fact ON edges(fact_id);
CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);
CREATE INDEX edges_kind ON edges(kind);
