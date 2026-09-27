package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

// BenchmarkGeneratedFixtureWrites compares the dynamic bounded adapter with
// the sqlc single-row reference using the same deterministic rows and one
// transaction per sample.
func BenchmarkGeneratedFixtureWrites(b *testing.B) {
	const rowCount = 5_000
	nodes := make([]sqlcgen.UpsertNodeParams, rowCount)
	facts := make([]sqlcgen.UpsertFactParams, rowCount)
	edges := make([]sqlcgen.InsertEdgeParams, rowCount)
	for index := range rowCount {
		id := fmt.Sprintf("node-%06d", index)
		factID := fmt.Sprintf("fact-%06d", index)
		nodes[index] = sqlcgen.UpsertNodeParams{ID: id, Kind: "function", Name: id,
			QualifiedName: "generated." + id, Language: "go", Path: "generated.go", Line: int64(index + 1),
			Properties: `{"fixture":"deterministic"}`, OwnerFile: "generated.go"}
		facts[index] = sqlcgen.UpsertFactParams{ID: factID, FromID: id, Kind: "calls", TargetID: id,
			Path: "generated.go", Line: int64(index + 1), Properties: `{"fixture":"deterministic"}`, OwnerFile: "generated.go"}
		edges[index] = sqlcgen.InsertEdgeParams{ID: "edge-" + factID, FactID: factID, FromID: id, ToID: id,
			Kind: "calls", Path: "generated.go", Line: int64(index + 1), Properties: `{"fixture":"deterministic"}`}
	}

	for _, implementation := range []string{"single", "bulk"} {
		b.Run(implementation, func(b *testing.B) {
			ctx := context.Background()
			b.ReportMetric(rowCount*3, "rows/op")
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				repository := openTestRepository(b)
				b.StartTimer()
				err := repository.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
					for index := range rowCount {
						if implementation == "single" {
							if err := q.UpsertNode(ctx, nodes[index]); err != nil {
								return err
							}
							if err := q.MarkDirtyNode(ctx, nodes[index].ID); err != nil {
								return err
							}
							if err := q.MarkDirtyTarget(ctx, sqlcgen.MarkDirtyTargetParams{Target: nodes[index].Name, TargetKind: nodes[index].Kind}); err != nil {
								return err
							}
							if err := q.MarkDirtyTarget(ctx, sqlcgen.MarkDirtyTargetParams{Target: nodes[index].QualifiedName, TargetKind: nodes[index].Kind}); err != nil {
								return err
							}
							continue
						}
						if err := writer.addNode(ctx, nodes[index]); err != nil {
							return err
						}
						if err := writer.addDirtyNode(ctx, nodes[index].ID); err != nil {
							return err
						}
						if err := writer.addDirtyTarget(ctx, nodes[index].Name, nodes[index].Kind); err != nil {
							return err
						}
						if err := writer.addDirtyTarget(ctx, nodes[index].QualifiedName, nodes[index].Kind); err != nil {
							return err
						}
					}
					for index := range rowCount {
						if implementation == "single" {
							if err := q.UpsertFact(ctx, facts[index]); err != nil {
								return err
							}
						} else if err := writer.addFact(ctx, facts[index]); err != nil {
							return err
						}
					}
					for index := range rowCount {
						if implementation == "single" {
							if err := q.InsertEdge(ctx, edges[index]); err != nil {
								return err
							}
						} else if err := writer.addEdge(ctx, edges[index]); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				counts, err := repository.Counts(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if counts.Nodes != rowCount || counts.Facts != rowCount || counts.Edges != rowCount {
					b.Fatalf("graph counts = %#v", counts)
				}
				if err := repository.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
