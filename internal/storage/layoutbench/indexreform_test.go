package layoutbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/pressly/goose/v3"
)

// TestIndexReformEquivalenceMatchesProduction gates the pure-DDL index-reform
// candidate through the production adapter on both sides: the variant opener
// pre-seeds a database with the reformed Goose migrations and then opens it
// with sqlite.Open, whose own migration check sees the shared version ids and
// skips. Identical adapter code must therefore render a byte-identical
// surface — any difference is the DDL's fault, not the statement text's.
func TestIndexReformEquivalenceMatchesProduction(t *testing.T) {
	ctx := context.Background()
	result, err := RunEquivalence(ctx, EquivalenceInput{
		Control: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return sqlite.Open(ctx, path)
		},
		Variant: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return OpenPreSeeded(ctx, indexReformMigrations, path)
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
		t.Fatalf("index-reform diverged from production: %s", result.FirstDifference)
	}
	if result.ControlDigest == "" || result.ControlDigest != result.VariantDigest {
		t.Fatalf("equivalence digests disagree: control %s variant %s",
			result.ControlDigest, result.VariantDigest)
	}
	if result.ControlProbes != result.VariantProbes || result.ControlProbes == 0 {
		t.Fatalf("probe counts disagree: control %d variant %d", result.ControlProbes, result.VariantProbes)
	}
}

// openIndexReformPlanDatabase opens a temporary SQLite database migrated to
// the index-reform candidate schema.
func openIndexReformPlanDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "index-reform.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, indexReformMigrations)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate index-reform database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// indexReformPlanSeek is one plan assertion: the named production query must
// reference the required index name in at least one step detail, optionally
// requiring the step to be a SEARCH (a seek) rather than a SCAN.
type indexReformPlanSeek struct {
	query        string
	index        string
	requireSeek  bool
	requireCover bool
}

// TestIndexReformPlansRemainCovered runs the whole production query inventory
// against a seeded index-reform database: every capture must validate (no
// uncovered high-cardinality scan, every INDEXED BY name resolvable), the
// retained indexes must still serve their queries with the production access
// path, and the WITHOUT ROWID re-creation must have removed the implicit
// primary-key autoindexes of the reformed tables.
func TestIndexReformPlansRemainCovered(t *testing.T) {
	ctx := context.Background()
	db := openIndexReformPlanDatabase(t)
	populatePlanDatabase(t, db, 500)

	captures, err := CapturePlans(ctx, db, ProductionPlanQueries())
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	byName := map[string]PlanCapture{}
	for _, capture := range ValidatePlans(captures) {
		if !capture.Valid {
			details := make([]string, 0, len(capture.Steps))
			for _, step := range capture.Steps {
				details = append(details, step.Detail)
			}
			t.Errorf("%s invalid: %s (steps: %s)", capture.Query.Name, capture.Reason,
				strings.Join(details, " | "))
			continue
		}
		byName[capture.Query.Name] = capture
	}

	// Every INDEXED BY pin of queries/*.sql stays resolvable and load-bearing.
	seeks := []indexReformPlanSeek{
		{query: "FindNodesExact", index: "nodes_qualified_resolve", requireSeek: true},
		{query: "FindNodesExact", index: "nodes_name_resolve", requireSeek: true},
		{query: "FindNodesExactKind", index: "nodes_qualified_resolve", requireSeek: true},
		{query: "FindNodesExactKind", index: "nodes_name_resolve", requireSeek: true},
		{query: "MatchNodesByQualifiedName", index: "nodes_qualified", requireSeek: true},
		{query: "MatchNodesByName", index: "nodes_name", requireSeek: true},
		{query: "CountNodeMatchesByQualifiedName", index: "nodes_qualified", requireSeek: true},
		{query: "CountNodeMatchesByName", index: "nodes_name", requireSeek: true},
		{query: "ListExternalNodesMatching", index: "nodes_qualified_resolve", requireSeek: true},
		{query: "ListExternalNodesMatching", index: "nodes_name_resolve", requireSeek: true},
		{query: "ListExternalEdgesMatching", index: "nodes_qualified_resolve", requireSeek: true},
		{query: "ListExternalEdgesMatching", index: "nodes_name_resolve", requireSeek: true},
		{query: "ListNodesByKind", index: "nodes_kind", requireSeek: true},
		{query: "CountNodesByKind", index: "nodes_kind", requireCover: true},
		{query: "CountExternalNodes", index: "nodes_external", requireCover: true},
		{query: "DeleteOrphanExternalNodes", index: "nodes_external", requireCover: true},
		{query: "DeleteNodesByOwner", index: "nodes_owner", requireSeek: true},
		{query: "MarkOwnedNodesDirty", index: "nodes_owner", requireSeek: true},
		{query: "GetNode", index: "sqlite_autoindex_nodes_1", requireSeek: true},
		{query: "ListIncomingRelationEdges", index: "edges_to", requireSeek: true},
		{query: "ListOutgoingRelationEdges", index: "edges_from", requireSeek: true},
		{query: "ListEdgesFrom", index: "edges_from", requireSeek: true},
		{query: "ListEdgesTo", index: "edges_to", requireSeek: true},
		{query: "DeleteEdgesByDirtyFactBatch", index: "edges_fact", requireSeek: true},
		{query: "DeleteEdgesByDirtyFactBatch", index: "dirty_facts_order"},
		{query: "DeleteDirtyFactBatch", index: "dirty_facts_order"},
		{query: "ListDirtyFactBatch", index: "dirty_facts_order"},
		{query: "EnqueueDirtyFacts", index: "facts_owner", requireSeek: true},
		{query: "EnqueueDirtyFacts", index: "facts_from_id", requireSeek: true},
		{query: "EnqueueDirtyFacts", index: "facts_target_id", requireSeek: true},
		{query: "EnqueueDirtyFacts", index: "facts_source", requireSeek: true},
		{query: "EnqueueDirtyFacts", index: "facts_target", requireSeek: true},
		{query: "DeleteFactsByOwner", index: "facts_owner", requireSeek: true},
		{query: "ListEmbeddingsByModel", index: "embeddings_model", requireSeek: true},
	}
	for _, seek := range seeks {
		capture, found := byName[seek.query]
		if !found {
			t.Errorf("plan capture missing %s", seek.query)
			continue
		}
		joined := strings.Join(stepDetails(capture), " ")
		if !strings.Contains(joined, seek.index) {
			t.Errorf("%s plan does not use %s: %s", seek.query, seek.index, joined)
			continue
		}
		if seek.requireSeek && !planSeeksIndex(capture, seek.index) {
			t.Errorf("%s no longer seeks %s: %s", seek.query, seek.index, joined)
		}
		if seek.requireCover && !planCoverScansIndex(capture, seek.index) {
			t.Errorf("%s no longer covering-scans %s: %s", seek.query, seek.index, joined)
		}
	}

	// The WITHOUT ROWID re-creation drops each reformed table's implicit
	// primary-key autoindex: the clustered key replaces it.
	for _, table := range indexReformWithoutRowidTables {
		var autoindexes int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND tbl_name = ? AND name LIKE 'sqlite_autoindex%'",
			table,
		).Scan(&autoindexes); err != nil {
			t.Fatalf("count autoindexes of %s: %v", table, err)
		}
		if autoindexes != 0 {
			t.Errorf("reformed table %s still carries %d implicit autoindexes", table, autoindexes)
		}
	}
	// The graph tables keep their rowid storage and therefore their PK
	// autoindexes; a missing one would mean the DDL drifted further than the
	// reform set.
	for _, table := range []string{"nodes", "facts", "edges"} {
		var autoindexes int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND tbl_name = ? AND name LIKE 'sqlite_autoindex%'",
			table,
		).Scan(&autoindexes); err != nil {
			t.Fatalf("count autoindexes of %s: %v", table, err)
		}
		if autoindexes == 0 {
			t.Errorf("graph table %s lost its primary-key autoindex", table)
		}
	}
}

// planSeeksIndex reports whether any step seeks the named index (SEARCH ...).
func planSeeksIndex(capture PlanCapture, index string) bool {
	for _, step := range capture.Steps {
		if strings.HasPrefix(step.Detail, "SEARCH ") && strings.Contains(step.Detail, index) {
			return true
		}
	}
	return false
}

// planCoverScansIndex reports whether any step covering-scans the named index.
func planCoverScansIndex(capture PlanCapture, index string) bool {
	for _, step := range capture.Steps {
		if strings.HasPrefix(step.Detail, "SCAN ") &&
			strings.Contains(step.Detail, "USING COVERING INDEX "+index) {
			return true
		}
	}
	return false
}

// TestIndexReformCollationConsolidationReverts records the measurement that
// reverted the candidate's one index consolidation attempt: replacing
// nodes_qualified + nodes_qualified_resolve with a single COLLATE NOCASE
// composite keeps the NOCASE match queries on a seek but degrades every
// binary-collation resolution query (FindNodesExact, FindNodesExactKind, the
// external matching joins) from a covering index seek to a full index scan,
// because a binary equality cannot seek a NOCASE b-tree. The revert is
// fail-closed: this test fails if that trade ever looks free again.
func TestIndexReformCollationConsolidationReverts(t *testing.T) {
	ctx := context.Background()
	db := openIndexReformPlanDatabase(t)
	for _, statement := range []string{
		"DROP INDEX nodes_qualified",
		"DROP INDEX nodes_name",
		"DROP INDEX nodes_qualified_resolve",
		"DROP INDEX nodes_name_resolve",
		"CREATE INDEX nodes_qualified_resolve ON nodes(qualified_name COLLATE NOCASE, external, kind, id)",
		"CREATE INDEX nodes_name_resolve ON nodes(name COLLATE NOCASE, external, kind, id)",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply consolidation DDL %q: %v", statement, err)
		}
	}
	populatePlanDatabase(t, db, 500)

	queries := []PlanQuery{
		findProductionQuery(t, "FindNodesExact"),
		findProductionQuery(t, "FindNodesExactKind"),
		findProductionQuery(t, "MatchNodesByQualifiedName"),
		findProductionQuery(t, "MatchNodesByName"),
		findProductionQuery(t, "ListExternalNodesMatching"),
	}
	captures, err := CapturePlans(ctx, db, queries)
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	validated := ValidatePlans(captures)
	for _, capture := range validated {
		if !capture.Valid {
			t.Fatalf("%s invalid under the consolidated schema: %s", capture.Query.Name, capture.Reason)
		}
	}
	byName := map[string]PlanCapture{}
	for _, capture := range validated {
		byName[capture.Query.Name] = capture
	}

	// The NOCASE match queries would keep their seek: consolidation serves
	// this side of the trade.
	for _, name := range []string{"MatchNodesByQualifiedName", "MatchNodesByName"} {
		if !planSeeksIndex(byName[name], "nodes_") {
			t.Errorf("%s lost its seek under the consolidated schema: %s", name,
				strings.Join(stepDetails(byName[name]), " "))
		}
	}
	// The binary resolution queries regress to a scan: no step seeks the
	// nodes indexes anymore (the planner may fall back to the partial
	// nodes_external b-tree or scan the composite). This is the reversion
	// evidence.
	for _, name := range []string{"FindNodesExact", "FindNodesExactKind", "ListExternalNodesMatching"} {
		joined := strings.Join(stepDetails(byName[name]), " ")
		if planSeeksIndex(byName[name], "nodes_") {
			t.Errorf("%s still seeks under the consolidated schema; re-examine the reversion: %s", name, joined)
		}
		if !strings.Contains(joined, "SCAN nodes USING ") {
			t.Errorf("%s regressed to something other than an index scan: %s", name, joined)
		}
	}
}

// measureIndexReformObjects checkpoints the database at path and returns the
// dbstat bytes of the queue and config objects the WITHOUT ROWID reform
// touches (each table plus its implicit primary-key autoindex, which the
// reformed storage no longer needs) and of the graph objects the candidate
// leaves alone.
func measureIndexReformObjects(t *testing.T, path string) (queueConfigBytes int64, graphBytes int64) {
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
	reformed := map[string]bool{}
	for _, table := range indexReformWithoutRowidTables {
		reformed[table] = true
		reformed["sqlite_autoindex_"+table+"_1"] = true
	}
	graph := map[string]bool{
		"nodes": true, "facts": true, "edges": true,
		"sqlite_autoindex_nodes_1": true, "sqlite_autoindex_facts_1": true, "sqlite_autoindex_edges_1": true,
		"nodes_name": true, "nodes_qualified": true, "nodes_owner": true, "nodes_kind": true,
		"nodes_external": true, "nodes_name_resolve": true, "nodes_qualified_resolve": true,
		"facts_owner": true, "facts_target": true, "facts_target_id": true,
		"facts_from_id": true, "facts_source": true,
		"edges_from": true, "edges_to": true, "edges_fact": true,
	}
	seen := map[string]bool{}
	for _, object := range attribution.Objects {
		switch {
		case reformed[object.Name]:
			seen[object.Name] = true
			queueConfigBytes += object.Bytes
		case graph[object.Name]:
			seen[object.Name] = true
			graphBytes += object.Bytes
		}
	}
	for name := range reformed {
		// The reformed database legitimately drops the autoindex half of each
		// pair; only the table itself is required in both.
		if !seen[name] && !strings.HasPrefix(name, "sqlite_autoindex_") {
			t.Fatalf("dbstat has no %s object: %v", name, objectNames(attribution))
		}
	}
	for name := range graph {
		if !seen[name] {
			t.Fatalf("dbstat has no %s object: %v", name, objectNames(attribution))
		}
	}
	return queueConfigBytes, graphBytes
}

// TestIndexReformAttributionDelta is the structural size check: with
// identical reconciled data the WITHOUT ROWID queue and config storage must
// occupy fewer dbstat bytes than the production rowid-plus-autoindex
// equivalents, while every graph object the candidate leaves alone must stay
// byte-identical — the reform's entire storage trade is the dropped
// autoindexes.
func TestIndexReformAttributionDelta(t *testing.T) {
	ctx := context.Background()
	fixture := GenerateFixture(2, 40)

	productionPath := filepath.Join(t.TempDir(), "graph.db")
	production, err := sqlite.Open(ctx, productionPath)
	if err != nil {
		t.Fatalf("open production adapter: %v", err)
	}
	productionCounts := indexFixtureForAttribution(t, production, fixture)

	reformPath := filepath.Join(t.TempDir(), "graph.db")
	reformed, err := OpenPreSeeded(ctx, indexReformMigrations, reformPath)
	if err != nil {
		t.Fatalf("open index-reform adapter: %v", err)
	}
	reformCounts := indexFixtureForAttribution(t, reformed, fixture)

	if productionCounts.Edges == 0 || productionCounts.Nodes == 0 || productionCounts.Files == 0 {
		t.Fatalf("fixture produced no graph: %+v", productionCounts)
	}
	if productionCounts.Edges != reformCounts.Edges || productionCounts.Nodes != reformCounts.Nodes {
		t.Fatalf("adapters hold different counts: production %d edges / %d nodes, index-reform %d edges / %d nodes",
			productionCounts.Edges, productionCounts.Nodes, reformCounts.Edges, reformCounts.Nodes)
	}

	productionQueueBytes, productionGraphBytes := measureIndexReformObjects(t, productionPath)
	reformQueueBytes, reformGraphBytes := measureIndexReformObjects(t, reformPath)
	if reformQueueBytes >= productionQueueBytes {
		t.Fatalf("reformed queue/config objects (%d bytes) did not shrink below production (%d bytes)",
			reformQueueBytes, productionQueueBytes)
	}
	if reformGraphBytes != productionGraphBytes {
		t.Fatalf("graph objects changed: production %d bytes, index-reform %d bytes",
			productionGraphBytes, reformGraphBytes)
	}
}

// TestIndexReformDecisionsAreComplete checks the machine-readable evidence
// artifact: every explicit index of the production schema has a recorded
// decision with plan evidence, the WITHOUT ROWID tables are recorded as
// reformed, the collation consolidation attempts are recorded as reverted,
// and the artifact round-trips as JSON for Task 8's report.
func TestIndexReformDecisionsAreComplete(t *testing.T) {
	ctx := context.Background()
	db := openPlanDatabase(t)
	rows, err := db.QueryContext(ctx,
		"SELECT name FROM sqlite_schema WHERE type = 'index' AND sql IS NOT NULL")
	if err != nil {
		t.Fatalf("list production indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var liveIndexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		liveIndexes = append(liveIndexes, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index names: %v", err)
	}
	if len(liveIndexes) < 10 {
		t.Fatalf("production schema exposes only %d explicit indexes", len(liveIndexes))
	}

	report := IndexReformReportEvidence()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var decoded IndexReformReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("round-trip report: %v", err)
	}
	if len(decoded.Decisions) != len(report.Decisions) || decoded.BaselineSeed != report.BaselineSeed {
		t.Fatalf("round-trip lost content: %d decisions became %d", len(report.Decisions), len(decoded.Decisions))
	}

	decisionsByIndex := map[string]IndexReformDecision{}
	reformedTables, reverted := 0, 0
	for _, decision := range report.Decisions {
		if decision.PlanEvidence == "" {
			t.Errorf("decision for %s records no plan evidence", decision.Index)
		}
		switch decision.Decision {
		case "keep", "reformed", "reverted":
		default:
			t.Errorf("decision for %s has unknown outcome %q", decision.Index, decision.Decision)
		}
		if decision.Decision == "reverted" {
			reverted++
		}
		if decision.Decision == "reformed" {
			reformedTables++
		}
		if _, duplicate := decisionsByIndex[decision.Index]; duplicate {
			t.Errorf("duplicate decision for %s", decision.Index)
		}
		decisionsByIndex[decision.Index] = decision
	}
	for _, name := range liveIndexes {
		decision, found := decisionsByIndex[name]
		if !found {
			t.Errorf("production index %s has no recorded decision", name)
			continue
		}
		if decision.Decision != "keep" {
			t.Errorf("live index %s is recorded as %q; the reformed schema keeps it unchanged", name, decision.Decision)
		}
	}
	if reformedTables != len(indexReformWithoutRowidTables) {
		t.Errorf("recorded %d reformed tables, want the %d WITHOUT ROWID tables of the reform set",
			reformedTables, len(indexReformWithoutRowidTables))
	}
	if reverted < 2 {
		t.Errorf("recorded only %d reverted elements; both collation consolidations must be documented", reverted)
	}
	if report.BaselineScale <= 0 || len(report.BaselineObjects) < 10 {
		t.Fatalf("baseline ranking is empty: scale %d, %d objects", report.BaselineScale, len(report.BaselineObjects))
	}
	for _, object := range report.BaselineObjects {
		if object.Bytes <= 0 || object.Pages <= 0 {
			t.Fatalf("baseline object %s carries non-positive measurements", object.Name)
		}
	}
}

// TestIndexReformSpecPreparesProductionStatements pins the registration: the
// index-reform spec carries no statement overrides, so its merged text is the
// production body everywhere and the variant adapter (when used directly,
// outside the pre-seeded production run) prepares the same statements.
func TestIndexReformSpecPreparesProductionStatements(t *testing.T) {
	spec := IndexReformSpec("index-reform")
	if spec.Migrations == nil {
		t.Fatal("index-reform spec has no migrations")
	}
	text, err := spec.statementText()
	if err != nil {
		t.Fatalf("statementText: %v", err)
	}
	production := ProductionPlanQueries()
	if len(text) != len(production) {
		t.Fatalf("merged text holds %d statements, production inventory holds %d", len(text), len(production))
	}
	for _, query := range production {
		if text[query.Name] != query.SQL {
			t.Errorf("statement %s drifted from the production body in the index-reform spec", query.Name)
		}
	}
}
