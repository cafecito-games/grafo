package golang_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
)

type fakeSemanticLoader struct {
	view golangparser.SemanticView
}

func (f fakeSemanticLoader) Load(context.Context, parserapi.Input) (golangparser.SemanticView, error) {
	return f.view, nil
}

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`package api
import (
  clientpkg "example.com/client"
  "net/http"
  "os"
)
type Server struct{}
func (s *Server) Start(input string) string {
  copy := input
  client := clientpkg.NewClient()
  client.Send(copy)
  s.deliver(copy)
  token := os.Getenv("API_TOKEN")
  _ = token
  http.HandleFunc("/health", health)
  http.Get("/ready")
  publish("user.created")
  return copy
}
func (s *Server) deliver(string) {}
func health(http.ResponseWriter, *http.Request) {}
`)
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Path: "api/server.go", Content: content, Repository: "sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindMethod, "Start")
	assertHasNode(t, result.Nodes, graph.KindParameter, "input")
	assertHasNode(t, result.Nodes, graph.KindVariable, "copy")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "ANY /health")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "user.created")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "GET /ready")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
	assertHasFact(t, result.Facts, graph.EdgePasses, "example.com/sample/api.Server.deliver")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/sample/api.Server.deliver")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/client.Client.Send")
}

func TestParserPrefersInjectedSemanticCallAndImplementationEvidence(t *testing.T) {
	content := []byte(`package sample
type Runner interface { Run() }
type Worker struct{}
func (Worker) Run() {}
func invoke(value Runner) { value.Run() }
`)
	callOffset := strings.Index(string(content), "value.Run") + len("value.Run")
	parser := golangparser.NewWithSemanticLoader(fakeSemanticLoader{view: golangparser.SemanticView{
		Available:    true,
		Included:     true,
		BuildContext: "goos=test;goarch=test",
		Calls: map[int]golangparser.SemanticCall{
			callOffset: {Target: "example.com/sample.Runner.Run"},
		},
		Implementations: []golangparser.SemanticImplementation{{
			Concrete: "example.com/sample.Worker", Interface: "example.com/sample.Runner",
		}},
	}})
	result, err := parser.Parse(context.Background(), parserapi.Input{
		Path: "sample.go", Content: content, Repository: "sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/sample.Runner.Run")
	assertHasFact(t, result.Facts, graph.EdgeImplements, "example.com/sample.Runner")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "example.com/sample.Worker.Run" {
			t.Fatalf("interface dispatch fanned out to concrete implementation: %#v", fact)
		}
	}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindFile && node.Properties["go_build_context"] != "goos=test;goarch=test" {
			t.Fatalf("build context not recorded on file node: %#v", node)
		}
	}
}

func TestPackageSemanticLoaderResolvesTypedCalls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	content := []byte(`package sample
import "reflect"
type Runner interface { Run() }
type Base struct{}
func (Base) Promoted() {}
type Worker struct{ Base }
func (*Worker) Run() {}
type Alias = Worker
type Left struct{}
func (Left) Touch() {}
type Right struct{}
func (Right) Touch() {}
func Generic[T any](T) {}
func invoke(r Runner, w *Worker, left Left, right Right) {
	r.Run()
	w.Promoted()
	method := w.Run
	method()
	(*Worker).Run(w)
	var alias *Alias = w
	alias.Run()
	left.Touch()
	right.Touch()
	Generic[int](1)
	println("ok")
	_ = string([]byte("ok"))
	reflect.ValueOf(w).MethodByName("Run").Call(nil)
}
`)
	writeFile(t, filepath.Join(root, "sample.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "sample.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"example.com/sample.Runner.Run",
		"example.com/sample.Base.Promoted",
		"example.com/sample.Worker.Run",
		"example.com/sample.Left.Touch",
		"example.com/sample.Right.Touch",
		"example.com/sample.Generic",
		"builtin.println",
		"reflect.ValueOf",
		"reflect.Value.MethodByName",
		"reflect.Value.Call",
	} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, target)
	}
	assertHasFact(t, result.Facts, graph.EdgeImplements, "example.com/sample.Runner")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "example.com/sample.string" {
			t.Fatalf("type conversion emitted a call edge: %#v", fact)
		}
	}
}

func TestPackageSemanticLoaderExcludesInactiveBuildTaggedFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/buildtag\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "active.go"), "package buildtag\nfunc Active() {}\n")
	content := []byte("//go:build grafo_never_enabled\n\npackage buildtag\nfunc Excluded() {}\n")
	writeFile(t, filepath.Join(root, "excluded.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "excluded.go", Content: content, Repository: "buildtag", RepoID: "repo", GoModule: "example.com/buildtag",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile || result.Nodes[0].Properties["go_build_excluded"] != "true" {
		t.Fatalf("excluded file emitted declarations or omitted context: %#v", result.Nodes)
	}
}

func TestPackageSemanticLoaderDoesNotResolveOrDownloadMissingModule(t *testing.T) {
	root := t.TempDir()
	modulePath := filepath.Join(root, "go.mod")
	moduleContent := "module example.com/missing-test\n\ngo 1.26\n"
	writeFile(t, modulePath, moduleContent)
	content := []byte("package missingtest\nimport missing \"example.invalid/not-present\"\nfunc Run() { missing.Call() }\n")
	writeFile(t, filepath.Join(root, "main.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "main.go", Content: content, Repository: "missing-test", RepoID: "repo", GoModule: "example.com/missing-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("expected unavailable-dependency diagnostic")
	}
	after, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != moduleContent {
		t.Fatalf("semantic load modified go.mod:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("semantic load created go.sum: %v", err)
	}
}

func TestPackageSemanticLoaderUsesWorkspaceModuleImportPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse ./service\n")
	writeFile(t, filepath.Join(root, "service", "go.mod"), "module example.com/workspace/service\n\ngo 1.26\n")
	content := []byte("package service\nfunc helper() {}\nfunc Run() { helper() }\n")
	writeFile(t, filepath.Join(root, "service", "service.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "service/service.go", Content: content, Repository: "workspace", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNodeQualified(t, result.Nodes, graph.KindFunction, "example.com/workspace/service.Run")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/workspace/service.helper")
}

func TestPackageSemanticLoaderFallsBackForUnloadedGoFiles(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		prepareRoot func(*testing.T, string)
		content     string
		function    string
	}{
		{
			name: "test file", path: "sample_test.go", function: "TestHelper",
			prepareRoot: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
				writeFile(t, filepath.Join(root, "sample.go"), "package sample\nfunc Helper() {}\n")
			},
			content: "package sample\nfunc TestHelper() { Helper() }\n",
		},
		{
			name: "unlisted nested module", path: "nested/nested.go", function: "Nested",
			prepareRoot: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n\ngo 1.26\n")
				writeFile(t, filepath.Join(root, "outer.go"), "package outer\n")
				writeFile(t, filepath.Join(root, "nested", "go.mod"), "module example.com/nested\n\ngo 1.26\n")
			},
			content: "package nested\nfunc Nested() {}\n",
		},
		{
			name: "cgo source", path: "cgo.go", function: "CGOHelper",
			prepareRoot: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.com/cgo\n\ngo 1.26\n")
			},
			content: "package cgo\n/* static void grafo_noop(void) {} */\nimport \"C\"\nfunc CGOHelper() { C.grafo_noop() }\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.prepareRoot(t, root)
			writeFile(t, filepath.Join(root, filepath.FromSlash(test.path)), test.content)
			result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
				Root: root, Path: test.path, Content: []byte(test.content), Repository: "sample", RepoID: "repo", GoModule: "example.com/outer",
			})
			if err != nil {
				t.Fatal(err)
			}
			assertHasNode(t, result.Nodes, graph.KindFunction, test.function)
			if result.Nodes[0].Properties["go_build_excluded"] == "true" {
				t.Fatalf("unloaded source was misclassified as build-excluded: %#v", result.Nodes[0])
			}
		})
	}
}

func TestSemanticKeySkipsSpecialGoEntries(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/special\n\ngo 1.26\n")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), filepath.Join(root, "dangling.go")); err != nil {
		t.Fatal(err)
	}
	parser := golangparser.New()
	if _, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root}); err != nil {
		t.Fatalf("special Go entry aborted workspace keying: %v", err)
	}
}

func TestPackageSemanticLoaderRetainsProvenFactsForBrokenPackage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/broken\n\ngo 1.26\n")
	content := []byte("package broken\nfunc helper() {}\nfunc run() { helper(); missing() }\n")
	writeFile(t, filepath.Join(root, "broken.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "broken.go", Content: content, Repository: "broken", RepoID: "repo", GoModule: "example.com/broken",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/broken.helper")
	if len(result.Diagnostics) == 0 {
		t.Fatal("expected type-checking diagnostic")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertHasFactKind(t *testing.T, facts []graph.Fact, kind graph.EdgeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s fact", kind)
}

func assertHasNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, name string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.Name == name {
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, name)
}

func assertHasNodeQualified(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, qualified)
}

func assertHasFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	var available []string
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
		if fact.Kind == kind {
			available = append(available, fact.Target)
		}
	}
	t.Fatalf("missing %s fact to %q; available targets: %v", kind, target, available)
}
