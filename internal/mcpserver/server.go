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
	"github.com/cafecito-games/grafo/internal/semantic"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Service struct {
	repository graph.ReadRepository
	query      *query.Service
	catalog    *query.Catalog
	projects   []indexer.Project
	refresh    func(context.Context) error
	reusable   func(context.Context, string, int) (semantic.SearchResult, error)
	source     func(context.Context, string, int, int) (sourcecontext.Excerpt, error)
	refreshMu  sync.Mutex
}

func New(repository graph.Repository, project indexer.Project) *Service {
	return NewFederated(repository, []indexer.Project{project})
}

func NewFederated(repository graph.ReadRepository, projects []indexer.Project) *Service {
	service := &Service{repository: repository, query: query.NewService(repository), projects: projects}
	if catalogRepository, ok := repository.(graph.CatalogRepository); ok {
		service.catalog = query.NewCatalog(catalogRepository)
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

func (s *Service) WithSource(read func(context.Context, string, int, int) (sourcecontext.Excerpt, error)) *Service {
	s.source = read
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
		Instructions: "Use Grafo tools for deterministic structural code retrieval. Resolve symbols first, then walk graph edges. Treat external nodes as explicit unresolved boundaries. Reusable-code search uses embeddings only to select candidates and includes graph-resolved context.",
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
	mcp.AddTool(server, &mcp.Tool{Name: "get_blast_radius", Title: "Get blast radius", Description: "Walk incoming dependency edges to identify code structurally affected by a symbol change.", Annotations: annotations}, s.getBlastRadius)
	if s.catalog != nil {
		mcp.AddTool(server, &mcp.Tool{Name: "list_data_resources", Title: "List data resources", Description: "Catalog indexed tables and views with normalized dialect and object metadata, plus unresolved external targets kept explicit.", Annotations: annotations}, s.listDataResources)
		mcp.AddTool(server, &mcp.Tool{Name: "get_data_resource_usage", Title: "Get data resource usage", Description: "Report the readers and writers of one table or view separately, each with its source evidence. An ambiguous name fails with its candidates named instead of guessing one.", Annotations: annotations}, s.getDataResourceUsage)
		mcp.AddTool(server, &mcp.Tool{Name: "list_config_keys", Title: "List configuration keys", Description: "Catalog configuration keys with their definitions, readers, and unresolved references. Stored values are never returned.", Annotations: annotations}, s.listConfigKeys)
		mcp.AddTool(server, &mcp.Tool{Name: "list_events", Title: "List events", Description: "Catalog events with their declarations, producers, consumers, and handlers.", Annotations: annotations}, s.listEvents)
		mcp.AddTool(server, &mcp.Tool{Name: "find_orphaned_events", Title: "Find orphaned events", Description: "Report events published without a consumer, consumed without a producer, or declared with neither. An unresolved possible counterpart makes the status unknown rather than orphaned.", Annotations: annotations}, s.findOrphanedEvents)
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
	Query string `json:"query" jsonschema:"name or qualified-name fragment to find"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum matches; defaults to 20"`
}

type FindSymbolsOutput struct {
	Matches []graph.Node `json:"matches"`
}

func (s *Service) findSymbols(ctx context.Context, _ *mcp.CallToolRequest, input FindSymbolsInput) (*mcp.CallToolResult, FindSymbolsOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, FindSymbolsOutput{}, err
	}
	if strings.TrimSpace(input.Query) == "" {
		return nil, FindSymbolsOutput{}, fmt.Errorf("query is required")
	}
	matches, err := s.query.Find(ctx, input.Query, input.Limit)
	return nil, FindSymbolsOutput{Matches: matches}, err
}

type SelectorInput struct {
	Selector string `json:"selector" jsonschema:"qualified symbol name or stable node ID"`
}

type SourceInput struct {
	Selector     string `json:"selector" jsonschema:"qualified symbol name or stable node ID"`
	ContextLines int    `json:"context_lines,omitempty" jsonschema:"surrounding lines from 0 to 20"`
	MaxLines     int    `json:"max_lines,omitempty" jsonschema:"maximum returned lines; defaults to 200 and may not exceed 1000"`
}

func (s *Service) getSource(ctx context.Context, _ *mcp.CallToolRequest, input SourceInput) (*mcp.CallToolResult, sourcecontext.Excerpt, error) {
	if err := s.ready(ctx); err != nil {
		return nil, sourcecontext.Excerpt{}, err
	}
	result, err := s.source(ctx, input.Selector, input.ContextLines, input.MaxLines)
	return nil, result, err
}

type NodeOutput struct {
	Node graph.Node `json:"node"`
}

func (s *Service) getNode(ctx context.Context, _ *mcp.CallToolRequest, input SelectorInput) (*mcp.CallToolResult, NodeOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, NodeOutput{}, err
	}
	node, err := s.query.Resolve(ctx, input.Selector)
	return nil, NodeOutput{Node: node}, withCandidates(err)
}

type TraversalInput struct {
	Selector  string   `json:"selector" jsonschema:"qualified symbol name or stable node ID"`
	Depth     int      `json:"depth,omitempty" jsonschema:"maximum traversal depth"`
	Direction string   `json:"direction,omitempty" jsonschema:"outgoing, incoming, or both"`
	Relations []string `json:"relations,omitempty" jsonschema:"optional edge kinds to follow"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum visited nodes"`
}

func (s *Service) getNeighbors(ctx context.Context, _ *mcp.CallToolRequest, input TraversalInput) (*mcp.CallToolResult, query.Traversal, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.Traversal{}, err
	}
	result, err := s.query.Neighborhood(ctx, input.Selector, input.Depth, query.Direction(input.Direction), edgeKinds(input.Relations), input.Limit)
	return nil, result, withCandidates(err)
}

type PathInput struct {
	From      string   `json:"from" jsonschema:"starting qualified symbol name or node ID"`
	To        string   `json:"to" jsonschema:"destination qualified symbol name or node ID"`
	Direction string   `json:"direction,omitempty" jsonschema:"outgoing, incoming, or both"`
	Relations []string `json:"relations,omitempty" jsonschema:"optional edge kinds to follow"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum visited nodes"`
}

func (s *Service) findPath(ctx context.Context, _ *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, query.Path, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.Path{}, err
	}
	result, err := s.query.ShortestPath(ctx, input.From, input.To, query.Direction(input.Direction), edgeKinds(input.Relations), input.Limit)
	return nil, result, withCandidates(err)
}

func (s *Service) getCallers(ctx context.Context, _ *mcp.CallToolRequest, input TraversalInput) (*mcp.CallToolResult, query.Traversal, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.Traversal{}, err
	}
	depth := input.Depth
	if depth == 0 {
		depth = 3
	}
	result, err := s.query.Neighborhood(ctx, input.Selector, depth, query.Incoming,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy}, input.Limit)
	return nil, result, withCandidates(err)
}

func (s *Service) getCallees(ctx context.Context, _ *mcp.CallToolRequest, input TraversalInput) (*mcp.CallToolResult, query.Traversal, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.Traversal{}, err
	}
	depth := input.Depth
	if depth == 0 {
		depth = 3
	}
	result, err := s.query.Neighborhood(ctx, input.Selector, depth, query.Outgoing,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy}, input.Limit)
	return nil, result, withCandidates(err)
}

func (s *Service) getBlastRadius(ctx context.Context, _ *mcp.CallToolRequest, input TraversalInput) (*mcp.CallToolResult, query.Traversal, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.Traversal{}, err
	}
	depth := input.Depth
	if depth == 0 {
		depth = 4
	}
	result, err := s.query.Neighborhood(ctx, input.Selector, depth, query.Incoming,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy, graph.EdgeImports, graph.EdgeExtends,
			graph.EdgeImplements, graph.EdgeEmbeds, graph.EdgeReferences, graph.EdgeReads,
			graph.EdgeWrites, graph.EdgeAssigns, graph.EdgeReturns, graph.EdgePasses,
			graph.EdgeRequests, graph.EdgeDependsOn}, input.Limit)
	return nil, result, withCandidates(err)
}

type CatalogInput struct {
	Repository string `json:"repository,omitempty" jsonschema:"restrict results to one indexed repository by name"`
	Name       string `json:"name,omitempty" jsonschema:"optional name or qualified-name fragment"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum catalog entries, and separately the maximum evidence sites per relation; defaults to 100 and may not exceed 1000"`
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

type DataResourceUsageInput struct {
	CatalogInput
	Selector string `json:"selector" jsonschema:"table or view name, qualified name, or stable node ID"`
}

func (s *Service) getDataResourceUsage(ctx context.Context, _ *mcp.CallToolRequest, input DataResourceUsageInput) (*mcp.CallToolResult, query.DataResourceUsage, error) {
	if err := s.ready(ctx); err != nil {
		return nil, query.DataResourceUsage{}, err
	}
	result, err := s.catalog.DataResourceUsage(ctx, input.Selector, input.options())
	return nil, result, withCandidates(err)
}

// withCandidates names the ambiguous candidates in the error, because a tool
// error carries no structured payload for a client to read them from. Every
// tool that resolves a selector uses it, so one ambiguous name behaves the same
// way across the whole surface.
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
