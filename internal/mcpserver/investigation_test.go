package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/investigation"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInvestigationMCPMatchesSharedServiceWithoutDuplicatedEvidence(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "checkout.go"), []byte("package shop\nfunc Checkout() int { return 7 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	node := graph.Node{ID: graph.NodeID(graph.KindFunction, "shop.Checkout"), Kind: graph.KindFunction, Name: "Checkout", QualifiedName: "shop.Checkout", OwnerFile: "checkout.go", Language: "go", Location: graph.Location{Path: "checkout.go", Line: 2, EndLine: 2}}
	if err := repo.ReplaceOwner(ctx, "checkout.go", graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
		t.Fatal(err)
	}
	project := indexer.Project{Name: "shop", Root: root, Branch: "main"}
	sources := sourcecontext.NewScopedService(repo, []indexer.Project{project})
	service := mcpserver.New(repo, project).WithSource(sources.ReadKind).WithInvestigationSource(sourcecontext.NewScopedService)
	session := investigationClient(t, service)
	input := investigation.Request{Query: "Checkout", MaxOutputBytes: 8000}
	expected, err := investigation.New(repo, sources).Investigate(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "investigate", Arguments: input})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool error: %#v", result.Content)
	}
	if result.StructuredContent != nil {
		t.Fatalf("duplicated report into structured content")
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected one compact report: %#v", result.Content)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content: %#v", result.Content)
	}
	if content.Text != string(want) {
		t.Fatalf("MCP differs from shared service\ngot %s\nwant %s", content.Text, want)
	}
	if len(content.Text) > input.MaxOutputBytes {
		t.Fatalf("report exceeded byte budget: %d", len(content.Text))
	}
	invalid, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "investigate", Arguments: map[string]any{"query": "Checkout", "max_work": -1}})
	if err == nil && !invalid.IsError {
		t.Fatal("negative budget was accepted")
	}
}

func TestInvestigationRequiresSourceAccess(t *testing.T) {
	repo, err := sqlite.Open(context.Background(), filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	session := investigationClient(t, mcpserver.New(repo, indexer.Project{Name: "shop"}).WithSource(func(context.Context, string, graph.NodeKind, int, int) (sourcecontext.Excerpt, error) {
		return sourcecontext.Excerpt{}, fmt.Errorf("source access denied by embedder")
	}))
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name == "investigate" {
			t.Fatal("source-bearing investigation exposed without source access")
		}
	}
}

func investigationClient(t *testing.T, service *mcpserver.Service) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := service.Server("test").Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "investigate-test", Version: "test"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
