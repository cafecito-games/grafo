-- name: DeleteAllEdges :exec
DELETE FROM edges;

-- name: DeleteEdgesByDirtyFactBatch :exec
DELETE FROM edges
WHERE fact_id IN (
    SELECT fact_id
    FROM dirty_facts INDEXED BY dirty_facts_order
    ORDER BY owner_file, fact_id
    LIMIT ?
);

-- name: DeleteEdgesByOwnerFacts :exec
DELETE FROM edges WHERE fact_id IN (SELECT id FROM facts WHERE owner_file = ?);

-- name: InsertEdge :exec
INSERT INTO edges(
    id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListEdgesFrom :many
SELECT * FROM edges WHERE from_id = ? ORDER BY kind, to_id, id;

-- name: ListEdgesTo :many
SELECT * FROM edges WHERE to_id = ? ORDER BY kind, from_id, id;

-- name: ListIncomingRelationEdges :many
SELECT
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
WHERE edges.to_id = @subject_id AND edges.kind = @relation
ORDER BY edges.from_id, edges.id
LIMIT @max_results;

-- name: ListOutgoingRelationEdges :many
SELECT
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
WHERE edges.from_id = @subject_id AND edges.kind = @relation
ORDER BY edges.to_id, edges.id
LIMIT @max_results;

-- name: ListExternalEdgesMatching :many
SELECT edges.*
FROM edges
JOIN nodes ON nodes.id = edges.to_id
WHERE nodes.external = 1
  AND (
      nodes.qualified_name = @qualified_name
      OR nodes.qualified_name = @name
      OR nodes.name = @qualified_name
      OR nodes.name = @name
  )
ORDER BY edges.kind, edges.from_id, edges.id;

-- name: ListExternalRequestEdges :many
SELECT
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
    source.id AS source_id,
    source.kind AS source_kind,
    source.name AS source_name,
    source.qualified_name AS source_qualified_name,
    source.language AS source_language,
    source.path AS source_path,
    source.line AS source_line,
    source.column_no AS source_column_no,
    source.end_line AS source_end_line,
    source.properties AS source_properties,
    source.owner_file AS source_owner_file,
    source.external AS source_external,
    target.id AS target_id,
    target.kind AS target_kind,
    target.name AS target_name,
    target.qualified_name AS target_qualified_name,
    target.language AS target_language,
    target.path AS target_path,
    target.line AS target_line,
    target.column_no AS target_column_no,
    target.end_line AS target_end_line,
    target.properties AS target_properties,
    target.owner_file AS target_owner_file,
    target.external AS target_external
FROM edges
JOIN nodes AS source ON source.id = edges.from_id
JOIN nodes AS target ON target.id = edges.to_id
WHERE edges.kind = 'requests'
  AND target.external = 1
  AND edges.id > @after_id
ORDER BY edges.id
LIMIT @max_results;

-- name: CountEdges :one
SELECT COUNT(*) FROM edges;

-- name: CountEdgesByKind :many
SELECT kind, COUNT(*) AS count FROM edges GROUP BY kind ORDER BY kind;
