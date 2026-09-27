package query

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cafecito-games/grafo/internal/graph"
)

type Direction string

const (
	Outgoing Direction = "outgoing"
	Incoming Direction = "incoming"
	Both     Direction = "both"
)

// AmbiguousError reports that a selector matched several equally good nodes.
// Total is the number of matches in the graph, which can exceed the number of
// listed Candidates, so a caller can always tell that the list is partial. Level
// and Reason say why the candidates were grouped together: they all matched the
// selector the same way, and no weaker match is ever mixed in.
type AmbiguousError struct {
	Term       string               `json:"term"`
	Kind       graph.NodeKind       `json:"kind,omitempty"`
	Level      graph.NodeMatchLevel `json:"level,omitempty"`
	Reason     string               `json:"reason,omitempty"`
	Total      int                  `json:"total"`
	Candidates []graph.Node         `json:"candidates"`
}

func (e *AmbiguousError) Error() string {
	total := e.Total
	if total < len(e.Candidates) {
		total = len(e.Candidates)
	}
	var message strings.Builder
	fmt.Fprintf(&message, "%q matches %d nodes", e.Term, total)
	if e.Kind != "" {
		fmt.Fprintf(&message, " of kind %s", e.Kind)
	}
	if e.Reason != "" {
		message.WriteString(" " + e.Reason)
	}
	if len(e.Candidates) < total {
		fmt.Fprintf(&message, " (showing %d)", len(e.Candidates))
	}
	message.WriteString("; use a qualified name or node ID")
	return message.String()
}

// matchReason names the evidence that grouped a set of candidates, for the
// human-readable half of AmbiguousError.
func matchReason(level graph.NodeMatchLevel) string {
	switch level {
	case graph.MatchQualifiedName:
		return "by qualified name"
	case graph.MatchName:
		return "by name"
	case graph.MatchSubstring:
		return "by substring"
	default:
		return ""
	}
}

var ErrNotFound = errors.New("node not found")

type ReachedNode struct {
	Depth int        `json:"depth"`
	Node  graph.Node `json:"node"`
}

type Traversal struct {
	Root      graph.Node    `json:"root"`
	Nodes     []ReachedNode `json:"nodes"`
	Edges     []graph.Edge  `json:"edges"`
	Direction Direction     `json:"direction"`
	Truncated bool          `json:"truncated"`
}

type Path struct {
	From  graph.Node   `json:"from"`
	To    graph.Node   `json:"to"`
	Nodes []graph.Node `json:"nodes"`
	Edges []graph.Edge `json:"edges"`
}

type Service struct {
	repository graph.QueryRepository
	source     SourceReader
}

func NewService(repository graph.QueryRepository) *Service { return &Service{repository: repository} }

func (s *Service) Find(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.repository.SearchNodes(ctx, strings.TrimSpace(term), limit)
}

// candidateLimit bounds how many candidates an ambiguity report lists. It never
// bounds the counts the ambiguity decision is made from: the repository reports
// complete totals, so truncating this list can never narrow a match set.
const candidateLimit = 25

// Resolve resolves a selector to exactly one node, or reports why it cannot.
// It is the single resolution path behind every CLI command and MCP tool.
func (s *Service) Resolve(ctx context.Context, selector string) (graph.Node, error) {
	return s.ResolveKind(ctx, selector, "")
}

// ResolveKind resolves a selector restricted to one node kind; an empty kind
// accepts any kind. A caller that means the function rather than one of its
// parameters can say so instead of guessing at the candidate list.
//
// The contract is that resolution either returns one node on evidence or fails,
// never one of several equally good matches. That holds because every decision
// below is made from graph.NodeMatchGroup totals, which cover the whole graph,
// rather than from the truncated candidate list:
//
//   - Only the strongest match level contributes. A substring match can never
//     make an exact name match ambiguous.
//   - Within a level, case-sensitive matches decide uniqueness whenever any
//     exist, so "impact" resolves to App.impact rather than tying with
//     Service.Impact. Case-insensitive-only matches remain in the reported
//     candidate list, because they are what the caller most likely has to
//     disambiguate between.
//   - A candidate whose qualified name merely extends another candidate's is a
//     descendant of it, not a rival: a method is preferred over its own
//     parameters and local variables.
//   - Narrowing a truncated list is refused outright, because the nodes that
//     were cut off could be equally good matches.
func (s *Service) ResolveKind(ctx context.Context, selector string, kind graph.NodeKind) (graph.Node, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return graph.Node{}, errors.New("selector is required")
	}
	if strings.HasPrefix(selector, "n:") {
		if node, err := s.repository.Node(ctx, selector); err == nil && (kind == "" || node.Kind == kind) {
			return node, nil
		}
	}
	group, err := s.repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: selector, Kind: kind, Limit: candidateLimit})
	if err != nil {
		return graph.Node{}, err
	}
	if group.Total == 0 || len(group.Nodes) == 0 {
		return graph.Node{}, fmt.Errorf("%w: %s", ErrNotFound, selector)
	}
	// The pool is the evidence uniqueness is decided from; it is complete only
	// when the repository listed every match it counted.
	pool, poolTotal := group.Nodes, group.Total
	if group.Strict > 0 {
		pool, poolTotal = strictMatches(group, selector), group.Strict
	}
	if poolTotal == 1 && len(pool) == 1 {
		return pool[0], nil
	}
	if poolTotal <= len(pool) {
		if declarations := preferDeclarations(pool); len(declarations) == 1 {
			return declarations[0], nil
		}
	}
	// Report every match at this level, not just the case-sensitive ones: the
	// caller needs to see the near miss that made the selector ambiguous.
	candidates, total := group.Nodes, group.Total
	if total <= len(candidates) {
		if declarations := preferDeclarations(candidates); len(declarations) < len(candidates) {
			candidates, total = declarations, len(declarations)
		}
	}
	return graph.Node{}, &AmbiguousError{Term: selector, Kind: kind, Level: group.Level,
		Reason: matchReason(group.Level), Total: total, Candidates: candidates}
}

// strictMatches keeps the case-sensitive matches of a group. The group contract
// orders them first, so the result holds every strict match in the graph
// whenever group.Strict does not exceed the listed candidates.
func strictMatches(group graph.NodeMatchGroup, selector string) []graph.Node {
	result := make([]graph.Node, 0, len(group.Nodes))
	for _, node := range group.Nodes {
		if graph.StrictMatch(group.Level, selector, node) {
			result = append(result, node)
		}
	}
	return result
}

// preferDeclarations drops candidates whose qualified name merely extends
// another candidate's at a non-identifier boundary. Parameters, local variables,
// and fields carry their declaring symbol's qualified name as a prefix, so
// matching would otherwise make every local a rival candidate for the selector
// that names its parent. Keeping the shorter declaration is always the right
// call, because the caller named the parent and not the child. The input is
// returned unchanged when the rule would leave nothing.
func preferDeclarations(nodes []graph.Node) []graph.Node {
	if len(nodes) < 2 {
		return nodes
	}
	result := make([]graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if !extendsAnyQualifiedName(node, nodes) {
			result = append(result, node)
		}
	}
	if len(result) == 0 {
		return nodes
	}
	return result
}

func extendsAnyQualifiedName(node graph.Node, nodes []graph.Node) bool {
	for _, other := range nodes {
		if other.ID == node.ID {
			continue
		}
		if extendsQualifiedName(node.QualifiedName, other.QualifiedName) {
			return true
		}
	}
	return false
}

// extendsQualifiedName reports whether child continues parent past a separator.
// The boundary test keeps sibling symbols apart: "Impacted" does not extend
// "Impact", while "Impact.selector" and "Impact.err@185" both do.
func extendsQualifiedName(child, parent string) bool {
	if parent == "" || len(child) <= len(parent) || !strings.HasPrefix(child, parent) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(child[len(parent):])
	return !(next == '_' || unicode.IsLetter(next) || unicode.IsDigit(next))
}

// Neighborhood walks edges from a resolved root. kind optionally restricts
// which node the selector may resolve to; an empty kind accepts any kind.
func (s *Service) Neighborhood(ctx context.Context, selector string, kind graph.NodeKind, depth int, direction Direction, relations []graph.EdgeKind, limit int) (Traversal, error) {
	root, err := s.ResolveKind(ctx, selector, kind)
	if err != nil {
		return Traversal{}, err
	}
	if depth < 1 {
		depth = 1
	}
	if direction == "" {
		direction = Both
	}
	if direction != Outgoing && direction != Incoming && direction != Both {
		return Traversal{}, fmt.Errorf("invalid direction %q", direction)
	}
	if limit <= 0 {
		limit = 1000
	}
	relationSet := make(map[graph.EdgeKind]bool, len(relations))
	for _, relation := range relations {
		relationSet[relation] = true
	}
	result := Traversal{Root: root, Direction: direction, Nodes: []ReachedNode{{Depth: 0, Node: root}}, Edges: []graph.Edge{}}
	visited := map[string]bool{root.ID: true}
	frontier := []string{root.ID}
	edgeSeen := map[string]bool{}
	for level := 1; level <= depth && len(frontier) > 0; level++ {
		var next []string
		for _, id := range frontier {
			edges, err := s.edges(ctx, id, direction, relationSet)
			if err != nil {
				return Traversal{}, err
			}
			for _, edge := range edges {
				neighbor := edge.ToID
				if edge.ToID == id {
					neighbor = edge.FromID
				}
				if !edgeSeen[edge.ID] {
					result.Edges = append(result.Edges, edge)
					edgeSeen[edge.ID] = true
				}
				if visited[neighbor] {
					continue
				}
				if len(visited) >= limit {
					result.Truncated = true
					continue
				}
				node, err := s.repository.Node(ctx, neighbor)
				if err != nil {
					return Traversal{}, err
				}
				visited[neighbor] = true
				next = append(next, neighbor)
				result.Nodes = append(result.Nodes, ReachedNode{Depth: level, Node: node})
			}
		}
		sort.Strings(next)
		frontier = next
	}
	sort.Slice(result.Nodes, func(i, j int) bool {
		if result.Nodes[i].Depth != result.Nodes[j].Depth {
			return result.Nodes[i].Depth < result.Nodes[j].Depth
		}
		if result.Nodes[i].Node.QualifiedName != result.Nodes[j].Node.QualifiedName {
			return result.Nodes[i].Node.QualifiedName < result.Nodes[j].Node.QualifiedName
		}
		return result.Nodes[i].Node.ID < result.Nodes[j].Node.ID
	})
	sortEdges(result.Edges)
	return result, nil
}

// ShortestPath walks from one resolved node to another. kind optionally
// restricts both endpoints to one node kind.
func (s *Service) ShortestPath(ctx context.Context, fromSelector, toSelector string, kind graph.NodeKind, direction Direction, relations []graph.EdgeKind, limit int) (Path, error) {
	from, err := s.ResolveKind(ctx, fromSelector, kind)
	if err != nil {
		return Path{}, err
	}
	to, err := s.ResolveKind(ctx, toSelector, kind)
	if err != nil {
		return Path{}, err
	}
	if direction == "" {
		direction = Outgoing
	}
	if limit <= 0 {
		limit = 10000
	}
	relationSet := make(map[graph.EdgeKind]bool, len(relations))
	for _, relation := range relations {
		relationSet[relation] = true
	}
	type parentStep struct {
		parent string
		edge   graph.Edge
	}
	parents := map[string]parentStep{}
	visited := map[string]bool{from.ID: true}
	queue := []string{from.ID}
	found := from.ID == to.ID
	for len(queue) > 0 && !found && len(visited) < limit {
		id := queue[0]
		queue = queue[1:]
		edges, err := s.edges(ctx, id, direction, relationSet)
		if err != nil {
			return Path{}, err
		}
		for _, edge := range edges {
			neighbor := edge.ToID
			if edge.ToID == id {
				neighbor = edge.FromID
			}
			if visited[neighbor] {
				continue
			}
			visited[neighbor] = true
			parents[neighbor] = parentStep{parent: id, edge: edge}
			if neighbor == to.ID {
				found = true
				break
			}
			queue = append(queue, neighbor)
		}
	}
	if !found {
		return Path{}, fmt.Errorf("no path from %s to %s", from.QualifiedName, to.QualifiedName)
	}
	ids := []string{to.ID}
	var reversedEdges []graph.Edge
	for current := to.ID; current != from.ID; {
		step := parents[current]
		reversedEdges = append(reversedEdges, step.edge)
		current = step.parent
		ids = append(ids, current)
	}
	reverseStrings(ids)
	reverseEdges(reversedEdges)
	nodes := make([]graph.Node, 0, len(ids))
	for _, id := range ids {
		node, err := s.repository.Node(ctx, id)
		if err != nil {
			return Path{}, err
		}
		nodes = append(nodes, node)
	}
	return Path{From: from, To: to, Nodes: nodes, Edges: reversedEdges}, nil
}

func (s *Service) edges(ctx context.Context, id string, direction Direction, relations map[graph.EdgeKind]bool) ([]graph.Edge, error) {
	var result []graph.Edge
	if direction == Outgoing || direction == Both {
		edges, err := s.repository.EdgesFrom(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, edges...)
	}
	if direction == Incoming || direction == Both {
		edges, err := s.repository.EdgesTo(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, edges...)
	}
	filtered := result[:0]
	seen := map[string]bool{}
	for _, edge := range result {
		if len(relations) > 0 && !relations[edge.Kind] {
			continue
		}
		if !seen[edge.ID] {
			filtered = append(filtered, edge)
			seen[edge.ID] = true
		}
	}
	sortEdges(filtered)
	return filtered, nil
}

func sortEdges(edges []graph.Edge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		if edges[i].FromID != edges[j].FromID {
			return edges[i].FromID < edges[j].FromID
		}
		if edges[i].ToID != edges[j].ToID {
			return edges[i].ToID < edges[j].ToID
		}
		return edges[i].ID < edges[j].ID
	})
}

func reverseStrings(values []string) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func reverseEdges(values []graph.Edge) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}
