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
class ChargeService { charge(value: string) {} }
class Checkout extends Base implements Handler {
  async run(orderId: string) {
    const request = orderId;
    const token = process.env.API_TOKEN;
    const service = new ChargeService();
    service.charge(request);
    axios.post("/checkout", request);
    events.publish("order.created", {});
    return request;
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
	assertHasNode(t, result.Nodes, graph.KindParameter, "orderId")
	assertHasNode(t, result.Nodes, graph.KindVariable, "request")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "POST /checkout")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "order.created")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "src/app.ChargeService.charge")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "POST /checkout")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "Base")
	assertHasFact(t, result.Facts, graph.EdgeImplements, "Handler")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePasses)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
}

func assertHasFactKind(t *testing.T, facts []graph.Fact, kind graph.EdgeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s fact; got %#v", kind, facts)
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
