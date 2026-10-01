package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func BenchmarkCatalogEvidence(b *testing.B) {
	const degree = 10_000
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(b), "catalog.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = repository.Close() })

	subject := graph.Node{ID: "n:table", Kind: graph.KindTable, Name: "orders",
		QualifiedName: "orders", OwnerFile: "fixture.go"}
	source := graph.Node{ID: "source", Kind: graph.KindFunction, Name: "UseOrders",
		QualifiedName: "fixture.UseOrders", OwnerFile: "fixture.go"}
	facts := make([]graph.Fact, 0, degree+1)
	for index := 0; index < degree; index++ {
		facts = append(facts, graph.Fact{ID: fmt.Sprintf("irrelevant-%05d", index), FromID: source.ID,
			Kind: graph.EdgeCalls, TargetID: subject.ID, OwnerFile: "fixture.go"})
	}
	facts = append(facts, graph.Fact{ID: "read", FromID: source.ID, Kind: graph.EdgeReads,
		TargetID: subject.ID, OwnerFile: "fixture.go"})
	if err := repository.ReplaceOwner(ctx, "fixture.go", graph.ParseResult{
		Nodes: []graph.Node{subject, source}, Facts: facts,
	}); err != nil {
		b.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		b.Fatal(err)
	}
	catalog := query.NewCatalog(repository)
	b.ResetTimer()
	b.ReportMetric(degree, "fixture_degree")
	for range b.N {
		usage, err := catalog.DataResourceUsage(ctx, subject.ID, query.CatalogOptions{Limit: 1})
		if err != nil {
			b.Fatal(err)
		}
		if len(usage.Readers) != 1 {
			b.Fatalf("readers = %d", len(usage.Readers))
		}
	}
}
