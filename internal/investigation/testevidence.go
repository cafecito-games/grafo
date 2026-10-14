package investigation

import (
	"context"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

// testBudgetAdapter is intentionally not a second coverage engine: it bounds
// and scopes the existing engine's evidence reads. No adjacency fallback exists.
type testBudgetAdapter struct {
	graph.QueryRepository
	relations  graph.RelationEdgeRepository
	request    Request
	repository string
	usage      *Usage
}

func (a *testBudgetAdapter) RelationEdges(ctx context.Context, q graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	out := graph.RelationEdgePage{}
	for _, kind := range q.Relations {
		remaining := a.request.MaxWork - a.usage.Work
		if remaining < 3 {
			out.Truncated = true
			break
		}
		limit := min(q.Limit, remaining-2)
		a.usage.Work += 2
		a.usage.GraphQueries++
		p, err := a.relations.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: q.SubjectID, Direction: q.Direction, Relations: []graph.EdgeKind{kind}, Limit: limit})
		if err != nil {
			return out, err
		}
		a.usage.Work += len(p.Items)
		out.Truncated = out.Truncated || p.Truncated
		for _, item := range p.Items {
			repo := item.Repository
			if repo == "" {
				repo = a.repository
			}
			if !within(item.Counterpart, repo, a.request) || repo != a.repository || a.request.Component != "" {
				out.Truncated = true
				continue
			}
			out.Items = append(out.Items, item)
		}
	}
	return out, nil
}
func (s *Service) testEvidence(ctx context.Context, m graph.DeclarationMatch, r Request, out *Report, addNode func(graph.Node, string), edges map[string]bool) {
	if m.Node.Kind != graph.KindFunction && m.Node.Kind != graph.KindMethod || graph.IsTestSupportNode(m.Node) {
		return
	}
	relations, ok := s.localRelations(m.Repository)
	if !ok {
		return
	}
	adapter := &testBudgetAdapter{relations: relations, request: r, repository: m.Repository, usage: &out.Usage}
	report, err := query.NewService(adapter).FindTestsForCallable(ctx, m.Node, query.TestCoverageOptions{Depth: 4, Limit: 8})
	if err != nil {
		out.Truncated = true
		return
	}
	out.Truncated = out.Truncated || report.Truncated
	for index, match := range report.Matches {
		if index >= 1 {
			out.Truncated = true
			break
		}
		for _, n := range match.Nodes {
			addNode(n, m.Repository)
		}
		for _, e := range match.Edges {
			key := m.Repository + "\x00" + e.ID
			if edges[key] {
				continue
			}
			edges[key] = true
			out.Edges = append(out.Edges, Edge{ID: e.ID, Repository: m.Repository, From: e.FromID, To: e.ToID, Kind: e.Kind, Path: e.Location.Path, Line: e.Location.Line})
		}
		out.Followups = append(out.Followups, Followup{Tool: "get_source", Selector: match.Test.ID, Repository: m.Repository, Reason: "Read this structurally linked test; its presence does not establish runtime coverage or asserted behavior."})
	}
}
