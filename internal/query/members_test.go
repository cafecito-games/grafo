package query_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestImpactAggregatesClassMemberCallers proves a class root reports the
// callers and dependencies its declared methods carry, through the declaration
// edge that proves each membership, instead of a complete-looking empty answer.
func TestImpactAggregatesClassMemberCallers(t *testing.T) {
	class := impactNode(graph.KindClass, "TradeModule", "trade/trade_module.gd", 1)
	install := impactNode(graph.KindMethod, "TradeModule.install", "trade/trade_module.gd", 10)
	initializer := impactNode(graph.KindMethod, "TradeModule._init", "trade/trade_module.gd", 4)
	field := impactNode(graph.KindField, "TradeModule.catalog", "trade/trade_module.gd", 2)
	coordinator := impactNode(graph.KindMethod, "InGameSessionCoordinator._build_container_runtime",
		"session/in_game_session_coordinator.gd", 30)
	dependency := impactNode(graph.KindMethod, "Catalog.lookup", "trade/catalog.gd", 8)
	nodes := nodeSet(class, install, initializer, field, coordinator, dependency)
	edges := []graph.Edge{
		declaresEdge("declares-install", class.ID, install.ID),
		declaresEdge("declares-init", class.ID, initializer.ID),
		declaresEdge("declares-field", class.ID, field.ID),
		{ID: "calls-install", FromID: coordinator.ID, ToID: install.ID, Kind: graph.EdgeCalls},
		{ID: "calls-dependency", FromID: install.ID, ToID: dependency.ID, Kind: graph.EdgeCalls},
	}

	service := query.NewService(&fakeRepository{nodes: nodes, edges: edges})
	report, err := service.Impact(context.Background(), "TradeModule", query.ImpactOptions{Kind: graph.KindClass})
	if err != nil {
		t.Fatal(err)
	}
	if report.Members == nil || report.Members.Relation != graph.EdgeDeclares || report.Members.Truncated {
		t.Fatalf("member aggregation = %#v", report.Members)
	}
	// A field carries no call evidence of its own, so only the callables are
	// aggregated; a complete member list must not be reported as truncated.
	if names := nodeNames(report.Members.Members); len(names) != 2 ||
		names[0] != "TradeModule._init" || names[1] != "TradeModule.install" {
		t.Fatalf("aggregated members = %#v", names)
	}
	assertReached(t, report.Upstream, map[string]int{
		"TradeModule": 0, "TradeModule.install": 1, "TradeModule._init": 1,
		"InGameSessionCoordinator._build_container_runtime": 2,
	})
	assertReached(t, report.Downstream, map[string]int{
		"TradeModule": 0, "TradeModule.install": 1, "TradeModule._init": 1, "Catalog.lookup": 2,
	})
	// The declaration edge has to travel with the member, or the caller appears
	// in the report with no path connecting it to the class.
	if !hasEdge(report.Upstream.Edges, "declares-install") || !hasEdge(report.Upstream.Edges, "calls-install") {
		t.Fatalf("upstream edges do not spell the class-to-caller path: %#v", report.Upstream.Edges)
	}
	// A section reports the edge kinds it carries, so a consumer validating
	// Edges against Relations cannot be made to reject the membership edge.
	for _, section := range []query.ImpactSection{report.Upstream, report.Downstream} {
		declared := map[graph.EdgeKind]bool{}
		for _, relation := range section.Relations {
			declared[relation] = true
		}
		if !declared[graph.EdgeDeclares] {
			t.Fatalf("%s section omits declares from its relation vocabulary: %#v", section.Direction, section.Relations)
		}
		for _, edge := range section.Edges {
			if !declared[edge.Kind] {
				t.Fatalf("%s section reports edge kind %q outside its vocabulary", section.Direction, edge.Kind)
			}
		}
	}
	// Containment must stay one level outward from the root: a sibling of the
	// class in the same declaring module is not part of its blast radius.
	for _, reached := range report.Upstream.Nodes {
		if reached.Node.Kind == graph.KindField {
			t.Fatalf("aggregation widened into non-callable members: %#v", report.Upstream.Nodes)
		}
	}
}

// TestImpactReportsMemberAggregationBoundSeparately proves a member list cut
// short by the traversal limit is reported on its own flag, so an incomplete
// aggregation is never hidden behind a complete-looking traversal.
func TestImpactReportsMemberAggregationBoundSeparately(t *testing.T) {
	class := impactNode(graph.KindClass, "Wide", "wide.gd", 1)
	nodes := []graph.Node{class}
	var edges []graph.Edge
	for _, name := range []string{"Wide.alpha", "Wide.beta", "Wide.gamma"} {
		member := impactNode(graph.KindMethod, name, "wide.gd", 5)
		nodes = append(nodes, member)
		edges = append(edges, declaresEdge("declares-"+name, class.ID, member.ID))
	}
	service := query.NewService(&fakeRepository{nodes: nodeSet(nodes...), edges: edges})
	report, err := service.Impact(context.Background(), "Wide",
		query.ImpactOptions{Kind: graph.KindClass, UpstreamLimit: 2, DownstreamLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if report.Members == nil || !report.Members.Truncated || len(report.Members.Members) != 2 || !report.Truncated {
		t.Fatalf("bounded member aggregation = %#v (report truncated %t)", report.Members, report.Truncated)
	}
}

// TestImpactLeavesMemberlessRootsAlone proves a method answers for itself: it
// reports no member section at all rather than an empty one, and a class that
// declares nothing aggregatable is equally silent.
func TestImpactLeavesMemberlessRootsAlone(t *testing.T) {
	method := impactNode(graph.KindMethod, "TradeModule.install", "trade/trade_module.gd", 10)
	bare := impactNode(graph.KindClass, "Marker", "marker.gd", 1)
	service := query.NewService(&fakeRepository{nodes: nodeSet(method, bare)})
	for _, selector := range []string{"TradeModule.install", "Marker"} {
		report, err := service.Impact(context.Background(), selector, query.ImpactOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.Members != nil {
			t.Fatalf("%s reported a member section: %#v", selector, report.Members)
		}
		// A root with nothing to aggregate keeps the unchanged impact
		// vocabulary, so containment stays out of a report that never seeded a
		// member.
		for _, section := range []query.ImpactSection{report.Upstream, report.Downstream} {
			if slices.Contains(section.Relations, graph.EdgeDeclares) {
				t.Fatalf("%s widened its relation vocabulary without members: %#v", selector, section.Relations)
			}
		}
	}
}

// TestFindTestsAggregatesClassMemberTests proves a class root returns the
// structural tests of its declared methods with the direct and helper-expanded
// designations and depths a member query returns, each match naming the member
// it covers.
func TestFindTestsAggregatesClassMemberTests(t *testing.T) {
	ctx := context.Background()
	repository := classMemberCoverageRepository(t)
	service := query.NewService(repository)

	report, err := service.FindTests(ctx, "TradeModule", query.TestCoverageOptions{Kind: graph.KindClass})
	if err != nil {
		t.Fatal(err)
	}
	if report.Members == nil || report.Members.Truncated || len(report.Members.Members) != 2 {
		t.Fatalf("member aggregation = %#v", report.Members)
	}
	want := map[string]struct {
		target string
		direct bool
		depth  int
	}{
		// The test that constructs the class is direct evidence for the class
		// itself; the rig reaches a member through a test helper.
		"TradeModuleTest.test_installs":                 {target: "TradeModule", direct: true, depth: 1},
		"InGameSessionCoordinatorTradeTest.test_builds": {target: "TradeModule.install", direct: false, depth: 2},
		"TradeModuleTest.test_calls_install":            {target: "TradeModule.install", direct: true, depth: 1},
	}
	for _, match := range report.Matches {
		expected, ok := want[match.Test.QualifiedName]
		if !ok {
			t.Fatalf("unexpected match: %#v", match)
		}
		if match.Target.QualifiedName != expected.target || match.Direct != expected.direct ||
			match.Depth != expected.depth || match.Designation != "structural" {
			t.Fatalf("match %s = %#v", match.Test.QualifiedName, match)
		}
		if match.Nodes[0].ID != match.Test.ID || match.Nodes[len(match.Nodes)-1].ID != match.Target.ID {
			t.Fatalf("match path is not test-to-target ordered: %#v", match)
		}
		delete(want, match.Test.QualifiedName)
	}
	if len(want) != 0 {
		t.Fatalf("missing member-aggregated matches: %#v", want)
	}

	// A member query must keep returning exactly what the aggregated class
	// query attributes to that member.
	member, err := service.FindTests(ctx, "TradeModule.install", query.TestCoverageOptions{Kind: graph.KindMethod})
	if err != nil {
		t.Fatal(err)
	}
	if member.Members != nil || len(member.Matches) != 2 {
		t.Fatalf("member query = %#v", member)
	}
}

// classMemberCoverageRepository models the shape issue #213 reported: a class
// whose construction sites and member tests live outside its own declaration.
func classMemberCoverageRepository(t *testing.T) *sqlite.Repository {
	t.Helper()
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	production := []graph.Node{
		{ID: "class", Kind: graph.KindClass, Name: "TradeModule", QualifiedName: "TradeModule", OwnerFile: "trade_module.gd"},
		{ID: "install", Kind: graph.KindMethod, Name: "install", QualifiedName: "TradeModule.install", OwnerFile: "trade_module.gd"},
		{ID: "init", Kind: graph.KindMethod, Name: "_init", QualifiedName: "TradeModule._init", OwnerFile: "trade_module.gd"},
	}
	productionFacts := []graph.Fact{
		{ID: "declares-install", FromID: "class", Kind: graph.EdgeDeclares, TargetID: "install",
			OwnerFile: "trade_module.gd", Producer: graph.ProducerGDScript, Location: graph.Location{Path: "trade_module.gd", Line: 5}},
		{ID: "declares-init", FromID: "class", Kind: graph.EdgeDeclares, TargetID: "init",
			OwnerFile: "trade_module.gd", Producer: graph.ProducerGDScript, Location: graph.Location{Path: "trade_module.gd", Line: 2}},
	}
	tests := []graph.Node{
		{ID: "class-test", Kind: graph.KindTest, Name: "test_installs", QualifiedName: "TradeModuleTest.test_installs",
			OwnerFile: "trade_module_test.gd"},
		{ID: "member-test", Kind: graph.KindTest, Name: "test_calls_install", QualifiedName: "TradeModuleTest.test_calls_install",
			OwnerFile: "trade_module_test.gd"},
		{ID: "rig-test", Kind: graph.KindTest, Name: "test_builds",
			QualifiedName: "InGameSessionCoordinatorTradeTest.test_builds", OwnerFile: "coordinator_trade_test.gd"},
		{ID: "rig", Kind: graph.KindMethod, Name: "_init", QualifiedName: "InGameSessionCoordinatorTradeTest.Rig._init",
			OwnerFile: "coordinator_trade_test.gd", Properties: map[string]string{"test_role": "helper"}},
	}
	testFacts := []graph.Fact{
		constructionFact("construct-class", "class-test", "class", "trade_module_test.gd"),
		coverageCallFact("call-install", "member-test", "install", "trade_module_test.gd"),
		coverageCallFact("rig-helper", "rig-test", "rig", "coordinator_trade_test.gd"),
		coverageCallFact("rig-install", "rig", "install", "coordinator_trade_test.gd"),
	}
	if err := repository.ReplaceOwner(ctx, "trade_module.gd",
		graph.ParseResult{Nodes: production, Facts: productionFacts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "trade_module_test.gd",
		graph.ParseResult{Nodes: tests[:2], Facts: testFacts[:2]}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "coordinator_trade_test.gd",
		graph.ParseResult{Nodes: tests[2:], Facts: testFacts[2:]}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return repository
}

func constructionFact(id, from, target, owner string) graph.Fact {
	fact := coverageCallFact(id, from, target, owner)
	fact.Properties = map[string]string{"form": "construction", "constructor": "new"}
	return fact
}

func coverageCallFact(id, from, target, owner string) graph.Fact {
	return graph.Fact{ID: id, FromID: from, Kind: graph.EdgeCalls, TargetID: target, OwnerFile: owner,
		Producer: graph.ProducerGDScript, Location: graph.Location{Path: owner, Line: 1}}
}

func declaresEdge(id, from, to string) graph.Edge {
	return graph.Edge{ID: id, FromID: from, ToID: to, Kind: graph.EdgeDeclares}
}

func nodeNames(nodes []graph.Node) []string {
	result := make([]string, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node.QualifiedName)
	}
	return result
}

func hasEdge(edges []graph.Edge, id string) bool {
	for _, edge := range edges {
		if edge.ID == id {
			return true
		}
	}
	return false
}

func assertReached(t *testing.T, section query.ImpactSection, want map[string]int) {
	t.Helper()
	got := map[string]int{}
	for _, reached := range section.Nodes {
		got[reached.Node.QualifiedName] = reached.Depth
	}
	for name, depth := range want {
		actual, ok := got[name]
		if !ok {
			t.Fatalf("%s section is missing %s: %#v", section.Direction, name, got)
		}
		if actual != depth {
			t.Fatalf("%s section reached %s at depth %d, want %d", section.Direction, name, actual, depth)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s section reached unexpected nodes: %#v", section.Direction, got)
	}
}
