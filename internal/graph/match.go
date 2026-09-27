package graph

import (
	"sort"
	"strings"
)

// NodeMatchLevel names one class of evidence that a selector matched a node.
// Levels are ordered: a qualified-name match is stronger evidence than a bare
// name match, which is stronger than a substring match. Resolution never mixes
// levels, so a weaker match can never make a stronger one ambiguous.
type NodeMatchLevel string

const (
	// MatchNone is the zero level, reported when a selector matched nothing.
	MatchNone NodeMatchLevel = ""
	// MatchQualifiedName means qualified_name equals the selector.
	MatchQualifiedName NodeMatchLevel = "qualified_name"
	// MatchName means name equals the selector.
	MatchName NodeMatchLevel = "name"
	// MatchSubstring means name or qualified_name contains the selector.
	MatchSubstring NodeMatchLevel = "substring"
)

// Rank orders levels from strongest to weakest. MatchNone ranks last so an
// empty group never displaces a real one.
func (l NodeMatchLevel) Rank() int {
	switch l {
	case MatchQualifiedName:
		return 0
	case MatchName:
		return 1
	case MatchSubstring:
		return 2
	default:
		return 3
	}
}

// Stronger reports whether l is better evidence than other.
func (l NodeMatchLevel) Stronger(other NodeMatchLevel) bool { return l.Rank() < other.Rank() }

// NodeMatchQuery asks an adapter for the match evidence behind one selector.
// Kind is an optional filter: an empty kind matches any kind. Limit bounds only
// the returned node list, never the reported totals, so a caller can always
// tell that a list was truncated.
type NodeMatchQuery struct {
	Selector string   `json:"selector"`
	Kind     NodeKind `json:"kind,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

// NodeMatchGroup is the complete evidence for a selector at one level.
//
// Adapters must honor three rules, because selector resolution is only sound
// if they hold:
//
//   - Level is the strongest level that matched anything. Weaker levels are not
//     reported, so a substring match never competes with an exact name match.
//   - Total and Strict count every match at that level in the graph, whether or
//     not it fits in Nodes. Truncation must therefore be observable, and a
//     truncated list can never silently narrow a match set.
//   - Nodes lists case-sensitive ("strict") matches first and is otherwise
//     deterministically ordered, so the first Strict entries of Nodes are
//     exactly the strict matches whenever Strict <= len(Nodes).
type NodeMatchGroup struct {
	Level  NodeMatchLevel `json:"level"`
	Nodes  []Node         `json:"nodes"`
	Strict int            `json:"strict"`
	Total  int            `json:"total"`
}

// Truncated reports whether the graph holds matches that Nodes omits.
func (g NodeMatchGroup) Truncated() bool { return g.Total > len(g.Nodes) }

// StrictMatch reports whether node matches selector case-sensitively at level.
// Adapters compare case-insensitively so a caller can see near misses; this
// predicate is the single definition of the stronger, case-sensitive form, and
// every adapter and use case must agree on it.
func StrictMatch(level NodeMatchLevel, selector string, node Node) bool {
	switch level {
	case MatchQualifiedName:
		return node.QualifiedName == selector
	case MatchName:
		return node.Name == selector
	case MatchSubstring:
		return strings.Contains(node.QualifiedName, selector) || strings.Contains(node.Name, selector)
	default:
		return false
	}
}

// LooseMatch reports whether node matches selector at level ignoring ASCII
// case. It mirrors the SQL predicates behind MatchNodes so in-memory adapters
// and fakes can implement the same contract.
func LooseMatch(level NodeMatchLevel, selector string, node Node) bool {
	folded := strings.ToLower(selector)
	switch level {
	case MatchQualifiedName:
		return strings.ToLower(node.QualifiedName) == folded
	case MatchName:
		return strings.ToLower(node.Name) == folded
	case MatchSubstring:
		return strings.Contains(strings.ToLower(node.QualifiedName), folded) ||
			strings.Contains(strings.ToLower(node.Name), folded)
	default:
		return false
	}
}

// SortNodeMatches applies the ordering NodeMatchGroup documents: strict matches
// first, then local nodes before external boundary nodes, then shortest
// qualified name, then qualified name and ID for stability.
func SortNodeMatches(level NodeMatchLevel, selector string, nodes []Node) {
	strictOf := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		strictOf[node.ID] = StrictMatch(level, selector, node)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if strictOf[a.ID] != strictOf[b.ID] {
			return strictOf[a.ID]
		}
		if a.External != b.External {
			return !a.External
		}
		if len(a.QualifiedName) != len(b.QualifiedName) {
			return len(a.QualifiedName) < len(b.QualifiedName)
		}
		if a.QualifiedName != b.QualifiedName {
			return a.QualifiedName < b.QualifiedName
		}
		return a.ID < b.ID
	})
}
