package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

func TestBulkWritesAreExactlyEquivalentToSingleRowQueries(t *testing.T) {
	ctx := context.Background()
	bulk := openTestRepository(t)
	control := openTestRepository(t)

	nodes := []sqlcgen.UpsertNodeParams{
		{ID: "caller", Kind: "function", Name: "Caller", QualifiedName: "pkg.Caller", Language: "go", Path: "a.go", Line: 2, ColumnNo: 3, EndLine: 4, Properties: `{"a":"1"}`, OwnerFile: "a.go"},
		{ID: "target", Kind: "function", Name: "Target", QualifiedName: "pkg.Target", OwnerFile: "b.go"},
		{ID: "caller", Kind: "method", Name: "Caller2", QualifiedName: "pkg.Caller2", Language: "go", Path: "updated.go", Line: 7, ColumnNo: 8, EndLine: 9, Properties: `{"updated":"true"}`, OwnerFile: "updated.go"},
	}
	facts := []sqlcgen.UpsertFactParams{
		{ID: "fact", FromID: "caller", Kind: "calls", TargetID: "target", Path: "a.go", Line: 5, Properties: `{"proof":"direct"}`, OwnerFile: "a.go"},
		{ID: "fact", Source: "pkg.Caller", SourceKind: "method", Kind: "references", Target: "pkg.Target", TargetKind: "function", Path: "updated.go", Line: 11, ColumnNo: 2, EndLine: 12, Properties: `{"proof":"name"}`, OwnerFile: "updated.go"},
	}
	edges := []sqlcgen.InsertEdgeParams{
		{ID: "edge-1", FactID: "fact", FromID: "caller", ToID: "target", Kind: "references", Path: "updated.go", Line: 11, ColumnNo: 2, EndLine: 12, Properties: `{"proof":"name"}`},
		{ID: "edge-2", FactID: "other", FromID: "target", ToID: "caller", Kind: "calls", Properties: `{}`},
	}

	if err := control.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		for _, row := range nodes {
			if err := q.UpsertNode(ctx, row); err != nil {
				return err
			}
		}
		for _, row := range facts {
			if err := q.UpsertFact(ctx, row); err != nil {
				return err
			}
		}
		for _, row := range edges {
			if err := q.InsertEdge(ctx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := bulk.inTransaction(ctx, func(_ *sqlcgen.Queries, writer *batchWriter) error {
		for _, row := range nodes {
			if err := writer.addNode(ctx, row); err != nil {
				return err
			}
		}
		for _, row := range facts {
			if err := writer.addFact(ctx, row); err != nil {
				return err
			}
		}
		for _, row := range edges {
			if err := writer.addEdge(ctx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"nodes", "facts", "edges"} {
		want := tableRows(t, control, table)
		got := tableRows(t, bulk, table)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s differ\nbulk:    %v\ncontrol: %v", table, got, want)
		}
	}
}

func TestBulkConstraintErrorRollsBackTransaction(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)
	duplicate := sqlcgen.InsertEdgeParams{ID: "duplicate", FactID: "fact", FromID: "a", ToID: "b", Kind: "calls"}
	err := repository.inTransaction(ctx, func(_ *sqlcgen.Queries, writer *batchWriter) error {
		if err := writer.addEdge(ctx, duplicate); err != nil {
			return err
		}
		return writer.addEdge(ctx, duplicate)
	})
	if err == nil {
		t.Fatal("duplicate edge batch unexpectedly committed")
	}
	if rows := tableRows(t, repository, "edges"); len(rows) != 0 {
		t.Fatalf("rolled-back edges = %v", rows)
	}
}

func TestBulkLimitsAndCommittedInstrumentation(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)
	repository.limits = batchLimits{MaxRows: 100, MaxVariables: 28, MaxBytes: 1 << 20}
	parsed := graph.ParseResult{}
	for index := range 5 {
		id := fmt.Sprintf("node-%d", index)
		parsed.Nodes = append(parsed.Nodes, graph.Node{ID: id, Kind: graph.KindFunction, Name: id, QualifiedName: "pkg." + id, OwnerFile: "bulk.go"})
		parsed.Facts = append(parsed.Facts, graph.Fact{ID: "fact-" + id, FromID: id, Kind: graph.EdgeCalls, TargetID: id, OwnerFile: "bulk.go"})
	}
	if err := repository.ReplaceOwner(ctx, "bulk.go", parsed); err != nil {
		t.Fatal(err)
	}
	stats := repository.WriteStats()
	if stats.Nodes.Batches != 3 || stats.Nodes.Rows != 5 || stats.Facts.Batches != 3 || stats.Facts.Rows != 5 {
		t.Fatalf("unexpected write stats: %#v", stats)
	}

	before := repository.WriteStats()
	repository.limits.MaxBytes = 64
	huge := graph.Node{ID: "huge", Kind: graph.KindFunction, Name: "huge", QualifiedName: "pkg.huge",
		OwnerFile: "huge.go", Properties: map[string]string{"payload": strings.Repeat("x", 4096)}}
	if err := repository.ReplaceOwner(ctx, "huge.go", graph.ParseResult{Nodes: []graph.Node{huge}}); err != nil {
		t.Fatal(err)
	}
	delta := subtractWriteStats(repository.WriteStats(), before)
	if delta.Nodes.Rows != 1 || delta.Nodes.Batches != 1 || delta.Nodes.Bytes < 4096 {
		t.Fatalf("oversized row stats = %#v", delta.Nodes)
	}
}

func TestCanceledEdgeBatchRollsBackAndRestartResumesQueue(t *testing.T) {
	database := filepath.Join(t.TempDir(), "graph.sqlite")
	ctx := context.Background()
	repository, err := Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	caller := graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "Caller", QualifiedName: "pkg.Caller", OwnerFile: "caller.go"}
	target := graph.Node{ID: "target", Kind: graph.KindFunction, Name: "Target", QualifiedName: "pkg.Target", OwnerFile: "target.go"}
	fact := graph.Fact{ID: "call", FromID: caller.ID, Kind: graph.EdgeCalls, Target: target.QualifiedName, OwnerFile: "caller.go"}
	if err := repository.ReplaceOwner(ctx, "caller.go", graph.ParseResult{Nodes: []graph.Node{caller}, Facts: []graph.Fact{fact}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "target.go", graph.ParseResult{Nodes: []graph.Node{target}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "target.go", graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	repository.afterBatch = func(kind string, _ graph.WriteBatchStats) {
		if kind == edgeBatchSpec.name {
			cancel()
		}
	}
	if err := repository.Reconcile(canceled); err == nil {
		t.Fatal("canceled reconciliation unexpectedly committed")
	}
	repository.afterBatch = nil
	pending, err := repository.queries.CountDirtyFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending queue rows = %d, want 1", pending)
	}
	edges, err := repository.EdgesFrom(ctx, caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID != target.ID {
		t.Fatalf("replacement transaction did not roll back: %#v", edges)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err = Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err = repository.EdgesFrom(ctx, caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID == target.ID {
		t.Fatalf("restart did not resume replacement: %#v", edges)
	}
	external, err := repository.Node(ctx, edges[0].ToID)
	if err != nil || !external.External || external.QualifiedName != target.QualifiedName {
		t.Fatalf("resumed target = %#v, err=%v", external, err)
	}
}

func TestCanceledNamedSourceBatchRollsBackAndRestartResumesQueue(t *testing.T) {
	database := filepath.Join(t.TempDir(), "named-source.sqlite")
	ctx := context.Background()
	repository, err := Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	source := graph.Node{ID: "signal", Kind: graph.KindEvent, Name: "ready",
		QualifiedName: "Backend.ready", OwnerFile: "backend.gd"}
	target := graph.Node{ID: "handler", Kind: graph.KindMethod, Name: "on_ready",
		QualifiedName: "Kit.on_ready", OwnerFile: "kit.gd"}
	fact := graph.Fact{ID: "handled", Source: source.QualifiedName, SourceKind: graph.KindEvent,
		Kind: graph.EdgeHandledBy, TargetID: target.ID, OwnerFile: "kit.gd"}
	if err := repository.ReplaceOwner(ctx, "kit.gd", graph.ParseResult{Nodes: []graph.Node{target}, Facts: []graph.Fact{fact}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "backend.gd", graph.ParseResult{Nodes: []graph.Node{source}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "backend.gd", graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(ctx)
	repository.afterBatch = func(kind string, _ graph.WriteBatchStats) {
		if kind == edgeBatchSpec.name {
			cancel()
		}
	}
	if err := repository.Reconcile(canceled); err == nil {
		t.Fatal("canceled named-source reconciliation unexpectedly committed")
	}
	repository.afterBatch = nil
	edges, err := repository.EdgesTo(ctx, target.ID)
	if err != nil || len(edges) != 1 || edges[0].FromID != source.ID {
		t.Fatalf("named-source replacement did not roll back: edges=%#v err=%v", edges, err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err = Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err = repository.EdgesTo(ctx, target.ID)
	if err != nil || len(edges) != 1 || edges[0].FromID == source.ID {
		t.Fatalf("restart did not resume named-source replacement: edges=%#v err=%v", edges, err)
	}
	unresolved, err := repository.Node(ctx, edges[0].FromID)
	if err != nil || !unresolved.External || unresolved.QualifiedName != source.QualifiedName {
		t.Fatalf("resumed source = %#v, err=%v", unresolved, err)
	}
}

func TestBulkExternalDeduplicationAndInternalConvergence(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)
	callers := []graph.Node{
		{ID: "one", Kind: graph.KindFunction, Name: "One", QualifiedName: "pkg.One", OwnerFile: "calls.go"},
		{ID: "two", Kind: graph.KindFunction, Name: "Two", QualifiedName: "pkg.Two", OwnerFile: "calls.go"},
	}
	facts := []graph.Fact{
		{ID: "one-call", FromID: "one", Kind: graph.EdgeCalls, Target: "pkg.Missing", OwnerFile: "calls.go"},
		{ID: "two-call", FromID: "two", Kind: graph.EdgeCalls, Target: "pkg.Missing", OwnerFile: "calls.go"},
	}
	if err := repository.ReplaceOwner(ctx, "calls.go", graph.ParseResult{Nodes: callers, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.External != 1 || counts.Edges != 2 {
		t.Fatalf("unresolved counts = %#v", counts)
	}
	declaration := graph.Node{ID: "missing", Kind: graph.KindFunction, Name: "Missing", QualifiedName: "pkg.Missing", OwnerFile: "target.go"}
	if err := repository.ReplaceOwner(ctx, "target.go", graph.ParseResult{Nodes: []graph.Node{declaration}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err = repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.External != 0 || counts.Edges != 2 {
		t.Fatalf("converged counts = %#v", counts)
	}
	for _, caller := range callers {
		edges, err := repository.EdgesFrom(ctx, caller.ID)
		if err != nil || len(edges) != 1 || edges[0].ToID != declaration.ID {
			t.Fatalf("%s edges = %#v, err=%v", caller.ID, edges, err)
		}
	}
}

func openTestRepository(t testing.TB) *Repository {
	t.Helper()
	repository, err := Open(context.Background(), filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

func tableRows(t *testing.T, repository *Repository, table string) []string {
	t.Helper()
	columns := map[string]string{
		"nodes": "id,kind,name,qualified_name,language,path,line,column_no,end_line,properties,owner_file,external,name_folded,qualified_name_folded",
		"facts": "id,from_id,source,source_kind,kind,target_id,target,target_kind,path,line,column_no,end_line,properties,owner_file",
		"edges": "id,fact_id,from_id,to_id,kind,path,line,column_no,end_line,properties",
	}[table]
	if columns == "" {
		t.Fatalf("unsupported table %q", table)
	}
	rows, err := repository.db.QueryContext(context.Background(), "SELECT "+columns+" FROM "+table+" ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columnNames, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result []string
	for rows.Next() {
		values := make([]any, len(columnNames))
		destinations := make([]any, len(columnNames))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			t.Fatal(err)
		}
		fields := make([]string, len(values))
		for index, value := range values {
			fields[index] = fmt.Sprintf("%T:%v", value, value)
		}
		result = append(result, strings.Join(fields, "\x1f"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
