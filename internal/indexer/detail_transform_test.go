package indexer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/detailprofile"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestBenchmarkTransformSwitchesProfilesAndConverges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.profile"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(detailFixtureParser{}))

	full, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if full.Counts.ByEdge[string(graph.EdgeAssigns)] != 1 {
		t.Fatalf("full assigns = %d", full.Counts.ByEdge[string(graph.EdgeAssigns)])
	}

	structuralProjector := detailprofile.NewProjector(detailprofile.Options{Profile: detailprofile.ProfileStructural})
	structural, err := service.Run(ctx, project, indexer.Options{ResultTransform: structuralProjector})
	if err != nil {
		t.Fatal(err)
	}
	if structural.Counts.ByEdge[string(graph.EdgeAssigns)] != 0 || len(structural.Updated) != 1 {
		t.Fatalf("structural report = %#v", structural)
	}
	unchanged, err := service.Run(ctx, project, indexer.Options{ResultTransform: structuralProjector})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Unchanged != 1 || len(unchanged.Updated) != 0 || unchanged.Counts.Nodes != structural.Counts.Nodes {
		t.Fatalf("structural refresh did not converge: %#v", unchanged)
	}

	restored, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Counts.ByEdge[string(graph.EdgeAssigns)] != 1 || len(restored.Updated) != 1 {
		t.Fatalf("restored full report = %#v", restored)
	}

	_, err = service.Run(ctx, project, indexer.Options{ResultTransform: canceledTransform{}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transform error = %v", err)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.ByEdge[string(graph.EdgeAssigns)] != 1 {
		t.Fatalf("canceled transform mutated last valid graph: %#v", counts)
	}
}

type detailFixtureParser struct{}

func (detailFixtureParser) Language() string          { return "detail-fixture" }
func (detailFixtureParser) Supports(path string) bool { return filepath.Ext(path) == ".profile" }
func (detailFixtureParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	file := parserapi.FileNode(input, "detail-fixture")
	callable := graph.Node{ID: "callable", Kind: graph.KindFunction, Name: "run", QualifiedName: "fixture.run", OwnerFile: input.Path}
	local := graph.Node{ID: "local", Kind: graph.KindVariable, Name: "value", QualifiedName: "fixture.run.value", OwnerFile: input.Path}
	return graph.ParseResult{Nodes: []graph.Node{file, callable, local}, Facts: []graph.Fact{
		{ID: "declare-callable", FromID: file.ID, Kind: graph.EdgeDeclares, TargetID: callable.ID, OwnerFile: input.Path},
		{ID: "declare-local", FromID: callable.ID, Kind: graph.EdgeDeclares, TargetID: local.ID, OwnerFile: input.Path},
		{ID: "assign", FromID: local.ID, Kind: graph.EdgeAssigns, TargetID: local.ID, OwnerFile: input.Path},
	}}, nil
}

type canceledTransform struct{}

func (canceledTransform) SemanticKey() string { return "canceled-v1" }
func (canceledTransform) Transform(context.Context, parserapi.Input, graph.ParseResult) (graph.ParseResult, error) {
	return graph.ParseResult{}, context.Canceled
}
