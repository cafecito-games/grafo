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
