package java_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	javaparser "github.com/cafecito-games/grafo/internal/parser/java"
)

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`package com.example.checkout;

import com.acme.events.EventBus;
import org.springframework.web.client.RestTemplate;
import static com.acme.trace.Trace.record;

interface Handler {
    String run(String orderId);
}

class Base {}

record Receipt(String id) {}

class ChargeService {
    String charge(String value) { return value; }
}

@RequestMapping("/api")
class Checkout extends Base implements Handler {
    private final ChargeService service = new ChargeService();
    private final EventBus events = new EventBus();
    private final RestTemplate client = new RestTemplate();

    @PostMapping("/checkout")
    public String run(String orderId) {
        String request = orderId;
        String token = System.getenv("API_TOKEN");
        service.charge(request);
        client.postForObject("https://payments.test/charge", request, String.class);
        events.publish("order.created");
        record(request);
        return request;
    }
}
`)
	result, err := javaparser.New().Parse(context.Background(), parserapi.Input{
		Path: "src/main/java/com/example/checkout/Checkout.java", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindInterface, "Handler")
	assertHasNode(t, result.Nodes, graph.KindClass, "Checkout")
	assertHasNode(t, result.Nodes, graph.KindType, "Receipt")
	assertHasNode(t, result.Nodes, graph.KindMethod, "run")
	assertHasNode(t, result.Nodes, graph.KindParameter, "orderId")
	assertHasNode(t, result.Nodes, graph.KindVariable, "request")
	assertHasNode(t, result.Nodes, graph.KindField, "service")
	assertHasNode(t, result.Nodes, graph.KindField, "id")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "POST /api/checkout")
	assertHasFact(t, result.Facts, graph.EdgeImports, "com.acme.events.EventBus")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "order.created")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "com.example.checkout.ChargeService.charge")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "com.acme.trace.Trace.record")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "POST https://payments.test/charge")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "com.example.checkout.Base")
	assertHasFact(t, result.Facts, graph.EdgeImplements, "com.example.checkout.Handler")
	assertHasFactKind(t, result.Facts, graph.EdgeHasField)
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePasses)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
	assertHasFactKind(t, result.Facts, graph.EdgeExposes)
	assertHasFactKind(t, result.Facts, graph.EdgeHandledBy)
}

func TestParserDoesNotTreatUnrelatedPutCallsAsHTTP(t *testing.T) {
	result, err := javaparser.New().Parse(context.Background(), parserapi.Input{
		Path: "Cache.java",
		Content: []byte(`class Cache {
    void save(java.util.Map<String, String> values) {
        values.put("/not-an-endpoint", "value");
    }
}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			t.Fatalf("unrelated Map.put call became an HTTP request: %#v", fact)
		}
	}
}

func TestParserKeepsWildcardImportCallsUnresolved(t *testing.T) {
	result, err := javaparser.New().Parse(context.Background(), parserapi.Input{
		Path: "Ambiguous.java",
		Content: []byte(`import alpha.*;
import beta.*;
class Ambiguous {
    void run(Service service) { service.execute(); }
}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "Service.execute")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && (fact.Target == "alpha.Service.execute" || fact.Target == "beta.Service.execute") {
			t.Fatalf("wildcard import produced a guessed call target: %#v", fact)
		}
	}
}

func TestParserHandlesConstructorsJAXRSAndRecoverableSyntax(t *testing.T) {
	result, err := javaparser.New().Parse(context.Background(), parserapi.Input{
		Path: "Resource.java",
		Content: []byte(`@Path("/orders")
class Resource {
    Resource(String name) {}

    @GET
    @Path("/{id}")
    String find(String id) { return id; }
}
class Broken { void missing( {
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasQualifiedNode(t, result.Nodes, graph.KindMethod, "Resource.Resource")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "GET /orders/{id}")
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Level != "warning" {
		t.Fatalf("expected recoverable-syntax warning, got %#v", result.Diagnostics)
	}
}

func TestParserSupportsJavaFilesCaseInsensitively(t *testing.T) {
	p := javaparser.New()
	tests := map[string]bool{"Service.java": true, "SERVICE.JAVA": true, "Service.kt": false}
	for path, want := range tests {
		if got := p.Supports(path); got != want {
			t.Errorf("Supports(%q) = %t, want %t", path, got, want)
		}
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
