package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
)

// LayoutSpec describes one candidate SQLite physical layout as deltas from
// production: which Goose migrations build the schema, which named statements
// replace the production query text, and which write or read behaviors the
// shared adapter routes differently. The zero-delta spec (ProductionSpec) must
// behave exactly like the production adapter, which is what the equivalence
// driver proves before any layout measurement happens; candidates are small
// deltas on top of that proven core.
type LayoutSpec struct {
	// Name labels the layout in reports.
	Name string
	// Migrations builds the schema; Goose applies it at open time. Candidate
	// layouts mirror the production migration set with the same version
	// numbers so a pre-seeded database stays interchangeable.
	Migrations fs.FS
	// SQL overrides the text of one named statement (the -- name: values in
	// internal/storage/sqlite/queries/*.sql). Entries absent here fall back to
	// the production body mirrored by ProductionPlanQueries, whose drift test
	// keeps that mirror aligned with the sqlc sources. A name that is not a
	// production statement declares a candidate-only statement; every entry is
	// prepared when the adapter opens.
	//
	// Statements may use @name placeholders. For prepared (non-batched)
	// statements the adapter numbers them by first appearance, so repeated
	// names bind one call-site value; an override that needs one logical value
	// twice (say a node id and the subselect that resolves its integer key)
	// names the parameter instead of duplicating the value at the call site.
	// Batched writes (nodes, facts, edges, and the dirty markers) are
	// different: each occurrence of a placeholder inside the values tuple
	// binds one value per emitted row, and placeholders outside the tuple are
	// rejected at parse time.
	SQL map[string]string
	// EdgeInsert replaces the default batched edge write during file
	// replacement and reconciliation. derived marks a DirectTestEdge
	// derivation, whose evidence is not the fact row.
	EdgeInsert EdgeInsertFunc
	// Hydrate replaces the default edge-row scan for adjacency queries
	// (ListEdgesFrom, ListEdgesTo, ListExternalEdgesMatching). The default
	// expects the production edge column order.
	Hydrate EdgeHydrationFunc
	// PostEdgeDelete runs inside the same transaction immediately before the
	// adapter deletes edges for one scope, so a layout with side tables can
	// mirror the deletion.
	PostEdgeDelete PostEdgeDeleteFunc
	// DerivedEvidence moves derived-edge evidence out of the edge row. When
	// true the adapter requires EdgeInsert and routes every edge write through
	// it with the derived flag.
	DerivedEvidence bool
	// IntegerKeys keys adjacency on integer node surrogates. When true the
	// adapter resolves a public node id to its surrogate once per adjacency
	// lookup through the ResolveNodeKey statement and binds the surrogate as
	// the subject of the (overridden) adjacency statements.
	IntegerKeys bool
}

// EdgeDeleteScope names the edge deletion a PostEdgeDelete hook precedes. The
// scope value is the production statement name whose predicate the hook must
// mirror.
type EdgeDeleteScope string

const (
	// EdgeDeleteDirtyFactBatch precedes DeleteEdgesByDirtyFactBatch; the
	// argument is the reconciliation batch bound.
	EdgeDeleteDirtyFactBatch EdgeDeleteScope = "DeleteEdgesByDirtyFactBatch"
	// EdgeDeleteOwnerFacts precedes DeleteEdgesByOwnerFacts; the argument is
	// the owner path.
	EdgeDeleteOwnerFacts EdgeDeleteScope = "DeleteEdgesByOwnerFacts"
)

// EdgeDeleteExecutor runs one named statement inside the active transaction.
type EdgeDeleteExecutor interface {
	ExecStatement(ctx context.Context, statementName string, values ...any) error
}

// PostEdgeDeleteFunc mirrors an edge deletion into layout-owned side tables.
// It runs immediately before the scoped production deletion, inside the same
// transaction, with the same bound arguments.
type PostEdgeDeleteFunc func(ctx context.Context, executor EdgeDeleteExecutor, scope EdgeDeleteScope, arguments []any) error

// StatementWriter buffers rows for one named single-row statement inside the
// active transaction, under the same bounded batch limits as the default
// write path.
type StatementWriter interface {
	AddStatement(ctx context.Context, statementName string, values ...any) error
}

// EdgeInsertFunc writes one edge row. derived is true only for DirectTestEdge
// derivations, whose evidence is not the fact row.
type EdgeInsertFunc func(ctx context.Context, writer StatementWriter, edge graph.Edge, derived bool) error

// EdgeHydrationFunc scans adjacency query rows into edges.
type EdgeHydrationFunc func(rows *sql.Rows) ([]graph.Edge, error)

// ResolveNodeKeyStatementName is the candidate-only statement an IntegerKeys
// layout must provide: it maps one public node id to its integer surrogate.
const ResolveNodeKeyStatementName = "ResolveNodeKey"

// ProductionSpec returns the zero-delta layout: production migrations and the
// production statement text everywhere.
func ProductionSpec(name string) LayoutSpec {
	return LayoutSpec{Name: name, Migrations: migrations.Files, SQL: map[string]string{}}
}

// Candidate-only statement names of the slim-edges layout: the derived
// evidence side table's insert and the scoped deletions its PostEdgeDelete
// hook mirrors.
const (
	slimEdgesEvidenceInsertStatement                 = "InsertDerivedEdgeEvidence"
	slimEdgesEvidenceDeleteByDirtyFactBatchStatement = "DeleteDerivedEdgeEvidenceByDirtyFactBatch"
	slimEdgesEvidenceDeleteByOwnerFactsStatement     = "DeleteDerivedEdgeEvidenceByOwnerFacts"
)

// SlimEdgesSpec returns the slim resolved-adjacency candidate: facts stay the
// canonical evidence, ordinary edge rows keep only identity and resolution
// columns, hydration joins the fact, and the one derived edge family keeps
// exact evidence in a compact override table written alongside the edge.
func SlimEdgesSpec(name string) LayoutSpec {
	return LayoutSpec{
		Name:       name,
		Migrations: slimEdgesMigrations,
		SQL: map[string]string{
			edgeStatement: `INSERT INTO edges(id, fact_id, from_id, to_id, kind) VALUES (?, ?, ?, ?, ?);`,
			slimEdgesEvidenceInsertStatement: `INSERT OR REPLACE INTO derived_edge_evidence(edge_id, producer, path, line, column_no, end_line, properties)
VALUES (?, ?, ?, ?, ?, ?, ?);`,
			// The hydration statements INNER JOIN facts: edges are always
			// written from facts read in the same transaction and deleted
			// before their facts, so a dangling edge is unreachable — a LEFT
			// JOIN would surface rather than hide any future integrity
			// violation.
			"ListEdgesFrom": `SELECT e.id, e.fact_id, e.from_id, e.to_id, e.kind,
       COALESCE(de.producer, f.producer) AS producer,
       COALESCE(de.path, f.path) AS path,
       COALESCE(de.line, f.line) AS line,
       COALESCE(de.column_no, f.column_no) AS column_no,
       COALESCE(de.end_line, f.end_line) AS end_line,
       COALESCE(de.properties, f.properties) AS properties
FROM edges e
JOIN facts f ON f.id = e.fact_id
LEFT JOIN derived_edge_evidence de ON de.edge_id = e.id
WHERE e.from_id = ? ORDER BY e.kind, e.to_id, e.id;`,
			"ListEdgesTo": `SELECT e.id, e.fact_id, e.from_id, e.to_id, e.kind,
       COALESCE(de.producer, f.producer) AS producer,
       COALESCE(de.path, f.path) AS path,
       COALESCE(de.line, f.line) AS line,
       COALESCE(de.column_no, f.column_no) AS column_no,
       COALESCE(de.end_line, f.end_line) AS end_line,
       COALESCE(de.properties, f.properties) AS properties
FROM edges e
JOIN facts f ON f.id = e.fact_id
LEFT JOIN derived_edge_evidence de ON de.edge_id = e.id
WHERE e.to_id = ? ORDER BY e.kind, e.from_id, e.id;`,
			"ListIncomingRelationEdges": `SELECT
    edges.id AS edge_id,
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    COALESCE(de.producer, f.producer) AS edge_producer,
    COALESCE(de.path, f.path) AS edge_path,
    COALESCE(de.line, f.line) AS edge_line,
    COALESCE(de.column_no, f.column_no) AS edge_column_no,
    COALESCE(de.end_line, f.end_line) AS edge_end_line,
    COALESCE(de.properties, f.properties) AS edge_properties,
    COALESCE(nodes.id, '') AS counterpart_id,
    COALESCE(nodes.kind, '') AS counterpart_kind,
    COALESCE(nodes.name, '') AS counterpart_name,
    COALESCE(nodes.qualified_name, '') AS counterpart_qualified_name,
    COALESCE(nodes.language, '') AS counterpart_language,
    COALESCE(nodes.path, '') AS counterpart_path,
    COALESCE(nodes.line, 0) AS counterpart_line,
    COALESCE(nodes.column_no, 0) AS counterpart_column_no,
    COALESCE(nodes.end_line, 0) AS counterpart_end_line,
    COALESCE(nodes.properties, '{}') AS counterpart_properties,
    COALESCE(nodes.owner_file, '') AS counterpart_owner_file,
    COALESCE(nodes.external, 0) AS counterpart_external
FROM edges INDEXED BY edges_to
JOIN facts f ON f.id = edges.fact_id
LEFT JOIN derived_edge_evidence de ON de.edge_id = edges.id
LEFT JOIN nodes ON nodes.id = edges.from_id
WHERE edges.to_id = @subject_id AND edges.kind = @relation
ORDER BY edges.from_id, edges.id
LIMIT @max_results;`,
			"ListOutgoingRelationEdges": `SELECT
    edges.id AS edge_id,
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    COALESCE(de.producer, f.producer) AS edge_producer,
    COALESCE(de.path, f.path) AS edge_path,
    COALESCE(de.line, f.line) AS edge_line,
    COALESCE(de.column_no, f.column_no) AS edge_column_no,
    COALESCE(de.end_line, f.end_line) AS edge_end_line,
    COALESCE(de.properties, f.properties) AS edge_properties,
    COALESCE(nodes.id, '') AS counterpart_id,
    COALESCE(nodes.kind, '') AS counterpart_kind,
    COALESCE(nodes.name, '') AS counterpart_name,
    COALESCE(nodes.qualified_name, '') AS counterpart_qualified_name,
    COALESCE(nodes.language, '') AS counterpart_language,
    COALESCE(nodes.path, '') AS counterpart_path,
    COALESCE(nodes.line, 0) AS counterpart_line,
    COALESCE(nodes.column_no, 0) AS counterpart_column_no,
    COALESCE(nodes.end_line, 0) AS counterpart_end_line,
    COALESCE(nodes.properties, '{}') AS counterpart_properties,
    COALESCE(nodes.owner_file, '') AS counterpart_owner_file,
    COALESCE(nodes.external, 0) AS counterpart_external
FROM edges INDEXED BY edges_from
JOIN facts f ON f.id = edges.fact_id
LEFT JOIN derived_edge_evidence de ON de.edge_id = edges.id
LEFT JOIN nodes ON nodes.id = edges.to_id
WHERE edges.from_id = @subject_id AND edges.kind = @relation
ORDER BY edges.to_id, edges.id
LIMIT @max_results;`,
			"ListExternalEdgesMatching": `SELECT edges.id, edges.fact_id, edges.from_id, edges.to_id, edges.kind,
       COALESCE(de.producer, f.producer) AS producer,
       COALESCE(de.path, f.path) AS path,
       COALESCE(de.line, f.line) AS line,
       COALESCE(de.column_no, f.column_no) AS column_no,
       COALESCE(de.end_line, f.end_line) AS end_line,
       COALESCE(de.properties, f.properties) AS properties
FROM edges
JOIN facts f ON f.id = edges.fact_id
LEFT JOIN derived_edge_evidence de ON de.edge_id = edges.id
JOIN nodes ON nodes.id = edges.to_id
WHERE nodes.external = 1
  AND (
      nodes.qualified_name = @qualified_name
      OR nodes.qualified_name = @name
      OR nodes.name = @qualified_name
      OR nodes.name = @name
  )
ORDER BY edges.kind, edges.from_id, edges.id;`,
			slimEdgesEvidenceDeleteByDirtyFactBatchStatement: `DELETE FROM derived_edge_evidence WHERE edge_id IN (
    SELECT id FROM edges WHERE fact_id IN (
        SELECT fact_id
        FROM dirty_facts INDEXED BY dirty_facts_order
        ORDER BY owner_file, fact_id
        LIMIT ?
    )
);`,
			slimEdgesEvidenceDeleteByOwnerFactsStatement: `DELETE FROM derived_edge_evidence WHERE edge_id IN (
    SELECT id FROM edges WHERE fact_id IN (SELECT id FROM facts WHERE owner_file = ?)
);`,
		},
		EdgeInsert:      slimEdgesInsertEdge,
		Hydrate:         slimEdgesHydrateEdge,
		PostEdgeDelete:  slimEdgesDeleteEvidence,
		DerivedEvidence: true,
	}
}

// IndexReformSpec returns the pure-DDL index-reform candidate: the
// production statement text runs unchanged against reformed B-trees, so the
// spec carries no overrides. The small natural-key tables (meta, files, and
// the dirty queues) are stored WITHOUT ROWID, dropping one implicit
// primary-key autoindex each. The candidate's one index consolidation
// attempt — one COLLATE NOCASE composite per node lookup column replacing
// the NOCASE single plus the binary resolve pair — reverted after the
// captured plans showed the binary-collation resolution queries degrading
// from a covering seek to a full index scan; IndexReformReportEvidence
// records every keep/reform/revert decision with its plan evidence. The
// canonical way to run this layout is OpenPreSeeded, which drives the
// production adapter itself over the reformed DDL.
func IndexReformSpec(name string) LayoutSpec {
	return LayoutSpec{Name: name, Migrations: indexReformMigrations, SQL: map[string]string{}}
}

// IntegerKeysSpec returns the compact integer internal keys candidate: public
// textual ids stay unchanged at every repository boundary while adjacency and
// the node-id secondary indexes key on a VACUUM-stable integer surrogate the
// writer maintains — facts resolve their surrogate references through write
// time triggers, a node deletion retires the surrogate it owned, and every
// adjacency lookup resolves the public id once through ResolveNodeKey.
func IntegerKeysSpec(name string) LayoutSpec {
	return LayoutSpec{
		Name:        name,
		Migrations:  integerKeysMigrations,
		SQL:         integerKeysStatements,
		EdgeInsert:  integerKeysInsertEdge,
		IntegerKeys: true,
	}
}

// slimEdgesInsertEdge writes one edge row through the bounded batch and, for
// a DirectTestEdge derivation whose evidence is not the fact row, records the
// exact evidence in the override table.
func slimEdgesInsertEdge(ctx context.Context, writer StatementWriter, edge graph.Edge, derived bool) error {
	if err := writer.AddStatement(ctx, edgeStatement,
		edge.ID, edge.FactID, edge.FromID, edge.ToID, string(edge.Kind)); err != nil {
		return err
	}
	if !derived {
		return nil
	}
	return writer.AddStatement(ctx, slimEdgesEvidenceInsertStatement,
		edge.ID, edge.Producer, edge.Location.Path,
		int64(edge.Location.Line), int64(edge.Location.Column), int64(edge.Location.EndLine),
		graph.MarshalProperties(edge.Properties))
}

// slimEdgesHydrateEdge scans the joined adjacency shape: the five edge columns
// then the seven evidence columns the hydration statements coalesce from the
// derived override or the fact.
func slimEdgesHydrateEdge(rows *sql.Rows) ([]graph.Edge, error) {
	defer func() { _ = rows.Close() }()
	result := []graph.Edge{}
	for rows.Next() {
		var edge graph.Edge
		var kind string
		var properties string
		if err := rows.Scan(&edge.ID, &edge.FactID, &edge.FromID, &edge.ToID, &kind,
			&edge.Producer, &edge.Location.Path, &edge.Location.Line, &edge.Location.Column,
			&edge.Location.EndLine, &properties); err != nil {
			return nil, err
		}
		edge.Kind = graph.EdgeKind(kind)
		edge.Properties = graph.UnmarshalProperties(properties)
		result = append(result, edge)
	}
	return result, rows.Err()
}

// slimEdgesDeleteEvidence mirrors a scoped edge deletion into the derived
// evidence table inside the same transaction, with the same bound arguments.
func slimEdgesDeleteEvidence(ctx context.Context, executor EdgeDeleteExecutor, scope EdgeDeleteScope, arguments []any) error {
	switch scope {
	case EdgeDeleteDirtyFactBatch:
		return executor.ExecStatement(ctx, slimEdgesEvidenceDeleteByDirtyFactBatchStatement, arguments...)
	case EdgeDeleteOwnerFacts:
		return executor.ExecStatement(ctx, slimEdgesEvidenceDeleteByOwnerFactsStatement, arguments...)
	default:
		return fmt.Errorf("slim edges layout cannot mirror edge deletion scope %q", scope)
	}
}

// statementText merges the spec overrides over the production bodies and
// fails closed on a spec that cannot run: missing migrations, an empty
// override, an integer-key layout without a key resolution statement, or a
// derived-evidence layout without an edge insert hook.
func (spec LayoutSpec) statementText() (map[string]string, error) {
	if spec.Migrations == nil {
		return nil, fmt.Errorf("layout %q has no migrations", spec.Name)
	}
	text := make(map[string]string, len(spec.SQL)+64)
	for _, query := range ProductionPlanQueries() {
		text[query.Name] = query.SQL
	}
	for name, statement := range spec.SQL {
		if strings.TrimSpace(statement) == "" {
			return nil, fmt.Errorf("layout %q overrides statement %s with empty SQL", spec.Name, name)
		}
		text[name] = statement
	}
	if spec.IntegerKeys {
		if _, found := text[ResolveNodeKeyStatementName]; !found {
			return nil, fmt.Errorf("layout %q uses integer keys but defines no %s statement",
				spec.Name, ResolveNodeKeyStatementName)
		}
	}
	if spec.DerivedEvidence && spec.EdgeInsert == nil {
		return nil, fmt.Errorf("layout %q separates derived evidence but provides no EdgeInsert hook", spec.Name)
	}
	return text, nil
}
