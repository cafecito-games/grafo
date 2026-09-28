package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestRelationEdgesBoundsEachRelationAndHydratesCounterparts(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	subject := graph.Node{ID: "subject", Kind: graph.KindTable, Name: "orders", QualifiedName: "orders",
		OwnerFile: "fixture.go"}
	nodes := []graph.Node{subject,
		{ID: "a", Kind: graph.KindFunction, Name: "A", QualifiedName: "fixture.A", OwnerFile: "fixture.go",
			Properties: map[string]string{"node": "a"}},
		{ID: "b", Kind: graph.KindFunction, Name: "B", QualifiedName: "fixture.B", OwnerFile: "fixture.go"},
		{ID: "c", Kind: graph.KindFunction, Name: "C", QualifiedName: "fixture.C", OwnerFile: "fixture.go"},
		{ID: "writer", Kind: graph.KindFunction, Name: "Writer", QualifiedName: "fixture.Writer", OwnerFile: "fixture.go"},
		{ID: "handler", Kind: graph.KindMethod, Name: "Handle", QualifiedName: "fixture.Handle", OwnerFile: "fixture.go"},
	}
	facts := []graph.Fact{
		{ID: "read-c", FromID: "c", Kind: graph.EdgeReads, TargetID: subject.ID, OwnerFile: "fixture.go"},
		{ID: "read-a", FromID: "a", Kind: graph.EdgeReads, TargetID: subject.ID, OwnerFile: "fixture.go",
			Location: graph.Location{Path: "a.go", Line: 7}, Properties: map[string]string{"proof": "a"}},
		{ID: "read-b", FromID: "b", Kind: graph.EdgeReads, TargetID: subject.ID, OwnerFile: "fixture.go"},
		{ID: "write", FromID: "writer", Kind: graph.EdgeWrites, TargetID: subject.ID, OwnerFile: "fixture.go"},
		{ID: "irrelevant", FromID: "writer", Kind: graph.EdgeCalls, TargetID: subject.ID, OwnerFile: "fixture.go"},
		{ID: "handled", FromID: subject.ID, Kind: graph.EdgeHandledBy, TargetID: "handler", OwnerFile: "fixture.go"},
	}
	if err := repository.ReplaceOwner(ctx, "fixture.go", graph.ParseResult{Nodes: nodes, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	incoming, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: subject.ID,
		Direction: graph.IncomingRelations,
		Relations: []graph.EdgeKind{graph.EdgeReads, graph.EdgeReads, graph.EdgeWrites}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !incoming.Truncated || len(incoming.Items) != 3 {
		t.Fatalf("incoming = %#v", incoming)
	}
	got := []string{incoming.Items[0].Counterpart.ID, incoming.Items[1].Counterpart.ID,
		incoming.Items[2].Counterpart.ID}
	if !reflect.DeepEqual(got, []string{"a", "b", "writer"}) {
		t.Fatalf("counterparts = %v", got)
	}
	first := incoming.Items[0]
	if first.Edge.FactID != "read-a" || first.Edge.Location.Path != "a.go" ||
		first.Edge.Properties["proof"] != "a" || first.Counterpart.Properties["node"] != "a" {
		t.Fatalf("hydration lost evidence: %#v", first)
	}

	outgoing, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: subject.ID,
		Direction: graph.OutgoingRelations, Relations: []graph.EdgeKind{graph.EdgeHandledBy}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if outgoing.Truncated || len(outgoing.Items) != 1 || outgoing.Items[0].Counterpart.ID != "handler" {
		t.Fatalf("outgoing = %#v", outgoing)
	}
	repeated, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: subject.ID,
		Direction: graph.IncomingRelations,
		Relations: []graph.EdgeKind{graph.EdgeReads, graph.EdgeWrites}, Limit: 2})
	if err != nil || !reflect.DeepEqual(incoming, repeated) {
		t.Fatalf("replay = %#v, %v", repeated, err)
	}
}

func TestRelationEdgesFailsClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	for _, request := range []graph.RelationEdgeQuery{
		{},
		{SubjectID: "subject", Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}},
		{SubjectID: "subject", Direction: "sideways", Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1},
	} {
		if _, err := repository.RelationEdges(ctx, request); err == nil {
			t.Fatalf("invalid request succeeded: %#v", request)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.RelationEdges(cancelled, graph.RelationEdgeQuery{SubjectID: "subject",
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1}); err == nil {
		t.Fatal("cancelled request succeeded")
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err = database.ExecContext(ctx, `INSERT INTO edges
        (id, fact_id, from_id, to_id, kind, path, line, column_no, end_line, properties)
        VALUES ('missing-edge', 'missing-fact', 'missing-node', 'subject', 'reads', '', 0, 0, 0, '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: "subject",
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1}); err == nil {
		t.Fatal("missing counterpart was silently omitted")
	}
	if _, err = database.ExecContext(ctx, `DELETE FROM edges WHERE id = 'missing-edge'`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO nodes
        (id, kind, name, qualified_name, language, path, line, column_no, end_line, properties, owner_file, external)
        VALUES ('bad-node', 'function', 'Bad', 'fixture.Bad', 'go', '', 0, 0, 0, '{', 'bad.go', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO edges
        (id, fact_id, from_id, to_id, kind, path, line, column_no, end_line, properties)
        VALUES ('bad-edge', 'bad-fact', 'bad-node', 'subject', 'reads', '', 0, 0, 0, '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: "subject",
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1}); err == nil {
		t.Fatal("malformed counterpart properties were silently accepted")
	}
}

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

func TestListNodesByKindFiltersSegmentPathsBeforeLimit(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, "root", "/tmp/example/checkout"); err != nil {
		t.Fatal(err)
	}
	for index, candidate := range []struct{ name, path string }{
		{name: "a", path: "internal/application/a.sql"},
		{name: "b", path: "other/b.sql"},
		{name: "c", path: "internal/app/c.sql"},
		{name: "d", path: "internal/app/nested/d.sql"},
	} {
		node := graph.Node{ID: graph.NodeID(graph.KindTable, candidate.name), Kind: graph.KindTable,
			Name: candidate.name, QualifiedName: candidate.name, OwnerFile: candidate.path,
			Location: graph.Location{Path: candidate.path, Line: index + 1}}
		if err := repository.ReplaceOwner(ctx, candidate.path, graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindTable},
		PathPrefixes: []string{"internal/app"}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Node.Name != "c" {
		t.Fatalf("path-filtered bounded nodes = %#v", listed)
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
