package typescript_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
)

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`import express from "express";
interface Handler { run(): void }
class Checkout extends Base implements Handler {
  async run() {
    const token = process.env.API_TOKEN;
    charge(token);
    events.publish("order.created", {});
  }
}
const app = express();
app.post("/checkout", checkout);
`)
	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "src/app.ts", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindClass, "Checkout")
	assertHasNode(t, result.Nodes, graph.KindMethod, "run")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "POST /checkout")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "order.created")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "charge")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "Base")
	assertHasFact(t, result.Facts, graph.EdgeImplements, "Handler")
}

func assertHasNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, name string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.Name == name {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, name, nodes)
}

func assertHasFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
}
