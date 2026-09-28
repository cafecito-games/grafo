-- name: UpsertNode :exec
INSERT INTO nodes(
    id, kind, name, qualified_name, language, path, line, column_no, end_line,
    properties, owner_file, external, name_folded, qualified_name_folded
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
    external = excluded.external,
    name_folded = excluded.name_folded,
    qualified_name_folded = excluded.qualified_name_folded;

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

-- name: ListExternalNodesMatching :many
SELECT * FROM nodes
WHERE external = 1
  AND (
      qualified_name = @qualified_name
      OR qualified_name = @name
      OR name = @qualified_name
      OR name = @name
  )
ORDER BY id;

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
WHERE name_folded LIKE '%' || CAST(@term AS TEXT) || '%'
   OR qualified_name_folded LIKE '%' || CAST(@term AS TEXT) || '%'
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
  AND (instr(name_folded, @name_fragment) > 0 OR instr(qualified_name_folded, @name_fragment) > 0)
ORDER BY qualified_name, id
LIMIT @max_results;

-- name: ListCanonicalMessages :many
SELECT * FROM nodes
WHERE kind = 'type'
  AND external = 0
  AND json_extract(properties, '$.declaration') = 'message'
  AND (
      CAST(@package_name AS TEXT) = ''
      OR substr(qualified_name, 1, length(CAST(@package_name AS TEXT)) + 1) = CAST(@package_name AS TEXT) || '.'
  )
  AND (
      CAST(@message_name AS TEXT) = ''
      OR name = CAST(@message_name AS TEXT)
      OR qualified_name = CAST(@message_name AS TEXT)
  )
ORDER BY qualified_name, id
LIMIT @max_results;

-- Selector resolution queries. Each level is narrow and index-backed so
-- ambiguity is decided from complete counts instead of a truncated substring
-- window. nodes_qualified and nodes_name are COLLATE NOCASE indexes, so the
-- case-insensitive equality below stays an index seek; the case-sensitive
-- ("strict") form is counted separately because it is stronger evidence.
-- @external selects the scope rather than discarding one: the adapter asks for
-- local declarations first and falls back to external boundary nodes, so exact
-- evidence about an external target is still exact rather than being pushed down
-- into the weaker substring level.

-- name: MatchNodesByQualifiedName :many
SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM (
    SELECT nodes.*, CASE WHEN qualified_name = @target THEN 0 ELSE 1 END AS strict_rank
    FROM nodes
    WHERE qualified_name = @target COLLATE NOCASE
      AND external = CAST(@external AS INTEGER)
      AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
)
ORDER BY strict_rank, length(qualified_name), qualified_name, id
LIMIT @max_results;

-- name: CountNodeMatchesByQualifiedName :one
SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN qualified_name = @target THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE qualified_name = @target COLLATE NOCASE
  AND external = CAST(@external AS INTEGER)
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT));

-- name: MatchNodesByName :many
SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM (
    SELECT nodes.*, CASE WHEN name = @target THEN 0 ELSE 1 END AS strict_rank
    FROM nodes
    WHERE name = @target COLLATE NOCASE
      AND external = CAST(@external AS INTEGER)
      AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
)
ORDER BY strict_rank, length(qualified_name), qualified_name, id
LIMIT @max_results;

-- name: CountNodeMatchesByName :one
SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN name = @target THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE name = @target COLLATE NOCASE
  AND external = CAST(@external AS INTEGER)
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT));

-- name: MatchNodesBySubstring :many
SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM (
    SELECT nodes.*,
        CASE WHEN instr(qualified_name, @target) > 0 OR instr(name, @target) > 0 THEN 0 ELSE 1 END AS strict_rank
    FROM nodes
    WHERE (lower(name) LIKE '%' || lower(@target) || '%'
        OR lower(qualified_name) LIKE '%' || lower(@target) || '%')
      AND external = CAST(@external AS INTEGER)
      AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
)
ORDER BY strict_rank, length(qualified_name), qualified_name, id
LIMIT @max_results;

-- name: CountNodeMatchesBySubstring :one
SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN instr(qualified_name, @target) > 0 OR instr(name, @target) > 0 THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE (lower(name) LIKE '%' || lower(@target) || '%'
    OR lower(qualified_name) LIKE '%' || lower(@target) || '%')
  AND external = CAST(@external AS INTEGER)
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT));
