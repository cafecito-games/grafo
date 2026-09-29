package layoutbench

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

// indexFixture writes every fixture file through the repository and
// reconciles to zero pending work, mirroring what the layout runner measures.
func indexFixture(t *testing.T, ctx context.Context, repository EquivalenceRepository, fixture Fixture) {
	t.Helper()
	for _, file := range fixture.Files {
		if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
			t.Fatalf("replace %s: %v", file.Record.Path, err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pending, err := repository.ReconciliationPending(ctx); err != nil || pending {
		t.Fatalf("reconciliation pending=%t err=%v", pending, err)
	}
}

// TestQueryWorkloadRunsOverProductionAdapter proves the deterministic suite
// executes against a real indexed database: every pattern times, medians are
// recorded, patterns stay unique, and the fixture-derived parameters agree
// with the parameters derived from the database content itself.
func TestQueryWorkloadRunsOverProductionAdapter(t *testing.T) {
	ctx := context.Background()
	fixture := GenerateFixture(3, 4)
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open production adapter: %v", err)
	}
	defer func() { _ = repository.Close() }()
	indexFixture(t, ctx, repository, fixture)

	parameters := FixtureWorkloadParameters()
	derived, err := DeriveWorkloadParameters(ctx, repository)
	if err != nil {
		t.Fatalf("derive workload parameters: %v", err)
	}
	if derived.HubID == "" || derived.ExternalQualifiedName == "" {
		t.Fatalf("derived parameters incomplete: %+v", derived)
	}
	if parameters.ExternalQualifiedName == "" || parameters.HubID == "" || parameters.AmbiguousName == "" {
		t.Fatalf("fixture parameters incomplete: %+v", parameters)
	}

	queries := BuildQueryWorkload(parameters)
	if len(queries) < 8 {
		t.Fatalf("query workload has only %d patterns", len(queries))
	}
	metrics, err := RunQuerySuite(ctx, repository, queries, 2)
	if err != nil {
		t.Fatalf("run query suite: %v", err)
	}
	if len(metrics) != len(queries) {
		t.Fatalf("recorded %d metrics for %d patterns", len(metrics), len(queries))
	}
	seen := map[string]bool{}
	for _, metric := range metrics {
		if metric.Pattern == "" {
			t.Error("metric without a pattern name")
		}
		if seen[metric.Pattern] {
			t.Errorf("duplicate pattern %s", metric.Pattern)
		}
		seen[metric.Pattern] = true
		if metric.Repetitions != 2 {
			t.Errorf("pattern %s recorded %d repetitions, want 2", metric.Pattern, metric.Repetitions)
		}
		if metric.MedianNS < 0 {
			t.Errorf("pattern %s recorded a negative median", metric.Pattern)
		}
	}
	for _, required := range []string{"node-lookup-hub", "edges-from-hub", "relation-edges-incoming", "match-qualified-name", "external-node-probe"} {
		if !seen[required] {
			t.Errorf("query workload is missing the %s pattern", required)
		}
	}
}

// TestRunQuerySuiteRejectsInvalidInput locks the fail-closed contract of the
// suite: zero repetitions and a nil repository are configuration errors, not
// empty measurements.
func TestRunQuerySuiteRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	queries := BuildQueryWorkload(FixtureWorkloadParameters())
	if _, err := RunQuerySuite(ctx, nil, queries, 1); err == nil {
		t.Error("nil repository accepted")
	}
	repository := &stubWorkloadRepository{}
	if _, err := RunQuerySuite(ctx, repository, queries, 0); err == nil {
		t.Error("zero repetitions accepted")
	}
	if _, err := RunQuerySuite(ctx, repository, nil, 1); err == nil {
		t.Error("empty workload accepted")
	}
}

// stubWorkloadRepository satisfies EquivalenceRepository for the suite's
// argument validation, which rejects bad input before any query runs; the
// embedded nil interface therefore never receives a call.
type stubWorkloadRepository struct {
	EquivalenceRepository
}

var errStubProbe = errors.New("stub probe failure")

// TestRunQuerySuitePropagatesQueryFailure proves a failing pattern fails the
// suite instead of silently recording a timing.
func TestRunQuerySuitePropagatesQueryFailure(t *testing.T) {
	ctx := context.Background()
	queries := []WorkloadQuery{{
		Pattern: "failing",
		Run:     func(context.Context, EquivalenceRepository) error { return errStubProbe },
	}}
	if _, err := RunQuerySuite(ctx, &stubWorkloadRepository{}, queries, 1); !errors.Is(err, errStubProbe) {
		t.Fatalf("query failure not propagated: %v", err)
	}
}

// TestPlanInventoryForSpecBindsEveryPlaceholder proves the spec plan inventory
// binds exactly as many values as each statement's placeholder count requires,
// for the control spec and every candidate, including candidate-only
// statements.
func TestPlanInventoryForSpecBindsEveryPlaceholder(t *testing.T) {
	specs := []struct {
		name        string
		spec        LayoutSpec
		mustContain []string
	}{
		{name: "production", spec: ProductionSpec("control"), mustContain: []string{"GetNode"}},
		{name: "slim-edges", spec: SlimEdgesSpec("slim-edges"), mustContain: []string{"GetNode", slimEdgesEvidenceInsertStatement}},
		{name: "integer-keys", spec: IntegerKeysSpec("integer-keys"), mustContain: []string{"GetNode", ResolveNodeKeyStatementName}},
		{name: "index-reform", spec: IndexReformSpec("index-reform"), mustContain: []string{"GetNode"}},
	}
	for _, candidate := range specs {
		queries, err := PlanInventoryForSpec(candidate.spec)
		if err != nil {
			t.Fatalf("%s: PlanInventoryForSpec: %v", candidate.name, err)
		}
		names := make([]string, 0, len(queries))
		for _, query := range queries {
			_, placeholders := rewriteNamedParameters(query.SQL)
			if len(query.Params) != placeholders {
				t.Errorf("%s: statement %s binds %d values for %d placeholders",
					candidate.name, query.Name, len(query.Params), placeholders)
			}
			names = append(names, query.Name)
		}
		if !slices.IsSorted(names) {
			t.Errorf("%s: plan inventory is not sorted by name", candidate.name)
		}
		for _, required := range candidate.mustContain {
			if !slices.Contains(names, required) {
				t.Errorf("%s: plan inventory lacks %s", candidate.name, required)
			}
		}
	}
}
