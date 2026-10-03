package query

import (
	"context"
	"sort"

	"github.com/cafecito-games/grafo/internal/graph"
)

// memberRelation is the declaration relation that connects a type-level root to
// the members that carry its call and test evidence. It is deliberately not one
// of the impact relations: it is followed exactly one level, outward from the
// root, so a report gains its own members without gaining every sibling in the
// declaring file.
const memberRelation = graph.EdgeDeclares

// MemberAggregation records the bounded member expansion behind a report whose
// root is a declaration container. It is reported separately from a traversal's
// own truncation flag so a caller can tell an incomplete member list from an
// incomplete walk of a complete one.
type MemberAggregation struct {
	Relation  graph.EdgeKind `json:"relation"`
	Members   []graph.Node   `json:"members"`
	Truncated bool           `json:"truncated"`
}

// declaredMember is one callable a type-level root declares, kept with the
// declaration edge that proves the membership so an aggregated answer can show
// the path from the type to the evidence instead of asserting it.
type declaredMember struct {
	node graph.Node
	edge graph.Edge
}

// aggregatesMembers reports whether a root is a declaration container whose own
// relations under-report what a change to it would affect. A class is a natural
// root for "what breaks if I change this type?", but the callers and tests that
// answer that question reach its methods, not the class declaration, so a
// type-level report has to aggregate them. A method, function, or value already
// answers for itself.
func aggregatesMembers(node graph.Node) bool {
	switch node.Kind {
	case graph.KindClass, graph.KindType, graph.KindInterface:
		return !node.External
	default:
		return false
	}
}

// aggregatedMemberKind reports whether a declared member carries call and test
// evidence on the declaring type's behalf. Nested classes are excluded because
// they are type-level roots in their own right, and fields, parameters, and
// variables are reached through the member that declares them.
func aggregatedMemberKind(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindMethod, graph.KindFunction:
		return true
	default:
		return false
	}
}

// declaredMembers returns the callables a type-level root declares, ordered
// deterministically, together with whether the list was cut short by limit. A
// root that aggregates nothing yields no members and no truncation, so callers
// can treat the empty result as complete.
func (s *Service) declaredMembers(ctx context.Context, root graph.Node, limit int) ([]declaredMember, bool, error) {
	if !aggregatesMembers(root) {
		return nil, false, nil
	}
	edges, err := s.repository.EdgesFrom(ctx, root.ID)
	if err != nil {
		return nil, false, err
	}
	sortEdges(edges)
	members := []declaredMember{}
	seen := map[string]bool{}
	truncated := false
	for _, edge := range edges {
		if edge.Kind != memberRelation || edge.FromID != root.ID || seen[edge.ToID] {
			continue
		}
		node, err := s.repository.Node(ctx, edge.ToID)
		if err != nil {
			return nil, false, err
		}
		if node.External || !aggregatedMemberKind(node.Kind) {
			continue
		}
		seen[edge.ToID] = true
		if limit > 0 && len(members) >= limit {
			truncated = true
			continue
		}
		members = append(members, declaredMember{node: node, edge: edge})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].node.QualifiedName != members[j].node.QualifiedName {
			return members[i].node.QualifiedName < members[j].node.QualifiedName
		}
		return members[i].node.ID < members[j].node.ID
	})
	return members, truncated, nil
}

// memberAggregation describes an expansion for a report. It returns nil when
// the root declares nothing to aggregate, so an answer about a method never
// carries an empty member section.
func memberAggregation(members []declaredMember, truncated bool) *MemberAggregation {
	if len(members) == 0 && !truncated {
		return nil
	}
	nodes := make([]graph.Node, 0, len(members))
	for _, member := range members {
		nodes = append(nodes, member.node)
	}
	return &MemberAggregation{Relation: memberRelation, Members: nodes, Truncated: truncated}
}
