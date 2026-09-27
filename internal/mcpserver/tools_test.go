package mcpserver_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	"github.com/cafecito-games/grafo/internal/search"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// batchFixture builds a two-node graph where handler calls charge, plus a
// worktree holding the matching source so search and excerpts have real files.
func batchFixture(t *testing.T) (*sqlite.Repository, indexer.Project) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "checkout.go"),
		[]byte("package sample\n\nfunc Handler() {\n\tCharge()\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "charge.go"),
		[]byte("package sample\n\nfunc Charge() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })

	handler := graph.Node{ID: graph.NodeID(graph.KindFunction, "sample.Handler"), Kind: graph.KindFunction,
		Name: "Handler", QualifiedName: "sample.Handler", OwnerFile: "checkout.go",
		Location: graph.Location{Path: "checkout.go", Line: 3, EndLine: 5}}
	charge := graph.Node{ID: graph.NodeID(graph.KindFunction, "sample.Charge"), Kind: graph.KindFunction,
		Name: "Charge", QualifiedName: "sample.Charge", OwnerFile: "charge.go",
		Location: graph.Location{Path: "charge.go", Line: 3, EndLine: 3}}
	// ReplaceFile records the file so search can find it and stores its parse
	// result in one step; a later empty ReplaceOwner would erase the nodes.
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "checkout.go", Language: "go"}, graph.ParseResult{
		Nodes: []graph.Node{handler},
		Facts: []graph.Fact{{
			ID:     graph.FactID("checkout.go", handler.ID, graph.EdgeCalls, "sample.Charge", 4, 1),
			FromID: handler.ID, Kind: graph.EdgeCalls, Target: "sample.Charge", OwnerFile: "checkout.go",
			Location: graph.Location{Path: "checkout.go", Line: 4},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "charge.go", Language: "go"},
		graph.ParseResult{Nodes: []graph.Node{charge}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return repository, indexer.Project{Root: root, Name: "sample", Branch: "main"}
}

func connect(t *testing.T, service *mcpserver.Service) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := service.Server("test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "grafo-test", Version: "test"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientSession.Close() })
	return clientSession
}

func call(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s returned an error: %#v", name, result.Content)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s produced no structured result: %#v", name, result.StructuredContent)
	}
	return structured
}

func callExpectingError(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return
	}
	if !result.IsError {
		t.Fatalf("%s accepted invalid input: %#v", name, result.StructuredContent)
	}
}

func TestBatchedToolsReturnOneEnvelopePerInput(t *testing.T) {
	repository, project := batchFixture(t)
	session := connect(t, mcpserver.New(repository, project))

	structured := call(t, session, "get_node", map[string]any{
		"selectors": []any{"sample.Handler", "sample.Missing", "sample.Charge"},
	})
	results, ok := structured["results"].([]any)
	if !ok || len(results) != 3 {
		t.Fatalf("expected three envelopes, got %#v", structured["results"])
	}
	if _, present := structured["node"]; present {
		t.Fatalf("a batched request must not fill the scalar field: %#v", structured)
	}
	first, _ := results[0].(map[string]any)
	if first["error"] != nil {
		t.Fatalf("first input should have resolved: %#v", first)
	}
	second, _ := results[1].(map[string]any)
	if second["error"] == nil {
		t.Fatalf("the unknown selector should report its own error: %#v", second)
	}
	third, _ := results[2].(map[string]any)
	if third["error"] != nil || third["value"] == nil {
		t.Fatalf("a failed input must not erase a later success: %#v", third)
	}
	if index, _ := third["index"].(float64); int(index) != 2 {
		t.Fatalf("envelopes must keep caller order: %#v", third)
	}

	// A scalar request keeps the original top-level shape.
	structured = call(t, session, "get_node", map[string]any{"selector": "sample.Handler"})
	node, ok := structured["node"].(map[string]any)
	if !ok || node["qualified_name"] != "sample.Handler" {
		t.Fatalf("scalar get_node lost its legacy shape: %#v", structured)
	}

	// A scalar request that cannot resolve still fails the call outright.
	callExpectingError(t, session, "get_node", map[string]any{"selector": "sample.Missing"})
	callExpectingError(t, session, "get_node", map[string]any{})
	callExpectingError(t, session, "get_node", map[string]any{
		"selector": "sample.Handler", "selectors": []any{"sample.Charge"},
	})

	paths := call(t, session, "find_path", map[string]any{
		"pairs": []any{
			map[string]any{"from": "sample.Handler", "to": "sample.Charge"},
			map[string]any{"from": "sample.Charge", "to": "sample.Handler"},
		},
	})
	results, ok = paths["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("expected one envelope per pair, got %#v", paths["results"])
	}
	found, _ := results[0].(map[string]any)
	if found["error"] != nil {
		t.Fatalf("Handler should reach Charge: %#v", found)
	}
	unreachable, _ := results[1].(map[string]any)
	if unreachable["error"] == nil {
		t.Fatalf("Charge should not reach Handler outgoing: %#v", unreachable)
	}
}

func TestBlastRadiusReturnsBidirectionalImpact(t *testing.T) {
	repository, project := batchFixture(t)
	sourceService := sourcecontext.NewService(repository, sourcecontext.NewSingleProjectLocator(repository, project))
	session := connect(t, mcpserver.New(repository, project).WithSource(sourceService.Read))

	// Charge is called by Handler, so Charge has upstream impact and none down.
	structured := call(t, session, "get_blast_radius", map[string]any{
		"selector": "sample.Charge", "include_source": true,
	})
	upstream, ok := structured["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("missing upstream section: %#v", structured)
	}
	if upstream["direction"] != "upstream" {
		t.Fatalf("unexpected upstream section: %#v", upstream)
	}
	upstreamNodes, _ := upstream["nodes"].([]any)
	if len(upstreamNodes) != 2 {
		t.Fatalf("expected Charge and its caller upstream, got %#v", upstreamNodes)
	}
	downstream, ok := structured["downstream"].(map[string]any)
	if !ok || downstream["direction"] != "downstream" {
		t.Fatalf("missing downstream section: %#v", structured)
	}
	downstreamNodes, _ := downstream["nodes"].([]any)
	if len(downstreamNodes) != 1 {
		t.Fatalf("Charge calls nothing, so only the root should appear: %#v", downstreamNodes)
	}
	files, _ := structured["impacted_files"].([]any)
	if len(files) != 2 {
		t.Fatalf("expected both files impacted, got %#v", files)
	}
	sources, _ := structured["sources"].([]any)
	if len(sources) == 0 {
		t.Fatalf("expected bounded source excerpts: %#v", structured)
	}

	// The legacy selector/depth/limit inputs still work.
	structured = call(t, session, "get_blast_radius", map[string]any{
		"selector": "sample.Charge", "depth": 1, "limit": 5,
	})
	upstream, _ = structured["upstream"].(map[string]any)
	if depth, _ := upstream["depth"].(float64); int(depth) != 1 {
		t.Fatalf("legacy depth was not applied to both directions: %#v", upstream)
	}
	if downstream, _ := structured["downstream"].(map[string]any); downstream == nil {
		t.Fatalf("missing downstream section: %#v", structured)
	} else if depth, _ := downstream["depth"].(float64); int(depth) != 1 {
		t.Fatalf("legacy depth was not applied to both directions: %#v", downstream)
	}
}

func TestSearchSourceIsRegisteredOnlyWhenConfigured(t *testing.T) {
	repository, project := batchFixture(t)

	withoutSearch := connect(t, mcpserver.New(repository, project))
	listed, err := withoutSearch.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "search_source" {
			t.Fatal("search_source must not be registered without a search service")
		}
	}

	searchService := search.NewService([]search.Source{{Project: project, Catalog: repository}})
	session := connect(t, mcpserver.New(repository, project).WithSearch(searchService))
	structured := call(t, session, "search_source", map[string]any{"pattern": "Charge()"})
	matches, ok := structured["matches"].([]any)
	if !ok || len(matches) != 2 {
		t.Fatalf("expected the declaration and the call site, got %#v", structured)
	}
	// Ordering is by path then line, never by filesystem enumeration.
	declaration, _ := matches[0].(map[string]any)
	if declaration["path"] != "charge.go" {
		t.Fatalf("matches are not ordered by path: %#v", matches)
	}
	callSite, _ := matches[1].(map[string]any)
	if callSite["path"] != "checkout.go" {
		t.Fatalf("matches are not ordered by path: %#v", matches)
	}
	if line, _ := callSite["line"].(float64); int(line) != 4 {
		t.Fatalf("unexpected call-site line: %#v", callSite)
	}

	// Multiple patterns search in one pass.
	structured = call(t, session, "search_source", map[string]any{
		"patterns": []any{"func Handler", "func Charge"},
	})
	matches, _ = structured["matches"].([]any)
	if len(matches) != 2 {
		t.Fatalf("expected one match per pattern, got %#v", structured)
	}

	// An invalid regex names the pattern instead of searching.
	callExpectingError(t, session, "search_source", map[string]any{"pattern": "a(", "regex": true})
	callExpectingError(t, session, "search_source", map[string]any{})
}
