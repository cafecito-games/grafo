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

-- name: PruneDirtyFacts :exec
DELETE FROM dirty_facts
WHERE NOT EXISTS (SELECT 1 FROM facts WHERE facts.id = dirty_facts.fact_id);

-- name: EnqueueDirtyFacts :exec
INSERT OR IGNORE INTO dirty_facts(fact_id, owner_file)
SELECT facts.id, facts.owner_file
FROM dirty_owners
CROSS JOIN facts INDEXED BY facts_owner
WHERE facts.owner_file = dirty_owners.owner_file
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_nodes
CROSS JOIN facts INDEXED BY facts_from_id
WHERE facts.from_id = dirty_nodes.node_id
  AND (
    NOT EXISTS (
      SELECT 1 FROM nodes
      WHERE nodes.id = facts.from_id AND nodes.external = 0
    )
    OR NOT EXISTS (
      SELECT 1 FROM edges
      WHERE edges.fact_id = facts.id AND edges.from_id = facts.from_id
    )
  )
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_nodes
CROSS JOIN facts INDEXED BY facts_target_id
WHERE facts.target_id = dirty_nodes.node_id
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_targets
CROSS JOIN facts INDEXED BY facts_source
WHERE facts.source = dirty_targets.target
  AND (facts.source_kind = '' OR facts.source_kind = dirty_targets.target_kind)
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_targets
CROSS JOIN facts INDEXED BY facts_target
WHERE facts.target = dirty_targets.target
  AND (facts.target_kind = '' OR facts.target_kind = dirty_targets.target_kind);

-- name: DeleteDirtyFactBatch :exec
DELETE FROM dirty_facts
WHERE fact_id IN (
    SELECT fact_id
    FROM dirty_facts INDEXED BY dirty_facts_order
    ORDER BY owner_file, fact_id
    LIMIT ?
);

-- name: CountDirtyFacts :one
SELECT COUNT(*) FROM dirty_facts;

-- name: ReconciliationPending :one
SELECT EXISTS (
    SELECT 1 FROM dirty_owners
    UNION ALL SELECT 1 FROM dirty_nodes
    UNION ALL SELECT 1 FROM dirty_targets
    UNION ALL SELECT 1 FROM dirty_facts
    UNION ALL SELECT 1 FROM reconciliation_cleanup
);

-- name: MarkReconciliationCleanup :exec
INSERT OR IGNORE INTO reconciliation_cleanup(id)
SELECT 1 WHERE EXISTS (SELECT 1 FROM dirty_facts);

-- name: ReconciliationCleanupPending :one
SELECT EXISTS (SELECT 1 FROM reconciliation_cleanup);

-- name: ClearReconciliationCleanup :exec
DELETE FROM reconciliation_cleanup;

-- name: ClearDirtyOwners :exec
DELETE FROM dirty_owners;

-- name: ClearDirtyNodes :exec
DELETE FROM dirty_nodes;

-- name: ClearDirtyTargets :exec
DELETE FROM dirty_targets;
