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
	Truncated   bool                `json:"truncated"`
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
	node     graph.Node
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
	queue := []testCoverageState{{node: root, nodes: []graph.Node{root}, ancestry: map[string]bool{root.ID: true}}}
	seenMatches := map[string]bool{}
	work := 0
	discovered := 1
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
		for _, item := range page.Items {
			next := item.Counterpart
			edges := append(append([]graph.Edge(nil), state.edges...), item.Edge)
			nodes := append(append([]graph.Node(nil), state.nodes...), next)
			if item.Edge.Kind == graph.EdgeTests {
				if (outgoing && state.node.ID == root.ID && next.External) || (!outgoing && next.Kind != graph.KindTest) {
					continue
				}
				match := coverageMatch(root, next, nodes, edges, outgoing, true)
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
				// Direct production relationships are represented by the persisted
				// tests edge. Only helper-expanded evidence is assembled here.
				if state.node.ID != root.ID && !graph.IsTestSupportNode(next) {
					appendCoverageMatch(&report, seenMatches, coverageMatch(root, next, nodes, edges, true, false), options.Limit)
				}
				continue
			}
			if next.Kind == graph.KindTest {
				// Its persisted tests edge is the authoritative direct path. A
				// test reached after at least one support node is helper-expanded.
				if state.node.ID != root.ID {
					appendCoverageMatch(&report, seenMatches, coverageMatch(root, next, nodes, edges, false, false), options.Limit)
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
	return testCoverageState{node: next, nodes: nodes, edges: edges, ancestry: ancestry}
}

func coverageMatch(root, counterpart graph.Node, nodes []graph.Node, edges []graph.Edge, outgoing, direct bool) TestCoverageMatch {
	match := TestCoverageMatch{Direct: direct, Depth: len(edges), Designation: "structural"}
	if outgoing {
		match.Test, match.Target, match.Nodes, match.Edges = root, counterpart, nodes, edges
		return match
	}
	match.Test, match.Target = counterpart, root
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
