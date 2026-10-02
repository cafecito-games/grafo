package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func secondaryIndexNames(t *testing.T, repository *Repository) []string {
	t.Helper()
	rows, err := repository.db.QueryContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// storeBulkLoadFixture stores a small graph the same way an indexing run does,
// so a loaded index can be compared row for row against one loaded without the
// bulk path. It stops short of reconciling, because reconciling is itself one of
// the boundaries that finishes a bulk load.
func storeBulkLoadFixture(t *testing.T, repository *Repository) {
	t.Helper()
	ctx := context.Background()
	for file := range 3 {
		path := fmt.Sprintf("pkg/file%d.go", file)
		parsed := graph.ParseResult{}
		for index := range 4 {
			name := fmt.Sprintf("Symbol%d_%d", file, index)
			parsed.Nodes = append(parsed.Nodes, graph.Node{ID: "node:" + name, Kind: graph.KindFunction,
				Name: name, QualifiedName: "pkg." + name, Language: "go", OwnerFile: path,
				Location: graph.Location{Path: path, Line: index + 1, Column: 1}})
			parsed.Facts = append(parsed.Facts, graph.Fact{ID: "fact:" + name, FromID: "node:" + name,
				Kind: graph.EdgeCalls, Producer: "go", Target: "pkg.Symbol0_0", TargetKind: graph.KindFunction,
				Location: graph.Location{Path: path, Line: index + 1, Column: 1}, OwnerFile: path})
		}
		record := graph.FileRecord{Path: path, Hash: fmt.Sprintf("hash%d", file), Language: "go",
			Size: 100, ModifiedNS: 1, IndexedAt: graph.NowUTC()}
		if err := repository.ReplaceFile(ctx, record, parsed); err != nil {
			t.Fatal(err)
		}
	}
}

func bulkLoadFixture(t *testing.T, repository *Repository) {
	t.Helper()
	storeBulkLoadFixture(t, repository)
	if err := repository.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestBulkLoadStoresTheSameGraphAndSchema is the equivalence the capability has
// to hold: deferring secondary indexes may change when they are built, never
// which rows exist, which indexes the finished database has, or what a query
// resolves to.
func TestBulkLoadStoresTheSameGraphAndSchema(t *testing.T) {
	ctx := context.Background()
	deferred := openTestRepository(t)
	control := openTestRepository(t)

	if err := deferred.BeginBulkLoad(ctx); err != nil {
		t.Fatalf("begin bulk load: %v", err)
	}
	if len(deferred.deferredIndexes) == 0 {
		t.Fatal("a bulk load into an empty graph deferred no secondary index")
	}
	bulkLoadFixture(t, deferred)
	if err := deferred.EndBulkLoad(ctx); err != nil {
		t.Fatalf("end bulk load: %v", err)
	}
	bulkLoadFixture(t, control)

	if got, want := secondaryIndexNames(t, deferred), secondaryIndexNames(t, control); !reflect.DeepEqual(got, want) {
		t.Fatalf("secondary indexes after a bulk load = %v, want %v", got, want)
	}
	for _, table := range []string{"nodes", "facts", "edges", "paths"} {
		if got, want := tableRows(t, deferred, table), tableRows(t, control, table); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s differ\ndeferred: %v\ncontrol:  %v", table, got, want)
		}
	}
	deferredCounts, err := deferred.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	controlCounts, err := control.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deferredCounts, controlCounts) {
		t.Fatalf("counts after a bulk load = %+v, want %+v", deferredCounts, controlCounts)
	}
	if ledger, err := deferred.Meta(ctx, deferredIndexesMeta); err != nil || ledger != "" {
		t.Fatalf("deferred index ledger after a finished load = %q (err %v)", ledger, err)
	}
}

// TestBulkLoadRetainsTheIndexesItsOwnQueriesName pins the retained set. Each one
// is either named in an INDEXED BY clause, which SQLite rejects outright when the
// index is missing, or drives a delete that every file replacement issues.
func TestBulkLoadRetainsTheIndexesItsOwnQueriesName(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)
	if err := repository.BeginBulkLoad(ctx); err != nil {
		t.Fatalf("begin bulk load: %v", err)
	}
	present := map[string]bool{}
	for _, name := range secondaryIndexNames(t, repository) {
		present[name] = true
	}
	for name := range bulkLoadRetainedIndexes {
		if !present[name] {
			t.Errorf("bulk load dropped retained index %s", name)
		}
	}
	for _, index := range repository.deferredIndexes {
		if bulkLoadRetainedIndexes[index.Name] {
			t.Errorf("bulk load deferred retained index %s", index.Name)
		}
		if present[index.Name] {
			t.Errorf("bulk load recorded %s as deferred but left it in place", index.Name)
		}
	}
	// Replacing a file and reconciling are exactly the paths that run while the
	// indexes are deferred, so they have to work with what the load retained.
	bulkLoadFixture(t, repository)
	if err := repository.EndBulkLoad(ctx); err != nil {
		t.Fatalf("end bulk load: %v", err)
	}
}

// TestBulkLoadLeavesANonEmptyGraphAlone covers the guard: an index that already
// holds evidence is being updated, not loaded, and dropping its indexes would
// make every incremental delete scan the whole table.
func TestBulkLoadLeavesANonEmptyGraphAlone(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)
	bulkLoadFixture(t, repository)
	before := secondaryIndexNames(t, repository)

	if err := repository.BeginBulkLoad(ctx); err != nil {
		t.Fatalf("begin bulk load: %v", err)
	}
	if len(repository.deferredIndexes) != 0 {
		t.Fatalf("a populated graph deferred %v", repository.deferredIndexes)
	}
	if got := secondaryIndexNames(t, repository); !reflect.DeepEqual(got, before) {
		t.Fatalf("secondary indexes = %v, want %v", got, before)
	}
}

// TestInterruptedBulkLoadIsRepairedOnOpen covers the recovery that makes
// dropping an index safe at all: the ledger outlives the process, and opening
// the database is the only way to reach the graph.
func TestInterruptedBulkLoadIsRepairedOnOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	interrupted, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	complete := secondaryIndexNames(t, interrupted)
	if err := interrupted.BeginBulkLoad(ctx); err != nil {
		t.Fatalf("begin bulk load: %v", err)
	}
	storeBulkLoadFixture(t, interrupted)
	partial := secondaryIndexNames(t, interrupted)
	ledger, err := interrupted.Meta(ctx, deferredIndexesMeta)
	if err != nil {
		t.Fatal(err)
	}
	if ledger == "" {
		t.Fatal("a bulk load in progress recorded no ledger")
	}
	if err := interrupted.Close(); err != nil {
		t.Fatal(err)
	}
	if len(partial) >= len(complete) {
		t.Fatalf("interrupted load still had every index: %v", partial)
	}

	repaired, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen an interrupted bulk load: %v", err)
	}
	defer func() { _ = repaired.Close() }()
	if got := secondaryIndexNames(t, repaired); !reflect.DeepEqual(got, complete) {
		t.Fatalf("secondary indexes after repair = %v, want %v", got, complete)
	}
	if ledger, err := repaired.Meta(ctx, deferredIndexesMeta); err != nil || ledger != "" {
		t.Fatalf("ledger after repair = %q (err %v)", ledger, err)
	}
}
