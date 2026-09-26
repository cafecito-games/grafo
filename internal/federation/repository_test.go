package federation_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	manifestparser "github.com/cafecito-games/grafo/internal/parser/manifest"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestRepositoryResolvesHTTPAcrossIndexes(t *testing.T) {
	ctx := context.Background()
	clientRoot := t.TempDir()
	serverRoot := t.TempDir()
	write(t, filepath.Join(clientRoot, "go.mod"), "module example.com/client\n\ngo 1.26\n\nrequire example.com/server v0.0.0\n")
	write(t, filepath.Join(clientRoot, "client.go"), `package client
import "net/http"
func Call() { http.Get("/charge") }
`)
	write(t, filepath.Join(serverRoot, "go.mod"), "module example.com/server\n\ngo 1.26\n")
	write(t, filepath.Join(serverRoot, "server.go"), `package server
func Handler() {}
func Routes() { router.Get("/charge", Handler) }
`)
	index(t, ctx, clientRoot)
	index(t, ctx, serverRoot)

	repository, err := federation.Open(ctx, []string{clientRoot, serverRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	result, err := query.NewService(repository).Neighborhood(ctx, "example.com/client.Call", 1,
		query.Outgoing, []graph.EdgeKind{graph.EdgeRequests}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 || result.Nodes[1].Node.Kind != graph.KindEndpoint || result.Nodes[1].Node.Name != "GET /charge" {
		t.Fatalf("cross-repository endpoint was not resolved: %#v", result.Nodes)
	}
	if len(result.Edges) != 1 || result.Edges[0].Properties["federated"] != "true" {
		t.Fatalf("expected a federated request edge: %#v", result.Edges)
	}
	incoming, err := repository.EdgesTo(ctx, result.Nodes[1].Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundIncoming := false
	for _, edge := range incoming {
		if edge.Kind == graph.EdgeRequests && edge.FromID == result.Root.ID && edge.Properties["federated"] == "true" {
			foundIncoming = true
		}
	}
	if !foundIncoming {
		t.Fatalf("incoming federated edge missing: %#v", incoming)
	}
	dependency, err := query.NewService(repository).Neighborhood(ctx, "example.com/client", 1,
		query.Outgoing, []graph.EdgeKind{graph.EdgeDependsOn}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependency.Nodes) != 2 || dependency.Nodes[1].Node.Kind != graph.KindModule || dependency.Nodes[1].Node.QualifiedName != "example.com/server" {
		t.Fatalf("cross-repository module dependency was not resolved: %#v", dependency.Nodes)
	}
	if len(dependency.Edges) != 1 || dependency.Edges[0].Properties["federated"] != "true" {
		t.Fatalf("expected a federated dependency edge: %#v", dependency.Edges)
	}

	semanticService := semantic.NewService(repository, repository, handlerEmbedder{})
	report, err := semanticService.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Candidates < 3 || report.Updated != report.Candidates {
		t.Fatalf("federated embeddings were not distributed to member indexes: %#v", report)
	}
	matches, err := semanticService.Search(ctx, "find a request handler", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches.Matches) != 1 || matches.Matches[0].Node.Name != "Handler" {
		t.Fatalf("unexpected federated semantic match: %#v", matches.Matches)
	}
}

type handlerEmbedder struct{}

func (handlerEmbedder) Model() string { return "federation-test" }

func (handlerEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	result := make([][]float32, 0, len(inputs))
	for _, input := range inputs {
		if strings.Contains(strings.ToLower(input), "handler") {
			result = append(result, []float32{1, 0})
		} else {
			result = append(result, []float32{0, 1})
		}
	}
	return result, nil
}

func index(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New(), manifestparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
