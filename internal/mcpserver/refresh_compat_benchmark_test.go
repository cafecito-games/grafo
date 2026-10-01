package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPUnconditionalRefreshCorpusSamples(t *testing.T) {
	corpus := os.Getenv("GRAFO_MCP_BENCHMARK_CORPUS")
	if corpus == "" {
		t.Skip("set GRAFO_MCP_BENCHMARK_CORPUS")
	}
	ctx := context.Background()
	root := compatPrepareCorpus(t, corpus)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	repository := &compatCountingRepository{Repository: opened}
	service := indexer.NewService(repository, parserdefaults.NewRegistry())
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := mcpserver.New(repository, project).WithRefresh(func(refreshContext context.Context) error {
		_, refreshErr := service.Run(refreshContext, project, indexer.Options{})
		return refreshErr
	}).Server("benchmark").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "grafo-benchmark", Version: "benchmark"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(); _ = server.Close(); _ = repository.Close() }()
	selector := compatExactSelector(t, ctx, client, "DiscoverProject")
	compatExactNode(t, ctx, client, selector)
	beforeMeta, beforeCounts := repository.meta.Load(), repository.counts.Load()
	samples := make([]int64, 0, 100)
	for range 100 {
		started := time.Now()
		compatExactNode(t, ctx, client, selector)
		samples = append(samples, time.Since(started).Nanoseconds())
	}
	if repository.meta.Load()-beforeMeta < 100 || repository.counts.Load()-beforeCounts < 100 {
		t.Fatalf("unconditional corpus control did not refresh every call: meta=%d counts=%d",
			repository.meta.Load()-beforeMeta, repository.counts.Load()-beforeCounts)
	}
	encoded, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("BENCHMARK_SAMPLES_NS=%s", encoded)
}

// BenchmarkMCPUnconditionalRefreshCompat intentionally uses only APIs present
// at the 60028383 baseline. The identical source is run there untracked and at
// the implementation head to retain a same-fixture unconditional comparison.
func BenchmarkMCPUnconditionalRefreshCompat(b *testing.B) {
	ctx := context.Background()
	root := testtemp.Dir(b)
	compatGit(b, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "sample.snap"), []byte("first"), 0o644); err != nil {
		b.Fatal(err)
	}
	for index := range 63 {
		path := filepath.Join(root, fmt.Sprintf("bench-%03d.snap", index))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("bench%d", index)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	compatGit(b, root, "add", ".")
	compatGit(b, root, "-c", "user.name=Grafo Benchmark", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	opened, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		b.Fatal(err)
	}
	repository := &compatCountingRepository{Repository: opened}
	registry := parserapi.NewRegistry(compatParser{})
	service := indexer.NewService(repository, registry)
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		b.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := mcpserver.New(repository, project).WithRefresh(func(refreshContext context.Context) error {
		_, refreshErr := service.Run(refreshContext, project, indexer.Options{})
		return refreshErr
	}).Server("benchmark").Connect(ctx, serverTransport, nil)
	if err != nil {
		b.Fatal(err)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "grafo-benchmark", Version: "benchmark"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		_ = client.Close()
		_ = server.Close()
		_ = repository.Close()
	}()
	call := func() {
		compatExactNode(b, ctx, client, "fixture.first")
	}
	call()
	beforeMeta, beforeCounts := repository.meta.Load(), repository.counts.Load()
	b.ResetTimer()
	for range b.N {
		call()
	}
	b.StopTimer()
	if repository.meta.Load()-beforeMeta < int64(b.N) || repository.counts.Load()-beforeCounts < int64(b.N) {
		b.Fatalf("unconditional control did not refresh every call: meta=%d counts=%d n=%d",
			repository.meta.Load()-beforeMeta, repository.counts.Load()-beforeCounts, b.N)
	}
}

func BenchmarkMCPUnconditionalRefreshCorpusCompat(b *testing.B) {
	corpus := os.Getenv("GRAFO_MCP_BENCHMARK_CORPUS")
	if corpus == "" {
		b.Skip("set GRAFO_MCP_BENCHMARK_CORPUS")
	}
	ctx := context.Background()
	root := compatPrepareCorpus(b, corpus)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	opened, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		b.Fatal(err)
	}
	repository := &compatCountingRepository{Repository: opened}
	service := indexer.NewService(repository, parserdefaults.NewRegistry())
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		b.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := mcpserver.New(repository, project).WithRefresh(func(refreshContext context.Context) error {
		_, refreshErr := service.Run(refreshContext, project, indexer.Options{})
		return refreshErr
	}).Server("benchmark").Connect(ctx, serverTransport, nil)
	if err != nil {
		b.Fatal(err)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "grafo-benchmark", Version: "benchmark"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = client.Close(); _ = server.Close(); _ = repository.Close() }()
	selector := compatExactSelector(b, ctx, client, "DiscoverProject")
	call := func() { compatExactNode(b, ctx, client, selector) }
	call()
	beforeMeta, beforeCounts := repository.meta.Load(), repository.counts.Load()
	b.ResetTimer()
	for range b.N {
		call()
	}
	b.StopTimer()
	if repository.meta.Load()-beforeMeta < int64(b.N) || repository.counts.Load()-beforeCounts < int64(b.N) {
		b.Fatalf("unconditional corpus control did not refresh every call: meta=%d counts=%d n=%d",
			repository.meta.Load()-beforeMeta, repository.counts.Load()-beforeCounts, b.N)
	}
}

type compatCountingRepository struct {
	graph.Repository
	meta   atomic.Int64
	counts atomic.Int64
}

func (r *compatCountingRepository) SetMeta(ctx context.Context, key, value string) error {
	r.meta.Add(1)
	return r.Repository.SetMeta(ctx, key, value)
}

func (r *compatCountingRepository) Counts(ctx context.Context) (graph.Counts, error) {
	r.counts.Add(1)
	return r.Repository.Counts(ctx)
}

func compatExactSelector(tb testing.TB, ctx context.Context, client *mcp.ClientSession, name string) string {
	tb.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "find_symbols", Arguments: map[string]any{"query": name}})
	if err != nil || result.IsError {
		tb.Fatalf("find_symbols error=%v result=%#v", err, result)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		tb.Fatalf("find_symbols structured payload = %#v", result.StructuredContent)
	}
	matches, ok := structured["matches"].([]any)
	if !ok || len(matches) == 0 {
		tb.Fatalf("find_symbols matches = %#v", structured["matches"])
	}
	node, ok := matches[0].(map[string]any)
	if !ok {
		tb.Fatalf("find_symbols node = %#v", matches[0])
	}
	qualified, ok := node["qualified_name"].(string)
	if !ok || qualified == "" {
		tb.Fatalf("find_symbols qualified_name = %#v", node["qualified_name"])
	}
	return qualified
}

func compatExactNode(tb testing.TB, ctx context.Context, client *mcp.ClientSession, selector string) {
	tb.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_node", Arguments: map[string]any{"selector": selector}})
	if err != nil || result.IsError {
		tb.Fatalf("get_node error=%v result=%#v", err, result)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		tb.Fatalf("get_node structured payload = %#v", result.StructuredContent)
	}
	node, ok := structured["node"].(map[string]any)
	if !ok || node["qualified_name"] != selector {
		tb.Fatalf("get_node node = %#v, want qualified_name %q", structured["node"], selector)
	}
}

func compatPrepareCorpus(tb testing.TB, source string) string {
	tb.Helper()
	root := testtemp.Dir(tb)
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
		defer func() { _ = input.Close() }()
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
	compatGit(tb, root, "init", "-b", "main")
	compatGit(tb, root, "add", ".")
	compatGit(tb, root, "-c", "user.name=Grafo Benchmark", "-c", "user.email=grafo@example.invalid", "commit", "-m", "corpus")
	return root
}

type compatParser struct{}

func (compatParser) Language() string          { return "compat" }
func (compatParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (compatParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	name := strings.TrimSpace(string(input.Content))
	builder := parserapi.NewBuilder(input, "compat")
	builder.AddNode(graph.Node{Kind: graph.KindFunction, Name: name, QualifiedName: "fixture." + name,
		Location: graph.Location{Path: input.Path, Line: 1, Column: 1}})
	if strings.HasPrefix(filepath.Base(input.Path), "bench-") {
		for index := range 32 {
			qualified := "fixture." + name + ".Helper" + fmt.Sprint(index)
			builder.AddNode(graph.Node{Kind: graph.KindFunction, Name: "Helper" + fmt.Sprint(index), QualifiedName: qualified,
				Location: graph.Location{Path: input.Path, Line: index + 2, Column: 1}})
		}
	}
	return builder.Result, nil
}

func compatGit(tb testing.TB, root string, arguments ...string) {
	tb.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		tb.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}
