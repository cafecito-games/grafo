package source

import (
	"context"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

// ReaderFunc adapts a bounded excerpt reader to query.SourceReader so impact
// reports can include source without internal/query importing this package,
// which would close an import cycle.
type ReaderFunc func(context.Context, string, graph.NodeKind, int, int) (Excerpt, error)

var _ query.SourceReader = ReaderFunc(nil)
var _ query.SourceReader = (*Service)(nil)

// ReadNodeSource resolves a node ID as a selector and narrows the excerpt to
// the fields internal/query declares. Bounds stay enforced by the reader.
func (f ReaderFunc) ReadNodeSource(ctx context.Context, nodeID string, contextLines, maxLines int) (query.SourceExcerpt, error) {
	excerpt, err := f(ctx, nodeID, "", contextLines, maxLines)
	if err != nil {
		return query.SourceExcerpt{}, err
	}
	return query.SourceExcerpt{
		NodeID:     excerpt.Node.ID,
		Repository: excerpt.Repository,
		Branch:     excerpt.Branch,
		Path:       excerpt.Path,
		StartLine:  excerpt.StartLine,
		EndLine:    excerpt.EndLine,
		Content:    excerpt.Content,
		Truncated:  excerpt.Truncated,
	}, nil
}

// ReadNodeSource lets a Service satisfy query.SourceReader directly.
func (s *Service) ReadNodeSource(ctx context.Context, nodeID string, contextLines, maxLines int) (query.SourceExcerpt, error) {
	return ReaderFunc(s.ReadKind).ReadNodeSource(ctx, nodeID, contextLines, maxLines)
}
