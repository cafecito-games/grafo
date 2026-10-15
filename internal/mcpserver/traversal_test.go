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

// callerTreeFixture builds the shape that produced a fabricated relationship: a
// root with exactly one direct caller, an intermediate caller that carries two
// callers of its own, and one caller a further hop out.
func callerTreeFixture(t *testing.T) (*sqlite.Repository, indexer.Project) {
	t.Helper()
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	root := graph.Node{ID: "root", Kind: graph.KindFunction, Name: "NewRoot", QualifiedName: "sample.NewRoot", OwnerFile: "root.go"}
	intermediate := graph.Node{ID: "intermediate", Kind: graph.KindFunction, Name: "Intermediate", QualifiedName: "sample.Intermediate", OwnerFile: "root.go"}
	firstOuter := graph.Node{ID: "first-outer", Kind: graph.KindFunction, Name: "FirstOuter", QualifiedName: "sample.FirstOuter", OwnerFile: "root.go"}
	secondOuter := graph.Node{ID: "second-outer", Kind: graph.KindFunction, Name: "SecondOuter", QualifiedName: "sample.SecondOuter", OwnerFile: "root.go"}
	outermost := graph.Node{ID: "outermost", Kind: graph.KindFunction, Name: "Outermost", QualifiedName: "sample.Outermost", OwnerFile: "root.go"}
	facts := []graph.Fact{
		{ID: "calls-root", FromID: intermediate.ID, Kind: graph.EdgeCalls, TargetID: root.ID, OwnerFile: "root.go"},
		{ID: "calls-intermediate-first", FromID: firstOuter.ID, Kind: graph.EdgeCalls, TargetID: intermediate.ID, OwnerFile: "root.go"},
		{ID: "calls-intermediate-second", FromID: secondOuter.ID, Kind: graph.EdgeCalls, TargetID: intermediate.ID, OwnerFile: "root.go"},
		{ID: "calls-first-outer", FromID: outermost.ID, Kind: graph.EdgeCalls, TargetID: firstOuter.ID, OwnerFile: "root.go"},
	}
	parsed := graph.ParseResult{Nodes: []graph.Node{root, intermediate, firstOuter, secondOuter, outermost}, Facts: facts}
	if err := repository.ReplaceOwner(ctx, "root.go", parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return repository, indexer.Project{Name: "sample", Branch: "main"}
}

func TestGetCallersSeparatesDirectCallSitesFromTransitiveReach(t *testing.T) {
	repository, project := callerTreeFixture(t)
	session := connect(t, mcpserver.New(repository, project))

	result := call(t, session, "get_callers", map[string]any{"selector": "sample.NewRoot", "depth": 3})
	edges, _ := result["edges"].([]any)
	if len(edges) != 4 {
		t.Fatalf("expected the whole walked tree: %#v", result["edges"])
	}
	expectedDepth := map[string]float64{"calls-root": 1, "calls-intermediate-first": 2,
		"calls-intermediate-second": 2, "calls-first-outer": 3}
	// Edge IDs are derived, so the tree is keyed by the fact each edge came from.
	var direct []string
	for _, item := range edges {
		edge, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("edge is not an object: %#v", item)
		}
		id, _ := edge["fact_id"].(string)
		depth, hasDepth := edge["depth"].(float64)
		isDirect, hasDirect := edge["direct"].(bool)
		if !hasDepth || !hasDirect {
			t.Fatalf("edge %q carries no depth or direct marker: %#v", id, edge)
		}
		if edge["from_id"] == nil || edge["to_id"] == nil {
			t.Fatalf("edge %q lost its endpoints: %#v", id, edge)
		}
		want, known := expectedDepth[id]
		if !known {
			t.Fatalf("unexpected edge %q: %#v", id, edge)
		}
		if depth != want {
			t.Fatalf("edge %q reported depth %v, want %v", id, depth, want)
		}
		if isDirect {
			direct = append(direct, id)
		}
		if isDirect && depth > 1 {
			t.Fatalf("transitive edge %q is marked direct: %#v", id, edge)
		}
	}
	if len(direct) != 1 || direct[0] != "calls-root" {
		t.Fatalf("direct call sites = %#v, want only the one edge incident to the root", direct)
	}
	if root, _ := result["root"].(map[string]any); root == nil || root["id"] != "root" {
		t.Fatalf("traversal root = %#v", result["root"])
	}
}

func TestTraversalToolDescriptionsWarnAgainstReadingEdgesAsDirect(t *testing.T) {
	repository, project := callerTreeFixture(t)
	session := connect(t, mcpserver.New(repository, project))

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{"get_callers": false, "get_callees": false, "get_neighbors": false}
	for _, tool := range listed.Tools {
		if _, wanted := required[tool.Name]; !wanted {
			continue
		}
		description := strings.ToLower(tool.Description)
		for _, phrase := range []string{"multi-level traversal", "depth", "direct"} {
			if !strings.Contains(description, phrase) {
				t.Fatalf("%s description does not say the result is a bounded multi-level tree (%q missing): %q",
					tool.Name, phrase, tool.Description)
			}
		}
		required[tool.Name] = true
	}
	for name, found := range required {
		if !found {
			t.Fatalf("traversal tool %s is not exposed", name)
		}
	}
}
