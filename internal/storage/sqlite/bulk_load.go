package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cafecito-games/grafo/internal/graph"
)

// deferredIndexesMeta stores the DDL of the secondary indexes a bulk load
// dropped. It is written before the first index is dropped and cleared only
// once the last one is back, so a load that is interrupted anywhere leaves
// behind the exact statements needed to repair the index, and opening it runs
// them.
const deferredIndexesMeta = "deferred_secondary_indexes"

// bulkLoadTables are the tables whose secondary indexes a bulk load rebuilds in
// one sorted pass instead of maintaining through millions of random inserts.
// Node, fact, and edge identities are content hashes, so every entry lands in a
// different page of every index it belongs to.
var bulkLoadTables = []string{"nodes", "facts", "edges"}

// bulkLoadRetainedIndexes are the secondary indexes a bulk load keeps.
//
// nodes_owner, facts_owner, and edges_fact drive the three deletes that every
// file replacement issues, which would otherwise scan tables that grow as the
// load proceeds. facts_path is named by DeleteUnreferencedPaths in an INDEXED BY
// clause, and SQLite rejects a query that names an index it cannot find rather
// than planning around it.
//
// edges_from and edges_to are named the same way by the relation-edge read
// queries. Dropping them would be the largest single saving here, because the
// two of them are the widest indexes in the schema and reconciliation inserts
// millions of edges, but a query that another process runs against a
// half-loaded index would fail outright instead of returning partial evidence.
// Relaxing those two hints is what deferring them waits on.
var bulkLoadRetainedIndexes = map[string]bool{
	"nodes_owner": true,
	"facts_owner": true,
	"facts_path":  true,
	"edges_fact":  true,
	"edges_from":  true,
	"edges_to":    true,
}

// repairDeferredIndexes rebuilds the indexes an interrupted bulk load left
// dropped. It runs on a bare connection before any statement is prepared,
// because the schema names several of these indexes in INDEXED BY clauses and
// SQLite refuses to prepare a statement whose named index is missing: without
// this, an interrupted load would leave an index that cannot even be opened.
func repairDeferredIndexes(ctx context.Context, db *sql.DB) error {
	var stored string
	err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", deferredIndexesMeta).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && stored == "") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load deferred secondary indexes: %w", err)
	}
	var deferred []deferredIndex
	if err := json.Unmarshal([]byte(stored), &deferred); err != nil {
		return fmt.Errorf("decode deferred secondary indexes: %w", err)
	}
	for index, deferredIndex := range deferred {
		if _, err := db.ExecContext(ctx, deferredIndex.DDL); err != nil {
			return fmt.Errorf("rebuild secondary index %s: %w", deferredIndex.Name, err)
		}
		if err := recordDeferredIndexesOn(ctx, db, deferred[index+1:]); err != nil {
			return err
		}
	}
	return nil
}

func recordDeferredIndexesOn(ctx context.Context, db *sql.DB, deferred []deferredIndex) error {
	value := ""
	if len(deferred) > 0 {
		encoded, err := json.Marshal(deferred)
		if err != nil {
			return fmt.Errorf("encode deferred secondary indexes: %w", err)
		}
		value = string(encoded)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		deferredIndexesMeta, value); err != nil {
		return fmt.Errorf("record deferred secondary indexes: %w", err)
	}
	return nil
}

// BeginBulkLoad drops the deferrable secondary indexes when the graph is empty,
// which is the only state in which the whole index is about to be written from
// nothing. A graph that already holds evidence is left exactly as it is, so the
// call is safe for any caller that cannot tell the difference.
func (r *Repository) BeginBulkLoad(ctx context.Context) error {
	if r.deferredIndexes != nil {
		return nil
	}
	empty, err := r.graphIsEmpty(ctx)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}
	deferred, err := r.deferrableIndexDDL(ctx)
	if err != nil {
		return err
	}
	if len(deferred) == 0 {
		return nil
	}
	if err := r.recordDeferredIndexes(ctx, deferred); err != nil {
		return err
	}
	for _, index := range deferred {
		if _, err := r.db.ExecContext(ctx, fmt.Sprintf("DROP INDEX IF EXISTS %q", index.Name)); err != nil {
			return fmt.Errorf("drop secondary index %s: %w", index.Name, err)
		}
	}
	r.deferredIndexes = deferred
	return nil
}

// EndBulkLoad rebuilds whatever BeginBulkLoad dropped. It is idempotent, and
// reconciliation calls it on its own behalf as well, because its resolution
// queries name several of these indexes explicitly.
func (r *Repository) EndBulkLoad(ctx context.Context) error {
	deferred := r.deferredIndexes
	if deferred == nil {
		stored, err := r.storedDeferredIndexes(ctx)
		if err != nil {
			return err
		}
		deferred = stored
	}
	for index, deferredIndex := range deferred {
		if _, err := r.db.ExecContext(ctx, deferredIndex.DDL); err != nil {
			return fmt.Errorf("rebuild secondary index %s: %w", deferredIndex.Name, err)
		}
		// Each index is rebuilt in its own implicit transaction and removed from
		// the ledger immediately, so an interruption leaves only the indexes that
		// really are still missing to be rebuilt on the next open.
		if err := r.recordDeferredIndexes(ctx, deferred[index+1:]); err != nil {
			return err
		}
	}
	r.deferredIndexes = nil
	return nil
}

// deferredIndex is one secondary index a bulk load is allowed to rebuild late.
type deferredIndex struct {
	Name string `json:"name"`
	DDL  string `json:"ddl"`
}

func (r *Repository) graphIsEmpty(ctx context.Context) (bool, error) {
	for _, counted := range []struct {
		name  string
		count func(context.Context) (int64, error)
	}{
		{name: "nodes", count: r.queries.CountNodes},
		{name: "facts", count: r.queries.CountFacts},
		{name: "edges", count: r.queries.CountEdges},
	} {
		total, err := counted.count(ctx)
		if err != nil {
			return false, fmt.Errorf("count %s before bulk load: %w", counted.name, err)
		}
		if total > 0 {
			return false, nil
		}
	}
	return true, nil
}

// deferrableIndexDDL reads the schema rather than restating it, so an index
// added by a later migration is deferred without this file having to know about
// it. A new index that the load itself depends on has to join
// bulkLoadRetainedIndexes.
func (r *Repository) deferrableIndexDDL(ctx context.Context) ([]deferredIndex, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, sql FROM sqlite_master
WHERE type = 'index' AND sql IS NOT NULL AND tbl_name IN (?, ?, ?) ORDER BY name`,
		bulkLoadTables[0], bulkLoadTables[1], bulkLoadTables[2])
	if err != nil {
		return nil, fmt.Errorf("list secondary indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var deferred []deferredIndex
	for rows.Next() {
		var index deferredIndex
		if err := rows.Scan(&index.Name, &index.DDL); err != nil {
			return nil, fmt.Errorf("read secondary index: %w", err)
		}
		if bulkLoadRetainedIndexes[index.Name] {
			continue
		}
		deferred = append(deferred, index)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secondary indexes: %w", err)
	}
	return deferred, nil
}

func (r *Repository) recordDeferredIndexes(ctx context.Context, deferred []deferredIndex) error {
	if len(deferred) == 0 {
		return r.SetMeta(ctx, deferredIndexesMeta, "")
	}
	encoded, err := json.Marshal(deferred)
	if err != nil {
		return fmt.Errorf("encode deferred secondary indexes: %w", err)
	}
	return r.SetMeta(ctx, deferredIndexesMeta, string(encoded))
}

func (r *Repository) storedDeferredIndexes(ctx context.Context) ([]deferredIndex, error) {
	stored, err := r.Meta(ctx, deferredIndexesMeta)
	if err != nil {
		return nil, fmt.Errorf("load deferred secondary indexes: %w", err)
	}
	if stored == "" {
		return nil, nil
	}
	var deferred []deferredIndex
	if err := json.Unmarshal([]byte(stored), &deferred); err != nil {
		return nil, fmt.Errorf("decode deferred secondary indexes: %w", err)
	}
	return deferred, nil
}

var _ graph.BulkLoadRepository = (*Repository)(nil)
