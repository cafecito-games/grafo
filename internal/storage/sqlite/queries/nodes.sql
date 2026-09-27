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
SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_qualified_resolve
WHERE nodes.qualified_name = @target AND nodes.external = 0
UNION ALL
SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_name_resolve
WHERE nodes.name = @target AND nodes.external = 0 AND nodes.qualified_name != @target;

-- name: FindNodesExactKind :many
SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_qualified_resolve
WHERE nodes.qualified_name = @target AND nodes.external = 0 AND nodes.kind = @kind
UNION ALL
SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_name_resolve
WHERE nodes.name = @target AND nodes.external = 0 AND nodes.kind = @kind AND nodes.qualified_name != @target;

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

-- name: ListNodesByKind :many
SELECT * FROM nodes
WHERE kind = @kind
  AND external >= @min_external
  AND external <= @max_external
  AND (instr(lower(name), @name_fragment) > 0 OR instr(lower(qualified_name), @name_fragment) > 0)
ORDER BY qualified_name, id
LIMIT @max_results;
