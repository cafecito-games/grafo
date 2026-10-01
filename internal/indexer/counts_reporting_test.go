package indexer_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type failingCountsRepository struct {
	graph.IndexRepository
}

func (f failingCountsRepository) Counts(ctx context.Context) (graph.Counts, error) {
	return graph.Counts{Files: 99, Nodes: 99}, errors.New("count query failed")
}

// TestCountsFailureLeavesIndexRunSuccessful pins the fail-closed contract: a
// failed count query reports counts as uncollected and never fabricates totals,
// while the indexing run itself still succeeds because the graph is durable.
func TestCountsFailureLeavesIndexRunSuccessful(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "README.md"), "# Sample\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(markdownparser.New())
	report, err := indexer.NewService(failingCountsRepository{IndexRepository: repository}, registry).
		Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatalf("count failure aborted the index run: %v", err)
	}
	if report.CountsCollected {
		t.Fatalf("failed counts reported as collected: %#v", report)
	}
	if report.Counts.Files != 0 || report.Counts.Nodes != 0 {
		t.Fatalf("failed counts leaked into the report: %#v", report.Counts)
	}
	if len(report.Updated) != 1 {
		t.Fatalf("indexing results lost on count failure: %#v", report.Updated)
	}
}
