package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestRepositoryDerivesAndReconcilesDirectStructuralTestEdges(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "graph.sqlite")
	repository, err := sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	testNode := graph.Node{ID: "test", Kind: graph.KindTest, Name: "TestProduce", QualifiedName: "sample.TestProduce",
		Language: "go", OwnerFile: "sample_test.go", Properties: map[string]string{"test_subtype": "test"}}
	target := graph.Node{ID: "target", Kind: graph.KindFunction, Name: "Produce", QualifiedName: "sample.Produce",
		Language: "go", OwnerFile: "sample.go"}
	fact := graph.Fact{ID: "call", FromID: testNode.ID, Kind: graph.EdgeCalls,
		Producer: "go", Target: target.QualifiedName, TargetKind: target.Kind, OwnerFile: testNode.OwnerFile,
		Location: graph.Location{Path: testNode.OwnerFile, Line: 4, Column: 2}, Properties: map[string]string{"resolution": "go/types"}}
	if err := repository.ReplaceOwner(ctx, testNode.OwnerFile, graph.ParseResult{Nodes: []graph.Node{testNode}, Facts: []graph.Fact{fact}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, target.OwnerFile, graph.ParseResult{Nodes: []graph.Node{target}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertDirectTestEdge(t, ctx, repository, testNode.ID, target.ID)

	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err = sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	assertDirectTestEdge(t, ctx, repository, testNode.ID, target.ID)

	// Removing the declaration retargets the source call to an unresolved
	// external node and removes the evidence-derived tests edge.
	if err := repository.ReplaceOwner(ctx, target.OwnerFile, graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, testNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind == graph.EdgeTests {
			t.Fatalf("stale test edge after target removal: %#v", edge)
		}
	}
}

func TestRepositoryDoesNotDeriveTestEdgesForSupportOrUnresolvedTargets(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	testNode := graph.Node{ID: "test", Kind: graph.KindTest, Name: "TestRun", QualifiedName: "sample.TestRun", OwnerFile: "sample_test.go"}
	helper := graph.Node{ID: "helper", Kind: graph.KindFunction, Name: "helper", QualifiedName: "sample.helper",
		OwnerFile: "sample_test.go", Properties: map[string]string{"test_role": "helper"}}
	facts := []graph.Fact{
		{ID: "helper-call", FromID: testNode.ID, Kind: graph.EdgeCalls, TargetID: helper.ID, Target: helper.QualifiedName, OwnerFile: testNode.OwnerFile},
		{ID: "missing-call", FromID: testNode.ID, Kind: graph.EdgeCalls, Target: "sample.Missing", OwnerFile: testNode.OwnerFile},
		{ID: "ambiguous-call", FromID: testNode.ID, Kind: graph.EdgeCalls, Target: "Produce", OwnerFile: testNode.OwnerFile},
	}
	if err := repository.ReplaceOwner(ctx, testNode.OwnerFile, graph.ParseResult{Nodes: []graph.Node{testNode, helper}, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "production.go", graph.ParseResult{Nodes: []graph.Node{
		{ID: "first-produce", Kind: graph.KindFunction, Name: "Produce", QualifiedName: "first.Produce", OwnerFile: "production.go"},
		{ID: "second-produce", Kind: graph.KindFunction, Name: "Produce", QualifiedName: "second.Produce", OwnerFile: "production.go"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, testNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind == graph.EdgeTests {
			t.Fatalf("unsupported target produced test edge: %#v", edge)
		}
	}
}

func assertDirectTestEdge(t *testing.T, ctx context.Context, repository *sqlite.Repository, sourceID, targetID string) {
	t.Helper()
	edges, err := repository.EdgesFrom(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	var found graph.Edge
	for _, edge := range edges {
		if edge.Kind == graph.EdgeTests {
			found = edge
		}
	}
	if found.ToID != targetID || found.FactID != "call" || found.Producer != "go" ||
		found.Location.Path != "sample_test.go" || found.Properties["coverage"] != "structural" ||
		found.Properties["evidence_relation"] != string(graph.EdgeCalls) {
		t.Fatalf("derived test edge = %#v; all edges = %#v", found, edges)
	}
}
