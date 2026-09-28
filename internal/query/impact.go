package query

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

// ImpactDirection names the two halves of a change-impact report. Upstream
// answers "who depends on the root" (incoming traversal); downstream answers
// "what does the root depend on" (outgoing traversal).
type ImpactDirection string

const (
	Upstream   ImpactDirection = "upstream"
	Downstream ImpactDirection = "downstream"
)

const (
	defaultImpactDepth        = 4
	defaultImpactLimit        = 1000
	defaultSourceContextLines = 2
	defaultSourceMaxLines     = 200
	defaultSourceLimit        = 10
)

// federatedProperty marks an edge that crosses a repository boundary. See
// internal/federation/repository.go (federatedEdge), which sets it.
const federatedProperty = "federated"

// impactRelations is the single relation vocabulary used by change-impact
// reports. It starts from the incoming-only blast-radius list that
// internal/mcpserver (getBlastRadius) and internal/cli (neighbors mode
// "impact") used to keep separately, and adds the declaration-style relations
// that connect code to configuration, endpoints, and events
// (reads_config, defines, exposes, publishes, subscribes) so those facts are
// reachable from either side of the report.
//
// Upstream and downstream deliberately share the same set: every relation here
// is meaningful in reverse, so the direction of traversal - not a different
// vocabulary - decides whether an edge means "depends on the root" or "the
// root depends on this". Pure containment relations (contains, declares,
// has_field, documents) are excluded from both: they describe where a symbol
// lives rather than what would break if it changed, and including them floods
// a report with every sibling in a file.
var impactRelations = []graph.EdgeKind{
	graph.EdgeCalls,
	graph.EdgeHandledBy,
	graph.EdgeImports,
	graph.EdgeExtends,
	graph.EdgeImplements,
	graph.EdgeEmbeds,
	graph.EdgeReferences,
	graph.EdgeReads,
	graph.EdgeWrites,
	graph.EdgeEncodes,
	graph.EdgeDecodes,
	graph.EdgeAssigns,
	graph.EdgeReturns,
	graph.EdgePasses,
	graph.EdgeRequests,
	graph.EdgeDependsOn,
	graph.EdgeReadsConfig,
	graph.EdgeDefines,
	graph.EdgeExposes,
	graph.EdgePublishes,
	graph.EdgeSubscribes,
}

// UpstreamRelations returns the relations traversed incoming from the root to
// find code that depends on it. Callers must not mutate the result.
func UpstreamRelations() []graph.EdgeKind {
	return append([]graph.EdgeKind(nil), impactRelations...)
}

// DownstreamRelations returns the relations traversed outgoing from the root to
// find what the root itself depends on. It mirrors UpstreamRelations; see
// impactRelations for why the two sets are identical.
func DownstreamRelations() []graph.EdgeKind {
	return append([]graph.EdgeKind(nil), impactRelations...)
}

// SourceExcerpt is a bounded snippet of the worktree backing one node.
type SourceExcerpt struct {
	NodeID     string `json:"node_id"`
	Repository string `json:"repository,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Path       string `json:"path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`
}

// SourceReader is the narrow port used to attach bounded source excerpts to a
// report. internal/source imports internal/query, so this package must not
// import it back; the CLI and MCP layers supply an adapter over
// source.Service instead. A node ID is a valid source selector, which makes
// that adapter trivial.
type SourceReader interface {
	ReadNodeSource(ctx context.Context, nodeID string, contextLines, maxLines int) (SourceExcerpt, error)
}

// WithSourceReader returns a copy of the service that can attach source
// excerpts. The receiver is left unchanged so a shared service stays safe for
// concurrent use.
func (s *Service) WithSourceReader(reader SourceReader) *Service {
	clone := *s
	clone.source = reader
	return &clone
}

// ImpactOptions bounds a report. Zero fields fall back to documented defaults:
// depth 4 and limit 1000 per direction, 2 context lines, 200 max source lines,
// and at most 10 excerpts.
type ImpactOptions struct {
	// Kind optionally restricts which node the selector may resolve to, so a
	// caller can say it means a method rather than one of its parameters.
	Kind               graph.NodeKind
	UpstreamDepth      int
	DownstreamDepth    int
	UpstreamLimit      int
	DownstreamLimit    int
	IncludeSource      bool
	SourceContextLines int
	SourceMaxLines     int
	SourceLimit        int
}

// ImpactSection is one direction of a report, with the relations that produced
// it and its own truncation flag.
type ImpactSection struct {
	Direction ImpactDirection  `json:"direction"`
	Depth     int              `json:"depth"`
	Relations []graph.EdgeKind `json:"relations"`
	Nodes     []ReachedNode    `json:"nodes"`
	Edges     []graph.Edge     `json:"edges"`
	Truncated bool             `json:"truncated"`
}

// ImpactedFile is one file touched by the report, deduplicated by repository
// identity plus canonical relative path while preserving every node and edge
// that caused its inclusion.
type ImpactedFile struct {
	Repository string   `json:"repository,omitempty"`
	Path       string   `json:"path"`
	NodeIDs    []string `json:"node_ids"`
	EdgeIDs    []string `json:"edge_ids"`
	Directions []string `json:"directions"`
	Federated  bool     `json:"federated,omitempty"`
}

// ImpactRelation is a config, data, or event relationship kept with the edge
// that proves it.
type ImpactRelation struct {
	Direction ImpactDirection `json:"direction"`
	Edge      graph.Edge      `json:"edge"`
	Node      graph.Node      `json:"node"`
}

// CrossRepositoryHop is an edge that federation resolved across a repository
// boundary, kept with the node on the far side.
type CrossRepositoryHop struct {
	Direction ImpactDirection `json:"direction"`
	Edge      graph.Edge      `json:"edge"`
	Node      graph.Node      `json:"node"`
}

// ImpactReport is the full bidirectional change-impact answer for one root.
type ImpactReport struct {
	Root            graph.Node           `json:"root"`
	Upstream        ImpactSection        `json:"upstream"`
	Downstream      ImpactSection        `json:"downstream"`
	ImpactedFiles   []ImpactedFile       `json:"impacted_files"`
	CrossRepository []CrossRepositoryHop `json:"cross_repository_hops"`
	Config          []ImpactRelation     `json:"config_relations"`
	Data            []ImpactRelation     `json:"data_relations"`
	Events          []ImpactRelation     `json:"event_relations"`
	Sources         []SourceExcerpt      `json:"sources,omitempty"`
	Truncated       bool                 `json:"truncated"`
}

// Impact resolves the selector and returns a deterministic bidirectional
// change-impact report. Resolution errors (ErrNotFound, *AmbiguousError)
// propagate unchanged and never yield a partial report.
func (s *Service) Impact(ctx context.Context, selector string, options ImpactOptions) (ImpactReport, error) {
	root, err := s.ResolveKind(ctx, selector, options.Kind)
	if err != nil {
		return ImpactReport{}, err
	}
	options = options.withDefaults()

	upstream, err := s.impactSection(ctx, root, Upstream, options.UpstreamDepth, options.UpstreamLimit)
	if err != nil {
		return ImpactReport{}, err
	}
	downstream, err := s.impactSection(ctx, root, Downstream, options.DownstreamDepth, options.DownstreamLimit)
	if err != nil {
		return ImpactReport{}, err
	}

	report := ImpactReport{
		Root:            root,
		Upstream:        upstream,
		Downstream:      downstream,
		ImpactedFiles:   []ImpactedFile{},
		CrossRepository: []CrossRepositoryHop{},
		Config:          []ImpactRelation{},
		Data:            []ImpactRelation{},
		Events:          []ImpactRelation{},
		Truncated:       upstream.Truncated || downstream.Truncated,
	}
	sections := []ImpactSection{upstream, downstream}
	report.ImpactedFiles = impactedFiles(sections)
	report.CrossRepository = crossRepositoryHops(sections)
	report.Config, report.Data, report.Events = partitionRelations(sections)
	report.Sources = s.excerpts(ctx, root, sections, options)
	return report, nil
}

func (o ImpactOptions) withDefaults() ImpactOptions {
	if o.UpstreamDepth <= 0 {
		o.UpstreamDepth = defaultImpactDepth
	}
	if o.DownstreamDepth <= 0 {
		o.DownstreamDepth = defaultImpactDepth
	}
	if o.UpstreamLimit <= 0 {
		o.UpstreamLimit = defaultImpactLimit
	}
	if o.DownstreamLimit <= 0 {
		o.DownstreamLimit = defaultImpactLimit
	}
	if o.SourceContextLines <= 0 {
		o.SourceContextLines = defaultSourceContextLines
	}
	if o.SourceMaxLines <= 0 {
		o.SourceMaxLines = defaultSourceMaxLines
	}
	if o.SourceLimit <= 0 {
		o.SourceLimit = defaultSourceLimit
	}
	return o
}

// impactSection reuses the shared Neighborhood traversal so impact reports,
// callers/callees, and generic traversal all agree on ordering and bounds.
func (s *Service) impactSection(ctx context.Context, root graph.Node, direction ImpactDirection, depth, limit int) (ImpactSection, error) {
	traversalDirection := Incoming
	relations := UpstreamRelations()
	if direction == Downstream {
		traversalDirection = Outgoing
		relations = DownstreamRelations()
	}
	traversal, err := s.Neighborhood(ctx, root.ID, "", depth, traversalDirection, relations, limit)
	if err != nil {
		return ImpactSection{}, err
	}
	return ImpactSection{
		Direction: direction,
		Depth:     depth,
		Relations: relations,
		Nodes:     traversal.Nodes,
		Edges:     traversal.Edges,
		Truncated: traversal.Truncated,
	}, nil
}

type fileKey struct {
	repository string
	path       string
}

type fileAccumulator struct {
	nodes      map[string]bool
	edges      map[string]bool
	directions map[string]bool
	federated  bool
}

// impactedFiles groups every reached node - including the root, whose own file
// is always part of the change - by repository identity plus canonical
// relative path, and records each node and edge that caused inclusion.
func impactedFiles(sections []ImpactSection) []ImpactedFile {
	accumulators := map[fileKey]*fileAccumulator{}
	keyForNode := map[string]fileKey{}
	ensure := func(key fileKey) *fileAccumulator {
		accumulator, ok := accumulators[key]
		if !ok {
			accumulator = &fileAccumulator{nodes: map[string]bool{}, edges: map[string]bool{}, directions: map[string]bool{}}
			accumulators[key] = accumulator
		}
		return accumulator
	}
	for _, section := range sections {
		nodes := map[string]graph.Node{}
		for _, reached := range section.Nodes {
			nodes[reached.Node.ID] = reached.Node
			key, ok := fileKeyFor(reached.Node)
			if !ok {
				continue
			}
			keyForNode[reached.Node.ID] = key
			accumulator := ensure(key)
			accumulator.nodes[reached.Node.ID] = true
			accumulator.directions[string(section.Direction)] = true
		}
		for _, edge := range section.Edges {
			for _, endpoint := range []string{edge.FromID, edge.ToID} {
				if _, ok := nodes[endpoint]; !ok {
					continue
				}
				key, ok := keyForNode[endpoint]
				if !ok {
					continue
				}
				accumulator := ensure(key)
				accumulator.edges[edge.ID] = true
				accumulator.directions[string(section.Direction)] = true
				if isFederated(edge) {
					accumulator.federated = true
				}
			}
		}
	}
	result := make([]ImpactedFile, 0, len(accumulators))
	for key, accumulator := range accumulators {
		result = append(result, ImpactedFile{
			Repository: key.repository,
			Path:       key.path,
			NodeIDs:    sortedKeys(accumulator.nodes),
			EdgeIDs:    sortedKeys(accumulator.edges),
			Directions: sortedKeys(accumulator.directions),
			Federated:  accumulator.federated,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Repository != result[j].Repository {
			return result[i].Repository < result[j].Repository
		}
		return result[i].Path < result[j].Path
	})
	return result
}

// fileKeyFor derives repository identity plus canonical path for a node.
// Nodes without a source path - external or unresolved targets, for instance -
// stay visible in their section but contribute no impacted file, because
// guessing a path for them would fabricate evidence.
func fileKeyFor(node graph.Node) (fileKey, bool) {
	relative := canonicalPath(node.Location.Path)
	if relative == "" {
		return fileKey{}, false
	}
	return fileKey{repository: nodeRepository(node), path: relative}, true
}

func canonicalPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ""
	}
	cleaned := path.Clean(value)
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	return cleaned
}

// nodeRepository reads repository identity from node properties. Local indexes
// leave it unset, so single-repository reports carry an empty repository and
// deduplicate on path alone; federated members annotate nodes with one of
// these keys.
func nodeRepository(node graph.Node) string {
	for _, key := range []string{"repository", "repo", "repository_name"} {
		if value := strings.TrimSpace(node.Properties[key]); value != "" {
			return value
		}
	}
	return ""
}

func isFederated(edge graph.Edge) bool {
	return edge.Properties[federatedProperty] == "true"
}

// crossRepositoryHops reports the edges federation resolved across repository
// boundaries, paired with the node on the far side of the hop.
func crossRepositoryHops(sections []ImpactSection) []CrossRepositoryHop {
	var result []CrossRepositoryHop
	seen := map[string]bool{}
	for _, section := range sections {
		nodes := map[string]graph.Node{}
		for _, reached := range section.Nodes {
			nodes[reached.Node.ID] = reached.Node
		}
		for _, edge := range section.Edges {
			if !isFederated(edge) {
				continue
			}
			far := edge.FromID
			if section.Direction == Downstream {
				far = edge.ToID
			}
			node, ok := nodes[far]
			if !ok {
				continue
			}
			key := string(section.Direction) + "\x00" + edge.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, CrossRepositoryHop{Direction: section.Direction, Edge: edge, Node: node})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Direction != result[j].Direction {
			return result[i].Direction < result[j].Direction
		}
		if result[i].Node.QualifiedName != result[j].Node.QualifiedName {
			return result[i].Node.QualifiedName < result[j].Node.QualifiedName
		}
		return result[i].Edge.ID < result[j].Edge.ID
	})
	if result == nil {
		return []CrossRepositoryHop{}
	}
	return result
}

// partitionRelations splits the edges already present in the sections into
// configuration, data, and event relationships using the existing graph
// vocabulary. The partition is decided by edge kind together with the kind of
// the non-code endpoint, because the same edge kind can mean different things:
//
//   - config: reads_config or defines against a config_key node. references is
//     also accepted against config_key, matching how federation already treats
//     a reference to a configuration key (see candidateAllowed).
//   - data: reads or writes against a table, view, or column; references
//     against those kinds for the same reason.
//   - events: publishes or subscribes against an event node.
//
// Endpoint relations (exposes, handled_by, requests) are intentionally left in
// the ordinary upstream/downstream sections: they describe call routing, not a
// separate config/data/event fact.
func partitionRelations(sections []ImpactSection) (config, data, events []ImpactRelation) {
	config, data, events = []ImpactRelation{}, []ImpactRelation{}, []ImpactRelation{}
	seen := map[string]bool{}
	for _, section := range sections {
		nodes := map[string]graph.Node{}
		for _, reached := range section.Nodes {
			nodes[reached.Node.ID] = reached.Node
		}
		for _, edge := range section.Edges {
			bucket, node, ok := classifyRelation(edge, nodes)
			if !ok {
				continue
			}
			key := string(section.Direction) + "\x00" + edge.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			relation := ImpactRelation{Direction: section.Direction, Edge: edge, Node: node}
			switch bucket {
			case relationConfig:
				config = append(config, relation)
			case relationData:
				data = append(data, relation)
			case relationEvent:
				events = append(events, relation)
			}
		}
	}
	sortRelations(config)
	sortRelations(data)
	sortRelations(events)
	return config, data, events
}

type relationBucket int

const (
	relationNone relationBucket = iota
	relationConfig
	relationData
	relationEvent
)

func classifyRelation(edge graph.Edge, nodes map[string]graph.Node) (relationBucket, graph.Node, bool) {
	for _, endpoint := range []string{edge.ToID, edge.FromID} {
		node, ok := nodes[endpoint]
		if !ok {
			continue
		}
		bucket := bucketFor(edge.Kind, node.Kind)
		if bucket != relationNone {
			return bucket, node, true
		}
	}
	return relationNone, graph.Node{}, false
}

func bucketFor(edge graph.EdgeKind, node graph.NodeKind) relationBucket {
	switch node {
	case graph.KindConfigKey:
		if edge == graph.EdgeReadsConfig || edge == graph.EdgeDefines || edge == graph.EdgeReferences {
			return relationConfig
		}
	case graph.KindTable, graph.KindView, graph.KindColumn:
		if edge == graph.EdgeReads || edge == graph.EdgeWrites || edge == graph.EdgeReferences {
			return relationData
		}
	case graph.KindEvent:
		if edge == graph.EdgePublishes || edge == graph.EdgeSubscribes {
			return relationEvent
		}
	case graph.KindGodotInputAction:
		// An input action is a project.godot declaration promoted to its own
		// kind, so it belongs with configuration rather than needing a section
		// of its own. Node groups are deliberately not mapped here: they are
		// neither configuration, data, nor events, and inventing a bucket for
		// them would change the shape of every impact report.
		if edge == graph.EdgeUsesInputAction || edge == graph.EdgeDefines {
			return relationConfig
		}
	}
	return relationNone
}

func sortRelations(relations []ImpactRelation) {
	sort.Slice(relations, func(i, j int) bool {
		if relations[i].Direction != relations[j].Direction {
			return relations[i].Direction < relations[j].Direction
		}
		if relations[i].Node.QualifiedName != relations[j].Node.QualifiedName {
			return relations[i].Node.QualifiedName < relations[j].Node.QualifiedName
		}
		return relations[i].Edge.ID < relations[j].Edge.ID
	})
}

// excerpts attaches bounded source for the root first and then the reached
// nodes in section order. A node whose source cannot be read is skipped so a
// stale or unavailable worktree never costs the structural report.
func (s *Service) excerpts(ctx context.Context, root graph.Node, sections []ImpactSection, options ImpactOptions) []SourceExcerpt {
	if !options.IncludeSource || s.source == nil {
		return nil
	}
	ordered := []graph.Node{root}
	for _, section := range sections {
		for _, reached := range section.Nodes {
			ordered = append(ordered, reached.Node)
		}
	}
	var result []SourceExcerpt
	seen := map[string]bool{}
	for _, node := range ordered {
		if len(result) >= options.SourceLimit {
			break
		}
		if seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		if node.External || canonicalPath(node.Location.Path) == "" {
			continue
		}
		excerpt, err := s.source.ReadNodeSource(ctx, node.ID, options.SourceContextLines, options.SourceMaxLines)
		if err != nil {
			continue
		}
		excerpt.NodeID = node.ID
		result = append(result, excerpt)
	}
	return result
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
