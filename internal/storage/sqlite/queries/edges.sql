-- An edge stores only what reconciliation can resolve differently from its
-- originating fact: the resolved endpoints, the relation kind (a structural
-- test edge is derived from a calls or references fact), and the properties
-- (destination evidence and test-coverage evidence are added per edge). Its
-- producer and location are the fact's, so they are joined back instead of
-- stored twice. origin_resolved lets the adapter fail closed on an index whose
-- edge has lost its fact or interned path rather than report an empty location.

-- name: DeleteAllEdges :exec
DELETE FROM edges;

-- name: DeleteEdgesByDirtyFactBatch :exec
DELETE FROM edges
WHERE fact_id IN (
    SELECT fact_id
    FROM dirty_facts INDEXED BY dirty_facts_order
    ORDER BY owner_file, fact_id
    LIMIT ?
);

-- name: DeleteEdgesByOwnerFacts :exec
DELETE FROM edges WHERE fact_id IN (
    SELECT id FROM facts WHERE owner_path_id = (SELECT id FROM paths WHERE path = ?)
);

-- name: InsertEdge :exec
INSERT INTO edges(fact_id, from_id, to_id, kind, properties) VALUES (?, ?, ?, ?, ?);

-- name: ListEdgesFrom :many
SELECT edges.fact_id, edges.from_id, edges.to_id, edges.kind, edges.properties,
       COALESCE(facts.producer, '') AS producer,
       COALESCE(origin_paths.path, '') AS path,
       COALESCE(facts.line, 0) AS line,
       COALESCE(facts.column_no, 0) AS column_no,
       COALESCE(facts.end_line, 0) AS end_line,
       CASE WHEN facts.id IS NULL OR origin_paths.id IS NULL THEN 0 ELSE 1 END AS origin_resolved
FROM edges
LEFT JOIN facts ON facts.id = edges.fact_id
LEFT JOIN paths AS origin_paths ON origin_paths.id = facts.path_id
WHERE edges.from_id = ? ORDER BY edges.kind, edges.to_id, edges.fact_id;

-- name: ListEdgesTo :many
SELECT edges.fact_id, edges.from_id, edges.to_id, edges.kind, edges.properties,
       COALESCE(facts.producer, '') AS producer,
       COALESCE(origin_paths.path, '') AS path,
       COALESCE(facts.line, 0) AS line,
       COALESCE(facts.column_no, 0) AS column_no,
       COALESCE(facts.end_line, 0) AS end_line,
       CASE WHEN facts.id IS NULL OR origin_paths.id IS NULL THEN 0 ELSE 1 END AS origin_resolved
FROM edges
LEFT JOIN facts ON facts.id = edges.fact_id
LEFT JOIN paths AS origin_paths ON origin_paths.id = facts.path_id
WHERE edges.to_id = ? ORDER BY edges.kind, edges.from_id, edges.fact_id;

-- name: ListIncomingRelationEdges :many
SELECT
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    COALESCE(facts.producer, '') AS edge_producer,
    COALESCE(origin_paths.path, '') AS edge_path,
    COALESCE(facts.line, 0) AS edge_line,
    COALESCE(facts.column_no, 0) AS edge_column_no,
    COALESCE(facts.end_line, 0) AS edge_end_line,
    edges.properties AS edge_properties,
    CASE WHEN facts.id IS NULL OR origin_paths.id IS NULL THEN 0 ELSE 1 END AS edge_origin_resolved,
    COALESCE(nodes.id, '') AS counterpart_id,
    COALESCE(nodes.kind, '') AS counterpart_kind,
    COALESCE(nodes.name, '') AS counterpart_name,
    COALESCE(nodes.qualified_name, '') AS counterpart_qualified_name,
    COALESCE(nodes.language, '') AS counterpart_language,
    COALESCE(nodes.path, '') AS counterpart_path,
    COALESCE(nodes.line, 0) AS counterpart_line,
    COALESCE(nodes.column_no, 0) AS counterpart_column_no,
    COALESCE(nodes.end_line, 0) AS counterpart_end_line,
    COALESCE(nodes.properties, '{}') AS counterpart_properties,
    COALESCE(nodes.owner_file, '') AS counterpart_owner_file,
    COALESCE(nodes.external, 0) AS counterpart_external
FROM edges
LEFT JOIN nodes ON nodes.id = edges.from_id
LEFT JOIN facts ON facts.id = edges.fact_id
LEFT JOIN paths AS origin_paths ON origin_paths.id = facts.path_id
WHERE edges.to_id = @subject_id AND edges.kind = @relation
ORDER BY edges.from_id, edges.fact_id
LIMIT @max_results;

-- name: ListOutgoingRelationEdges :many
SELECT
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    COALESCE(facts.producer, '') AS edge_producer,
    COALESCE(origin_paths.path, '') AS edge_path,
    COALESCE(facts.line, 0) AS edge_line,
    COALESCE(facts.column_no, 0) AS edge_column_no,
    COALESCE(facts.end_line, 0) AS edge_end_line,
    edges.properties AS edge_properties,
    CASE WHEN facts.id IS NULL OR origin_paths.id IS NULL THEN 0 ELSE 1 END AS edge_origin_resolved,
    COALESCE(nodes.id, '') AS counterpart_id,
    COALESCE(nodes.kind, '') AS counterpart_kind,
    COALESCE(nodes.name, '') AS counterpart_name,
    COALESCE(nodes.qualified_name, '') AS counterpart_qualified_name,
    COALESCE(nodes.language, '') AS counterpart_language,
    COALESCE(nodes.path, '') AS counterpart_path,
    COALESCE(nodes.line, 0) AS counterpart_line,
    COALESCE(nodes.column_no, 0) AS counterpart_column_no,
    COALESCE(nodes.end_line, 0) AS counterpart_end_line,
    COALESCE(nodes.properties, '{}') AS counterpart_properties,
    COALESCE(nodes.owner_file, '') AS counterpart_owner_file,
    COALESCE(nodes.external, 0) AS counterpart_external
FROM edges
LEFT JOIN nodes ON nodes.id = edges.to_id
LEFT JOIN facts ON facts.id = edges.fact_id
LEFT JOIN paths AS origin_paths ON origin_paths.id = facts.path_id
WHERE edges.from_id = @subject_id AND edges.kind = @relation
ORDER BY edges.to_id, edges.fact_id
LIMIT @max_results;

-- name: ListExternalEdgesMatching :many
SELECT edges.fact_id, edges.from_id, edges.to_id, edges.kind, edges.properties,
       COALESCE(facts.producer, '') AS producer,
       COALESCE(origin_paths.path, '') AS path,
       COALESCE(facts.line, 0) AS line,
       COALESCE(facts.column_no, 0) AS column_no,
       COALESCE(facts.end_line, 0) AS end_line,
       CASE WHEN facts.id IS NULL OR origin_paths.id IS NULL THEN 0 ELSE 1 END AS origin_resolved
FROM edges
JOIN nodes ON nodes.id = edges.to_id
LEFT JOIN facts ON facts.id = edges.fact_id
LEFT JOIN paths AS origin_paths ON origin_paths.id = facts.path_id
WHERE nodes.external = 1
  AND (
      nodes.qualified_name = @qualified_name
      OR nodes.qualified_name = @name
      OR nodes.name = @qualified_name
      OR nodes.name = @name
  )
ORDER BY edges.kind, edges.from_id, edges.fact_id, edges.to_id;

-- name: ListExternalRequestEdges :many
SELECT
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    COALESCE(origin.producer, '') AS edge_producer,
    COALESCE(origin_paths.path, '') AS edge_path,
    COALESCE(origin.line, 0) AS edge_line,
    COALESCE(origin.column_no, 0) AS edge_column_no,
    COALESCE(origin.end_line, 0) AS edge_end_line,
    edges.properties AS edge_properties,
    CASE WHEN origin.id IS NULL OR origin_paths.id IS NULL THEN 0 ELSE 1 END AS edge_origin_resolved,
    source.id AS source_id,
    source.kind AS source_kind,
    source.name AS source_name,
    source.qualified_name AS source_qualified_name,
    source.language AS source_language,
    source.path AS source_path,
    source.line AS source_line,
    source.column_no AS source_column_no,
    source.end_line AS source_end_line,
    source.properties AS source_properties,
    source.owner_file AS source_owner_file,
    source.external AS source_external,
    target.id AS target_id,
    target.kind AS target_kind,
    target.name AS target_name,
    target.qualified_name AS target_qualified_name,
    target.language AS target_language,
    target.path AS target_path,
    target.line AS target_line,
    target.column_no AS target_column_no,
    target.end_line AS target_end_line,
    target.properties AS target_properties,
    target.owner_file AS target_owner_file,
    target.external AS target_external
FROM edges
JOIN nodes AS source ON source.id = edges.from_id
JOIN nodes AS target ON target.id = edges.to_id
LEFT JOIN facts AS origin ON origin.id = edges.fact_id
LEFT JOIN paths AS origin_paths ON origin_paths.id = origin.path_id
WHERE edges.kind = 'requests'
  AND target.external = 1
  AND (edges.fact_id, edges.to_id, edges.kind) > (@after_fact_id, @after_to_id, @after_kind)
ORDER BY edges.fact_id, edges.to_id, edges.kind
LIMIT @max_results;

-- name: CountEdges :one
SELECT COUNT(*) FROM edges;

-- name: CountEdgesByKind :many
SELECT kind, COUNT(*) AS count FROM edges GROUP BY kind ORDER BY kind;
