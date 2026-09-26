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
    id, fact_id, from_id, to_id, kind, path, line, column_no, end_line, properties
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListEdgesFrom :many
SELECT * FROM edges WHERE from_id = ? ORDER BY kind, to_id, id;

-- name: ListEdgesTo :many
SELECT * FROM edges WHERE to_id = ? ORDER BY kind, from_id, id;

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

-- name: CountEdges :one
SELECT COUNT(*) FROM edges;

-- name: CountEdgesByKind :many
SELECT kind, COUNT(*) AS count FROM edges GROUP BY kind ORDER BY kind;
