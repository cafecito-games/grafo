package query

import (
	"context"
	"fmt"
	"github.com/cafecito-games/grafo/internal/graph"
)

// FindTestsForCallable reuses structural coverage semantics for an already
// resolved callable. It deliberately excludes type expansion; callers that
// need member aggregation should use FindTests. Repository adapters can enforce
// a request-wide relation budget without repeating symbol resolution.
func (s *Service) FindTestsForCallable(ctx context.Context, root graph.Node, options TestCoverageOptions) (TestCoverageReport, error) {
	options, err := options.normalized()
	if err != nil {
		return TestCoverageReport{}, err
	}
	if root.Kind != graph.KindFunction && root.Kind != graph.KindMethod || graph.IsTestSupportNode(root) {
		return TestCoverageReport{}, fmt.Errorf("structural test target must be a production callable")
	}
	return s.testCoverage(ctx, root, false, options)
}
