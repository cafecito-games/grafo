package mcpserver_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	"github.com/cafecito-games/grafo/internal/query"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerExposesCatalogTools(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, "root", "/tmp/example/shop"); err != nil {
		t.Fatal(err)
	}
	orders := graph.Node{ID: graph.NodeID(graph.KindTable, "orders"), Kind: graph.KindTable,
		Name: "orders", QualifiedName: "orders", OwnerFile: "schema.sql",
		Location:   graph.Location{Path: "schema.sql", Line: 1},
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
	defer func() { _ = serverSession.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "grafo-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()

	listed, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_data_resources", Arguments: map[string]any{"repository": "shop", "path_prefixes": []string{"schema.sql"}, "limit": 10},
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

	blank, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_data_resources", Arguments: map[string]any{"kinds": []string{" "}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !blank.IsError {
		t.Fatalf("a kind list naming no kind must be rejected, not answered: %#v", blank.StructuredContent)
	}
	invalidPath, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_events", Arguments: map[string]any{"path_prefixes": []string{"../outside"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !invalidPath.IsError {
		t.Fatalf("an unsafe path prefix must be rejected: %#v", invalidPath.StructuredContent)
	}

	ambiguous := graph.Node{ID: graph.NodeID(graph.KindTable, "archive.stock"), Kind: graph.KindTable,
		Name: "stock", QualifiedName: "archive.stock", OwnerFile: "archive.sql"}
	other := graph.Node{ID: graph.NodeID(graph.KindTable, "live.stock"), Kind: graph.KindTable,
		Name: "stock", QualifiedName: "live.stock", OwnerFile: "archive.sql"}
	if err := repository.ReplaceOwner(ctx, "archive.sql", graph.ParseResult{
		Nodes: []graph.Node{ambiguous, other},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	conflicted, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_data_resource_usage", Arguments: map[string]any{"selector": "stock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !conflicted.IsError {
		t.Fatalf("an ambiguous selector must not resolve: %#v", conflicted.StructuredContent)
	}
	message := contentText(conflicted)
	for _, want := range []string{"archive.stock", "live.stock"} {
		if !strings.Contains(message, want) {
			t.Fatalf("ambiguity error does not name candidate %q: %s", want, message)
		}
	}
}

func contentText(result *mcp.CallToolResult) string {
	var builder strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	return builder.String()
}

func TestSourceToolNamesAmbiguousCandidates(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	candidates := []graph.Node{
		{ID: "n:archive-stock", Kind: graph.KindTable, Name: "stock", QualifiedName: "archive.stock"},
		{ID: "n:live-stock", Kind: graph.KindTable, Name: "stock", QualifiedName: "live.stock"},
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := mcpserver.New(repository, indexer.Project{Name: "shop", Branch: "main"}).
		WithSource(func(context.Context, string, graph.NodeKind, int, int) (sourcecontext.Excerpt, error) {
			return sourcecontext.Excerpt{}, &query.AmbiguousError{Term: "stock", Candidates: candidates}
		}).
		Server("test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "grafo-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_source", Arguments: map[string]any{"selector": "stock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("an ambiguous selector must not resolve: %#v", result.StructuredContent)
	}
	message := contentText(result)
	for _, want := range []string{"archive.stock", "live.stock"} {
		if !strings.Contains(message, want) {
			t.Fatalf("source ambiguity error does not name candidate %q: %s", want, message)
		}
	}
}

func TestBatchedAmbiguityNamesCandidates(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	archive := graph.Node{ID: graph.NodeID(graph.KindTable, "archive.stock"), Kind: graph.KindTable,
		Name: "stock", QualifiedName: "archive.stock", OwnerFile: "schema.sql"}
	live := graph.Node{ID: graph.NodeID(graph.KindTable, "live.stock"), Kind: graph.KindTable,
		Name: "stock", QualifiedName: "live.stock", OwnerFile: "schema.sql"}
	if err := repository.ReplaceOwner(ctx, "schema.sql", graph.ParseResult{
		Nodes: []graph.Node{archive, live},
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
	defer func() { _ = serverSession.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "grafo-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()

	// A batched call reports per-input errors in an envelope, which keeps only
	// the error's text. The candidates must survive that flattening.
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_node", Arguments: map[string]any{"selectors": []string{"stock"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("missing structured result: %#v", result.StructuredContent)
	}
	results, ok := structured["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("unexpected batched results: %#v", structured)
	}
	envelope, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected envelope: %#v", results[0])
	}
	message, ok := envelope["error"].(string)
	if !ok || message == "" {
		t.Fatalf("batched ambiguity did not report an error: %#v", envelope)
	}
	for _, want := range []string{"archive.stock", "live.stock"} {
		if !strings.Contains(message, want) {
			t.Fatalf("batched ambiguity error does not name candidate %q: %s", want, message)
		}
	}
}
