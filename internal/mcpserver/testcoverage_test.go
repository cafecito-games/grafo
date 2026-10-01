package mcpserver_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestServerExposesScalarAndBatchedStructuralTestTools(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	testNode := graph.Node{ID: "test", Kind: graph.KindTest, Name: "TestCharge", QualifiedName: "sample.TestCharge", OwnerFile: "charge_test.go"}
	helper := graph.Node{ID: "helper", Kind: graph.KindFunction, Name: "helper", QualifiedName: "sample.helper",
		OwnerFile: "charge_test.go", Properties: map[string]string{"test_role": "helper"}}
	target := graph.Node{ID: "charge", Kind: graph.KindFunction, Name: "Charge", QualifiedName: "sample.Charge", OwnerFile: "charge.go"}
	facts := []graph.Fact{
		{ID: "to-helper", FromID: testNode.ID, Kind: graph.EdgeCalls, TargetID: helper.ID, OwnerFile: "charge_test.go"},
		{ID: "to-charge", FromID: helper.ID, Kind: graph.EdgeCalls, TargetID: target.ID, OwnerFile: "charge_test.go"},
	}
	if err := repository.ReplaceOwner(ctx, "charge_test.go", graph.ParseResult{Nodes: []graph.Node{testNode, helper}, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "charge.go", graph.ParseResult{Nodes: []graph.Node{target}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	session := connect(t, mcpserver.New(repository, indexer.Project{Name: "sample", Branch: "main"}))

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tool := range listed.Tools {
		if tool.Name == "find_tests" || tool.Name == "get_test_coverage" {
			found[tool.Name] = true
			if !strings.Contains(strings.ToLower(tool.Description), "not runtime") {
				t.Fatalf("%s description does not disambiguate structural coverage: %q", tool.Name, tool.Description)
			}
		}
	}
	if !found["find_tests"] || !found["get_test_coverage"] {
		t.Fatalf("test tools missing: %#v", found)
	}

	coverage := call(t, session, "get_test_coverage", map[string]any{"selector": "sample.TestCharge"})
	if coverage["designation"] != "structural" || coverage["direction"] != "test_to_production" {
		t.Fatalf("coverage metadata = %#v", coverage)
	}
	matches, _ := coverage["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("coverage matches = %#v", coverage["matches"])
	}

	batch := call(t, session, "find_tests", map[string]any{"selectors": []any{"sample.Charge", "sample.Missing"}})
	results, _ := batch["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("batch results = %#v", batch)
	}
	first, _ := results[0].(map[string]any)
	second, _ := results[1].(map[string]any)
	if first["error"] != nil || first["value"] == nil || second["error"] == nil {
		t.Fatalf("batch envelopes = %#v", results)
	}

	callExpectingError(t, session, "get_test_coverage", map[string]any{
		"selector": "sample.TestCharge", "depth": 33,
	})
}
