package sqlite

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
	"github.com/pressly/goose/v3"
	modernsqlite "modernc.org/sqlite"
)

// Repository is the SQLite adapter for graph.Repository.
type Repository struct {
	db      *sql.DB
	queries *sqlcgen.Queries
	path    string
	limits  batchLimits

	writeStatsMu sync.Mutex
	writeStats   graph.WriteStats
	afterBatch   func(string, graph.WriteBatchStats)
}

const (
	reconciliationBatchSize = 10_000
	resolutionCacheSize     = 50_000
	sqliteLimitVariables    = 9
)

var _ graph.Repository = (*Repository)(nil)
var _ graph.CatalogRepository = (*Repository)(nil)
var _ semantic.Repository = (*Repository)(nil)

func Open(ctx context.Context, path string) (*Repository, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open graph: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA busy_timeout=5000", "PRAGMA wal_autocheckpoint=1000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure SQLite: %w", err)
		}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate graph database: %w", err)
	}
	queries, err := sqlcgen.Prepare(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare graph queries: %w", err)
	}
	variableLimit, err := activeVariableLimit(ctx, db)
	if err != nil {
		_ = queries.Close()
		_ = db.Close()
		return nil, fmt.Errorf("read SQLite variable limit: %w", err)
	}
	repository := &Repository{db: db, queries: queries, path: path, limits: batchLimits{
		MaxRows: defaultBatchRows, MaxVariables: variableLimit, MaxBytes: defaultBatchBytes,
	}}
	if err := repository.SetMeta(ctx, "schema_version", fmt.Sprint(graph.SchemaVersion)); err != nil {
		_ = repository.Close()
		return nil, err
	}
	return repository, nil
}

func (r *Repository) Close() error { return errors.Join(r.queries.Close(), r.db.Close()) }
func (r *Repository) Path() string { return r.path }

func (r *Repository) WriteStats() graph.WriteStats {
	r.writeStatsMu.Lock()
	defer r.writeStatsMu.Unlock()
	return r.writeStats
}

func (r *Repository) SetMeta(ctx context.Context, key, value string) error {
	return r.queries.SetMeta(ctx, sqlcgen.SetMetaParams{Key: key, Value: value})
}

func (r *Repository) Meta(ctx context.Context, key string) (string, error) {
	value, err := r.queries.GetMeta(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (r *Repository) CandidateNodes(ctx context.Context) ([]graph.Node, error) {
	rows, err := r.queries.ListSemanticCandidateNodes(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		result = append(result, nodeFromRow(row))
	}
	return result, nil
}

func (r *Repository) Embeddings(ctx context.Context, model string) ([]semantic.Embedding, error) {
	rows, err := r.queries.ListEmbeddingsByModel(ctx, model)
	if err != nil {
		return nil, err
	}
	result := make([]semantic.Embedding, 0, len(rows))
	for _, row := range rows {
		var vector []float32
		if err := json.Unmarshal([]byte(row.VectorJson), &vector); err != nil {
			return nil, fmt.Errorf("decode embedding for %s: %w", row.NodeID, err)
		}
		if int64(len(vector)) != row.Dimensions {
			return nil, fmt.Errorf("embedding for %s declares %d dimensions but stores %d", row.NodeID, row.Dimensions, len(vector))
		}
		result = append(result, semantic.Embedding{NodeID: row.NodeID, Model: row.Model,
			ContentHash: row.ContentHash, Vector: vector, UpdatedAt: row.UpdatedAt})
	}
	return result, nil
}

func (r *Repository) EmbeddingHashes(ctx context.Context, model string) (map[string]string, error) {
	rows, err := r.queries.ListEmbeddingHashesByModel(ctx, model)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(rows))
	for _, row := range rows {
		result[row.NodeID] = row.ContentHash
	}
	return result, nil
}

func (r *Repository) UpsertEmbedding(ctx context.Context, embedding semantic.Embedding) error {
	vector, err := json.Marshal(embedding.Vector)
	if err != nil {
		return err
	}
	return r.queries.UpsertEmbedding(ctx, sqlcgen.UpsertEmbeddingParams{NodeID: embedding.NodeID,
		Model: embedding.Model, ContentHash: embedding.ContentHash, Dimensions: int64(len(embedding.Vector)),
		VectorJson: string(vector), UpdatedAt: embedding.UpdatedAt})
}

func (r *Repository) DeleteStaleEmbeddings(ctx context.Context, model string) (int64, error) {
	return r.queries.DeleteStaleEmbeddings(ctx, model)
}

func (r *Repository) Files(ctx context.Context) (map[string]graph.FileRecord, error) {
	rows, err := r.queries.ListFiles(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]graph.FileRecord, len(rows))
	for _, row := range rows {
		result[row.Path] = graph.FileRecord{Path: row.Path, Hash: row.Hash, Language: row.Language,
			Size: row.Size, ModifiedNS: row.ModifiedNs, IndexedAt: row.IndexedAt}
	}
	return result, nil
}

func (r *Repository) ReplaceFile(ctx context.Context, file graph.FileRecord, parsed graph.ParseResult) error {
	if err := validateParseResult(parsed); err != nil {
		return err
	}
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		if err := markOwnerDirty(ctx, q, file.Path); err != nil {
			return err
		}
		if err := q.DeleteEdgesByOwnerFacts(ctx, file.Path); err != nil {
			return err
		}
		if err := q.DeleteFactsByOwner(ctx, file.Path); err != nil {
			return err
		}
		if err := q.DeleteNodesByOwner(ctx, file.Path); err != nil {
			return err
		}
		if err := q.UpsertFile(ctx, sqlcgen.UpsertFileParams{Path: file.Path, Hash: file.Hash,
			Language: file.Language, Size: file.Size, ModifiedNs: file.ModifiedNS, IndexedAt: file.IndexedAt}); err != nil {
			return err
		}
		return insertParseResult(ctx, q, writer, parsed)
	})
}

func (r *Repository) ReplaceOwner(ctx context.Context, owner string, parsed graph.ParseResult) error {
	if err := validateParseResult(parsed); err != nil {
		return err
	}
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		if err := markOwnerDirty(ctx, q, owner); err != nil {
			return err
		}
		if err := q.DeleteEdgesByOwnerFacts(ctx, owner); err != nil {
			return err
		}
		if err := q.DeleteFactsByOwner(ctx, owner); err != nil {
			return err
		}
		if err := q.DeleteNodesByOwner(ctx, owner); err != nil {
			return err
		}
		return insertParseResult(ctx, q, writer, parsed)
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

func (r *Repository) RemoveFiles(ctx context.Context, paths []string) error {
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		for _, path := range paths {
			if err := markOwnerDirty(ctx, q, path); err != nil {
				return err
			}
			if err := q.DeleteEdgesByOwnerFacts(ctx, path); err != nil {
				return err
			}
			if err := q.DeleteFactsByOwner(ctx, path); err != nil {
				return err
			}
			if err := q.DeleteNodesByOwner(ctx, path); err != nil {
				return err
			}
			if err := q.DeleteFile(ctx, path); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertParseResult(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, parsed graph.ParseResult) error {
	for _, node := range parsed.Nodes {
		external := int64(0)
		if node.External {
			external = 1
		}
		if err := writer.addNode(ctx, nodeParams(node, external)); err != nil {
			return fmt.Errorf("upsert node %s: %w", node.QualifiedName, err)
		}
		if !node.External {
			if err := markNodeDirty(ctx, writer, node); err != nil {
				return err
			}
		}
	}
	for _, fact := range parsed.Facts {
		if err := writer.addFact(ctx, factParams(fact)); err != nil {
			return fmt.Errorf("upsert fact %s: %w", fact.ID, err)
		}
	}
	return nil
}

func (r *Repository) Reconcile(ctx context.Context) error {
	_, err := r.ReconcileWithStats(ctx, nil)
	return err
}

func (r *Repository) ReconcileWithStats(ctx context.Context, observer graph.ReconciliationObserver) (graph.ReconciliationStats, error) {
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
	cleanupPending, err := r.queries.ReconciliationCleanupPending(ctx)
	if err != nil {
		return stats, fmt.Errorf("check reconciliation cleanup: %w", err)
	}
	if !cleanupPending {
		return stats, nil
	}
	if err := r.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		if err := q.DeleteOrphanExternalNodes(ctx); err != nil {
			return err
		}
		return q.ClearReconciliationCleanup(ctx)
	}); err != nil {
		return stats, fmt.Errorf("remove orphan external nodes: %w", err)
	}
	return stats, r.checkpoint(ctx, true)
}

func (r *Repository) queueDirtyFacts(ctx context.Context) error {
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		if err := q.PruneDirtyFacts(ctx); err != nil {
			return err
		}
		if err := q.EnqueueDirtyFacts(ctx); err != nil {
			return err
		}
		if err := q.MarkReconciliationCleanup(ctx); err != nil {
			return err
		}
		if err := q.ClearDirtyOwners(ctx); err != nil {
			return err
		}
		if err := q.ClearDirtyNodes(ctx); err != nil {
			return err
		}
		return q.ClearDirtyTargets(ctx)
	})
}

func (r *Repository) reconcileBatch(ctx context.Context, resolved *resolutionCache) (int, error) {
	processed := 0
	err := r.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		facts, err := q.ListDirtyFactBatch(ctx, reconciliationBatchSize)
		if err != nil {
			return err
		}
		if len(facts) == 0 {
			return nil
		}
		if err := q.DeleteEdgesByDirtyFactBatch(ctx, reconciliationBatchSize); err != nil {
			return err
		}
		for _, row := range facts {
			processed++
			fact := factFromDirtyRow(row)
			sources, err := resolveSources(ctx, q, writer, fact, row.SourceExists != 0, resolved)
			if err != nil {
				return err
			}
			targets, err := resolveTargets(ctx, q, writer, fact, row.TargetExists != 0, resolved)
			if err != nil {
				return err
			}
			for _, source := range sources {
				for _, target := range targets {
					edge := graph.Edge{ID: graph.EdgeID(fact.ID, target), FactID: fact.ID,
						FromID: source, ToID: target, Kind: fact.Kind, Location: fact.Location,
						Properties: fact.Properties}
					if err := writer.addEdge(ctx, edgeParams(edge)); err != nil {
						return err
					}
				}
			}
		}
		if err := writer.flush(ctx); err != nil {
			return err
		}
		return q.DeleteDirtyFactBatch(ctx, reconciliationBatchSize)
	})
	return processed, err
}

func (r *Repository) checkpoint(ctx context.Context, truncate bool) error {
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

func markOwnerDirty(ctx context.Context, q *sqlcgen.Queries, owner string) error {
	if err := q.MarkDirtyOwner(ctx, owner); err != nil {
		return err
	}
	if err := q.MarkOwnedNodesDirty(ctx, owner); err != nil {
		return err
	}
	return q.MarkOwnedNamesDirty(ctx, sqlcgen.MarkOwnedNamesDirtyParams{OwnerFile: owner, OwnerFile_2: owner})
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

func resolveSources(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, fact graph.Fact, sourceExists bool, cache *resolutionCache) ([]string, error) {
	return resolveEndpoint(ctx, q, writer, fact, sourceEndpoint, sourceExists, cache)
}

func resolveTargets(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, fact graph.Fact, targetExists bool, cache *resolutionCache) ([]string, error) {
	return resolveEndpoint(ctx, q, writer, fact, targetEndpoint, targetExists, cache)
}

func resolveEndpoint(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, fact graph.Fact, direction endpointDirection, exactExists bool, cache *resolutionCache) ([]string, error) {
	exactID, name, kind := fact.TargetID, fact.Target, fact.TargetKind
	if direction == sourceEndpoint {
		exactID, name, kind = fact.FromID, fact.Source, fact.SourceKind
	}
	if exactID != "" {
		if exactExists {
			return []string{exactID}, nil
		}
		external := externalNode(exactID, "")
		if err := writer.addNode(ctx, nodeParams(external, 1)); err != nil {
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
		if kind == "" {
			found, err := q.FindNodesExact(ctx, name)
			if err != nil {
				return nil, err
			}
			rows = make([]resolutionCandidate, 0, len(found))
			for _, row := range found {
				rows = append(rows, resolutionCandidate{id: row.ID, kind: graph.NodeKind(row.Kind), qualifiedName: row.QualifiedName})
			}
		} else {
			found, err := q.FindNodesExactKind(ctx, sqlcgen.FindNodesExactKindParams{
				Target: name, Kind: string(kind)})
			if err != nil {
				return nil, err
			}
			rows = make([]resolutionCandidate, 0, len(found))
			for _, row := range found {
				rows = append(rows, resolutionCandidate{id: row.ID, kind: graph.NodeKind(row.Kind), qualifiedName: row.QualifiedName})
			}
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
		if err := writer.addNode(ctx, nodeParams(external, 1)); err != nil {
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

func (r *Repository) Counts(ctx context.Context) (graph.Counts, error) {
	result := graph.Counts{ByKind: map[string]int{}, ByEdge: map[string]int{}}
	files, err := r.queries.CountFiles(ctx)
	if err != nil {
		return result, err
	}
	nodes, err := r.queries.CountNodes(ctx)
	if err != nil {
		return result, err
	}
	facts, err := r.queries.CountFacts(ctx)
	if err != nil {
		return result, err
	}
	edges, err := r.queries.CountEdges(ctx)
	if err != nil {
		return result, err
	}
	external, err := r.queries.CountExternalNodes(ctx)
	if err != nil {
		return result, err
	}
	result.Files, result.Nodes, result.Facts, result.Edges, result.External = int(files), int(nodes), int(facts), int(edges), int(external)
	nodeKinds, err := r.queries.CountNodesByKind(ctx)
	if err != nil {
		return result, err
	}
	for _, row := range nodeKinds {
		result.ByKind[row.Kind] = int(row.Count)
	}
	edgeKinds, err := r.queries.CountEdgesByKind(ctx)
	if err != nil {
		return result, err
	}
	for _, row := range edgeKinds {
		result.ByEdge[row.Kind] = int(row.Count)
	}
	return result, nil
}

func (r *Repository) SearchNodes(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	rows, err := r.queries.SearchNodes(ctx, sqlcgen.SearchNodesParams{Term: foldName(term), MaxResults: int64(limit)})
	if err != nil {
		return nil, err
	}
	result := make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		result = append(result, nodeFromRow(row))
	}
	return result, nil
}

// Repositories reports the indexed repository this database stores, derived
// from the root recorded at index time.
func (r *Repository) Repositories(ctx context.Context) ([]string, error) {
	name, err := r.repositoryName(ctx)
	if err != nil || name == "" {
		return []string{}, err
	}
	return []string{name}, nil
}

// ListNodesByKind enumerates nodes of the requested kinds. It only enumerates:
// kind membership, name matching, and bounds are storage concerns, while usage
// direction and orphan classification stay in the query use cases.
func (r *Repository) ListNodesByKind(ctx context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
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
	result := []graph.ScopedNode{}
	for _, kind := range request.Kinds {
		rows, err := r.queries.ListNodesByKind(ctx, sqlcgen.ListNodesByKindParams{
			Kind: string(kind), MinExternal: minExternal, MaxExternal: maxExternal,
			NameFragment: fragment, MaxResults: limit,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			result = append(result, graph.ScopedNode{Repository: name, Node: nodeFromRow(row)})
		}
	}
	return result, nil
}

func (r *Repository) repositoryName(ctx context.Context) (string, error) {
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

// MatchNodes resolves a selector against the narrow, index-backed match queries
// in nodes.sql. Scopes are tried strongest first and the first one holding a
// case-sensitive match wins, so a substring match never competes with an exact
// name match, an unresolved boundary node never competes with a real declaration,
// and a case-insensitive-only hit at a strong level never outranks a
// case-sensitive match at a weaker one. A scope with only case-insensitive matches
// is kept as the fallback for selectors that match nothing case-sensitively. The
// count query runs before the row query so the reported totals cover the whole
// graph even when the row list is truncated.
func (r *Repository) MatchNodes(ctx context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
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

func (r *Repository) matchNodesInScope(ctx context.Context, scope matchScope,
	selector, kind string, limit int) (graph.NodeMatchGroup, error) {
	group := graph.NodeMatchGroup{Level: scope.level, External: scope.external}
	external := int64(0)
	if scope.external {
		external = 1
	}
	var rows []sqlcgen.Node
	var err error
	switch scope.level {
	case graph.MatchQualifiedName:
		counts, countErr := r.queries.CountNodeMatchesByQualifiedName(ctx,
			sqlcgen.CountNodeMatchesByQualifiedNameParams{Target: selector, External: external, Kind: kind})
		if countErr != nil {
			return graph.NodeMatchGroup{}, countErr
		}
		group.Total, group.Strict = int(counts.Total), int(counts.StrictMatches)
		if group.Total == 0 {
			return group, nil
		}
		rows, err = r.queries.MatchNodesByQualifiedName(ctx,
			sqlcgen.MatchNodesByQualifiedNameParams{Target: selector, External: external, Kind: kind,
				MaxResults: int64(limit)})
	case graph.MatchName:
		counts, countErr := r.queries.CountNodeMatchesByName(ctx,
			sqlcgen.CountNodeMatchesByNameParams{Target: selector, External: external, Kind: kind})
		if countErr != nil {
			return graph.NodeMatchGroup{}, countErr
		}
		group.Total, group.Strict = int(counts.Total), int(counts.StrictMatches)
		if group.Total == 0 {
			return group, nil
		}
		rows, err = r.queries.MatchNodesByName(ctx,
			sqlcgen.MatchNodesByNameParams{Target: selector, External: external, Kind: kind,
				MaxResults: int64(limit)})
	case graph.MatchSubstring:
		counts, countErr := r.queries.CountNodeMatchesBySubstring(ctx,
			sqlcgen.CountNodeMatchesBySubstringParams{Target: selector, External: external, Kind: kind})
		if countErr != nil {
			return graph.NodeMatchGroup{}, countErr
		}
		group.Total, group.Strict = int(counts.Total), int(counts.StrictMatches)
		if group.Total == 0 {
			return group, nil
		}
		rows, err = r.queries.MatchNodesBySubstring(ctx,
			sqlcgen.MatchNodesBySubstringParams{Target: selector, External: external, Kind: kind,
				MaxResults: int64(limit)})
	default:
		return graph.NodeMatchGroup{}, fmt.Errorf("unknown node match level %q", scope.level)
	}
	if err != nil {
		return graph.NodeMatchGroup{}, err
	}
	group.Nodes = make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		group.Nodes = append(group.Nodes, nodeFromRow(row))
	}
	return group, nil
}

func (r *Repository) Node(ctx context.Context, id string) (graph.Node, error) {
	row, err := r.queries.GetNode(ctx, id)
	if err != nil {
		return graph.Node{}, err
	}
	return nodeFromRow(row), nil
}

func (r *Repository) ExternalNodesMatching(ctx context.Context, node graph.Node) ([]graph.Node, error) {
	rows, err := r.queries.ListExternalNodesMatching(ctx, sqlcgen.ListExternalNodesMatchingParams{
		QualifiedName: node.QualifiedName, Name: node.Name,
	})
	if err != nil {
		return nil, err
	}
	result := make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		result = append(result, nodeFromRow(row))
	}
	return result, nil
}

func (r *Repository) EdgesFrom(ctx context.Context, id string) ([]graph.Edge, error) {
	rows, err := r.queries.ListEdgesFrom(ctx, id)
	if err != nil {
		return nil, err
	}
	return edgesFromRows(rows), nil
}

func (r *Repository) EdgesTo(ctx context.Context, id string) ([]graph.Edge, error) {
	rows, err := r.queries.ListEdgesTo(ctx, id)
	if err != nil {
		return nil, err
	}
	return edgesFromRows(rows), nil
}

// RelationEdges loads at most Limit+1 rows for each exact relation and joins
// the node opposite the subject in the same query. Catalog callers therefore
// pay only for requested evidence and never perform per-edge node lookups.
func (r *Repository) RelationEdges(ctx context.Context, request graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
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
	page := graph.RelationEdgePage{Items: []graph.HydratedRelationEdge{}}
	seenEdges := map[string]bool{}
	maxResults := int64(request.Limit) + 1
	for _, relation := range relations {
		items := []graph.HydratedRelationEdge{}
		switch request.Direction {
		case graph.IncomingRelations:
			rows, err := r.queries.ListIncomingRelationEdges(ctx, sqlcgen.ListIncomingRelationEdgesParams{
				SubjectID: request.SubjectID, Relation: string(relation), MaxResults: maxResults,
			})
			if err != nil {
				return graph.RelationEdgePage{}, err
			}
			for _, row := range rows {
				item, err := hydratedRelationEdge(row.EdgeID, row.EdgeFactID, row.EdgeFromID, row.EdgeToID,
					row.EdgeKind, row.EdgePath, row.EdgeLine, row.EdgeColumnNo, row.EdgeEndLine, row.EdgeProperties,
					row.CounterpartID, row.CounterpartKind, row.CounterpartName, row.CounterpartQualifiedName,
					row.CounterpartLanguage, row.CounterpartPath, row.CounterpartLine, row.CounterpartColumnNo,
					row.CounterpartEndLine, row.CounterpartProperties, row.CounterpartOwnerFile, row.CounterpartExternal)
				if err != nil {
					return graph.RelationEdgePage{}, err
				}
				items = append(items, item)
			}
		case graph.OutgoingRelations:
			rows, err := r.queries.ListOutgoingRelationEdges(ctx, sqlcgen.ListOutgoingRelationEdgesParams{
				SubjectID: request.SubjectID, Relation: string(relation), MaxResults: maxResults,
			})
			if err != nil {
				return graph.RelationEdgePage{}, err
			}
			for _, row := range rows {
				item, err := hydratedRelationEdge(row.EdgeID, row.EdgeFactID, row.EdgeFromID, row.EdgeToID,
					row.EdgeKind, row.EdgePath, row.EdgeLine, row.EdgeColumnNo, row.EdgeEndLine, row.EdgeProperties,
					row.CounterpartID, row.CounterpartKind, row.CounterpartName, row.CounterpartQualifiedName,
					row.CounterpartLanguage, row.CounterpartPath, row.CounterpartLine, row.CounterpartColumnNo,
					row.CounterpartEndLine, row.CounterpartProperties, row.CounterpartOwnerFile, row.CounterpartExternal)
				if err != nil {
					return graph.RelationEdgePage{}, err
				}
				items = append(items, item)
			}
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

func (r *Repository) ExternalEdgesTo(ctx context.Context, node graph.Node) ([]graph.Edge, error) {
	rows, err := r.queries.ListExternalEdgesMatching(ctx, sqlcgen.ListExternalEdgesMatchingParams{
		QualifiedName: node.QualifiedName, Name: node.Name,
	})
	if err != nil {
		return nil, err
	}
	return edgesFromRows(rows), nil
}

func (r *Repository) inTransaction(ctx context.Context, fn func(*sqlcgen.Queries, *batchWriter) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	writer, err := newBatchWriter(tx, r.limits)
	if err != nil {
		return err
	}
	defer func() { _ = writer.close() }()
	writer.afterBatch = r.afterBatch
	if err := fn(r.queries.WithTx(tx), writer); err != nil {
		return err
	}
	if err := writer.flush(ctx); err != nil {
		return err
	}
	if err := writer.close(); err != nil {
		return fmt.Errorf("close SQLite batch statements: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.writeStatsMu.Lock()
	addWriteStats(&r.writeStats, writer.stats())
	r.writeStatsMu.Unlock()
	return nil
}

func activeVariableLimit(ctx context.Context, db *sql.DB) (int, error) {
	connection, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = connection.Close() }()
	limit, err := modernsqlite.Limit(connection, sqliteLimitVariables, -1)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, fmt.Errorf("driver reported invalid limit %d", limit)
	}
	return limit, nil
}

func nodeParams(n graph.Node, external int64) sqlcgen.UpsertNodeParams {
	return sqlcgen.UpsertNodeParams{ID: n.ID, Kind: string(n.Kind), Name: n.Name,
		QualifiedName: n.QualifiedName, Language: n.Language, Path: n.Location.Path,
		Line: int64(n.Location.Line), ColumnNo: int64(n.Location.Column), EndLine: int64(n.Location.EndLine),
		Properties: graph.MarshalProperties(n.Properties), OwnerFile: n.OwnerFile, External: external,
		NameFolded: foldName(n.Name), QualifiedNameFolded: foldName(n.QualifiedName)}
}

func foldName(name string) string {
	return strings.ToLower(name)
}

func factParams(f graph.Fact) sqlcgen.UpsertFactParams {
	return sqlcgen.UpsertFactParams{ID: f.ID, FromID: f.FromID, Source: f.Source,
		SourceKind: string(f.SourceKind), Kind: string(f.Kind), TargetID: f.TargetID,
		Target: f.Target, TargetKind: string(f.TargetKind), Path: f.Location.Path, Line: int64(f.Location.Line),
		ColumnNo: int64(f.Location.Column), EndLine: int64(f.Location.EndLine),
		Properties: graph.MarshalProperties(f.Properties), OwnerFile: f.OwnerFile}
}

func edgeParams(e graph.Edge) sqlcgen.InsertEdgeParams {
	return sqlcgen.InsertEdgeParams{ID: e.ID, FactID: e.FactID, FromID: e.FromID, ToID: e.ToID,
		Kind: string(e.Kind), Path: e.Location.Path, Line: int64(e.Location.Line),
		ColumnNo: int64(e.Location.Column), EndLine: int64(e.Location.EndLine),
		Properties: graph.MarshalProperties(e.Properties)}
}

func nodeFromRow(n sqlcgen.Node) graph.Node {
	return graph.Node{ID: n.ID, Kind: graph.NodeKind(n.Kind), Name: n.Name, QualifiedName: n.QualifiedName,
		Language: n.Language, Location: graph.Location{Path: n.Path, Line: int(n.Line), Column: int(n.ColumnNo), EndLine: int(n.EndLine)},
		Properties: graph.UnmarshalProperties(n.Properties), OwnerFile: n.OwnerFile, External: n.External != 0}
}

func factFromDirtyRow(f sqlcgen.ListDirtyFactBatchRow) graph.Fact {
	return graph.Fact{ID: f.ID, FromID: f.FromID, Source: f.Source, SourceKind: graph.NodeKind(f.SourceKind),
		Kind: graph.EdgeKind(f.Kind), TargetID: f.TargetID,
		Target: f.Target, TargetKind: graph.NodeKind(f.TargetKind),
		Location:   graph.Location{Path: f.Path, Line: int(f.Line), Column: int(f.ColumnNo), EndLine: int(f.EndLine)},
		Properties: graph.UnmarshalProperties(f.Properties), OwnerFile: f.OwnerFile}
}

func edgesFromRows(rows []sqlcgen.Edge) []graph.Edge {
	result := make([]graph.Edge, 0, len(rows))
	for _, e := range rows {
		result = append(result, graph.Edge{ID: e.ID, FactID: e.FactID, FromID: e.FromID,
			ToID: e.ToID, Kind: graph.EdgeKind(e.Kind),
			Location:   graph.Location{Path: e.Path, Line: int(e.Line), Column: int(e.ColumnNo), EndLine: int(e.EndLine)},
			Properties: graph.UnmarshalProperties(e.Properties)})
	}
	return result
}

func hydratedRelationEdge(edgeID, factID, fromID, toID, kind, path string,
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
		Edge: graph.Edge{ID: edgeID, FactID: factID, FromID: fromID, ToID: toID, Kind: graph.EdgeKind(kind),
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
