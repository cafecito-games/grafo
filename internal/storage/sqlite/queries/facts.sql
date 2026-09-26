-- name: UpsertFact :exec
INSERT INTO facts(
    id, from_id, kind, target_id, target, target_kind, path, line, column_no,
    end_line, properties, owner_file
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    from_id = excluded.from_id,
    kind = excluded.kind,
    target_id = excluded.target_id,
    target = excluded.target,
    target_kind = excluded.target_kind,
    path = excluded.path,
    line = excluded.line,
    column_no = excluded.column_no,
    end_line = excluded.end_line,
    properties = excluded.properties,
    owner_file = excluded.owner_file;

-- name: DeleteFactsByOwner :exec
DELETE FROM facts WHERE owner_file = ?;

-- name: ListFacts :many
SELECT * FROM facts ORDER BY owner_file, id;

-- name: ListDirtyFacts :many
SELECT DISTINCT facts.*
FROM facts
WHERE owner_file IN (SELECT owner_file FROM dirty_owners)
   OR target_id IN (SELECT node_id FROM dirty_nodes)
   OR target IN (SELECT target FROM dirty_targets)
ORDER BY owner_file, id;
