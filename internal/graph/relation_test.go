package graph_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
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

func TestClosedKindAccessorsCoverEveryDeclaredConstant(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(source), "model.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string][]string{"NodeKind": {}, "EdgeKind": {}}
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			kind, ok := value.Type.(*ast.Ident)
			literal, literalOK := value.Values[0].(*ast.BasicLit)
			if !ok || !literalOK || (kind.Name != "NodeKind" && kind.Name != "EdgeKind") {
				continue
			}
			decoded, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			declared[kind.Name] = append(declared[kind.Name], decoded)
		}
	}
	nodes := make([]string, len(graph.NodeKinds()))
	for index, kind := range graph.NodeKinds() {
		nodes[index] = string(kind)
	}
	edges := make([]string, len(graph.EdgeKinds()))
	for index, kind := range graph.EdgeKinds() {
		edges[index] = string(kind)
	}
	for _, values := range [][]string{declared["NodeKind"], declared["EdgeKind"], nodes, edges} {
		sort.Strings(values)
	}
	if !reflect.DeepEqual(declared["NodeKind"], nodes) {
		t.Fatalf("NodeKinds() = %v, declared %v", nodes, declared["NodeKind"])
	}
	if !reflect.DeepEqual(declared["EdgeKind"], edges) {
		t.Fatalf("EdgeKinds() = %v, declared %v", edges, declared["EdgeKind"])
	}
}
