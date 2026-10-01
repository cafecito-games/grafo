package indexer_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// componentCorpus writes a Git corpus whose grafo.yaml declares components, so
// the synthetic workspace owner holds one `contains` fact per component member.
func componentCorpus(t testing.TB) string {
	t.Helper()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "grafo.yaml"), `components:
  - name: server
    roots: [server]
  - name: client
    roots: [client]
`)
	write(t, filepath.Join(root, "go.mod"), "module example.com/convergence\n\ngo 1.26\n")
	for _, area := range []string{"server", "client"} {
		for index := 0; index < 4; index++ {
			write(t, filepath.Join(root, area, fmt.Sprintf("unit%d.go", index)), fmt.Sprintf(`package %s

type server struct{}
type client struct{}

func Use%d() {
	var listener server
	var caller client
	_, _ = listener, caller
	helper%d()
}

func helper%d() {}
`, area, index, (index+1)%4, index))
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid",
		"commit", "-m", "initial")
	return root
}

type convergenceRun struct {
	report     indexer.Report
	boundaries []indexer.BoundaryKind
}

// indexOnce opens the index from scratch so each pass also proves the run
// converges across a repository restart rather than within one process.
func indexOnce(t testing.TB, root string, withBoundary bool) convergenceRun {
	t.Helper()
	ctx := context.Background()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	result := convergenceRun{}
	options := indexer.Options{}
	if withBoundary {
		options.Boundary = func(boundary indexer.Boundary) error {
			result.boundaries = append(result.boundaries, boundary.Kind)
			return nil
		}
	}
	report, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, options)
	if err != nil {
		t.Fatal(err)
	}
	result.report = report
	return result
}

func observed(kinds []indexer.BoundaryKind, want indexer.BoundaryKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

// TestUnchangedRefreshConvergesInOnePass locks the resume_unchanged corpus
// contract: an unchanged refresh of an indexed corpus reports no reconciliation
// batches, and repeating it never discovers residual work. A boundary hook only
// observes durable work, so installing one must not change what a run performs.
func TestUnchangedRefreshConvergesInOnePass(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		withBoundary bool
	}{
		{name: "without boundary hook"},
		{name: "with boundary hook", withBoundary: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := componentCorpus(t)
			cold := indexOnce(t, root, testCase.withBoundary)
			if len(cold.report.Updated) == 0 || cold.report.ReconciliationBatches == 0 {
				t.Fatalf("cold index did not build the graph: %#v", cold.report)
			}
			if testCase.withBoundary && !observed(cold.boundaries, indexer.BoundaryWorkspacePersisted) {
				t.Fatalf("cold index did not report the workspace it persisted: %v", cold.boundaries)
			}
			for pass := 2; pass <= 3; pass++ {
				refresh := indexOnce(t, root, testCase.withBoundary)
				if len(refresh.report.Updated) != 0 || len(refresh.report.Removed) != 0 {
					t.Fatalf("pass %d rewrote an unchanged corpus: %#v", pass, refresh.report)
				}
				if refresh.report.ReconciliationPendingAtStart {
					t.Fatalf("pass %d started with pending reconciliation work", pass)
				}
				if refresh.report.ReconciliationBatches != 0 {
					t.Fatalf("pass %d ran %d reconciliation batches, want 0",
						pass, refresh.report.ReconciliationBatches)
				}
				if testCase.withBoundary && observed(refresh.boundaries, indexer.BoundaryWorkspacePersisted) {
					t.Fatalf("pass %d reported a workspace it did not persist: %v", pass, refresh.boundaries)
				}
			}
		})
	}
}

// TestChangedWorkspaceStillPersistsUnderBoundaryHook proves the convergence fix
// did not turn the digest into a reason to miss a real workspace change: adding
// a component must still persist the workspace and report its boundary.
func TestChangedWorkspaceStillPersistsUnderBoundaryHook(t *testing.T) {
	root := componentCorpus(t)
	indexOnce(t, root, true)
	write(t, filepath.Join(root, "grafo.yaml"), `components:
  - name: server
    roots: [server]
  - name: client
    roots: [client]
  - name: module
    roots: [go.mod]
`)
	runGit(t, root, "add", "grafo.yaml")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid",
		"commit", "-m", "declare another component")
	changed := indexOnce(t, root, true)
	if !observed(changed.boundaries, indexer.BoundaryWorkspacePersisted) {
		t.Fatalf("changed workspace was not persisted: %v", changed.boundaries)
	}
	if changed.report.ReconciliationBatches == 0 {
		t.Fatalf("changed workspace did not reconcile: %#v", changed.report)
	}
	settled := indexOnce(t, root, true)
	if settled.report.ReconciliationBatches != 0 || observed(settled.boundaries, indexer.BoundaryWorkspacePersisted) {
		t.Fatalf("refresh after a workspace change did not converge: %#v %v", settled.report, settled.boundaries)
	}
}
