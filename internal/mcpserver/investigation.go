package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cafecito-games/grafo/internal/investigation"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Service) investigate(ctx context.Context, _ *mcp.CallToolRequest, input investigation.Request) (*mcp.CallToolResult, any, error) {
	release, err := s.ready(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	if s.investigationSource == nil {
		return nil, nil, fmt.Errorf("investigation source access is disabled")
	}
	// Bind both graph and local source to the generation acquired for this call.
	sources := s.investigationSource(s.repository, s.projects)
	if sources == nil {
		return nil, nil, fmt.Errorf("investigation source access is unavailable for this generation")
	}
	report, err := investigation.New(s.repository, sources).Investigate(ctx, input)
	if err != nil {
		return nil, nil, err
	}
	content, err := json.Marshal(report)
	if err != nil {
		return nil, nil, err
	}
	// Returning no typed output prevents the SDK from duplicating the complete
	// evidence into structuredContent. The budget covers these UTF-8 report
	// bytes, before JSON transport escaping and the small protocol envelope.
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(content)}}}, nil, nil
}
