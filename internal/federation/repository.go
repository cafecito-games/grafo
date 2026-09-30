package federation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/semantic"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type member struct {
	project    indexer.Project
	repository memberRepository
}

type memberRepository interface {
	graph.ReadRepository
	graph.CatalogRepository
	graph.FileCatalog
	Close() error
}

// Repository presents multiple branch-specific SQLite indexes as one read-only
// graph and resolves explicit external targets against declarations in peers.
type Repository struct {
	members []member
}

var _ graph.ReadRepository = (*Repository)(nil)
var _ graph.CatalogRepository = (*Repository)(nil)
var _ graph.CanonicalMessageRepository = (*Repository)(nil)
var _ semantic.CandidateRepository = (*Repository)(nil)
var _ sourcecontext.ProjectLocator = (*Repository)(nil)

// ReadRepository exposes the federated read surface without refresh, indexing,
// or embedding-write methods.
type ReadRepository struct {
	repository *Repository
}

var _ graph.ReadRepository = (*ReadRepository)(nil)
var _ graph.CatalogRepository = (*ReadRepository)(nil)
var _ graph.TopologyRepository = (*ReadRepository)(nil)
var _ graph.CanonicalMessageRepository = (*ReadRepository)(nil)
var _ semantic.CandidateRepository = (*ReadRepository)(nil)
var _ sourcecontext.ProjectLocator = (*ReadRepository)(nil)

func Open(ctx context.Context, paths []string) (*Repository, error) {
	if len(paths) < 2 {
		return nil, fmt.Errorf("federation requires at least two repository paths")
	}
	result := &Repository{}
	seen := map[string]bool{}
	for _, path := range paths {
		project, err := indexer.DiscoverProject(ctx, strings.TrimSpace(path))
		if err != nil {
			_ = result.Close()
			return nil, err
		}
		if seen[project.IndexPath] {
			continue
		}
		seen[project.IndexPath] = true
		if _, err := os.Stat(project.IndexPath); errors.Is(err, os.ErrNotExist) {
			_ = result.Close()
			return nil, fmt.Errorf("repository %s branch %q has no index; run 'grafo index %s'", project.Name, project.Branch, project.Root)
		} else if err != nil {
			_ = result.Close()
			return nil, err
		}
		repository, err := sqlite.Open(ctx, project.IndexPath)
		if err != nil {
			_ = result.Close()
			return nil, err
		}
		result.members = append(result.members, member{project: project, repository: repository})
	}
	if len(result.members) < 2 {
		_ = result.Close()
		return nil, fmt.Errorf("federation requires at least two distinct indexes")
	}
	sort.Slice(result.members, func(i, j int) bool { return result.members[i].project.Root < result.members[j].project.Root })
	return result, nil
}

// OpenProjects opens the exact already-discovered writable project indexes.
// It is intended for generation-bound auxiliary writes such as embeddings;
// source indexing remains owned by the freshness coordinator.
func OpenProjects(ctx context.Context, projects []indexer.Project) (*Repository, error) {
	if len(projects) < 2 {
		return nil, fmt.Errorf("federation requires at least two distinct indexes")
	}
	ordered := append([]indexer.Project(nil), projects...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Root < ordered[j].Root })
	result := &Repository{}
	seen := map[string]bool{}
	for _, project := range ordered {
		if seen[project.IndexPath] {
			_ = result.Close()
			return nil, fmt.Errorf("federation projects share index %s", project.IndexPath)
		}
		seen[project.IndexPath] = true
		repository, err := sqlite.Open(ctx, project.IndexPath)
		if err != nil {
			_ = result.Close()
			return nil, err
		}
		result.members = append(result.members, member{project: project, repository: repository})
	}
	return result, nil
}

// OpenReadOnly opens existing compatible indexes without migrations or write
// capabilities and federates their query surfaces.
func OpenReadOnly(ctx context.Context, paths []string) (*ReadRepository, error) {
	if len(paths) < 2 {
		return nil, fmt.Errorf("federation requires at least two repository paths")
	}
	result := &Repository{}
	seen := map[string]bool{}
	for _, path := range paths {
		project, err := indexer.DiscoverProject(ctx, strings.TrimSpace(path))
		if err != nil {
			_ = result.Close()
			return nil, err
		}
		if seen[project.IndexPath] {
			continue
		}
		seen[project.IndexPath] = true
		if _, err := os.Stat(project.IndexPath); errors.Is(err, os.ErrNotExist) {
			_ = result.Close()
			return nil, fmt.Errorf("repository %s branch %q has no index; run 'grafo index %s'", project.Name, project.Branch, project.Root)
		} else if err != nil {
			_ = result.Close()
			return nil, err
		}
		repository, err := sqlite.OpenReadOnly(ctx, project.IndexPath)
		if err != nil {
			_ = result.Close()
			return nil, err
		}
		result.members = append(result.members, member{project: project, repository: repository})
	}
	if len(result.members) < 2 {
		_ = result.Close()
		return nil, fmt.Errorf("federation requires at least two distinct indexes")
	}
	sort.Slice(result.members, func(i, j int) bool { return result.members[i].project.Root < result.members[j].project.Root })
	return &ReadRepository{repository: result}, nil
}

// OpenReadOnlyProjects opens the exact discovered project generations supplied
// by a freshness coordinator. Unlike OpenReadOnly, it does not rediscover roots
// between the coordinator's stable post-refresh probe and publication.
func OpenReadOnlyProjects(ctx context.Context, projects []indexer.Project) (*ReadRepository, error) {
	if len(projects) < 2 {
		return nil, fmt.Errorf("federation requires at least two distinct indexes")
	}
	ordered := append([]indexer.Project(nil), projects...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Root < ordered[j].Root })
	result := &Repository{}
	seen := map[string]bool{}
	for _, project := range ordered {
		if seen[project.IndexPath] {
			continue
		}
		seen[project.IndexPath] = true
		repository, err := sqlite.OpenReadOnly(ctx, project.IndexPath)
		if err != nil {
			_ = result.Close()
			return nil, err
		}
		result.members = append(result.members, member{project: project, repository: repository})
	}
	if len(result.members) < 2 {
		_ = result.Close()
		return nil, fmt.Errorf("federation requires at least two distinct indexes")
	}
	return &ReadRepository{repository: result}, nil
}

func (r *Repository) Close() error {
	var joined error
	for _, item := range r.members {
		joined = errors.Join(joined, item.repository.Close())
	}
	r.members = nil
	return joined
}

func (r *Repository) Projects() []indexer.Project {
	result := make([]indexer.Project, 0, len(r.members))
	for _, item := range r.members {
		result = append(result, item.project)
	}
	return result
}

func (r *Repository) ProjectForNode(ctx context.Context, id string) (indexer.Project, error) {
	for _, item := range r.members {
		if _, err := item.repository.Node(ctx, id); err == nil {
			return item.project, nil
		}
	}
	return indexer.Project{}, fmt.Errorf("node %s does not belong to this federation", id)
}

func (r *Repository) CandidateNodes(ctx context.Context) ([]graph.Node, error) {
	var result []graph.Node
	for _, item := range r.members {
		repository, ok := item.repository.(semantic.CandidateRepository)
		if !ok {
			return nil, fmt.Errorf("repository %s does not support semantic candidates", item.project.Name)
		}
		nodes, err := repository.CandidateNodes(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nodes...)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].QualifiedName != result[j].QualifiedName {
			return result[i].QualifiedName < result[j].QualifiedName
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (r *Repository) Refresh(ctx context.Context, parsers *parserapi.Registry) error {
	_, err := r.RefreshReports(ctx, parsers, indexer.Options{ReportDetail: indexer.ReportWithoutCounts})
	return err
}

// RefreshReports refreshes members in canonical order and returns only reports
// for successful members. A failure is terminal and is never represented as a
// successful federation refresh.
func (r *Repository) RefreshReports(ctx context.Context, parsers *parserapi.Registry, options indexer.Options) ([]indexer.Report, error) {
	reports := make([]indexer.Report, 0, len(r.members))
	for _, item := range r.members {
		indexed, ok := item.repository.(graph.IndexRepository)
		if !ok {
			return nil, fmt.Errorf("repository %s does not support index refresh", item.project.Name)
		}
		options.ReportDetail = indexer.ReportWithoutCounts
		report, err := indexer.NewService(indexed, parsers).Run(ctx, item.project, options)
		if err != nil {
			return nil, fmt.Errorf("refresh %s: %w", item.project.Name, err)
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func (r *ReadRepository) Close() error                { return r.repository.Close() }
func (r *ReadRepository) Projects() []indexer.Project { return r.repository.Projects() }
func (r *ReadRepository) Meta(ctx context.Context, key string) (string, error) {
	return r.repository.Meta(ctx, key)
}
func (r *ReadRepository) ProjectForNode(ctx context.Context, id string) (indexer.Project, error) {
	return r.repository.ProjectForNode(ctx, id)
}
func (r *ReadRepository) CandidateNodes(ctx context.Context) ([]graph.Node, error) {
	return r.repository.CandidateNodes(ctx)
}
func (r *ReadRepository) SearchNodes(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	return r.repository.SearchNodes(ctx, term, limit)
}
func (r *ReadRepository) Repositories(ctx context.Context) ([]string, error) {
	return r.repository.Repositories(ctx)
}
func (r *ReadRepository) ListNodesByKind(ctx context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	return r.repository.ListNodesByKind(ctx, request)
}
func (r *ReadRepository) CanonicalMessages(ctx context.Context, request graph.CanonicalMessageQuery) (graph.CanonicalMessagePage, error) {
	return r.repository.CanonicalMessages(ctx, request)
}
func (r *ReadRepository) MatchNodes(ctx context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
	return r.repository.MatchNodes(ctx, request)
}
func (r *ReadRepository) Node(ctx context.Context, id string) (graph.Node, error) {
	return r.repository.Node(ctx, id)
}
func (r *ReadRepository) EdgesFrom(ctx context.Context, id string) ([]graph.Edge, error) {
	return r.repository.EdgesFrom(ctx, id)
}
func (r *ReadRepository) EdgesTo(ctx context.Context, id string) ([]graph.Edge, error) {
	return r.repository.EdgesTo(ctx, id)
}
func (r *ReadRepository) RelationEdges(ctx context.Context, request graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	return r.repository.RelationEdges(ctx, request)
}
func (r *ReadRepository) Counts(ctx context.Context) (graph.Counts, error) {
	return r.repository.Counts(ctx)
}

func (r *Repository) SearchNodes(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	if limit <= 0 {
		limit = 20
	}
	byID := map[string]graph.Node{}
	for _, item := range r.members {
		nodes, err := item.repository.SearchNodes(ctx, term, limit)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			if existing, ok := byID[node.ID]; !ok || (existing.External && !node.External) {
				byID[node.ID] = node
			}
		}
	}
	result := make([]graph.Node, 0, len(byID))
	for _, node := range byID {
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool {
		ri, rj := matchRank(result[i], term), matchRank(result[j], term)
		if ri != rj {
			return ri < rj
		}
		if result[i].QualifiedName != result[j].QualifiedName {
			return result[i].QualifiedName < result[j].QualifiedName
		}
		return result[i].ID < result[j].ID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// Repositories names every indexed repository in this federation.
func (r *Repository) Repositories(_ context.Context) ([]string, error) {
	result := make([]string, 0, len(r.members))
	for _, item := range r.members {
		result = append(result, item.project.Name)
	}
	sort.Strings(result)
	return result, nil
}

// ListNodesByKind enumerates catalog nodes across members and attributes each
// one to the repository that declares it. The per-kind bound applies to every
// member, so the caller still applies its own total bound.
func (r *Repository) ListNodesByKind(ctx context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	result := []graph.ScopedNode{}
	seen := map[string]bool{}
	for _, item := range r.members {
		if request.Repository != "" && request.Repository != item.project.Name {
			continue
		}
		lister, ok := item.repository.(graph.NodeListRepository)
		if !ok {
			return nil, fmt.Errorf("repository %s does not support node catalogs", item.project.Name)
		}
		member := request
		member.Repository = ""
		scopedNodes, err := lister.ListNodesByKind(ctx, member)
		if err != nil {
			return nil, err
		}
		for _, scoped := range scopedNodes {
			// Several members may record the same unresolved target, so
			// attributing one of them would invent a home for a name no
			// repository declares.
			scoped.Repository = ""
			if !scoped.Node.External {
				scoped.Repository = item.project.Name
			}
			key := scoped.Repository + "\x00" + scoped.Node.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, scoped)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Node.QualifiedName != result[j].Node.QualifiedName {
			return result[i].Node.QualifiedName < result[j].Node.QualifiedName
		}
		if result[i].Node.Kind != result[j].Node.Kind {
			return result[i].Node.Kind < result[j].Node.Kind
		}
		if result[i].Repository != result[j].Repository {
			return result[i].Repository < result[j].Repository
		}
		return result[i].Node.ID < result[j].Node.ID
	})
	return result, nil
}

// CanonicalMessages merges already-filtered member pages, then applies one
// repository-qualified deterministic global bound.
func (r *Repository) CanonicalMessages(ctx context.Context, request graph.CanonicalMessageQuery) (graph.CanonicalMessagePage, error) {
	if err := request.Validate(); err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	page := graph.CanonicalMessagePage{Items: []graph.ScopedNode{}}
	seen := map[string]bool{}
	for _, item := range r.members {
		if err := ctx.Err(); err != nil {
			return graph.CanonicalMessagePage{}, err
		}
		if request.Repository != "" && request.Repository != item.project.Name {
			continue
		}
		catalog, ok := item.repository.(graph.CanonicalMessageRepository)
		if !ok {
			return graph.CanonicalMessagePage{}, fmt.Errorf("repository %s does not support canonical message catalogs", item.project.Name)
		}
		memberRequest := request
		memberRequest.Repository = ""
		memberPage, err := catalog.CanonicalMessages(ctx, memberRequest)
		if err != nil {
			return graph.CanonicalMessagePage{}, err
		}
		page.Truncated = page.Truncated || memberPage.Truncated
		for _, scoped := range memberPage.Items {
			scoped.Repository = item.project.Name
			key := scoped.Repository + "\x00" + scoped.Node.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			page.Items = append(page.Items, scoped)
		}
	}
	sort.Slice(page.Items, func(i, j int) bool {
		if page.Items[i].Repository != page.Items[j].Repository {
			return page.Items[i].Repository < page.Items[j].Repository
		}
		if page.Items[i].Node.QualifiedName != page.Items[j].Node.QualifiedName {
			return page.Items[i].Node.QualifiedName < page.Items[j].Node.QualifiedName
		}
		return page.Items[i].Node.ID < page.Items[j].Node.ID
	})
	if len(page.Items) > request.Limit {
		page.Truncated = true
		page.Items = page.Items[:request.Limit]
	}
	return page, nil
}

// MatchNodes merges the per-member match evidence for a selector. Only the
// strongest evidence any member reported contributes - the strongest level, and
// within it a local group over an external fallback - because a member holding
// stronger evidence always reports it itself. Totals sum across the contributing
// members so a federated ambiguity still states the true count.
func (r *Repository) MatchNodes(ctx context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
	selector := strings.TrimSpace(request.Selector)
	if selector == "" {
		return graph.NodeMatchGroup{}, nil
	}
	limit := request.Limit
	if limit <= 0 {
		limit = sqlite.DefaultNodeMatchLimit
	}
	request.Selector, request.Limit = selector, limit
	merged := graph.NodeMatchGroup{Level: graph.MatchNone}
	byID := map[string]graph.Node{}
	complete := true
	for _, item := range r.members {
		if request.Repository != "" && request.Repository != item.project.Name {
			continue
		}
		memberRequest := request
		memberRequest.Repository = ""
		group, err := item.repository.MatchNodes(ctx, memberRequest)
		if err != nil {
			return graph.NodeMatchGroup{}, err
		}
		if group.Total == 0 || merged.StrongerThan(group) {
			continue
		}
		if group.StrongerThan(merged) {
			merged = graph.NodeMatchGroup{Level: group.Level, External: group.External}
			byID = map[string]graph.Node{}
			complete = true
		}
		if group.Truncated() {
			complete = false
		}
		merged.Total += group.Total
		merged.Strict += group.Strict
		for _, node := range group.Nodes {
			if existing, ok := byID[node.ID]; !ok || (existing.External && !node.External) {
				byID[node.ID] = node
			}
		}
	}
	if merged.Level == graph.MatchNone {
		return graph.NodeMatchGroup{}, nil
	}
	merged.Nodes = make([]graph.Node, 0, len(byID))
	for _, node := range byID {
		merged.Nodes = append(merged.Nodes, node)
	}
	graph.SortNodeMatches(merged.Level, selector, merged.Nodes)
	// A node indexed by several members is one candidate, not several. When every
	// contributing member listed its complete set, the deduplicated list is the
	// authority for the totals; otherwise summed member totals are the best
	// available bound and stay an over-count rather than an under-count.
	if complete {
		merged.Total = len(merged.Nodes)
		merged.Strict = 0
		for _, node := range merged.Nodes {
			if graph.StrictMatch(merged.Level, selector, node) {
				merged.Strict++
			}
		}
	}
	if len(merged.Nodes) > limit {
		merged.Nodes = merged.Nodes[:limit]
	}
	if merged.Total < len(merged.Nodes) {
		merged.Total = len(merged.Nodes)
	}
	return merged, nil
}

func (r *Repository) Node(ctx context.Context, id string) (graph.Node, error) {
	var firstErr error
	for _, item := range r.members {
		node, err := item.repository.Node(ctx, id)
		if err == nil {
			return node, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return graph.Node{}, firstErr
}

func (r *Repository) EdgesFrom(ctx context.Context, id string) ([]graph.Edge, error) {
	var raw []graph.Edge
	for _, item := range r.members {
		edges, err := item.repository.EdgesFrom(ctx, id)
		if err != nil {
			return nil, err
		}
		raw = append(raw, edges...)
	}
	var result []graph.Edge
	var httpProjection *federatedHTTPProjection
	for _, edge := range raw {
		if !federationAllowed(edge, graph.Node{}) {
			result = append(result, edge)
			continue
		}
		target, err := r.Node(ctx, edge.ToID)
		if err != nil || !target.External || !federationAllowed(edge, target) {
			result = append(result, edge)
			continue
		}
		if edge.Kind == graph.EdgeRequests && httpProjection == nil {
			httpProjection, err = r.newFederatedHTTPProjection(ctx)
			if err != nil {
				return nil, err
			}
		}
		candidates, err := r.resolutionCandidates(ctx, target, edge, httpProjection)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			result = append(result, edge)
			continue
		}
		if edge.Kind == graph.EdgeRequests && len(candidates) > 1 {
			result = append(result, federatedAmbiguousEdge(edge))
			continue
		}
		for _, candidate := range candidates {
			result = append(result, federatedEdge(edge, candidate.Node.ID))
		}
	}
	return uniqueEdges(result), nil
}

func (r *Repository) EdgesTo(ctx context.Context, id string) ([]graph.Edge, error) {
	var result []graph.Edge
	for _, item := range r.members {
		edges, err := item.repository.EdgesTo(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, edges...)
	}
	target, err := r.Node(ctx, id)
	if err == nil && !target.External {
		for _, item := range r.members {
			externalRepository, ok := item.repository.(graph.ExternalEdgeRepository)
			if !ok {
				continue
			}
			edges, err := externalRepository.ExternalEdgesTo(ctx, target)
			if err != nil {
				return nil, err
			}
			for _, edge := range edges {
				if edge.Kind != graph.EdgeRequests && federationAllowed(edge, graph.Node{}) && candidateAllowed(edge.Kind, target) {
					result = append(result, federatedEdge(edge, target.ID))
				}
			}
		}
		projection, projectionErr := r.newFederatedHTTPProjection(ctx)
		if projectionErr != nil {
			return nil, projectionErr
		}
		incoming, incomingErr := r.incomingFederatedHTTPEdges(ctx, target, projection)
		if incomingErr != nil {
			return nil, incomingErr
		}
		result = append(result, incoming...)
	}
	return uniqueEdges(result), nil
}

// RelationEdges merges bounded hydrated pages from every member, applies the
// same cross-repository projection as traversal adjacency, then deduplicates
// and enforces the global per-relation bound on final edge identities.
func (r *Repository) RelationEdges(ctx context.Context, request graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	if err := request.Validate(); err != nil {
		return graph.RelationEdgePage{}, err
	}
	items := []graph.HydratedRelationEdge{}
	truncated := false
	for _, member := range r.members {
		page, err := memberRelationEdges(ctx, member.repository, request)
		if err != nil {
			return graph.RelationEdgePage{}, err
		}
		for index := range page.Items {
			page.Items[index].Repository = member.project.Name
		}
		truncated = truncated || page.Truncated
		items = append(items, page.Items...)
	}

	switch request.Direction {
	case graph.OutgoingRelations:
		projected := make([]graph.HydratedRelationEdge, 0, len(items))
		var httpProjection *federatedHTTPProjection
		for _, item := range items {
			if !federationAllowed(item.Edge, item.Counterpart) {
				projected = append(projected, item)
				continue
			}
			if !item.Counterpart.External {
				projected = append(projected, item)
				continue
			}
			if item.Edge.Kind == graph.EdgeRequests && httpProjection == nil {
				var projectionErr error
				httpProjection, projectionErr = r.newFederatedHTTPProjection(ctx)
				if projectionErr != nil {
					return graph.RelationEdgePage{}, projectionErr
				}
			}
			candidates, err := r.resolutionCandidates(ctx, item.Counterpart, item.Edge, httpProjection)
			if err != nil {
				return graph.RelationEdgePage{}, err
			}
			if len(candidates) == 0 {
				projected = append(projected, item)
				continue
			}
			if item.Edge.Kind == graph.EdgeRequests && len(candidates) > 1 {
				item.Edge = federatedAmbiguousEdge(item.Edge)
				projected = append(projected, item)
				continue
			}
			for _, candidate := range candidates {
				projected = append(projected, graph.HydratedRelationEdge{
					Edge: federatedEdge(item.Edge, candidate.Node.ID), Counterpart: candidate.Node, Repository: candidate.Repository,
				})
			}
		}
		items = projected
	case graph.IncomingRelations:
		target, found, err := r.preferLocalNode(ctx, request.SubjectID)
		if err != nil {
			return graph.RelationEdgePage{}, err
		}
		if found && !target.External {
			relations := make([]graph.EdgeKind, 0, len(request.Relations))
			seen := map[graph.EdgeKind]bool{}
			includeRequests := false
			for _, relation := range request.Relations {
				if relation == graph.EdgeRequests {
					includeRequests = true
					continue
				}
				if !seen[relation] && candidateAllowed(relation, target) {
					seen[relation] = true
					relations = append(relations, relation)
				}
			}
			if len(relations) > 0 {
				for _, member := range r.members {
					external, err := matchingExternalNodes(ctx, member.repository, target)
					if err != nil {
						return graph.RelationEdgePage{}, err
					}
					for _, unresolved := range external {
						page, err := memberRelationEdges(ctx, member.repository, graph.RelationEdgeQuery{
							SubjectID: unresolved.ID, Direction: graph.IncomingRelations,
							Relations: relations, Limit: request.Limit,
						})
						if err != nil {
							return graph.RelationEdgePage{}, err
						}
						for index := range page.Items {
							page.Items[index].Repository = member.project.Name
						}
						truncated = truncated || page.Truncated
						for _, item := range page.Items {
							if !federationAllowed(item.Edge, unresolved) {
								continue
							}
							item.Edge = federatedEdge(item.Edge, target.ID)
							items = append(items, item)
						}
					}
				}
			}
			if includeRequests && candidateAllowed(graph.EdgeRequests, target) {
				projection, projectionErr := r.newFederatedHTTPProjection(ctx)
				if projectionErr != nil {
					return graph.RelationEdgePage{}, projectionErr
				}
				projected, projectedTruncated, projectionErr := r.incomingFederatedHTTPRelations(ctx, target, projection, request.Limit)
				if projectionErr != nil {
					return graph.RelationEdgePage{}, projectionErr
				}
				items = append(items, projected...)
				truncated = truncated || projectedTruncated
			}
		}
	}
	return boundRelationEdges(items, request.Limit, truncated), nil
}

func memberRelationEdges(ctx context.Context, repository memberRepository,
	request graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	loader, ok := repository.(graph.RelationEdgeRepository)
	if !ok {
		return graph.RelationEdgePage{}, fmt.Errorf("repository does not support bounded relation edges")
	}
	return loader.RelationEdges(ctx, request)
}

func (r *Repository) Counts(ctx context.Context) (graph.Counts, error) {
	result := graph.Counts{ByKind: map[string]int{}, ByEdge: map[string]int{}}
	for _, item := range r.members {
		counts, err := item.repository.Counts(ctx)
		if err != nil {
			return result, err
		}
		result.Files += counts.Files
		result.Nodes += counts.Nodes
		result.Facts += counts.Facts
		result.Edges += counts.Edges
		result.External += counts.External
		for kind, count := range counts.ByKind {
			result.ByKind[kind] += count
		}
		for kind, count := range counts.ByEdge {
			result.ByEdge[kind] += count
		}
	}
	return result, nil
}

func (r *Repository) Meta(ctx context.Context, key string) (string, error) {
	var values []string
	for _, item := range r.members {
		value, err := item.repository.Meta(ctx, key)
		if err != nil {
			return "", err
		}
		if value != "" {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	return strings.Join(values, ","), nil
}

func (r *Repository) exactCandidates(ctx context.Context, target graph.Node, relation graph.EdgeKind) ([]graph.ScopedNode, error) {
	byID := map[string]graph.ScopedNode{}
	for _, term := range []string{target.QualifiedName, target.Name} {
		if term == "" {
			continue
		}
		for _, item := range r.members {
			nodes, err := item.repository.SearchNodes(ctx, term, 100)
			if err != nil {
				return nil, err
			}
			for _, node := range nodes {
				if node.External || !candidateAllowed(relation, node) {
					continue
				}
				if node.QualifiedName == target.QualifiedName || node.Name == target.QualifiedName || node.QualifiedName == target.Name || node.Name == target.Name {
					key := item.project.Name + "\x00" + node.ID
					byID[key] = graph.ScopedNode{Repository: item.project.Name, Node: node}
				}
			}
		}
	}
	result := make([]graph.ScopedNode, 0, len(byID))
	for _, node := range byID {
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Repository != result[j].Repository {
			return result[i].Repository < result[j].Repository
		}
		if result[i].Node.QualifiedName != result[j].Node.QualifiedName {
			return result[i].Node.QualifiedName < result[j].Node.QualifiedName
		}
		return result[i].Node.ID < result[j].Node.ID
	})
	return result, nil
}

func (r *Repository) preferLocalNode(ctx context.Context, id string) (graph.Node, bool, error) {
	var fallback graph.Node
	for _, item := range r.members {
		node, err := item.repository.Node(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return graph.Node{}, false, err
		}
		if !node.External {
			return node, true, nil
		}
		fallback = node
	}
	return fallback, fallback.ID != "", nil
}

func matchingExternalNodes(ctx context.Context, repository memberRepository, target graph.Node) ([]graph.Node, error) {
	matcher, ok := repository.(graph.ExternalNodeRepository)
	if !ok {
		return nil, fmt.Errorf("repository does not support external node matching")
	}
	return matcher.ExternalNodesMatching(ctx, target)
}

type federatedHTTPCandidate struct {
	scoped graph.ScopedNode
	match  httpmodel.EndpointCandidate
}

type federatedHTTPProjection struct {
	candidates []federatedHTTPCandidate
	catalog    httpmodel.CandidateCatalog
}

func (r *Repository) newFederatedHTTPProjection(ctx context.Context) (*federatedHTTPProjection, error) {
	projection := &federatedHTTPProjection{}
	matches := []httpmodel.EndpointCandidate{}
	for _, item := range r.members {
		nodes, err := item.repository.ListNodesByKind(ctx, graph.NodeListQuery{
			Kinds: []graph.NodeKind{graph.KindEndpoint}, Visibility: graph.LocalNodes,
		})
		if err != nil {
			return nil, err
		}
		for _, scoped := range nodes {
			candidateMethod, candidateRoute, valid := endpointNodeMethodRoute(scoped.Node)
			if !valid {
				continue
			}
			scoped.Repository = item.project.Name
			match := httpmodel.EndpointCandidate{Method: candidateMethod, Route: candidateRoute}
			projection.candidates = append(projection.candidates, federatedHTTPCandidate{scoped: scoped, match: match})
			matches = append(matches, match)
		}
	}
	projection.catalog = httpmodel.NewCandidateCatalog(matches)
	return projection, nil
}

func (projection *federatedHTTPProjection) resolve(edge graph.Edge, target graph.Node) []graph.ScopedNode {
	if projection == nil {
		return nil
	}
	method, requestRoute, ok := requestEdgeMethodRoute(edge, target)
	if !ok {
		return nil
	}
	indexes := projection.catalog.BestCandidateIndexes(method, requestRoute)
	result := make([]graph.ScopedNode, 0, len(indexes))
	for _, index := range indexes {
		result = append(result, projection.candidates[index].scoped)
	}
	return result
}

func (r *Repository) resolutionCandidates(ctx context.Context, target graph.Node, edge graph.Edge,
	projection *federatedHTTPProjection,
) ([]graph.ScopedNode, error) {
	if edge.Kind != graph.EdgeRequests {
		return r.exactCandidates(ctx, target, edge.Kind)
	}
	return projection.resolve(edge, target), nil
}

func (r *Repository) incomingFederatedHTTPEdges(ctx context.Context, target graph.Node,
	projection *federatedHTTPProjection,
) ([]graph.Edge, error) {
	result := []graph.Edge{}
	for _, item := range r.members {
		nodes, err := item.repository.ListNodesByKind(ctx, graph.NodeListQuery{
			Kinds: []graph.NodeKind{graph.KindEndpoint}, Visibility: graph.ExternalNodes,
		})
		if err != nil {
			return nil, err
		}
		for _, unresolved := range nodes {
			edges, err := item.repository.EdgesTo(ctx, unresolved.Node.ID)
			if err != nil {
				return nil, err
			}
			for _, edge := range edges {
				if edge.Kind != graph.EdgeRequests || !federationAllowed(edge, unresolved.Node) {
					continue
				}
				candidates := projection.resolve(edge, unresolved.Node)
				if len(candidates) == 1 && candidates[0].Node.ID == target.ID {
					result = append(result, federatedEdge(edge, target.ID))
				}
			}
		}
	}
	return result, nil
}

func (r *Repository) incomingFederatedHTTPRelations(ctx context.Context, target graph.Node,
	projection *federatedHTTPProjection, limit int,
) ([]graph.HydratedRelationEdge, bool, error) {
	items := []graph.HydratedRelationEdge{}
	truncated := false
	for _, member := range r.members {
		nodes, err := member.repository.ListNodesByKind(ctx, graph.NodeListQuery{
			Kinds: []graph.NodeKind{graph.KindEndpoint}, Visibility: graph.ExternalNodes,
		})
		if err != nil {
			return nil, false, err
		}
		for _, unresolved := range nodes {
			page, err := memberRelationEdges(ctx, member.repository, graph.RelationEdgeQuery{
				SubjectID: unresolved.Node.ID, Direction: graph.IncomingRelations,
				Relations: []graph.EdgeKind{graph.EdgeRequests}, Limit: limit,
			})
			if err != nil {
				return nil, false, err
			}
			truncated = truncated || page.Truncated
			for _, item := range page.Items {
				if !federationAllowed(item.Edge, unresolved.Node) {
					continue
				}
				candidates := projection.resolve(item.Edge, unresolved.Node)
				if len(candidates) != 1 || candidates[0].Node.ID != target.ID {
					continue
				}
				item.Repository = member.project.Name
				item.Edge = federatedEdge(item.Edge, target.ID)
				items = append(items, item)
			}
		}
	}
	return items, truncated, nil
}

func requestEdgeMethodRoute(edge graph.Edge, target graph.Node) (string, httpmodel.Route, bool) {
	method := strings.TrimSpace(edge.Properties["http_method"])
	routeText := strings.TrimSpace(edge.Properties["http_route"])
	label := strings.TrimSpace(target.QualifiedName)
	if label == "" {
		label = strings.TrimSpace(target.Name)
	}
	if fallbackMethod, fallbackRoute, ok := strings.Cut(label, " "); ok {
		if method == "" {
			method = fallbackMethod
		}
		if routeText == "" {
			routeText = fallbackRoute
		}
	}
	normalized, methodErr := httpmodel.NormalizeMethod(method)
	route, routeErr := httpmodel.ParseRoute(routeText)
	return normalized, route, methodErr == nil && routeErr == nil && edge.Properties["http_invalid"] != "true"
}

func endpointNodeMethodRoute(node graph.Node) (string, httpmodel.Route, bool) {
	method := strings.TrimSpace(node.Properties["method"])
	routeText := strings.TrimSpace(node.Properties["route"])
	if fallbackMethod, fallbackRoute, ok := strings.Cut(strings.TrimSpace(node.Name), " "); ok {
		if method == "" {
			method = fallbackMethod
		}
		if routeText == "" {
			routeText = fallbackRoute
		}
	}
	normalized, methodErr := httpmodel.NormalizeMethod(method)
	route, routeErr := httpmodel.ParseRoute(routeText)
	return normalized, route, methodErr == nil && routeErr == nil && node.Properties["http_invalid"] != "true"
}

func candidateAllowed(relation graph.EdgeKind, node graph.Node) bool {
	kind := node.Kind
	switch relation {
	case graph.EdgeCalls, graph.EdgePasses, graph.EdgeHandledBy, graph.EdgeUsesMiddleware:
		return kind == graph.KindFunction || kind == graph.KindMethod
	case graph.EdgeRequests:
		return kind == graph.KindEndpoint
	case graph.EdgeReadsConfig:
		return kind == graph.KindConfigKey
	case graph.EdgeReads, graph.EdgeWrites:
		return kind == graph.KindTable || kind == graph.KindView ||
			(kind == graph.KindField && node.Language == "protobuf")
	case graph.EdgeEncodes, graph.EdgeDecodes:
		return kind == graph.KindType && node.Language == "protobuf"
	case graph.EdgeSends, graph.EdgeReceives:
		return kind == graph.KindTransportOperation
	case graph.EdgeCarries:
		return kind == graph.KindType && node.Language == "protobuf"
	case graph.EdgeReferences:
		return kind == graph.KindConfigKey || kind == graph.KindTable || kind == graph.KindView ||
			kind == graph.KindGodotAutoload || kind == graph.KindGodotScene ||
			kind == graph.KindGodotResource || kind == graph.KindGodotSceneNode
	case graph.EdgeExtends, graph.EdgeImplements, graph.EdgeEmbeds:
		return kind == graph.KindType || kind == graph.KindClass || kind == graph.KindInterface
	case graph.EdgeImports, graph.EdgeDependsOn:
		return kind == graph.KindModule || kind == graph.KindPackage ||
			kind == graph.KindGodotScene || kind == graph.KindGodotResource
	case graph.EdgeInstantiates:
		return kind == graph.KindGodotScene || kind == graph.KindGodotResource
	case graph.EdgeAttachesScript:
		return kind == graph.KindModule || kind == graph.KindClass
	case graph.EdgeAutoloads:
		return kind == graph.KindModule || kind == graph.KindClass || kind == graph.KindGodotScene
	case graph.EdgeUsesInputAction:
		return kind == graph.KindGodotInputAction
	case graph.EdgeInGroup, graph.EdgeUsesGroup:
		return kind == graph.KindGodotNodeGroup
	case graph.EdgeGeneratedFrom:
		return node.Language == "protobuf" && (kind == graph.KindType || kind == graph.KindField)
	default:
		return true
	}
}

func federationAllowed(edge graph.Edge, target graph.Node) bool {
	if edge.Kind != graph.EdgeRequests {
		return true
	}
	routeText := strings.TrimSpace(edge.Properties["http_route"])
	if routeText == "" {
		label := strings.TrimSpace(target.QualifiedName)
		if label == "" {
			label = strings.TrimSpace(target.Name)
		}
		if _, value, ok := strings.Cut(label, " "); ok {
			routeText = value
		}
	}
	route, _ := httpmodel.ParseRoute(routeText)
	contract, err := httpmodel.ParseDestinationContract(edge.Properties, route)
	if err != nil || contract.Authority != httpmodel.AuthorityLocal {
		return false
	}
	return contract.Resolution == "" || contract.Resolution == httpmodel.DestinationUnresolved ||
		contract.Resolution == httpmodel.DestinationAmbiguous
}

func federatedEdge(edge graph.Edge, targetID string) graph.Edge {
	edge.ToID = targetID
	edge.ID = graph.EdgeID(edge.FactID, targetID)
	properties := make(map[string]string, len(edge.Properties)+1)
	for key, value := range edge.Properties {
		properties[key] = value
	}
	if edge.Kind == graph.EdgeRequests {
		properties = httpmodel.WithDestinationEvidence(properties,
			httpmodel.DestinationResolved, httpmodel.EvidenceFederated)
	}
	properties["federated"] = "true"
	edge.Properties = properties
	return edge
}

func federatedAmbiguousEdge(edge graph.Edge) graph.Edge {
	properties := httpmodel.WithDestinationEvidence(edge.Properties,
		httpmodel.DestinationAmbiguous, httpmodel.EvidenceFederated)
	properties["federated"] = "true"
	edge.Properties = properties
	return edge
}

func uniqueEdges(edges []graph.Edge) []graph.Edge {
	byID := map[string]graph.Edge{}
	for _, edge := range edges {
		byID[edge.ID] = edge
	}
	result := make([]graph.Edge, 0, len(byID))
	for _, edge := range byID {
		result = append(result, edge)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].FromID != result[j].FromID {
			return result[i].FromID < result[j].FromID
		}
		if result[i].ToID != result[j].ToID {
			return result[i].ToID < result[j].ToID
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func boundRelationEdges(items []graph.HydratedRelationEdge, limit int, truncated bool) graph.RelationEdgePage {
	byID := map[string]graph.HydratedRelationEdge{}
	for _, item := range items {
		byID[item.Edge.ID] = item
	}
	unique := make([]graph.HydratedRelationEdge, 0, len(byID))
	for _, item := range byID {
		unique = append(unique, item)
	}
	sort.Slice(unique, func(i, j int) bool {
		left, right := unique[i].Edge, unique[j].Edge
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.FromID != right.FromID {
			return left.FromID < right.FromID
		}
		if left.ToID != right.ToID {
			return left.ToID < right.ToID
		}
		return left.ID < right.ID
	})
	page := graph.RelationEdgePage{Items: []graph.HydratedRelationEdge{}, Truncated: truncated}
	kept := map[graph.EdgeKind]int{}
	for _, item := range unique {
		if kept[item.Edge.Kind] >= limit {
			page.Truncated = true
			continue
		}
		kept[item.Edge.Kind]++
		page.Items = append(page.Items, item)
	}
	return page
}

func matchRank(node graph.Node, term string) int {
	term = strings.ToLower(term)
	name, qualified := strings.ToLower(node.Name), strings.ToLower(node.QualifiedName)
	switch {
	case qualified == term:
		return 0
	case name == term:
		return 1
	case strings.HasPrefix(name, term):
		return 2
	case strings.HasPrefix(qualified, term):
		return 3
	default:
		return 4
	}
}

// Member pairs one federated index with its project so callers can build
// per-repository services without reaching into federation internals.
type Member struct {
	Project indexer.Project
	Files   graph.FileCatalog
}

// Members returns each federated index in deterministic order.
func (r *Repository) Members() []Member {
	result := make([]Member, 0, len(r.members))
	for _, item := range r.members {
		result = append(result, Member{Project: item.project, Files: item.repository})
	}
	return result
}

func (r *ReadRepository) Members() []Member { return r.repository.Members() }
