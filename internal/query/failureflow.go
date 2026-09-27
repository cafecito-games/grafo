package query

import (
	"context"
	"sort"

	"github.com/cafecito-games/grafo/internal/graph"
)

// FailureFlowFact keeps one failure or cleanup relation together with its far
// side and recognition evidence. Direction is outgoing when the selected root
// produced the fact and incoming when another symbol points at the root.
type FailureFlowFact struct {
	Direction   Direction  `json:"direction"`
	Form        string     `json:"form,omitempty"`
	Conditional bool       `json:"conditional,omitempty"`
	Unresolved  bool       `json:"unresolved,omitempty"`
	Federated   bool       `json:"federated,omitempty"`
	Edge        graph.Edge `json:"edge"`
	Node        graph.Node `json:"node"`
}

// FailureFlow separates a callable's typed error-return contract from actual
// escaping failures, handlers, panic/recovery sites, and deferred cleanup.
// Incoming relations are included as well, so selecting an error identity or
// callee answers which functions return, propagate, or handle it.
type FailureFlow struct {
	Root            graph.Node        `json:"root"`
	ErrorReturns    []FailureFlowFact `json:"error_returns"`
	Escaping        []FailureFlowFact `json:"escaping"`
	Handled         []FailureFlowFact `json:"handled"`
	Panics          []FailureFlowFact `json:"panics"`
	Recoveries      []FailureFlowFact `json:"recoveries"`
	DeferredCleanup []FailureFlowFact `json:"deferred_cleanup"`
	Unresolved      int               `json:"unresolved"`
	Truncated       bool              `json:"truncated"`
}

type FailureFlowOptions struct {
	Kind      graph.NodeKind
	Direction Direction
	Limit     int
}

func (o FailureFlowOptions) withDefaults() FailureFlowOptions {
	if o.Direction != Incoming && o.Direction != Outgoing {
		o.Direction = Both
	}
	if o.Limit <= 0 {
		o.Limit = 1000
	}
	return o
}

// FailureFlow resolves selector and groups only the versioned failure-flow
// vocabulary already present in the graph. It never re-parses source.
func (s *Service) FailureFlow(ctx context.Context, selector string, options FailureFlowOptions) (FailureFlow, error) {
	root, err := s.ResolveKind(ctx, selector, options.Kind)
	if err != nil {
		return FailureFlow{}, err
	}
	options = options.withDefaults()
	report := FailureFlow{
		Root: root, ErrorReturns: []FailureFlowFact{}, Escaping: []FailureFlowFact{},
		Handled: []FailureFlowFact{}, Panics: []FailureFlowFact{}, Recoveries: []FailureFlowFact{},
		DeferredCleanup: []FailureFlowFact{},
	}
	if options.Direction == Outgoing || options.Direction == Both {
		edges, err := s.repository.EdgesFrom(ctx, root.ID)
		if err != nil {
			return FailureFlow{}, err
		}
		for _, edge := range edges {
			if err := s.addFailureFlowFact(ctx, &report, edge, edge.ToID, Outgoing); err != nil {
				return FailureFlow{}, err
			}
		}
	}
	if options.Direction == Incoming || options.Direction == Both {
		edges, err := s.repository.EdgesTo(ctx, root.ID)
		if err != nil {
			return FailureFlow{}, err
		}
		for _, edge := range edges {
			if err := s.addFailureFlowFact(ctx, &report, edge, edge.FromID, Incoming); err != nil {
				return FailureFlow{}, err
			}
		}
	}
	for _, section := range []*[]FailureFlowFact{
		&report.ErrorReturns, &report.Escaping, &report.Handled, &report.Panics,
		&report.Recoveries, &report.DeferredCleanup,
	} {
		sortFailureFlowFacts(*section)
		if len(*section) > options.Limit {
			*section = (*section)[:options.Limit]
			report.Truncated = true
		}
		for _, fact := range *section {
			if fact.Unresolved {
				report.Unresolved++
			}
		}
	}
	return report, nil
}

func (s *Service) addFailureFlowFact(ctx context.Context, report *FailureFlow, edge graph.Edge, nodeID string, direction Direction) error {
	section := failureFlowSection(report, edge.Kind)
	if section == nil {
		return nil
	}
	node, err := s.repository.Node(ctx, nodeID)
	if err != nil {
		return err
	}
	fact := FailureFlowFact{
		Direction: direction, Form: edge.Properties["form"],
		Conditional: edge.Properties["conditional"] == "true",
		// External is a storage boundary, not a confidence judgment. Standard
		// library and builtin identities are external to the indexed repository
		// but can still be proven exactly by a semantic extractor.
		Unresolved: edge.Properties["unresolved"] == "true",
		Federated:  isFederated(edge), Edge: edge, Node: node,
	}
	*section = append(*section, fact)
	return nil
}

func failureFlowSection(report *FailureFlow, kind graph.EdgeKind) *[]FailureFlowFact {
	switch kind {
	case graph.EdgeReturnsError:
		return &report.ErrorReturns
	case graph.EdgePropagatesError, graph.EdgeWrapsError:
		return &report.Escaping
	case graph.EdgeHandlesError:
		return &report.Handled
	case graph.EdgePanics:
		return &report.Panics
	case graph.EdgeRecovers:
		return &report.Recoveries
	case graph.EdgeDefers:
		return &report.DeferredCleanup
	default:
		return nil
	}
}

func sortFailureFlowFacts(facts []FailureFlowFact) {
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].Direction != facts[j].Direction {
			return facts[i].Direction < facts[j].Direction
		}
		if facts[i].Edge.Kind != facts[j].Edge.Kind {
			return facts[i].Edge.Kind < facts[j].Edge.Kind
		}
		if facts[i].Node.QualifiedName != facts[j].Node.QualifiedName {
			return facts[i].Node.QualifiedName < facts[j].Node.QualifiedName
		}
		return facts[i].Edge.ID < facts[j].Edge.ID
	})
}
