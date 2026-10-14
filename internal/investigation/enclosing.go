package investigation

import (
	"context"
	"github.com/cafecito-games/grafo/internal/graph"
)

func covered(excerpts []Excerpt, node graph.Node, repository string) bool {
	for _, ex := range excerpts {
		if ex.Repository == repository && ex.Path == node.Location.Path && ex.StartLine <= node.Location.Line && ex.EndLine >= node.Location.EndLine {
			return true
		}
	}
	return false
}

// A small lexically enclosing declaration can provide useful lifecycle context.
// Only a recorded declares edge and containing source span authorize expansion.
func (s *Service) enclosingSource(ctx context.Context, m graph.DeclarationMatch, r Request, out *Report, addNode func(graph.Node, string), edges map[string]bool) graph.Node {
	if r.Component != "" || out.Usage.Work+4 > r.MaxWork {
		return m.Node
	}
	relations, ok := s.localRelations(m.Repository)
	if !ok {
		return m.Node
	}
	out.Usage.GraphQueries++
	out.Usage.Work += 2
	page, err := relations.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: m.Node.ID, Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeDeclares}, Limit: 2})
	if err != nil {
		out.Truncated = true
		return m.Node
	}
	out.Usage.Work += len(page.Items)
	out.Truncated = out.Truncated || page.Truncated
	for _, item := range page.Items {
		n := item.Counterpart
		repo := item.Repository
		if repo == "" {
			repo = m.Repository
		}
		if repo != m.Repository || !within(n, repo, r) || n.Kind != graph.KindClass && n.Kind != graph.KindType {
			continue
		}
		if n.Location.Path != m.Node.Location.Path || n.Location.Line < 1 || n.Location.Line > m.Node.Location.Line || n.Location.EndLine < m.Node.Location.EndLine || n.Location.EndLine-n.Location.Line+1 > 160 {
			continue
		}
		addNode(n, repo)
		key := repo + "\x00" + item.Edge.ID
		if !edges[key] {
			edges[key] = true
			out.Edges = append(out.Edges, Edge{ID: item.Edge.ID, Repository: repo, From: item.Edge.FromID, To: item.Edge.ToID, Kind: item.Edge.Kind, Path: item.Edge.Location.Path, Line: item.Edge.Location.Line})
		}
		return n
	}
	return m.Node
}
