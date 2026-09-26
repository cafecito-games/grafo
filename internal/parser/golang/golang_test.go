package golang_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
)

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`package api
import (
  clientpkg "example.com/client"
  "net/http"
  "os"
)
type Server struct{}
func (s *Server) Start(input string) string {
  copy := input
  client := clientpkg.NewClient()
  client.Send(copy)
  s.deliver(copy)
  token := os.Getenv("API_TOKEN")
  _ = token
  http.HandleFunc("/health", health)
  http.Get("/ready")
  publish("user.created")
  return copy
}
func (s *Server) deliver(string) {}
func health(http.ResponseWriter, *http.Request) {}
`)
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Path: "api/server.go", Content: content, Repository: "sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindMethod, "Start")
	assertHasNode(t, result.Nodes, graph.KindParameter, "input")
	assertHasNode(t, result.Nodes, graph.KindVariable, "copy")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "ANY /health")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "user.created")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "GET /ready")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
	assertHasFact(t, result.Facts, graph.EdgePasses, "example.com/sample/api.Server.deliver")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/sample/api.Server.deliver")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/client.Client.Send")
}

func assertHasFactKind(t *testing.T, facts []graph.Fact, kind graph.EdgeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s fact", kind)
}

func assertHasNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, name string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.Name == name {
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, name)
}

func assertHasFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact to %q", kind, target)
}
