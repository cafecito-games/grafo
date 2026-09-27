package query

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

type Direction string

const (
	Outgoing Direction = "outgoing"
	Incoming Direction = "incoming"
	Both     Direction = "both"
)

type AmbiguousError struct {
	Term       string
	Candidates []graph.Node
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q matches %d nodes; use a qualified name or node ID", e.Term, len(e.Candidates))
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

func (s *Service) Resolve(ctx context.Context, selector string) (graph.Node, error) {
	selector = strings.TrimSpace(selector)
	if strings.HasPrefix(selector, "n:") {
		if node, err := s.repository.Node(ctx, selector); err == nil {
			return node, nil
		}
	}
	candidates, err := s.repository.SearchNodes(ctx, selector, 25)
	if err != nil {
		return graph.Node{}, err
	}
	if len(candidates) == 0 {
		return graph.Node{}, fmt.Errorf("%w: %s", ErrNotFound, selector)
	}
	var exactQualified, exactName []graph.Node
	for _, node := range candidates {
		if strings.EqualFold(node.QualifiedName, selector) {
			exactQualified = append(exactQualified, node)
		}
		if strings.EqualFold(node.Name, selector) {
			exactName = append(exactName, node)
		}
	}
	if len(exactQualified) == 1 {
		return exactQualified[0], nil
	}
	if len(exactName) == 1 {
		return exactName[0], nil
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	ambiguous := candidates
	if len(exactQualified) > 1 {
		ambiguous = exactQualified
	} else if len(exactName) > 1 {
		ambiguous = exactName
	}
	return graph.Node{}, &AmbiguousError{Term: selector, Candidates: ambiguous}
}

func (s *Service) Neighborhood(ctx context.Context, selector string, depth int, direction Direction, relations []graph.EdgeKind, limit int) (Traversal, error) {
	root, err := s.Resolve(ctx, selector)
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

func (s *Service) ShortestPath(ctx context.Context, fromSelector, toSelector string, direction Direction, relations []graph.EdgeKind, limit int) (Path, error) {
	from, err := s.Resolve(ctx, fromSelector)
	if err != nil {
		return Path{}, err
	}
	to, err := s.Resolve(ctx, toSelector)
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
