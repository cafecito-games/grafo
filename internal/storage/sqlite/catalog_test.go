package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestRelationEdgesBoundsEachRelationAndHydratesCounterparts(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
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
	if _, err = database.ExecContext(ctx,
		`INSERT OR IGNORE INTO paths(path) VALUES ('synthetic.go')`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO facts
        (id, from_id, kind, target_id, path_id, line, column_no, end_line, properties, owner_path_id)
        VALUES (grafo_identity_blob('synthetic-fact'), grafo_identity_blob('missing-node'), 'reads',
                grafo_identity_blob('subject'),
                (SELECT id FROM paths WHERE path = 'synthetic.go'), 1, 1, 1, '{}',
                (SELECT id FROM paths WHERE path = 'synthetic.go'))`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO edges
        (fact_id, from_id, to_id, kind, properties)
        VALUES (grafo_identity_blob('synthetic-fact'), grafo_identity_blob('missing-node'),
                grafo_identity_blob('subject'), 'reads', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: "subject",
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1}); err == nil {
		t.Fatal("missing counterpart was silently omitted")
	}
	if _, err = database.ExecContext(ctx,
		`DELETE FROM edges WHERE fact_id = grafo_identity_blob('synthetic-fact')
             AND to_id = grafo_identity_blob('subject') AND kind = 'reads'`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO nodes
        (id, kind, name, qualified_name, language, path, line, column_no, end_line, properties, owner_file, external)
        VALUES (grafo_identity_blob('bad-node'), 'function', 'Bad', 'fixture.Bad', 'go', '', 0, 0, 0, '{', 'bad.go', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO edges
        (fact_id, from_id, to_id, kind, properties)
        VALUES (grafo_identity_blob('synthetic-fact'), grafo_identity_blob('bad-node'),
                grafo_identity_blob('subject'), 'reads', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: "subject",
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1}); err == nil {
		t.Fatal("malformed counterpart properties were silently accepted")
	}
	if _, err = database.ExecContext(ctx,
		`DELETE FROM edges WHERE fact_id = grafo_identity_blob('synthetic-fact')
             AND to_id = grafo_identity_blob('subject') AND kind = 'reads'`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ExecContext(ctx, `DELETE FROM nodes WHERE id = grafo_identity_blob('bad-node')`); err != nil {
		t.Fatal(err)
	}
	// An edge whose originating fact is gone can no longer supply a location.
	// Reporting an empty one would hand the caller evidence pointing nowhere.
	if _, err = database.ExecContext(ctx, `INSERT INTO edges
        (fact_id, from_id, to_id, kind, properties)
        VALUES (grafo_identity_blob('deleted-fact'), grafo_identity_blob('writer'),
                grafo_identity_blob('subject'), 'reads', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: "subject",
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1}); err == nil {
		t.Fatal("edge without an originating fact was silently served")
	}
	if _, err := repository.EdgesTo(ctx, "subject"); err == nil {
		t.Fatal("adjacency read served an edge without an originating fact")
	}
}

func TestListNodesByKindEnumeratesExactKinds(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, "root", "/tmp/example/checkout"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
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
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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

func TestCanonicalMessagesFiltersBeforeOrderingAndBounds(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "root", "/tmp/example/checkout"); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		t.Fatal(err)
	}
	nodes := make([]graph.Node, 0, 1005)
	for index := 0; index <= 1000; index++ {
		nodes = append(nodes, graph.Node{ID: fmt.Sprintf("ordinary-%04d", index), Kind: graph.KindType,
			Name: fmt.Sprintf("Ordinary%04d", index), QualifiedName: fmt.Sprintf("aaa.Ordinary%04d", index),
			OwnerFile: "fixture.proto", Location: graph.Location{Path: "bulk/fixture.proto"},
			Properties: map[string]string{"declaration": "struct"}})
	}
	for _, qualified := range []string{"acme.v1.Alpha", "acme.v1.Bravo", "other.v1.Alpha"} {
		name := qualified[strings.LastIndex(qualified, ".")+1:]
		messagePath := "protocol/" + strings.ReplaceAll(qualified, ".", "/") + ".proto"
		nodes = append(nodes, graph.Node{ID: "message-" + qualified, Kind: graph.KindType, Name: name,
			QualifiedName: qualified, OwnerFile: "fixture.proto", Location: graph.Location{Path: messagePath},
			Properties: map[string]string{"declaration": "message"}})
	}
	if err := repository.ReplaceOwner(ctx, "fixture.proto", graph.ParseResult{Nodes: nodes}); err != nil {
		t.Fatal(err)
	}

	page, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{Package: "acme.v1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Truncated || len(page.Items) != 2 || page.Items[0].Node.QualifiedName != "acme.v1.Alpha" ||
		page.Items[1].Node.QualifiedName != "acme.v1.Bravo" {
		t.Fatalf("filtered canonical page = %#v", page)
	}
	bounded, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated || len(bounded.Items) != 2 {
		t.Fatalf("bounded canonical page = %#v", bounded)
	}
	exact, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{Message: "other.v1.Alpha", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Items) != 1 || exact.Items[0].Node.QualifiedName != "other.v1.Alpha" {
		t.Fatalf("qualified message filter = %#v", exact)
	}
	pathFiltered, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{
		PathPrefixes: []string{"protocol/acme/v1/Bravo.proto"}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pathFiltered.Truncated || len(pathFiltered.Items) != 1 || pathFiltered.Items[0].Node.QualifiedName != "acme.v1.Bravo" {
		t.Fatalf("path-filtered canonical page = %#v", pathFiltered)
	}
	foreign, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{Repository: "billing", Limit: 10})
	if err != nil || foreign.Truncated || len(foreign.Items) != 0 {
		t.Fatalf("foreign repository filter = %#v, %v", foreign, err)
	}
	if _, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{}); err == nil {
		t.Fatal("zero canonical message bound succeeded")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.CanonicalMessages(cancelled, graph.CanonicalMessageQuery{Limit: 10}); err == nil {
		t.Fatal("cancelled canonical message query succeeded")
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := sqlite.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	repeated, err := reader.CanonicalMessages(ctx, graph.CanonicalMessageQuery{Package: "acme.v1", Limit: 10})
	if err != nil || !reflect.DeepEqual(page, repeated) {
		t.Fatalf("read-only canonical page = %#v, %v", repeated, err)
	}
}

func TestNameMatchingUsesUnicodeLowercase(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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

// TestRelationEdgeQueriesUseTheEdgeEndpointIndexes keeps the guarantee that the
// queries used to state with an INDEXED BY clause. The clause had to go so that a
// cold load can defer those two indexes, which are the widest in the schema, but
// the plan it was protecting still has to hold: pinning it here fails if a future
// schema or planner change starts scanning the edge table instead.
func TestRelationEdgeQueriesUseTheEdgeEndpointIndexes(t *testing.T) {
	repository, err := sqlite.Open(context.Background(), filepath.Join(testtemp.Dir(t), "plan.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	for _, testCase := range []struct {
		name   string
		column string
		// order is the leading ORDER BY column the matching production query uses:
		// the endpoint the filter does not fix.
		order string
		index string
	}{
		{name: "incoming", column: "to_id", order: "from_id", index: "edges_to"},
		{name: "outgoing", column: "from_id", order: "to_id", index: "edges_from"},
	} {
		plan := queryPlan(t, repository, fmt.Sprintf(`SELECT edges.fact_id, COALESCE(nodes.id, ''), COALESCE(facts.producer, ''),
    COALESCE(origin_paths.path, '')
FROM edges
LEFT JOIN nodes ON nodes.id = edges.from_id
LEFT JOIN facts ON facts.id = edges.fact_id
LEFT JOIN paths AS origin_paths ON origin_paths.id = facts.path_id
WHERE edges.%s = 'subject' AND edges.kind = 'calls'
ORDER BY edges.%s, edges.fact_id
LIMIT 10`, testCase.column, testCase.order))
		if !strings.Contains(plan, "USING INDEX "+testCase.index) {
			t.Errorf("%s relation edge plan does not use %s:\n%s", testCase.name, testCase.index, plan)
		}
		if strings.Contains(plan, "SCAN edges") {
			t.Errorf("%s relation edge plan scans the edge table:\n%s", testCase.name, plan)
		}
	}
}

func queryPlan(t *testing.T, repository *sqlite.Repository, statement string) string {
	t.Helper()
	database, err := sql.Open("sqlite", repository.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	rows, err := database.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+statement)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var selectID, order, from int
		var detail string
		if err := rows.Scan(&selectID, &order, &from, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}
