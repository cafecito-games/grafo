package federation

import (
	"context"
	"fmt"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/lexical"
)

func (r *ReadRepository) SearchDeclarations(ctx context.Context, q graph.DeclarationQuery) (graph.DeclarationPage, error) {
	return r.repository.SearchDeclarations(ctx, q)
}

// SearchDeclarations shares work ceilings across members rather than multiplying
// each member's allowance. Repository order is stable; exhaustion is explicit.
func (r *Repository) SearchDeclarations(ctx context.Context, q graph.DeclarationQuery) (graph.DeclarationPage, error) {
	page := graph.DeclarationPage{Matches: []graph.DeclarationMatch{}}
	if q.Limit <= 0 || q.MaxPostings <= 0 || q.MaxCandidates <= 0 {
		return page, fmt.Errorf("declaration limits must be positive")
	}
	if q.MaxMetadata < 0 {
		return page, fmt.Errorf("declaration metadata limit must not be negative")
	}
	if q.MaxMetadata == 0 {
		q.MaxMetadata = graph.DefaultDeclarationMetadata
	}
	supported, selected := false, q.Repository == ""
	for _, member := range r.members {
		if page.MetadataReads >= q.MaxMetadata {
			page.Truncated = true
			break
		}
		page.MetadataReads++
		if q.Repository != "" && q.Repository != member.project.Name {
			continue
		}
		selected = true
		repo, ok := member.repository.(graph.DeclarationRepository)
		if !ok {
			return page, fmt.Errorf("repository %s has no lexical retrieval capability", member.project.Name)
		}
		supported = true
		if page.PostingsRead >= q.MaxPostings || page.CandidatesRead >= q.MaxCandidates {
			page.Truncated = true
			break
		}
		request := q
		request.Repository = ""
		request.MaxPostings -= page.PostingsRead
		request.MaxCandidates -= page.CandidatesRead
		request.MaxMetadata = q.MaxMetadata - page.MetadataReads
		if request.MaxMetadata == 0 {
			page.Truncated = true
			break
		}
		result, err := repo.SearchDeclarations(ctx, request)
		if err != nil {
			return page, err
		}
		page.MetadataReads += result.MetadataReads
		page.PostingsRead += result.PostingsRead
		page.CandidatesRead += result.CandidatesRead
		page.Truncated = page.Truncated || result.Truncated
		for _, match := range result.Matches {
			match.Repository = member.project.Name
			page.Matches = append(page.Matches, match)
		}
	}
	if !selected && !page.Truncated {
		// An unmatched selector would otherwise be indistinguishable from a
		// repository that genuinely holds no matching declaration.
		return page, fmt.Errorf("repository %s is not indexed", q.Repository)
	}
	if !supported && q.Repository == "" && !page.Truncated {
		return page, fmt.Errorf("no repository has lexical retrieval capability")
	}
	lexical.Sort(page.Matches)
	if len(page.Matches) > q.Limit {
		page.Matches = page.Matches[:q.Limit]
		page.Truncated = true
	}
	return page, nil
}
