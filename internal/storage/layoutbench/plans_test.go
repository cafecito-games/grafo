package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/pressly/goose/v3"
)

// openPlanDatabase opens a temporary SQLite database and migrates it to the
// production schema via the embedded Goose migrations.
func openPlanDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "plans.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate plan database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// populatePlanDatabase seeds enough nodes, facts, and edges that the query
// planner cannot treat any graph table as trivially small.
func populatePlanDatabase(t *testing.T, db *sql.DB, rows int) {
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
		id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties
	) VALUES (?, ?, ?, ?, ?, 'parser', 'src/fixture-000001.go', 10, 1, 10, '{}')`)
	if err != nil {
		t.Fatalf("prepare edge seed: %v", err)
	}
	nodeKinds := []string{"function", "method", "type", "class", "interface", "endpoint", "variable", "test"}
	edgeKinds := []string{"calls", "references", "defines"}
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
		if _, err := edgeStatement.Exec(fmt.Sprintf("e:fixture-%06d", index), factID, nodeID,
			counterpartID, edgeKinds[index%len(edgeKinds)]); err != nil {
			t.Fatalf("seed edge %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}
}

var queryNameAnnotation = regexp.MustCompile(`(?m)^--\s*name:\s*([A-Za-z_][A-Za-z0-9_]*)\s*:[^\n]*\n`)

// fileQueryBodies extracts every named query body from queries/*.sql with line
// comments stripped and whitespace collapsed so hand-copied inventory SQL can
// be compared against the sqlc sources without formatting noise.
func fileQueryBodies(t *testing.T) map[string]string {
	t.Helper()
	sources, err := fs.ReadDir(os.DirFS("../../storage/sqlite/queries"), ".")
	if err != nil {
		t.Fatalf("read query directory: %v", err)
	}
	bodies := map[string]string{}
	for _, source := range sources {
		if source.IsDir() || !strings.HasSuffix(source.Name(), ".sql") {
			continue
		}
		content, err := fs.ReadFile(os.DirFS("../../storage/sqlite/queries"), source.Name())
		if err != nil {
			t.Fatalf("read %s: %v", source.Name(), err)
		}
		matches := queryNameAnnotation.FindAllStringSubmatchIndex(string(content), -1)
		for position, match := range matches {
			bodyEnd := len(content)
			if position+1 < len(matches) {
				bodyEnd = matches[position+1][0]
			}
			bodies[string(content[match[2]:match[3]])] = normalizeQueryText(string(content[match[1]:bodyEnd]))
		}
	}
	if len(bodies) == 0 {
		t.Fatal("no named queries found in queries/*.sql")
	}
	return bodies
}

func normalizeQueryText(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if comment, _, found := strings.Cut(line, "--"); found {
			line = comment
		}
		lines = append(lines, line)
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

func TestPlanInventoryMatchesQueryFiles(t *testing.T) {
	fileBodies := fileQueryBodies(t)
	inventory := ProductionPlanQueries()
	seenNames := map[string]bool{}
	for _, query := range inventory {
		if seenNames[query.Name] {
			t.Errorf("duplicate inventory entry for %s", query.Name)
		}
		seenNames[query.Name] = true
		fileBody, found := fileBodies[query.Name]
		if !found {
			t.Errorf("inventory query %s has no -- name: annotation in queries/*.sql", query.Name)
			continue
		}
		if inventoryBody := normalizeQueryText(query.SQL); inventoryBody != fileBody {
			t.Errorf("inventory SQL for %s drifted from queries/*.sql:\n inventory: %s\n file:      %s",
				query.Name, inventoryBody, fileBody)
		}
	}
	for name := range fileBodies {
		if !seenNames[name] {
			t.Errorf("named query %s in queries/*.sql missing from ProductionPlanQueries", name)
		}
	}
}

func TestPlanCaptureValidatesProductionQueries(t *testing.T) {
	ctx := context.Background()
	db := openPlanDatabase(t)
	populatePlanDatabase(t, db, 500)

	captures, err := CapturePlans(ctx, db, ProductionPlanQueries())
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	if len(captures) == 0 {
		t.Fatal("no captures returned")
	}
	names := make([]string, 0, len(captures))
	for _, capture := range captures {
		names = append(names, capture.Query.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("captures are not sorted by query name: %v", names)
	}

	invalid := []PlanCapture{}
	for _, capture := range ValidatePlans(captures) {
		if !capture.Valid {
			invalid = append(invalid, capture)
			continue
		}
		if capture.Reason != "" {
			t.Errorf("%s: valid capture carries a reason: %s", capture.Query.Name, capture.Reason)
		}
	}
	if len(invalid) > 0 {
		for _, capture := range invalid {
			var details []string
			for _, step := range capture.Steps {
				details = append(details, step.Detail)
			}
			t.Errorf("%s invalid: %s (steps: %s)", capture.Query.Name, capture.Reason, strings.Join(details, " | "))
		}
		t.Fatalf("%d of %d production captures invalid", len(invalid), len(captures))
	}
}

func TestPlanCaptureFlagsMissingIndex(t *testing.T) {
	ctx := context.Background()
	db := openPlanDatabase(t)
	populatePlanDatabase(t, db, 500)
	if _, err := db.ExecContext(ctx, "DROP INDEX edges_from"); err != nil {
		t.Fatalf("drop edges_from: %v", err)
	}

	queries := []PlanQuery{findProductionQuery(t, "ListEdgesFrom")}
	captureList, err := CapturePlans(ctx, db, queries)
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	captures := ValidatePlans(captureList)
	if len(captures) != 1 {
		t.Fatalf("expected one capture, got %d", len(captures))
	}
	capture := captures[0]
	if capture.Valid {
		t.Fatal("ListEdgesFrom valid after dropping edges_from")
	}
	if !strings.Contains(capture.Reason, "SCAN edges") {
		t.Errorf("reason %q does not name the uncovered scan", capture.Reason)
	}

	// INDEXED BY guards variant schemas whose named index does not exist:
	// SQLite refuses to prepare, which the capture records as invalid.
	indexedList, err := CapturePlans(ctx, db, []PlanQuery{findProductionQuery(t, "ListOutgoingRelationEdges")})
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	indexed := ValidatePlans(indexedList)
	if indexed[0].Valid {
		t.Fatalf("ListOutgoingRelationEdges valid with edges_from dropped: %+v", indexed[0])
	}
	if !strings.Contains(indexed[0].Reason, "edges_from") {
		t.Errorf("reason %q does not name the missing index", indexed[0].Reason)
	}
}

func TestPlanCaptureRewritesNamedParameters(t *testing.T) {
	ctx := context.Background()
	db := openPlanDatabase(t)
	populatePlanDatabase(t, db, 500)

	subject := findProductionQuery(t, "ListIncomingRelationEdges")
	if !strings.Contains(subject.SQL, "@subject_id") {
		t.Fatalf("ListIncomingRelationEdges no longer uses named parameters: %s", subject.SQL)
	}
	captures, err := CapturePlans(ctx, db, []PlanQuery{subject})
	if err != nil {
		t.Fatalf("CapturePlans: %v", err)
	}
	if len(captures) != 1 || !captures[0].Valid || captures[0].Reason != "" {
		t.Fatalf("named-parameter capture failed: %+v", captures[0])
	}
	if len(captures[0].Steps) == 0 {
		t.Fatal("named-parameter capture recorded no plan steps")
	}

	cases := []struct {
		description string
		query       string
		expectedSQL string
		parameters  int
	}{
		{
			description: "named parameters become numbered in first-appearance order",
			query:       "SELECT * FROM edges WHERE to_id = @subject_id AND kind = @relation LIMIT @max_results",
			expectedSQL: "SELECT * FROM edges WHERE to_id = ?1 AND kind = ?2 LIMIT ?3",
			parameters:  3,
		},
		{
			description: "repeated named parameters bind one value",
			query:       "SELECT * FROM nodes WHERE qualified_name = @target OR name = @target",
			expectedSQL: "SELECT * FROM nodes WHERE qualified_name = ?1 OR name = ?1",
			parameters:  1,
		},
		{
			description: "positional placeholders number after named ones",
			query:       "SELECT * FROM facts WHERE source = @target AND owner_file = ?",
			expectedSQL: "SELECT * FROM facts WHERE source = ?1 AND owner_file = ?2",
			parameters:  2,
		},
		{
			description: "placeholders inside string literals and comments stay literal",
			query:       "SELECT * FROM meta WHERE value = '@target ?' -- @ignored\n AND kind = @kind AND key = ?",
			expectedSQL: "SELECT * FROM meta WHERE value = '@target ?' -- @ignored\n AND kind = ?1 AND key = ?2",
			parameters:  2,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.description, func(t *testing.T) {
			rewritten, parameters := rewriteNamedParameters(testCase.query)
			if rewritten != testCase.expectedSQL {
				t.Errorf("rewritten SQL:\n got %s\nwant %s", rewritten, testCase.expectedSQL)
			}
			if parameters != testCase.parameters {
				t.Errorf("parameter count = %d, want %d", parameters, testCase.parameters)
			}
		})
	}
}

func TestPlanValidationRules(t *testing.T) {
	highCardinality := PlanQuery{Name: "HighPath", SQL: "SELECT * FROM edges WHERE from_id = ?", HighCardinality: true}
	cases := []struct {
		description string
		capture     PlanCapture
		wantValid   bool
		wantReason  string
	}{
		{
			description: "uncovered scan on high-cardinality path is invalid",
			capture: PlanCapture{Query: highCardinality, Valid: true, Steps: []PlanStep{
				{SelectID: 1, Order: 0, From: 0, Detail: "SCAN edges"},
			}},
			wantValid:  false,
			wantReason: "SCAN edges",
		},
		{
			description: "index-covered scan on high-cardinality path is valid",
			capture: PlanCapture{Query: highCardinality, Valid: true, Steps: []PlanStep{
				{SelectID: 1, Order: 0, From: 0, Detail: "SCAN edges USING COVERING INDEX edges_from"},
			}},
			wantValid: true,
		},
		{
			description: "index scan on high-cardinality path is valid",
			capture: PlanCapture{Query: highCardinality, Valid: true, Steps: []PlanStep{
				{SelectID: 1, Order: 0, From: 0, Detail: "SEARCH edges USING INDEX edges_from (from_id=?)"},
			}},
			wantValid: true,
		},
		{
			description: "uncovered scan on a low-cardinality path stays valid",
			capture: PlanCapture{Query: PlanQuery{Name: "LowPath", SQL: "SELECT * FROM dirty_owners"}, Valid: true, Steps: []PlanStep{
				{SelectID: 1, Order: 0, From: 0, Detail: "SCAN dirty_owners"},
			}},
			wantValid: true,
		},
		{
			description: "INDEXED BY name absent from every step is invalid",
			capture: PlanCapture{Query: PlanQuery{
				Name: "Indexed", HighCardinality: true,
				SQL: "SELECT * FROM edges INDEXED BY edges_from WHERE from_id = ?",
			}, Valid: true, Steps: []PlanStep{
				{SelectID: 1, Order: 0, From: 0, Detail: "SCAN edges"},
			}},
			wantValid:  false,
			wantReason: "edges_from",
		},
		{
			description: "INDEXED BY name referenced by a step is valid",
			capture: PlanCapture{Query: PlanQuery{
				Name: "Indexed", HighCardinality: true,
				SQL: "SELECT * FROM edges INDEXED BY edges_from WHERE from_id = ?",
			}, Valid: true, Steps: []PlanStep{
				{SelectID: 1, Order: 0, From: 0, Detail: "SEARCH edges USING INDEX edges_from (from_id=?)"},
			}},
			wantValid: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.description, func(t *testing.T) {
			validated := ValidatePlans([]PlanCapture{testCase.capture})[0]
			if validated.Valid != testCase.wantValid {
				t.Errorf("Valid = %v, want %v (reason: %s)", validated.Valid, testCase.wantValid, validated.Reason)
			}
			if testCase.wantReason != "" && !strings.Contains(validated.Reason, testCase.wantReason) {
				t.Errorf("reason %q does not contain %q", validated.Reason, testCase.wantReason)
			}
			if testCase.wantValid && validated.Reason != "" {
				t.Errorf("valid capture carries reason %q", validated.Reason)
			}
		})
	}
}

func TestPlanQueriesForOverridesSQL(t *testing.T) {
	production := findProductionQuery(t, "ListEdgesTo")
	variantSQL := "SELECT * FROM edges INDEXED BY edges_to WHERE to_id = ? ORDER BY kind, from_id, id"
	queries := PlanQueriesFor(map[string]string{"ListEdgesTo": variantSQL, "NoSuchQuery": "SELECT 1"})
	found := false
	for _, query := range queries {
		if query.Name == "ListEdgesTo" {
			found = true
			if query.SQL != variantSQL {
				t.Errorf("override not applied: %s", query.SQL)
			}
		}
	}
	if !found {
		t.Fatal("ListEdgesTo missing from overridden inventory")
	}
	if production.SQL == variantSQL {
		t.Error("PlanQueriesFor mutated the production inventory")
	}
}

func findProductionQuery(t *testing.T, name string) PlanQuery {
	t.Helper()
	for _, query := range ProductionPlanQueries() {
		if query.Name == name {
			return query
		}
	}
	t.Fatalf("production inventory has no query named %s", name)
	return PlanQuery{}
}
