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

-- name: ListDirtyFactBatch :many
SELECT facts.*, CASE WHEN facts.target_id = '' OR target_node.id IS NOT NULL THEN 1 ELSE 0 END AS target_exists
FROM dirty_facts INDEXED BY dirty_facts_order
CROSS JOIN facts
LEFT JOIN nodes AS target_node ON target_node.id = facts.target_id
WHERE facts.id = dirty_facts.fact_id
ORDER BY dirty_facts.owner_file, dirty_facts.fact_id
LIMIT ?;
