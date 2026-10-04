package query

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

// GodotInteractionCategory names one class of Godot gameplay interaction. The
// three categories are the filter vocabulary a caller narrows a report with, and
// an unknown value is an error rather than a filter that can never match.
type GodotInteractionCategory string

const (
	// GodotActionInteraction covers input-action declarations and uses.
	GodotActionInteraction GodotInteractionCategory = "action"
	// GodotGroupInteraction covers node-group membership, lookups, and
	// dispatches.
	GodotGroupInteraction GodotInteractionCategory = "group"
	// GodotSignalInteraction covers signal routing, whether a scene declared it
	// or a script established it.
	GodotSignalInteraction GodotInteractionCategory = "signal"
)

// GodotInteractionCategories returns the category vocabulary in report order.
func GodotInteractionCategories() []GodotInteractionCategory {
	return []GodotInteractionCategory{GodotActionInteraction, GodotGroupInteraction, GodotSignalInteraction}
}

// ParseGodotInteractionCategory validates a caller-supplied category. An empty
// value parses to the empty category, which every filter treats as "any".
func ParseGodotInteractionCategory(value string) (GodotInteractionCategory, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return "", nil
	}
	for _, candidate := range GodotInteractionCategories() {
		if string(candidate) == trimmed {
			return candidate, nil
		}
	}
	names := make([]string, 0, 3)
	for _, candidate := range GodotInteractionCategories() {
		names = append(names, string(candidate))
	}
	return "", fmt.Errorf("unknown Godot interaction filter %q; expected one of %s",
		value, strings.Join(names, ", "))
}

// GodotInteraction is one gameplay-interaction fact kept with the node on the far
// side of the edge, the category it belongs to, and the operation form the
// producer recorded as evidence.
//
// Via is the scene node that carried the evidence when the report was asked about
// the scene rather than the node, exactly as in a composition report. An
// unresolved far side stays an external node so a missing declaration or a
// signal whose owner could not be proved is visible rather than absent.
type GodotInteraction struct {
	Category  GodotInteractionCategory `json:"category"`
	Form      string                   `json:"form,omitempty"`
	Direction Direction                `json:"direction"`
	Edge      graph.Edge               `json:"edge"`
	Node      graph.Node               `json:"node"`
	Via       *graph.Node              `json:"via,omitempty"`
	Federated bool                     `json:"federated,omitempty"`
}

// GodotInteractions is the gameplay-interaction report for one Godot node: the
// input actions it uses, the node groups it joins, leaves, inspects, and
// dispatches to, and the signal routes it takes part in, in both directions.
//
// It is a sibling of GodotComposition rather than an extension of it. Composition
// answers what a scene is built from; interactions answer how it is wired at
// runtime, and the two sets of edges are disjoint.
type GodotInteractions struct {
	Root       graph.Node         `json:"root"`
	SceneNodes []graph.Node       `json:"scene_nodes"`
	Outbound   []GodotInteraction `json:"outbound"`
	Inbound    []GodotInteraction `json:"inbound"`
	// Members is the declaration expansion behind a report whose root owns no
	// wiring of its own - a file, a script module, or a class - because GDScript
	// records every operation on the member that performs it. It is absent for a
	// scene, which answers for itself.
	Members *MemberAggregation `json:"members,omitempty"`
	// Unresolved counts the interactions whose far side is an unresolved
	// boundary node, so a caller can tell a fully wired report from one that
	// merely looks wired.
	Unresolved         int  `json:"unresolved"`
	Truncated          bool `json:"truncated"`
	SceneNodesExplored int  `json:"scene_nodes_explored"`
}

// GodotInteractionsOptions bounds an interactions report. Zero fields fall back
// to depth 8 for scene-tree traversal, 1000 interactions per direction, both
// directions, and every category.
type GodotInteractionsOptions struct {
	Depth      int
	Limit      int
	Kind       graph.NodeKind
	Direction  Direction
	Categories []GodotInteractionCategory
}

func (o GodotInteractionsOptions) withDefaults() GodotInteractionsOptions {
	if o.Depth <= 0 {
		o.Depth = defaultCompositionDepth
	}
	if o.Limit <= 0 {
		o.Limit = defaultCompositionLimit
	}
	if o.Direction != Outgoing && o.Direction != Incoming {
		o.Direction = Both
	}
	return o
}

func (o GodotInteractionsOptions) wants(category GodotInteractionCategory) bool {
	if len(o.Categories) == 0 {
		return true
	}
	for _, candidate := range o.Categories {
		if candidate == category {
			return true
		}
	}
	return false
}

// GodotInteractions resolves the selector and returns its deterministic Godot
// interaction report. Resolution errors (ErrNotFound, *AmbiguousError) propagate
// unchanged and never yield a partial report.
func (s *Service) GodotInteractions(ctx context.Context, selector string, options GodotInteractionsOptions) (GodotInteractions, error) {
	options = options.withDefaults()
	// A scene is wired through the nodes it declares, so the scene's own
	// interactions include theirs, and a container root is wired through the
	// members that perform the operations. Containment is followed only through
	// declares, which is the relation the parsers emit.
	evidence, err := s.godotEvidenceFor(ctx, selector, options.Kind, options.Depth, options.Limit)
	if err != nil {
		return GodotInteractions{}, err
	}
	root := evidence.root
	report := GodotInteractions{Root: root, SceneNodes: evidence.sceneNodes,
		Outbound: []GodotInteraction{}, Inbound: []GodotInteraction{},
		Members: evidence.aggregation(), Truncated: evidence.truncated,
		SceneNodesExplored: len(evidence.sceneNodes)}

	if options.Direction == Outgoing || options.Direction == Both {
		for _, member := range evidence.sources() {
			via := &member
			if member.ID == root.ID {
				via = nil
			}
			edges, err := s.repository.EdgesFrom(ctx, member.ID)
			if err != nil {
				return GodotInteractions{}, err
			}
			for _, edge := range edges {
				interaction, ok, err := s.godotInteraction(ctx, edge, member.Kind, edge.ToID, via, Outgoing, options)
				if err != nil {
					return GodotInteractions{}, err
				}
				if ok {
					report.Outbound = append(report.Outbound, interaction)
				}
			}
		}
	}

	if options.Direction == Incoming || options.Direction == Both {
		scenes := map[string]*graph.Node{}
		for _, target := range evidence.targets() {
			incoming, err := s.repository.EdgesTo(ctx, target.ID)
			if err != nil {
				return GodotInteractions{}, err
			}
			for _, edge := range incoming {
				// An edge from one of the root's own declarations is internal
				// wiring, already reported outbound, rather than something
				// outside reaching in.
				if evidence.internal(edge.FromID) {
					continue
				}
				interaction, ok, err := s.godotInteraction(ctx, edge, target.Kind, edge.FromID, nil, Incoming, options)
				if err != nil {
					return GodotInteractions{}, err
				}
				if !ok {
					continue
				}
				// An interaction declared by a scene node belongs to that node's
				// scene. Reporting the scene as the far side answers "which scenes
				// use this" while the scene node keeps the exact evidence.
				if owner, found := s.owningScene(ctx, interaction.Node, scenes); found {
					via := interaction.Node
					interaction.Node, interaction.Via = owner, &via
				}
				report.Inbound = append(report.Inbound, interaction)
			}
		}
	}

	for _, section := range []*[]GodotInteraction{&report.Outbound, &report.Inbound} {
		sortGodotInteractions(*section)
		if len(*section) > options.Limit {
			*section = (*section)[:options.Limit]
			report.Truncated = true
		}
	}
	for _, section := range [][]GodotInteraction{report.Outbound, report.Inbound} {
		for _, interaction := range section {
			if interaction.Node.External {
				report.Unresolved++
			}
		}
	}
	return report, nil
}

func (s *Service) godotInteraction(ctx context.Context, edge graph.Edge, near graph.NodeKind, nodeID string, via *graph.Node, direction Direction, options GodotInteractionsOptions) (GodotInteraction, bool, error) {
	if !godotInteractionEdge(edge.Kind) {
		return GodotInteraction{}, false, nil
	}
	node, err := s.repository.Node(ctx, nodeID)
	if err != nil {
		return GodotInteraction{}, false, err
	}
	category, ok := godotInteractionCategory(edge, near, node.Kind)
	if !ok || !options.wants(category) {
		return GodotInteraction{}, false, nil
	}
	return GodotInteraction{Category: category, Form: edge.Properties["form"], Direction: direction,
		Edge: edge, Node: node, Via: via, Federated: isFederated(edge)}, true, nil
}

// godotInteractionEdge reports whether an edge kind can carry an interaction, so
// a node with thousands of unrelated edges is not loaded to find out.
func godotInteractionEdge(kind graph.EdgeKind) bool {
	switch kind {
	case graph.EdgeUsesInputAction, graph.EdgeInGroup, graph.EdgeUsesGroup,
		graph.EdgePublishes, graph.EdgeSubscribes, graph.EdgeHandledBy,
		graph.EdgeReferences, graph.EdgeDefines:
		return true
	default:
		return false
	}
}

// godotSignalForms are the operation forms accepted after explicit producer
// provenance has established that a Godot parser created the edge.
//
// Provenance has to be proved rather than assumed, because events and the
// publishes, subscribes, and handled_by relations are shared vocabulary: the Go,
// Python, TypeScript, Java, and Swift extractors all emit them. Classifying them
// as Godot gameplay wiring on kind alone would answer a question about a Go
// message bus with fabricated Godot interactions, which is worse than an empty
// report. A matching form remains necessary operation evidence but is never a
// substitute for Edge.Producer.
var godotSignalForms = map[string]bool{
	"emit":                   true,
	"connect":                true,
	"signal_disconnect":      true,
	"signal_connection_test": true,
}

// godotInteractionCategory classifies one edge. The action and group relations
// are Godot-only edge kinds and name their category outright. The signal
// relations are shared with every other event producer, so they qualify only
// with Godot provenance. References and defines are generic relations shared with
// every producer, so they qualify only when one of their two endpoints is a
// Godot-only interaction kind - or, for an event, when the same provenance holds.
// Both endpoints are considered because either can be the interaction: a script
// references a signal, while a configuration key defines the action it declares.
// That is explicit kind and provenance compatibility rather than a name-prefix
// heuristic, which is what keeps a configuration key named "input/jump" out of
// the action section while the action itself stays in.
func godotInteractionCategory(edge graph.Edge, near, far graph.NodeKind) (GodotInteractionCategory, bool) {
	if edge.Producer != graph.ProducerGDScript && edge.Producer != graph.ProducerGodot {
		return "", false
	}
	switch edge.Kind {
	case graph.EdgeUsesInputAction:
		return GodotActionInteraction, true
	case graph.EdgeInGroup, graph.EdgeUsesGroup:
		return GodotGroupInteraction, true
	case graph.EdgePublishes, graph.EdgeSubscribes, graph.EdgeHandledBy:
		if !godotSignalForms[edge.Properties["form"]] {
			return "", false
		}
		return GodotSignalInteraction, true
	case graph.EdgeReferences, graph.EdgeDefines:
		for _, kind := range []graph.NodeKind{far, near} {
			switch kind {
			case graph.KindGodotInputAction:
				return GodotActionInteraction, true
			case graph.KindGodotNodeGroup:
				return GodotGroupInteraction, true
			case graph.KindEvent:
				if !godotSignalForms[edge.Properties["form"]] {
					return "", false
				}
				return GodotSignalInteraction, true
			}
		}
	}
	return "", false
}

func sortGodotInteractions(interactions []GodotInteraction) {
	sort.Slice(interactions, func(i, j int) bool {
		a, b := interactions[i], interactions[j]
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Node.QualifiedName != b.Node.QualifiedName {
			return a.Node.QualifiedName < b.Node.QualifiedName
		}
		if a.Form != b.Form {
			return a.Form < b.Form
		}
		if a.Edge.Kind != b.Edge.Kind {
			return a.Edge.Kind < b.Edge.Kind
		}
		return a.Edge.ID < b.Edge.ID
	})
}
