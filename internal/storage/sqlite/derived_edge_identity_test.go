package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// storedEdgeIdentitySchemaVersion is the last storage version that kept an
// edge's identity in a column of its own.
const storedEdgeIdentitySchemaVersion = 12

// TestMigrationDerivesTheEdgeIdentityItUsedToStore is the proof the column was
// redundant. It writes edges through the old schema, where the identity is
// whatever the writer stored, migrates, and requires the read path to report the
// same identities from the three columns that remain.
//
// Both rules are exercised, because only their difference makes the key
// (fact_id, to_id, kind) rather than (fact_id, to_id): a structural test edge
// shares a fact and a target with the calls edge it was derived from, and the two
// must keep distinct identities across the migration.
func TestMigrationDerivesTheEdgeIdentityItUsedToStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "stored-identity.sqlite")

	callsID := graph.DerivedEdgeID("fact", "target", graph.EdgeCalls)
	testsID := graph.DerivedEdgeID("fact", "target", graph.EdgeTests)
	if callsID == testsID {
		t.Fatal("the two identity rules collide, so this test proves nothing")
	}

	openIndexAtVersion(t, path, storedEdgeIdentitySchemaVersion, `
INSERT INTO paths(id, path) VALUES (1, 'owner.go');
INSERT INTO nodes(id, kind, name, qualified_name, path, line, owner_file) VALUES
    ('caller', 'test', 'Caller', 'pkg.Caller', 'owner.go', 2, 'owner.go'),
    ('target', 'function', 'Target', 'pkg.Target', 'owner.go', 9, 'owner.go');
INSERT INTO facts(id, from_id, kind, producer, target_id, path_id, line, column_no, end_line, properties, owner_path_id)
    VALUES ('fact', 'caller', 'calls', 'go', 'target', 1, 5, 7, 5, '{}', 1);
INSERT INTO edges(id, fact_id, from_id, to_id, kind, properties) VALUES
    ('`+callsID+`', 'fact', 'caller', 'target', 'calls', '{}'),
    ('`+testsID+`', 'fact', 'caller', 'target', 'tests', '{}');`)

	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	edges, err := repository.EdgesFrom(ctx, "caller")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("migrated edges = %#v, want the 2 that were stored", edges)
	}
	byKind := make(map[graph.EdgeKind]graph.Edge, len(edges))
	for _, edge := range edges {
		byKind[edge.Kind] = edge
	}
	if got := byKind[graph.EdgeCalls].ID; got != callsID {
		t.Errorf("derived calls identity = %q, want the stored %q", got, callsID)
	}
	if got := byKind[graph.EdgeTests].ID; got != testsID {
		t.Errorf("derived tests identity = %q, want the stored %q", got, testsID)
	}
}

// TestEdgeIdentityIsNotStored guards the shape the migration established, since
// every saving it makes depends on the column and its index staying absent.
func TestEdgeIdentityIsNotStored(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "edge-shape.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	if columns := countRows(t, path,
		"SELECT COUNT(*) FROM pragma_table_info('edges') WHERE name = 'id'"); columns != 0 {
		t.Error("edges stores an id column again, so the identity is kept twice")
	}
	// The index has to be unique, not merely present: the identity is derived from
	// these three columns, so two rows sharing them would derive one identity.
	if unique := countRows(t, path,
		"SELECT COUNT(*) FROM pragma_index_list('edges') WHERE name = 'edges_identity' AND \"unique\" = 1"); unique != 1 {
		t.Error("edges_identity is missing or not unique")
	}
	// edges_identity replaced edges_fact rather than joining it, because fact_id
	// leads it. Both existing would pay for the same lookup twice.
	if replaced := countRows(t, path,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'edges_fact'"); replaced != 0 {
		t.Error("edges_fact is back alongside edges_identity, which indexes fact_id twice")
	}
}

// TestDerivedEdgeIdentityRejectsADuplicate covers the constraint the derivation
// relies on, through the writer rather than through raw SQL.
func TestDerivedEdgeIdentityRejectsADuplicate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "duplicate-identity.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	parsed := graph.ParseResult{
		Nodes: []graph.Node{
			{ID: "caller", Kind: graph.KindFunction, Name: "Caller", QualifiedName: "pkg.Caller", OwnerFile: "a.go"},
			{ID: "target", Kind: graph.KindFunction, Name: "Target", QualifiedName: "pkg.Target", OwnerFile: "a.go"},
		},
		Facts: []graph.Fact{{ID: "fact", FromID: "caller", Kind: graph.EdgeCalls, Producer: "go",
			TargetID: "target", Location: graph.Location{Path: "a.go", Line: 5}, OwnerFile: "a.go"}},
	}
	record := graph.FileRecord{Path: "a.go", Hash: "hash", Language: "go", Size: 1,
		ModifiedNS: 1, IndexedAt: graph.NowUTC()}
	if err := repository.ReplaceFile(ctx, record, parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// Reconciling again must not duplicate the edge it already resolved, which is
	// the property the unique identity index enforces now that no stored id does.
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, "caller")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges after a second reconcile = %#v, want exactly 1", edges)
	}
	if want := graph.DerivedEdgeID("fact", "target", graph.EdgeCalls); edges[0].ID != want {
		t.Errorf("edge identity = %q, want %q", edges[0].ID, want)
	}
}
