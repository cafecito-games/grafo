-- name: UpsertNode :exec
INSERT INTO nodes(
    id, kind, name, qualified_name, language, path, line, column_no, end_line,
    properties, owner_file, external
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    kind = excluded.kind,
    name = excluded.name,
    qualified_name = excluded.qualified_name,
    language = excluded.language,
    path = excluded.path,
    line = excluded.line,
    column_no = excluded.column_no,
    end_line = excluded.end_line,
    properties = excluded.properties,
    owner_file = excluded.owner_file,
    external = excluded.external;

-- name: DeleteNodesByOwner :exec
DELETE FROM nodes WHERE owner_file = ?;

-- name: DeleteExternalNodes :exec
DELETE FROM nodes WHERE owner_file = '__external__';

-- name: DeleteOrphanExternalNodes :exec
DELETE FROM nodes
WHERE external = 1
  AND NOT EXISTS (SELECT 1 FROM edges WHERE edges.to_id = nodes.id OR edges.from_id = nodes.id);

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: FindNodesExact :many
SELECT * FROM nodes
WHERE external = 0 AND (qualified_name = ? OR name = ?)
ORDER BY qualified_name, id;

-- name: FindNodesExactKind :many
SELECT * FROM nodes
WHERE external = 0
  AND (qualified_name = ? OR name = ?)
  AND kind = ?
ORDER BY qualified_name, id;

-- name: FindNodesByName :many
SELECT * FROM nodes
WHERE external = 0 AND name = ?
ORDER BY qualified_name, id;

-- name: FindNodesByNameKind :many
SELECT * FROM nodes
WHERE external = 0 AND name = ? AND kind = ?
ORDER BY qualified_name, id;

-- name: SearchNodes :many
SELECT * FROM nodes
WHERE lower(name) LIKE '%' || lower(@term) || '%'
   OR lower(qualified_name) LIKE '%' || lower(@term) || '%'
ORDER BY
    external, length(qualified_name), qualified_name, id
LIMIT @max_results;

-- name: CountNodes :one
SELECT COUNT(*) FROM nodes;

-- name: CountExternalNodes :one
SELECT COUNT(*) FROM nodes WHERE external = 1;

-- name: CountNodesByKind :many
SELECT kind, COUNT(*) AS count FROM nodes GROUP BY kind ORDER BY kind;
