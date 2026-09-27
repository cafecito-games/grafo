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

func TestServerExposesCatalogTools(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.SetMeta(ctx, "root", "/tmp/example/shop"); err != nil {
		t.Fatal(err)
	}
	orders := graph.Node{ID: graph.NodeID(graph.KindTable, "orders"), Kind: graph.KindTable,
		Name: "orders", QualifiedName: "orders", OwnerFile: "schema.sql",
		Properties: map[string]string{"object_kind": "table", "dialect": "sqlite"}}
	writer := graph.Node{ID: graph.NodeID(graph.KindFunction, "shop.CreateOrder"), Kind: graph.KindFunction,
		Name: "CreateOrder", QualifiedName: "shop.CreateOrder", OwnerFile: "shop.go"}
	if err := repository.ReplaceOwner(ctx, "schema.sql", graph.ParseResult{Nodes: []graph.Node{orders}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "shop.go", graph.ParseResult{
		Nodes: []graph.Node{writer},
		Facts: []graph.Fact{{
			ID:     graph.FactID("shop.go", writer.ID, graph.EdgeWrites, "orders", 9, 1),
			FromID: writer.ID, Kind: graph.EdgeWrites, Target: "orders", TargetKind: graph.KindTable,
			Location: graph.Location{Path: "shop.go", Line: 9}, OwnerFile: "shop.go",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := mcpserver.New(repository, indexer.Project{Name: "shop", Branch: "main"}).
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

	listed, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_data_resources", Arguments: map[string]any{"repository": "shop", "limit": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if listed.IsError {
		t.Fatalf("list_data_resources returned an error: %#v", listed.Content)
	}
	structured, ok := listed.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("missing structured result: %#v", listed.StructuredContent)
	}
	resources, ok := structured["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("unexpected resources: %#v", structured)
	}

	usage, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_data_resource_usage", Arguments: map[string]any{"selector": "orders"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage.IsError {
		t.Fatalf("get_data_resource_usage returned an error: %#v", usage.Content)
	}
	structured, ok = usage.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("missing structured usage: %#v", usage.StructuredContent)
	}
	writers, ok := structured["writers"].([]any)
	if !ok || len(writers) != 1 {
		t.Fatalf("expected one writer, got %#v", structured)
	}
	if readers, ok := structured["readers"].([]any); !ok || len(readers) != 0 {
		t.Fatalf("expected no readers, got %#v", structured["readers"])
	}

	for _, name := range []string{"list_config_keys", "list_events", "find_orphaned_events"} {
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.IsError {
			t.Fatalf("%s returned an error: %#v", name, result.Content)
		}
	}

	rejected, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_data_resources", Arguments: map[string]any{"kinds": []string{"function"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rejected.IsError {
		t.Fatalf("an unsupported kind must be rejected, not answered: %#v", rejected.StructuredContent)
	}
}
