package federation

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
)

func TestGeneratedFromFederationRequiresCanonicalProtobufDeclaration(t *testing.T) {
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
