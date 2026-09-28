-- name: ListSemanticCandidateNodes :many
SELECT * FROM nodes
WHERE external = 0
  AND kind IN ('function', 'method', 'type', 'class', 'interface', 'endpoint')
ORDER BY qualified_name, id;
