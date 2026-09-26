-- name: MarkDirtyOwner :exec
INSERT OR IGNORE INTO dirty_owners(owner_file) VALUES (?);

-- name: MarkDirtyNode :exec
INSERT OR IGNORE INTO dirty_nodes(node_id) VALUES (?);

-- name: MarkDirtyTarget :exec
INSERT OR IGNORE INTO dirty_targets(target, target_kind) VALUES (?, ?);

-- name: MarkOwnedNodesDirty :exec
INSERT OR IGNORE INTO dirty_nodes(node_id)
SELECT nodes.id FROM nodes WHERE nodes.owner_file = ?;

-- name: MarkOwnedNamesDirty :exec
INSERT OR IGNORE INTO dirty_targets(target, target_kind)
SELECT nodes.name, nodes.kind FROM nodes WHERE nodes.owner_file = ?
UNION
SELECT nodes.qualified_name, nodes.kind FROM nodes WHERE nodes.owner_file = ?;

-- name: ClearDirtyOwners :exec
DELETE FROM dirty_owners;

-- name: ClearDirtyNodes :exec
DELETE FROM dirty_nodes;

-- name: ClearDirtyTargets :exec
DELETE FROM dirty_targets;
