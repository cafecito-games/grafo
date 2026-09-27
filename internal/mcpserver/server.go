package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/search"
	"github.com/cafecito-games/grafo/internal/semantic"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Service struct {
	repository graph.ReadRepository
	query      *query.Service
	projects   []indexer.Project
	refresh    func(context.Context) error
	reusable   func(context.Context, string, int) (semantic.SearchResult, error)
	source     func(context.Context, string, graph.NodeKind, int, int) (sourcecontext.Excerpt, error)
	search     *search.Service
	catalog    *query.Catalog
	topology   *query.Topology
	refreshMu  sync.Mutex
}

func New(repository graph.Repository, project indexer.Project) *Service {
	return NewFederated(repository, []indexer.Project{project})
}

func NewFederated(repository graph.ReadRepository, projects []indexer.Project) *Service {
	service := &Service{repository: repository, query: query.NewService(repository), projects: projects}
	if catalogRepository, ok := repository.(graph.CatalogRepository); ok {
		service.catalog = query.NewCatalog(catalogRepository)
		service.topology = query.NewTopology(catalogRepository)
	}
	return service
}

// WithRefresh configures a synchronization hook that runs before every tool
// call. It keeps a long-lived MCP session aligned with the active worktrees.
func (s *Service) WithRefresh(refresh func(context.Context) error) *Service {
	s.refresh = refresh
	return s
}

func (s *Service) WithReusable(search func(context.Context, string, int) (semantic.SearchResult, error)) *Service {
	s.reusable = search
	return s
}

func (s *Service) WithSource(read func(context.Context, string, graph.NodeKind, int, int) (sourcecontext.Excerpt, error)) *Service {
	s.source = read
	// Impact reports read excerpts through the same bounded reader, so
	// repository-root confinement and line/byte limits are enforced once.
	s.query = s.query.WithSourceReader(sourcecontext.ReaderFunc(read))
	return s
}

// WithSearch enables bounded content search over the refreshed indexes.
func (s *Service) WithSearch(service *search.Service) *Service {
	s.search = service
	return s
}

func (s *Service) ready(ctx context.Context) error {
	if s.refresh == nil {
		return nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.refresh(ctx)
}

func (s *Service) Server(version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "grafo", Version: version}, &mcp.ServerOptions{
		Instructions: "Use Grafo tools for deterministic structural code retrieval. Resolve symbols first, then walk graph edges. Treat external nodes as explicit unresolved boundaries. Prefer get_blast_radius before a behavior-changing edit: it reports both what depends on a symbol and what it depends on. Symbol, node, source, caller, callee, path, and impact tools accept a batch of inputs and return one result or error per input in order. Use search_source only for content questions the graph does not model. Reusable-code search uses embeddings only to select candidates and includes graph-resolved context.",
	})
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPointer(false)}
	mcp.AddTool(server, &mcp.Tool{Name: "find_symbols", Title: "Find symbols", Description: "Find graph nodes by deterministic name matching. Use this to obtain an unambiguous qualified name or stable node ID.", Annotations: annotations}, s.findSymbols)
	mcp.AddTool(server, &mcp.Tool{Name: "get_node", Title: "Get node", Description: "Resolve one symbol or stable ID and return its graph metadata and source location.", Annotations: annotations}, s.getNode)
	if s.source != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "get_source", Title: "Get source", Description: "Resolve a graph node and return its exact bounded source span from the correct worktree without text search.", Annotations: annotations}, s.getSource)
	}
	mcp.AddTool(server, &mcp.Tool{Name: "get_neighbors", Title: "Walk graph neighbors", Description: "Walk incoming, outgoing, or both edge directions from a symbol with deterministic breadth-first traversal.", Annotations: annotations}, s.getNeighbors)
	mcp.AddTool(server, &mcp.Tool{Name: "find_path", Title: "Find graph path", Description: "Find the deterministic shortest structural path between two symbols.", Annotations: annotations}, s.findPath)
	mcp.AddTool(server, &mcp.Tool{Name: "get_callers", Title: "Get callers", Description: "Walk incoming call and handler edges to find callers of a symbol.", Annotations: annotations}, s.getCallers)
	mcp.AddTool(server, &mcp.Tool{Name: "get_callees", Title: "Get callees", Description: "Walk outgoing call and handler edges to find callees of a symbol.", Annotations: annotations}, s.getCallees)
	mcp.AddTool(server, &mcp.Tool{Name: "get_godot_composition", Title: "Get Godot composition", Description: "Return Godot runtime composition for a scene, scene node, resource, script, or autoload: which scenes it instantiates, which scenes instantiate it, attached scripts, and autoload availability, each with its original resource evidence.", Annotations: annotations}, s.getGodotComposition)
	mcp.AddTool(server, &mcp.Tool{Name: "get_godot_interactions", Title: "Get Godot interactions", Description: "Return Godot gameplay wiring for a scene, scene node, script symbol, input action, node group, or signal: the input actions it uses, the node groups it joins, inspects, and dispatches to, and the signal routes it takes part in, whether a scene declared them or a script established them. Filter by action, group, or signal and by direction; unresolved actions, groups, and signals stay in the report and are counted so missing wiring is visible.", Annotations: annotations}, s.getGodotInteractions)
	mcp.AddTool(server, &mcp.Tool{Name: "get_failure_flow", Title: "Get failure flow", Description: "Return typed error-return declarations, escaping and wrapped errors, handlers, panic and recovery sites, and deferred cleanup. Every fact includes its source and recognition evidence; conditional and unresolved facts remain explicit.", Annotations: annotations}, s.getFailureFlow)
	mcp.AddTool(server, &mcp.Tool{Name: "get_blast_radius", Title: "Get change impact", Description: "Return a bounded bidirectional change-impact report: what depends on the symbol, what it depends on, impacted files, cross-repository hops, and config, data, and event relationships.", Annotations: annotations}, s.getBlastRadius)
	if s.search != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "search_source", Title: "Search indexed source", Description: "Search literal text or RE2 patterns across files belonging to the refreshed indexes. Use graph tools first when the question is structural; use this for content questions the graph does not model.", Annotations: annotations}, s.searchSource)
	}
	if s.catalog != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "list_data_resources", Title: "List data resources", Description: "Catalog indexed tables and views with normalized dialect and object metadata, plus unresolved external targets kept explicit.", Annotations: annotations}, s.listDataResources)
		mcp.AddTool(server, &mcp.Tool{Name: "get_data_resource_usage", Title: "Get data resource usage", Description: "Report the readers and writers of one table or view separately, each with its source evidence. An ambiguous name fails with its candidates named instead of guessing one.", Annotations: annotations}, s.getDataResourceUsage)
		mcp.AddTool(server, &mcp.Tool{Name: "list_config_keys", Title: "List configuration keys", Description: "Catalog configuration keys with their definitions, readers, and unresolved references. Stored values are never returned.", Annotations: annotations}, s.listConfigKeys)
		mcp.AddTool(server, &mcp.Tool{Name: "list_events", Title: "List events", Description: "Catalog events with their declarations, producers, consumers, and handlers.", Annotations: annotations}, s.listEvents)
		mcp.AddTool(server, &mcp.Tool{Name: "find_orphaned_events", Title: "Find orphaned events", Description: "Report events published without a consumer, consumed without a producer, or declared with neither. An unresolved possible counterpart makes the status unknown rather than orphaned.", Annotations: annotations}, s.findOrphanedEvents)
		mcp.AddTool(server, &mcp.Tool{Name: "list_endpoints", Title: "List endpoints", Description: "Catalog exact HTTP endpoint declarations with source locations, exposer evidence, and resolved, ambiguous, missing, or unresolved handlers.", Annotations: annotations}, s.listEndpoints)
		mcp.AddTool(server, &mcp.Tool{Name: "list_outbound_requests", Title: "List outbound requests", Description: "List outbound HTTP facts and resolve each to one exact endpoint when available. Duplicate method-and-route declarations remain ambiguous and unknown targets remain external.", Annotations: annotations}, s.listOutboundRequests)
		mcp.AddTool(server, &mcp.Tool{Name: "find_handler", Title: "Find handler", Description: "Find HTTP and event handlers only from handled_by graph evidence, preserving ambiguous, missing, and unresolved targets.", Annotations: annotations}, s.findHandler)
		mcp.AddTool(server, &mcp.Tool{Name: "get_service_topology", Title: "Get service topology", Description: "Return repository-backed service nodes and evidence-backed synchronous HTTP and asynchronous event links. Every link retains its endpoint or event node IDs and underlying edge IDs.", Annotations: annotations}, s.getServiceTopology)
	}
	mcp.AddTool(server, &mcp.Tool{Name: "get_index_status", Title: "Get index status", Description: "Return the active repository, branch, indexed commit, and graph counts.", Annotations: annotations}, s.getIndexStatus)
	if s.reusable != nil {
		semanticAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: true, OpenWorldHint: boolPointer(true)}
		mcp.AddTool(server, &mcp.Tool{Name: "find_reusable_code", Title: "Find reusable code", Description: "Use embeddings to select natural-language code candidates, then return deterministic one-hop graph context for each candidate. This may update the local vector cache and contact the configured embedding endpoint.", Annotations: semanticAnnotations}, s.findReusableCode)
	}
	return server
}

func (s *Service) Run(ctx context.Context, version string) error {
	return s.Server(version).Run(ctx, &mcp.StdioTransport{})
}

type FindSymbolsInput struct {
	Query   string   `json:"query,omitempty" jsonschema:"name or qualified-name fragment to find"`
	Queries []string `json:"queries,omitempty" jsonschema:"batch of name fragments searched in caller order"`
	Limit   int      `json:"limit,omitempty" jsonschema:"maximum matches per query; defaults to 20"`
}

type FindSymbolsOutput struct {
	Matches []graph.Node                   `json:"matches,omitempty"`
	Results []ResultEnvelope[[]graph.Node] `json:"results,omitempty"`
}

func (s *Service) findSymbols(ctx context.Context, _ *mcp.CallToolRequest, input FindSymbolsInput) (*mcp.CallToolResult, FindSymbolsOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, FindSymbolsOutput{}, err
	}
	queries, batched, err := batchInputs("query", input.Query, input.Queries)
	if err != nil {
		return nil, FindSymbolsOutput{}, err
	}
	results := runBatch(ctx, queries, func(findContext context.Context, term string) ([]graph.Node, error) {
		return s.query.Find(findContext, term, input.Limit)
	})
	matches, err := firstValue(results, batched)
	return nil, FindSymbolsOutput{Matches: matches, Results: results}, err
}

type SelectorInput struct {
	Selector  string   `json:"selector,omitempty" jsonschema:"qualified symbol name or stable node ID"`
	Selectors []string `json:"selectors,omitempty" jsonschema:"batch of qualified symbol names or stable node IDs resolved in caller order"`
	Kind      string   `json:"kind,omitempty" jsonschema:"optional node kind the selector must resolve to, such as function, method, type, or field"`
}

type SourceInput struct {
	Selector     string   `json:"selector,omitempty" jsonschema:"qualified symbol name or stable node ID"`
	Selectors    []string `json:"selectors,omitempty" jsonschema:"batch of symbol names or node IDs read in caller order"`
	ContextLines int      `json:"context_lines,omitempty" jsonschema:"surrounding lines from 0 to 20"`
	MaxLines     int      `json:"max_lines,omitempty" jsonschema:"maximum returned lines per excerpt; defaults to 200 and may not exceed 1000"`
	Kind         string   `json:"kind,omitempty" jsonschema:"optional node kind the selector must resolve to, such as function, method, type, or field"`
}

// SourceOutput embeds the single excerpt so scalar callers keep reading the
// original top-level fields while batched callers read Results.
type SourceOutput struct {
	sourcecontext.Excerpt
	Results []ResultEnvelope[sourcecontext.Excerpt] `json:"results,omitempty"`
}

func (s *Service) getSource(ctx context.Context, _ *mcp.CallToolRequest, input SourceInput) (*mcp.CallToolResult, SourceOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, SourceOutput{}, err
	}
	selectors, batched, err := batchInputs("selector", input.Selector, input.Selectors)
	if err != nil {
		return nil, SourceOutput{}, err
	}
	kind := graph.NodeKind(strings.TrimSpace(input.Kind))
	results := runBatch(ctx, selectors, namingCandidates(func(readContext context.Context, selector string) (sourcecontext.Excerpt, error) {
		return s.source(readContext, selector, kind, input.ContextLines, input.MaxLines)
	}))
	excerpt, err := firstValue(results, batched)
	return nil, SourceOutput{Excerpt: excerpt, Results: results}, err
}

type NodeOutput struct {
	Node    graph.Node                   `json:"node,omitzero"`
	Results []ResultEnvelope[graph.Node] `json:"results,omitempty"`
}

func (s *Service) getNode(ctx context.Context, _ *mcp.CallToolRequest, input SelectorInput) (*mcp.CallToolResult, NodeOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, NodeOutput{}, err
	}
	selectors, batched, err := batchInputs("selector", input.Selector, input.Selectors)
	if err != nil {
		return nil, NodeOutput{}, err
	}
	kind := graph.NodeKind(strings.TrimSpace(input.Kind))
	results := runBatch(ctx, selectors, namingCandidates(func(resolveContext context.Context, selector string) (graph.Node, error) {
		return s.query.ResolveKind(resolveContext, selector, kind)
	}))
	node, err := firstValue(results, batched)
	return nil, NodeOutput{Node: node, Results: results}, err
}

type TraversalInput struct {
	Selector  string   `json:"selector,omitempty" jsonschema:"qualified symbol name or stable node ID"`
	Selectors []string `json:"selectors,omitempty" jsonschema:"batch of symbol names or node IDs traversed in caller order"`
	Depth     int      `json:"depth,omitempty" jsonschema:"maximum traversal depth"`
	Direction string   `json:"direction,omitempty" jsonschema:"outgoing, incoming, or both"`
	Relations []string `json:"relations,omitempty" jsonschema:"optional edge kinds to follow"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum visited nodes"`
	Kind      string   `json:"kind,omitempty" jsonschema:"optional node kind the selector must resolve to, such as function, method, type, or field"`
}

// TraversalOutput embeds the single traversal so scalar callers keep reading
// the original top-level fields while batched callers read Results.
type TraversalOutput struct {
	query.Traversal
	Results []ResultEnvelope[query.Traversal] `json:"results,omitempty"`
}

// traverse runs one traversal shape over every batched selector. Callers,
// callees, and blast radius differ only in their defaults and relation set.
func (s *Service) traverse(ctx context.Context, input TraversalInput, depthDefault int,
	direction query.Direction, relations []graph.EdgeKind) (TraversalOutput, error) {
	if err := s.ready(ctx); err != nil {
		return TraversalOutput{}, err
	}
	selectors, batched, err := batchInputs("selector", input.Selector, input.Selectors)
	if err != nil {
		return TraversalOutput{}, err
	}
	depth := input.Depth
	if depth == 0 {
		depth = depthDefault
	}
	kind := graph.NodeKind(strings.TrimSpace(input.Kind))
	results := runBatch(ctx, selectors, namingCandidates(func(walkContext context.Context, selector string) (query.Traversal, error) {
		return s.query.Neighborhood(walkContext, selector, kind, depth, direction, relations, input.Limit)
	}))
	traversal, err := firstValue(results, batched)
	return TraversalOutput{Traversal: traversal, Results: results}, err
}

func (s *Service) getNeighbors(ctx context.Context, _ *mcp.CallToolRequest, input TraversalInput) (*mcp.CallToolResult, TraversalOutput, error) {
	direction := query.Direction(input.Direction)
	if direction == "" {
		direction = query.Both
	}
	result, err := s.traverse(ctx, input, 1, direction, edgeKinds(input.Relations))
	return nil, result, err
}

// PathPair is one batched from/to request.
type PathPair struct {
	From string `json:"from" jsonschema:"starting qualified symbol name or node ID"`
	To   string `json:"to" jsonschema:"destination qualified symbol name or node ID"`
}

type PathInput struct {
	From      string     `json:"from,omitempty" jsonschema:"starting qualified symbol name or node ID"`
	To        string     `json:"to,omitempty" jsonschema:"destination qualified symbol name or node ID"`
	Pairs     []PathPair `json:"pairs,omitempty" jsonschema:"batch of from/to pairs resolved in caller order"`
	Direction string     `json:"direction,omitempty" jsonschema:"outgoing, incoming, or both"`
	Relations []string   `json:"relations,omitempty" jsonschema:"optional edge kinds to follow"`
	Limit     int        `json:"limit,omitempty" jsonschema:"maximum visited nodes"`
	Kind      string     `json:"kind,omitempty" jsonschema:"optional node kind both endpoints must resolve to, such as function, method, type, or field"`
}

// PathOutput embeds the single path so scalar callers keep reading the
// original top-level fields while batched callers read Results.
type PathOutput struct {
	query.Path
	Results []ResultEnvelope[query.Path] `json:"results,omitempty"`
}

// pathInputs normalizes the scalar from/to fields and the plural pairs list
// into one ordered list, using the same one-form rule as batchInputs.
func pathInputs(input PathInput) ([]PathPair, bool, error) {
	from, to := strings.TrimSpace(input.From), strings.TrimSpace(input.To)
	cleaned := make([]PathPair, 0, len(input.Pairs))
	for index, pair := range input.Pairs {
		pair.From, pair.To = strings.TrimSpace(pair.From), strings.TrimSpace(pair.To)
		if pair.From == "" || pair.To == "" {
			return nil, false, fmt.Errorf("pairs[%d] requires both from and to", index)
		}
		cleaned = append(cleaned, pair)
	}
	scalar := from != "" || to != ""
	if scalar && (from == "" || to == "") {
		return nil, false, fmt.Errorf("from and to must be supplied together")
	}
	switch {
	case !scalar && len(cleaned) == 0:
		return nil, false, fmt.Errorf("from and to, or pairs, is required")
	case len(cleaned) == 0:
		return []PathPair{{From: from, To: to}}, false, nil
	case !scalar:
		if len(cleaned) > maxBatchInputs {
			return nil, false, fmt.Errorf("pairs accepts at most %d inputs, got %d", maxBatchInputs, len(cleaned))
		}
		return cleaned, true, nil
	case len(cleaned) == 1 && cleaned[0].From == from && cleaned[0].To == to:
		return cleaned, false, nil
	default:
		return nil, false, fmt.Errorf("from/to and pairs disagree; supply only one form")
	}
}

func (s *Service) findPath(ctx context.Context, _ *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, PathOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, PathOutput{}, err
	}
	pairs, batched, err := pathInputs(input)
	if err != nil {
		return nil, PathOutput{}, err
	}
	direction := query.Direction(input.Direction)
	relations := edgeKinds(input.Relations)
	results := make([]ResultEnvelope[query.Path], 0, len(pairs))
	for index, pair := range pairs {
		envelope := ResultEnvelope[query.Path]{Index: index, Input: pair.From + " -> " + pair.To}
		value, pathErr := s.query.ShortestPath(ctx, pair.From, pair.To,
			graph.NodeKind(strings.TrimSpace(input.Kind)), direction, relations, input.Limit)
		if pathErr = withCandidates(pathErr); pathErr != nil {
			envelope.Error = pathErr.Error()
		} else {
			envelope.Value = &value
		}
		results = append(results, envelope)
	}
	path, err := firstValue(results, batched)
	return nil, PathOutput{Path: path, Results: results}, err
}

func (s *Service) getCallers(ctx context.Context, _ *mcp.CallToolRequest, input TraversalInput) (*mcp.CallToolResult, TraversalOutput, error) {
	result, err := s.traverse(ctx, input, 3, query.Incoming,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy})
	return nil, result, err
}

func (s *Service) getCallees(ctx context.Context, _ *mcp.CallToolRequest, input TraversalInput) (*mcp.CallToolResult, TraversalOutput, error) {
	result, err := s.traverse(ctx, input, 3, query.Outgoing,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy})
	return nil, result, err
}

// ImpactInput keeps the original selector, depth, and limit fields working
// while adding independent per-direction bounds and optional source.
type ImpactInput struct {
	Selector        string   `json:"selector,omitempty" jsonschema:"qualified symbol name or stable node ID"`
	Selectors       []string `json:"selectors,omitempty" jsonschema:"batch of symbol names or node IDs reported in caller order"`
	Depth           int      `json:"depth,omitempty" jsonschema:"depth applied to both directions unless a per-direction depth is given"`
	Limit           int      `json:"limit,omitempty" jsonschema:"node limit applied to both directions unless a per-direction limit is given"`
	UpstreamDepth   int      `json:"upstream_depth,omitempty" jsonschema:"maximum depth of dependents; defaults to 4"`
	DownstreamDepth int      `json:"downstream_depth,omitempty" jsonschema:"maximum depth of dependencies; defaults to 4"`
	UpstreamLimit   int      `json:"upstream_limit,omitempty" jsonschema:"maximum dependent nodes; defaults to 1000"`
	DownstreamLimit int      `json:"downstream_limit,omitempty" jsonschema:"maximum dependency nodes; defaults to 1000"`
	IncludeSource   bool     `json:"include_source,omitempty" jsonschema:"include bounded source excerpts for the most relevant nodes"`
	ContextLines    int      `json:"context_lines,omitempty" jsonschema:"excerpt context lines from 0 to 20"`
	MaxLines        int      `json:"max_lines,omitempty" jsonschema:"maximum lines per excerpt; defaults to 200"`
	SourceLimit     int      `json:"source_limit,omitempty" jsonschema:"maximum excerpts; defaults to 10"`
	Kind            string   `json:"kind,omitempty" jsonschema:"optional node kind the selector must resolve to, such as function, method, type, or field"`
}

// ImpactOutput embeds the single report so scalar callers read it at the top
// level while batched callers read Results.
type ImpactOutput struct {
	query.ImpactReport
	Results []ResultEnvelope[query.ImpactReport] `json:"results,omitempty"`
}

func (input ImpactInput) options() query.ImpactOptions {
	options := query.ImpactOptions{
		UpstreamDepth: input.UpstreamDepth, DownstreamDepth: input.DownstreamDepth,
		UpstreamLimit: input.UpstreamLimit, DownstreamLimit: input.DownstreamLimit,
		IncludeSource: input.IncludeSource, SourceContextLines: input.ContextLines,
		SourceMaxLines: input.MaxLines, SourceLimit: input.SourceLimit,
		Kind: graph.NodeKind(strings.TrimSpace(input.Kind)),
	}
	if options.UpstreamDepth == 0 {
		options.UpstreamDepth = input.Depth
	}
	if options.DownstreamDepth == 0 {
		options.DownstreamDepth = input.Depth
	}
	if options.UpstreamLimit == 0 {
		options.UpstreamLimit = input.Limit
	}
	if options.DownstreamLimit == 0 {
		options.DownstreamLimit = input.Limit
	}
	return options
}

// GodotCompositionInput bounds one composition report or a batch of them.
type GodotCompositionInput struct {
	Selector  string   `json:"selector,omitempty" jsonschema:"qualified Godot scene, scene node, resource, script, or autoload name, or stable node ID"`
	Selectors []string `json:"selectors,omitempty" jsonschema:"batch of Godot selectors reported in caller order"`
	Kind      string   `json:"kind,omitempty" jsonschema:"optional node kind the selector must resolve to, such as godot_scene, godot_scene_node, godot_resource, godot_autoload, or module"`
	Depth     int      `json:"depth,omitempty" jsonschema:"maximum scene-tree depth explored for a scene; defaults to 8"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum relations per section; defaults to 1000"`
}

// GodotCompositionOutput embeds the single report so scalar callers read it at
// the top level while batched callers read Results.
type GodotCompositionOutput struct {
	query.GodotComposition
	Results []ResultEnvelope[query.GodotComposition] `json:"results,omitempty"`
}

func (s *Service) getGodotComposition(ctx context.Context, _ *mcp.CallToolRequest, input GodotCompositionInput) (*mcp.CallToolResult, GodotCompositionOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, GodotCompositionOutput{}, err
	}
	selectors, batched, err := batchInputs("selector", input.Selector, input.Selectors)
	if err != nil {
		return nil, GodotCompositionOutput{}, err
	}
	kind, err := graph.ParseNodeKind(input.Kind)
	if err != nil {
		return nil, GodotCompositionOutput{}, err
	}
	options := query.GodotCompositionOptions{Depth: input.Depth, Limit: input.Limit, Kind: kind}
	results := runBatch(ctx, selectors, func(reportContext context.Context, selector string) (query.GodotComposition, error) {
		return s.query.GodotComposition(reportContext, selector, options)
	})
	report, err := firstValue(results, batched)
	return nil, GodotCompositionOutput{GodotComposition: report, Results: results}, err
}

// GodotInteractionsInput bounds one interactions report or a batch of them.
type GodotInteractionsInput struct {
	Selector  string   `json:"selector,omitempty" jsonschema:"qualified Godot scene, scene node, script symbol, input action, node group, or signal name, or stable node ID"`
	Selectors []string `json:"selectors,omitempty" jsonschema:"batch of Godot selectors reported in caller order"`
	Kind      string   `json:"kind,omitempty" jsonschema:"optional node kind the selector must resolve to, such as godot_scene, godot_scene_node, godot_input_action, godot_node_group, event, or method"`
	Filters   []string `json:"filters,omitempty" jsonschema:"optional interaction categories to report: action, group, signal; omit for every category"`
	Direction string   `json:"direction,omitempty" jsonschema:"outgoing, incoming, or both; defaults to both"`
	Depth     int      `json:"depth,omitempty" jsonschema:"maximum scene-tree depth explored for a scene; defaults to 8"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum interactions per direction; defaults to 1000"`
}

// GodotInteractionsOutput embeds the single report so scalar callers read it at
// the top level while batched callers read Results.
type GodotInteractionsOutput struct {
	query.GodotInteractions
	Results []ResultEnvelope[query.GodotInteractions] `json:"results,omitempty"`
}

func (s *Service) getGodotInteractions(ctx context.Context, _ *mcp.CallToolRequest, input GodotInteractionsInput) (*mcp.CallToolResult, GodotInteractionsOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, GodotInteractionsOutput{}, err
	}
	selectors, batched, err := batchInputs("selector", input.Selector, input.Selectors)
	if err != nil {
		return nil, GodotInteractionsOutput{}, err
	}
	kind, err := graph.ParseNodeKind(input.Kind)
	if err != nil {
		return nil, GodotInteractionsOutput{}, err
	}
	// An unknown filter or direction is an error rather than a filter that can
	// never match or a silent fall back to a question the caller did not ask.
	var categories []query.GodotInteractionCategory
	for _, value := range input.Filters {
		category, err := query.ParseGodotInteractionCategory(value)
		if err != nil {
			return nil, GodotInteractionsOutput{}, err
		}
		if category != "" {
			categories = append(categories, category)
		}
	}
	direction := query.Both
	switch trimmed := strings.TrimSpace(input.Direction); trimmed {
	case "":
	case string(query.Outgoing), string(query.Incoming), string(query.Both):
		direction = query.Direction(trimmed)
	default:
		return nil, GodotInteractionsOutput{}, fmt.Errorf("unknown direction %q; expected %s, %s, or %s",
			input.Direction, query.Outgoing, query.Incoming, query.Both)
	}
	options := query.GodotInteractionsOptions{Depth: input.Depth, Limit: input.Limit, Kind: kind,
		Direction: direction, Categories: categories}
	results := runBatch(ctx, selectors, func(reportContext context.Context, selector string) (query.GodotInteractions, error) {
		return s.query.GodotInteractions(reportContext, selector, options)
	})
	report, err := firstValue(results, batched)
	return nil, GodotInteractionsOutput{GodotInteractions: report, Results: results}, err
}

type FailureFlowInput struct {
	Selector  string   `json:"selector,omitempty" jsonschema:"qualified function, method, error identity, callee, or stable node ID"`
	Selectors []string `json:"selectors,omitempty" jsonschema:"batch of selectors reported in caller order"`
	Kind      string   `json:"kind,omitempty" jsonschema:"optional node kind the selector must resolve to, such as function, method, type, or variable"`
	Direction string   `json:"direction,omitempty" jsonschema:"outgoing, incoming, or both; defaults to both"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum facts per report section; defaults to 1000"`
}

type FailureFlowOutput struct {
	query.FailureFlow
	Results []ResultEnvelope[query.FailureFlow] `json:"results,omitempty"`
}

func (s *Service) getFailureFlow(ctx context.Context, _ *mcp.CallToolRequest, input FailureFlowInput) (*mcp.CallToolResult, FailureFlowOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, FailureFlowOutput{}, err
	}
	selectors, batched, err := batchInputs("selector", input.Selector, input.Selectors)
	if err != nil {
		return nil, FailureFlowOutput{}, err
	}
	kind, err := graph.ParseNodeKind(input.Kind)
	if err != nil {
		return nil, FailureFlowOutput{}, err
	}
	direction := query.Both
	switch trimmed := strings.TrimSpace(input.Direction); trimmed {
	case "":
	case string(query.Outgoing), string(query.Incoming), string(query.Both):
		direction = query.Direction(trimmed)
	default:
		return nil, FailureFlowOutput{}, fmt.Errorf("unknown direction %q; expected %s, %s, or %s",
			input.Direction, query.Outgoing, query.Incoming, query.Both)
	}
	options := query.FailureFlowOptions{Kind: kind, Direction: direction, Limit: input.Limit}
	results := runBatch(ctx, selectors, func(reportContext context.Context, selector string) (query.FailureFlow, error) {
		return s.query.FailureFlow(reportContext, selector, options)
	})
	report, err := firstValue(results, batched)
	return nil, FailureFlowOutput{FailureFlow: report, Results: results}, err
}

func (s *Service) getBlastRadius(ctx context.Context, _ *mcp.CallToolRequest, input ImpactInput) (*mcp.CallToolResult, ImpactOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, ImpactOutput{}, err
	}
	selectors, batched, err := batchInputs("selector", input.Selector, input.Selectors)
	if err != nil {
		return nil, ImpactOutput{}, err
	}
	options := input.options()
	results := runBatch(ctx, selectors, namingCandidates(func(impactContext context.Context, selector string) (query.ImpactReport, error) {
		return s.query.Impact(impactContext, selector, options)
	}))
	report, err := firstValue(results, batched)
	return nil, ImpactOutput{ImpactReport: report, Results: results}, err
}

type SearchSourceInput struct {
	Pattern           string   `json:"pattern,omitempty" jsonschema:"literal text, or an RE2 pattern when regex is true"`
	Patterns          []string `json:"patterns,omitempty" jsonschema:"batch of patterns searched in one pass"`
	Regex             bool     `json:"regex,omitempty" jsonschema:"compile patterns as Go RE2 instead of matching literally"`
	CaseSensitive     bool     `json:"case_sensitive,omitempty" jsonschema:"match case exactly; defaults to case-insensitive"`
	PathPrefixes      []string `json:"path_prefixes,omitempty" jsonschema:"optional repository-relative path prefixes"`
	Languages         []string `json:"languages,omitempty" jsonschema:"optional indexed language filters"`
	Repositories      []string `json:"repositories,omitempty" jsonschema:"optional repository name filters"`
	ContextLines      int      `json:"context_lines,omitempty" jsonschema:"surrounding lines from 0 to 20"`
	MaxMatchesPerFile int      `json:"max_matches_per_file,omitempty" jsonschema:"per-file match cap; defaults to 50"`
	MaxMatchesPattern int      `json:"max_matches_per_pattern,omitempty" jsonschema:"per-pattern match cap; defaults to 200"`
	MaxMatches        int      `json:"max_matches,omitempty" jsonschema:"total match cap; defaults to 500"`
	MaxFileBytes      int64    `json:"max_file_bytes,omitempty" jsonschema:"largest file searched; defaults to 1048576"`
}

func (s *Service) searchSource(ctx context.Context, _ *mcp.CallToolRequest, input SearchSourceInput) (*mcp.CallToolResult, search.Result, error) {
	if err := s.ready(ctx); err != nil {
		return nil, search.Result{}, err
	}
	patterns, _, err := batchInputs("pattern", input.Pattern, input.Patterns)
	if err != nil {
		return nil, search.Result{}, err
	}
	result, err := s.search.Search(ctx, search.Request{
		Patterns: patterns, Regex: input.Regex, CaseSensitive: input.CaseSensitive,
		PathPrefixes: input.PathPrefixes, Languages: input.Languages, Repositories: input.Repositories,
		ContextLines: input.ContextLines, MaxMatchesPerFile: input.MaxMatchesPerFile,
		MaxMatchesPattern: input.MaxMatchesPattern, MaxMatches: input.MaxMatches,
		MaxFileBytes: input.MaxFileBytes,
	})
	return nil, result, err
}

type CatalogInput struct {
	Repository string `json:"repository,omitempty" jsonschema:"restrict results to one indexed repository by name"`
	Name       string `json:"name,omitempty" jsonschema:"optional name or qualified-name fragment"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum catalog entries per section, and separately the maximum evidence sites per relation; defaults to 100 and may not exceed 1000"`
}

func (i CatalogInput) options() query.CatalogOptions {
	return query.CatalogOptions{Repository: i.Repository, Name: i.Name, Limit: i.Limit}
}

type DataResourceInput struct {
	CatalogInput
	Kinds []string `json:"kinds,omitempty" jsonschema:"data resource kinds to catalog; defaults to table and view"`
}

func (s *Service) listDataResources(ctx context.Context, _ *mcp.CallToolRequest, input DataResourceInput) (*mcp.CallToolResult, query.DataResourceList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.DataResourceList{}, err
	}
	kinds, err := nodeKinds(input.Kinds)
	if err != nil {
		return nil, query.DataResourceList{}, err
	}
	result, err := s.catalog.DataResources(ctx, kinds, input.options())
	return nil, result, err
}

// DataResourceUsageInput deliberately omits the catalog name filter: the
// selector already names the resource.
type DataResourceUsageInput struct {
	Selector   string `json:"selector" jsonschema:"table or view name, qualified name, or stable node ID"`
	Repository string `json:"repository,omitempty" jsonschema:"restrict resolution to one indexed repository by name"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum evidence sites per relation; defaults to 100 and may not exceed 1000"`
}

func (s *Service) getDataResourceUsage(ctx context.Context, _ *mcp.CallToolRequest, input DataResourceUsageInput) (*mcp.CallToolResult, query.DataResourceUsage, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.DataResourceUsage{}, err
	}
	options := query.CatalogOptions{Repository: input.Repository, Limit: input.Limit}
	result, err := s.catalog.DataResourceUsage(ctx, input.Selector, options)
	return nil, result, withCandidates(err)
}

func (s *Service) listConfigKeys(ctx context.Context, _ *mcp.CallToolRequest, input CatalogInput) (*mcp.CallToolResult, query.ConfigKeyList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.ConfigKeyList{}, err
	}
	result, err := s.catalog.ConfigKeys(ctx, input.options())
	return nil, result, err
}

func (s *Service) listEvents(ctx context.Context, _ *mcp.CallToolRequest, input CatalogInput) (*mcp.CallToolResult, query.EventList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.EventList{}, err
	}
	result, err := s.catalog.Events(ctx, input.options())
	return nil, result, err
}

func (s *Service) findOrphanedEvents(ctx context.Context, _ *mcp.CallToolRequest, input CatalogInput) (*mcp.CallToolResult, query.OrphanedEventList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.OrphanedEventList{}, err
	}
	result, err := s.catalog.OrphanedEvents(ctx, input.options())
	return nil, result, err
}

type EndpointInput struct {
	Repository string `json:"repository,omitempty" jsonschema:"restrict results to one indexed repository service by stable name"`
	Method     string `json:"method,omitempty" jsonschema:"exact HTTP method such as GET or POST"`
	Route      string `json:"route,omitempty" jsonschema:"literal route fragment"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum entries or links; defaults to 100 and may not exceed 1000"`
}

func (i EndpointInput) options() query.TopologyOptions {
	return query.TopologyOptions{Repository: i.Repository, Method: i.Method, Route: i.Route, Limit: i.Limit}
}

type HandlerInput struct {
	Repository string `json:"repository,omitempty" jsonschema:"restrict results to one indexed repository service by stable name"`
	Method     string `json:"method,omitempty" jsonschema:"exact HTTP method such as GET or POST"`
	Route      string `json:"route,omitempty" jsonschema:"literal route fragment"`
	Event      string `json:"event,omitempty" jsonschema:"literal event name fragment; cannot be combined with method or route"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum handler matches; defaults to 100 and may not exceed 1000"`
}

func (i HandlerInput) options() query.TopologyOptions {
	return query.TopologyOptions{Repository: i.Repository, Method: i.Method, Route: i.Route,
		Event: i.Event, Limit: i.Limit}
}

type ServiceTopologyInput struct {
	Repository string `json:"repository,omitempty" jsonschema:"restrict results to one indexed repository service by stable name"`
	Method     string `json:"method,omitempty" jsonschema:"exact HTTP method such as GET or POST"`
	Route      string `json:"route,omitempty" jsonschema:"literal route fragment"`
	Event      string `json:"event,omitempty" jsonschema:"literal event name fragment; cannot be combined with method or route"`
	Direction  string `json:"direction,omitempty" jsonschema:"incoming, outgoing, or both when filtering one service"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum service links; defaults to 100 and may not exceed 1000"`
}

func (i ServiceTopologyInput) options() query.TopologyOptions {
	return query.TopologyOptions{Repository: i.Repository, Method: i.Method, Route: i.Route,
		Event: i.Event, Direction: query.Direction(i.Direction), Limit: i.Limit}
}

func (s *Service) listEndpoints(ctx context.Context, _ *mcp.CallToolRequest, input EndpointInput) (*mcp.CallToolResult, query.EndpointList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.EndpointList{}, err
	}
	result, err := s.topology.Endpoints(ctx, input.options())
	return nil, result, err
}

func (s *Service) listOutboundRequests(ctx context.Context, _ *mcp.CallToolRequest, input EndpointInput) (*mcp.CallToolResult, query.OutboundRequestList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.OutboundRequestList{}, err
	}
	result, err := s.topology.OutboundRequests(ctx, input.options())
	return nil, result, err
}

func (s *Service) findHandler(ctx context.Context, _ *mcp.CallToolRequest, input HandlerInput) (*mcp.CallToolResult, query.HandlerList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.HandlerList{}, err
	}
	result, err := s.topology.Handlers(ctx, input.options())
	return nil, result, err
}

func (s *Service) getServiceTopology(ctx context.Context, _ *mcp.CallToolRequest, input ServiceTopologyInput) (*mcp.CallToolResult, query.ServiceTopology, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.ServiceTopology{}, err
	}
	result, err := s.topology.ServiceTopology(ctx, input.options())
	return nil, result, err
}

type StatusInput struct{}

type FindReusableCodeInput struct {
	Query string `json:"query" jsonschema:"natural-language description of code to reuse"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum candidates; defaults to 5"`
}

func (s *Service) findReusableCode(ctx context.Context, _ *mcp.CallToolRequest, input FindReusableCodeInput) (*mcp.CallToolResult, semantic.SearchResult, error) {
	if err := s.ready(ctx); err != nil {
		return nil, semantic.SearchResult{}, err
	}
	if strings.TrimSpace(input.Query) == "" {
		return nil, semantic.SearchResult{}, fmt.Errorf("query is required")
	}
	result, err := s.reusable(ctx, input.Query, input.Limit)
	return nil, result, err
}

type StatusOutput struct {
	Projects      []indexer.Project `json:"projects"`
	IndexedAt     string            `json:"indexed_at"`
	IndexedCommit string            `json:"indexed_commit,omitempty"`
	Counts        graph.Counts      `json:"counts"`
}

func (s *Service) getIndexStatus(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, StatusOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, StatusOutput{}, err
	}
	counts, err := s.repository.Counts(ctx)
	if err != nil {
		return nil, StatusOutput{}, err
	}
	indexedAt, err := s.repository.Meta(ctx, "indexed_at")
	if err != nil {
		return nil, StatusOutput{}, err
	}
	commit, err := s.repository.Meta(ctx, "commit")
	if err != nil {
		return nil, StatusOutput{}, err
	}
	return nil, StatusOutput{Projects: s.projects, IndexedAt: indexedAt, IndexedCommit: commit, Counts: counts}, nil
}

// nodeKinds rejects a list that names no usable kind. Degrading it to the
// default would answer a malformed request with a full catalog.
func nodeKinds(values []string) ([]graph.NodeKind, error) {
	result := make([]graph.NodeKind, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, graph.NodeKind(value))
		}
	}
	if len(values) > 0 && len(result) == 0 {
		return nil, fmt.Errorf("kinds contains no node kind")
	}
	return result, nil
}

// withCandidates names the ambiguous candidates in the error, because a tool
// error carries no structured payload for a client to read them from. Batched
// operations apply it through namingCandidates, since an envelope keeps only
// the error's text.
func withCandidates(err error) error {
	var ambiguous *query.AmbiguousError
	if !errors.As(err, &ambiguous) {
		return err
	}
	names := make([]string, 0, len(ambiguous.Candidates))
	for _, candidate := range ambiguous.Candidates {
		names = append(names, fmt.Sprintf("%s [%s] %s", candidate.QualifiedName, candidate.Kind, candidate.ID))
	}
	return fmt.Errorf("%w; candidates: %s", err, strings.Join(names, "; "))
}

// namingCandidates wraps one batched operation so an ambiguous selector reports
// its candidates in every response shape. runBatch flattens an error to its
// text, so the candidates must be added before the envelope is built.
func namingCandidates[T any](run func(context.Context, string) (T, error)) func(context.Context, string) (T, error) {
	return func(ctx context.Context, input string) (T, error) {
		value, err := run(ctx, input)
		return value, withCandidates(err)
	}
}

func edgeKinds(values []string) []graph.EdgeKind {
	result := make([]graph.EdgeKind, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, graph.EdgeKind(value))
		}
	}
	return result
}

func boolPointer(value bool) *bool { return &value }
