package indexer_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestGoViewCacheProducesIdenticalGraphAcrossProcesses pins the contract that
// matters most about persisting derived Go semantic views: reusing them must
// yield the same graph as recomputing them, down to node, fact and edge
// identity and ordering. Each run uses a fresh database and a fresh parser, so
// the second one sees exactly what a second grafo process would.
func TestGoViewCacheProducesIdenticalGraphAcrossProcesses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\n\ntype Runner interface{ Run() error }\n")
	write(t, filepath.Join(root, "worker", "worker.go"), `package worker

import "example.com/sample/contract"

type Worker struct{}

func (Worker) Run() error { return nil }

func Drive(runner contract.Runner) error { return runner.Run() }
`)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	cold, coldMetrics := indexIntoFreshDatabase(t, ctx, root, "cold.db")
	if coldMetrics.Loads != 1 {
		t.Fatalf("cold run did not load packages exactly once: %#v", coldMetrics)
	}
	if coldMetrics.PersistedSaves != 1 {
		t.Fatalf("cold run persisted no views: %#v", coldMetrics)
	}

	warm, warmMetrics := indexIntoFreshDatabase(t, ctx, root, "warm.db")
	if warmMetrics.Loads != 0 {
		t.Fatalf("warm run re-ran packages.Load: %#v", warmMetrics)
	}
	if warmMetrics.PersistedHits == 0 {
		t.Fatalf("warm run did not adopt the persisted views: %#v", warmMetrics)
	}
	if !slices.Equal(cold, warm) {
		t.Fatalf("reused views changed the graph:\ncold=%s\nwarm=%s", strings.Join(cold, "\n"), strings.Join(warm, "\n"))
	}
	// A graph with no cross-package evidence would compare equal while proving
	// nothing, so the fixture's resolved implements edge is required.
	if !slices.ContainsFunc(cold, func(line string) bool { return strings.Contains(line, string(graph.EdgeImplements)) }) {
		t.Fatalf("fixture produced no cross-package evidence to compare: %s", strings.Join(cold, "\n"))
	}
}

// TestGoViewCacheRecoversFromCorruptPayload proves the fail-closed path end to
// end: a corrupt cache leaves indexing with the same graph it would have built
// without one, never a graph missing its cross-file edges.
func TestGoViewCacheRecoversFromCorruptPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\n\ntype Runner interface{ Run() error }\n")
	write(t, filepath.Join(root, "worker", "worker.go"), `package worker

type Worker struct{}

func (Worker) Run() error { return nil }
`)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	expected, _ := indexIntoFreshDatabase(t, ctx, root, "expected.db")

	corrupted := 0
	cache := filepath.Join(root, ".grafo", "cache")
	if err := filepath.WalkDir(cache, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		corrupted++
		return os.WriteFile(path, []byte("corrupt"), 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	if corrupted == 0 {
		t.Fatalf("no view cache to corrupt under %s", cache)
	}

	recovered, metrics := indexIntoFreshDatabase(t, ctx, root, "recovered.db")
	if metrics.Loads != 1 {
		t.Fatalf("corrupt cache was not replaced by a real load: %#v", metrics)
	}
	if !slices.Equal(expected, recovered) {
		t.Fatalf("corrupt cache changed the graph:\nwant=%s\ngot=%s", strings.Join(expected, "\n"), strings.Join(recovered, "\n"))
	}
}

// TestGoViewCacheKeepsRefreshAndForceIdentical covers the three ways the same
// worktree state can be reached once views are persisted and the two semantic
// keys have been narrowed: a cold build, an incremental refresh over an edit,
// and a forced full rebuild. All three must agree on the graph.
func TestGoViewCacheKeepsRefreshAndForceIdentical(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\n\ntype Runner interface{ Run() error }\n")
	write(t, filepath.Join(root, "worker", "worker.go"), `package worker

import "example.com/sample/contract"

type Worker struct{}

func (Worker) Run() error { return nil }

func Drive(runner contract.Runner) error { return runner.Run() }
`)
	write(t, filepath.Join(root, "web", "service.ts"), "export class Store {\n  get(id: string) { return id; }\n}\nexport function build() { return new Store(); }\n")
	write(t, filepath.Join(root, "web", "caller.ts"), "import { Store } from \"./service\";\nexport function run() { return new Store().get(\"a\"); }\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	// Reach the target state incrementally: index, edit a Go body and a
	// TypeScript body, then refresh.
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	project.IndexPath = filepath.Join(testtemp.Dir(t), "incremental.db")
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New(), typescriptparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "worker", "worker.go"), `package worker

import "example.com/sample/contract"

type Worker struct{}

func (Worker) Run() error { return nil }

func Drive(runner contract.Runner) error {
	result := runner.Run()
	return result
}
`)
	write(t, filepath.Join(root, "web", "service.ts"), "export class Store {\n  get(id: string) { const local = id; return local; }\n}\nexport function build() { return new Store(); }\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	incremental := dumpGraph(t, ctx, repository)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	cold := indexGraph(t, ctx, root, "cold.db", indexer.Options{})
	forced := indexGraph(t, ctx, root, "forced.db", indexer.Options{Force: true})
	if !slices.Equal(incremental, cold) {
		t.Fatalf("incremental refresh and cold build disagree:\nincremental=%s\ncold=%s",
			strings.Join(incremental, "\n"), strings.Join(cold, "\n"))
	}
	if !slices.Equal(incremental, forced) {
		t.Fatalf("incremental refresh and forced rebuild disagree:\nincremental=%s\nforced=%s",
			strings.Join(incremental, "\n"), strings.Join(forced, "\n"))
	}
}

// indexIntoFreshDatabase indexes root into a new database with a new parser and
// returns a canonical dump of the resulting graph alongside the loader metrics.
func indexIntoFreshDatabase(t *testing.T, ctx context.Context, root, name string) ([]string, golangparser.SemanticLoadMetrics) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	project.IndexPath = filepath.Join(testtemp.Dir(t), name)
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	parser := golangparser.New()
	if _, err := indexer.NewService(repository, parserapi.NewRegistry(parser)).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	return dumpGraph(t, ctx, repository), parser.SemanticLoadMetrics()
}

// indexGraph indexes root into a new database with the full Go and TypeScript
// registry and returns the resulting graph dump.
func indexGraph(t *testing.T, ctx context.Context, root, name string, options indexer.Options) []string {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	project.IndexPath = filepath.Join(testtemp.Dir(t), name)
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(golangparser.New(), typescriptparser.New())
	if _, err := indexer.NewService(repository, registry).Run(ctx, project, options); err != nil {
		t.Fatal(err)
	}
	return dumpGraph(t, ctx, repository)
}

// dumpGraph renders every node with its outgoing edges as sorted lines, so a
// comparison covers identity and ordering rather than only totals.
func dumpGraph(t *testing.T, ctx context.Context, repository *sqlite.Repository) []string {
	t.Helper()
	nodes, err := repository.SearchNodes(ctx, "", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("indexed graph has no nodes to compare")
	}
	lines := make([]string, 0, len(nodes)*2)
	for _, node := range nodes {
		lines = append(lines, fmt.Sprintf("node\t%s\t%s\t%s\t%s:%d", node.ID, node.Kind, node.QualifiedName,
			node.Location.Path, node.Location.Line))
		edges, err := repository.EdgesFrom(ctx, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		for index, edge := range edges {
			lines = append(lines, fmt.Sprintf("edge\t%s\t%d\t%s\t%s\t%s\t%s:%d", node.ID, index, edge.Kind,
				edge.ToID, edge.Producer, edge.Location.Path, edge.Location.Line))
		}
	}
	slices.Sort(lines)
	return lines
}
