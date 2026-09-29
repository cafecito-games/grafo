package layoutbench

import (
	"context"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

// TestVariantAdapterMatchesProduction is the red/green harness for the shared
// variant adapter core: the adapter opened with the zero-delta production
// layout must be exactly equivalent to the production adapter across every
// access pattern, reconciliation behavior, and incremental scenario the
// driver probes. Any later candidate layout is a small SQL/DDL delta on top of
// this proven core.
func TestVariantAdapterMatchesProduction(t *testing.T) {
	ctx := context.Background()
	result, err := RunEquivalence(ctx, EquivalenceInput{
		Control: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return sqlite.Open(ctx, path)
		},
		Variant: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return OpenVariant(ctx, ProductionSpec("production-layout"), path)
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
		t.Fatalf("variant adapter diverged from production: %s", result.FirstDifference)
	}
	if result.ControlDigest == "" || result.ControlDigest != result.VariantDigest {
		t.Fatalf("equivalence digests disagree: control %s variant %s",
			result.ControlDigest, result.VariantDigest)
	}
	if result.ControlProbes != result.VariantProbes || result.ControlProbes == 0 {
		t.Fatalf("probe counts disagree: control %d variant %d", result.ControlProbes, result.VariantProbes)
	}
}

// TestLayoutSpecStatementFallback locks the required fallback behavior of the
// spec: only the statements a layout overrides change; every other entry gets
// the production query text mirrored from queries/*.sql.
func TestLayoutSpecStatementFallback(t *testing.T) {
	spec := ProductionSpec("fallback-check")
	spec.SQL = map[string]string{
		"ListEdgesTo": "SELECT * FROM edges INDEXED BY edges_to WHERE to_id = ? ORDER BY kind, from_id, id",
	}
	text, err := spec.statementText()
	if err != nil {
		t.Fatalf("statementText: %v", err)
	}
	production := map[string]string{}
	for _, query := range ProductionPlanQueries() {
		production[query.Name] = query.SQL
	}
	if len(text) < len(production) {
		t.Fatalf("statement text must cover the production inventory, got %d of %d entries", len(text), len(production))
	}
	for name, body := range production {
		if name == "ListEdgesTo" {
			continue
		}
		if text[name] != body {
			t.Errorf("unspecified statement %s did not fall back to the production text:\n got %s\nwant %s",
				name, text[name], body)
		}
	}
	if text["ListEdgesTo"] == production["ListEdgesTo"] {
		t.Error("ListEdgesTo override was not applied")
	}
	if !strings.Contains(text["ListEdgesTo"], "INDEXED BY edges_to") {
		t.Errorf("ListEdgesTo override text is wrong: %s", text["ListEdgesTo"])
	}
}

// TestEquivalenceDetectsBrokenStatementOverride is the red case for the
// equivalence gate: a layout that overrides a statement candidate layouts own
// (here ListExternalEdgesMatching) with broken SQL must fail the run with the
// diverging probe named, proving the gate can actually fail.
func TestEquivalenceDetectsBrokenStatementOverride(t *testing.T) {
	brokenText := ""
	for _, query := range ProductionPlanQueries() {
		if query.Name == "ListExternalEdgesMatching" {
			brokenText = strings.Replace(query.SQL, "nodes.external = 1", "nodes.external = 1 AND 0", 1)
			if brokenText == query.SQL {
				t.Fatalf("could not break ListExternalEdgesMatching text: %s", query.SQL)
			}
			break
		}
	}
	if brokenText == "" {
		t.Fatal("production inventory has no ListExternalEdgesMatching statement")
	}
	spec := ProductionSpec("broken-external-edges")
	spec.SQL = map[string]string{"ListExternalEdgesMatching": brokenText}
	ctx := context.Background()
	result, err := RunEquivalence(ctx, EquivalenceInput{
		Control: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return sqlite.Open(ctx, path)
		},
		Variant: func(ctx context.Context, path string) (EquivalenceRepository, error) {
			return OpenVariant(ctx, spec, path)
		},
		Fixture:                  GenerateFixture(6, 4),
		BatchInterruptionFixture: GenerateFixture(6, 4),
		ControlDirectory:         t.TempDir(),
		VariantDirectory:         t.TempDir(),
	})
	if err != nil {
		t.Fatalf("RunEquivalence: %v", err)
	}
	if result.Equivalent {
		t.Fatal("equivalence gate passed a broken ListExternalEdgesMatching override")
	}
	if !strings.Contains(result.FirstDifference, "external-edges") {
		t.Fatalf("first difference does not name the external-edges probe: %s", result.FirstDifference)
	}
}

// TestParseBatchSpecRejectsPlaceholdersOutsideTuple locks the batch parser's
// fail-closed rule: a placeholder before or after the values tuple cannot be
// re-bound per emitted row, so it must be rejected at parse time.
func TestParseBatchSpecRejectsPlaceholdersOutsideTuple(t *testing.T) {
	cases := map[string]string{
		"prefix": "INSERT INTO t (a, b) SELECT a, ? FROM seed VALUES (?, ?) ON CONFLICT DO NOTHING",
		"suffix": "INSERT INTO t (a, b) VALUES (?, ?) WHERE rowid <> @excluded",
		"named":  "INSERT INTO t (a, b) VALUES (?, ?) RETURNING rowid = @rowid",
	}
	for name, statement := range cases {
		if _, err := parseBatchSpec("probe-"+name, statement); err == nil ||
			!strings.Contains(err.Error(), "placeholder") {
			t.Errorf("%s placeholder accepted: %v", name, err)
		}
	}
	if _, err := parseBatchSpec("clean",
		"INSERT INTO t (a, b) VALUES (?, ?) ON CONFLICT (a) DO UPDATE SET b = excluded.b"); err != nil {
		t.Errorf("clean statement rejected: %v", err)
	}
}

func TestLayoutSpecRejectsIncompleteSpecs(t *testing.T) {
	withoutMigrations := ProductionSpec("no-migrations")
	withoutMigrations.Migrations = nil
	if _, err := withoutMigrations.statementText(); err == nil || !strings.Contains(err.Error(), "migrations") {
		t.Errorf("spec without migrations accepted: %v", err)
	}
	emptyOverride := ProductionSpec("empty-override")
	emptyOverride.SQL = map[string]string{"GetNode": "  "}
	if _, err := emptyOverride.statementText(); err == nil || !strings.Contains(err.Error(), "GetNode") {
		t.Errorf("empty statement override accepted: %v", err)
	}
	integerKeys := ProductionSpec("integer-keys-missing-statement")
	integerKeys.IntegerKeys = true
	if _, err := integerKeys.statementText(); err == nil ||
		!strings.Contains(err.Error(), ResolveNodeKeyStatementName) {
		t.Errorf("integer-key layout without ResolveNodeKey accepted: %v", err)
	}
	derivedEvidence := ProductionSpec("derived-evidence-missing-hook")
	derivedEvidence.DerivedEvidence = true
	if _, err := derivedEvidence.statementText(); err == nil || !strings.Contains(err.Error(), "EdgeInsert") {
		t.Errorf("derived-evidence layout without EdgeInsert accepted: %v", err)
	}
}
