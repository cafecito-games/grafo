package query_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestNeighborhoodIsDeterministic(t *testing.T) {
	a := node("A")
	b := node("B")
	c := node("C")
	repository := &fakeRepository{
		nodes: map[string]graph.Node{a.ID: a, b.ID: b, c.ID: c},
		edges: []graph.Edge{
			{ID: "edge-c", FromID: a.ID, ToID: c.ID, Kind: graph.EdgeCalls},
			{ID: "edge-b", FromID: a.ID, ToID: b.ID, Kind: graph.EdgeCalls},
		},
	}
	service := query.NewService(repository)
	first, err := service.Neighborhood(context.Background(), "A", "", 2, query.Outgoing, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Neighborhood(context.Background(), "A", "", 2, query.Outgoing, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same graph produced different traversal:\n%#v\n%#v", first, second)
	}
	if len(first.Nodes) != 3 || first.Nodes[1].Node.Name != "B" || first.Nodes[2].Node.Name != "C" {
		t.Fatalf("unexpected stable ordering: %#v", first.Nodes)
	}
}

func node(name string) graph.Node {
	return graph.Node{ID: graph.NodeID(graph.KindFunction, name), Kind: graph.KindFunction, Name: name, QualifiedName: name}
}

type fakeRepository struct {
	nodes map[string]graph.Node
	edges []graph.Edge
	// matchLimit truncates the candidate list MatchNodes returns below the
	// requested limit, so a test can prove that resolution decides ambiguity from
	// the reported totals rather than from the window it happens to see.
	matchLimit int
}

// MatchNodes implements the graph.NodeMatchGroup contract over an in-memory node
// set: the strongest scope wins - strongest level, and within it local
// declarations over external boundary nodes - totals cover every match whether or
// not it is listed, and case-sensitive matches are listed first.
func (f *fakeRepository) MatchNodes(_ context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
	selector := strings.TrimSpace(request.Selector)
	if selector == "" {
		return graph.NodeMatchGroup{}, nil
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 25
	}
	if f.matchLimit > 0 && f.matchLimit < limit {
		limit = f.matchLimit
	}
	var fallback graph.NodeMatchGroup
	for _, level := range []graph.NodeMatchLevel{graph.MatchQualifiedName, graph.MatchName, graph.MatchSubstring} {
		for _, external := range []bool{false, true} {
			group := graph.NodeMatchGroup{Level: level, External: external}
			for _, node := range f.nodes {
				if request.Kind != "" && node.Kind != request.Kind {
					continue
				}
				if node.External != external {
					continue
				}
				if !graph.LooseMatch(level, selector, node) {
					continue
				}
				group.Total++
				if graph.StrictMatch(level, selector, node) {
					group.Strict++
				}
				group.Nodes = append(group.Nodes, node)
			}
			if group.Total == 0 {
				continue
			}
			graph.SortNodeMatches(level, selector, group.Nodes)
			if len(group.Nodes) > limit {
				group.Nodes = group.Nodes[:limit]
			}
			if group.Strict > 0 {
				return group, nil
			}
			if group.StrongerThan(fallback) {
				fallback = group
			}
		}
	}
	return fallback, nil
}

// SearchNodes mirrors the adapter's substring search, including its ordering and
// its limit, so a test can reproduce a truncated window faithfully.
func (f *fakeRepository) SearchNodes(_ context.Context, term string, limit int) ([]graph.Node, error) {
	var result []graph.Node
	for _, value := range f.nodes {
		if graph.LooseMatch(graph.MatchSubstring, term, value) {
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].External != result[j].External {
			return !result[i].External
		}
		if len(result[i].QualifiedName) != len(result[j].QualifiedName) {
			return len(result[i].QualifiedName) < len(result[j].QualifiedName)
		}
		if result[i].QualifiedName != result[j].QualifiedName {
			return result[i].QualifiedName < result[j].QualifiedName
		}
		return result[i].ID < result[j].ID
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeRepository) Node(_ context.Context, id string) (graph.Node, error) {
	value, ok := f.nodes[id]
	if !ok {
		return graph.Node{}, errors.New("not found")
	}
	return value, nil
}

func (f *fakeRepository) EdgesFrom(_ context.Context, id string) ([]graph.Edge, error) {
	var result []graph.Edge
	for _, edge := range f.edges {
		if edge.FromID == id {
			result = append(result, edge)
		}
	}
	return result, nil
}

func (f *fakeRepository) EdgesTo(_ context.Context, id string) ([]graph.Edge, error) {
	var result []graph.Edge
	for _, edge := range f.edges {
		if edge.ToID == id {
			result = append(result, edge)
		}
	}
	return result, nil
}

// resolutionNode builds a node with an explicit kind, name, and qualified name so
// a test can state the exact shape of a match set.
func resolutionNode(kind graph.NodeKind, name, qualifiedName string) graph.Node {
	return graph.Node{ID: graph.NodeID(kind, qualifiedName), Kind: kind, Name: name, QualifiedName: qualifiedName}
}

// TestResolveDecidesAmbiguityFromTheCompleteMatchSet pins the defect this package
// used to have: ambiguity was decided from a truncated candidate window, so a
// selector with several exact matches could resolve to an arbitrary one of them.
// Every case here keeps more matches in the graph than the window can list.
func TestResolveDecidesAmbiguityFromTheCompleteMatchSet(t *testing.T) {
	var many []graph.Node
	for index := 0; index < 30; index++ {
		many = append(many, resolutionNode(graph.KindMethod, "Handler",
			fmt.Sprintf("example.com/pkg%02d.Service.Handler", index)))
	}

	tests := []struct {
		name       string
		nodes      []graph.Node
		matchLimit int
		selector   string
		total      int
		candidates int
	}{
		{
			name: "exact name matches outnumber the window",
			// 30 methods named Handler, of which the repository lists only 5.
			nodes:      many,
			matchLimit: 5,
			selector:   "Handler",
			total:      30,
			candidates: 5,
		},
		{
			name: "exact matches displaced from the window by shorter substring matches",
			// This is the reported "grafo show Search" shape. Three nodes are named
			// exactly search or Search, and many shorter nodes merely contain that
			// text in their qualified name. Ordering by qualified-name length used
			// to fill the window with the shorter nodes, leaving exactly one exact
			// match inside it, which then resolved as though it were unique.
			nodes:      append(displacingNodes(30, "search"), exactSearchNodes()...),
			matchLimit: 25,
			selector:   "search",
			total:      3,
			candidates: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRepository{nodes: nodeSet(test.nodes...), matchLimit: test.matchLimit}
			node, err := query.NewService(repository).Resolve(context.Background(), test.selector)
			var ambiguous *query.AmbiguousError
			if !errors.As(err, &ambiguous) {
				t.Fatalf("expected *AmbiguousError, got node %q and error %v", node.QualifiedName, err)
			}
			if ambiguous.Total != test.total {
				t.Fatalf("expected total %d, got %d (%v)", test.total, ambiguous.Total, err)
			}
			if len(ambiguous.Candidates) != test.candidates {
				t.Fatalf("expected %d listed candidates, got %d", test.candidates, len(ambiguous.Candidates))
			}
			if ambiguous.Reason == "" || ambiguous.Level == graph.MatchNone {
				t.Fatalf("ambiguity must state how candidates were grouped: %#v", ambiguous)
			}
			if test.candidates < test.total && !strings.Contains(err.Error(), "showing") {
				t.Fatalf("a truncated candidate list must say so: %v", err)
			}
		})
	}
}

// TestResolveListsEveryExactMatchWhenSeveralAreCaseSensitive is the reported
// "grafo show Search" case on this repository: two methods are named Search and
// three more nodes are named search. The two case-sensitive matches make the
// selector ambiguous, and all five are reported because they are what the caller
// has to choose between.
func TestResolveListsEveryExactMatchWhenSeveralAreCaseSensitive(t *testing.T) {
	nodes := nodeSet(
		resolutionNode(graph.KindMethod, "Search", "example.com/search.Service.Search"),
		resolutionNode(graph.KindMethod, "Search", "example.com/semantic.Service.Search"),
		resolutionNode(graph.KindMethod, "search", "example.com/cli.App.search"),
		resolutionNode(graph.KindField, "search", "example.com/mcpserver.Service.search"),
		resolutionNode(graph.KindParameter, "search", "example.com/mcpserver.Service.WithReusable.search"),
	)
	node, err := query.NewService(&fakeRepository{nodes: nodes}).Resolve(context.Background(), "Search")
	var ambiguous *query.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected *AmbiguousError, got node %q and error %v", node.QualifiedName, err)
	}
	if ambiguous.Total != 5 || len(ambiguous.Candidates) != 5 {
		t.Fatalf("expected all five exact-name matches, got total %d and %d candidates",
			ambiguous.Total, len(ambiguous.Candidates))
	}
}

// TestResolvePrefersTheCaseSensitiveMatch covers the secondary defect: EqualFold
// conflated impact with Impact, so two differently-visible Go symbols tied.
func TestResolvePrefersTheCaseSensitiveMatch(t *testing.T) {
	nodes := nodeSet(
		resolutionNode(graph.KindMethod, "impact", "example.com/cli.App.impact"),
		resolutionNode(graph.KindMethod, "Impact", "example.com/query.Service.Impact"),
	)
	service := query.NewService(&fakeRepository{nodes: nodes})
	for selector, expected := range map[string]string{
		"impact": "example.com/cli.App.impact",
		"Impact": "example.com/query.Service.Impact",
	} {
		node, err := service.Resolve(context.Background(), selector)
		if err != nil {
			t.Fatalf("resolve %q: %v", selector, err)
		}
		if node.QualifiedName != expected {
			t.Fatalf("resolve %q: expected %s, got %s", selector, expected, node.QualifiedName)
		}
	}
}

// TestResolvePrefersDeclarationOverItsOwnMembers covers the reported
// "grafo impact query.Service.Impact" case: a declaration's parameters and local
// variables carry its qualified name as a prefix, so substring matching used to
// make every local a rival candidate for the selector that names its parent.
func TestResolvePrefersDeclarationOverItsOwnMembers(t *testing.T) {
	nodes := nodeSet(
		resolutionNode(graph.KindMethod, "Impact", "example.com/query.Service.Impact"),
		resolutionNode(graph.KindParameter, "ctx", "example.com/query.Service.Impact.ctx"),
		resolutionNode(graph.KindParameter, "options", "example.com/query.Service.Impact.options"),
		resolutionNode(graph.KindVariable, "err", "example.com/query.Service.Impact.err@185"),
		// A case-insensitive-only rival that must not enter the decision.
		resolutionNode(graph.KindMethod, "impactSection", "example.com/query.Service.impactSection"),
	)
	node, err := query.NewService(&fakeRepository{nodes: nodes}).Resolve(context.Background(), "Service.Impact")
	if err != nil {
		t.Fatalf("expected the declaration to resolve, got %v", err)
	}
	if node.QualifiedName != "example.com/query.Service.Impact" || node.Kind != graph.KindMethod {
		t.Fatalf("unexpected resolution: %#v", node)
	}
}

// TestResolveKindFiltersCandidates proves the optional kind filter lets a caller
// say it means the function rather than a parameter of the same name.
func TestResolveKindFiltersCandidates(t *testing.T) {
	nodes := nodeSet(
		resolutionNode(graph.KindFunction, "run", "example.com/pkg.run"),
		resolutionNode(graph.KindParameter, "run", "example.com/pkg.Start.run"),
	)
	service := query.NewService(&fakeRepository{nodes: nodes})
	if _, err := service.Resolve(context.Background(), "run"); err == nil {
		t.Fatal("expected an unfiltered selector to stay ambiguous")
	}
	node, err := service.ResolveKind(context.Background(), "run", graph.KindFunction)
	if err != nil {
		t.Fatalf("kind-filtered resolution failed: %v", err)
	}
	if node.QualifiedName != "example.com/pkg.run" {
		t.Fatalf("unexpected resolution: %#v", node)
	}
	if _, err := service.ResolveKind(context.Background(), "run", graph.KindMethod); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a kind nothing matches, got %v", err)
	}
}

// TestResolveKeepsUniqueSelectorsUnchanged guards the behavior the fix must not
// disturb: fully qualified names, node IDs, and single substring matches.
func TestResolveKeepsUniqueSelectorsUnchanged(t *testing.T) {
	handler := resolutionNode(graph.KindMethod, "Handler", "example.com/pkg.Service.Handler")
	other := resolutionNode(graph.KindMethod, "Charge", "example.com/pkg.Service.Charge")
	service := query.NewService(&fakeRepository{nodes: nodeSet(handler, other)})
	for _, selector := range []string{handler.QualifiedName, handler.ID, "Handler", "Service.Handler"} {
		node, err := service.Resolve(context.Background(), selector)
		if err != nil {
			t.Fatalf("resolve %q: %v", selector, err)
		}
		if node.ID != handler.ID {
			t.Fatalf("resolve %q: expected %s, got %s", selector, handler.ID, node.ID)
		}
	}
	if _, err := service.Resolve(context.Background(), "Missing"); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := service.Resolve(context.Background(), "   "); err == nil {
		t.Fatal("expected an empty selector to be rejected")
	}
}

// displacingNodes builds nodes that only contain term in their qualified name and
// sort ahead of any real match, because their qualified names are shorter.
func displacingNodes(count int, term string) []graph.Node {
	result := make([]graph.Node, 0, count)
	for index := 0; index < count; index++ {
		result = append(result, resolutionNode(graph.KindType, fmt.Sprintf("T%02d", index),
			fmt.Sprintf("%s.T%02d", term, index)))
	}
	return result
}

// exactSearchNodes are the nodes named exactly search or Search.
func exactSearchNodes() []graph.Node {
	return []graph.Node{
		resolutionNode(graph.KindMethod, "search", "example.com/internal/cli.App.search"),
		resolutionNode(graph.KindField, "search", "example.com/internal/mcpserver.Service.search"),
		resolutionNode(graph.KindMethod, "Search", "example.com/internal/semantic.Service.Search"),
	}
}

// TestResolveSuppressesOnlyDeclarationMembers is the counterpart to
// TestResolvePrefersDeclarationOverItsOwnMembers: a nested qualified name alone
// must never suppress a candidate, because distinct declarations can legitimately
// nest. Kind is the evidence for "sub-part of a declaration", not the string
// prefix.
func TestResolveSuppressesOnlyDeclarationMembers(t *testing.T) {
	tests := []struct {
		name     string
		nodes    []graph.Node
		selector string
		resolves string
		total    int
	}{
		{
			// Go permits "type Charge struct{}" beside "func (Charge) Charge()".
			// Both are declarations named exactly Charge, so the selector is
			// ambiguous even though one qualified name is a prefix of the other.
			name: "a type and its own method stay ambiguous",
			nodes: []graph.Node{
				resolutionNode(graph.KindType, "Charge", "example.com/pkg.Charge"),
				resolutionNode(graph.KindMethod, "Charge", "example.com/pkg.Charge.Charge"),
			},
			selector: "Charge",
			total:    2,
		},
		{
			name: "a package and a nested function of the same name stay ambiguous",
			nodes: []graph.Node{
				resolutionNode(graph.KindPackage, "charge", "example.com/charge"),
				resolutionNode(graph.KindFunction, "charge", "example.com/charge.charge"),
			},
			selector: "charge",
			total:    2,
		},
		{
			name: "a type and a nested inner type stay ambiguous",
			nodes: []graph.Node{
				resolutionNode(graph.KindClass, "Charge", "example.com/pkg.Charge"),
				resolutionNode(graph.KindClass, "Charge", "example.com/pkg.Outer.Charge"),
			},
			selector: "Charge",
			total:    2,
		},
		{
			name: "parameters and locals are suppressed under their declaration",
			nodes: []graph.Node{
				resolutionNode(graph.KindMethod, "Charge", "example.com/pkg.Service.Charge"),
				resolutionNode(graph.KindParameter, "ctx", "example.com/pkg.Service.Charge.ctx"),
				resolutionNode(graph.KindVariable, "err", "example.com/pkg.Service.Charge.err@12"),
			},
			selector: "Service.Charge",
			resolves: "example.com/pkg.Service.Charge",
		},
		{
			name: "a field is suppressed under the type that declares it",
			nodes: []graph.Node{
				resolutionNode(graph.KindType, "Charge", "example.com/pkg.Charge"),
				resolutionNode(graph.KindField, "Charge", "example.com/pkg.Charge.Charge"),
			},
			selector: "Charge",
			resolves: "example.com/pkg.Charge",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := query.NewService(&fakeRepository{nodes: nodeSet(test.nodes...)})
			node, err := service.Resolve(context.Background(), test.selector)
			if test.resolves != "" {
				if err != nil {
					t.Fatalf("expected %s to resolve, got %v", test.resolves, err)
				}
				if node.QualifiedName != test.resolves {
					t.Fatalf("expected %s, got %s", test.resolves, node.QualifiedName)
				}
				return
			}
			var ambiguous *query.AmbiguousError
			if !errors.As(err, &ambiguous) {
				t.Fatalf("expected *AmbiguousError, got node %q and error %v", node.QualifiedName, err)
			}
			if ambiguous.Total != test.total || len(ambiguous.Candidates) != test.total {
				t.Fatalf("expected %d candidates, got total %d with %d listed",
					test.total, ambiguous.Total, len(ambiguous.Candidates))
			}
		})
	}
}

// TestResolvePrefersLocalDeclarationsOverExternalNodes covers the scope half of
// the strongest-evidence rule: exact evidence about an external boundary node is
// still exact, but a local declaration outranks it, and a merely-containing local
// symbol must not be able to make an exact external match ambiguous.
func TestResolvePrefersLocalDeclarationsOverExternalNodes(t *testing.T) {
	external := graph.Node{ID: graph.NodeID(graph.KindExternal, "net/http.Client"),
		Kind: graph.KindExternal, Name: "Client", QualifiedName: "net/http.Client", External: true}
	container := resolutionNode(graph.KindType, "ClientRegistry", "example.com/pkg.ClientRegistry")
	local := resolutionNode(graph.KindType, "Client", "example.com/pkg.Client")

	// With no local declaration, the exact external qualified name resolves rather
	// than competing at the substring level with the containing local symbol.
	service := query.NewService(&fakeRepository{nodes: nodeSet(external, container)})
	node, err := service.Resolve(context.Background(), "net/http.Client")
	if err != nil {
		t.Fatalf("an exact external qualified name must resolve: %v", err)
	}
	if node.ID != external.ID {
		t.Fatalf("unexpected resolution: %#v", node)
	}
	if node, err = service.Resolve(context.Background(), "Client"); err != nil {
		t.Fatalf("an exact external name must resolve when nothing local matches: %v", err)
	}
	if node.ID != external.ID {
		t.Fatalf("unexpected resolution: %#v", node)
	}

	// A local declaration of the same name wins outright, without ambiguity.
	service = query.NewService(&fakeRepository{nodes: nodeSet(external, container, local)})
	node, err = service.Resolve(context.Background(), "Client")
	if err != nil {
		t.Fatalf("a local declaration must outrank an external boundary node: %v", err)
	}
	if node.ID != local.ID {
		t.Fatalf("unexpected resolution: %#v", node)
	}
}

// TestResolveRanksCaseSensitiveEvidenceAboveStrongerFoldedLevels pins the ordering
// that keeps case handling honest across levels, not just within one. A module
// whose qualified name is "path" matches the selector "Path" only after folding;
// it must not outrank the nodes named exactly "Path", even though a qualified-name
// match is a stronger level than a name match.
func TestResolveRanksCaseSensitiveEvidenceAboveStrongerFoldedLevels(t *testing.T) {
	module := resolutionNode(graph.KindModule, "path", "path")
	first := resolutionNode(graph.KindType, "Path", "example.com/query.Path")
	second := resolutionNode(graph.KindField, "Path", "example.com/parser.Input.Path")

	service := query.NewService(&fakeRepository{nodes: nodeSet(module, first, second)})
	node, err := service.Resolve(context.Background(), "Path")
	var ambiguous *query.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected *AmbiguousError, got node %q and error %v", node.QualifiedName, err)
	}
	// The name level decides, so the selector stays ambiguous instead of resolving
	// to the module. The module is still listed, because at the name level it is
	// one of the folded matches the caller has to tell apart.
	if ambiguous.Level != graph.MatchName || ambiguous.Total != 3 {
		t.Fatalf("expected the exact-name matches to decide, got %#v", ambiguous)
	}
	strict := 0
	for _, candidate := range ambiguous.Candidates {
		if candidate.Name == "Path" {
			strict++
		}
	}
	if strict != 2 {
		t.Fatalf("expected both case-sensitive matches to be listed: %#v", ambiguous.Candidates)
	}

	// The folded match is still the best evidence when nothing matches
	// case-sensitively, and the error says the grouping ignored case.
	service = query.NewService(&fakeRepository{nodes: nodeSet(module)})
	node, err = service.Resolve(context.Background(), "Path")
	if err != nil {
		t.Fatalf("a lone folded match must still resolve: %v", err)
	}
	if node.ID != module.ID {
		t.Fatalf("unexpected resolution: %#v", node)
	}
	service = query.NewService(&fakeRepository{nodes: nodeSet(
		resolutionNode(graph.KindMethod, "search", "example.com/a.Service.search"),
		resolutionNode(graph.KindMethod, "Search", "example.com/b.Service.Search"),
	)})
	if _, err = service.Resolve(context.Background(), "SEARCH"); err == nil ||
		!strings.Contains(err.Error(), "by name ignoring case") {
		t.Fatalf("a folded-only ambiguity must say it ignored case, got %v", err)
	}
}
