package typescript_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
)

func TestParserCatalogCacheSupportsConcurrentParses(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.ts", `export function a() {}`)
	writeFile(t, root, "b.ts", `import { a } from "./a"; export function b() { a() }`)
	parser := typescriptparser.New()
	key, err := parser.WorkspaceSemanticKey(context.Background(), parserapi.Input{Root: root, RepoID: "repo:sample"})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errors := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, parseErr := parser.Parse(context.Background(), parserapi.Input{Root: root, Path: "b.ts",
				Content: []byte(`import { a } from "./a"; export function b() { a() }`), RepoID: "repo:sample", SemanticKey: key})
			errors <- parseErr
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	metrics := parser.ResolutionMetrics()
	if metrics.CatalogLoads < 1 || metrics.CatalogLoads > 8 || metrics.CacheEntries != 1 || metrics.CachedModules != 2 {
		t.Fatalf("unexpected bounded cache metrics: %#v", metrics)
	}
}

func TestParserResolvesModuleBindingsAndReceiverCalls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/service.ts", `
export interface Runner { run(): void }
export class Service { static create() { return new Service() } run() {} }
export default function direct() {}
`)
	writeFile(t, root, "src/barrel.ts", `
export { Service as RenamedService, type Runner } from "./service";
export { default as direct } from "./service";
`)
	writeFile(t, root, "src/directory/index.ts", `export function indexed() {}`)
	writeFile(t, root, "src/anonymous.ts", `export default function () {}`)
	writeFile(t, root, "packages/tools/package.json", `{"name":"@sample/tools","exports":{".":{"types":"./src/index.ts","default":"./dist/index.js"}}}`)
	writeFile(t, root, "packages/tools/src/index.ts", `export function packaged() {}`)
	caller := `
import { RenamedService as Service, type Runner, direct as invoke } from "@app/barrel";
import * as API from "./barrel";
import directDefault from "./service";
import { indexed } from "./directory";
import { packaged } from "@sample/tools";
import anonymous from "./anonymous";
import "missing-package";
function use(runner: Runner) {
  invoke();
  directDefault();
  indexed();
  packaged();
  anonymous();
  Service.create();
  const service = new Service();
  service.run();
  runner.run();
  API.direct();
}
`
	writeFile(t, root, "src/caller.ts", caller)
	writeFile(t, root, "tsconfig.base.json", `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["src/*"]}}}`)
	writeFile(t, root, "tsconfig.json", `{"extends":"./tsconfig.base.json","compilerOptions":{"rootDirs":["src"]}}`)

	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "src/caller.ts", Content: []byte(caller), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindModule, "src/caller")
	assertHasFactTargetID(t, result.Facts, graph.EdgeImports,
		graph.NodeID(graph.KindModule, "repo:sample:src/barrel.ts"))
	assertHasFactProperty(t, result.Facts, graph.EdgeImports, "local", "Service")
	assertHasFactProperty(t, result.Facts, graph.EdgeImports, "type_only", "true")
	assertHasFactProperty(t, result.Facts, graph.EdgeImports, "binding_kind", "namespace")
	for _, target := range []string{
		"src/service.direct",
		"src/directory.indexed",
		"packages/tools/src.packaged",
		"src/anonymous.anonymous@1",
		"src/service.Service.create",
		"src/service.Service.run",
		"src/service.Runner.run",
	} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, target)
	}
	assertFactCount(t, result.Facts, graph.EdgeCalls, "src/service.direct", 3)
	assertHasFact(t, result.Facts, graph.EdgeImports, "missing-package")

	anonymousResult, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "src/anonymous.ts", Content: []byte(`export default function () {}`), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, anonymousResult.Nodes, graph.KindFunction, "anonymous@1")
}

func TestParserKeepsAmbiguousBarrelAndDynamicReceiversUnresolved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.ts", `export function duplicate() {}`)
	writeFile(t, root, "src/b.ts", `export function duplicate() {}`)
	writeFile(t, root, "src/barrel.ts", `export * from "./a"; export * from "./b"; export * from "./cycle";`)
	writeFile(t, root, "src/cycle.ts", `export * from "./barrel";`)
	caller := `
import { duplicate } from "./barrel";
function use(value: any, union: A | B) { duplicate(); value.run(); union.run(); }
`
	writeFile(t, root, "src/caller.ts", caller)
	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "src/caller.ts", Content: []byte(caller), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "duplicate")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "any.run")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "A | B.run")
	assertDiagnosticContains(t, result.Diagnostics, "ambiguous export")
}

func TestParserDiagnosesInvalidConfigAndStillResolvesRelativeModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", `{"compilerOptions": nope}`)
	writeFile(t, root, "src/service.ts", `export function invoke() {}`)
	caller := `import { invoke } from "./service"; invoke();`
	writeFile(t, root, "src/caller.ts", caller)
	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "src/caller.ts", Content: []byte(caller), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "src/service.invoke")
	assertDiagnosticContains(t, result.Diagnostics, "invalid tsconfig")
}

func TestParserResolvesInheritedPathsFromDeclaringConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.base.json", `{"compilerOptions":{"paths":{"@app/*":["src/*"]}}}`)
	writeFile(t, root, "packages/client/tsconfig.json", `{"extends":"../../tsconfig.base.json"}`)
	writeFile(t, root, "src/service.ts", `export function invoke() {}`)
	writeFile(t, root, "packages/client/src/service.ts", `export function invoke() {}`)
	caller := `import { invoke } from "@app/service"; invoke();`
	writeFile(t, root, "packages/client/caller.ts", caller)

	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "packages/client/caller.ts", Content: []byte(caller), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "src/service.invoke")
}

func TestParserKeepsCollidingPhysicalModulesDistinctAndCallsUnresolved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/service.ts", `export class Service { run() {} }`)
	writeFile(t, root, "src/service.d.ts", `export declare class Service { run(): void }`)
	caller := `import { Service } from "./service"; new Service().run();`
	writeFile(t, root, "src/caller.ts", caller)

	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "src/caller.ts", Content: []byte(caller), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFactTargetID(t, result.Facts, graph.EdgeImports,
		graph.NodeID(graph.KindModule, "repo:sample:src/service.ts"))
	assertDiagnosticContains(t, result.Diagnostics, "module identity src/service is ambiguous")
	assertLacksFact(t, result.Facts, graph.EdgeCalls, "src/service.Service.run")
}

func TestParserToleratesSemanticInputRaces(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "service.ts", `export function oldName() {}`)
	writeFile(t, root, "caller.ts", `import { oldName } from "./service"; oldName();`)
	parser := typescriptparser.New()
	key, err := parser.WorkspaceSemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "service.ts", `export function newName() {}`)
	content := []byte(`import { newName } from "./service"; newName();`)
	result, err := parser.Parse(context.Background(), parserapi.Input{Root: root, Path: "caller.ts", Content: content,
		Repository: "sample", RepoID: "repo:sample", SemanticKey: key})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "service.newName")
	assertDiagnosticContains(t, result.Diagnostics, "resolution inputs changed during parsing")
	newKey, err := parser.WorkspaceSemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err = parser.Parse(context.Background(), parserapi.Input{Root: root, Path: "caller.ts", Content: content,
		Repository: "sample", RepoID: "repo:sample", SemanticKey: newKey})
	if err != nil {
		t.Fatal(err)
	}
	assertLacksDiagnostic(t, result.Diagnostics, "resolution inputs changed during parsing")
}

func TestParserSkipsTrackedResolutionInputsThatDisappear(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "deleted.ts", `export function gone() {}`)
	if err := os.Symlink("missing-target.ts", filepath.Join(root, "broken.ts")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "deleted.ts", "broken.ts"}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.Remove(filepath.Join(root, "deleted.ts")); err != nil {
		t.Fatal(err)
	}

	parser := typescriptparser.New()
	if _, err := parser.WorkspaceSemanticKey(context.Background(), parserapi.Input{Root: root}); err != nil {
		t.Fatal(err)
	}
	content := []byte(`export function current() {}`)
	result, err := parser.Parse(context.Background(), parserapi.Input{
		Root: root, Path: "current.ts", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindFunction, "current")
}

func TestParserAttributesEachExportToItsStatement(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.ts", `export function one() {}`)
	writeFile(t, root, "b.ts", `export function two() {}`)
	content := []byte("export { one } from \"./a\";\nexport { two } from \"./b\";")
	writeFile(t, root, "barrel.ts", string(content))
	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "barrel.ts", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFactAtLine(t, result.Facts, graph.EdgeExports, "a.one", 1)
	assertHasFactAtLine(t, result.Facts, graph.EdgeExports, "b.two", 2)
}

func TestParserExportsValuesAndAnonymousDefaultClass(t *testing.T) {
	root := t.TempDir()
	values := []byte("export const answer = 42;\nconst config = {}; export default config;")
	writeFile(t, root, "values.ts", string(values))
	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "values.ts", Content: values, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeExports, "values.answer@1")
	assertHasFact(t, result.Facts, graph.EdgeExports, "values.config@2")

	writeFile(t, root, "base.ts", `export class Base {}`)
	anonymous := []byte("import { Base } from \"./base\";\nexport default\nclass extends Base { run() {} }")
	writeFile(t, root, "anonymous.ts", string(anonymous))
	result, err = typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "anonymous.ts", Content: anonymous, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindClass, "anonymous@3")
	assertHasFact(t, result.Facts, graph.EdgeExports, "anonymous.anonymous@3")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "base.Base")
}

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`import express from "express";
interface Handler { run(): void }
class ChargeService { charge(value: string) {} }
class Checkout extends Base implements Handler {
  async run(orderId: string) {
    const request = orderId;
    const token = process.env.API_TOKEN;
    const service = new ChargeService();
    service.charge(request);
    axios.post("/checkout", request);
    events.publish("order.created", {});
    return request;
  }
}
const app = express();
app.post("/checkout", checkout);
`)
	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "src/app.ts", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindClass, "Checkout")
	assertHasNode(t, result.Nodes, graph.KindMethod, "run")
	assertHasNode(t, result.Nodes, graph.KindParameter, "orderId")
	assertHasNode(t, result.Nodes, graph.KindVariable, "request")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "POST /checkout")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "order.created")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "src/app.ChargeService.charge")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "POST /checkout")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "src/app.Base")
	assertHasFact(t, result.Facts, graph.EdgeImplements, "src/app.Handler")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePasses)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
}

func TestParserInfersFetchRequestMethodsAndFailsClosed(t *testing.T) {
	content := []byte(`
function send(dynamicMethod: string, dynamicURL: string, unknownInit: RequestInit) {
  const METHOD = "post" as const;
  fetch("/health");
  fetch("/credentials", { credentials: "same-origin" });
  fetch("/literal-get", { method: "get" });
  fetch("/literal-post", { method: "POST" });
  fetch("/literal-patch", { method: ("PATCH" as const) });
  fetch("/literal-delete", { "method": "DELETE", credentials: "include" });
  fetch("/constant", { method: (METHOD) } as const);
  fetch("/ordered-exact", { ...unknownInit, method: "PATCH" });
  fetch("/known-spread", { method: "DELETE", ...{ credentials: "include" } });
  fetch("/spread-method", { ...{ method: "PUT" } });
  fetch("/ordered-unknown", { method: "POST", ...unknownInit });
  fetch("/dynamic", { method: dynamicMethod });
  fetch("/invalid", { method: "BAD METHOD" });
  fetch(dynamicURL, { method: "POST" });
  similarlyNamedFetch("/not-fetch", { method: "POST" });
  axios.post("/axios", {});
  client.request("/legacy", {});
}
`)
	result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "src/client.ts", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"GET /health",
		"GET /credentials",
		"GET /literal-get",
		"POST /literal-post",
		"PATCH /literal-patch",
		"DELETE /literal-delete",
		"POST /constant",
		"PATCH /ordered-exact",
		"DELETE /known-spread",
		"PUT /spread-method",
		"ANY /ordered-unknown",
		"ANY /dynamic",
		"ANY /invalid",
		"POST /axios",
		"ANY /legacy",
	}
	requests := requestFactsByTarget(result.Facts)
	if len(requests) != len(want) {
		t.Fatalf("request count = %d, want %d; got %#v", len(requests), len(want), requests)
	}
	for _, target := range want {
		if _, ok := requests[target]; !ok {
			t.Fatalf("missing request %q; got %#v", target, requests)
		}
	}
	if requests["GET /health"].Properties["http_method_expression"] != "<default>" ||
		requests["GET /health"].Properties["http_raw_method"] != "GET" {
		t.Fatalf("default method evidence = %#v", requests["GET /health"].Properties)
	}
	if requests["POST /constant"].Properties["http_method_expression"] != "(METHOD)" ||
		requests["POST /constant"].Properties["http_raw_method"] != "post" {
		t.Fatalf("constant method evidence = %#v", requests["POST /constant"].Properties)
	}
	for _, target := range []string{"ANY /ordered-unknown", "ANY /dynamic"} {
		if requests[target].Properties["http_method_unknown"] != "true" {
			t.Fatalf("%s did not retain unknown-method evidence: %#v", target, requests[target])
		}
	}
	if requests["ANY /invalid"].Properties["http_invalid"] != "true" {
		t.Fatalf("invalid method was not marked invalid: %#v", requests["ANY /invalid"])
	}
	assertDiagnosticContains(t, result.Diagnostics, "invalid fetch method")
	assertLacksFact(t, result.Facts, graph.EdgeRequests, "POST "+"dynamicURL")
	assertLacksFact(t, result.Facts, graph.EdgeRequests, "POST /not-fetch")
}

func TestParserEvaluatesFetchRequestInitCases(t *testing.T) {
	tests := []struct {
		name        string
		declaration string
		call        string
		want        string
		unknown     bool
	}{
		{name: "explicit undefined defaults", call: `fetch("/undefined", undefined)`, want: "GET /undefined"},
		{name: "constant shorthand", declaration: `const method = "POST" as const;`, call: `fetch("/shorthand", { method })`, want: "POST /shorthand"},
		{name: "computed literal key", call: `fetch("/computed", { ["method"]: "PATCH" })`, want: "PATCH /computed"},
		{name: "unknown computed key after method", call: `fetch("/computed-unknown", { method: "POST", [key]: value })`, want: "ANY /computed-unknown", unknown: true},
		{name: "later method overrides unknown computed key", call: `fetch("/computed-overridden", { [key]: value, method: "POST" })`, want: "POST /computed-overridden"},
		{name: "dynamic options", call: `fetch("/options", init)`, want: "ANY /options", unknown: true},
		{name: "parameter shadows outer constant", declaration: ``, call: `fetch("/shadowed", { method: METHOD })`, want: "ANY /shadowed", unknown: true},
		{name: "dynamic template URL", call: "fetch(`/orders/${value}`, { method: \"POST\" })"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := `const METHOD = "GET" as const;
function send(METHOD: string, key: string, value: unknown, init: RequestInit) {
` + test.declaration + "\n" + test.call + `;
}`
			result, err := typescriptparser.New().Parse(context.Background(), parserapi.Input{
				Path: "src/case.ts", Content: []byte(content), Repository: "sample",
			})
			if err != nil {
				t.Fatal(err)
			}
			requests := requestFactsByTarget(result.Facts)
			if test.want == "" {
				if len(requests) != 0 {
					t.Fatalf("dynamic URL emitted requests: %#v", requests)
				}
				return
			}
			request, ok := requests[test.want]
			if !ok || len(requests) != 1 {
				t.Fatalf("requests = %#v, want only %q", requests, test.want)
			}
			if got := request.Properties["http_method_unknown"] == "true"; got != test.unknown {
				t.Fatalf("unknown evidence = %v, want %v: %#v", got, test.unknown, request)
			}
		})
	}
}

func requestFactsByTarget(facts []graph.Fact) map[string]graph.Fact {
	result := map[string]graph.Fact{}
	for _, fact := range facts {
		if fact.Kind == graph.EdgeRequests {
			result[fact.Target] = fact
		}
	}
	return result
}

func assertHasFactKind(t *testing.T, facts []graph.Fact, kind graph.EdgeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s fact; got %#v", kind, facts)
}

func assertHasNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, name string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.Name == name {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, name, nodes)
}

func assertHasFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
}

func assertLacksFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			t.Fatalf("unexpected %s fact to %q: %#v", kind, target, fact)
		}
	}
}

func assertHasFactAtLine(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string, line int) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target && fact.Location.Line == line {
			return
		}
	}
	t.Fatalf("missing %s fact to %q at line %d; got %#v", kind, target, line, facts)
}

func assertFactCount(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string, expected int) {
	t.Helper()
	count := 0
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			count++
		}
	}
	if count != expected {
		t.Fatalf("expected %d %s facts to %q, got %d: %#v", expected, kind, target, count, facts)
	}
}

func assertHasFactTargetID(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, targetID string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.TargetID == targetID {
			return
		}
	}
	t.Fatalf("missing %s fact to id %q; got %#v", kind, targetID, facts)
}

func assertHasFactProperty(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, key, value string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Properties[key] == value {
			return
		}
	}
	t.Fatalf("missing %s fact with %s=%q; got %#v", kind, key, value, facts)
}

func assertDiagnosticContains(t *testing.T, diagnostics []graph.Diagnostic, substring string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, substring) {
			return
		}
	}
	t.Fatalf("missing diagnostic containing %q; got %#v", substring, diagnostics)
}

func assertLacksDiagnostic(t *testing.T, diagnostics []graph.Diagnostic, substring string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, substring) {
			t.Fatalf("unexpected diagnostic containing %q: %#v", substring, diagnostic)
		}
	}
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	absolute := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
