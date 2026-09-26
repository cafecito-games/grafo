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

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// Repository is the SQLite adapter for graph.Repository.
type Repository struct {
	db      *sql.DB
	queries *sqlcgen.Queries
	path    string
}

const (
	reconciliationBatchSize = 10_000
	resolutionCacheSize     = 50_000
)

var _ graph.Repository = (*Repository)(nil)
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
			db.Close()
			return nil, fmt.Errorf("configure SQLite: %w", err)
		}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate graph database: %w", err)
	}
	queries, err := sqlcgen.Prepare(ctx, db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare graph queries: %w", err)
	}
	repository := &Repository{db: db, queries: queries, path: path}
	if err := repository.SetMeta(ctx, "schema_version", fmt.Sprint(graph.SchemaVersion)); err != nil {
		repository.Close()
		return nil, err
	}
	return repository, nil
}

func (r *Repository) Close() error { return errors.Join(r.queries.Close(), r.db.Close()) }
func (r *Repository) Path() string { return r.path }

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
	return r.inTransaction(ctx, func(q *sqlcgen.Queries) error {
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
		return insertParseResult(ctx, q, parsed)
	})
}

func (r *Repository) ReplaceOwner(ctx context.Context, owner string, parsed graph.ParseResult) error {
	return r.inTransaction(ctx, func(q *sqlcgen.Queries) error {
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
		return insertParseResult(ctx, q, parsed)
	})
}

func (r *Repository) RemoveFiles(ctx context.Context, paths []string) error {
	return r.inTransaction(ctx, func(q *sqlcgen.Queries) error {
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

func insertParseResult(ctx context.Context, q *sqlcgen.Queries, parsed graph.ParseResult) error {
	for _, node := range parsed.Nodes {
		external := int64(0)
		if node.External {
			external = 1
		}
		if err := q.UpsertNode(ctx, nodeParams(node, external)); err != nil {
			return fmt.Errorf("upsert node %s: %w", node.QualifiedName, err)
		}
		if !node.External {
			if err := markNodeDirty(ctx, q, node); err != nil {
				return err
			}
		}
	}
	for _, fact := range parsed.Facts {
		if err := q.UpsertFact(ctx, factParams(fact)); err != nil {
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
		processed, err := r.reconcileBatch(ctx, resolved)
		if err != nil {
			return stats, fmt.Errorf("reconcile fact batch: %w", err)
		}
		if processed == 0 {
			break
		}
		stats.Batches++
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
	if err := r.inTransaction(ctx, func(q *sqlcgen.Queries) error {
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
	return r.inTransaction(ctx, func(q *sqlcgen.Queries) error {
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
	err := r.inTransaction(ctx, func(q *sqlcgen.Queries) error {
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
			targets, err := resolveTargets(ctx, q, fact, row.TargetExists != 0, resolved)
			if err != nil {
				return err
			}
			for _, target := range targets {
				edge := graph.Edge{ID: graph.EdgeID(fact.ID, target), FactID: fact.ID,
					FromID: fact.FromID, ToID: target, Kind: fact.Kind, Location: fact.Location,
					Properties: fact.Properties}
				if err := q.InsertEdge(ctx, edgeParams(edge)); err != nil {
					return err
				}
			}
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

func markNodeDirty(ctx context.Context, q *sqlcgen.Queries, node graph.Node) error {
	if err := q.MarkDirtyNode(ctx, node.ID); err != nil {
		return err
	}
	for _, target := range []string{node.Name, node.QualifiedName} {
		if target == "" {
			continue
		}
		if err := q.MarkDirtyTarget(ctx, sqlcgen.MarkDirtyTargetParams{Target: target, TargetKind: string(node.Kind)}); err != nil {
			return err
		}
	}
	return nil
}

type resolutionKey struct {
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

func resolveTargets(ctx context.Context, q *sqlcgen.Queries, fact graph.Fact, targetExists bool, cache *resolutionCache) ([]string, error) {
	if fact.TargetID != "" {
		if targetExists {
			return []string{fact.TargetID}, nil
		}
		external := externalNode(fact)
		if err := q.UpsertNode(ctx, nodeParams(external, 1)); err != nil {
			return nil, err
		}
		return []string{external.ID}, nil
	}
	key := resolutionKey{target: fact.Target, targetKind: fact.TargetKind, edgeKind: fact.Kind}
	if targets, ok := cache.get(key); ok {
		return targets, nil
	}
	var targets []string
	var rows []resolutionCandidate
	if fact.Target != "" {
		if fact.TargetKind == "" {
			found, err := q.FindNodesExact(ctx, fact.Target)
			if err != nil {
				return nil, err
			}
			rows = make([]resolutionCandidate, 0, len(found))
			for _, row := range found {
				rows = append(rows, resolutionCandidate{id: row.ID, kind: graph.NodeKind(row.Kind), qualifiedName: row.QualifiedName})
			}
		} else {
			found, err := q.FindNodesExactKind(ctx, sqlcgen.FindNodesExactKindParams{
				Target: fact.Target, Kind: string(fact.TargetKind)})
			if err != nil {
				return nil, err
			}
			rows = make([]resolutionCandidate, 0, len(found))
			for _, row := range found {
				rows = append(rows, resolutionCandidate{id: row.ID, kind: graph.NodeKind(row.Kind), qualifiedName: row.QualifiedName})
			}
		}
		rows = filterCandidates(fact, rows)
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].qualifiedName == rows[j].qualifiedName {
				return rows[i].id < rows[j].id
			}
			return rows[i].qualifiedName < rows[j].qualifiedName
		})
		// A name shared by multiple declarations is not enough evidence to
		// invent a fan-out edge. Preserve one explicit unresolved target and
		// let a parser provide a qualified name when it can prove the binding.
		if len(rows) == 1 {
			targets = []string{rows[0].id}
		}
	}
	if len(targets) == 0 {
		external := externalNode(fact)
		if err := q.UpsertNode(ctx, nodeParams(external, 1)); err != nil {
			return nil, err
		}
		targets = []string{external.ID}
	}
	cache.set(key, targets)
	return targets, nil
}

func filterCandidates(fact graph.Fact, rows []resolutionCandidate) []resolutionCandidate {
	if fact.TargetKind != "" {
		return rows
	}
	allowed := func(kind graph.NodeKind) bool { return true }
	switch fact.Kind {
	case graph.EdgeCalls, graph.EdgePasses, graph.EdgeHandledBy:
		allowed = func(kind graph.NodeKind) bool { return kind == graph.KindFunction || kind == graph.KindMethod }
	case graph.EdgeReads, graph.EdgeWrites:
		allowed = func(kind graph.NodeKind) bool { return kind == graph.KindTable || kind == graph.KindView }
	case graph.EdgeReferences:
		allowed = func(kind graph.NodeKind) bool {
			return kind == graph.KindConfigKey || kind == graph.KindTable || kind == graph.KindView
		}
	case graph.EdgeImports, graph.EdgeDependsOn:
		allowed = func(kind graph.NodeKind) bool { return kind == graph.KindModule || kind == graph.KindPackage }
	case graph.EdgeExtends, graph.EdgeImplements, graph.EdgeEmbeds:
		allowed = func(kind graph.NodeKind) bool {
			return kind == graph.KindType || kind == graph.KindClass || kind == graph.KindInterface
		}
	}
	result := rows[:0]
	for _, row := range rows {
		if allowed(row.kind) {
			result = append(result, row)
		}
	}
	return result
}

func externalNode(fact graph.Fact) graph.Node {
	kind := fact.TargetKind
	if kind == "" {
		kind = graph.KindExternal
	}
	qualified := fact.Target
	if qualified == "" {
		qualified = fact.TargetID
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
	rows, err := r.queries.SearchNodes(ctx, sqlcgen.SearchNodesParams{Term: term, MaxResults: int64(limit)})
	if err != nil {
		return nil, err
	}
	result := make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		result = append(result, nodeFromRow(row))
	}
	return result, nil
}

func (r *Repository) Node(ctx context.Context, id string) (graph.Node, error) {
	row, err := r.queries.GetNode(ctx, id)
	if err != nil {
		return graph.Node{}, err
	}
	return nodeFromRow(row), nil
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

func (r *Repository) ExternalEdgesTo(ctx context.Context, node graph.Node) ([]graph.Edge, error) {
	rows, err := r.queries.ListExternalEdgesMatching(ctx, sqlcgen.ListExternalEdgesMatchingParams{
		QualifiedName: node.QualifiedName, Name: node.Name,
	})
	if err != nil {
		return nil, err
	}
	return edgesFromRows(rows), nil
}

func (r *Repository) inTransaction(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(r.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

func nodeParams(n graph.Node, external int64) sqlcgen.UpsertNodeParams {
	return sqlcgen.UpsertNodeParams{ID: n.ID, Kind: string(n.Kind), Name: n.Name,
		QualifiedName: n.QualifiedName, Language: n.Language, Path: n.Location.Path,
		Line: int64(n.Location.Line), ColumnNo: int64(n.Location.Column), EndLine: int64(n.Location.EndLine),
		Properties: graph.MarshalProperties(n.Properties), OwnerFile: n.OwnerFile, External: external}
}

func factParams(f graph.Fact) sqlcgen.UpsertFactParams {
	return sqlcgen.UpsertFactParams{ID: f.ID, FromID: f.FromID, Kind: string(f.Kind), TargetID: f.TargetID,
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
	return graph.Fact{ID: f.ID, FromID: f.FromID, Kind: graph.EdgeKind(f.Kind), TargetID: f.TargetID,
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
