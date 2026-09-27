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
	defer func() { _ = repository.Close() }()

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
	defer func() { _ = repository.Close() }()

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
	defer func() { _ = repository.Close() }()

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
	defer func() { _ = repository.Close() }()

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

// TestMatchNodesReportsCompleteTotals pins the adapter contract that selector
// resolution depends on: the strongest level wins, the totals cover the whole
// graph even when the listed candidates are truncated, case-sensitive matches are
// listed first, and the optional kind filter is applied by the query.
func TestMatchNodesReportsCompleteTotals(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	var nodes []graph.Node
	for index := 0; index < 12; index++ {
		qualified := fmt.Sprintf("sample.pkg%02d.Service.Search", index)
		nodes = append(nodes, graph.Node{ID: graph.NodeID(graph.KindMethod, qualified),
			Kind: graph.KindMethod, Name: "Search", QualifiedName: qualified, OwnerFile: "search.go"})
	}
	// Two case-insensitive-only matches and one node that merely contains the term.
	nodes = append(nodes,
		graph.Node{ID: graph.NodeID(graph.KindField, "sample.cli.App.search"), Kind: graph.KindField,
			Name: "search", QualifiedName: "sample.cli.App.search", OwnerFile: "search.go"},
		graph.Node{ID: graph.NodeID(graph.KindParameter, "sample.cli.App.Run.search"), Kind: graph.KindParameter,
			Name: "search", QualifiedName: "sample.cli.App.Run.search", OwnerFile: "search.go"},
		graph.Node{ID: graph.NodeID(graph.KindPackage, "sample.searchengine"), Kind: graph.KindPackage,
			Name: "searchengine", QualifiedName: "sample.searchengine", OwnerFile: "search.go"})
	if err := repository.ReplaceOwner(ctx, "search.go", graph.ParseResult{Nodes: nodes}); err != nil {
		t.Fatal(err)
	}

	group, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "Search", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if group.Level != graph.MatchName {
		t.Fatalf("expected the name level to win, got %q", group.Level)
	}
	if group.Total != 14 || group.Strict != 12 {
		t.Fatalf("expected 14 matches with 12 case-sensitive, got %d and %d", group.Total, group.Strict)
	}
	if len(group.Nodes) != 5 || !group.Truncated() {
		t.Fatalf("expected a truncated list of 5, got %d", len(group.Nodes))
	}
	for _, node := range group.Nodes {
		if node.Name != "Search" {
			t.Fatalf("case-sensitive matches must be listed first, got %q", node.QualifiedName)
		}
	}

	// A fully qualified selector reaches the stronger level, kinds filter, and a
	// substring-only selector falls through to the weakest level.
	qualified, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "sample.pkg00.Service.Search"})
	if err != nil {
		t.Fatal(err)
	}
	if qualified.Level != graph.MatchQualifiedName || qualified.Total != 1 {
		t.Fatalf("unexpected qualified-name group: %#v", qualified)
	}
	filtered, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "search", Kind: graph.KindField})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 1 || len(filtered.Nodes) != 1 || filtered.Nodes[0].Kind != graph.KindField {
		t.Fatalf("unexpected kind-filtered group: %#v", filtered)
	}
	substring, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "searcheng"})
	if err != nil {
		t.Fatal(err)
	}
	if substring.Level != graph.MatchSubstring || substring.Total != 1 {
		t.Fatalf("unexpected substring group: %#v", substring)
	}
	if empty, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "absent"}); err != nil || empty.Total != 0 {
		t.Fatalf("expected an empty group, got %#v and %v", empty, err)
	}
}

// TestMatchNodesScopesExternalNodesAsFallback pins the scope half of the
// strongest-evidence rule: exact matching considers external boundary nodes
// instead of filtering them out, but a local declaration at the same level always
// wins, so exact evidence about an external target is never pushed down into the
// weaker substring level.
func TestMatchNodesScopesExternalNodesAsFallback(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	// A containing local symbol plus an exact external target. Nothing local is
	// named Client, so the external node is the strongest evidence there is.
	registry := graph.Node{ID: graph.NodeID(graph.KindType, "sample.ClientRegistry"), Kind: graph.KindType,
		Name: "ClientRegistry", QualifiedName: "sample.ClientRegistry", OwnerFile: "client.go"}
	external := graph.Node{ID: graph.NodeID(graph.KindExternal, "net/http.Client"), Kind: graph.KindExternal,
		Name: "Client", QualifiedName: "net/http.Client", OwnerFile: "__external__", External: true}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: []graph.Node{registry}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "__external__", graph.ParseResult{Nodes: []graph.Node{external}}); err != nil {
		t.Fatal(err)
	}

	group, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "net/http.Client"})
	if err != nil {
		t.Fatal(err)
	}
	if group.Level != graph.MatchQualifiedName || !group.External || group.Total != 1 {
		t.Fatalf("an exact external qualified name must stay exact evidence: %#v", group)
	}
	group, err = repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "Client"})
	if err != nil {
		t.Fatal(err)
	}
	if group.Level != graph.MatchName || !group.External || group.Total != 1 ||
		group.Nodes[0].ID != external.ID {
		t.Fatalf("an exact external name must beat a containing local symbol: %#v", group)
	}

	// Once a local declaration of the same name exists it wins outright, and the
	// group carries only local nodes.
	local := graph.Node{ID: graph.NodeID(graph.KindType, "sample.Client"), Kind: graph.KindType,
		Name: "Client", QualifiedName: "sample.Client", OwnerFile: "local.go"}
	if err := repository.ReplaceOwner(ctx, "local.go", graph.ParseResult{Nodes: []graph.Node{local}}); err != nil {
		t.Fatal(err)
	}
	group, err = repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "Client"})
	if err != nil {
		t.Fatal(err)
	}
	if group.Level != graph.MatchName || group.External || group.Total != 1 ||
		group.Nodes[0].ID != local.ID {
		t.Fatalf("a local declaration must outrank an external boundary node: %#v", group)
	}
}
