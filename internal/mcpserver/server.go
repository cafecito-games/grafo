package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Service struct {
	repository graph.ReadRepository
	query      *query.Service
	projects   []indexer.Project
	refresh    func(context.Context) error
	refreshMu  sync.Mutex
}

func New(repository graph.Repository, project indexer.Project) *Service {
	return NewFederated(repository, []indexer.Project{project})
}

func NewFederated(repository graph.ReadRepository, projects []indexer.Project) *Service {
	return &Service{repository: repository, query: query.NewService(repository), projects: projects}
}

// WithRefresh configures a synchronization hook that runs before every tool
// call. It keeps a long-lived MCP session aligned with the active worktrees.
func (s *Service) WithRefresh(refresh func(context.Context) error) *Service {
	s.refresh = refresh
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
		Instructions: "Use Grafo tools for deterministic structural code retrieval. Resolve symbols first, then walk graph edges. Treat external nodes as explicit unresolved boundaries.",
	})
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPointer(false)}
	mcp.AddTool(server, &mcp.Tool{Name: "find_symbols", Title: "Find symbols", Description: "Find graph nodes by deterministic name matching. Use this to obtain an unambiguous qualified name or stable node ID.", Annotations: annotations}, s.findSymbols)
	mcp.AddTool(server, &mcp.Tool{Name: "get_node", Title: "Get node", Description: "Resolve one symbol or stable ID and return its graph metadata and source location.", Annotations: annotations}, s.getNode)
	mcp.AddTool(server, &mcp.Tool{Name: "get_neighbors", Title: "Walk graph neighbors", Description: "Walk incoming, outgoing, or both edge directions from a symbol with deterministic breadth-first traversal.", Annotations: annotations}, s.getNeighbors)
	mcp.AddTool(server, &mcp.Tool{Name: "find_path", Title: "Find graph path", Description: "Find the deterministic shortest structural path between two symbols.", Annotations: annotations}, s.findPath)
	mcp.AddTool(server, &mcp.Tool{Name: "get_callers", Title: "Get callers", Description: "Walk incoming call and handler edges to find callers of a symbol.", Annotations: annotations}, s.getCallers)
	mcp.AddTool(server, &mcp.Tool{Name: "get_callees", Title: "Get callees", Description: "Walk outgoing call and handler edges to find callees of a symbol.", Annotations: annotations}, s.getCallees)
	mcp.AddTool(server, &mcp.Tool{Name: "get_blast_radius", Title: "Get blast radius", Description: "Walk incoming dependency edges to identify code structurally affected by a symbol change.", Annotations: annotations}, s.getBlastRadius)
	mcp.AddTool(server, &mcp.Tool{Name: "get_index_status", Title: "Get index status", Description: "Return the active repository, branch, indexed commit, and graph counts.", Annotations: annotations}, s.getIndexStatus)
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

type NodeOutput struct {
	Node graph.Node `json:"node"`
}

func (s *Service) getNode(ctx context.Context, _ *mcp.CallToolRequest, input SelectorInput) (*mcp.CallToolResult, NodeOutput, error) {
	if err := s.ready(ctx); err != nil {
		return nil, NodeOutput{}, err
	}
	node, err := s.query.Resolve(ctx, input.Selector)
	return nil, NodeOutput{Node: node}, err
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
	return nil, result, err
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
	return nil, result, err
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
	return nil, result, err
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
	return nil, result, err
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
			graph.EdgeRequests}, input.Limit)
	return nil, result, err
}

type StatusInput struct{}

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
