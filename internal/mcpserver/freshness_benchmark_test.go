package mcpserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BenchmarkMCPFreshnessSession measures the persistent-session boundary with
// real Git probes, SQLite generations, MCP encoding, and exact graph queries.
// Setup and startup refresh are excluded from steady-state sub-benchmarks.
func BenchmarkMCPFreshnessSession(b *testing.B) {
	b.Run("UnchangedCorpusExactNode", func(b *testing.B) {
		corpus := os.Getenv("GRAFO_MCP_BENCHMARK_CORPUS")
		if corpus == "" {
			b.Skip("set GRAFO_MCP_BENCHMARK_CORPUS to retain the repository-corpus comparison")
		}
		root := prepareBenchmarkCorpus(b, corpus)
		ctx := context.Background()
		coordinator, generation, err := NewFreshnessCoordinator(ctx, []string{root}, parserdefaults.NewRegistry(), FreshnessCoordinatorOptions{})
		if err != nil {
			b.Fatal(err)
		}
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		server, err := NewFederated(generation.Repository, generation.Projects).WithFreshness(coordinator).
			Server("benchmark").Connect(ctx, serverTransport, nil)
		if err != nil {
			b.Fatal(err)
		}
		client, err := mcp.NewClient(&mcp.Implementation{Name: "grafo-benchmark", Version: "benchmark"}, nil).
			Connect(ctx, clientTransport, nil)
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = client.Close(); _ = server.Close(); _ = coordinator.Close() }()
		call := func() {
			result, callErr := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_node", Arguments: map[string]any{"selector": "indexer.DiscoverProject"}})
			if callErr != nil || result.IsError {
				b.Fatalf("get_node error=%v result=%#v", callErr, result)
			}
		}
		call()
		b.ResetTimer()
		for range b.N {
			call()
		}
	})
	b.Run("UnchangedExactNode", func(b *testing.B) {
		fixture := newMCPBenchmarkFixture(b, false)
		defer fixture.close()
		arguments := map[string]any{"selector": "fixture.first"}
		fixture.call(b, "get_node", arguments)
		b.ResetTimer()
		for range b.N {
			fixture.call(b, "get_node", arguments)
		}
	})
	b.Run("UnchangedBatch", func(b *testing.B) {
		fixture := newMCPBenchmarkFixture(b, false)
		defer fixture.close()
		arguments := map[string]any{"selectors": []string{"fixture.first", "fixture.first", "missing"}}
		fixture.call(b, "get_node", arguments)
		b.ResetTimer()
		for range b.N {
			fixture.call(b, "get_node", arguments)
		}
	})
	b.Run("RepeatedSamePathDirtyEdit", func(b *testing.B) {
		fixture := newMCPBenchmarkFixture(b, false)
		defer fixture.close()
		values := []string{"second-a", "second-b"}
		b.ResetTimer()
		for index := range b.N {
			value := values[index%len(values)]
			if err := os.WriteFile(filepath.Join(fixture.roots[0], "sample.snap"), []byte(value), 0o644); err != nil {
				b.Fatal(err)
			}
			fixture.call(b, "get_node", map[string]any{"selector": "fixture." + value})
		}
	})
	b.Run("FederatedOneMemberChange", func(b *testing.B) {
		fixture := newMCPBenchmarkFixture(b, true)
		defer fixture.close()
		values := []string{"second-a", "second-b"}
		b.ResetTimer()
		for index := range b.N {
			value := values[index%len(values)]
			if err := os.WriteFile(filepath.Join(fixture.roots[1], "sample.snap"), []byte(value), 0o644); err != nil {
				b.Fatal(err)
			}
			fixture.call(b, "get_node", map[string]any{"selector": "fixture." + value})
		}
	})
}

func prepareBenchmarkCorpus(tb testing.TB, source string) string {
	tb.Helper()
	root := tb.TempDir()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative != "." {
			switch entry.Name() {
			case ".git", ".grafo", ".worktrees":
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if relative == "." || entry.IsDir() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		target := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		tb.Fatal(err)
	}
	coordinatorGitTB(tb, root, "init", "-b", "main")
	coordinatorGitTB(tb, root, "add", ".")
	coordinatorGitTB(tb, root, "-c", "user.name=Grafo Benchmark", "-c", "user.email=grafo@example.invalid", "commit", "-m", "corpus")
	return root
}

type mcpBenchmarkFixture struct {
	ctx     context.Context
	roots   []string
	client  *mcp.ClientSession
	server  *mcp.ServerSession
	coord   *FreshnessCoordinator
	cleanup func()
}

func newMCPBenchmarkFixture(b *testing.B, federated bool) *mcpBenchmarkFixture {
	b.Helper()
	first := coordinatorProbeTB(b, "first")
	roots := []string{first.Project.Root}
	if federated {
		second := coordinatorProbeTB(b, "member")
		roots = append(roots, second.Project.Root)
	}
	ctx := context.Background()
	coordinator, generation, err := NewFreshnessCoordinator(ctx, roots,
		parserapi.NewRegistry(coordinatorParser{}), FreshnessCoordinatorOptions{})
	if err != nil {
		b.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := NewFederated(generation.Repository, generation.Projects).WithFreshness(coordinator).
		Server("benchmark").Connect(ctx, serverTransport, nil)
	if err != nil {
		_ = coordinator.Close()
		b.Fatal(err)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "grafo-benchmark", Version: "benchmark"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = server.Close()
		_ = coordinator.Close()
		b.Fatal(err)
	}
	return &mcpBenchmarkFixture{ctx: ctx, roots: roots, client: client, server: server, coord: coordinator}
}

func (f *mcpBenchmarkFixture) call(b *testing.B, name string, arguments map[string]any) {
	b.Helper()
	result, err := f.client.CallTool(f.ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		b.Fatal(err)
	}
	if result.IsError {
		b.Fatalf("%s returned an error: %#v", name, result.Content)
	}
}

func (f *mcpBenchmarkFixture) close() {
	_ = f.client.Close()
	_ = f.server.Close()
	_ = f.coord.Close()
}

func coordinatorProbeTB(tb testing.TB, content string) indexer.FreshnessProbe {
	tb.Helper()
	root := tb.TempDir()
	coordinatorGitTB(tb, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "sample.snap"), []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
	for index := range 63 {
		path := filepath.Join(root, fmt.Sprintf("bench-%03d.snap", index))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("bench%d", index)), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	coordinatorGitTB(tb, root, "add", ".")
	coordinatorGitTB(tb, root, "-c", "user.name=Grafo Benchmark", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	probe, err := indexer.ProbeFreshness(context.Background(), root, parserapi.NewRegistry(coordinatorParser{}), indexer.FreshnessOptions{})
	if err != nil {
		tb.Fatal(err)
	}
	return probe
}

func coordinatorGitTB(tb testing.TB, root string, arguments ...string) {
	tb.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		tb.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}
