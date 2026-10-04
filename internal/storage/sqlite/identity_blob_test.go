package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// textIdentitySchemaVersion is the last storage version that stored identities
// as text rather than as the blob encoding.
const textIdentitySchemaVersion = 13

// TestMigrationPreservesEveryIdentityItReEncodes reads the graph back through the
// repository after the rebuild, which is the only check that covers the encoding
// and the decoding together. A transform that corrupted both consistently would
// satisfy a comparison made in SQL.
func TestMigrationPreservesEveryIdentityItReEncodes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "text-identities.sqlite")

	caller := graph.NodeID(graph.KindFunction, "pkg.Caller")
	target := graph.NodeID(graph.KindFunction, "pkg.Target")
	factID := graph.FactID("owner.go", caller, graph.EdgeCalls, "pkg.Target", 5, 0)

	openIndexAtVersion(t, path, textIdentitySchemaVersion, `
INSERT INTO paths(id, path) VALUES (1, 'owner.go');
INSERT INTO nodes(id, kind, name, qualified_name, path, line, owner_file) VALUES
    ('`+caller+`', 'function', 'Caller', 'pkg.Caller', 'owner.go', 2, 'owner.go'),
    ('`+target+`', 'function', 'Target', 'pkg.Target', 'owner.go', 9, 'owner.go');
INSERT INTO facts(id, from_id, kind, producer, target_id, path_id, line, column_no, end_line, properties, owner_path_id)
    VALUES ('`+factID+`', '`+caller+`', 'calls', 'go', '`+target+`', 1, 5, 7, 5, '{}', 1);
INSERT INTO edges(fact_id, from_id, to_id, kind, properties) VALUES
    ('`+factID+`', '`+caller+`', '`+target+`', 'calls', '{}');`)

	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	node, err := repository.Node(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != caller {
		t.Errorf("node identity after the rebuild = %q, want %q", node.ID, caller)
	}

	edges, err := repository.EdgesFrom(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges after the rebuild = %#v, want 1", edges)
	}
	if edges[0].FactID != factID || edges[0].FromID != caller || edges[0].ToID != target {
		t.Errorf("edge identities after the rebuild = %#v", edges[0])
	}
	// The adjacency lookup itself proves the encoding is self-consistent: the
	// query bound the caller identity encoded and matched the stored column.
	if incoming, err := repository.EdgesTo(ctx, target); err != nil {
		t.Fatal(err)
	} else if len(incoming) != 1 {
		t.Errorf("incoming edges after the rebuild = %#v, want 1", incoming)
	}
}

// TestMigrationKeepsTheEmptyTargetSentinel covers the value the encoding is most
// likely to lose. A fact with no target stores the empty identity, and the SQL
// function the migration uses returns NULL for it unless that is guarded, which
// would both violate the column and erase the distinction reconciliation reads.
func TestMigrationKeepsTheEmptyTargetSentinel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "empty-target.sqlite")

	caller := graph.NodeID(graph.KindFunction, "pkg.Caller")
	factID := graph.FactID("owner.go", caller, graph.EdgeCalls, "pkg.Unresolved", 5, 0)

	openIndexAtVersion(t, path, textIdentitySchemaVersion, `
INSERT INTO paths(id, path) VALUES (1, 'owner.go');
INSERT INTO nodes(id, kind, name, qualified_name, path, line, owner_file) VALUES
    ('`+caller+`', 'function', 'Caller', 'pkg.Caller', 'owner.go', 2, 'owner.go');
INSERT INTO facts(id, from_id, kind, producer, target_id, target, path_id, line, column_no, end_line, properties, owner_path_id)
    VALUES ('`+factID+`', '`+caller+`', 'calls', 'go', '', 'pkg.Unresolved', 1, 5, 7, 5, '{}', 1);`)

	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	if empty := countRows(t, path,
		"SELECT COUNT(*) FROM facts WHERE target_id = x'' AND typeof(target_id) = 'blob'"); empty != 1 {
		t.Error("the empty target identity did not survive as a zero-length blob")
	}
	if nulls := countRows(t, path, "SELECT COUNT(*) FROM facts WHERE target_id IS NULL"); nulls != 0 {
		t.Error("the empty target identity became NULL")
	}
}

// TestReconciliationQueuesMatchStoredIdentities guards a failure that reports no
// error. The queues hold identities and are joined against facts.from_id and
// facts.id; SQLite compares storage classes before contents, so a queue holding
// text would match no encoded identity, and reconciliation would resolve nothing
// and succeed. Only the resulting absence of edges reveals it.
func TestReconciliationQueuesMatchStoredIdentities(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "queue-identities.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	caller := graph.NodeID(graph.KindFunction, "pkg.Caller")
	target := graph.NodeID(graph.KindFunction, "pkg.Target")
	parsed := graph.ParseResult{
		Nodes: []graph.Node{
			{ID: caller, Kind: graph.KindFunction, Name: "Caller", QualifiedName: "pkg.Caller", OwnerFile: "a.go"},
			{ID: target, Kind: graph.KindFunction, Name: "Target", QualifiedName: "pkg.Target", OwnerFile: "a.go"},
		},
		Facts: []graph.Fact{{
			ID: graph.FactID("a.go", caller, graph.EdgeCalls, "pkg.Target", 5, 0), FromID: caller,
			Kind: graph.EdgeCalls, Producer: "go", Target: "pkg.Target", TargetKind: graph.KindFunction,
			Location: graph.Location{Path: "a.go", Line: 5}, OwnerFile: "a.go"}},
	}
	record := graph.FileRecord{Path: "a.go", Hash: "hash", Language: "go", Size: 1,
		ModifiedNS: 1, IndexedAt: graph.NowUTC()}
	if err := repository.ReplaceFile(ctx, record, parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// The fact resolves by name, so reconciliation had to find the target through
	// the queues and the node lookups, all of which compare encoded identities.
	edges, err := repository.EdgesFrom(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("reconciliation produced %d edges, want 1; a queue that stored a "+
			"different storage class would match nothing and report no error", len(edges))
	}
	if edges[0].ToID != target {
		t.Errorf("resolved edge points at %q, want %q", edges[0].ToID, target)
	}

	// Replacing the owner marks its nodes dirty through the same queues, so a
	// second pass has to reach the fact again rather than silently skip it.
	if err := repository.ReplaceFile(ctx, record, parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if edges, err = repository.EdgesFrom(ctx, caller); err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("after an owner replacement there are %d edges, want 1", len(edges))
	}
}
