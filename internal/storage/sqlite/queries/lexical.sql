-- name: LexicalDirtyNodes :many
SELECT nodes.* FROM lexical_dirty JOIN nodes ON nodes.id=lexical_dirty.node_id ORDER BY lexical_dirty.node_id LIMIT ?;
-- name: LexicalClearDirty :exec
DELETE FROM lexical_dirty WHERE node_id=?;
-- name: LexicalDeletePostings :exec
DELETE FROM lexical_postings WHERE node_id=?;
-- name: LexicalInsertPosting :exec
INSERT OR IGNORE INTO lexical_postings(term,node_id) VALUES (?,?);
-- name: LexicalPopulation :one
SELECT population FROM lexical_terms WHERE term=?;
-- name: LexicalPostings :many
WITH visited AS (SELECT node_id FROM lexical_postings WHERE term=? ORDER BY node_id LIMIT ?)
SELECT nodes.id, nodes.name, nodes.qualified_name, nodes.path, nodes.language, nodes.kind
FROM visited CROSS JOIN nodes ON nodes.id=visited.node_id;
-- name: LexicalPending :one
SELECT EXISTS(SELECT 1 FROM lexical_dirty) AS pending_count;
-- name: LexicalFlags :one
WITH file_candidates AS (
 SELECT nodes.id, nodes.properties FROM nodes WHERE nodes.path=@path AND nodes.kind='file' ORDER BY nodes.id LIMIT 2
), parent_candidates AS (
 SELECT edges.from_id FROM edges WHERE edges.to_id=@id AND edges.kind IN ('contains','declares','has_field') ORDER BY edges.kind,edges.from_id LIMIT 33
), component_candidates AS (
 SELECT e.from_id FROM file_candidates f CROSS JOIN edges e INDEXED BY edges_to ON e.to_id=f.id AND e.kind='contains' LIMIT 33
)
SELECT
 EXISTS(SELECT 1 FROM file_candidates WHERE json_extract(properties,'$.generated')='true') AS is_generated,
 EXISTS(SELECT 1 FROM parent_candidates e CROSS JOIN nodes p ON p.id=e.from_id WHERE p.kind='interface') AS interface_member,
 CASE WHEN CAST(@component AS TEXT)='' THEN 1 ELSE EXISTS(
 SELECT 1 FROM component_candidates e CROSS JOIN nodes c ON c.id=e.from_id
 WHERE c.kind='component' AND (c.name=@component OR c.qualified_name=@component OR c.id=@component_id)) END AS in_component,
 (SELECT count(*) FROM file_candidates)>=2 OR (SELECT count(*) FROM parent_candidates)>=33 OR (SELECT count(*) FROM component_candidates)>=33 AS lookup_truncated;
