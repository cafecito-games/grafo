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
  "net/http"
  "os"
)
type Server struct{}
func (s *Server) Start() {
  token := os.Getenv("API_TOKEN")
  _ = token
  http.HandleFunc("/health", health)
  publish("user.created")
}
func health(http.ResponseWriter, *http.Request) {}
`)
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Path: "api/server.go", Content: content, Repository: "sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindMethod, "Start")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "ANY /health")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "user.created")
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
