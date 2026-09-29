package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/pressly/goose/v3"
)

// TestIntegerKeysEquivalenceMatchesProduction gates the compact integer
// internal keys candidate: public ids stay unchanged at every repository
// boundary while adjacency and the node-id secondary indexes seek integer
// surrogates maintained at write time — the driver must prove that surface
// byte-identical to the production adapter across every probe, including the
// incremental removal, restore, and branch-switch scenarios that re-key nodes
// under foreign facts, plus batch interruption and resume.
func TestIntegerKeysEquivalenceMatchesProduction(t *testing.T) {
	ctx := context.Background()
	result, err := RunEquivalence(ctx, EquivalenceInput{
		Control: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return sqlite.Open(ctx, path)
		},
		Variant: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return OpenVariant(ctx, IntegerKeysSpec("integer-keys"), path)
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
		t.Fatalf("integer-keys diverged from production: %s", result.FirstDifference)
	}
	if result.ControlDigest == "" || result.ControlDigest != result.VariantDigest {
		t.Fatalf("equivalence digests disagree: control %s variant %s",
			result.ControlDigest, result.VariantDigest)
	}
	if result.ControlProbes != result.VariantProbes || result.ControlProbes == 0 {
		t.Fatalf("probe counts disagree: control %d variant %d", result.ControlProbes, result.VariantProbes)
	}
}

// openIntegerKeysPlanDatabase opens a temporary SQLite database migrated to
// the integer-keys candidate schema.
func openIntegerKeysPlanDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "integer-keys.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, integerKeysMigrations)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate integer-keys database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// populateIntegerKeysPlanDatabase seeds the integer-keys schema with enough
// nodes, facts, and edges that the query planner cannot treat any graph table
// as trivially small. The write-time triggers resolve the fact surrogate
// columns and the edge seed resolves its surrogates through subselects, so
// the seeded key distribution matches what the adapter produces.
func populateIntegerKeysPlanDatabase(t *testing.T, db *sql.DB, rows int) {
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
	edgeStatement, err := tx.PrepareContext(ctx, `INSERT INTO edges(
		id, fact_id, from_id, to_id, kind, from_key, to_key
	) VALUES (?, ?, ?, ?, ?,
		COALESCE((SELECT node_key FROM nodes WHERE nodes.id = ?), 0),
		COALESCE((SELECT node_key FROM nodes WHERE nodes.id = ?), 0))`)
	if err != nil {
		t.Fatalf("prepare edge seed: %v", err)
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
			edgeKinds[index%len(edgeKinds)], nodeID, counterpartID); err != nil {
			t.Fatalf("seed edge %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}
}

// TestIntegerKeysPlanValidity proves the candidate's overridden statements
// resolve against the integer-keys schema without an uncovered scan: the
// adjacency statements seek the integer indexes, the surrogate resolution is
// a unique-key lookup, and the dirty-fact enqueue arms — whose from and
// target sides now match surrogate-or-unresolved keys — stay covered.
func TestIntegerKeysPlanValidity(t *testing.T) {
	ctx := context.Background()
	db := openIntegerKeysPlanDatabase(t)
	populateIntegerKeysPlanDatabase(t, db, 500)

	spec := IntegerKeysSpec("integer-keys-plan")
	queries, unmatched := PlanQueriesFor(spec.SQL)
	if len(unmatched) != 1 || unmatched[0] != ResolveNodeKeyStatementName {
		t.Fatalf("unmatched override names %v must be exactly [%s]", unmatched, ResolveNodeKeyStatementName)
	}
	for index, query := range queries {
		switch query.Name {
		case edgeStatement:
			// The production inventory binds the production InsertEdge's
			// eleven values; the integer-keys override appends the two
			// textual endpoints the subselects resolve.
			queries[index].Params = []any{"e:fixture-000001", planFactID, planNodeID, planCounterpart,
				planRelation, "parser", planOwner, 10, 1, 10, "{}", planNodeID, planCounterpart}
		case "ListEdgesFrom", "ListEdgesTo":
			queries[index].Params = []any{int64(1)}
		case "ListIncomingRelationEdges", "ListOutgoingRelationEdges":
			queries[index].Params = []any{int64(1), planRelation, planLimit}
		}
	}
	queries = append(queries,
		PlanQuery{Name: ResolveNodeKeyStatementName, SQL: spec.SQL[ResolveNodeKeyStatementName],
			Params: []any{planNodeID}, HighCardinality: true},
	)

	captures, err := CapturePlans(ctx, db, queries)
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	tracked := map[string]bool{
		"EnqueueDirtyFacts":         false,
		"InsertEdge":                false,
		"ListEdgesFrom":             false,
		"ListEdgesTo":               false,
		"ListIncomingRelationEdges": false,
		"ListOutgoingRelationEdges": false,
		"ListExternalEdgesMatching": false,
		"DeleteOrphanExternalNodes": false,
		ResolveNodeKeyStatementName: false,
	}
	integerSeeks := map[string][]string{
		"ListEdgesFrom":             {"edges_from"},
		"ListEdgesTo":               {"edges_to"},
		"ListIncomingRelationEdges": {"edges_to"},
		"ListOutgoingRelationEdges": {"edges_from"},
		ResolveNodeKeyStatementName: {"sqlite_autoindex_nodes_1"},
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
		if capture.Query.Name == "EnqueueDirtyFacts" {
			joined := strings.Join(stepDetails(capture), " ")
			for _, required := range []string{"facts_from_id", "facts_target_id"} {
				if !strings.Contains(joined, required) {
					t.Errorf("EnqueueDirtyFacts plan no longer uses %s: %s", required, joined)
				}
			}
			// The queue driving tables scan by design; the graph tables they
			// join must stay covered.
			for _, step := range capture.Steps {
				for _, table := range []string{"facts", "edges", "nodes"} {
					if strings.HasPrefix(step.Detail, "SCAN "+table) && isUncoveredScan(step.Detail) {
						t.Errorf("EnqueueDirtyFacts scans %s without an index: %s", table, step.Detail)
					}
				}
			}
		}
		for _, required := range integerSeeks[capture.Query.Name] {
			joined := strings.Join(stepDetails(capture), " ")
			if !strings.Contains(joined, required) {
				t.Errorf("%s plan does not seek %s: %s", capture.Query.Name, required, joined)
			}
		}
	}
	for name, seen := range tracked {
		if !seen {
			t.Errorf("plan capture missing %s", name)
		}
	}
}

func stepDetails(capture PlanCapture) []string {
	details := make([]string, 0, len(capture.Steps))
	for _, step := range capture.Steps {
		details = append(details, step.Detail)
	}
	return details
}

// measureIntegerKeysObjects checkpoints the database at path and returns the
// dbstat bytes of the adjacency and node-id secondary indexes repurposed onto
// integer surrogates — the edges and facts index objects both layouts name
// identically — together with the nodes table bytes, which carry the
// surrogate column.
func measureIntegerKeysObjects(t *testing.T, path string) (indexBytes int64, nodesBytes int64) {
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
	required := []string{"edges_from", "edges_to", "edges_fact", "facts_from_id", "facts_target_id"}
	seen := map[string]bool{}
	for _, object := range attribution.Objects {
		switch {
		case slices.Contains(required, object.Name):
			seen[object.Name] = true
			indexBytes += object.Bytes
		case strings.HasPrefix(object.Name, "sqlite_autoindex_edges"):
			indexBytes += object.Bytes
		case object.Name == "nodes":
			nodesBytes += object.Bytes
		}
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("dbstat has no %s object: %v", name, objectNames(attribution))
		}
	}
	return indexBytes, nodesBytes
}

// TestIntegerKeysAttributionIsolatesIndexDelta is the structural size check:
// with identical reconciled data, the integer-keyed edges and facts index
// objects must occupy fewer dbstat bytes than the production textual-id
// equivalents, while the nodes table — which carries the surrogate column —
// must not shrink. The delta is the integer-key layout's storage trade in one
// direction, isolated from lookup timing, which Task 8 measures.
func TestIntegerKeysAttributionIsolatesIndexDelta(t *testing.T) {
	ctx := context.Background()
	fixture := GenerateFixture(2, 40)

	productionPath := filepath.Join(t.TempDir(), "graph.db")
	production, err := sqlite.Open(ctx, productionPath)
	if err != nil {
		t.Fatalf("open production adapter: %v", err)
	}
	productionCounts := indexFixtureForAttribution(t, production, fixture)

	integerPath := filepath.Join(t.TempDir(), "graph.db")
	integerKeys, err := OpenVariant(ctx, IntegerKeysSpec("integer-keys"), integerPath)
	if err != nil {
		t.Fatalf("open integer-keys adapter: %v", err)
	}
	integerCounts := indexFixtureForAttribution(t, integerKeys, fixture)

	if productionCounts.Edges == 0 {
		t.Fatal("fixture produced no edges")
	}
	if productionCounts.Edges != integerCounts.Edges || productionCounts.Nodes != integerCounts.Nodes {
		t.Fatalf("adapters hold different counts: production %d edges / %d nodes, integer-keys %d edges / %d nodes",
			productionCounts.Edges, productionCounts.Nodes, integerCounts.Edges, integerCounts.Nodes)
	}

	productionIndexBytes, productionNodesBytes := measureIntegerKeysObjects(t, productionPath)
	integerIndexBytes, integerNodesBytes := measureIntegerKeysObjects(t, integerPath)
	if integerIndexBytes >= productionIndexBytes {
		t.Fatalf("integer-keyed index objects (%d bytes) did not shrink below production (%d bytes) for %d edges",
			integerIndexBytes, productionIndexBytes, productionCounts.Edges)
	}
	if integerNodesBytes < productionNodesBytes {
		t.Fatalf("nodes table shrank from %d to %d bytes despite the surrogate column",
			productionNodesBytes, integerNodesBytes)
	}
}

// TestIntegerKeysBatchBindsPerRow verifies the bounded-batch bind accounting
// for the overridden edge write: every placeholder inside the values tuple —
// including the two surrogate-resolving subselects — binds one value per
// emitted row, while the node, fact, and dirty-marker statements keep the
// production row widths.
func TestIntegerKeysBatchBindsPerRow(t *testing.T) {
	spec := IntegerKeysSpec("integer-keys-binds")
	text, err := spec.statementText()
	if err != nil {
		t.Fatalf("statementText: %v", err)
	}
	expected := map[string]int{
		nodeStatement:      14,
		dirtyNodeStatement: 1,
		factStatement:      15,
		edgeStatement:      13,
	}
	for name, binds := range expected {
		statement, found := text[name]
		if !found {
			t.Fatalf("statement %s missing from merged text", name)
		}
		spec, err := parseBatchSpec(name, statement)
		if err != nil {
			t.Fatalf("parseBatchSpec(%s): %v", name, err)
		}
		if spec.bindsPerRow != binds {
			t.Fatalf("%s binds %d values per row, want %d", name, spec.bindsPerRow, binds)
		}
	}
}
