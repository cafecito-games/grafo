package mcpserver_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	"github.com/cafecito-games/grafo/internal/semantic"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerListsAndCallsGraphTools(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	node := graph.Node{ID: graph.NodeID(graph.KindFunction, "sample.Checkout"), Kind: graph.KindFunction,
		Name: "Checkout", QualifiedName: "sample.Checkout", OwnerFile: "checkout.go"}
	if err := repository.ReplaceOwner(ctx, "checkout.go", graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	var refreshes atomic.Int32
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := mcpserver.New(repository, indexer.Project{Name: "sample", Branch: "main"}).
		WithRefresh(func(context.Context) error { refreshes.Add(1); return nil }).
		WithReusable(func(_ context.Context, text string, _ int) (semantic.SearchResult, error) {
			return semantic.SearchResult{Query: text, Model: "test"}, nil
		}).
		WithSource(func(_ context.Context, _ string, _ graph.NodeKind, _, _ int) (sourcecontext.Excerpt, error) {
			return sourcecontext.Excerpt{Path: "checkout.go", StartLine: 1, EndLine: 2, Content: "func Checkout() {}"}, nil
		}).
		Server("test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "grafo-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 16 {
		t.Fatalf("expected 16 tools, got %d", len(listed.Tools))
	}
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "find_symbols", Arguments: map[string]any{"query": "Checkout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool returned an error: %#v", result.Content)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("missing structured result: %#v", result.StructuredContent)
	}
	matches, ok := structured["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("unexpected matches: %#v", structured["matches"])
	}
	if refreshes.Load() != 1 {
		t.Fatalf("expected one pre-query refresh, got %d", refreshes.Load())
	}
	reusable, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "find_reusable_code", Arguments: map[string]any{"query": "charge a card"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reusable.IsError {
		t.Fatalf("reusable tool returned an error: %#v", reusable.Content)
	}
	structured, ok = reusable.StructuredContent.(map[string]any)
	if !ok || structured["query"] != "charge a card" || structured["model"] != "test" {
		t.Fatalf("unexpected reusable result: %#v", reusable.StructuredContent)
	}
	if refreshes.Load() != 2 {
		t.Fatalf("expected refresh before every tool call, got %d", refreshes.Load())
	}
	source, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_source", Arguments: map[string]any{"selector": "sample.Checkout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.IsError {
		t.Fatalf("source tool returned an error: %#v", source.Content)
	}
	structured, ok = source.StructuredContent.(map[string]any)
	if !ok || structured["path"] != "checkout.go" || structured["content"] != "func Checkout() {}" {
		t.Fatalf("unexpected source result: %#v", source.StructuredContent)
	}
	if refreshes.Load() != 3 {
		t.Fatalf("expected refresh before every tool call, got %d", refreshes.Load())
	}
}
