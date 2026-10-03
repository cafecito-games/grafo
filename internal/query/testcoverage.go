package query

import (
	"context"
	"fmt"
	"sort"

	"github.com/cafecito-games/grafo/internal/graph"
)

const (
	DefaultTestCoverageDepth = 8
	DefaultTestCoverageLimit = 100
	MaxTestCoverageDepth     = 32
	MaxTestCoverageLimit     = 1000
)

// TestCoverageOptions bounds structural helper expansion. It never requests
// or represents runtime execution coverage.
type TestCoverageOptions struct {
	Depth int            `json:"depth,omitempty"`
	Limit int            `json:"limit,omitempty"`
	Kind  graph.NodeKind `json:"kind,omitempty"`
}

func (o TestCoverageOptions) normalized() (TestCoverageOptions, error) {
	if o.Depth < 0 {
		return o, fmt.Errorf("test coverage depth must not be negative")
	}
	if o.Limit < 0 {
		return o, fmt.Errorf("test coverage limit must not be negative")
	}
	if o.Depth == 0 {
		o.Depth = DefaultTestCoverageDepth
	}
	if o.Limit == 0 {
		o.Limit = DefaultTestCoverageLimit
	}
	if o.Depth > MaxTestCoverageDepth {
		return o, fmt.Errorf("test coverage depth %d exceeds maximum %d", o.Depth, MaxTestCoverageDepth)
	}
	if o.Limit > MaxTestCoverageLimit {
		return o, fmt.Errorf("test coverage limit %d exceeds maximum %d", o.Limit, MaxTestCoverageLimit)
	}
	return o, nil
}

// TestCoverageMatch is one evidence-backed structural relationship. Nodes and
// Edges are ordered from the test declaration to the production target.
type TestCoverageMatch struct {
	Test        graph.Node   `json:"test"`
	Target      graph.Node   `json:"target"`
	Direct      bool         `json:"direct"`
	Depth       int          `json:"depth"`
	Designation string       `json:"designation"`
	Nodes       []graph.Node `json:"nodes"`
	Edges       []graph.Edge `json:"edges"`
}

// TestCoverageReport is deliberately structural rather than runtime coverage.
// Truncated is set for relation overflow, depth/size exhaustion, or a helper
// cycle, so incomplete evidence is never presented as exhaustive.
type TestCoverageReport struct {
	Root        graph.Node          `json:"root"`
	Direction   string              `json:"direction"`
	Designation string              `json:"designation"`
	Depth       int                 `json:"depth"`
	Limit       int                 `json:"limit"`
	Matches     []TestCoverageMatch `json:"matches"`
	// Members is the bounded member expansion a type-level root was answered
	// through, absent for a root that answers for itself. Each match it
	// produced names the member as its target, so a type-level answer carries
	// the same evidence a member query returns rather than a summary of it.
	Members   *MemberAggregation `json:"members,omitempty"`
	Truncated bool               `json:"truncated"`
}

// TestCoverage reports production targets structurally reached by one test.
func (s *Service) TestCoverage(ctx context.Context, selector string, options TestCoverageOptions) (TestCoverageReport, error) {
	options, err := options.normalized()
	if err != nil {
		return TestCoverageReport{}, err
	}
	root, err := s.ResolveKind(ctx, selector, graph.KindTest)
	if err != nil {
		return TestCoverageReport{}, err
	}
	return s.testCoverage(ctx, root, true, options)
}

// FindTests reports tests that structurally reach one production declaration.
func (s *Service) FindTests(ctx context.Context, selector string, options TestCoverageOptions) (TestCoverageReport, error) {
	options, err := options.normalized()
	if err != nil {
		return TestCoverageReport{}, err
	}
	root, err := s.ResolveKind(ctx, selector, options.Kind)
	if err != nil {
		return TestCoverageReport{}, err
	}
	if graph.IsTestSupportNode(root) {
		return TestCoverageReport{}, fmt.Errorf("find tests requires a production target, got %s", root.Kind)
	}
	return s.testCoverage(ctx, root, false, options)
}

type testCoverageState struct {
	node graph.Node
	// target is the production declaration this branch of the walk answers for:
	// the root itself, or one member a type-level root was expanded into. It
	// stays fixed while helper expansion moves node away from it, so direct
	// evidence is judged against the declaration it actually names.
	target   graph.Node
	nodes    []graph.Node
	edges    []graph.Edge
	ancestry map[string]bool
}

func (s *Service) testCoverage(ctx context.Context, root graph.Node, outgoing bool, options TestCoverageOptions) (TestCoverageReport, error) {
	direction := "production_to_tests"
	if outgoing {
		direction = "test_to_production"
	}
	report := TestCoverageReport{Root: root, Direction: direction, Designation: "structural",
		Depth: options.Depth, Limit: options.Limit, Matches: []TestCoverageMatch{}}
	relations, ok := s.repository.(graph.RelationEdgeRepository)
	if !ok {
		return TestCoverageReport{}, fmt.Errorf("repository does not support bounded structural test evidence")
	}
	queue := []testCoverageState{{node: root, target: root, nodes: []graph.Node{root},
		ancestry: map[string]bool{root.ID: true}}}
	seenMatches := map[string]bool{}
	work := 0
	discovered := 1
	// A type declaration carries no test edges of its own; the tests that cover
	// it reach its methods. Expand them as additional targets so a type-level
	// question returns the member evidence instead of a complete-looking empty
	// answer. A test root is asked what it covers, which needs no expansion.
	if !outgoing {
		members, membersTruncated, err := s.declaredMembers(ctx, root, options.Limit)
		if err != nil {
			return TestCoverageReport{}, err
		}
		report.Members = memberAggregation(members, membersTruncated)
		report.Truncated = report.Truncated || membersTruncated
		for _, member := range members {
			if discovered >= options.Limit {
				report.Truncated = true
				break
			}
			discovered++
			queue = append(queue, testCoverageState{node: member.node, target: member.node,
				nodes: []graph.Node{member.node}, ancestry: map[string]bool{root.ID: true, member.node.ID: true}})
		}
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		work++
		if work > options.Limit {
			report.Truncated = true
			break
		}
		direction := graph.IncomingRelations
		if outgoing {
			direction = graph.OutgoingRelations
		}
		page, err := relations.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: state.node.ID,
			Direction: direction, Relations: []graph.EdgeKind{graph.EdgeTests, graph.EdgeCalls, graph.EdgeReferences}, Limit: options.Limit})
		if err != nil {
			return TestCoverageReport{}, err
		}
		report.Truncated = report.Truncated || page.Truncated
		if len(state.edges) >= options.Depth {
			if len(page.Items) > 0 {
				report.Truncated = true
			}
			continue
		}
		authoritativeDirect := map[string]bool{}
		if state.node.ID == state.target.ID {
			for _, item := range page.Items {
				if item.Edge.Kind == graph.EdgeTests {
					authoritativeDirect[item.Counterpart.ID] = true
				}
			}
		}
		for _, item := range page.Items {
			next := item.Counterpart
			edges := append(append([]graph.Edge(nil), state.edges...), item.Edge)
			nodes := append(append([]graph.Node(nil), state.nodes...), next)
			if item.Edge.Kind == graph.EdgeTests {
				if (outgoing && state.node.ID == state.target.ID && next.External) || (!outgoing && next.Kind != graph.KindTest) {
					continue
				}
				match := coverageMatch(state.target, next, nodes, edges, outgoing, true)
				appendCoverageMatch(&report, seenMatches, match, options.Limit)
				continue
			}
			if next.External {
				continue
			}
			if outgoing {
				if graph.IsTestSupportNode(next) && next.Kind != graph.KindTest {
					if state.ancestry[next.ID] {
						report.Truncated = true
						continue
					}
					if discovered >= options.Limit {
						report.Truncated = true
						continue
					}
					discovered++
					queue = append(queue, testCoverageNext(state, next, nodes, edges))
					continue
				}
				// Local direct production relationships are represented by the
				// persisted tests edge. A uniquely projected federated call/reference
				// has no local tests edge to derive, so retain that explicit evidence.
				if !graph.IsTestSupportNode(next) &&
					(state.node.ID != state.target.ID ||
						(item.Edge.Properties["federated"] == "true" && !authoritativeDirect[next.ID])) {
					appendCoverageMatch(&report, seenMatches, coverageMatch(state.target, next, nodes, edges, true,
						state.node.ID == state.target.ID), options.Limit)
				}
				continue
			}
			if next.Kind == graph.KindTest {
				// Its persisted tests edge is the authoritative direct path. A
				// test reached after at least one support node is helper-expanded.
				// Federated raw evidence is direct because no member can persist a
				// tests edge to a declaration that was external during indexing.
				if state.node.ID != state.target.ID ||
					(item.Edge.Properties["federated"] == "true" && !authoritativeDirect[next.ID]) {
					appendCoverageMatch(&report, seenMatches, coverageMatch(state.target, next, nodes, edges, false,
						state.node.ID == state.target.ID), options.Limit)
				}
				continue
			}
			if graph.IsTestSupportNode(next) {
				if state.ancestry[next.ID] {
					report.Truncated = true
					continue
				}
				if discovered >= options.Limit {
					report.Truncated = true
					continue
				}
				discovered++
				queue = append(queue, testCoverageNext(state, next, nodes, edges))
			}
		}
	}
	sort.Slice(report.Matches, func(i, j int) bool {
		left, right := report.Matches[i], report.Matches[j]
		if left.Test.QualifiedName != right.Test.QualifiedName {
			return left.Test.QualifiedName < right.Test.QualifiedName
		}
		if left.Target.QualifiedName != right.Target.QualifiedName {
			return left.Target.QualifiedName < right.Target.QualifiedName
		}
		if left.Depth != right.Depth {
			return left.Depth < right.Depth
		}
		return coveragePathKey(left) < coveragePathKey(right)
	})
	if len(report.Matches) > options.Limit {
		report.Matches = report.Matches[:options.Limit]
		report.Truncated = true
	}
	return report, nil
}

func testCoverageNext(state testCoverageState, next graph.Node, nodes []graph.Node, edges []graph.Edge) testCoverageState {
	ancestry := make(map[string]bool, len(state.ancestry)+1)
	for id := range state.ancestry {
		ancestry[id] = true
	}
	ancestry[next.ID] = true
	return testCoverageState{node: next, target: state.target, nodes: nodes, edges: edges, ancestry: ancestry}
}

func coverageMatch(target, counterpart graph.Node, nodes []graph.Node, edges []graph.Edge, outgoing, direct bool) TestCoverageMatch {
	match := TestCoverageMatch{Direct: direct, Depth: len(edges), Designation: "structural"}
	if outgoing {
		match.Test, match.Target, match.Nodes, match.Edges = target, counterpart, nodes, edges
		return match
	}
	match.Test, match.Target = counterpart, target
	match.Nodes = reversedNodes(nodes)
	match.Edges = reversedCoverageEdges(edges)
	return match
}

func appendCoverageMatch(report *TestCoverageReport, seen map[string]bool, match TestCoverageMatch, limit int) {
	key := match.Test.ID + "\x00" + match.Target.ID
	if seen[key] {
		return
	}
	seen[key] = true
	if len(report.Matches) >= limit {
		report.Truncated = true
		return
	}
	report.Matches = append(report.Matches, match)
}

func coveragePathKey(match TestCoverageMatch) string {
	key := ""
	for _, edge := range match.Edges {
		key += "\x00" + edge.ID
	}
	return key
}

func reversedNodes(values []graph.Node) []graph.Node {
	result := make([]graph.Node, len(values))
	for index := range values {
		result[len(values)-1-index] = values[index]
	}
	return result
}

func reversedCoverageEdges(values []graph.Edge) []graph.Edge {
	result := make([]graph.Edge, len(values))
	for index := range values {
		result[len(values)-1-index] = values[index]
	}
	return result
}
