-- +goose Up
-- dirty_facts is a natural-key queue; WITHOUT ROWID clusters it on fact_id
-- and drops the implicit autoindex. The nodes_*_resolve indexes keep the
-- production BINARY collation: the measured NOCASE consolidation of the
-- lookup index pairs regressed FindNodesExact from a covering seek to a full
-- index scan and reverted (see the IndexReformReportEvidence artifact).
CREATE TABLE dirty_facts (
    fact_id TEXT PRIMARY KEY,
    owner_file TEXT NOT NULL
) WITHOUT ROWID;

CREATE INDEX dirty_facts_order ON dirty_facts(owner_file, fact_id);

CREATE TABLE reconciliation_cleanup (
    id INTEGER PRIMARY KEY CHECK (id = 1)
) WITHOUT ROWID;

CREATE INDEX edges_fact ON edges(fact_id);
CREATE INDEX nodes_external ON nodes(id) WHERE external = 1;
CREATE INDEX nodes_name_resolve ON nodes(name, external, kind, id);
CREATE INDEX nodes_qualified_resolve ON nodes(qualified_name, external, kind, id);

-- +goose Down
DROP INDEX IF EXISTS nodes_qualified_resolve;
DROP INDEX IF EXISTS nodes_name_resolve;
DROP INDEX IF EXISTS nodes_external;
DROP INDEX IF EXISTS edges_fact;
DROP INDEX IF EXISTS dirty_facts_order;
DROP TABLE IF EXISTS reconciliation_cleanup;
DROP TABLE IF EXISTS dirty_facts;
