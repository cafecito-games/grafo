-- name: UpsertFact :exec
INSERT INTO facts(
    id, from_id, source, source_kind, kind, producer, target_id, target, target_kind,
    path_id, line, column_no, end_line, properties, owner_path_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    from_id = excluded.from_id,
    source = excluded.source,
    source_kind = excluded.source_kind,
    kind = excluded.kind,
    producer = excluded.producer,
    target_id = excluded.target_id,
    target = excluded.target,
    target_kind = excluded.target_kind,
    path_id = excluded.path_id,
    line = excluded.line,
    column_no = excluded.column_no,
    end_line = excluded.end_line,
    properties = excluded.properties,
    owner_path_id = excluded.owner_path_id;

-- name: DeleteFactsByOwner :exec
DELETE FROM facts WHERE owner_path_id = (SELECT id FROM paths WHERE path = ?);

-- name: CountFacts :one
SELECT COUNT(*) FROM facts;

-- name: ListDirtyFactBatch :many
SELECT facts.id, facts.from_id, facts.source, facts.source_kind, facts.kind, facts.producer,
       facts.target_id, facts.target, facts.target_kind,
       COALESCE(location_paths.path, '') AS path, facts.line,
       facts.column_no, facts.end_line, facts.properties,
       COALESCE(owner_paths.path, '') AS owner_file,
       CASE WHEN location_paths.id IS NULL OR owner_paths.id IS NULL THEN 0 ELSE 1 END AS paths_resolved,
       CASE WHEN facts.from_id = '' OR source_node.id IS NOT NULL THEN 1 ELSE 0 END AS source_exists,
       CASE WHEN facts.target_id = '' OR target_node.id IS NOT NULL THEN 1 ELSE 0 END AS target_exists
FROM dirty_facts INDEXED BY dirty_facts_order
CROSS JOIN facts
LEFT JOIN paths AS location_paths ON location_paths.id = facts.path_id
LEFT JOIN paths AS owner_paths ON owner_paths.id = facts.owner_path_id
LEFT JOIN nodes AS source_node ON source_node.id = facts.from_id AND source_node.external = 0
LEFT JOIN nodes AS target_node ON target_node.id = facts.target_id
WHERE facts.id = dirty_facts.fact_id
ORDER BY dirty_facts.owner_file, dirty_facts.fact_id
LIMIT ?;
