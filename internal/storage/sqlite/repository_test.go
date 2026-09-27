package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestRepositoryMigratesAndReconcilesFacts(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	a := graph.Node{ID: graph.NodeID(graph.KindFunction, "sample.A"), Kind: graph.KindFunction,
		Name: "A", QualifiedName: "sample.A", OwnerFile: "a.go"}
	b := graph.Node{ID: graph.NodeID(graph.KindFunction, "sample.B"), Kind: graph.KindFunction,
		Name: "B", QualifiedName: "sample.B", OwnerFile: "b.go"}
	if err := repository.ReplaceOwner(ctx, "a.go", graph.ParseResult{
		Nodes: []graph.Node{a},
		Facts: []graph.Fact{{
			ID:     graph.FactID("a.go", a.ID, graph.EdgeCalls, "sample.B", 4, 1),
			FromID: a.ID, Kind: graph.EdgeCalls, Target: "sample.B", OwnerFile: "a.go",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "b.go", graph.ParseResult{Nodes: []graph.Node{b}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID != b.ID {
		t.Fatalf("expected A -> B, got %#v", edges)
	}

	if err := repository.ReplaceOwner(ctx, "b.go", graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err = repository.EdgesFrom(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID == b.ID {
		t.Fatalf("expected an explicit unresolved target, got %#v", edges)
	}
	target, err := repository.Node(ctx, edges[0].ToID)
	if err != nil {
		t.Fatal(err)
	}
	if !target.External || target.QualifiedName != "sample.B" {
		t.Fatalf("unexpected unresolved node: %#v", target)
	}
}

func TestRepositoryReconcilesMoreThanOneBatchAndTruncatesWAL(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "graph.sqlite")
	repository, err := sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	caller := graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "Caller",
		QualifiedName: "sample.Caller", OwnerFile: "large.go"}
	target := graph.Node{ID: "target", Kind: graph.KindFunction, Name: "Target",
		QualifiedName: "sample.Target", OwnerFile: "large.go"}
	facts := make([]graph.Fact, 10_205)
	for index := range facts {
		facts[index] = graph.Fact{ID: fmt.Sprintf("call-%04d", index), FromID: caller.ID,
			Kind: graph.EdgeCalls, TargetID: target.ID, OwnerFile: "large.go"}
	}
	if err := repository.ReplaceOwner(ctx, "large.go", graph.ParseResult{
		Nodes: []graph.Node{caller, target}, Facts: facts,
	}); err != nil {
		t.Fatal(err)
	}
	var observed []int
	stats, err := repository.ReconcileWithStats(ctx, func(stats graph.ReconciliationStats) error {
		observed = append(observed, stats.Batches)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Batches != 2 || len(observed) != 2 || observed[0] != 1 || observed[1] != 2 {
		t.Fatalf("unexpected reconciliation progress: stats=%#v observed=%v", stats, observed)
	}
	edges, err := repository.EdgesFrom(ctx, caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != len(facts) {
		t.Fatalf("reconciled %d edges, want %d", len(edges), len(facts))
	}
	if info, err := os.Stat(databasePath + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("WAL was not truncated after reconciliation: %d bytes", info.Size())
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestRepositoryRestrictsSQLAccessToDataResources(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	reader := graph.Node{ID: "reader", Kind: graph.KindFunction, Name: "Load",
		QualifiedName: "queries.Load", OwnerFile: "queries.sql"}
	table := graph.Node{ID: "table", Kind: graph.KindTable, Name: "accounts",
		QualifiedName: "public.accounts", OwnerFile: "schema.sql"}
	function := graph.Node{ID: "function", Kind: graph.KindFunction, Name: "accounts",
		QualifiedName: "public.accounts", OwnerFile: "functions.sql"}
	if err := repository.ReplaceOwner(ctx, "queries.sql", graph.ParseResult{Nodes: []graph.Node{reader}, Facts: []graph.Fact{{
		ID: "reads-accounts", FromID: reader.ID, Kind: graph.EdgeReads,
		Target: "public.accounts", OwnerFile: "queries.sql",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "schema.sql", graph.ParseResult{Nodes: []graph.Node{table}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "functions.sql", graph.ParseResult{Nodes: []graph.Node{function}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, reader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID != table.ID {
		t.Fatalf("SQL read resolved outside data resources: %#v", edges)
	}
}

func TestRepositoryKeepsAmbiguousSymbolicTargetsUnresolved(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	caller := graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "Caller",
		QualifiedName: "sample.Caller", OwnerFile: "caller.go"}
	first := graph.Node{ID: "first", Kind: graph.KindFunction, Name: "String",
		QualifiedName: "first.String", OwnerFile: "first.go"}
	second := graph.Node{ID: "second", Kind: graph.KindFunction, Name: "String",
		QualifiedName: "second.String", OwnerFile: "second.go"}
	if err := repository.ReplaceOwner(ctx, "caller.go", graph.ParseResult{
		Nodes: []graph.Node{caller}, Facts: []graph.Fact{{ID: "ambiguous-call", FromID: caller.ID,
			Kind: graph.EdgeCalls, Target: "String", OwnerFile: "caller.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "first.go", graph.ParseResult{Nodes: []graph.Node{first}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "second.go", graph.ParseResult{Nodes: []graph.Node{second}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID == first.ID || edges[0].ToID == second.ID {
		t.Fatalf("ambiguous call invented declaration edges: %#v", edges)
	}
	target, err := repository.Node(ctx, edges[0].ToID)
	if err != nil {
		t.Fatal(err)
	}
	if !target.External || target.QualifiedName != "String" {
		t.Fatalf("ambiguous target was not explicit: %#v", target)
	}
}
