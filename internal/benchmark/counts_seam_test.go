package benchmark

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// countingFailureRepository fails the first failures calls to Counts and
// delegates afterwards. The indexer's own summary call is the first one, so
// failures=1 reproduces a run that reports counts_collected: false while a
// direct recount still succeeds, and a large value reproduces storage that
// stays broken.
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

// withRepositoryWrapper installs a scenario repository wrapper for the duration
// of one test and returns the wrapper that was handed to the harness.
func withRepositoryWrapper(t *testing.T, failures int64) func() *countingFailureRepository {
	t.Helper()
	original := openRepository
	t.Cleanup(func() { openRepository = original })
	var installed *countingFailureRepository
	openRepository = func(ctx context.Context, engine, path string) (graph.Repository, error) {
		repository, err := original(ctx, engine, path)
		if err != nil {
			return nil, err
		}
		installed = &countingFailureRepository{Repository: repository, failures: failures}
		return installed, nil
	}
	return func() *countingFailureRepository { return installed }
}

// TestScenarioRecountsWhenRunDidNotCollectCounts covers the recovery branch: a
// run that reports counts_collected: false still yields real graph totals,
// because the harness recounts directly against the repository.
func TestScenarioRecountsWhenRunDidNotCollectCounts(t *testing.T) {
	wrapper := withRepositoryWrapper(t, 1)
	root := fixtureRepository(t)
	database := filepath.Join(testtemp.Dir(t), "scenario.db")

	scenario, err := executeScenarioRun(context.Background(), "sqlite", ScenarioReport{Name: "recount"}, root, database, 0, nil)
	if err != nil {
		t.Fatalf("recoverable count failure failed the scenario: %v", err)
	}
	if scenario.Status != StatusPassed {
		t.Fatalf("scenario status = %q, want passed: %s", scenario.Status, scenario.Error)
	}
	if scenario.Index.Counts.Files == 0 || scenario.Index.Counts.Nodes == 0 {
		t.Fatalf("recount did not replace the uncollected summary: %#v", scenario.Index.Counts)
	}
	if scenario.Index.Counts.Files == 99 || scenario.Index.Counts.Nodes == 99 {
		t.Fatalf("failed count values leaked into the report: %#v", scenario.Index.Counts)
	}
	if installed := wrapper(); installed == nil || installed.calls.Load() < 2 {
		t.Fatalf("harness did not recount after an uncollected summary: %#v", installed)
	}
}

// TestScenarioFailsWhenRecountAlsoFails covers the surfacing branch: storage
// that cannot count at all must fail the scenario rather than report zeros as
// if they were measured totals.
func TestScenarioFailsWhenRecountAlsoFails(t *testing.T) {
	withRepositoryWrapper(t, 1<<20)
	root := fixtureRepository(t)
	database := filepath.Join(testtemp.Dir(t), "scenario.db")

	scenario, err := executeScenarioRun(context.Background(), "sqlite", ScenarioReport{Name: "recount"}, root, database, 0, nil)
	if err == nil {
		t.Fatal("an unrecoverable count failure did not fail the scenario")
	}
	for _, want := range []string{"collect graph counts for scenario recount", "count query failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("scenario error %q does not mention %q", err, want)
		}
	}
	if scenario.Status != StatusFailed {
		t.Fatalf("scenario status = %q, want failed", scenario.Status)
	}
	if scenario.Index.Counts.Files != 0 || scenario.Index.Counts.Nodes != 0 {
		t.Fatalf("failed count values leaked into the report: %#v", scenario.Index.Counts)
	}
}
