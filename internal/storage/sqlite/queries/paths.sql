-- name: InternPath :one
INSERT INTO paths(path) VALUES (?)
ON CONFLICT(path) DO UPDATE SET path = excluded.path
RETURNING id;

-- name: DeleteUnreferencedPaths :exec
DELETE FROM paths
WHERE NOT EXISTS (SELECT 1 FROM facts INDEXED BY facts_path WHERE facts.path_id = paths.id)
  AND NOT EXISTS (SELECT 1 FROM facts INDEXED BY facts_owner WHERE facts.owner_path_id = paths.id);

-- name: CountPaths :one
SELECT COUNT(*) FROM paths;
