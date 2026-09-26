package python_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	pythonparser "github.com/cafecito-games/grafo/internal/parser/python"
)

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`import requests as http
from base import Base as Parent
from os import getenv

class ChargeService:
    def charge(self, value: str):
        return value

class Checkout(Parent):
    timeout: int = 30

    def __init__(self):
        self.gateway: ChargeService = ChargeService()

    @app.post("/checkout")
    async def run(self, order_id: str) -> str:
        request = order_id
        token = getenv("API_TOKEN")
        service = ChargeService()
        service.charge(request)
        http.post("https://payments.test/charge", json=request)
        events.publish("order.created", {})
        return request
`)
	result, err := pythonparser.New().Parse(context.Background(), parserapi.Input{
		Path: "src/app.py", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindClass, "Checkout")
	assertHasNode(t, result.Nodes, graph.KindMethod, "run")
	assertHasNode(t, result.Nodes, graph.KindParameter, "order_id")
	assertHasNode(t, result.Nodes, graph.KindVariable, "request")
	assertHasNode(t, result.Nodes, graph.KindField, "timeout")
	assertHasNode(t, result.Nodes, graph.KindField, "gateway")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "POST /checkout")
	assertHasFact(t, result.Facts, graph.EdgeImports, "requests")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "order.created")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "src.app.ChargeService.charge")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "POST https://payments.test/charge")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "base.Base")
	assertHasFactKind(t, result.Facts, graph.EdgeHasField)
	assertHasFactKind(t, result.Facts, graph.EdgeHandledBy)
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePasses)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
}

func TestParserHandlesEnvironmentSubscriptsAndRecoverableSyntax(t *testing.T) {
	result, err := pythonparser.New().Parse(context.Background(), parserapi.Input{
		Path: "settings.py", Content: []byte("import os\nvalue = os.environ['DATABASE_URL']\ndef broken(:\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "DATABASE_URL")
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Level != "warning" {
		t.Fatalf("expected recoverable-syntax warning, got %#v", result.Diagnostics)
	}
}

func TestParserSupportsPythonFilesCaseInsensitively(t *testing.T) {
	p := pythonparser.New()
	if !p.Supports("service.py") || !p.Supports("SERVICE.PY") || !p.Supports("service.pyi") || p.Supports("service.js") {
		t.Fatal("unexpected Python file support")
	}
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
