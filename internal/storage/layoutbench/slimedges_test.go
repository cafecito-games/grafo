package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/pressly/goose/v3"
)

// TestSlimEdgesEquivalenceMatchesProduction gates the slim resolved-adjacency
// candidate: facts stay canonical, edge rows carry identity only, and hydration
// joins the fact — the driver must prove that surface byte-identical to the
// production adapter across every probe, including the derived tests-edge
// evidence rendered by the edges-from-test-source probe and the adjacency
// sweeps.
func TestSlimEdgesEquivalenceMatchesProduction(t *testing.T) {
	ctx := context.Background()
	result, err := RunEquivalence(ctx, EquivalenceInput{
		Control: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return sqlite.Open(ctx, path)
		},
		Variant: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return OpenVariant(ctx, SlimEdgesSpec("slim-edges"), path)
		},
		Fixture:                  GenerateFixture(6, 60),
		BatchInterruptionFixture: GenerateFixture(6, 900),
		ControlDirectory:         t.TempDir(),
		VariantDirectory:         t.TempDir(),
	})
	if err != nil {
		t.Fatalf("RunEquivalence: %v", err)
	}
	if !result.Equivalent {
		t.Fatalf("slim-edges diverged from production: %s", result.FirstDifference)
	}
	if result.ControlDigest == "" || result.ControlDigest != result.VariantDigest {
		t.Fatalf("equivalence digests disagree: control %s variant %s",
			result.ControlDigest, result.VariantDigest)
	}
	if result.ControlProbes != result.VariantProbes || result.ControlProbes == 0 {
		t.Fatalf("probe counts disagree: control %d variant %d", result.ControlProbes, result.VariantProbes)
	}
}

// TestSlimEdgesDerivedTestEdgeEvidenceMatchesProduction is the dedicated
// derived-edge probe: a test source with an exact calls fact to a production
// symbol must hydrate a tests edge whose evidence — kind, producer, location,
// properties — renders byte-identically on both adapters, from both the source
// and the target side.
func TestSlimEdgesDerivedTestEdgeEvidenceMatchesProduction(t *testing.T) {
	ctx := context.Background()
	fixture := GenerateFixture(6, 4)
	moduleCount := fixturePopulationFileCount(fixture)
	shared, err := fileByPath(fixture, sharedOwnerPath(moduleCount+2))
	if err != nil {
		t.Fatalf("locate shared file: %v", err)
	}
	testFile, err := fileByPath(fixture, testOwnerPath(moduleCount+4))
	if err != nil {
		t.Fatalf("locate test file: %v", err)
	}

	render := func(opener RepositoryOpener) string {
		repository, err := opener(ctx, filepath.Join(t.TempDir(), "graph.db"))
		if err != nil {
			t.Fatalf("open adapter: %v", err)
		}
		defer func() { _ = repository.Close() }()
		if err := repository.SetMeta(ctx, "root", equivalenceRoot); err != nil {
			t.Fatalf("set root: %v", err)
		}
		for _, file := range []FixtureFile{shared, testFile} {
			if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
				t.Fatalf("replace %s: %v", file.Record.Path, err)
			}
		}
		if err := repository.Reconcile(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		testSource := longNodeID("test", 0)
		productionTarget := longNodeID("production", 0)
		from, err := repository.EdgesFrom(ctx, testSource)
		if err != nil {
			t.Fatalf("edges from test source: %v", err)
		}
		to, err := repository.EdgesTo(ctx, productionTarget)
		if err != nil {
			t.Fatalf("edges to production target: %v", err)
		}
		if !slices.ContainsFunc(from, func(edge graph.Edge) bool { return edge.Kind == graph.EdgeTests }) {
			t.Fatalf("no derived tests edge from the test source: %s", renderEdges(from))
		}
		if !slices.ContainsFunc(to, func(edge graph.Edge) bool { return edge.Kind == graph.EdgeTests }) {
			t.Fatalf("no derived tests edge into the production target: %s", renderEdges(to))
		}
		return renderEdges(from) + "\n" + renderEdges(to)
	}

	control := render(func(ctx context.Context, path string) (EquivalenceRepository, error) {
		return sqlite.Open(ctx, path)
	})
	variant := render(func(ctx context.Context, path string) (EquivalenceRepository, error) {
		return OpenVariant(ctx, SlimEdgesSpec("slim-edges"), path)
	})
	if control != variant {
		t.Fatalf("derived tests-edge evidence diverged:\nproduction: %s\nslim-edges: %s", control, variant)
	}
}

// openSlimEdgesPlanDatabase opens a temporary SQLite database migrated to the
// slim-edges candidate schema.
func openSlimEdgesPlanDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "slim-edges.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, slimEdgesMigrations)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate slim-edges database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// populateSlimEdgesPlanDatabase seeds the slim schema with enough nodes, facts,
// and edges that the query planner cannot treat any graph table as trivially
// small.
func populateSlimEdgesPlanDatabase(t *testing.T, db *sql.DB, rows int) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	nodeStatement, err := tx.PrepareContext(ctx, `INSERT INTO nodes(
		id, kind, name, qualified_name, language, path, line, column_no, end_line,
		properties, owner_file, external, name_folded, qualified_name_folded
	) VALUES (?, ?, ?, ?, 'go', 'src/fixture-000001.go', 10, 1, 10, '{}', 'src/fixture-000001.go', ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare node seed: %v", err)
	}
	factStatement, err := tx.PrepareContext(ctx, `INSERT INTO facts(
		id, from_id, source, source_kind, kind, producer, target_id, target, target_kind,
		path, line, column_no, end_line, properties, owner_file
	) VALUES (?, ?, ?, 'function', ?, 'parser', ?, ?, 'function', 'src/fixture-000001.go', 10, 1, 10, '{}', 'src/fixture-000001.go')`)
	if err != nil {
		t.Fatalf("prepare fact seed: %v", err)
	}
	edgeStatement, err := tx.PrepareContext(ctx,
		`INSERT INTO edges(id, fact_id, from_id, to_id, kind) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare edge seed: %v", err)
	}
	evidenceStatement, err := tx.PrepareContext(ctx, `INSERT INTO derived_edge_evidence(
		edge_id, producer, path, line, column_no, end_line, properties
	) VALUES (?, 'parser', 'src/fixture-000001.go', 11, 1, 11, '{"coverage":"structural"}')`)
	if err != nil {
		t.Fatalf("prepare evidence seed: %v", err)
	}
	nodeKinds := []string{"function", "method", "type", "class", "interface", "endpoint", "variable", "test"}
	edgeKinds := []string{"calls", "references", "defines", "tests"}
	for index := range rows {
		name := fmt.Sprintf("Function%06d", index)
		qualifiedName := "pkg." + name
		nodeID := fmt.Sprintf("n:fixture-%06d", index)
		counterpartID := fmt.Sprintf("n:fixture-%06d", (index+1)%rows)
		external := 0
		if index%10 == 0 {
			external = 1
		}
		if _, err := nodeStatement.Exec(nodeID, nodeKinds[index%len(nodeKinds)], name, qualifiedName,
			external, name, qualifiedName); err != nil {
			t.Fatalf("seed node %d: %v", index, err)
		}
		factID := fmt.Sprintf("f:fixture-%06d", index)
		if _, err := factStatement.Exec(factID, nodeID, qualifiedName, edgeKinds[index%len(edgeKinds)],
			counterpartID, qualifiedName); err != nil {
			t.Fatalf("seed fact %d: %v", index, err)
		}
		edgeID := fmt.Sprintf("e:fixture-%06d", index)
		if _, err := edgeStatement.Exec(edgeID, factID, nodeID, counterpartID,
			edgeKinds[index%len(edgeKinds)]); err != nil {
			t.Fatalf("seed edge %d: %v", index, err)
		}
		if index%16 == 0 {
			if _, err := evidenceStatement.Exec(edgeID); err != nil {
				t.Fatalf("seed evidence %d: %v", index, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}
}

// TestSlimEdgesPlanValidity proves the candidate's overridden statements and
// the untouched EnqueueDirtyFacts resolve against the slim schema without an
// uncovered scan: the edges-existence subquery still resolves through covered
// columns and the hydration joins stay index-backed.
func TestSlimEdgesPlanValidity(t *testing.T) {
	ctx := context.Background()
	db := openSlimEdgesPlanDatabase(t)
	populateSlimEdgesPlanDatabase(t, db, 500)

	spec := SlimEdgesSpec("slim-edges-plan")
	queries, unmatched := PlanQueriesFor(spec.SQL)
	candidateOnly := map[string]bool{
		slimEdgesEvidenceInsertStatement:                 true,
		slimEdgesEvidenceDeleteByDirtyFactBatchStatement: true,
		slimEdgesEvidenceDeleteByOwnerFactsStatement:     true,
	}
	if len(unmatched) != len(candidateOnly) {
		t.Fatalf("unmatched override names %v must be exactly the candidate-only statements %v",
			unmatched, candidateOnly)
	}
	for _, name := range unmatched {
		if !candidateOnly[name] {
			t.Fatalf("override %s matched no production query", name)
		}
	}
	for index, query := range queries {
		if query.Name != edgeStatement {
			continue
		}
		// The production inventory binds the production InsertEdge's eleven
		// values; the slim override binds five.
		queries[index].Params = []any{"e:fixture-000001", planFactID, planNodeID, planCounterpart, planRelation}
	}
	queries = append(queries,
		PlanQuery{Name: slimEdgesEvidenceInsertStatement, SQL: spec.SQL[slimEdgesEvidenceInsertStatement],
			Params: []any{"e:fixture-000001", "parser", planOwner, 10, 1, 10, "{}"}},
		PlanQuery{Name: slimEdgesEvidenceDeleteByDirtyFactBatchStatement,
			SQL:    spec.SQL[slimEdgesEvidenceDeleteByDirtyFactBatchStatement],
			Params: []any{planLimit}},
		PlanQuery{Name: slimEdgesEvidenceDeleteByOwnerFactsStatement,
			SQL:    spec.SQL[slimEdgesEvidenceDeleteByOwnerFactsStatement],
			Params: []any{planOwner}},
	)

	captures, err := CapturePlans(ctx, db, queries)
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	tracked := map[string]bool{
		"EnqueueDirtyFacts":              false,
		"InsertEdge":                     false,
		"ListEdgesFrom":                  false,
		"ListEdgesTo":                    false,
		"ListIncomingRelationEdges":      false,
		"ListOutgoingRelationEdges":      false,
		"ListExternalEdgesMatching":      false,
		slimEdgesEvidenceInsertStatement: false,
	}
	for _, capture := range ValidatePlans(captures) {
		if !capture.Valid {
			var details []string
			for _, step := range capture.Steps {
				details = append(details, step.Detail)
			}
			t.Errorf("%s invalid: %s (steps: %s)", capture.Query.Name, capture.Reason,
				strings.Join(details, " | "))
			continue
		}
		if _, watched := tracked[capture.Query.Name]; watched {
			tracked[capture.Query.Name] = true
		}
		if capture.Query.Name != "EnqueueDirtyFacts" {
			continue
		}
		for _, step := range capture.Steps {
			if strings.HasPrefix(step.Detail, "SCAN edges") && isUncoveredScan(step.Detail) {
				t.Errorf("EnqueueDirtyFacts edges-existence subquery scans edges without an index: %s", step.Detail)
			}
		}
	}
	for name, seen := range tracked {
		if !seen {
			t.Errorf("plan capture missing %s", name)
		}
	}
}

// attributionIndexRepository is the indexing surface the attribution
// comparison drives both adapters through.
type attributionIndexRepository interface {
	ReplaceFile(context.Context, graph.FileRecord, graph.ParseResult) error
	Reconcile(context.Context) error
	Counts(context.Context) (graph.Counts, error)
	Close() error
}

func indexFixtureForAttribution(t *testing.T, repository attributionIndexRepository, fixture Fixture) graph.Counts {
	t.Helper()
	ctx := context.Background()
	for _, file := range fixture.Files {
		if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
			t.Fatalf("replace %s: %v", file.Record.Path, err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return counts
}

// measureEdgesObjectBytes checkpoints the database at path and returns the
// dbstat bytes of the edges table together with every edges index, including
// the implicit primary-key autoindex.
func measureEdgesObjectBytes(t *testing.T, path string) int64 {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := Checkpoint(ctx, db, true); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	attribution, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("CaptureAttribution: %v", err)
	}
	if !attribution.DBStatSupported {
		t.Fatalf("dbstat unsupported: %s", attribution.DBStatReason)
	}
	required := []string{"edges", "edges_from", "edges_to", "edges_fact"}
	seen := map[string]bool{}
	var total int64
	for _, object := range attribution.Objects {
		switch {
		case slices.Contains(required, object.Name):
			seen[object.Name] = true
			total += object.Bytes
		case strings.HasPrefix(object.Name, "sqlite_autoindex_edges"):
			total += object.Bytes
		}
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("dbstat has no %s object: %v", name, objectNames(attribution))
		}
	}
	return total
}

// TestSlimEdgesAttributionShrinksEdges is the structural size check: with
// identical reconciled data, the slim edges table and its indexes must occupy
// fewer dbstat bytes than the production ones.
func TestSlimEdgesAttributionShrinksEdges(t *testing.T) {
	ctx := context.Background()
	fixture := GenerateFixture(2, 40)

	productionPath := filepath.Join(t.TempDir(), "graph.db")
	production, err := sqlite.Open(ctx, productionPath)
	if err != nil {
		t.Fatalf("open production adapter: %v", err)
	}
	productionCounts := indexFixtureForAttribution(t, production, fixture)

	slimPath := filepath.Join(t.TempDir(), "graph.db")
	slim, err := OpenVariant(ctx, SlimEdgesSpec("slim-edges"), slimPath)
	if err != nil {
		t.Fatalf("open slim-edges adapter: %v", err)
	}
	slimCounts := indexFixtureForAttribution(t, slim, fixture)

	if productionCounts.Edges == 0 {
		t.Fatal("fixture produced no edges")
	}
	if productionCounts.Edges != slimCounts.Edges {
		t.Fatalf("adapters hold different edge counts: production %d slim %d",
			productionCounts.Edges, slimCounts.Edges)
	}

	productionBytes := measureEdgesObjectBytes(t, productionPath)
	slimBytes := measureEdgesObjectBytes(t, slimPath)
	if slimBytes >= productionBytes {
		t.Fatalf("slim edges objects (%d bytes) did not shrink below production (%d bytes) for %d edges",
			slimBytes, productionBytes, productionCounts.Edges)
	}
}
