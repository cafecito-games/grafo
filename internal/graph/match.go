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

// memberKinds are the node kinds that exist only as part of a declaration: a
// function's parameters and local variables, a type's fields, and a table's
// columns. Every parser builds their qualified name by extending the declaring
// symbol's, so a selector naming the parent also matches them, and selector
// resolution may drop them in favour of that parent.
//
// The rule for classifying a kind, for whoever extends the vocabulary: a kind
// belongs here only if both of these hold.
//
//  1. Every parser that emits it builds its qualified name by extending the
//     declaring symbol's. Check the emitting parser rather than reasoning from
//     the name. KindIndex is the counter-example: internal/parser/sql/sqlite and
//     .../postgres give an index its own top-level qualified name, not
//     "table.index", so it never nests in the first place.
//  2. It cannot be referenced on its own bare name, only through its parent.
//     KindDocSection and KindConfigKey are the counter-examples: markdown links
//     resolve "path#anchor" as a target and config references resolve a bare key,
//     so both are declarations that merely happen to nest. KindEvent is another:
//     a GDScript signal's qualified name extends its class's, but signals are
//     connected and emitted by their bare name.
//
// When in doubt, leave the kind out. A wrongly excluded kind costs an ambiguity
// error, which a caller resolves with a qualified name or a kind filter; a
// wrongly included one silently suppresses a real declaration, which is the
// defect this resolution path exists to prevent.
//
// Every other kind is a declaration in its own right even when its qualified name
// nests under another candidate's. Go allows "type Charge struct{}" beside
// "func (Charge) Charge()", whose qualified names are pkg.Charge and
// pkg.Charge.Charge: two distinct declarations sharing one name, and a selector
// naming both must stay ambiguous rather than silently picking the outer one.
var memberKinds = map[NodeKind]bool{
	KindParameter: true,
	KindVariable:  true,
	KindField:     true,
	KindColumn:    true,
}

// IsDeclarationMember reports whether kind only ever exists as part of a
// declaration. Only such a node may be treated as a sub-part of the symbol whose
// qualified name it extends; anything else is a rival declaration. See
// memberKinds for the rule that decides which bucket a kind belongs in.
func IsDeclarationMember(kind NodeKind) bool { return memberKinds[kind] }

// NodeMatchGroup is the complete evidence for a selector at one level.
//
// Adapters must honor three rules, because selector resolution is only sound
// if they hold:
//
//   - The group is the strongest evidence that matched anything, ranked by
//     StrongerThan: a case-sensitive match outranks a case-insensitive-only one at
//     any level, then a stronger level wins, then a local declaration beats an
//     external boundary node. Weaker evidence is not reported, so a substring
//     match never competes with an exact name match and an unresolved boundary
//     node never competes with a real declaration.
//   - Total and Strict count every match at that level in the graph, whether or
//     not it fits in Nodes. Truncation must therefore be observable, and a
//     truncated list can never silently narrow a match set.
//   - Nodes lists case-sensitive ("strict") matches first and is otherwise
//     deterministically ordered, so the first Strict entries of Nodes are
//     exactly the strict matches whenever Strict <= len(Nodes).
type NodeMatchGroup struct {
	Level NodeMatchLevel `json:"level"`
	// External marks a group that holds external boundary nodes because no local
	// declaration matched at this level. It makes the group weaker evidence than a
	// local group at the same level, which matters when merging federated members.
	External bool   `json:"external,omitempty"`
	Nodes    []Node `json:"nodes"`
	Strict   int    `json:"strict"`
	Total    int    `json:"total"`
}

// StrongerThan reports whether g is better evidence than other. The ranking is,
// in order: a group holding at least one case-sensitive match beats one holding
// none, because a case-insensitive-only match is a weaker form of evidence and
// must not outrank a case-sensitive match at a weaker level - a folded
// qualified-name hit on "path" must not beat the nodes named exactly "Path"; then
// a stronger level wins; then a local group beats an external fallback. An empty
// group is never stronger than anything.
func (g NodeMatchGroup) StrongerThan(other NodeMatchGroup) bool {
	if g.Total == 0 {
		return false
	}
	if other.Total == 0 {
		return true
	}
	if (g.Strict > 0) != (other.Strict > 0) {
		return g.Strict > 0
	}
	if g.Level != other.Level {
		return g.Level.Stronger(other.Level)
	}
	return !g.External && other.External
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
