package query_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestNeighborhoodIsDeterministic(t *testing.T) {
	a := node("A")
	b := node("B")
	c := node("C")
	repository := &fakeRepository{
		nodes: map[string]graph.Node{a.ID: a, b.ID: b, c.ID: c},
		edges: []graph.Edge{
			{ID: "edge-c", FromID: a.ID, ToID: c.ID, Kind: graph.EdgeCalls},
			{ID: "edge-b", FromID: a.ID, ToID: b.ID, Kind: graph.EdgeCalls},
		},
	}
	service := query.NewService(repository)
	first, err := service.Neighborhood(context.Background(), "A", 2, query.Outgoing, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Neighborhood(context.Background(), "A", 2, query.Outgoing, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same graph produced different traversal:\n%#v\n%#v", first, second)
	}
	if len(first.Nodes) != 3 || first.Nodes[1].Node.Name != "B" || first.Nodes[2].Node.Name != "C" {
		t.Fatalf("unexpected stable ordering: %#v", first.Nodes)
	}
}

func node(name string) graph.Node {
	return graph.Node{ID: graph.NodeID(graph.KindFunction, name), Kind: graph.KindFunction, Name: name, QualifiedName: name}
}

type fakeRepository struct {
	nodes map[string]graph.Node
	edges []graph.Edge
}

func (f *fakeRepository) SearchNodes(_ context.Context, term string, _ int) ([]graph.Node, error) {
	var result []graph.Node
	for _, value := range f.nodes {
		if strings.Contains(strings.ToLower(value.Name), strings.ToLower(term)) {
			result = append(result, value)
		}
	}
	return result, nil
}

func (f *fakeRepository) Node(_ context.Context, id string) (graph.Node, error) {
	value, ok := f.nodes[id]
	if !ok {
		return graph.Node{}, errors.New("not found")
	}
	return value, nil
}

func (f *fakeRepository) EdgesFrom(_ context.Context, id string) ([]graph.Edge, error) {
	var result []graph.Edge
	for _, edge := range f.edges {
		if edge.FromID == id {
			result = append(result, edge)
		}
	}
	return result, nil
}

func (f *fakeRepository) EdgesTo(_ context.Context, id string) ([]graph.Edge, error) {
	var result []graph.Edge
	for _, edge := range f.edges {
		if edge.ToID == id {
			result = append(result, edge)
		}
	}
	return result, nil
}
