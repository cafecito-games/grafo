-- name: ListSemanticCandidateNodes :many
SELECT * FROM nodes
WHERE external = 0
  AND kind IN ('function', 'method', 'type', 'class', 'interface', 'endpoint')
ORDER BY qualified_name, id;

-- name: ListEmbeddingsByModel :many
SELECT * FROM embeddings WHERE model = ? ORDER BY node_id;

-- name: ListEmbeddingHashesByModel :many
SELECT node_id, content_hash FROM embeddings WHERE model = ? ORDER BY node_id;

-- name: UpsertEmbedding :exec
INSERT INTO embeddings(node_id, model, content_hash, dimensions, vector_json, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id, model) DO UPDATE SET
    content_hash = excluded.content_hash,
    dimensions = excluded.dimensions,
    vector_json = excluded.vector_json,
    updated_at = excluded.updated_at;

-- name: DeleteStaleEmbeddings :execrows
DELETE FROM embeddings
WHERE model = ? AND node_id NOT IN (
    SELECT id FROM nodes
    WHERE external = 0
      AND kind IN ('function', 'method', 'type', 'class', 'interface', 'endpoint')
);
