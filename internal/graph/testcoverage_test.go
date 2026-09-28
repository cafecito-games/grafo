package graph_test

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
)

func TestDirectTestEdgeRequiresExactProductionEvidence(t *testing.T) {
	testNode := graph.Node{ID: "test", Kind: graph.KindTest, QualifiedName: "pkg.TestThing"}
	production := graph.Node{ID: "production", Kind: graph.KindFunction, QualifiedName: "pkg.Thing"}
	evidence := graph.Edge{ID: "call", FactID: "fact", FromID: testNode.ID, ToID: production.ID,
		Kind: graph.EdgeCalls, Producer: "go", Location: graph.Location{Path: "thing_test.go", Line: 12},
		Properties: map[string]string{"resolution": "go/types"}}

	edge, ok := graph.DirectTestEdge(testNode, production, evidence)
	if !ok {
		t.Fatal("resolved test-to-production call did not derive a tests edge")
	}
	if edge.Kind != graph.EdgeTests || edge.FromID != testNode.ID || edge.ToID != production.ID ||
		edge.FactID != evidence.FactID || edge.Producer != evidence.Producer || edge.Location != evidence.Location {
		t.Fatalf("derived tests edge lost evidence: %#v", edge)
	}
	if edge.ID == evidence.ID || edge.Properties["evidence_relation"] != string(graph.EdgeCalls) ||
		edge.Properties["coverage"] != "structural" || edge.Properties["resolution"] != "go/types" {
		t.Fatalf("derived tests edge identity/properties = %#v", edge)
	}
	reference := evidence
	reference.ID = "reference"
	reference.Kind = graph.EdgeReferences
	if edge, ok := graph.DirectTestEdge(testNode, production, reference); !ok ||
		edge.Properties["evidence_relation"] != string(graph.EdgeReferences) {
		t.Fatalf("resolved reference did not preserve direct test evidence: %#v, %v", edge, ok)
	}

	for name, item := range map[string]struct {
		source   graph.Node
		target   graph.Node
		relation graph.EdgeKind
	}{
		"ordinary source":      {graph.Node{ID: "caller", Kind: graph.KindFunction}, production, graph.EdgeCalls},
		"unresolved target":    {testNode, graph.Node{ID: "external", Kind: graph.KindFunction, External: true}, graph.EdgeCalls},
		"another test":         {testNode, graph.Node{ID: "other-test", Kind: graph.KindTest}, graph.EdgeCalls},
		"test helper":          {testNode, graph.Node{ID: "helper", Kind: graph.KindFunction, Properties: map[string]string{"test_role": "helper"}}, graph.EdgeCalls},
		"lifecycle method":     {testNode, graph.Node{ID: "before", Kind: graph.KindMethod, Properties: map[string]string{"test_role": "lifecycle"}}, graph.EdgeCalls},
		"unsupported relation": {testNode, production, graph.EdgeAssigns},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := evidence
			candidate.FromID, candidate.ToID, candidate.Kind = item.source.ID, item.target.ID, item.relation
			if got, ok := graph.DirectTestEdge(item.source, item.target, candidate); ok {
				t.Fatalf("unexpected tests edge: %#v", got)
			}
		})
	}
}

func TestTestVocabularyIsFirstClassAndCallable(t *testing.T) {
	if parsed, err := graph.ParseNodeKind("test"); err != nil || parsed != graph.KindTest {
		t.Fatalf("ParseNodeKind(test) = %q, %v", parsed, err)
	}
	if !graph.AllowsResolutionKind(graph.EdgeCalls, graph.KindTest) {
		t.Fatal("calls cannot resolve another first-class test declaration")
	}
}
