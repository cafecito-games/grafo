package sqlite_test

import (
	"context"
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
