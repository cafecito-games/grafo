package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

// edgeKindHistogramSQL mirrors the CountEdgesByKind query in
// internal/storage/sqlite/queries/edges.sql. The plan for this statement is the
// contract: a full-graph edge histogram must stream an index instead of
// spilling into SQLite's external merge sorter.
const edgeKindHistogramSQL = "SELECT kind, COUNT(*) AS count FROM edges GROUP BY kind ORDER BY kind"

func TestEdgeKindHistogramStreamsAnIndexInsteadOfSorting(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "graph.sqlite")
	repository := histogramRepository(t, ctx, databasePath)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	rows, err := database.QueryContext(ctx, "EXPLAIN QUERY PLAN "+edgeKindHistogramSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var selectID, order, from int
		var detail string
		if err := rows.Scan(&selectID, &order, &from, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if strings.Contains(strings.ToUpper(joined), "TEMP B-TREE") {
		t.Fatalf("edge-kind histogram still sorts:\n%s", joined)
	}
	if !strings.Contains(joined, "edges_kind") {
		t.Fatalf("edge-kind histogram does not stream edges_kind:\n%s", joined)
	}
}

func TestCountsMatchAuthoritativeRecount(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "graph.sqlite")
	repository := histogramRepository(t, ctx, databasePath)
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var edges int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM edges").Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if counts.Edges != edges {
		t.Fatalf("Counts reported %d edges, recount found %d", counts.Edges, edges)
	}
	byEdge := map[string]int{}
	rows, err := database.QueryContext(ctx, edgeKindHistogramSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	total := 0
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatal(err)
		}
		byEdge[kind] = count
		total += count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if total != edges {
		t.Fatalf("histogram total %d does not match %d edges", total, edges)
	}
	if len(byEdge) != len(counts.ByEdge) {
		t.Fatalf("histogram kinds = %v, Counts reported %v", byEdge, counts.ByEdge)
	}
	for kind, count := range byEdge {
		if counts.ByEdge[kind] != count {
			t.Fatalf("Counts reported %d %q edges, recount found %d", counts.ByEdge[kind], kind, count)
		}
	}
}

// histogramRepository builds an index holding several edge kinds across mixed
// insert, remove, and reconcile mutations.
func histogramRepository(t *testing.T, ctx context.Context, databasePath string) *sqlite.Repository {
	t.Helper()
	repository, err := sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeContains}
	for index := range 12 {
		owner := fmt.Sprintf("sample%d.go", index)
		caller := graph.Node{ID: fmt.Sprintf("caller%d", index), Kind: graph.KindFunction,
			Name: fmt.Sprintf("Caller%d", index), QualifiedName: fmt.Sprintf("sample.Caller%d", index),
			Language: "go", OwnerFile: owner}
		target := graph.Node{ID: fmt.Sprintf("target%d", index), Kind: graph.KindFunction,
			Name: fmt.Sprintf("Target%d", index), QualifiedName: fmt.Sprintf("sample.Target%d", index),
			Language: "go", OwnerFile: owner}
		fact := graph.Fact{ID: fmt.Sprintf("fact%d", index), FromID: caller.ID, Kind: kinds[index%len(kinds)],
			Producer: "go", Target: target.QualifiedName, TargetKind: target.Kind, OwnerFile: owner,
			Location: graph.Location{Path: owner, Line: index + 1, Column: 1}}
		if err := repository.ReplaceOwner(ctx, owner,
			graph.ParseResult{Nodes: []graph.Node{caller, target}, Facts: []graph.Fact{fact}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveFiles(ctx, []string{"sample11.go"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return repository
}
