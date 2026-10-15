-- name: GetUser :one
SELECT sqlc.embed(u)
FROM users AS u
WHERE u.id = sqlc.arg(id);
