package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func BenchmarkRepositoryOpenAndExactNode(b *testing.B) {
	path, nodeIDs := benchmarkReadDatabase(b)
	ctx := context.Background()
	b.Run("query-only", func(b *testing.B) {
		for range b.N {
			repository, err := OpenReadOnly(ctx, path)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := repository.Node(ctx, nodeIDs[0]); err != nil {
				b.Fatal(err)
			}
			if err := repository.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("writable", func(b *testing.B) {
		for range b.N {
			repository, err := Open(ctx, path)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := repository.Node(ctx, nodeIDs[0]); err != nil {
				b.Fatal(err)
			}
			if err := repository.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkReadOnlyRepresentativeQueries(b *testing.B) {
	path, nodeIDs := benchmarkReadDatabase(b)
	ctx := context.Background()
	for _, adapter := range []struct {
		name string
		open func() (graph.QueryRepository, func() error, error)
	}{
		{name: "query-only", open: func() (graph.QueryRepository, func() error, error) {
			repository, err := OpenReadOnly(ctx, path)
			if err != nil {
				return nil, nil, err
			}
			return repository, repository.Close, err
		}},
		{name: "writable", open: func() (graph.QueryRepository, func() error, error) {
			repository, err := Open(ctx, path)
			if err != nil {
				return nil, nil, err
			}
			return repository, repository.Close, err
		}},
	} {
		b.Run(adapter.name, func(b *testing.B) {
			repository, closeRepository, err := adapter.open()
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = closeRepository() })
			b.Run("exact-node", func(b *testing.B) {
				for range b.N {
					if _, err := repository.Node(ctx, nodeIDs[0]); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("symbol-match", func(b *testing.B) {
				for range b.N {
					if _, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "fixture.A", Limit: 10}); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("callers", func(b *testing.B) {
				for range b.N {
					if _, err := repository.EdgesTo(ctx, nodeIDs[2]); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("bounded-path", func(b *testing.B) {
				service := query.NewService(repository)
				for range b.N {
					if _, err := service.ShortestPath(ctx, "fixture.A", "fixture.C", graph.KindFunction,
						query.Outgoing, []graph.EdgeKind{graph.EdgeCalls}, 10); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func benchmarkReadDatabase(b *testing.B) (string, []string) {
	b.Helper()
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(b), "graph.sqlite")
	repository, err := Open(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	nodes := []graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "A", QualifiedName: "fixture.A", OwnerFile: "fixture.go"},
		{ID: "b", Kind: graph.KindFunction, Name: "B", QualifiedName: "fixture.B", OwnerFile: "fixture.go"},
		{ID: "c", Kind: graph.KindFunction, Name: "C", QualifiedName: "fixture.C", OwnerFile: "fixture.go"},
	}
	facts := []graph.Fact{
		{ID: "a-b", FromID: "a", Kind: graph.EdgeCalls, TargetID: "b", OwnerFile: "fixture.go"},
		{ID: "b-c", FromID: "b", Kind: graph.EdgeCalls, TargetID: "c", OwnerFile: "fixture.go"},
	}
	if err := repository.ReplaceOwner(ctx, "fixture.go", graph.ParseResult{Nodes: nodes, Facts: facts}); err != nil {
		b.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		b.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		b.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		b.Fatal(err)
	}
	return path, []string{"a", "b", "c"}
}
