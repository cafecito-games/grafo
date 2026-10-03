package federation

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
)

func TestGeneratedFromFederationRequiresCanonicalProtobufDeclaration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		node graph.Node
		want bool
	}{
		{name: "protobuf type", node: graph.Node{Kind: graph.KindType, Language: "protobuf"}, want: true},
		{name: "protobuf field", node: graph.Node{Kind: graph.KindField, Language: "protobuf"}, want: true},
		{name: "same-named Go type", node: graph.Node{Kind: graph.KindType, Language: "go"}, want: false},
		{name: "protobuf method", node: graph.Node{Kind: graph.KindMethod, Language: "protobuf"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := candidateAllowed(graph.EdgeGeneratedFrom, test.node); got != test.want {
				t.Fatalf("candidateAllowed(generated_from, %#v) = %v, want %v", test.node, got, test.want)
			}
		})
	}
}

func TestProtocolUsageFederationRequiresCanonicalProtobufDeclaration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		relation graph.EdgeKind
		node     graph.Node
		want     bool
	}{
		{graph.EdgeEncodes, graph.Node{Kind: graph.KindType, Language: "protobuf"}, true},
		{graph.EdgeDecodes, graph.Node{Kind: graph.KindType, Language: "go"}, false},
		{graph.EdgeReads, graph.Node{Kind: graph.KindField, Language: "protobuf"}, true},
		{graph.EdgeWrites, graph.Node{Kind: graph.KindField, Language: "go"}, false},
		{graph.EdgeReads, graph.Node{Kind: graph.KindTable, Language: "sql"}, true},
	}
	for _, test := range tests {
		if got := candidateAllowed(test.relation, test.node); got != test.want {
			t.Errorf("candidateAllowed(%s, %#v) = %v, want %v", test.relation, test.node, got, test.want)
		}
	}
}

func TestTransportFederationRequiresOperationAndCanonicalPayload(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		relation graph.EdgeKind
		node     graph.Node
		want     bool
	}{
		{graph.EdgeSends, graph.Node{Kind: graph.KindTransportOperation}, true},
		{graph.EdgeReceives, graph.Node{Kind: graph.KindMethod}, false},
		{graph.EdgeCarries, graph.Node{Kind: graph.KindType, Language: "protobuf"}, true},
		{graph.EdgeCarries, graph.Node{Kind: graph.KindType, Language: "go"}, false},
	} {
		if got := candidateAllowed(test.relation, test.node); got != test.want {
			t.Errorf("candidateAllowed(%s, %#v) = %v, want %v", test.relation, test.node, got, test.want)
		}
	}
}

func TestMiddlewareFederationRequiresCallableDeclaration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		kind graph.NodeKind
		want bool
	}{
		{graph.KindFunction, true},
		{graph.KindMethod, true},
		{graph.KindConfigKey, false},
		{graph.KindEndpoint, false},
	} {
		if got := candidateAllowed(graph.EdgeUsesMiddleware, graph.Node{Kind: test.kind}); got != test.want {
			t.Errorf("candidateAllowed(uses_middleware, %s) = %v, want %v", test.kind, got, test.want)
		}
	}
}
