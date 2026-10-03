package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// countingFailureRepository fails the first failures calls to Counts and
// delegates afterwards, so a test can choose between an indexing run that
// reports counts_collected: false and storage that stays broken.
type countingFailureRepository struct {
	graph.Repository
	failures int64
	calls    atomic.Int64
}

func (r *countingFailureRepository) Counts(ctx context.Context) (graph.Counts, error) {
	if r.calls.Add(1) <= r.failures {
		return graph.Counts{Files: 99, Nodes: 99}, errors.New("count query failed")
	}
	return r.Repository.Counts(ctx)
}

func withIndexRepositoryWrapper(t *testing.T, failures int64) {
	t.Helper()
	original := openIndexRepository
	t.Cleanup(func() { openIndexRepository = original })
	openIndexRepository = func(ctx context.Context, path string) (graph.Repository, error) {
		repository, err := original(ctx, path)
		if err != nil {
			return nil, err
		}
		return &countingFailureRepository{Repository: repository, failures: failures}, nil
	}
}

func fixtureProject(t *testing.T) indexer.Project {
	t.Helper()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// Neither test below calls t.Parallel: both reach indexOnce, which resolves the
// package-level openIndexRepository that withIndexRepositoryWrapper replaces, so
// running them concurrently would let one test's wrapper decide the other's
// storage.

// TestIndexOnceRejectsUnverifiableUnchangedRefresh pins the guard that the
// unchanged-refresh comparison reads indexed file totals, so a summary the run
// could not collect is reported rather than compared against zero.
func TestIndexOnceRejectsUnverifiableUnchangedRefresh(t *testing.T) {
	ctx := context.Background()
	project := fixtureProject(t)
	if _, err := indexOnce(ctx, project, false); err != nil {
		t.Fatal(err)
	}
	withIndexRepositoryWrapper(t, 1<<20)
	report, err := indexOnce(ctx, project, true)
	if err == nil {
		t.Fatal("an uncollected summary passed the unchanged-refresh check")
	}
	if !strings.Contains(err.Error(), "graph counts were not collected") {
		t.Fatalf("unexpected error %q", err)
	}
	if report.CountsCollected {
		t.Fatalf("failed counts reported as collected: %#v", report)
	}
	if report.Counts.Files != 0 || report.Counts.Nodes != 0 {
		t.Fatalf("failed count values leaked into the report: %#v", report.Counts)
	}
}

// TestIndexOnceAcceptsUnchangedRefreshWithCollectedCounts proves the guard above
// rejects only an uncollected summary: the same fixture with working storage
// still passes the unchanged-refresh comparison.
func TestIndexOnceAcceptsUnchangedRefreshWithCollectedCounts(t *testing.T) {
	ctx := context.Background()
	project := fixtureProject(t)
	if _, err := indexOnce(ctx, project, false); err != nil {
		t.Fatal(err)
	}
	report, err := indexOnce(ctx, project, true)
	if err != nil {
		t.Fatalf("unchanged refresh failed with working storage: %v", err)
	}
	if !report.CountsCollected || report.Counts.Files == 0 {
		t.Fatalf("unchanged refresh reported no totals: %#v", report)
	}
}
