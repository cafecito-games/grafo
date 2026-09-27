package query

import (
	"context"
	"sort"

	"github.com/cafecito-games/grafo/internal/graph"
)

const (
	defaultCompositionDepth = 8
	defaultCompositionLimit = 1000
)

// GodotRelation is one Godot composition fact kept with the node on the far
// side of the edge and, when the evidence came from a scene node rather than
// the scene itself, the scene node that carried it.
type GodotRelation struct {
	Edge      graph.Edge  `json:"edge"`
	Node      graph.Node  `json:"node"`
	Via       *graph.Node `json:"via,omitempty"`
	Federated bool        `json:"federated,omitempty"`
}

// GodotComposition answers the runtime composition questions for one Godot
// node: which scenes it instantiates, which scenes instantiate it, which
// scripts are attached where, and which autoload singletons expose it.
//
// Every section is a set of resolved edges with their original evidence, so an
// unresolved or ambiguous reference appears as an external node rather than as
// a guessed edge or as a missing row.
type GodotComposition struct {
	Root               graph.Node      `json:"root"`
	SceneNodes         []graph.Node    `json:"scene_nodes"`
	OutboundInstances  []GodotRelation `json:"outbound_instances"`
	InboundInstances   []GodotRelation `json:"inbound_instances"`
	AttachedScripts    []GodotRelation `json:"attached_scripts"`
	ScriptAttachments  []GodotRelation `json:"script_attachments"`
	AutoloadTargets    []GodotRelation `json:"autoload_targets"`
	AutoloadExposures  []GodotRelation `json:"autoload_exposures"`
	Truncated          bool            `json:"truncated"`
	SceneNodesExplored int             `json:"scene_nodes_explored"`
}

// GodotCompositionOptions bounds a composition report. Zero fields fall back to
// depth 8 for scene-tree traversal and 1000 relations per section. Kind
// restricts selector resolution to one node kind, which is how a caller names
// the scene rather than the script when a scene and its script share a canonical
// identity - they differ only by an extension the identity drops.
type GodotCompositionOptions struct {
	Depth int
	Limit int
	Kind  graph.NodeKind
}

func (o GodotCompositionOptions) withDefaults() GodotCompositionOptions {
	if o.Depth <= 0 {
		o.Depth = defaultCompositionDepth
	}
	if o.Limit <= 0 {
		o.Limit = defaultCompositionLimit
	}
	return o
}

// GodotComposition resolves the selector and returns its deterministic Godot
// composition report. Resolution errors (ErrNotFound, *AmbiguousError)
// propagate unchanged and never yield a partial report.
func (s *Service) GodotComposition(ctx context.Context, selector string, options GodotCompositionOptions) (GodotComposition, error) {
	root, err := s.ResolveKind(ctx, selector, options.Kind)
	if err != nil {
		return GodotComposition{}, err
	}
	options = options.withDefaults()
	report := GodotComposition{
		Root: root, SceneNodes: []graph.Node{}, OutboundInstances: []GodotRelation{},
		InboundInstances: []GodotRelation{}, AttachedScripts: []GodotRelation{},
		ScriptAttachments: []GodotRelation{}, AutoloadTargets: []GodotRelation{},
		AutoloadExposures: []GodotRelation{},
	}

	// A scene owns its tree, so the scene's own composition includes the
	// instances and scripts its nodes declare. Containment is followed only
	// through declares, which is the relation the scene parser emits.
	members := []graph.Node{root}
	if root.Kind == graph.KindGodotScene || root.Kind == graph.KindGodotSceneNode {
		traversal, err := s.Neighborhood(ctx, root.ID, "", options.Depth, Outgoing,
			[]graph.EdgeKind{graph.EdgeDeclares}, options.Limit)
		if err != nil {
			return GodotComposition{}, err
		}
		for _, reached := range traversal.Nodes {
			if reached.Depth == 0 || reached.Node.Kind != graph.KindGodotSceneNode {
				continue
			}
			report.SceneNodes = append(report.SceneNodes, reached.Node)
			members = append(members, reached.Node)
		}
		report.Truncated = report.Truncated || traversal.Truncated
	}
	report.SceneNodesExplored = len(report.SceneNodes)

	scenes := map[string]*graph.Node{}
	for _, member := range members {
		via := &member
		if member.ID == root.ID {
			via = nil
		}
		edges, err := s.repository.EdgesFrom(ctx, member.ID)
		if err != nil {
			return GodotComposition{}, err
		}
		for _, edge := range edges {
			switch edge.Kind {
			case graph.EdgeInstantiates:
				relation, err := s.relation(ctx, edge, edge.ToID, via)
				if err != nil {
					return GodotComposition{}, err
				}
				report.OutboundInstances = append(report.OutboundInstances, relation)
			case graph.EdgeAttachesScript:
				relation, err := s.relation(ctx, edge, edge.ToID, via)
				if err != nil {
					return GodotComposition{}, err
				}
				report.AttachedScripts = append(report.AttachedScripts, relation)
			case graph.EdgeAutoloads:
				relation, err := s.relation(ctx, edge, edge.ToID, via)
				if err != nil {
					return GodotComposition{}, err
				}
				report.AutoloadTargets = append(report.AutoloadTargets, relation)
			}
		}
	}

	incoming, err := s.repository.EdgesTo(ctx, root.ID)
	if err != nil {
		return GodotComposition{}, err
	}
	for _, edge := range incoming {
		switch edge.Kind {
		case graph.EdgeInstantiates, graph.EdgeAttachesScript, graph.EdgeAutoloads:
		default:
			continue
		}
		source, err := s.repository.Node(ctx, edge.FromID)
		if err != nil {
			return GodotComposition{}, err
		}
		relation := GodotRelation{Edge: edge, Node: source, Federated: isFederated(edge)}
		// An instance or attachment declared by a scene node belongs to that
		// node's scene. Reporting the scene as the far side answers "which
		// scenes use this" while the scene node keeps the exact evidence.
		if owner, ok := s.owningScene(ctx, source, scenes); ok {
			via := source
			relation.Node, relation.Via = owner, &via
		}
		switch edge.Kind {
		case graph.EdgeInstantiates:
			report.InboundInstances = append(report.InboundInstances, relation)
		case graph.EdgeAttachesScript:
			report.ScriptAttachments = append(report.ScriptAttachments, relation)
		case graph.EdgeAutoloads:
			report.AutoloadExposures = append(report.AutoloadExposures, relation)
		}
	}

	for _, section := range []*[]GodotRelation{
		&report.OutboundInstances, &report.InboundInstances, &report.AttachedScripts,
		&report.ScriptAttachments, &report.AutoloadTargets, &report.AutoloadExposures,
	} {
		sortGodotRelations(*section)
		if len(*section) > options.Limit {
			*section = (*section)[:options.Limit]
			report.Truncated = true
		}
	}
	return report, nil
}

func (s *Service) relation(ctx context.Context, edge graph.Edge, nodeID string, via *graph.Node) (GodotRelation, error) {
	node, err := s.repository.Node(ctx, nodeID)
	if err != nil {
		return GodotRelation{}, err
	}
	return GodotRelation{Edge: edge, Node: node, Via: via, Federated: isFederated(edge)}, nil
}

// owningScene resolves the scene that declares a scene node. The scene parser
// records the canonical scene identity on every scene node, and the scene is
// accepted only when exactly one non-external scene carries that identity, so
// an ambiguous project never attributes a node to the wrong scene.
func (s *Service) owningScene(ctx context.Context, node graph.Node, cache map[string]*graph.Node) (graph.Node, bool) {
	if node.Kind != graph.KindGodotSceneNode {
		return graph.Node{}, false
	}
	scene := node.Properties["scene"]
	if scene == "" {
		return graph.Node{}, false
	}
	if cached, ok := cache[scene]; ok {
		if cached == nil {
			return graph.Node{}, false
		}
		return *cached, true
	}
	candidates, err := s.repository.SearchNodes(ctx, scene, 50)
	if err != nil {
		cache[scene] = nil
		return graph.Node{}, false
	}
	var matches []graph.Node
	for _, candidate := range candidates {
		if candidate.External || candidate.Kind != graph.KindGodotScene || candidate.QualifiedName != scene {
			continue
		}
		matches = append(matches, candidate)
	}
	if len(matches) != 1 {
		cache[scene] = nil
		return graph.Node{}, false
	}
	cache[scene] = &matches[0]
	return matches[0], true
}

func sortGodotRelations(relations []GodotRelation) {
	sort.Slice(relations, func(i, j int) bool {
		if relations[i].Node.QualifiedName != relations[j].Node.QualifiedName {
			return relations[i].Node.QualifiedName < relations[j].Node.QualifiedName
		}
		if relations[i].Edge.Kind != relations[j].Edge.Kind {
			return relations[i].Edge.Kind < relations[j].Edge.Kind
		}
		return relations[i].Edge.ID < relations[j].Edge.ID
	})
}
