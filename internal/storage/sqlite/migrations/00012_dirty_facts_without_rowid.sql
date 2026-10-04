-- +goose Up
-- dirty_facts is the reconciliation queue, and enqueueing it is the single widest
-- write in an indexing run: a cold index of a 12k-file repository puts every one of
-- its ~2.4M facts into this table in one transaction, which is what forces the
-- write-ahead log to just over half a gigabyte.
--
-- As a rowid table, `fact_id TEXT PRIMARY KEY` costs two b-trees for one logical
-- key. The rowid b-tree holds both columns keyed by an integer nobody uses, and
-- SQLite adds sqlite_autoindex_dirty_facts_1 to map fact_id back to that integer,
-- so the 22-byte content hash is stored twice per row and two trees take an insert
-- apiece. WITHOUT ROWID merges them: the table becomes one b-tree keyed by fact_id
-- with owner_file as its payload.
--
-- dirty_facts_order stays, because ordering batches by (owner_file, fact_id) is
-- what gives the resolution caches their per-owner locality, and it does not grow
-- under this change: an index on a WITHOUT ROWID table identifies rows by the
-- primary key, and fact_id is already the index's trailing column, so its entries
-- hold exactly what they held before.
--
-- The queue is rebuilt rather than recreated empty because reconciliation can be
-- pending when a migration runs -- an interrupted run leaves rows here, and
-- ReconciliationPending reads them to decide whether work is owed. Dropping them
-- would silently retire that work and leave edges unreconciled against facts that
-- no longer match.
CREATE TABLE dirty_facts_rebuilt (
    fact_id TEXT PRIMARY KEY,
    owner_file TEXT NOT NULL
) WITHOUT ROWID;

INSERT INTO dirty_facts_rebuilt(fact_id, owner_file)
SELECT fact_id, owner_file FROM dirty_facts;

-- Dropping the table drops dirty_facts_order with it, so the index is recreated
-- after the rename rather than before it.
DROP TABLE dirty_facts;

ALTER TABLE dirty_facts_rebuilt RENAME TO dirty_facts;

CREATE INDEX dirty_facts_order ON dirty_facts(owner_file, fact_id);

-- +goose Down
CREATE TABLE dirty_facts_rowid (
    fact_id TEXT PRIMARY KEY,
    owner_file TEXT NOT NULL
);

INSERT INTO dirty_facts_rowid(fact_id, owner_file)
SELECT fact_id, owner_file FROM dirty_facts;

DROP TABLE dirty_facts;

ALTER TABLE dirty_facts_rowid RENAME TO dirty_facts;

CREATE INDEX dirty_facts_order ON dirty_facts(owner_file, fact_id);
