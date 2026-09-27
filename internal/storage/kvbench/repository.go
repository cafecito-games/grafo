// Package kvbench provides benchmark-only graph repositories for embedded
// key/value engines. It deliberately is not wired into the Grafo CLI.
package kvbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cafecito-games/grafo/internal/graph"
)

const defaultReconciliationBatchSize = 10_000
const defaultNodeMatchLimit = 25

type Options struct {
	ReconciliationBatchSize int
}

type Repository struct {
	store     store
	engine    Engine
	path      string
	batchSize int

	statsMu sync.Mutex
	stats   graph.WriteStats
}

var _ graph.Repository = (*Repository)(nil)
var _ graph.InstrumentedIndexRepository = (*Repository)(nil)
var _ graph.InstrumentedWriteRepository = (*Repository)(nil)

type nodeRecord struct {
	Node  graph.Node `json:"node"`
	Owner string     `json:"owner"`
}

type factRecord struct {
	Fact  graph.Fact `json:"fact"`
	Owner string     `json:"owner"`
}

func Open(ctx context.Context, engine Engine, path string, options Options) (*Repository, error) {
	storage, err := openStore(engine, path)
	if err != nil {
		return nil, fmt.Errorf("open %s graph: %w", engine, err)
	}
	batchSize := options.ReconciliationBatchSize
	if batchSize <= 0 {
		batchSize = defaultReconciliationBatchSize
	}
	repository := &Repository{store: storage, engine: engine, path: path, batchSize: batchSize}
	if err := repository.SetMeta(ctx, "schema_version", fmt.Sprint(graph.SchemaVersion)); err != nil {
		storage.close()
		return nil, err
	}
	return repository, nil
}

func (r *Repository) Close() error   { return r.store.close() }
func (r *Repository) Path() string   { return r.path }
func (r *Repository) Engine() Engine { return r.engine }

func (r *Repository) WriteStats() graph.WriteStats {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	return r.stats
}

func (r *Repository) SetMeta(ctx context.Context, name, value string) error {
	return r.store.update(ctx, func(tx transaction) error { return tx.set(key("meta", name), []byte(value)) })
}

func (r *Repository) Meta(ctx context.Context, name string) (string, error) {
	var value string
	err := r.store.view(ctx, func(tx transaction) error {
		raw, err := tx.get(key("meta", name))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		value = string(raw)
		return err
	})
	return value, err
}

func (r *Repository) Files(ctx context.Context) (map[string]graph.FileRecord, error) {
	result := map[string]graph.FileRecord{}
	err := r.store.view(ctx, func(tx transaction) error {
		return tx.scan(prefix("file"), func(_ []byte, value []byte) error {
			var file graph.FileRecord
			if err := json.Unmarshal(value, &file); err != nil {
				return err
			}
			result[file.Path] = file
			return nil
		})
	})
	return result, err
}

func (r *Repository) ReplaceFile(ctx context.Context, file graph.FileRecord, parsed graph.ParseResult) error {
	stats := graph.WriteStats{}
	err := r.store.update(ctx, func(tx transaction) error {
		if err := r.removeOwner(tx, file.Path); err != nil {
			return err
		}
		encoded, err := json.Marshal(file)
		if err != nil {
			return err
		}
		if err := tx.set(key("file", file.Path), encoded); err != nil {
			return err
		}
		return r.insertParseResult(tx, parsed, &stats)
	})
	if err == nil {
		r.addStats(stats)
	}
	return err
}

func (r *Repository) ReplaceOwner(ctx context.Context, owner string, parsed graph.ParseResult) error {
	stats := graph.WriteStats{}
	err := r.store.update(ctx, func(tx transaction) error {
		if err := r.removeOwner(tx, owner); err != nil {
			return err
		}
		return r.insertParseResult(tx, parsed, &stats)
	})
	if err == nil {
		r.addStats(stats)
	}
	return err
}

func (r *Repository) RemoveFiles(ctx context.Context, paths []string) error {
	return r.store.update(ctx, func(tx transaction) error {
		for _, path := range paths {
			if err := r.removeOwner(tx, path); err != nil {
				return err
			}
			if err := tx.delete(key("file", path)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repository) insertParseResult(tx transaction, parsed graph.ParseResult, stats *graph.WriteStats) error {
	for _, node := range parsed.Nodes {
		encoded, err := r.putNode(tx, node)
		if err != nil {
			return fmt.Errorf("upsert node %s: %w", node.QualifiedName, err)
		}
		stats.Nodes.Rows++
		stats.Nodes.Bytes += int64(len(encoded))
		if !node.External {
			if err := markNodeDirty(tx, node); err != nil {
				return err
			}
		}
	}
	for _, fact := range parsed.Facts {
		encoded, err := r.putFact(tx, fact)
		if err != nil {
			return fmt.Errorf("upsert fact %s: %w", fact.ID, err)
		}
		stats.Facts.Rows++
		stats.Facts.Bytes += int64(len(encoded))
	}
	if stats.Nodes.Rows > 0 {
		stats.Nodes.Batches++
	}
	if stats.Facts.Rows > 0 {
		stats.Facts.Batches++
	}
	return nil
}

func (r *Repository) removeOwner(tx transaction, owner string) error {
	if err := tx.set(key("dirty_owner", owner), nil); err != nil {
		return err
	}
	var nodes []graph.Node
	if err := tx.scan(prefix("node_owner", owner), func(_ []byte, value []byte) error {
		node, err := getNode(tx, string(value))
		if err == nil {
			nodes = append(nodes, node)
		}
		return err
	}); err != nil {
		return err
	}
	for _, node := range nodes {
		if err := markNodeDirty(tx, node); err != nil {
			return err
		}
		if err := deleteNode(tx, node); err != nil {
			return err
		}
	}
	var facts []graph.Fact
	if err := tx.scan(prefix("fact_owner", owner), func(_ []byte, value []byte) error {
		fact, err := getFact(tx, string(value))
		if err == nil {
			facts = append(facts, fact)
		}
		return err
	}); err != nil {
		return err
	}
	for _, fact := range facts {
		if err := deleteEdgesByFact(tx, fact.ID); err != nil {
			return err
		}
		if err := deleteFact(tx, fact); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) Reconcile(ctx context.Context) error {
	_, err := r.ReconcileWithStats(ctx, nil)
	return err
}

func (r *Repository) ReconcileWithStats(ctx context.Context, observer graph.ReconciliationObserver) (graph.ReconciliationStats, error) {
	var result graph.ReconciliationStats
	if err := r.queueDirtyFacts(ctx); err != nil {
		return result, fmt.Errorf("queue dirty facts: %w", err)
	}
	for {
		processed, writes, err := r.reconcileBatch(ctx)
		if err != nil {
			return result, fmt.Errorf("reconcile fact batch: %w", err)
		}
		if processed == 0 {
			break
		}
		result.Batches++
		addStats(&result.Writes, writes)
		r.addStats(writes)
		if observer != nil {
			if err := observer(result); err != nil {
				return result, err
			}
		}
	}
	if err := r.cleanupExternal(ctx); err != nil {
		return result, fmt.Errorf("remove orphan external nodes: %w", err)
	}
	return result, nil
}

func (r *Repository) queueDirtyFacts(ctx context.Context) error {
	return r.store.update(ctx, func(tx transaction) error {
		var stale [][]byte
		if err := tx.scan(prefix("dirty_fact"), func(k, value []byte) error {
			if _, err := tx.get(key("fact", string(value))); errors.Is(err, ErrNotFound) {
				stale = append(stale, k)
				return nil
			} else {
				return err
			}
		}); err != nil {
			return err
		}
		for _, k := range stale {
			if err := tx.delete(k); err != nil {
				return err
			}
		}
		if err := r.enqueueFromSet(tx, "dirty_owner", func(value string) ([]graph.Fact, error) {
			return factsFromIndex(tx, prefix("fact_owner", value))
		}); err != nil {
			return err
		}
		if err := r.enqueueFromSet(tx, "dirty_node", func(value string) ([]graph.Fact, error) {
			return factsFromIndex(tx, prefix("fact_target_id", value))
		}); err != nil {
			return err
		}
		if err := tx.scan(prefix("dirty_target"), func(k, _ []byte) error {
			parts := splitKey(k)
			if len(parts) != 3 {
				return fmt.Errorf("invalid dirty target key %q", k)
			}
			facts, err := factsFromIndex(tx, prefix("fact_target", parts[1]))
			if err != nil {
				return err
			}
			for _, fact := range facts {
				if fact.TargetKind == "" || string(fact.TargetKind) == parts[2] {
					if err := enqueueFact(tx, fact); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		hasDirty := false
		if err := tx.scan(prefix("dirty_fact"), func(_, _ []byte) error { hasDirty = true; return nil }); err != nil {
			return err
		}
		if hasDirty {
			if err := tx.set(key("state", "cleanup"), []byte("1")); err != nil {
				return err
			}
		}
		for _, set := range []string{"dirty_owner", "dirty_node", "dirty_target"} {
			if err := deletePrefix(tx, prefix(set)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repository) enqueueFromSet(tx transaction, set string, resolve func(string) ([]graph.Fact, error)) error {
	return tx.scan(prefix(set), func(k, _ []byte) error {
		parts := splitKey(k)
		if len(parts) != 2 {
			return fmt.Errorf("invalid %s key %q", set, k)
		}
		facts, err := resolve(parts[1])
		if err != nil {
			return err
		}
		for _, fact := range facts {
			if err := enqueueFact(tx, fact); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repository) reconcileBatch(ctx context.Context) (int, graph.WriteStats, error) {
	processed := 0
	writes := graph.WriteStats{}
	err := r.store.update(ctx, func(tx transaction) error {
		type queued struct {
			key  []byte
			fact graph.Fact
		}
		var batch []queued
		if err := tx.scan(prefix("dirty_fact"), func(k, value []byte) error {
			if len(batch) >= r.batchSize {
				return nil
			}
			fact, err := getFact(tx, string(value))
			if err != nil {
				return err
			}
			batch = append(batch, queued{key: k, fact: fact})
			return nil
		}); err != nil {
			return err
		}
		for _, item := range batch {
			if err := deleteEdgesByFact(tx, item.fact.ID); err != nil {
				return err
			}
			targets, err := r.resolveTargets(tx, item.fact)
			if err != nil {
				return err
			}
			for _, target := range targets {
				edge := graph.Edge{ID: graph.EdgeID(item.fact.ID, target), FactID: item.fact.ID, FromID: item.fact.FromID,
					ToID: target, Kind: item.fact.Kind, Location: item.fact.Location, Properties: item.fact.Properties}
				encoded, err := putEdge(tx, edge)
				if err != nil {
					return err
				}
				writes.Edges.Rows++
				writes.Edges.Bytes += int64(len(encoded))
			}
			if err := tx.delete(item.key); err != nil {
				return err
			}
			processed++
		}
		if writes.Edges.Rows > 0 {
			writes.Edges.Batches++
		}
		return nil
	})
	return processed, writes, err
}

func (r *Repository) resolveTargets(tx transaction, fact graph.Fact) ([]string, error) {
	if fact.TargetID != "" {
		if _, err := getNode(tx, fact.TargetID); err == nil {
			return []string{fact.TargetID}, nil
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return r.ensureExternal(tx, fact)
	}
	candidates := map[string]graph.Node{}
	for _, index := range []string{"node_qname", "node_name"} {
		nodes, err := nodesFromIndex(tx, prefix(index, fact.Target))
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			if node.External || fact.TargetKind != "" && node.Kind != fact.TargetKind ||
				fact.TargetKind == "" && !allowsKind(fact.Kind, node.Kind) {
				continue
			}
			candidates[node.ID] = node
		}
	}
	if len(candidates) == 1 {
		for id := range candidates {
			return []string{id}, nil
		}
	}
	return r.ensureExternal(tx, fact)
}

func (r *Repository) ensureExternal(tx transaction, fact graph.Fact) ([]string, error) {
	kind := fact.TargetKind
	if kind == "" {
		kind = graph.KindExternal
	}
	qualified := fact.Target
	if qualified == "" {
		qualified = fact.TargetID
	}
	node := graph.Node{ID: graph.NodeID(kind, "external:"+qualified), Kind: kind, Name: graph.SimpleName(qualified),
		QualifiedName: qualified, OwnerFile: "__external__", External: true, Properties: map[string]string{"unresolved": "true"}}
	if _, err := r.putNode(tx, node); err != nil {
		return nil, err
	}
	return []string{node.ID}, nil
}

func (r *Repository) cleanupExternal(ctx context.Context) error {
	return r.store.update(ctx, func(tx transaction) error {
		if _, err := tx.get(key("state", "cleanup")); errors.Is(err, ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		nodes, err := nodesFromIndex(tx, prefix("node_external"))
		if err != nil {
			return err
		}
		for _, node := range nodes {
			referenced := false
			for _, index := range []string{"edge_from", "edge_to"} {
				if err := tx.scan(prefix(index, node.ID), func(_, _ []byte) error { referenced = true; return nil }); err != nil {
					return err
				}
			}
			if !referenced {
				if err := deleteNode(tx, node); err != nil {
					return err
				}
			}
		}
		return tx.delete(key("state", "cleanup"))
	})
}

func (r *Repository) Counts(ctx context.Context) (graph.Counts, error) {
	result := graph.Counts{ByKind: map[string]int{}, ByEdge: map[string]int{}}
	err := r.store.view(ctx, func(tx transaction) error {
		for collection, destination := range map[string]*int{"file": &result.Files, "node": &result.Nodes, "fact": &result.Facts, "edge": &result.Edges} {
			if err := tx.scan(prefix(collection), func(_, _ []byte) error { *destination++; return nil }); err != nil {
				return err
			}
		}
		if err := tx.scan(prefix("node"), func(_ []byte, value []byte) error {
			node, err := decodeNode(value)
			if err != nil {
				return err
			}
			result.ByKind[string(node.Kind)]++
			if node.External {
				result.External++
			}
			return nil
		}); err != nil {
			return err
		}
		return tx.scan(prefix("edge"), func(_ []byte, value []byte) error {
			var edge graph.Edge
			if err := json.Unmarshal(value, &edge); err != nil {
				return err
			}
			result.ByEdge[string(edge.Kind)]++
			return nil
		})
	})
	return result, err
}

func (r *Repository) SearchNodes(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	nodes := make([]graph.Node, 0)
	needle := strings.ToLower(term)
	err := r.store.view(ctx, func(tx transaction) error {
		return tx.scan(prefix("node"), func(_ []byte, value []byte) error {
			node, err := decodeNode(value)
			if err == nil && (strings.Contains(strings.ToLower(node.Name), needle) || strings.Contains(strings.ToLower(node.QualifiedName), needle)) {
				nodes = append(nodes, node)
			}
			return err
		})
	})
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].External != nodes[j].External {
			return !nodes[i].External
		}
		leftLength, rightLength := utf8.RuneCountInString(nodes[i].QualifiedName), utf8.RuneCountInString(nodes[j].QualifiedName)
		if leftLength != rightLength {
			return leftLength < rightLength
		}
		if nodes[i].QualifiedName != nodes[j].QualifiedName {
			return nodes[i].QualifiedName < nodes[j].QualifiedName
		}
		return nodes[i].ID < nodes[j].ID
	})
	if limit >= 0 && len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes, err
}

func (r *Repository) MatchNodes(ctx context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
	selector := strings.TrimSpace(request.Selector)
	if selector == "" {
		return graph.NodeMatchGroup{}, nil
	}
	limit := request.Limit
	if limit <= 0 {
		limit = defaultNodeMatchLimit
	}
	scopes := []struct {
		level    graph.NodeMatchLevel
		external bool
	}{
		{graph.MatchQualifiedName, false},
		{graph.MatchQualifiedName, true},
		{graph.MatchName, false},
		{graph.MatchName, true},
		{graph.MatchSubstring, false},
		{graph.MatchSubstring, true},
	}
	var fallback graph.NodeMatchGroup
	for _, scope := range scopes {
		group := graph.NodeMatchGroup{Level: scope.level, External: scope.external}
		err := r.store.view(ctx, func(tx transaction) error {
			return tx.scan(prefix("node"), func(_ []byte, value []byte) error {
				node, err := decodeNode(value)
				if err != nil {
					return err
				}
				if node.External != scope.external || request.Kind != "" && node.Kind != request.Kind ||
					!graph.LooseMatch(scope.level, selector, node) {
					return nil
				}
				group.Total++
				if graph.StrictMatch(scope.level, selector, node) {
					group.Strict++
				}
				group.Nodes = append(group.Nodes, node)
				return nil
			})
		})
		if err != nil {
			return graph.NodeMatchGroup{}, err
		}
		graph.SortNodeMatches(scope.level, selector, group.Nodes)
		if len(group.Nodes) > limit {
			group.Nodes = group.Nodes[:limit]
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

func (r *Repository) Node(ctx context.Context, id string) (graph.Node, error) {
	var node graph.Node
	err := r.store.view(ctx, func(tx transaction) error {
		var err error
		node, err = getNode(tx, id)
		return err
	})
	return node, err
}

func (r *Repository) EdgesFrom(ctx context.Context, id string) ([]graph.Edge, error) {
	return r.edgesFor(ctx, "edge_from", id)
}

func (r *Repository) EdgesTo(ctx context.Context, id string) ([]graph.Edge, error) {
	return r.edgesFor(ctx, "edge_to", id)
}

func (r *Repository) edgesFor(ctx context.Context, index, id string) ([]graph.Edge, error) {
	var edges []graph.Edge
	err := r.store.view(ctx, func(tx transaction) error {
		var err error
		edges, err = edgesFromIndex(tx, prefix(index, id))
		return err
	})
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		if index == "edge_from" && edges[i].ToID != edges[j].ToID {
			return edges[i].ToID < edges[j].ToID
		}
		if index == "edge_to" && edges[i].FromID != edges[j].FromID {
			return edges[i].FromID < edges[j].FromID
		}
		return edges[i].ID < edges[j].ID
	})
	return edges, err
}

func (r *Repository) ExternalEdgesTo(ctx context.Context, target graph.Node) ([]graph.Edge, error) {
	seen := map[string]graph.Edge{}
	err := r.store.view(ctx, func(tx transaction) error {
		for _, index := range []string{"node_qname", "node_name"} {
			for _, value := range []string{target.QualifiedName, target.Name} {
				nodes, err := nodesFromIndex(tx, prefix(index, value))
				if err != nil {
					return err
				}
				for _, node := range nodes {
					if !node.External {
						continue
					}
					edges, err := edgesFromIndex(tx, prefix("edge_to", node.ID))
					if err != nil {
						return err
					}
					for _, edge := range edges {
						seen[edge.ID] = edge
					}
				}
			}
		}
		return nil
	})
	edges := make([]graph.Edge, 0, len(seen))
	for _, edge := range seen {
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		if edges[i].FromID != edges[j].FromID {
			return edges[i].FromID < edges[j].FromID
		}
		return edges[i].ID < edges[j].ID
	})
	return edges, err
}

func (r *Repository) putNode(tx transaction, node graph.Node) ([]byte, error) {
	if existing, err := getNode(tx, node.ID); err == nil {
		if err := deleteNodeIndexes(tx, existing); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	record := nodeRecord{Node: node, Owner: node.OwnerFile}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := tx.set(key("node", node.ID), encoded); err != nil {
		return nil, err
	}
	for _, entry := range [][]byte{key("node_owner", node.OwnerFile, node.ID), key("node_name", node.Name, node.ID), key("node_qname", node.QualifiedName, node.ID)} {
		if err := tx.set(entry, []byte(node.ID)); err != nil {
			return nil, err
		}
	}
	if node.External {
		if err := tx.set(key("node_external", node.ID), []byte(node.ID)); err != nil {
			return nil, err
		}
	}
	return encoded, nil
}

func (r *Repository) putFact(tx transaction, fact graph.Fact) ([]byte, error) {
	if existing, err := getFact(tx, fact.ID); err == nil {
		if err := deleteFactIndexes(tx, existing); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	record := factRecord{Fact: fact, Owner: fact.OwnerFile}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := tx.set(key("fact", fact.ID), encoded); err != nil {
		return nil, err
	}
	for _, entry := range [][]byte{key("fact_owner", fact.OwnerFile, fact.ID), key("fact_target_id", fact.TargetID, fact.ID), key("fact_target", fact.Target, fact.ID)} {
		if err := tx.set(entry, []byte(fact.ID)); err != nil {
			return nil, err
		}
	}
	return encoded, nil
}

func putEdge(tx transaction, edge graph.Edge) ([]byte, error) {
	if existing, err := getEdge(tx, edge.ID); err == nil {
		if err := deleteEdge(tx, existing); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	encoded, err := json.Marshal(edge)
	if err != nil {
		return nil, err
	}
	if err := tx.set(key("edge", edge.ID), encoded); err != nil {
		return nil, err
	}
	for _, entry := range [][]byte{key("edge_fact", edge.FactID, edge.ID), key("edge_from", edge.FromID, edge.ID), key("edge_to", edge.ToID, edge.ID)} {
		if err := tx.set(entry, []byte(edge.ID)); err != nil {
			return nil, err
		}
	}
	return encoded, nil
}

func getNode(tx transaction, id string) (graph.Node, error) {
	raw, err := tx.get(key("node", id))
	if err != nil {
		return graph.Node{}, err
	}
	return decodeNode(raw)
}
func decodeNode(raw []byte) (graph.Node, error) {
	var record nodeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return graph.Node{}, err
	}
	record.Node.OwnerFile = record.Owner
	return record.Node, nil
}
func getFact(tx transaction, id string) (graph.Fact, error) {
	raw, err := tx.get(key("fact", id))
	if err != nil {
		return graph.Fact{}, err
	}
	var record factRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return graph.Fact{}, err
	}
	record.Fact.OwnerFile = record.Owner
	return record.Fact, nil
}
func getEdge(tx transaction, id string) (graph.Edge, error) {
	raw, err := tx.get(key("edge", id))
	if err != nil {
		return graph.Edge{}, err
	}
	var edge graph.Edge
	err = json.Unmarshal(raw, &edge)
	return edge, err
}

func deleteNode(tx transaction, node graph.Node) error {
	if err := deleteNodeIndexes(tx, node); err != nil {
		return err
	}
	return tx.delete(key("node", node.ID))
}
func deleteNodeIndexes(tx transaction, node graph.Node) error {
	for _, entry := range [][]byte{key("node_owner", node.OwnerFile, node.ID), key("node_name", node.Name, node.ID), key("node_qname", node.QualifiedName, node.ID), key("node_external", node.ID)} {
		if err := tx.delete(entry); err != nil {
			return err
		}
	}
	return nil
}
func deleteFact(tx transaction, fact graph.Fact) error {
	if err := deleteFactIndexes(tx, fact); err != nil {
		return err
	}
	return tx.delete(key("fact", fact.ID))
}
func deleteFactIndexes(tx transaction, fact graph.Fact) error {
	for _, entry := range [][]byte{key("fact_owner", fact.OwnerFile, fact.ID), key("fact_target_id", fact.TargetID, fact.ID), key("fact_target", fact.Target, fact.ID)} {
		if err := tx.delete(entry); err != nil {
			return err
		}
	}
	return nil
}
func deleteEdge(tx transaction, edge graph.Edge) error {
	for _, entry := range [][]byte{key("edge_fact", edge.FactID, edge.ID), key("edge_from", edge.FromID, edge.ID), key("edge_to", edge.ToID, edge.ID), key("edge", edge.ID)} {
		if err := tx.delete(entry); err != nil {
			return err
		}
	}
	return nil
}
func deleteEdgesByFact(tx transaction, factID string) error {
	edges, err := edgesFromIndex(tx, prefix("edge_fact", factID))
	if err != nil {
		return err
	}
	for _, edge := range edges {
		if err := deleteEdge(tx, edge); err != nil {
			return err
		}
	}
	return nil
}

func nodesFromIndex(tx transaction, p []byte) ([]graph.Node, error) {
	var result []graph.Node
	err := tx.scan(p, func(_, value []byte) error {
		node, err := getNode(tx, string(value))
		if err == nil {
			result = append(result, node)
		}
		return err
	})
	return result, err
}
func factsFromIndex(tx transaction, p []byte) ([]graph.Fact, error) {
	var result []graph.Fact
	err := tx.scan(p, func(_, value []byte) error {
		fact, err := getFact(tx, string(value))
		if err == nil {
			result = append(result, fact)
		}
		return err
	})
	return result, err
}
func edgesFromIndex(tx transaction, p []byte) ([]graph.Edge, error) {
	result := make([]graph.Edge, 0)
	err := tx.scan(p, func(_, value []byte) error {
		edge, err := getEdge(tx, string(value))
		if err == nil {
			result = append(result, edge)
		}
		return err
	})
	return result, err
}

func enqueueFact(tx transaction, fact graph.Fact) error {
	return tx.set(key("dirty_fact", fact.OwnerFile, fact.ID), []byte(fact.ID))
}
func markNodeDirty(tx transaction, node graph.Node) error {
	if err := tx.set(key("dirty_node", node.ID), nil); err != nil {
		return err
	}
	for _, target := range []string{node.Name, node.QualifiedName} {
		if target != "" {
			if err := tx.set(key("dirty_target", target, string(node.Kind)), nil); err != nil {
				return err
			}
		}
	}
	return nil
}
func deletePrefix(tx transaction, p []byte) error {
	var keys [][]byte
	if err := tx.scan(p, func(k, _ []byte) error { keys = append(keys, k); return nil }); err != nil {
		return err
	}
	for _, k := range keys {
		if err := tx.delete(k); err != nil {
			return err
		}
	}
	return nil
}

func allowsKind(edge graph.EdgeKind, kind graph.NodeKind) bool {
	switch edge {
	case graph.EdgeCalls, graph.EdgePasses, graph.EdgeHandledBy:
		return kind == graph.KindFunction || kind == graph.KindMethod
	case graph.EdgeReads, graph.EdgeWrites:
		return kind == graph.KindTable || kind == graph.KindView
	case graph.EdgeReferences:
		return kind == graph.KindConfigKey || kind == graph.KindTable || kind == graph.KindView
	case graph.EdgeImports, graph.EdgeDependsOn:
		return kind == graph.KindModule || kind == graph.KindPackage
	case graph.EdgeExtends, graph.EdgeImplements, graph.EdgeEmbeds:
		return kind == graph.KindType || kind == graph.KindClass || kind == graph.KindInterface
	default:
		return true
	}
}

const keySeparator = "\x00"

func key(collection string, parts ...string) []byte {
	return []byte(collection + keySeparator + strings.Join(parts, keySeparator))
}
func prefix(collection string, parts ...string) []byte {
	value := key(collection, parts...)
	if len(parts) > 0 {
		value = append(value, 0)
	}
	return value
}
func splitKey(value []byte) []string { return strings.Split(string(value), keySeparator) }

func (r *Repository) addStats(stats graph.WriteStats) {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	addStats(&r.stats, stats)
}
func addStats(target *graph.WriteStats, delta graph.WriteStats) {
	for _, pair := range []struct{ target, delta *graph.WriteBatchStats }{{&target.Nodes, &delta.Nodes}, {&target.Facts, &delta.Facts}, {&target.Edges, &delta.Edges}} {
		pair.target.Batches += pair.delta.Batches
		pair.target.Rows += pair.delta.Rows
		pair.target.Bytes += pair.delta.Bytes
	}
}
