package detailprofile_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/detailprofile"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

func TestMatrixClassifiesClosedVocabulariesAndProductionProducers(t *testing.T) {
	matrix := detailprofile.Matrix()
	if err := matrix.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := matrix.NodeNames(), stringifyNodes(graph.NodeKinds()); !reflect.DeepEqual(got, want) {
		t.Fatalf("node classifications = %v, want %v", got, want)
	}
	if got, want := matrix.EdgeNames(), stringifyEdges(graph.EdgeKinds()); !reflect.DeepEqual(got, want) {
		t.Fatalf("edge classifications = %v, want %v", got, want)
	}
	wantProducers := append(parserdefaults.NewRegistry().Languages(), graph.ProducerIndexer)
	if got := matrix.ProducerNames(); !reflect.DeepEqual(got, wantProducers) {
		t.Fatalf("producer classifications = %v, want %v", got, wantProducers)
	}
	if len(matrix.Properties) == 0 || len(matrix.Operations) == 0 {
		t.Fatal("property/form and operation classifications must be explicit")
	}
}

func TestFullProjectionPreservesParseResultExactly(t *testing.T) {
	input := fixture()
	got, err := detailprofile.NewProjector(detailprofile.Options{Profile: detailprofile.ProfileFull}).Transform(
		context.Background(), parserapi.Input{Path: "app/main.go"}, input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("full projection changed result:\n got %#v\nwant %#v", got, input)
	}
}

func TestStructuralProjectionDropsLocalPropagationAndPreservesClosure(t *testing.T) {
	input := fixture()
	got, err := detailprofile.NewProjector(detailprofile.Options{Profile: detailprofile.ProfileStructural}).Transform(
		context.Background(), parserapi.Input{Path: "app/main.go"}, input)
	if err != nil {
		t.Fatal(err)
	}
	assertNoFactKind(t, got, graph.EdgeAssigns, graph.EdgePasses, graph.EdgeReturns, graph.EdgeReturnsError)
	assertHasFactKind(t, got, graph.EdgeCalls, graph.EdgeRequests, graph.EdgeTests)
	assertHasNode(t, got, "handler")
	assertNoNode(t, got, "local")
	if err := detailprofile.ValidateClosure([]graph.ParseResult{got}); err != nil {
		t.Fatalf("projected closure: %v", err)
	}
	claimed := detailprofile.ProfileCapabilities(detailprofile.ProfileStructural)
	for _, missing := range []detailprofile.Capability{
		detailprofile.CapabilityImpactDataflow,
		detailprofile.CapabilityFailureFlow,
		detailprofile.CapabilityMessageFieldFlow,
	} {
		if claimed.Has(missing) {
			t.Fatalf("structural candidate claims omitted capability %q", missing)
		}
	}
}

func TestProjectionRejectsDanglingExactEndpointsAndResolutionDrift(t *testing.T) {
	dangling := graph.ParseResult{Nodes: []graph.Node{{ID: "source", Kind: graph.KindFunction, Name: "Source", QualifiedName: "Source"}}, Facts: []graph.Fact{{
		ID: "fact", FromID: "source", Kind: graph.EdgeCalls, TargetID: "missing",
	}}}
	if err := detailprofile.ValidateClosure([]graph.ParseResult{dangling}); err == nil || !strings.Contains(err.Error(), "missing exact target") {
		t.Fatalf("dangling validation error = %v", err)
	}

	full := graph.ParseResult{Nodes: []graph.Node{
		{ID: "caller", Kind: graph.KindFunction, Name: "Caller", QualifiedName: "Caller"},
		{ID: "one", Kind: graph.KindFunction, Name: "Run", QualifiedName: "a.Run"},
		{ID: "two", Kind: graph.KindFunction, Name: "Run", QualifiedName: "b.Run"},
	}, Facts: []graph.Fact{{ID: "call", FromID: "caller", Kind: graph.EdgeCalls, Target: "Run", TargetKind: graph.KindFunction}}}
	projected := full
	projected.Nodes = projected.Nodes[:2]
	if err := detailprofile.ValidateResolution(full, projected); err == nil || !strings.Contains(err.Error(), "resolution drift") {
		t.Fatalf("resolution validation error = %v", err)
	}
}

func TestScopedFullComposesWithIndexScopeBeforeDetailProjection(t *testing.T) {
	projector := detailprofile.NewProjector(detailprofile.Options{
		Profile:         detailprofile.ProfileScopedFull,
		Membership:      projectconfig.IndexScope{Include: []string{"app/**", "vendor/**"}, Exclude: []string{"app/generated/**"}},
		FullDetailRoots: []string{"app"},
	})
	for _, test := range []struct {
		path       string
		excluded   bool
		wantAssign bool
	}{
		{path: "app/main.go", wantAssign: true},
		{path: "vendor/lib.go", wantAssign: false},
		{path: "app/generated/api.go", excluded: true},
		{path: "docs/readme.md", excluded: true},
	} {
		got, decision, err := projector.Project(context.Background(), parserapi.Input{Path: test.path}, fixture())
		if err != nil {
			t.Fatalf("%s: %v", test.path, err)
		}
		if decision.Excluded != test.excluded {
			t.Fatalf("%s excluded = %v, want %v", test.path, decision.Excluded, test.excluded)
		}
		if test.excluded {
			if len(got.Nodes) != 0 || len(got.Facts) != 0 {
				t.Fatalf("%s returned projected evidence despite membership exclusion", test.path)
			}
			continue
		}
		if hasFactKind(got, graph.EdgeAssigns) != test.wantAssign {
			t.Fatalf("%s assigns retained = %v, want %v", test.path, hasFactKind(got, graph.EdgeAssigns), test.wantAssign)
		}
	}
}

func TestCapabilityGateFailsBeforeRepositoryReadAndAttributesFederationMembers(t *testing.T) {
	reads := 0
	err := detailprofile.RunOperation(detailprofile.ProfileCapabilities(detailprofile.ProfileStructural), "impact", func() error {
		reads++
		return nil
	})
	var capabilityErr *detailprofile.CapabilityError
	if !errors.As(err, &capabilityErr) || reads != 0 || capabilityErr.Operation != "impact" {
		t.Fatalf("preflight = (%v, reads=%d)", err, reads)
	}
	if !strings.Contains(err.Error(), "index lacks capability") || !strings.Contains(err.Error(), "rebuild with profile full") {
		t.Fatalf("capability error = %q", err)
	}

	err = detailprofile.RequireFederated(map[string]detailprofile.CapabilitySet{
		"api": detailprofile.ProfileCapabilities(detailprofile.ProfileFull),
		"web": detailprofile.ProfileCapabilities(detailprofile.ProfileStructural),
	}, "message-flow")
	var federated *detailprofile.FederatedCapabilityError
	if !errors.As(err, &federated) || !reflect.DeepEqual(federated.UnsupportedMembers, []string{"web"}) {
		t.Fatalf("federated preflight = %#v", err)
	}
}

func TestProjectionIsDeterministicAndHonorsCancellation(t *testing.T) {
	projector := detailprofile.NewProjector(detailprofile.Options{Profile: detailprofile.ProfileStructural})
	first, err := projector.Transform(context.Background(), parserapi.Input{Path: "app/main.go"}, fixture())
	if err != nil {
		t.Fatal(err)
	}
	second, err := projector.Transform(context.Background(), parserapi.Input{Path: "app/main.go"}, fixture())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical input produced different projection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := projector.Transform(ctx, parserapi.Input{Path: "app/main.go"}, fixture()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled projection error = %v", err)
	}
}

func fixture() graph.ParseResult {
	return graph.ParseResult{Nodes: []graph.Node{
		{ID: "file", Kind: graph.KindFile, Name: "main.go", QualifiedName: "app/main.go", OwnerFile: "app/main.go"},
		{ID: "caller", Kind: graph.KindFunction, Name: "Caller", QualifiedName: "app.Caller", OwnerFile: "app/main.go"},
		{ID: "handler", Kind: graph.KindFunction, Name: "Handler", QualifiedName: "app.Handler", OwnerFile: "app/main.go"},
		{ID: "local", Kind: graph.KindVariable, Name: "value", QualifiedName: "app.Caller.value", OwnerFile: "app/main.go"},
		{ID: "endpoint", Kind: graph.KindEndpoint, Name: "POST /items", QualifiedName: "POST /items", OwnerFile: "app/main.go"},
		{ID: "test", Kind: graph.KindTest, Name: "TestCaller", QualifiedName: "app.TestCaller", OwnerFile: "app/main.go"},
	}, Facts: []graph.Fact{
		{ID: "decl-caller", FromID: "file", Kind: graph.EdgeDeclares, TargetID: "caller", OwnerFile: "app/main.go"},
		{ID: "decl-handler", FromID: "file", Kind: graph.EdgeDeclares, TargetID: "handler", OwnerFile: "app/main.go"},
		{ID: "decl-local", FromID: "caller", Kind: graph.EdgeDeclares, TargetID: "local", OwnerFile: "app/main.go"},
		{ID: "assign", FromID: "local", Kind: graph.EdgeAssigns, TargetID: "local", OwnerFile: "app/main.go"},
		{ID: "pass", FromID: "local", Kind: graph.EdgePasses, TargetID: "handler", OwnerFile: "app/main.go"},
		{ID: "return", FromID: "local", Kind: graph.EdgeReturns, TargetID: "caller", OwnerFile: "app/main.go"},
		{ID: "failure", FromID: "caller", Kind: graph.EdgeReturnsError, TargetID: "handler", OwnerFile: "app/main.go"},
		{ID: "call", FromID: "caller", Kind: graph.EdgeCalls, TargetID: "handler", OwnerFile: "app/main.go"},
		{ID: "request", FromID: "caller", Kind: graph.EdgeRequests, TargetID: "endpoint", OwnerFile: "app/main.go"},
		{ID: "tests", FromID: "test", Kind: graph.EdgeTests, TargetID: "caller", OwnerFile: "app/main.go"},
	}, Diagnostics: []graph.Diagnostic{{Path: "app/main.go", Level: "warning", Message: "fixture"}}}
}

func stringifyNodes(values []graph.NodeKind) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

func stringifyEdges(values []graph.EdgeKind) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

func hasFactKind(result graph.ParseResult, kind graph.EdgeKind) bool {
	for _, fact := range result.Facts {
		if fact.Kind == kind {
			return true
		}
	}
	return false
}

func assertHasFactKind(t *testing.T, result graph.ParseResult, kinds ...graph.EdgeKind) {
	t.Helper()
	for _, kind := range kinds {
		if !hasFactKind(result, kind) {
			t.Errorf("missing %s fact", kind)
		}
	}
}

func assertNoFactKind(t *testing.T, result graph.ParseResult, kinds ...graph.EdgeKind) {
	t.Helper()
	for _, kind := range kinds {
		if hasFactKind(result, kind) {
			t.Errorf("unexpected %s fact", kind)
		}
	}
}

func assertHasNode(t *testing.T, result graph.ParseResult, id string) {
	t.Helper()
	for _, node := range result.Nodes {
		if node.ID == id {
			return
		}
	}
	t.Errorf("missing node %q", id)
}

func assertNoNode(t *testing.T, result graph.ParseResult, id string) {
	t.Helper()
	for _, node := range result.Nodes {
		if node.ID == id {
			t.Errorf("unexpected node %q", id)
		}
	}
}
