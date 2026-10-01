package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestReconciliationQueueSurvivesRepositoryRestart(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	caller := graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "Caller",
		QualifiedName: "sample.Caller", OwnerFile: "caller.go"}
	target := graph.Node{ID: "target", Kind: graph.KindFunction, Name: "Target",
		QualifiedName: "sample.Target", OwnerFile: "target.go"}
	if err := repository.ReplaceOwner(ctx, "caller.go", graph.ParseResult{
		Nodes: []graph.Node{caller},
		Facts: []graph.Fact{{ID: "call", FromID: caller.ID, Kind: graph.EdgeCalls,
			Target: target.QualifiedName, OwnerFile: "caller.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "target.go", graph.ParseResult{Nodes: []graph.Node{target}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.queueDirtyFacts(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := repository.queries.CountDirtyFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending facts = %d, want 1", pending)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID != target.ID {
		t.Fatalf("queued fact was not resumed: %#v", edges)
	}
}

func TestCompletedReconciliationClearsCleanupMarker(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	node := graph.Node{ID: "node", Kind: graph.KindFunction, Name: "Node",
		QualifiedName: "sample.Node", OwnerFile: "node.go"}
	if err := repository.ReplaceOwner(ctx, "node.go", graph.ParseResult{Nodes: []graph.Node{node}, Facts: []graph.Fact{{
		ID: "self", FromID: node.ID, Kind: graph.EdgeCalls, TargetID: node.ID, OwnerFile: "node.go",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := repository.queries.ReconciliationCleanupPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("completed reconciliation retained cleanup marker")
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatalf("no-op reconciliation failed: %v", err)
	}
}

func TestResolutionCacheIsBoundedAndLeastRecentlyUsed(t *testing.T) {
	cache := newResolutionCache(2)
	first := resolutionKey{target: "first"}
	second := resolutionKey{target: "second"}
	third := resolutionKey{target: "third"}
	cache.set(first, []string{"1"})
	cache.set(second, []string{"2"})
	if _, ok := cache.get(first); !ok {
		t.Fatal("first key was not cached")
	}
	cache.set(third, []string{"3"})
	if _, ok := cache.get(second); ok {
		t.Fatal("least recently used key was not evicted")
	}
	if cache.recent.Len() != 2 || len(cache.entries) != 2 {
		t.Fatalf("cache exceeded capacity: list=%d map=%d", cache.recent.Len(), len(cache.entries))
	}
}
