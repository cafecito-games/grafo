package layoutbench

import (
	"context"
	"embed"
	"io/fs"

	"github.com/cafecito-games/grafo/internal/graph"
)

// integerKeysEmbedded holds the integer-keys candidate's Goose migrations.
// The files mirror the production migration set with the same version
// numbers, so Goose treats migration as a no-op on a version-matched
// database; a database whose schema is otherwise incompatible (say one
// seeded by production, whose nodes table has no node_key) fails closed at
// statement preparation.
//
//go:embed variants/integerkeys/*.sql
var integerKeysEmbedded embed.FS

// integerKeysMigrations is the migration filesystem OpenVariant hands to
// Goose.
var integerKeysMigrations = func() fs.FS {
	migrations, err := fs.Sub(integerKeysEmbedded, "variants/integerkeys")
	if err != nil {
		// The embedded directory is fixed at compile time, so a failed sub
		// walk means the embed directive and this path disagree.
		panic("layoutbench: embed integer-keys migrations: " + err.Error())
	}
	return migrations
}()

// integerKeysStatements overrides the production statements whose shape the
// integer surrogates change: node hydration pins an explicit column list
// (node_key leads the table), adjacency and the relation queries seek the
// integer indexes through the surrogate the adapter resolves once per
// lookup, the edge write resolves both surrogates at write time, and the
// dirty-node enqueue arms match a fact whose stored key is either the dirty
// node's surrogate or 0 (unresolved or retired) paired with the textual id.
var integerKeysStatements = map[string]string{
	ResolveNodeKeyStatementName: `SELECT node_key FROM nodes WHERE id = ?;`,
	"InsertEdge": `INSERT INTO edges(
    id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties, from_key, to_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
    COALESCE((SELECT node_key FROM nodes WHERE nodes.id = ?), 0),
    COALESCE((SELECT node_key FROM nodes WHERE nodes.id = ?), 0));`,
	"GetNode": `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM nodes WHERE id = ?;`,
	"ListExternalNodesMatching": `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM nodes
WHERE external = 1
  AND (
      qualified_name = @qualified_name
      OR qualified_name = @name
      OR name = @qualified_name
      OR name = @name
  )
ORDER BY id;`,
	"SearchNodes": `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM nodes
WHERE name_folded LIKE '%' || CAST(@term AS TEXT) || '%'
   OR qualified_name_folded LIKE '%' || CAST(@term AS TEXT) || '%'
ORDER BY
    external, length(qualified_name), qualified_name, id
LIMIT @max_results;`,
	"ListNodesByKind": `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM nodes
WHERE kind = @kind
  AND external >= @min_external
  AND external <= @max_external
  AND (instr(name_folded, @name_fragment) > 0 OR instr(qualified_name_folded, @name_fragment) > 0)
ORDER BY qualified_name, id
LIMIT @max_results;`,
	"ListSemanticCandidateNodes": `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM nodes
WHERE external = 0
  AND kind IN ('function', 'method', 'type', 'class', 'interface', 'endpoint')
ORDER BY qualified_name, id;`,
	"ListEdgesFrom": `SELECT id, fact_id, from_id, to_id, kind, path, line, column_no, end_line, properties, producer
FROM edges WHERE from_key = ? ORDER BY kind, to_id, id;`,
	"ListEdgesTo": `SELECT id, fact_id, from_id, to_id, kind, path, line, column_no, end_line, properties, producer
FROM edges WHERE to_key = ? ORDER BY kind, from_id, id;`,
	"ListIncomingRelationEdges": `SELECT
    edges.id AS edge_id,
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    edges.producer AS edge_producer,
    edges.path AS edge_path,
    edges.line AS edge_line,
    edges.column_no AS edge_column_no,
    edges.end_line AS edge_end_line,
    edges.properties AS edge_properties,
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
LEFT JOIN nodes ON nodes.id = edges.from_id
WHERE edges.to_key = @subject_id AND edges.kind = @relation
ORDER BY edges.from_id, edges.id
LIMIT @max_results;`,
	"ListOutgoingRelationEdges": `SELECT
    edges.id AS edge_id,
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    edges.producer AS edge_producer,
    edges.path AS edge_path,
    edges.line AS edge_line,
    edges.column_no AS edge_column_no,
    edges.end_line AS edge_end_line,
    edges.properties AS edge_properties,
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
LEFT JOIN nodes ON nodes.id = edges.to_id
WHERE edges.from_key = @subject_id AND edges.kind = @relation
ORDER BY edges.to_id, edges.id
LIMIT @max_results;`,
	"ListExternalEdgesMatching": `SELECT edges.id, edges.fact_id, edges.from_id, edges.to_id, edges.kind,
       edges.path, edges.line, edges.column_no, edges.end_line, edges.properties, edges.producer
FROM edges
JOIN nodes ON nodes.node_key = edges.to_key
WHERE nodes.external = 1
  AND (
      nodes.qualified_name = @qualified_name
      OR nodes.qualified_name = @name
      OR nodes.name = @qualified_name
      OR nodes.name = @name
  )
ORDER BY edges.kind, edges.from_id, edges.id;`,
	"DeleteOrphanExternalNodes": `DELETE FROM nodes
WHERE external = 1
  AND NOT EXISTS (SELECT 1 FROM edges WHERE edges.to_key = nodes.node_key OR edges.from_key = nodes.node_key);`,
	"EnqueueDirtyFacts": `INSERT OR IGNORE INTO dirty_facts(fact_id, owner_file)
SELECT facts.id, facts.owner_file
FROM dirty_owners
CROSS JOIN facts INDEXED BY facts_owner
WHERE facts.owner_file = dirty_owners.owner_file
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_nodes
CROSS JOIN facts INDEXED BY facts_from_id
WHERE (facts.from_key = dirty_nodes.node_key
       OR (facts.from_key = 0 AND facts.from_id = dirty_nodes.node_id))
  AND (
    NOT EXISTS (
      SELECT 1 FROM nodes
      WHERE nodes.id = facts.from_id AND nodes.external = 0
    )
    OR NOT EXISTS (
      SELECT 1 FROM edges
      WHERE edges.fact_id = facts.id AND edges.from_id = facts.from_id
    )
  )
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_nodes
CROSS JOIN facts INDEXED BY facts_target_id
WHERE facts.target_key = dirty_nodes.node_key
   OR (facts.target_key = 0 AND facts.target_id = dirty_nodes.node_id)
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_targets
CROSS JOIN facts INDEXED BY facts_source
WHERE facts.source = dirty_targets.target
  AND (facts.source_kind = '' OR facts.source_kind = dirty_targets.target_kind)
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_targets
CROSS JOIN facts INDEXED BY facts_target
WHERE facts.target = dirty_targets.target
  AND (facts.target_kind = '' OR facts.target_kind = dirty_targets.target_kind);`,
}

// integerKeysInsertEdge writes one edge row through the bounded batch with
// both surrogates resolved from the textual endpoints at write time (0 when
// an endpoint node does not exist; reconciliation re-derives the edge and
// re-resolves).
func integerKeysInsertEdge(ctx context.Context, writer StatementWriter, edge graph.Edge, derived bool) error {
	values := edgeValues(edge)
	values = append(values, edge.FromID, edge.ToID)
	return writer.AddStatement(ctx, edgeStatement, values...)
}
