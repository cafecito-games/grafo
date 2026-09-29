package graph_test

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
)

func TestRelationEdgeQueryValidatesBoundedExactRequests(t *testing.T) {
	valid := graph.RelationEdgeQuery{SubjectID: "n:subject", Direction: graph.IncomingRelations,
		Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 2}
	tests := []struct {
		name    string
		request graph.RelationEdgeQuery
		wantErr bool
	}{
		{name: "valid", request: valid},
		{name: "duplicate relation is valid", request: graph.RelationEdgeQuery{SubjectID: valid.SubjectID,
			Direction: valid.Direction, Relations: []graph.EdgeKind{graph.EdgeReads, graph.EdgeReads}, Limit: valid.Limit}},
		{name: "empty subject", request: graph.RelationEdgeQuery{Direction: valid.Direction, Relations: valid.Relations, Limit: valid.Limit}, wantErr: true},
		{name: "empty relations", request: graph.RelationEdgeQuery{SubjectID: valid.SubjectID, Direction: valid.Direction, Limit: valid.Limit}, wantErr: true},
		{name: "empty relation", request: graph.RelationEdgeQuery{SubjectID: valid.SubjectID, Direction: valid.Direction,
			Relations: []graph.EdgeKind{""}, Limit: valid.Limit}, wantErr: true},
		{name: "unknown direction", request: graph.RelationEdgeQuery{SubjectID: valid.SubjectID, Direction: "sideways",
			Relations: valid.Relations, Limit: valid.Limit}, wantErr: true},
		{name: "zero limit", request: graph.RelationEdgeQuery{SubjectID: valid.SubjectID, Direction: valid.Direction,
			Relations: valid.Relations}, wantErr: true},
		{name: "negative limit", request: graph.RelationEdgeQuery{SubjectID: valid.SubjectID, Direction: valid.Direction,
			Relations: valid.Relations, Limit: -1}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.request.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestEdgeKindsReturnsDefensiveClosedVocabulary(t *testing.T) {
	first := graph.EdgeKinds()
	if len(first) == 0 || first[0] != graph.EdgeContains || first[len(first)-1] != graph.EdgeUsesGroup {
		t.Fatalf("unexpected edge vocabulary: %v", first)
	}
	first[0] = "mutated"
	if second := graph.EdgeKinds(); second[0] != graph.EdgeContains {
		t.Fatalf("caller mutated shared vocabulary: %v", second)
	}
}
