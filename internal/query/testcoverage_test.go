package query_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestStructuralTestCoverageDirectAndBoundedHelperExpansion(t *testing.T) {
	ctx := context.Background()
	repository := testCoverageRepository(t)
	service := query.NewService(repository)

	report, err := service.TestCoverage(ctx, "pkg.TestProduce", query.TestCoverageOptions{Depth: 8, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if report.Designation != "structural" || report.Direction != "test_to_production" || !report.Truncated {
		t.Fatalf("report metadata = %#v", report)
	}
	if len(report.Matches) != 3 {
		t.Fatalf("matches = %#v", report.Matches)
	}
	want := map[string]struct {
		direct bool
		depth  int
	}{"pkg.Direct": {true, 1}, "pkg.ThroughOne": {false, 2}, "pkg.ThroughTwo": {false, 3}}
	for _, match := range report.Matches {
		expected, ok := want[match.Target.QualifiedName]
		if !ok || match.Direct != expected.direct || match.Depth != expected.depth || match.Designation != "structural" ||
			match.Nodes[0].Kind != graph.KindTest || match.Nodes[len(match.Nodes)-1].ID != match.Target.ID {
			t.Fatalf("unexpected structural match: %#v", match)
		}
		if match.Target.QualifiedName == "pkg.Direct" && match.Edges[0].Kind != graph.EdgeTests {
			t.Fatalf("persisted tests edge did not win direct evidence deduplication: %#v", match)
		}
		delete(want, match.Target.QualifiedName)
	}
	if len(want) != 0 {
		t.Fatalf("missing matches: %#v", want)
	}

	bounded, err := service.TestCoverage(ctx, "pkg.TestProduce", query.TestCoverageOptions{Depth: 1, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Matches) != 1 || bounded.Matches[0].Target.QualifiedName != "pkg.Direct" || !bounded.Truncated {
		t.Fatalf("depth-bounded report = %#v", bounded)
	}
}

func TestFindTestsReversesHelperEvidenceAndRejectsTestTargets(t *testing.T) {
	ctx := context.Background()
	repository := testCoverageRepository(t)
	service := query.NewService(repository)

	report, err := service.FindTests(ctx, "pkg.ThroughTwo", query.TestCoverageOptions{Depth: 8, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 || report.Direction != "production_to_tests" || report.Matches[0].Direct ||
		report.Matches[0].Test.QualifiedName != "pkg.TestProduce" || report.Matches[0].Depth != 3 {
		t.Fatalf("reverse report = %#v", report)
	}
	path := report.Matches[0].Nodes
	if path[0].QualifiedName != "pkg.TestProduce" || path[len(path)-1].QualifiedName != "pkg.ThroughTwo" {
		t.Fatalf("reverse path is not test-to-target ordered: %#v", path)
	}
	if _, err := service.FindTests(ctx, "pkg.TestProduce", query.TestCoverageOptions{}); err == nil {
		t.Fatal("test declaration accepted as a production target")
	}
}

func TestStructuralTestCoverageValidatesHardBounds(t *testing.T) {
	repository := testCoverageRepository(t)
	service := query.NewService(repository)
	if _, err := service.TestCoverage(context.Background(), "pkg.TestProduce", query.TestCoverageOptions{Depth: query.MaxTestCoverageDepth + 1}); err == nil {
		t.Fatal("oversized depth accepted")
	}
	if _, err := service.FindTests(context.Background(), "pkg.Direct", query.TestCoverageOptions{Limit: query.MaxTestCoverageLimit + 1}); err == nil {
		t.Fatal("oversized limit accepted")
	}
	if _, err := service.TestCoverage(context.Background(), "pkg.TestProduce", query.TestCoverageOptions{Depth: -1}); err == nil {
		t.Fatal("negative depth accepted")
	}
}

func testCoverageRepository(t *testing.T) *sqlite.Repository {
	t.Helper()
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	nodes := []graph.Node{
		{ID: "test", Kind: graph.KindTest, Name: "TestProduce", QualifiedName: "pkg.TestProduce", OwnerFile: "sample_test.go"},
		{ID: "helper-one", Kind: graph.KindFunction, Name: "helperOne", QualifiedName: "pkg.helperOne", OwnerFile: "sample_test.go", Properties: map[string]string{"test_role": "helper"}},
		{ID: "helper-two", Kind: graph.KindMethod, Name: "helperTwo", QualifiedName: "pkg.helperTwo", OwnerFile: "sample_test.go", Properties: map[string]string{"test_role": "helper"}},
		{ID: "direct", Kind: graph.KindFunction, Name: "Direct", QualifiedName: "pkg.Direct", OwnerFile: "sample.go"},
		{ID: "one", Kind: graph.KindFunction, Name: "ThroughOne", QualifiedName: "pkg.ThroughOne", OwnerFile: "sample.go"},
		{ID: "two", Kind: graph.KindFunction, Name: "ThroughTwo", QualifiedName: "pkg.ThroughTwo", OwnerFile: "sample.go"},
	}
	facts := []graph.Fact{
		testCoverageFact("direct", "test", "direct"),
		testCoverageFact("to-helper", "test", "helper-one"),
		testCoverageFact("helper-one-target", "helper-one", "one"),
		testCoverageFact("to-helper-two", "helper-one", "helper-two"),
		testCoverageFact("helper-two-target", "helper-two", "two"),
		testCoverageFact("cycle", "helper-two", "helper-one"),
	}
	// Even if raw evidence carries a federation marker, a persisted local tests
	// edge for the same pair remains authoritative and must deduplicate it.
	facts[0].Properties = map[string]string{"federated": "true"}
	if err := repository.ReplaceOwner(ctx, "sample_test.go", graph.ParseResult{Nodes: nodes[:3], Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "sample.go", graph.ParseResult{Nodes: nodes[3:]}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return repository
}

func testCoverageFact(id, from, target string) graph.Fact {
	return graph.Fact{ID: id, FromID: from, Kind: graph.EdgeCalls, TargetID: target, OwnerFile: "sample_test.go",
		Producer: "go", Location: graph.Location{Path: "sample_test.go", Line: 1}}
}
