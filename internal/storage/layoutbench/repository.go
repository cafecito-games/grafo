package layoutbench

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/semantic"
)

// VariantRepository is the shared database/sql adapter for the layout spike:
// one implementation of the full repository surface whose statements, edge
// writes, and edge reads are parameterized by a LayoutSpec. With the
// zero-delta production spec it must be exactly equivalent to the production
// adapter, which the equivalence driver proves; candidate layouts are small
// SQL/DDL deltas on this core.
//
// The method order and logic mirror internal/storage/sqlite/repository.go so
// the two stay reviewable side by side.
type VariantRepository struct {
	db            *sql.DB
	statements    *statementSet
	statementText map[string]string
	spec          LayoutSpec
	path          string
	limits        batchLimits

	writeStatsMu sync.Mutex
	writeStats   graph.WriteStats
	afterBatch   func(string, graph.WriteBatchStats)
}

const (
	reconciliationBatchSize = 10_000
	resolutionCacheSize     = 50_000
	sqliteLimitVariables    = 9
)

var _ graph.Repository = (*VariantRepository)(nil)
var _ graph.CatalogRepository = (*VariantRepository)(nil)
var _ graph.RelationEdgeRepository = (*VariantRepository)(nil)
var _ graph.ExternalEdgeRepository = (*VariantRepository)(nil)
var _ graph.ExternalNodeRepository = (*VariantRepository)(nil)
var _ graph.InstrumentedIndexRepository = (*VariantRepository)(nil)
var _ graph.InstrumentedWriteRepository = (*VariantRepository)(nil)
var _ graph.ReconciliationStatusRepository = (*VariantRepository)(nil)
var _ graph.CanonicalMessageRepository = (*VariantRepository)(nil)
var _ semantic.CandidateRepository = (*VariantRepository)(nil)

func (r *VariantRepository) Close() error { return errors.Join(r.statements.Close(), r.db.Close()) }
func (r *VariantRepository) Path() string { return r.path }

func (r *VariantRepository) WriteStats() graph.WriteStats {
	r.writeStatsMu.Lock()
	defer r.writeStatsMu.Unlock()
	return r.writeStats
}

func (r *VariantRepository) SetMeta(ctx context.Context, key, value string) error {
	_, err := r.statements.exec(ctx, "SetMeta", key, value)
	return err
}

func (r *VariantRepository) Meta(ctx context.Context, key string) (string, error) {
	var value string
	row, err := r.statements.queryRow(ctx, "GetMeta", key)
	if err != nil {
		return "", err
	}
	err = row.Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (r *VariantRepository) CandidateNodes(ctx context.Context) ([]graph.Node, error) {
	rows, err := r.statements.query(ctx, "ListSemanticCandidateNodes")
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

func (r *VariantRepository) Files(ctx context.Context) (map[string]graph.FileRecord, error) {
	rows, err := r.statements.query(ctx, "ListFiles")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string]graph.FileRecord{}
	for rows.Next() {
		var record graph.FileRecord
		if err := rows.Scan(&record.Path, &record.Hash, &record.Language,
			&record.Size, &record.ModifiedNS, &record.IndexedAt); err != nil {
			return nil, err
		}
		result[record.Path] = record
	}
	return result, rows.Err()
}

func (r *VariantRepository) ReplaceFile(ctx context.Context, file graph.FileRecord, parsed graph.ParseResult) error {
	if err := validateParseResult(parsed); err != nil {
		return err
	}
	return r.inTransaction(ctx, func(q *transactionStatements, writer *batchWriter) error {
		if err := markOwnerDirty(ctx, q, file.Path); err != nil {
			return err
		}
		if err := r.deleteEdgesByOwnerFacts(ctx, q, file.Path); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "DeleteFactsByOwner", file.Path); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "DeleteNodesByOwner", file.Path); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "UpsertFile", file.Path, file.Hash, file.Language,
			file.Size, file.ModifiedNS, file.IndexedAt); err != nil {
			return err
		}
		return insertParseResult(ctx, writer, parsed)
	})
}

func (r *VariantRepository) ReplaceOwner(ctx context.Context, owner string, parsed graph.ParseResult) error {
	if err := validateParseResult(parsed); err != nil {
		return err
	}
	return r.inTransaction(ctx, func(q *transactionStatements, writer *batchWriter) error {
		if err := markOwnerDirty(ctx, q, owner); err != nil {
			return err
		}
		if err := r.deleteEdgesByOwnerFacts(ctx, q, owner); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "DeleteFactsByOwner", owner); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "DeleteNodesByOwner", owner); err != nil {
			return err
		}
		return insertParseResult(ctx, writer, parsed)
	})
}

func validateParseResult(parsed graph.ParseResult) error {
	for index, fact := range parsed.Facts {
		if err := fact.ValidateSourceLocator(); err != nil {
			return fmt.Errorf("validate fact %d: %w", index, err)
		}
	}
	return nil
}

func (r *VariantRepository) RemoveFiles(ctx context.Context, paths []string) error {
	return r.inTransaction(ctx, func(q *transactionStatements, _ *batchWriter) error {
		for _, path := range paths {
			if err := markOwnerDirty(ctx, q, path); err != nil {
				return err
			}
			if err := r.deleteEdgesByOwnerFacts(ctx, q, path); err != nil {
				return err
			}
			if _, err := q.exec(ctx, "DeleteFactsByOwner", path); err != nil {
				return err
			}
			if _, err := q.exec(ctx, "DeleteNodesByOwner", path); err != nil {
				return err
			}
			if _, err := q.exec(ctx, "DeleteFile", path); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertParseResult(ctx context.Context, writer *batchWriter, parsed graph.ParseResult) error {
	for _, node := range parsed.Nodes {
		external := int64(0)
		if node.External {
			external = 1
		}
		if err := writer.addNode(ctx, nodeValues(node, external)); err != nil {
			return fmt.Errorf("upsert node %s: %w", node.QualifiedName, err)
		}
		if !node.External {
			if err := markNodeDirty(ctx, writer, node); err != nil {
				return err
			}
		}
	}
	for _, fact := range parsed.Facts {
		if err := writer.addFact(ctx, factValues(fact)); err != nil {
			return fmt.Errorf("upsert fact %s: %w", fact.ID, err)
		}
	}
	return nil
}

func (r *VariantRepository) Reconcile(ctx context.Context) error {
	_, err := r.ReconcileWithStats(ctx, nil)
	return err
}

// ReconciliationPending reports whether any durable resolver queue contains
// work, mirroring the production adapter's proof for the indexer.
func (r *VariantRepository) ReconciliationPending(ctx context.Context) (bool, error) {
	var pending bool
	row, err := r.statements.queryRow(ctx, "ReconciliationPending")
	if err != nil {
		return false, err
	}
	err = row.Scan(&pending)
	return pending, err
}

func (r *VariantRepository) ReconcileWithStats(ctx context.Context, observer graph.ReconciliationObserver) (graph.ReconciliationStats, error) {
	var stats graph.ReconciliationStats
	if err := r.queueDirtyFacts(ctx); err != nil {
		return stats, fmt.Errorf("queue dirty facts: %w", err)
	}

	resolved := newResolutionCache(resolutionCacheSize)
	for {
		before := r.WriteStats()
		processed, err := r.reconcileBatch(ctx, resolved)
		if err != nil {
			return stats, fmt.Errorf("reconcile fact batch: %w", err)
		}
		if processed == 0 {
			break
		}
		stats.Batches++
		addWriteStats(&stats.Writes, subtractWriteStats(r.WriteStats(), before))
		if observer != nil {
			if err := observer(stats); err != nil {
				return stats, err
			}
		}
		if err := r.checkpoint(ctx, false); err != nil {
			return stats, err
		}
	}
	cleanupPending, err := r.queryExists(ctx, "ReconciliationCleanupPending")
	if err != nil {
		return stats, fmt.Errorf("check reconciliation cleanup: %w", err)
	}
	if !cleanupPending {
		return stats, nil
	}
	if err := r.inTransaction(ctx, func(q *transactionStatements, _ *batchWriter) error {
		if _, err := q.exec(ctx, "DeleteOrphanExternalNodes"); err != nil {
			return err
		}
		_, err := q.exec(ctx, "ClearReconciliationCleanup")
		return err
	}); err != nil {
		return stats, fmt.Errorf("remove orphan external nodes: %w", err)
	}
	return stats, r.checkpoint(ctx, true)
}

func (r *VariantRepository) queueDirtyFacts(ctx context.Context) error {
	return r.inTransaction(ctx, func(q *transactionStatements, _ *batchWriter) error {
		if _, err := q.exec(ctx, "PruneDirtyFacts"); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "EnqueueDirtyFacts"); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "MarkReconciliationCleanup"); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "ClearDirtyOwners"); err != nil {
			return err
		}
		if _, err := q.exec(ctx, "ClearDirtyNodes"); err != nil {
			return err
		}
		_, err := q.exec(ctx, "ClearDirtyTargets")
		return err
	})
}

func (r *VariantRepository) reconcileBatch(ctx context.Context, resolved *resolutionCache) (int, error) {
	processed := 0
	err := r.inTransaction(ctx, func(q *transactionStatements, writer *batchWriter) error {
		rows, err := q.query(ctx, "ListDirtyFactBatch", int64(reconciliationBatchSize))
		if err != nil {
			return err
		}
		facts, err := scanDirtyFactRows(rows)
		if err != nil {
			return err
		}
		if len(facts) == 0 {
			return nil
		}
		if err := r.deleteEdgesByDirtyFactBatch(ctx, q, int64(reconciliationBatchSize)); err != nil {
			return err
		}
		nodeCache := map[string]graph.Node{}
		loadNode := func(id string) (graph.Node, error) {
			if node, ok := nodeCache[id]; ok {
				return node, nil
			}
			row, rowErr := q.queryRow(ctx, "GetNode", id)
			if rowErr != nil {
				return graph.Node{}, rowErr
			}
			node, err := scanNode(row)
			if err != nil {
				return graph.Node{}, err
			}
			nodeCache[id] = node
			return node, nil
		}
		for _, row := range facts {
			processed++
			fact := row.fact()
			sources, err := resolveSources(ctx, q, writer, fact, row.sourceExists, resolved)
			if err != nil {
				return err
			}
			targets, err := resolveTargets(ctx, q, writer, fact, row.targetExists, resolved)
			if err != nil {
				return err
			}
			for _, source := range sources {
				for _, target := range targets {
					edge := graph.Edge{ID: graph.EdgeID(fact.ID, target), FactID: fact.ID,
						FromID: source, ToID: target, Kind: fact.Kind, Producer: fact.Producer, Location: fact.Location,
						Properties: fact.Properties}
					if err := r.writeEdge(ctx, writer, edge, false); err != nil {
						return err
					}
					if edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeReferences {
						continue
					}
					sourceNode, sourceErr := loadNode(source)
					if errors.Is(sourceErr, sql.ErrNoRows) {
						continue // a newly materialized unresolved source cannot be a local test
					}
					if sourceErr != nil {
						return sourceErr
					}
					if sourceNode.Kind != graph.KindTest {
						continue
					}
					targetNode, targetErr := loadNode(target)
					if errors.Is(targetErr, sql.ErrNoRows) {
						continue // a newly materialized unresolved target is never production evidence
					}
					if targetErr != nil {
						return targetErr
					}
					if testEdge, ok := graph.DirectTestEdge(sourceNode, targetNode, edge); ok {
						if err := r.writeEdge(ctx, writer, testEdge, true); err != nil {
							return err
						}
					}
				}
			}
		}
		if err := writer.flush(ctx); err != nil {
			return err
		}
		_, err = q.exec(ctx, "DeleteDirtyFactBatch", int64(reconciliationBatchSize))
		return err
	})
	return processed, err
}

func (r *VariantRepository) checkpoint(ctx context.Context, truncate bool) error {
	statement := "PRAGMA wal_checkpoint(PASSIVE)"
	if truncate {
		statement = "PRAGMA wal_checkpoint(TRUNCATE)"
	}
	var busy, logFrames, checkpointedFrames int
	row := r.db.QueryRowContext(ctx, statement)
	if err := row.Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return fmt.Errorf("checkpoint SQLite WAL: %w", err)
	}
	return nil
}

// deleteEdgesByOwnerFacts runs the layout's post-delete hook, then the scoped
// production deletion, inside the caller's transaction.
func (r *VariantRepository) deleteEdgesByOwnerFacts(ctx context.Context, q *transactionStatements, owner string) error {
	if r.spec.PostEdgeDelete != nil {
		if err := r.spec.PostEdgeDelete(ctx, q, EdgeDeleteOwnerFacts, []any{owner}); err != nil {
			return err
		}
	}
	_, err := q.exec(ctx, "DeleteEdgesByOwnerFacts", owner)
	return err
}

func (r *VariantRepository) deleteEdgesByDirtyFactBatch(ctx context.Context, q *transactionStatements, limit int64) error {
	if r.spec.PostEdgeDelete != nil {
		if err := r.spec.PostEdgeDelete(ctx, q, EdgeDeleteDirtyFactBatch, []any{limit}); err != nil {
			return err
		}
	}
	_, err := q.exec(ctx, "DeleteEdgesByDirtyFactBatch", limit)
	return err
}

// writeEdge routes one edge through the layout: an EdgeInsert hook owns the
// write when the spec provides one (a derived-evidence layout must), otherwise
// the default bounded batch insert.
func (r *VariantRepository) writeEdge(ctx context.Context, writer *batchWriter, edge graph.Edge, derived bool) error {
	if r.spec.EdgeInsert != nil {
		return r.spec.EdgeInsert(ctx, writer, edge, derived)
	}
	return writer.addEdge(ctx, edgeValues(edge))
}

// adjacencySubject binds the value an adjacency statement expects for one
// public node id: the id itself, or the integer surrogate an IntegerKeys
// layout resolves once per lookup.
func (r *VariantRepository) adjacencySubject(ctx context.Context, id string) (any, error) {
	if !r.spec.IntegerKeys {
		return id, nil
	}
	var surrogate int64
	row, err := r.statements.queryRow(ctx, ResolveNodeKeyStatementName, id)
	if err != nil {
		return 0, err
	}
	err = row.Scan(&surrogate)
	if errors.Is(err, sql.ErrNoRows) {
		return int64(-1), nil
	}
	if err != nil {
		return nil, err
	}
	return surrogate, nil
}

func markOwnerDirty(ctx context.Context, q *transactionStatements, owner string) error {
	if _, err := q.exec(ctx, "MarkDirtyOwner", owner); err != nil {
		return err
	}
	if _, err := q.exec(ctx, "MarkOwnedNodesDirty", owner); err != nil {
		return err
	}
	_, err := q.exec(ctx, "MarkOwnedNamesDirty", owner, owner)
	return err
}

func markNodeDirty(ctx context.Context, writer *batchWriter, node graph.Node) error {
	if err := writer.addDirtyNode(ctx, node.ID); err != nil {
		return err
	}
	for _, target := range []string{node.Name, node.QualifiedName} {
		if target == "" {
			continue
		}
		if err := writer.addDirtyTarget(ctx, target, string(node.Kind)); err != nil {
			return err
		}
	}
	return nil
}

func subtractWriteStats(after, before graph.WriteStats) graph.WriteStats {
	return graph.WriteStats{
		Nodes: subtractBatchStats(after.Nodes, before.Nodes),
		Facts: subtractBatchStats(after.Facts, before.Facts),
		Edges: subtractBatchStats(after.Edges, before.Edges),
	}
}

func subtractBatchStats(after, before graph.WriteBatchStats) graph.WriteBatchStats {
	return graph.WriteBatchStats{
		Batches: max(0, after.Batches-before.Batches),
		Rows:    max(0, after.Rows-before.Rows),
		Bytes:   max(0, after.Bytes-before.Bytes),
	}
}

type endpointDirection uint8

const (
	targetEndpoint endpointDirection = iota
	sourceEndpoint
)

type resolutionKey struct {
	direction  endpointDirection
	target     string
	targetKind graph.NodeKind
	edgeKind   graph.EdgeKind
}

type resolutionCacheEntry struct {
	key     resolutionKey
	targets []string
}

type resolutionCache struct {
	capacity int
	entries  map[resolutionKey]*list.Element
	recent   *list.List
}

func newResolutionCache(capacity int) *resolutionCache {
	return &resolutionCache{capacity: capacity, entries: make(map[resolutionKey]*list.Element, capacity), recent: list.New()}
}

func (c *resolutionCache) get(key resolutionKey) ([]string, bool) {
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.recent.MoveToFront(element)
	return element.Value.(resolutionCacheEntry).targets, true
}

func (c *resolutionCache) set(key resolutionKey, targets []string) {
	if element, ok := c.entries[key]; ok {
		element.Value = resolutionCacheEntry{key: key, targets: targets}
		c.recent.MoveToFront(element)
		return
	}
	element := c.recent.PushFront(resolutionCacheEntry{key: key, targets: targets})
	c.entries[key] = element
	if c.recent.Len() <= c.capacity {
		return
	}
	oldest := c.recent.Back()
	delete(c.entries, oldest.Value.(resolutionCacheEntry).key)
	c.recent.Remove(oldest)
}

type resolutionCandidate struct {
	id            string
	kind          graph.NodeKind
	qualifiedName string
}

func resolveSources(ctx context.Context, q *transactionStatements, writer *batchWriter, fact graph.Fact, sourceExists bool, cache *resolutionCache) ([]string, error) {
	return resolveEndpoint(ctx, q, writer, fact, sourceEndpoint, sourceExists, cache)
}

func resolveTargets(ctx context.Context, q *transactionStatements, writer *batchWriter, fact graph.Fact, targetExists bool, cache *resolutionCache) ([]string, error) {
	return resolveEndpoint(ctx, q, writer, fact, targetEndpoint, targetExists, cache)
}

func resolveEndpoint(ctx context.Context, q *transactionStatements, writer *batchWriter, fact graph.Fact, direction endpointDirection, exactExists bool, cache *resolutionCache) ([]string, error) {
	exactID, name, kind := fact.TargetID, fact.Target, fact.TargetKind
	if direction == sourceEndpoint {
		exactID, name, kind = fact.FromID, fact.Source, fact.SourceKind
	}
	if exactID != "" {
		if exactExists {
			return []string{exactID}, nil
		}
		external := externalNode(exactID, "")
		if err := writer.addNode(ctx, nodeValues(external, 1)); err != nil {
			return nil, err
		}
		return []string{external.ID}, nil
	}
	key := resolutionKey{direction: direction, target: name, targetKind: kind, edgeKind: fact.Kind}
	if targets, ok := cache.get(key); ok {
		return targets, nil
	}
	var targets []string
	var rows []resolutionCandidate
	if name != "" {
		var found *sql.Rows
		var err error
		if kind == "" {
			found, err = q.query(ctx, "FindNodesExact", name)
		} else {
			found, err = q.query(ctx, "FindNodesExactKind", name, string(kind))
		}
		if err != nil {
			return nil, err
		}
		rows, err = scanResolutionCandidates(found)
		if err != nil {
			return nil, err
		}
		rows = filterCandidates(fact, direction, rows)
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].qualifiedName == rows[j].qualifiedName {
				return rows[i].id < rows[j].id
			}
			return rows[i].qualifiedName < rows[j].qualifiedName
		})
		// A name shared by multiple declarations is not enough evidence to
		// invent a fan-out edge. Preserve one explicit unresolved endpoint and
		// let a parser provide a qualified name when it can prove the binding.
		if len(rows) == 1 {
			targets = []string{rows[0].id}
		}
	}
	if len(targets) == 0 {
		external := externalNode(name, kind)
		if err := writer.addNode(ctx, nodeValues(external, 1)); err != nil {
			return nil, err
		}
		targets = []string{external.ID}
	}
	cache.set(key, targets)
	return targets, nil
}

func filterCandidates(fact graph.Fact, direction endpointDirection, rows []resolutionCandidate) []resolutionCandidate {
	kind := fact.TargetKind
	if direction == sourceEndpoint {
		kind = fact.SourceKind
	}
	if kind != "" || direction == sourceEndpoint {
		return rows
	}
	result := rows[:0]
	for _, row := range rows {
		if graph.AllowsResolutionKind(fact.Kind, row.kind) {
			result = append(result, row)
		}
	}
	return result
}

func externalNode(qualified string, kind graph.NodeKind) graph.Node {
	if kind == "" {
		kind = graph.KindExternal
	}
	return graph.Node{ID: graph.NodeID(kind, "external:"+qualified), Kind: kind,
		Name: graph.SimpleName(qualified), QualifiedName: qualified, OwnerFile: "__external__",
		External: true, Properties: map[string]string{"unresolved": "true"}}
}

func (r *VariantRepository) Counts(ctx context.Context) (graph.Counts, error) {
	result := graph.Counts{ByKind: map[string]int{}, ByEdge: map[string]int{}}
	files, err := r.queryCount(ctx, "CountFiles")
	if err != nil {
		return result, err
	}
	nodes, err := r.queryCount(ctx, "CountNodes")
	if err != nil {
		return result, err
	}
	facts, err := r.queryCount(ctx, "CountFacts")
	if err != nil {
		return result, err
	}
	edges, err := r.queryCount(ctx, "CountEdges")
	if err != nil {
		return result, err
	}
	external, err := r.queryCount(ctx, "CountExternalNodes")
	if err != nil {
		return result, err
	}
	result.Files, result.Nodes, result.Facts, result.Edges, result.External = int(files), int(nodes), int(facts), int(edges), int(external)
	nodeKinds, err := r.statements.query(ctx, "CountNodesByKind")
	if err != nil {
		return result, err
	}
	kindCounts, err := scanKindCounts(nodeKinds)
	if err != nil {
		return result, err
	}
	maps.Copy(result.ByKind, kindCounts)
	edgeKindRows, err := r.statements.query(ctx, "CountEdgesByKind")
	if err != nil {
		return result, err
	}
	edgeCounts, err := scanKindCounts(edgeKindRows)
	if err != nil {
		return result, err
	}
	maps.Copy(result.ByEdge, edgeCounts)
	return result, nil
}

func (r *VariantRepository) queryCount(ctx context.Context, statementName string) (int64, error) {
	var count int64
	row, err := r.statements.queryRow(ctx, statementName)
	if err != nil {
		return 0, err
	}
	err = row.Scan(&count)
	return count, err
}

func (r *VariantRepository) queryExists(ctx context.Context, statementName string) (bool, error) {
	var exists bool
	row, err := r.statements.queryRow(ctx, statementName)
	if err != nil {
		return false, err
	}
	err = row.Scan(&exists)
	return exists, err
}

func (r *VariantRepository) SearchNodes(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	rows, err := r.statements.query(ctx, "SearchNodes", foldName(term), int64(limit))
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// Repositories reports the indexed repository this database stores, derived
// from the root recorded at index time.
func (r *VariantRepository) Repositories(ctx context.Context) ([]string, error) {
	name, err := r.repositoryName(ctx)
	if err != nil || name == "" {
		return []string{}, err
	}
	return []string{name}, nil
}

// ListNodesByKind enumerates nodes of the requested kinds. It only enumerates:
// kind membership, name matching, and bounds are storage concerns, while usage
// direction and orphan classification stay in the query use cases.
func (r *VariantRepository) ListNodesByKind(ctx context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	name, err := r.repositoryName(ctx)
	if err != nil {
		return nil, err
	}
	if request.Repository != "" && request.Repository != name {
		return []graph.ScopedNode{}, nil
	}
	minExternal, maxExternal := int64(0), int64(0)
	switch request.Visibility {
	case graph.ExternalNodes:
		minExternal, maxExternal = 1, 1
	case graph.AllNodes:
		minExternal, maxExternal = 0, 1
	case "", graph.LocalNodes:
	default:
		return nil, fmt.Errorf("unsupported node visibility %q", request.Visibility)
	}
	limit := int64(-1)
	if request.Limit > 0 {
		limit = int64(request.Limit)
	}
	// The fragment is matched literally, so a name containing % or _ narrows
	// the catalog instead of silently widening it.
	fragment := foldName(request.Name)
	prefixes := request.PathPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	prefixesJSON, err := json.Marshal(prefixes)
	if err != nil {
		return nil, fmt.Errorf("encode node path prefixes: %w", err)
	}
	result := []graph.ScopedNode{}
	for _, kind := range request.Kinds {
		rows, err := r.statements.query(ctx, "ListNodesByKind", string(kind),
			minExternal, maxExternal, fragment, string(prefixesJSON), limit)
		if err != nil {
			return nil, err
		}
		nodes, err := scanNodes(rows)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			result = append(result, graph.ScopedNode{Repository: name, Node: node})
		}
	}
	return result, nil
}

// CanonicalMessages enumerates authoritative protocol-message declarations.
// Filtering happens in SQLite before the deterministic order and bound, so
// unrelated type declarations cannot hide or truncate message coverage.
func (r *VariantRepository) CanonicalMessages(ctx context.Context, request graph.CanonicalMessageQuery) (graph.CanonicalMessagePage, error) {
	if err := request.Validate(); err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	name, err := r.repositoryName(ctx)
	if err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	if request.Repository != "" && request.Repository != name {
		return graph.CanonicalMessagePage{Items: []graph.ScopedNode{}}, nil
	}
	if request.Limit == int(^uint(0)>>1) {
		return graph.CanonicalMessagePage{}, fmt.Errorf("canonical message limit is too large")
	}
	prefixes := request.PathPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	prefixesJSON, err := json.Marshal(prefixes)
	if err != nil {
		return graph.CanonicalMessagePage{}, fmt.Errorf("encode canonical message path prefixes: %w", err)
	}
	rows, err := r.statements.query(ctx, "ListCanonicalMessages",
		strings.TrimSuffix(request.Package, "."), request.Message, string(prefixesJSON),
		int64(request.Limit)+1)
	if err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	nodes, err := scanNodes(rows)
	if err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	page := graph.CanonicalMessagePage{Items: make([]graph.ScopedNode, 0, min(len(nodes), request.Limit))}
	if len(nodes) > request.Limit {
		page.Truncated = true
		nodes = nodes[:request.Limit]
	}
	for _, node := range nodes {
		page.Items = append(page.Items, graph.ScopedNode{Repository: name, Node: node})
	}
	return page, nil
}

func (r *VariantRepository) repositoryName(ctx context.Context) (string, error) {
	root, err := r.Meta(ctx, "root")
	if err != nil || root == "" {
		return "", err
	}
	return filepath.Base(root), nil
}

// DefaultNodeMatchLimit bounds the candidate list MatchNodes returns when a
// caller does not ask for a different size. Totals are never bounded by it.
const DefaultNodeMatchLimit = 25

// matchScope pairs a match level with the node scope it is evaluated over.
// Scopes are ordered so a local declaration always outranks an external boundary
// node at the same level, and exact evidence about an external target is still
// exact rather than being pushed down into the substring level.
type matchScope struct {
	level    graph.NodeMatchLevel
	external bool
}

var matchScopes = []matchScope{
	{graph.MatchQualifiedName, false},
	{graph.MatchQualifiedName, true},
	{graph.MatchName, false},
	{graph.MatchName, true},
	{graph.MatchSubstring, false},
	{graph.MatchSubstring, true},
}

// MatchNodes resolves a selector against the narrow, index-backed match
// queries. Scopes are tried strongest first and the first one holding a
// case-sensitive match wins, so a substring match never competes with an exact
// name match, an unresolved boundary node never competes with a real declaration,
// and a case-insensitive-only hit at a strong level never outranks a
// case-sensitive match at a weaker one. A scope with only case-insensitive matches
// is kept as the fallback for selectors that match nothing case-sensitively. The
// count query runs before the row query so the reported totals cover the whole
// graph even when the row list is truncated.
func (r *VariantRepository) MatchNodes(ctx context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
	if request.Repository != "" {
		name, err := r.repositoryName(ctx)
		if err != nil {
			return graph.NodeMatchGroup{}, err
		}
		if request.Repository != name {
			return graph.NodeMatchGroup{}, nil
		}
	}
	selector := strings.TrimSpace(request.Selector)
	if selector == "" {
		return graph.NodeMatchGroup{}, nil
	}
	limit := request.Limit
	if limit <= 0 {
		limit = DefaultNodeMatchLimit
	}
	var fallback graph.NodeMatchGroup
	for _, scope := range matchScopes {
		group, err := r.matchNodesInScope(ctx, scope, selector, string(request.Kind), limit)
		if err != nil {
			return graph.NodeMatchGroup{}, err
		}
		if group.Strict > 0 {
			return group, nil
		}
		if group.StrongerThan(fallback) {
			fallback = group
		}
	}
	return fallback, nil
}

func (r *VariantRepository) matchNodesInScope(ctx context.Context, scope matchScope,
	selector, kind string, limit int) (graph.NodeMatchGroup, error) {
	group := graph.NodeMatchGroup{Level: scope.level, External: scope.external}
	external := int64(0)
	if scope.external {
		external = 1
	}
	var matchStatement, countStatement string
	switch scope.level {
	case graph.MatchQualifiedName:
		matchStatement, countStatement = "MatchNodesByQualifiedName", "CountNodeMatchesByQualifiedName"
	case graph.MatchName:
		matchStatement, countStatement = "MatchNodesByName", "CountNodeMatchesByName"
	case graph.MatchSubstring:
		matchStatement, countStatement = "MatchNodesBySubstring", "CountNodeMatchesBySubstring"
	default:
		return graph.NodeMatchGroup{}, fmt.Errorf("unknown node match level %q", scope.level)
	}
	var total, strict int64
	countRow, err := r.statements.queryRow(ctx, countStatement, selector, external, kind)
	if err != nil {
		return graph.NodeMatchGroup{}, err
	}
	if err := countRow.Scan(&total, &strict); err != nil {
		return graph.NodeMatchGroup{}, err
	}
	group.Total, group.Strict = int(total), int(strict)
	if group.Total == 0 {
		return group, nil
	}
	rows, err := r.statements.query(ctx, matchStatement, selector, external, kind, int64(limit))
	if err != nil {
		return graph.NodeMatchGroup{}, err
	}
	group.Nodes, err = scanNodes(rows)
	if err != nil {
		return graph.NodeMatchGroup{}, err
	}
	return group, nil
}

func (r *VariantRepository) Node(ctx context.Context, id string) (graph.Node, error) {
	row, err := r.statements.queryRow(ctx, "GetNode", id)
	if err != nil {
		return graph.Node{}, err
	}
	return scanNode(row)
}

func (r *VariantRepository) ExternalNodesMatching(ctx context.Context, node graph.Node) ([]graph.Node, error) {
	rows, err := r.statements.query(ctx, "ListExternalNodesMatching", node.QualifiedName, node.Name)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

func (r *VariantRepository) EdgesFrom(ctx context.Context, id string) ([]graph.Edge, error) {
	subject, err := r.adjacencySubject(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := r.statements.query(ctx, "ListEdgesFrom", subject)
	if err != nil {
		return nil, err
	}
	return r.scanEdges(rows)
}

func (r *VariantRepository) EdgesTo(ctx context.Context, id string) ([]graph.Edge, error) {
	subject, err := r.adjacencySubject(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := r.statements.query(ctx, "ListEdgesTo", subject)
	if err != nil {
		return nil, err
	}
	return r.scanEdges(rows)
}

// scanEdges reads adjacency rows through the layout: an Hydrate hook owns the
// scan when the spec provides one, otherwise the default expects the
// production edge column order.
func (r *VariantRepository) scanEdges(rows *sql.Rows) ([]graph.Edge, error) {
	if r.spec.Hydrate != nil {
		return r.spec.Hydrate(rows)
	}
	return scanEdges(rows)
}

// RelationEdges loads at most Limit+1 rows for each exact relation and joins
// the node opposite the subject in the same query. Catalog callers therefore
// pay only for requested evidence and never perform per-edge node lookups.
func (r *VariantRepository) RelationEdges(ctx context.Context, request graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	if err := request.Validate(); err != nil {
		return graph.RelationEdgePage{}, err
	}
	if request.Limit == int(^uint(0)>>1) {
		return graph.RelationEdgePage{}, fmt.Errorf("relation edge limit is too large")
	}
	relations := make([]graph.EdgeKind, 0, len(request.Relations))
	seenRelations := map[graph.EdgeKind]bool{}
	for _, relation := range request.Relations {
		if !seenRelations[relation] {
			seenRelations[relation] = true
			relations = append(relations, relation)
		}
	}
	sort.Slice(relations, func(i, j int) bool { return relations[i] < relations[j] })
	subject, err := r.adjacencySubject(ctx, request.SubjectID)
	if err != nil {
		return graph.RelationEdgePage{}, err
	}
	page := graph.RelationEdgePage{Items: []graph.HydratedRelationEdge{}}
	seenEdges := map[string]bool{}
	maxResults := int64(request.Limit) + 1
	for _, relation := range relations {
		statementName := "ListIncomingRelationEdges"
		if request.Direction == graph.OutgoingRelations {
			statementName = "ListOutgoingRelationEdges"
		}
		rows, err := r.statements.query(ctx, statementName, subject, string(relation), maxResults)
		if err != nil {
			return graph.RelationEdgePage{}, err
		}
		items, err := scanHydratedRelationEdges(rows)
		if err != nil {
			return graph.RelationEdgePage{}, err
		}
		if len(items) > request.Limit {
			page.Truncated = true
			items = items[:request.Limit]
		}
		for _, item := range items {
			if seenEdges[item.Edge.ID] {
				continue
			}
			seenEdges[item.Edge.ID] = true
			page.Items = append(page.Items, item)
		}
	}
	return page, nil
}

func (r *VariantRepository) ExternalEdgesTo(ctx context.Context, node graph.Node) ([]graph.Edge, error) {
	rows, err := r.statements.query(ctx, "ListExternalEdgesMatching", node.QualifiedName, node.Name)
	if err != nil {
		return nil, err
	}
	return r.scanEdges(rows)
}

func (r *VariantRepository) inTransaction(ctx context.Context, fn func(*transactionStatements, *batchWriter) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	writer, err := newBatchWriter(tx, r.limits, r.statementText)
	if err != nil {
		return err
	}
	defer func() { _ = writer.close() }()
	writer.afterBatch = r.afterBatch
	bound := newTransactionStatements(tx, r.statements)
	if err := fn(bound, writer); err != nil {
		return err
	}
	if err := writer.flush(ctx); err != nil {
		return err
	}
	if err := writer.close(); err != nil {
		return fmt.Errorf("close variant batch statements: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.writeStatsMu.Lock()
	addWriteStats(&r.writeStats, writer.stats())
	r.writeStatsMu.Unlock()
	return nil
}

func nodeValues(n graph.Node, external int64) []any {
	return []any{n.ID, string(n.Kind), n.Name, n.QualifiedName, n.Language, n.Location.Path,
		int64(n.Location.Line), int64(n.Location.Column), int64(n.Location.EndLine),
		graph.MarshalProperties(n.Properties), n.OwnerFile, external,
		foldName(n.Name), foldName(n.QualifiedName)}
}

func foldName(name string) string {
	return strings.ToLower(name)
}

func factValues(f graph.Fact) []any {
	return []any{f.ID, f.FromID, f.Source, string(f.SourceKind), string(f.Kind), f.Producer,
		f.TargetID, f.Target, string(f.TargetKind), f.Location.Path, int64(f.Location.Line),
		int64(f.Location.Column), int64(f.Location.EndLine), graph.MarshalProperties(f.Properties), f.OwnerFile}
}

func edgeValues(e graph.Edge) []any {
	return []any{e.ID, e.FactID, e.FromID, e.ToID, string(e.Kind), e.Producer, e.Location.Path,
		int64(e.Location.Line), int64(e.Location.Column), int64(e.Location.EndLine),
		graph.MarshalProperties(e.Properties)}
}

// dirtyFactRow is one ListDirtyFactBatch row: the fact columns in UpsertFact
// order plus the source and target existence proofs.
type dirtyFactRow struct {
	id, fromID, source, sourceKind, kind, producer, targetID, target, targetKind,
	path, properties, ownerFile string
	line, column, endLine      int64
	sourceExists, targetExists bool
}

func (row dirtyFactRow) fact() graph.Fact {
	return graph.Fact{ID: row.id, FromID: row.fromID, Source: row.source, SourceKind: graph.NodeKind(row.sourceKind),
		Kind: graph.EdgeKind(row.kind), Producer: row.producer, TargetID: row.targetID,
		Target: row.target, TargetKind: graph.NodeKind(row.targetKind),
		Location:   graph.Location{Path: row.path, Line: int(row.line), Column: int(row.column), EndLine: int(row.endLine)},
		Properties: graph.UnmarshalProperties(row.properties), OwnerFile: row.ownerFile}
}

func scanDirtyFactRows(rows *sql.Rows) ([]dirtyFactRow, error) {
	defer func() { _ = rows.Close() }()
	result := []dirtyFactRow{}
	for rows.Next() {
		var row dirtyFactRow
		if err := rows.Scan(&row.id, &row.fromID, &row.source, &row.sourceKind, &row.kind, &row.producer,
			&row.targetID, &row.target, &row.targetKind, &row.path, &row.line,
			&row.column, &row.endLine, &row.properties, &row.ownerFile,
			&row.sourceExists, &row.targetExists); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func scanResolutionCandidates(rows *sql.Rows) ([]resolutionCandidate, error) {
	defer func() { _ = rows.Close() }()
	result := []resolutionCandidate{}
	for rows.Next() {
		var candidate resolutionCandidate
		var kind string
		if err := rows.Scan(&candidate.id, &kind, &candidate.qualifiedName); err != nil {
			return nil, err
		}
		candidate.kind = graph.NodeKind(kind)
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func scanKindCounts(rows *sql.Rows) (map[string]int, error) {
	defer func() { _ = rows.Close() }()
	result := map[string]int{}
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		result[kind] = int(count)
	}
	return result, rows.Err()
}

// scanNode reads one node row in the physical column order of the production
// nodes table: fourteen columns with the folded names appended last.
func scanNode(row *sql.Row) (graph.Node, error) {
	var node graph.Node
	var kind string
	var external int64
	var properties string
	var nameFolded, qualifiedNameFolded string
	err := row.Scan(&node.ID, &kind, &node.Name, &node.QualifiedName, &node.Language,
		&node.Location.Path, &node.Location.Line, &node.Location.Column, &node.Location.EndLine,
		&properties, &node.OwnerFile, &external, &nameFolded, &qualifiedNameFolded)
	if err != nil {
		return graph.Node{}, err
	}
	node.Kind = graph.NodeKind(kind)
	node.External = external != 0
	node.Properties = graph.UnmarshalProperties(properties)
	return node, nil
}

func scanNodes(rows *sql.Rows) ([]graph.Node, error) {
	defer func() { _ = rows.Close() }()
	result := []graph.Node{}
	for rows.Next() {
		var node graph.Node
		var kind string
		var external int64
		var properties string
		var nameFolded, qualifiedNameFolded string
		if err := rows.Scan(&node.ID, &kind, &node.Name, &node.QualifiedName, &node.Language,
			&node.Location.Path, &node.Location.Line, &node.Location.Column, &node.Location.EndLine,
			&properties, &node.OwnerFile, &external, &nameFolded, &qualifiedNameFolded); err != nil {
			return nil, err
		}
		node.Kind = graph.NodeKind(kind)
		node.External = external != 0
		node.Properties = graph.UnmarshalProperties(properties)
		result = append(result, node)
	}
	return result, rows.Err()
}

// scanEdges reads edge rows in the physical column order of the production
// edges table, with the producer column appended last by migration.
func scanEdges(rows *sql.Rows) ([]graph.Edge, error) {
	defer func() { _ = rows.Close() }()
	result := []graph.Edge{}
	for rows.Next() {
		var edge graph.Edge
		var kind string
		var properties string
		if err := rows.Scan(&edge.ID, &edge.FactID, &edge.FromID, &edge.ToID, &kind,
			&edge.Location.Path, &edge.Location.Line, &edge.Location.Column, &edge.Location.EndLine,
			&properties, &edge.Producer); err != nil {
			return nil, err
		}
		edge.Kind = graph.EdgeKind(kind)
		edge.Properties = graph.UnmarshalProperties(properties)
		result = append(result, edge)
	}
	return result, rows.Err()
}

// scanHydratedRelationEdges reads the twenty-four columns the relation edge
// statements return: eleven edge columns then the counterpart node columns.
func scanHydratedRelationEdges(rows *sql.Rows) ([]graph.HydratedRelationEdge, error) {
	defer func() { _ = rows.Close() }()
	result := []graph.HydratedRelationEdge{}
	for rows.Next() {
		var edgeID, factID, fromID, toID, kind, producer, path string
		var line, column, endLine int64
		var properties string
		var counterpartID, counterpartKind, counterpartName, counterpartQualifiedName string
		var counterpartLanguage, counterpartPath string
		var counterpartLine, counterpartColumn, counterpartEndLine int64
		var counterpartProperties, counterpartOwnerFile string
		var counterpartExternal int64
		if err := rows.Scan(&edgeID, &factID, &fromID, &toID, &kind, &producer, &path,
			&line, &column, &endLine, &properties,
			&counterpartID, &counterpartKind, &counterpartName, &counterpartQualifiedName,
			&counterpartLanguage, &counterpartPath, &counterpartLine, &counterpartColumn,
			&counterpartEndLine, &counterpartProperties, &counterpartOwnerFile, &counterpartExternal); err != nil {
			return nil, err
		}
		item, err := hydratedRelationEdge(edgeID, factID, fromID, toID, kind, producer, path,
			line, column, endLine, properties, counterpartID, counterpartKind, counterpartName,
			counterpartQualifiedName, counterpartLanguage, counterpartPath, counterpartLine,
			counterpartColumn, counterpartEndLine, counterpartProperties, counterpartOwnerFile,
			counterpartExternal)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func hydratedRelationEdge(edgeID, factID, fromID, toID, kind, producer, path string,
	line, column, endLine int64, properties, counterpartID, counterpartKind, counterpartName,
	counterpartQualifiedName, counterpartLanguage, counterpartPath string, counterpartLine,
	counterpartColumn, counterpartEndLine int64, counterpartProperties, counterpartOwnerFile string,
	counterpartExternal int64) (graph.HydratedRelationEdge, error) {
	if counterpartID == "" {
		return graph.HydratedRelationEdge{}, fmt.Errorf("edge %s has no counterpart node", edgeID)
	}
	edgeProperties, err := decodeRelationProperties("edge "+edgeID, properties)
	if err != nil {
		return graph.HydratedRelationEdge{}, err
	}
	nodeProperties, err := decodeRelationProperties("counterpart node "+counterpartID, counterpartProperties)
	if err != nil {
		return graph.HydratedRelationEdge{}, err
	}
	return graph.HydratedRelationEdge{
		Edge: graph.Edge{ID: edgeID, FactID: factID, FromID: fromID, ToID: toID, Kind: graph.EdgeKind(kind), Producer: producer,
			Location:   graph.Location{Path: path, Line: int(line), Column: int(column), EndLine: int(endLine)},
			Properties: edgeProperties},
		Counterpart: graph.Node{ID: counterpartID, Kind: graph.NodeKind(counterpartKind), Name: counterpartName,
			QualifiedName: counterpartQualifiedName, Language: counterpartLanguage,
			Location: graph.Location{Path: counterpartPath, Line: int(counterpartLine),
				Column: int(counterpartColumn), EndLine: int(counterpartEndLine)},
			Properties: nodeProperties, OwnerFile: counterpartOwnerFile,
			External: counterpartExternal != 0},
	}, nil
}

func decodeRelationProperties(label, raw string) (map[string]string, error) {
	if raw == "{}" {
		return nil, nil
	}
	var properties map[string]string
	if err := json.Unmarshal([]byte(raw), &properties); err != nil {
		return nil, fmt.Errorf("decode %s properties: %w", label, err)
	}
	if len(properties) == 0 {
		return nil, nil
	}
	return properties, nil
}
