package mcpserver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/investigation"
	"github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

type investigationGeneration struct {
	generation *FreshnessGeneration
	held       bool
	acquires   int
	releases   int
}

func (f *investigationGeneration) Acquire(context.Context) (*FreshnessGeneration, func(), error) {
	f.held = true
	f.acquires++
	return f.generation, func() { f.held = false; f.releases++ }, nil
}
func TestInvestigationSourceFactoryUsesAcquiredGeneration(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	generation := &investigationGeneration{}
	service := New(repo, indexer.Project{Name: "stale"})
	service.freshness = generation
	factoryCalls := 0
	service.WithInvestigationSource(func(current graph.QueryRepository, projects []indexer.Project) *source.Service {
		factoryCalls++
		if !generation.held {
			t.Fatal("source factory called outside generation lease")
		}
		if current != generation.generation.Repository || len(projects) != 1 || projects[0].Name != generation.generation.Key {
			t.Fatal("source factory received stale generation")
		}
		return source.NewScopedService(current, projects)
	})
	for _, name := range []string{"first", "second"} {
		generation.generation = &FreshnessGeneration{Key: name, Repository: repo, Projects: []indexer.Project{{Name: name, Root: testtemp.Dir(t)}}}
		if _, _, err := service.investigate(ctx, nil, investigation.Request{Query: "Checkout"}); err != nil {
			t.Fatal(err)
		}
		if generation.held {
			t.Fatal("generation not released")
		}
	}
	if factoryCalls != 2 || generation.acquires != 2 || generation.releases != 2 {
		t.Fatalf("factory %d acquires %d releases %d", factoryCalls, generation.acquires, generation.releases)
	}
	service.WithInvestigationSource(func(graph.QueryRepository, []indexer.Project) *source.Service { return nil })
	if _, _, err := service.investigate(ctx, nil, investigation.Request{Query: "Checkout"}); err == nil {
		t.Fatal("nil source factory result silently exposed an evidence report")
	}
	if generation.held {
		t.Fatal("failed source factory leaked generation lease")
	}

}
