-- name: DeleteAllEdges :exec
DELETE FROM edges;

-- name: DeleteEdgesByFact :exec
DELETE FROM edges WHERE fact_id = ?;

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

-- name: CountEdges :one
SELECT COUNT(*) FROM edges;

-- name: CountEdgesByKind :many
SELECT kind, COUNT(*) AS count FROM edges GROUP BY kind ORDER BY kind;
