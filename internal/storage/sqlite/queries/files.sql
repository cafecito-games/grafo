-- name: ListFiles :many
SELECT path, hash, language, size, modified_ns, indexed_at FROM files ORDER BY path;

-- name: UpsertFile :exec
INSERT INTO files(path, hash, language, size, modified_ns, indexed_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
    hash = excluded.hash,
    language = excluded.language,
    size = excluded.size,
    modified_ns = excluded.modified_ns,
    indexed_at = excluded.indexed_at;

-- name: DeleteFile :exec
DELETE FROM files WHERE path = ?;

-- name: CountFiles :one
SELECT COUNT(*) FROM files;
