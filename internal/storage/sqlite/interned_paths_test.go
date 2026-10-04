package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"github.com/pressly/goose/v3"
)

// denormalizedSchemaVersion is the last storage version that stored a fact's
// path twice and repeated its producer and location on every edge.
const denormalizedSchemaVersion = 9

func openDenormalizedIndex(t *testing.T, path string, statements string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(context.Background(), denormalizedSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(), statements); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, path, query string) int64 {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var count int64
	if err := database.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMigrationInternsPathsAndDerivesEdgeEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "denormalized.sqlite")
	openDenormalizedIndex(t, path, `
INSERT INTO nodes(id, kind, name, qualified_name, path, line, owner_file) VALUES
    ('caller', 'function', 'Caller', 'legacy.Caller', 'legacy.go', 2, 'legacy.go'),
    ('target', 'function', 'Target', 'legacy.Target', 'other.go', 9, 'other.go');
INSERT INTO facts(id, from_id, kind, producer, target_id, path, line, column_no, end_line, properties, owner_file)
    VALUES ('legacy-fact', 'caller', 'calls', 'go', 'target', 'legacy.go', 5, 7, 5, '{"proof":"direct"}', 'legacy.go');
INSERT INTO edges(id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties)
    VALUES ('legacy-edge', 'legacy-fact', 'caller', 'target', 'calls', 'go', 'legacy.go', 5, 7, 5, '{"proof":"direct"}');`)

	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, "caller")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("migrated edges = %#v", edges)
	}
	migrated := edges[0]
	wantID := graph.DerivedEdgeID("legacy-fact", "target", graph.EdgeCalls)
	if migrated.ID != wantID || migrated.FactID != "legacy-fact" || migrated.ToID != "target" ||
		migrated.Kind != graph.EdgeCalls || migrated.Producer != "go" ||
		migrated.Location != (graph.Location{Path: "legacy.go", Line: 5, Column: 7, EndLine: 5}) ||
		migrated.Properties["proof"] != "direct" {
		t.Fatalf("migration lost edge evidence: %#v", migrated)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	// The one distinct fact path value is interned once, and the edge no longer
	// carries a copy of its fact's producer or location.
	if interned := countRows(t, path, "SELECT COUNT(*) FROM paths"); interned != 1 {
		t.Fatalf("interned paths = %d, want 1", interned)
	}
	for _, column := range []string{"path", "line", "column_no", "end_line", "producer"} {
		remaining := countRows(t, path,
			"SELECT COUNT(*) FROM pragma_table_info('edges') WHERE name = '"+column+"'")
		if remaining != 0 {
			t.Fatalf("edges still store derivable column %q", column)
		}
	}
}

func TestMigrationFailsClosedOnEdgeEvidenceThatCannotBeDerived(t *testing.T) {
	for name, edgeRow := range map[string]string{
		"missing fact": `INSERT INTO edges(id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties)
    VALUES ('orphan-edge', 'absent-fact', 'caller', 'target', 'calls', 'go', 'legacy.go', 5, 7, 5, '{}')`,
		"divergent location": `INSERT INTO edges(id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties)
    VALUES ('divergent-edge', 'legacy-fact', 'caller', 'target', 'calls', 'go', 'generated.go', 5, 7, 5, '{}')`,
		"divergent producer": `INSERT INTO edges(id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties)
    VALUES ('divergent-edge', 'legacy-fact', 'caller', 'target', 'calls', 'gdscript', 'legacy.go', 5, 7, 5, '{}')`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(testtemp.Dir(t), "divergent.sqlite")
			openDenormalizedIndex(t, path, `
INSERT INTO facts(id, from_id, kind, producer, target_id, path, line, column_no, end_line, properties, owner_file)
    VALUES ('legacy-fact', 'caller', 'calls', 'go', 'target', 'legacy.go', 5, 7, 5, '{}', 'legacy.go');
`+edgeRow+";")
			repository, err := sqlite.Open(context.Background(), path)
			if err == nil {
				_ = repository.Close()
				t.Fatal("migration accepted edge evidence it cannot derive")
			}
			if !strings.Contains(err.Error(), "migrate graph database") {
				t.Fatalf("unexpected migration failure: %v", err)
			}
			// A refused migration must leave nothing behind: neither a guard
			// table nor a half-rebuilt facts or edges table.
			for _, table := range []string{"paths", "edge_missing_fact_guard",
				"edge_fact_divergence_guard", "facts_interned", "edges_derived"} {
				if countRows(t, path,
					"SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = '"+table+"'") != 0 {
					t.Fatalf("refused migration left table %q behind", table)
				}
			}
			for _, column := range []string{"path", "line", "column_no", "end_line", "producer"} {
				if countRows(t, path,
					"SELECT COUNT(*) FROM pragma_table_info('edges') WHERE name = '"+column+"'") != 1 {
					t.Fatalf("refused migration lost the legacy edges column %q", column)
				}
			}
			if countRows(t, path,
				"SELECT COUNT(*) FROM pragma_table_info('facts') WHERE name = 'owner_file'") != 1 {
				t.Fatal("refused migration lost the legacy facts.owner_file column")
			}
		})
	}
}

func TestEnqueueFailsClosedOnAFactWithoutAnInternedOwnerPath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	parsed := graph.ParseResult{
		Nodes: []graph.Node{{ID: "node", Kind: graph.KindFunction, Name: "Fn", QualifiedName: "pkg.Fn",
			Location: graph.Location{Path: "owned.go", Line: 1}, OwnerFile: "owned.go"}},
		Facts: []graph.Fact{{ID: "fact", FromID: "node", Kind: graph.EdgeCalls, Producer: "go",
			TargetID: "node", Location: graph.Location{Path: "owned.go", Line: 2}, OwnerFile: "owned.go"}},
	}
	record := graph.FileRecord{Path: "owned.go", Hash: "hash", Language: "go", Size: 1,
		ModifiedNS: 1, IndexedAt: graph.NowUTC()}
	if err := repository.ReplaceFile(ctx, record, parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	// Point the fact at an owner path row that does not exist, then make the
	// fact dirty through its target node so enqueueing has to reach it without
	// resolving its owner by path.
	if _, err := database.ExecContext(ctx,
		"UPDATE facts SET owner_path_id = 9999 WHERE id = grafo_identity_blob('fact')"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		"INSERT OR IGNORE INTO dirty_nodes(node_id) VALUES (grafo_identity_blob('node'))"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err == nil {
		t.Fatal("reconciliation accepted a fact whose interned owner path is missing")
	}
}

func TestInternedPathsKeepALocationThatDiffersFromItsOwnerFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	// The indexer owns its workspace facts under a synthetic owner key while
	// their location points at the project configuration file. Interning must
	// keep both values instead of collapsing them.
	parsed := graph.ParseResult{
		Nodes: []graph.Node{
			{ID: "repository", Kind: graph.KindRepository, Name: "fixture", QualifiedName: "fixture",
				OwnerFile: "__workspace__"},
			{ID: "component", Kind: graph.KindComponent, Name: "core", QualifiedName: "fixture/core",
				OwnerFile: "__workspace__"},
		},
		Facts: []graph.Fact{{ID: "workspace-fact", FromID: "repository", Kind: graph.EdgeContains,
			Producer: graph.ProducerIndexer, TargetID: "component",
			Location:  graph.Location{Path: "grafo.yaml", Line: 4, Column: 3},
			OwnerFile: "__workspace__"}},
	}
	if err := repository.ReplaceOwner(ctx, "__workspace__", parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, "repository")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Location.Path != "grafo.yaml" || edges[0].Location.Line != 4 {
		t.Fatalf("workspace edge lost its configuration location: %#v", edges)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var location, owner string
	if err := database.QueryRowContext(ctx, `
SELECT location_paths.path, owner_paths.path
FROM facts
JOIN paths AS location_paths ON location_paths.id = facts.path_id
JOIN paths AS owner_paths ON owner_paths.id = facts.owner_path_id
WHERE facts.id = grafo_identity_blob('workspace-fact')`).Scan(&location, &owner); err != nil {
		t.Fatal(err)
	}
	if location != "grafo.yaml" || owner != "__workspace__" {
		t.Fatalf("interned fact location = %q, owner = %q", location, owner)
	}
}

func TestRemoveFilesPrunesInternedPaths(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	for _, owner := range []string{"kept.go", "removed.go"} {
		parsed := graph.ParseResult{
			Nodes: []graph.Node{{ID: owner + ":node", Kind: graph.KindFunction, Name: "Fn",
				QualifiedName: owner + ".Fn", Location: graph.Location{Path: owner, Line: 1},
				OwnerFile: owner}},
			Facts: []graph.Fact{{ID: owner + ":fact", FromID: owner + ":node", Kind: graph.EdgeCalls,
				Producer: "go", TargetID: owner + ":node",
				Location: graph.Location{Path: owner, Line: 2}, OwnerFile: owner}},
		}
		record := graph.FileRecord{Path: owner, Hash: "hash-" + owner, Language: "go", Size: 1,
			ModifiedNS: 1, IndexedAt: graph.NowUTC()}
		if err := repository.ReplaceFile(ctx, record, parsed); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if interned := countRows(t, path, "SELECT COUNT(*) FROM paths"); interned != 2 {
		t.Fatalf("interned paths before removal = %d, want 2", interned)
	}

	if err := repository.RemoveFiles(ctx, []string{"removed.go"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int64{
		"SELECT COUNT(*) FROM paths":                                                   1,
		"SELECT COUNT(*) FROM paths WHERE path = 'kept.go'":                            1,
		"SELECT COUNT(*) FROM facts":                                                   1,
		"SELECT COUNT(*) FROM edges":                                                   1,
		"SELECT COUNT(*) FROM facts WHERE id = 'removed.go:fact'":                      0,
		"SELECT COUNT(*) FROM edges WHERE fact_id = 'removed.go:fact'":                 0,
		"SELECT COUNT(*) FROM facts WHERE path_id NOT IN (SELECT id FROM paths)":       0,
		"SELECT COUNT(*) FROM facts WHERE owner_path_id NOT IN (SELECT id FROM paths)": 0,
	} {
		if got := countRows(t, path, query); got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}

	// A removed path is interned again on re-index, and the stale key the write
	// cache held before pruning must not be reused.
	reinstated := graph.ParseResult{
		Nodes: []graph.Node{{ID: "removed.go:node", Kind: graph.KindFunction, Name: "Fn",
			QualifiedName: "removed.go.Fn", Location: graph.Location{Path: "removed.go", Line: 1},
			OwnerFile: "removed.go"}},
		Facts: []graph.Fact{{ID: "removed.go:fact", FromID: "removed.go:node", Kind: graph.EdgeCalls,
			Producer: "go", TargetID: "removed.go:node",
			Location: graph.Location{Path: "removed.go", Line: 2}, OwnerFile: "removed.go"}},
	}
	record := graph.FileRecord{Path: "removed.go", Hash: "hash-2", Language: "go", Size: 1,
		ModifiedNS: 2, IndexedAt: graph.NowUTC()}
	if err := repository.ReplaceFile(ctx, record, reinstated); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, "removed.go:node")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Location.Path != "removed.go" || edges[0].Location.Line != 2 {
		t.Fatalf("re-indexed edge location = %#v", edges)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if orphaned := countRows(t, path,
		"SELECT COUNT(*) FROM facts WHERE path_id NOT IN (SELECT id FROM paths)"); orphaned != 0 {
		t.Fatalf("facts referencing a pruned path row = %d", orphaned)
	}
}

func TestReconciliationSweepsPathsLeftUnreferencedByAnOwnerReplacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	owned := graph.Node{ID: "node", Kind: graph.KindFunction, Name: "Fn", QualifiedName: "pkg.Fn",
		Location: graph.Location{Path: "a.go", Line: 1}, OwnerFile: "a.go"}
	withGenerated := graph.ParseResult{Nodes: []graph.Node{owned}, Facts: []graph.Fact{
		{ID: "local", FromID: "node", Kind: graph.EdgeCalls, Producer: "go", TargetID: "node",
			Location: graph.Location{Path: "a.go", Line: 2}, OwnerFile: "a.go"},
		{ID: "generated", FromID: "node", Kind: graph.EdgeGeneratedFrom, Producer: "go", TargetID: "node",
			Location: graph.Location{Path: "generated.go", Line: 9}, OwnerFile: "a.go"},
	}}
	if err := repository.ReplaceOwner(ctx, "a.go", withGenerated); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if interned := countRows(t, path, "SELECT COUNT(*) FROM paths WHERE path = 'generated.go'"); interned != 1 {
		t.Fatalf("interned generated.go rows = %d, want 1", interned)
	}

	withoutGenerated := graph.ParseResult{Nodes: []graph.Node{owned}, Facts: withGenerated.Facts[:1]}
	if err := repository.ReplaceOwner(ctx, "a.go", withoutGenerated); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if interned := countRows(t, path, "SELECT COUNT(*) FROM paths WHERE path = 'generated.go'"); interned != 0 {
		t.Fatal("an owner replacement stranded an unreferenced interned path row")
	}
	if interned := countRows(t, path, "SELECT COUNT(*) FROM paths WHERE path = 'a.go'"); interned != 1 {
		t.Fatal("the sweep removed a referenced interned path row")
	}
	edges, err := repository.EdgesFrom(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Location.Path != "a.go" {
		t.Fatalf("surviving edges = %#v", edges)
	}
}

// Rebuilding a table drops every index an earlier migration created on it.
// This locks the index set both rebuilt tables must still carry afterwards, so
// a later rebuild cannot silently remove one a query plan depends on.
func TestRebuiltTablesKeepEveryIndexEarlierMigrationsCreated(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string][]string{
		"edges": {"edges_identity", "edges_from", "edges_kind", "edges_to"},
		"facts": {"facts_from_id", "facts_owner", "facts_path", "facts_source",
			"facts_target", "facts_target_id"},
	} {
		for _, index := range want {
			if countRows(t, path, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index'"+
				" AND tbl_name = '"+table+"' AND name = '"+index+"'") != 1 {
				t.Fatalf("table %q is missing index %q", table, index)
			}
		}
	}
}

func TestMigrationDownRestoresTheDenormalizedShapeWithItsEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := graph.ParseResult{
		Nodes: []graph.Node{{ID: "node", Kind: graph.KindFunction, Name: "Fn", QualifiedName: "pkg.Fn",
			Location: graph.Location{Path: "a.go", Line: 1}, OwnerFile: "a.go"}},
		Facts: []graph.Fact{{ID: "fact", FromID: "node", Kind: graph.EdgeCalls, Producer: "go",
			TargetID: "node", Location: graph.Location{Path: "a.go", Line: 5, Column: 7, EndLine: 5},
			Properties: map[string]string{"proof": "direct"}, OwnerFile: "a.go"}},
	}
	record := graph.FileRecord{Path: "a.go", Hash: "hash", Language: "go", Size: 1,
		ModifiedNS: 1, IndexedAt: graph.NowUTC()}
	if err := repository.ReplaceFile(ctx, record, parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, denormalizedSchemaVersion); err != nil {
		t.Fatal(err)
	}
	var factPath, factOwner, edgePath, edgeProducer string
	var edgeLine, edgeColumn, edgeEndLine int64
	if err := database.QueryRowContext(ctx,
		"SELECT path, owner_file FROM facts WHERE id = 'fact'").Scan(&factPath, &factOwner); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx,
		"SELECT path, producer, line, column_no, end_line FROM edges WHERE fact_id = 'fact'").
		Scan(&edgePath, &edgeProducer, &edgeLine, &edgeColumn, &edgeEndLine); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if factPath != "a.go" || factOwner != "a.go" {
		t.Fatalf("reverted fact path = %q, owner = %q", factPath, factOwner)
	}
	if edgePath != "a.go" || edgeProducer != "go" || edgeLine != 5 || edgeColumn != 7 || edgeEndLine != 5 {
		t.Fatalf("reverted edge evidence = %q/%q/%d/%d/%d",
			edgePath, edgeProducer, edgeLine, edgeColumn, edgeEndLine)
	}
	if countRows(t, path, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'paths'") != 0 {
		t.Fatal("reverting left the paths table behind")
	}
	for table, want := range map[string][]string{
		"edges": {"edges_fact", "edges_from", "edges_kind", "edges_to"},
		"facts": {"facts_from_id", "facts_owner", "facts_source", "facts_target", "facts_target_id"},
	} {
		for _, index := range want {
			if countRows(t, path, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index'"+
				" AND tbl_name = '"+table+"' AND name = '"+index+"'") != 1 {
				t.Fatalf("reverted table %q is missing index %q", table, index)
			}
		}
	}
}
