package mcpserver_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
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

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := mcpserver.New(repository, indexer.Project{Name: "sample", Branch: "main"}).Server("test").Connect(ctx, serverTransport, nil)
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
	if len(listed.Tools) != 8 {
		t.Fatalf("expected 8 tools, got %d", len(listed.Tools))
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
}
