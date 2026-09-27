package swift_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	swiftparser "github.com/cafecito-games/grafo/internal/parser/swift"
)

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`import Foundation

protocol Runnable {
    func run(orderID: String) -> String
}

class Base {}

struct ChargeService {
    func charge(_ value: String) -> String { return value }
}

class Checkout: Base, Runnable {
    let timeout: Int = 30
    var service: ChargeService = ChargeService()

    func run(orderID: String) -> String {
        let request = orderID
        let token = ProcessInfo.processInfo.environment["API_TOKEN"]
        service.charge(request)
        URLSession.shared.data(from: URL(string: "https://payments.test/charge")!)
        events.publish("order.created")
        return request
    }
}

func configure() {
    app.post("/checkout")
}
`)
	result, err := swiftparser.New().Parse(context.Background(), parserapi.Input{
		Path: "Sources/App.swift", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindInterface, "Runnable")
	assertHasNode(t, result.Nodes, graph.KindClass, "Checkout")
	assertHasNode(t, result.Nodes, graph.KindType, "ChargeService")
	assertHasNode(t, result.Nodes, graph.KindMethod, "run")
	assertHasNode(t, result.Nodes, graph.KindParameter, "orderID")
	assertHasNode(t, result.Nodes, graph.KindVariable, "request")
	assertHasNode(t, result.Nodes, graph.KindField, "timeout")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "POST /checkout")
	assertHasFact(t, result.Facts, graph.EdgeImports, "Foundation")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "order.created")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "Sources/App.ChargeService.charge")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "ANY https://payments.test/charge")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "Sources/App.Base")
	assertHasFact(t, result.Facts, graph.EdgeImplements, "Sources/App.Runnable")
	assertHasFactKind(t, result.Facts, graph.EdgeHasField)
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePasses)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
	assertHasFactKind(t, result.Facts, graph.EdgeExposes)
}

func TestParserHandlesExtensionsAndRecoverableSyntax(t *testing.T) {
	result, err := swiftparser.New().Parse(context.Background(), parserapi.Input{
		Path: "Feature.swift", Content: []byte("extension Feature { func enabled() -> Bool { true } }\nfunc broken( {\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasQualifiedNode(t, result.Nodes, graph.KindMethod, "Feature.Feature.enabled")
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Level != "warning" {
		t.Fatalf("expected recoverable-syntax warning, got %#v", result.Diagnostics)
	}
}

func TestParserSupportsSwiftFilesCaseInsensitively(t *testing.T) {
	p := swiftparser.New()
	if !p.Supports("Service.swift") || !p.Supports("SERVICE.SWIFT") || p.Supports("Service.m") {
		t.Fatal("unexpected Swift file support")
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

func assertHasQualifiedNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, qualified, nodes)
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
