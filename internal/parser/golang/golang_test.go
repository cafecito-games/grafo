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

func TestSyntaxFailureFallbackRequiresUnshadowedBuiltins(t *testing.T) {
	safe := []byte(`package sample
func cleanup() {}
func Guard(value any) {
	if value != nil { defer cleanup(); panic(value) }
	_ = recover()
}
`)
	result, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "safe.go", Content: safe, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	panicFact := assertFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgePanics, "example.com/sample.Guard")
	if panicFact.Properties["conditional"] != "true" || panicFact.Properties["evidence"] != "go/ast" {
		t.Fatalf("syntax panic evidence = %#v", panicFact)
	}
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeDefers,
		"example.com/sample.Guard", "example.com/sample.cleanup", "defer")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeRecovers,
		"example.com/sample.Guard", "builtin.recover", "recover")

	shadowed := []byte(`package sample
var panic = func(any) {}
func Guard(value any, recover func() any) { panic(value); _ = recover() }
`)
	result, err = golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "shadowed.go", Content: shadowed, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgePanics, "example.com/sample.Guard")
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeRecovers, "example.com/sample.Guard")
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
	content := []byte("package broken\nfunc helper() {}\nfunc run(value any) { defer helper(); panic(value); _ = recover(); missing() }\n")
	writeFile(t, filepath.Join(root, "broken.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "broken.go", Content: content, Repository: "broken", RepoID: "repo", GoModule: "example.com/broken",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/broken.helper")
	assertHasFactKind(t, result.Facts, graph.EdgePanics)
	assertHasFact(t, result.Facts, graph.EdgeRecovers, "builtin.recover")
	assertHasFact(t, result.Facts, graph.EdgeDefers, "example.com/broken.helper")
	if len(result.Diagnostics) == 0 {
		t.Fatal("expected type-checking diagnostic")
	}
}

func TestPackageSemanticLoaderExtractsFailureAndCleanupFlow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/failures\n\ngo 1.26\n")
	content := []byte(`package failures
import (
	"errors"
	"fmt"
	"log"
)

var ErrSentinel = errors.New("sentinel")

type Problem struct{}
func (*Problem) Error() string { return "problem" }

type Source struct{}
func (Source) Load() error { return ErrSentinel }

type Cleaner struct{}
func (Cleaner) Close() {}

type Provider interface { Read() (string, error) }

func Produce() (int, error) { return 0, ErrSentinel }
func Multi() (error, error) { return ErrSentinel, &Problem{} }
func cleanup() {}
func worker() {}

func Wrap() error {
	_, err := Produce()
	if err != nil {
		return fmt.Errorf("produce: %w", err)
	}
	return nil
}

func Join(left, right error) error { return errors.Join(left, right) }
func Mixed() error { return fmt.Errorf("seen %v, wrap %w", ErrSentinel, &Problem{}) }
func Method(source Source) error { return source.Load() }

func Handle() {
	_, err := Produce()
	if errors.Is(err, ErrSentinel) { return }
	var problem *Problem
	if errors.As(err, &problem) { return }
	if err == ErrSentinel { return }
}

func Switch() {
	_, err := Produce()
	switch err {
	case ErrSentinel:
		return
	}
}

func Retry() {
	for {
		_, err := Produce()
		if err != nil { continue }
		break
	}
}

func LogOnly() {
	_, err := Produce()
	if err != nil { log.Print(err) }
}

func Guard(value any, cleaner Cleaner) {
	defer cleanup()
	defer cleaner.Close()
	defer func() { _ = recover() }()
	go worker()
	panic(value)
}
`)
	writeFile(t, filepath.Join(root, "failures.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "failures.go", Content: content, Repository: "failures",
		RepoID: "repo", GoModule: "example.com/failures",
	})
	if err != nil {
		t.Fatal(err)
	}

	assertNodeProperty(t, result.Nodes, graph.KindVariable, "example.com/failures.ErrSentinel", "error_identity", "sentinel")
	assertNodeProperty(t, result.Nodes, graph.KindType, "example.com/failures.Problem", "error_type", "true")
	assertNodeProperty(t, result.Nodes, graph.KindFunction, "example.com/failures.Multi", "error_result_positions", "0,1")
	assertNodeProperty(t, result.Nodes, graph.KindMethod, "example.com/failures.Provider.Read", "error_result_positions", "1")

	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("returns_error"), "example.com/failures.Multi", "builtin.error", "signature")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeReturnsError, "example.com/failures.Provider.Read", "builtin.error", "signature")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("propagates_error"), "example.com/failures.Produce", "example.com/failures.ErrSentinel", "return")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("wraps_error"), "example.com/failures.Wrap", "example.com/failures.Produce", "wrap")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("propagates_error"), "example.com/failures.Join", "builtin.error", "join")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeWrapsError, "example.com/failures.Mixed", "example.com/failures.Problem", "wrap")
	assertNoFailureFact(t, result.Nodes, result.Facts, graph.EdgeWrapsError, "example.com/failures.Mixed", "example.com/failures.ErrSentinel")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgePropagatesError, "example.com/failures.Method", "example.com/failures.Source.Load", "return_call")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Handle", "example.com/failures.ErrSentinel", "is")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Handle", "example.com/failures.Problem", "as")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Handle", "example.com/failures.ErrSentinel", "comparison")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Switch", "example.com/failures.ErrSentinel", "switch")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Retry", "example.com/failures.Produce", "comparison")
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.LogOnly")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("defers"), "example.com/failures.Guard", "example.com/failures.cleanup", "defer")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeDefers, "example.com/failures.Guard", "example.com/failures.Cleaner.Close", "defer")
	assertNoFailureTarget(t, result.Facts, graph.EdgeKind("defers"), "example.com/failures.worker")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("recovers"), "example.com/failures.Guard", "builtin.recover", "recover")
	panicFact := assertFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeKind("panics"), "example.com/failures.Guard")
	if panicFact.TargetKind != graph.KindExternal || panicFact.Properties["unresolved"] != "true" {
		t.Fatalf("dynamic panic payload must stay unresolved: %#v", panicFact)
	}
	for _, fact := range result.Facts {
		switch fact.Kind {
		case graph.EdgeKind("returns_error"), graph.EdgeKind("propagates_error"), graph.EdgeKind("handles_error"),
			graph.EdgeKind("wraps_error"), graph.EdgeKind("panics"), graph.EdgeKind("recovers"), graph.EdgeKind("defers"):
			if fact.Location.Path == "" || fact.Location.Line == 0 || fact.Properties["form"] == "" || fact.Properties["evidence"] == "" {
				t.Fatalf("failure fact lost source or evidence: %#v", fact)
			}
		}
	}
}

func TestFailureIdentityUsesPackageObjectsNotAliasesOrSimpleNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/identity\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "left", "left.go"), "package left\ntype Problem struct{}\nfunc (*Problem) Error() string { return \"left\" }\n")
	writeFile(t, filepath.Join(root, "right", "right.go"), "package right\ntype Problem struct{}\nfunc (*Problem) Error() string { return \"right\" }\n")
	content := []byte(`package identity
import (
	l "example.com/identity/left"
	r "example.com/identity/right"
)
func Left() error { return &l.Problem{} }
func Right() error { return &r.Problem{} }
`)
	writeFile(t, filepath.Join(root, "identity.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "identity.go", Content: content, Repository: "identity",
		RepoID: "repo", GoModule: "example.com/identity",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgePropagatesError,
		"example.com/identity.Left", "example.com/identity/left.Problem", "return")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgePropagatesError,
		"example.com/identity.Right", "example.com/identity/right.Problem", "return")
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

func assertNodeProperty(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified, key, want string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			if got := node.Properties[key]; got != want {
				t.Fatalf("%s property %s = %q, want %q: %#v", qualified, key, got, want, node)
			}
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, qualified)
}

func assertHasFailureFact(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from, target, form string) {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target && fact.FromID == fromID && fact.Properties["form"] == form {
			return
		}
	}
	t.Fatalf("missing %s fact from %q to %q with form %q", kind, from, target, form)
}

func assertFailureFactFrom(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from string) graph.Fact {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID {
			return fact
		}
	}
	t.Fatalf("missing %s fact from %q", kind, from)
	return graph.Fact{}
}

func assertNoFailureFactFrom(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from string) {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID {
			t.Fatalf("unexpected %s fact from %q: %#v", kind, from, fact)
		}
	}
}

func assertNoFailureFact(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from, target string) {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID && fact.Target == target {
			t.Fatalf("unexpected %s fact from %q to %q: %#v", kind, from, target, fact)
		}
	}
}

func nodeIDByQualified(t *testing.T, nodes []graph.Node, qualified string) string {
	t.Helper()
	for _, node := range nodes {
		if node.QualifiedName == qualified {
			return node.ID
		}
	}
	t.Fatalf("missing node %q", qualified)
	return ""
}

func assertNoFailureTarget(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			t.Fatalf("unexpected %s fact to %q: %#v", kind, target, fact)
		}
	}
}
