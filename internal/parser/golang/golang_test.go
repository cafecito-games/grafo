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

func TestHTTPExtractionUsesCanonicalIdentityAndKeepsRawEvidence(t *testing.T) {
	content := []byte(`package api
import "net/http"
func Handler(http.ResponseWriter, *http.Request) {}
func Routes() {
	router.Get("/users/{characterID}/?view=full#details", Handler)
	_, _ = http.Get("https://api.example.test/users/{id}/?expand=true#top")
	_, _ = http.Get("https://api.example.test/users/{id}/?expand=false#other")
	router.Get("/bad/%zz", Handler)
}
`)
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Path: "api/routes.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	var endpoint graph.Node
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint && node.Properties["route"] == "/users/{_}" {
			endpoint = node
		}
	}
	if endpoint.ID == "" || endpoint.Properties["raw_route"] != "/users/{characterID}/?view=full#details" ||
		endpoint.Properties["query"] != "view=full" || endpoint.Properties["fragment"] != "details" {
		t.Fatalf("canonical endpoint or raw evidence missing: %#v", endpoint)
	}
	var requests []graph.Fact
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			requests = append(requests, fact)
		}
	}
	if len(requests) != 2 || requests[0].Target != "GET https://api.example.test/users/{_}" || requests[1].Target != requests[0].Target {
		t.Fatalf("absolute URL identity contains non-path evidence: %#v", requests)
	}
	foundFirstEvidence := false
	for _, request := range requests {
		if request.Properties["http_query"] == "expand=true" {
			foundFirstEvidence = request.Properties["http_route"] == "/users/{_}" &&
				request.Properties["http_authority"] == "api.example.test" && request.Properties["http_fragment"] == "top"
		}
	}
	if !foundFirstEvidence {
		t.Fatalf("raw request evidence missing: %#v", requests)
	}
	foundDiagnostic := false
	for _, diagnostic := range result.Diagnostics {
		foundDiagnostic = foundDiagnostic || strings.Contains(diagnostic.Message, "invalid HTTP endpoint")
	}
	if !foundDiagnostic {
		t.Fatalf("malformed HTTP route was not diagnosed: %#v", result.Diagnostics)
	}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint && node.Properties["raw_route"] == "/bad/%zz" && node.Properties["http_invalid"] != "true" {
			t.Fatalf("malformed endpoint was not kept fail-closed: %#v", node)
		}
	}
}

func TestCanonicalEndpointsAtSameLineRemainDistinct(t *testing.T) {
	content := []byte(`package api
func Handler() {}
func Routes() { router.Get("/users/{id}", Handler); router.Get("/users/{name}", Handler) }
`)
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Path: "routes.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint && node.Name == "GET /users/{_}" {
			ids[node.ID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("canonical duplicate declarations collapsed: %#v", result.Nodes)
	}
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
func callbackCleanup() {}
func Register(func()) {}
func Guard(value any) {
	if value != nil { defer cleanup(); panic(value) }
	Register(func() { defer callbackCleanup(); panic(value) })
	func() { _ = recover() }()
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
	deferFact := assertFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeDefers, "example.com/sample.Guard")
	if deferFact.Target != "example.com/sample.cleanup" || deferFact.Properties["unresolved"] != "true" {
		t.Fatalf("syntax defer must keep its syntactic target and unresolved evidence: %#v", deferFact)
	}
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeRecovers,
		"example.com/sample.Guard", "builtin.recover", "recover")
	if count := failureFactCountFrom(t, result.Nodes, result.Facts, graph.EdgeRecovers, "example.com/sample.Guard"); count != 1 {
		t.Fatalf("syntax recover facts = %d, want only the named function site", count)
	}
	assertNoFailureTarget(t, result.Facts, graph.EdgeDefers, "example.com/sample.callbackCleanup")
	panicCount := 0
	guardID := graph.NodeID(graph.KindFunction, "example.com/sample.Guard", "repo", "safe.go")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgePanics && fact.FromID == guardID {
			panicCount++
		}
	}
	if panicCount != 1 {
		t.Fatalf("syntax fallback panics = %d, want only the directly executed panic", panicCount)
	}

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

func TestParserSuppressesOnlyCorroboratedProtobufGeneratedGo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "buf.gen.yaml"), `version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: example.com/generated
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen
    opt: paths=source_relative
`)
	writeFile(t, filepath.Join(root, "schema.proto"), "syntax = \"proto3\"; message Message { string value = 1; }\n")
	generated := []byte(`// Code generated by protoc-gen-go. DO NOT EDIT.
// source: schema.proto
package generated
type Message struct{}
func (*Message) Reset() {}
func (*Message) ProtoReflect() any { return nil }
func (*Message) GetValue() string { return "" }
`)
	path := "gen/schema.pb.go"
	writeFile(t, filepath.Join(root, filepath.FromSlash(path)), string(generated))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: path, Content: generated, RepoID: "repo:test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile || result.Nodes[0].Properties["generator"] != "protoc-gen-go" {
		t.Fatalf("generated runtime was not suppressed: %#v", result.Nodes)
	}
	mismatched := []byte(strings.Replace(string(generated), "source: schema.proto", "source: other.proto", 1))
	result, err = golangparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: path, Content: mismatched, RepoID: "repo:test"})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindMethod, "Reset")
	if len(result.Diagnostics) == 0 || !strings.Contains(result.Diagnostics[len(result.Diagnostics)-1].Message, "provenance rejected") {
		t.Fatalf("mismatched generated source was not diagnosed: %#v", result.Diagnostics)
	}
	ordinary := []byte("// Code generated by stringer. DO NOT EDIT.\npackage generated\nfunc Keep() {}\n")
	result, err = golangparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: "ordinary_generated.go", Content: ordinary, RepoID: "repo:test"})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindFunction, "Keep")
}

func TestPackageSemanticLoaderExtractsCanonicalProtobufUsage(t *testing.T) {
	root := protobufUsageFixture(t)
	content, err := os.ReadFile(filepath.Join(root, "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "app.go", Content: content, Repository: "protobuf-usage",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}

	useID := nodeIDByQualified(t, result.Nodes, "example.com/app.Use")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeEncodes, "acme.v1.Envelope", "marshal")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeDecodes, "acme.v1.Envelope", "unmarshal")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.title", "composite_literal")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.text", "oneof_wrapper")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.title", "getter")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.title", "field_selection")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.title", "field_selection")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.text", "type_switch")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "google.golang.org/protobuf/proto.Marshal")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "google.golang.org/protobuf/proto.Unmarshal")

	falsePositiveID := nodeIDByQualified(t, result.Nodes, "example.com/app.FalsePositive")
	for _, fact := range result.Facts {
		if fact.FromID == falsePositiveID && (fact.Kind == graph.EdgeEncodes || fact.Kind == graph.EdgeDecodes ||
			fact.Kind == graph.EdgeReads || fact.Kind == graph.EdgeWrites) && fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("ordinary same-name API produced protocol usage: %#v", fact)
		}
	}
}

func TestPackageSemanticLoaderFailsClosedForIllTypedProtobufUsage(t *testing.T) {
	root := protobufUsageFixture(t)
	content := []byte(`package app
import (
	generated "example.com/app/gen"
	wire "google.golang.org/protobuf/proto"
)
func Broken() { _, _ = wire.Marshal(&generated.Envelope{Title: "x"}); missing() }
`)
	writeFile(t, filepath.Join(root, "app.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "app.go", Content: content, Repository: "protobuf-usage",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("ill-typed package produced no diagnostic")
	}
	for _, fact := range result.Facts {
		if fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("ill-typed package produced protocol usage: %#v", fact)
		}
	}
}

func protobufUsageFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), `module example.com/app

go 1.26

require google.golang.org/protobuf v0.0.0
replace google.golang.org/protobuf => ./third_party/protobuf
`)
	writeFile(t, filepath.Join(root, "third_party", "protobuf", "go.mod"), "module google.golang.org/protobuf\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "third_party", "protobuf", "proto", "proto.go"), `package proto
type Message interface{}
func Marshal(Message) ([]byte, error) { return nil, nil }
func Unmarshal([]byte, Message) error { return nil }
`)
	writeFile(t, filepath.Join(root, "buf.gen.yaml"), `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen
    opt: paths=source_relative
`)
	writeFile(t, filepath.Join(root, "schema.proto"), `syntax = "proto3";
package acme.v1;
option go_package = "example.com/app/gen;generated";
message Envelope {
  string title = 1;
  oneof payload { string text = 2; }
}
`)
	writeFile(t, filepath.Join(root, "gen", "schema.pb.go"), `// Code generated by protoc-gen-go. DO NOT EDIT.
// source: schema.proto
package generated
type Envelope struct { Title string; Payload isEnvelope_Payload }
func (value *Envelope) GetTitle() string { return value.Title }
type isEnvelope_Payload interface { isEnvelope_Payload() }
type Envelope_Text struct { Text string }
func (*Envelope_Text) isEnvelope_Payload() {}
`)
	writeFile(t, filepath.Join(root, "app.go"), `package app
import (
	generated "example.com/app/gen"
	wire "google.golang.org/protobuf/proto"
)
type EnvelopeAlias = generated.Envelope
type Ordinary struct { Title string }
func (*Ordinary) GetTitle() string { return "" }
func Use(data []byte, input *generated.Envelope) {
	value := &EnvelopeAlias{Title: "hello", Payload: &generated.Envelope_Text{Text: "world"}}
	encode := wire.Marshal
	_, _ = encode(value)
	_ = wire.Unmarshal(data, input)
	_ = input.GetTitle()
	_ = input.Title
	input.Title = "updated"
	switch input.Payload.(type) {
	case *generated.Envelope_Text:
	}
}
func FalsePositive(value *Ordinary) {
	_ = value.GetTitle()
	_ = value.Title
	_, _ = wire.Marshal(value)
}
`)
	return root
}

func assertProtocolFact(t *testing.T, facts []graph.Fact, fromID string, kind graph.EdgeKind, target, form string) {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID != fromID || fact.Kind != kind || fact.Target != target || fact.Properties["form"] != form {
			continue
		}
		if fact.TargetID == "" || fact.TargetKind == "" || fact.Location.Path == "" || fact.Location.Line == 0 ||
			fact.Properties["protocol"] != "protobuf" || fact.Properties["api"] == "" || fact.Properties["binding"] == "" ||
			fact.Properties["binding_id"] == "" || fact.Properties["static_type"] == "" ||
			fact.Properties["evidence"] != "go/types" {
			t.Fatalf("protocol fact lost canonical evidence: %#v", fact)
		}
		return
	}
	t.Fatalf("missing %s protocol fact from %q to %q with form %q", kind, fromID, target, form)
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
type BigProvider interface { Provider }

func Produce() (int, error) { return 0, ErrSentinel }
func Multi() (error, error) { return ErrSentinel, &Problem{} }
func cleanup() {}
func callbackCleanup() {}
func worker() {}
func Register(func()) {}

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
	func() { _ = recover() }()
	Register(func() { defer callbackCleanup(); panic(ErrSentinel) })
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
	assertNodeProperty(t, result.Nodes, graph.KindMethod, "example.com/failures.BigProvider.Read", "error_result_positions", "1")

	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("returns_error"), "example.com/failures.Multi", "builtin.error", "signature")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeReturnsError, "example.com/failures.Provider.Read", "builtin.error", "signature")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeReturnsError, "example.com/failures.BigProvider.Read", "builtin.error", "signature")
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
	assertNoFailureTarget(t, result.Facts, graph.EdgeDefers, "example.com/failures.callbackCleanup")
	assertNoFailureTarget(t, result.Facts, graph.EdgePanics, "example.com/failures.ErrSentinel")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("recovers"), "example.com/failures.Guard", "builtin.recover", "recover")
	if count := failureFactCountFrom(t, result.Nodes, result.Facts, graph.EdgeRecovers, "example.com/failures.Guard"); count != 1 {
		t.Fatalf("semantic recover facts = %d, want only the deferred recovery closure", count)
	}
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

func TestSyntaxFailureFallbackSeesPackageShadowingAcrossFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "shadow.go"), "package sample\nvar panic = func(any) {}\n")
	content := []byte("package sample\nfunc Guard(value any) { panic(value) }\n")
	writeFile(t, filepath.Join(root, "guard.go"), string(content))

	result, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "guard.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgePanics, "example.com/sample.Guard")
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

func failureFactCountFrom(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from string) int {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	count := 0
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID {
			count++
		}
	}
	return count
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
