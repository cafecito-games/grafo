package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestListNodesByKindEnumeratesExactKinds(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, "root", "/tmp/example/checkout"); err != nil {
		t.Fatal(err)
	}
	orders := graph.Node{ID: graph.NodeID(graph.KindTable, "orders"), Kind: graph.KindTable,
		Name: "orders", QualifiedName: "orders", OwnerFile: "schema.sql"}
	summary := graph.Node{ID: graph.NodeID(graph.KindView, "order_summary"), Kind: graph.KindView,
		Name: "order_summary", QualifiedName: "order_summary", OwnerFile: "schema.sql"}
	column := graph.Node{ID: graph.NodeID(graph.KindColumn, "orders.id"), Kind: graph.KindColumn,
		Name: "id", QualifiedName: "orders.id", OwnerFile: "schema.sql"}
	if err := repository.ReplaceOwner(ctx, "schema.sql", graph.ParseResult{
		Nodes: []graph.Node{orders, summary, column},
		Facts: []graph.Fact{{
			ID:     graph.FactID("schema.sql", summary.ID, graph.EdgeReads, "archived_orders", 1, 1),
			FromID: summary.ID, Kind: graph.EdgeReads, Target: "archived_orders",
			TargetKind: graph.KindTable, OwnerFile: "schema.sql",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	names, err := repository.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "checkout" {
		t.Fatalf("unexpected repository attribution: %#v", names)
	}

	tables, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: graph.DataResourceKinds(), Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 2 {
		t.Fatalf("expected the table and the view, got %#v", tables)
	}
	for _, scoped := range tables {
		if scoped.Repository != "checkout" {
			t.Fatalf("node %s lost repository attribution", scoped.Node.ID)
		}
		if scoped.Node.External {
			t.Fatalf("external node %s leaked into a local listing", scoped.Node.ID)
		}
	}

	withExternal, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: []graph.NodeKind{graph.KindTable}, Visibility: graph.AllNodes, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(withExternal) != 2 {
		t.Fatalf("expected the local and the unresolved table, got %#v", withExternal)
	}

	filtered, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: graph.DataResourceKinds(), Name: "SUMMARY", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Node.QualifiedName != "order_summary" {
		t.Fatalf("name filter is not case-insensitive: %#v", filtered)
	}

	literal, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: graph.DataResourceKinds(), Name: "%", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(literal) != 0 {
		t.Fatalf("a name fragment acted as a wildcard: %#v", literal)
	}

	bounded, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: []graph.NodeKind{graph.KindTable}, Visibility: graph.AllNodes, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 1 {
		t.Fatalf("limit was not applied per kind: %#v", bounded)
	}

	other, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: graph.DataResourceKinds(), Repository: "billing", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("repository filter matched a foreign repository: %#v", other)
	}
}

func TestNameMatchingUsesUnicodeLowercase(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, "root", "/tmp/example/checkout"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		nodeName  string
		qualified string
		fragment  string
	}{
		{name: "ASCII", nodeName: "Orders", qualified: "Sales.Orders", fragment: "orders"},
		{name: "non-ASCII uppercase", nodeName: "École", qualified: "Sales.École", fragment: "école"},
		{name: "Kelvin sign", nodeName: "Kelvin", qualified: "Units.Kelvin", fragment: "kelvin"},
	}
	nodes := make([]graph.Node, 0, len(tests))
	for _, test := range tests {
		nodes = append(nodes, graph.Node{
			ID:            graph.NodeID(graph.KindTable, test.qualified),
			Kind:          graph.KindTable,
			Name:          test.nodeName,
			QualifiedName: test.qualified,
			OwnerFile:     "schema.sql",
		})
	}
	if err := repository.ReplaceOwner(ctx, "schema.sql", graph.ParseResult{Nodes: nodes}); err != nil {
		t.Fatal(err)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			searched, err := repository.SearchNodes(ctx, test.fragment, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(searched) != 1 || searched[0].QualifiedName != test.qualified {
				t.Fatalf("SearchNodes(%q) = %#v, want %q", test.fragment, searched, test.qualified)
			}

			listed, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
				Kinds: []graph.NodeKind{graph.KindTable}, Name: test.fragment, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Node.QualifiedName != test.qualified {
				t.Fatalf("ListNodesByKind(%q) = %#v, want %q", test.fragment, listed, test.qualified)
			}
		})
	}
}
