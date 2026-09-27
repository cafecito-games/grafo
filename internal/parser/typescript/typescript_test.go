package typescript_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
)

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
	writeFile(t, root, "packages/tools/package.json", `{"name":"@sample/tools","exports":{".":{"types":"./src/index.ts","default":"./dist/index.js"}}}`)
	writeFile(t, root, "packages/tools/src/index.ts", `export function packaged() {}`)
	caller := `
import { RenamedService as Service, type Runner, direct as invoke } from "@app/barrel";
import * as API from "./barrel";
import directDefault from "./service";
import { indexed } from "./directory";
import { packaged } from "@sample/tools";
import "missing-package";
function use(runner: Runner) {
  invoke();
  directDefault();
  indexed();
  packaged();
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
		graph.NodeID(graph.KindModule, "repo:sample:src/barrel"))
	assertHasFactProperty(t, result.Facts, graph.EdgeImports, "local", "Service")
	assertHasFactProperty(t, result.Facts, graph.EdgeImports, "type_only", "true")
	for _, target := range []string{
		"src/service.direct",
		"src/directory.indexed",
		"packages/tools/src.packaged",
		"src/service.Service.create",
		"src/service.Service.run",
		"src/service.Runner.run",
	} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, target)
	}
	assertHasFact(t, result.Facts, graph.EdgeImports, "missing-package")
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
