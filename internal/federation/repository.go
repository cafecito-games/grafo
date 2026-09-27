package federation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/semantic"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type member struct {
	project    indexer.Project
	repository graph.Repository
}

// Repository presents multiple branch-specific SQLite indexes as one read-only
// graph and resolves explicit external targets against declarations in peers.
type Repository struct {
	members []member
}

var _ graph.ReadRepository = (*Repository)(nil)
var _ graph.CatalogRepository = (*Repository)(nil)
var _ semantic.Repository = (*Repository)(nil)
var _ sourcecontext.ProjectLocator = (*Repository)(nil)

func Open(ctx context.Context, paths []string) (*Repository, error) {
	if len(paths) < 2 {
		return nil, fmt.Errorf("federation requires at least two repository paths")
	}
	result := &Repository{}
	seen := map[string]bool{}
	for _, path := range paths {
		project, err := indexer.DiscoverProject(ctx, strings.TrimSpace(path))
		if err != nil {
			result.Close()
			return nil, err
		}
		if seen[project.IndexPath] {
			continue
		}
		seen[project.IndexPath] = true
		if _, err := os.Stat(project.IndexPath); errors.Is(err, os.ErrNotExist) {
			result.Close()
			return nil, fmt.Errorf("repository %s branch %q has no index; run 'grafo index %s'", project.Name, project.Branch, project.Root)
		} else if err != nil {
			result.Close()
			return nil, err
		}
		repository, err := sqlite.Open(ctx, project.IndexPath)
		if err != nil {
			result.Close()
			return nil, err
		}
		result.members = append(result.members, member{project: project, repository: repository})
	}
	if len(result.members) < 2 {
		result.Close()
		return nil, fmt.Errorf("federation requires at least two distinct indexes")
	}
	sort.Slice(result.members, func(i, j int) bool { return result.members[i].project.Root < result.members[j].project.Root })
	return result, nil
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
		repository, ok := item.repository.(semantic.Repository)
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

func (r *Repository) EmbeddingHashes(ctx context.Context, model string) (map[string]string, error) {
	result := map[string]string{}
	for _, item := range r.members {
		repository, ok := item.repository.(semantic.Repository)
		if !ok {
			return nil, fmt.Errorf("repository %s does not support embeddings", item.project.Name)
		}
		hashes, err := repository.EmbeddingHashes(ctx, model)
		if err != nil {
			return nil, err
		}
		for id, hash := range hashes {
			result[id] = hash
		}
	}
	return result, nil
}

func (r *Repository) Embeddings(ctx context.Context, model string) ([]semantic.Embedding, error) {
	var result []semantic.Embedding
	for _, item := range r.members {
		repository, ok := item.repository.(semantic.Repository)
		if !ok {
			return nil, fmt.Errorf("repository %s does not support embeddings", item.project.Name)
		}
		embeddings, err := repository.Embeddings(ctx, model)
		if err != nil {
			return nil, err
		}
		result = append(result, embeddings...)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NodeID < result[j].NodeID })
	return result, nil
}

func (r *Repository) UpsertEmbedding(ctx context.Context, embedding semantic.Embedding) error {
	for _, item := range r.members {
		if _, err := item.repository.Node(ctx, embedding.NodeID); err != nil {
			continue
		}
		repository, ok := item.repository.(semantic.Repository)
		if !ok {
			return fmt.Errorf("repository %s does not support embeddings", item.project.Name)
		}
		return repository.UpsertEmbedding(ctx, embedding)
	}
	return fmt.Errorf("embedding node %s does not belong to this federation", embedding.NodeID)
}

func (r *Repository) DeleteStaleEmbeddings(ctx context.Context, model string) (int64, error) {
	var removed int64
	for _, item := range r.members {
		repository, ok := item.repository.(semantic.Repository)
		if !ok {
			return removed, fmt.Errorf("repository %s does not support embeddings", item.project.Name)
		}
		count, err := repository.DeleteStaleEmbeddings(ctx, model)
		if err != nil {
			return removed, err
		}
		removed += count
	}
	return removed, nil
}

func (r *Repository) Refresh(ctx context.Context, parsers *parserapi.Registry) error {
	for _, item := range r.members {
		if _, err := indexer.NewService(item.repository, parsers).Run(ctx, item.project, indexer.Options{}); err != nil {
			return fmt.Errorf("refresh %s: %w", item.project.Name, err)
		}
	}
	return nil
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
			if seen[scoped.Node.ID] {
				continue
			}
			seen[scoped.Node.ID] = true
			// Several members may record the same unresolved target, so
			// attributing one of them would invent a home for a name no
			// repository declares.
			scoped.Repository = ""
			if !scoped.Node.External {
				scoped.Repository = item.project.Name
			}
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
	for _, edge := range raw {
		target, err := r.Node(ctx, edge.ToID)
		if err != nil || !target.External {
			result = append(result, edge)
			continue
		}
		candidates, err := r.exactCandidates(ctx, target, edge.Kind)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			result = append(result, edge)
			continue
		}
		for _, candidate := range candidates {
			result = append(result, federatedEdge(edge, candidate.ID))
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
				if candidateAllowed(edge.Kind, target.Kind) {
					result = append(result, federatedEdge(edge, target.ID))
				}
			}
		}
	}
	return uniqueEdges(result), nil
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

func (r *Repository) exactCandidates(ctx context.Context, target graph.Node, relation graph.EdgeKind) ([]graph.Node, error) {
	byID := map[string]graph.Node{}
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
				if node.External || !candidateAllowed(relation, node.Kind) {
					continue
				}
				if node.QualifiedName == target.QualifiedName || node.Name == target.QualifiedName || node.QualifiedName == target.Name || node.Name == target.Name {
					byID[node.ID] = node
				}
			}
		}
	}
	result := make([]graph.Node, 0, len(byID))
	for _, node := range byID {
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].QualifiedName != result[j].QualifiedName {
			return result[i].QualifiedName < result[j].QualifiedName
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func candidateAllowed(relation graph.EdgeKind, kind graph.NodeKind) bool {
	switch relation {
	case graph.EdgeCalls, graph.EdgePasses, graph.EdgeHandledBy:
		return kind == graph.KindFunction || kind == graph.KindMethod
	case graph.EdgeRequests:
		return kind == graph.KindEndpoint
	case graph.EdgeReadsConfig:
		return kind == graph.KindConfigKey
	case graph.EdgeReads, graph.EdgeWrites:
		return kind == graph.KindTable || kind == graph.KindView
	case graph.EdgeReferences:
		return kind == graph.KindConfigKey || kind == graph.KindTable || kind == graph.KindView
	case graph.EdgeExtends, graph.EdgeImplements, graph.EdgeEmbeds:
		return kind == graph.KindType || kind == graph.KindClass || kind == graph.KindInterface
	case graph.EdgeImports, graph.EdgeDependsOn:
		return kind == graph.KindModule || kind == graph.KindPackage
	default:
		return true
	}
}

func federatedEdge(edge graph.Edge, targetID string) graph.Edge {
	edge.ToID = targetID
	edge.ID = graph.EdgeID(edge.FactID, targetID)
	properties := make(map[string]string, len(edge.Properties)+1)
	for key, value := range edge.Properties {
		properties[key] = value
	}
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
