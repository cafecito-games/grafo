package indexer_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	pythonparser "github.com/cafecito-games/grafo/internal/parser/python"
	sqlparser "github.com/cafecito-games/grafo/internal/parser/sql"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestServiceLinksDocumentationSectionsToCode(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "README.md"), "# Runtime flow\nThe function `Serve` delegates to class `Checkout` through endpoint `GET /orders`.\nSee [the implementation](server.ts).\n")
	write(t, filepath.Join(root, "server.ts"), "export class Checkout {}\nexport function Serve() {}\nconst app = express();\napp.get(\"/orders\", Serve);\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := indexer.NewService(repository, parserapi.NewRegistry(markdownparser.New(), typescriptparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 2 || report.Counts.ByKind[string(graph.KindDocSection)] != 1 || report.Counts.ByEdge[string(graph.EdgeDocuments)] != 4 {
		t.Fatalf("unexpected documentation index report: %#v", report)
	}

	queries := query.NewService(repository)
	flow, err := queries.Neighborhood(ctx, "README.md#runtime-flow", 1, query.Outgoing, []graph.EdgeKind{graph.EdgeDocuments}, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, flow, "server.ts")
	assertReached(t, flow, "server.Serve")
	assertReached(t, flow, "server.Checkout")
	for _, reached := range flow.Nodes {
		if reached.Node.Kind == graph.KindEndpoint && reached.Node.Name == "GET /orders" && !reached.Node.External {
			return
		}
	}
	t.Fatalf("documentation did not resolve to endpoint: %#v", flow.Nodes)
}

func TestServiceReconcilesTypeScriptBindingsAfterBarrelAndConfigEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["src/*"]}}}`)
	write(t, filepath.Join(root, "src", "service.ts"), `export class Service { run() {} }`)
	write(t, filepath.Join(root, "src", "other.ts"), `export class Other { run() {} }`)
	write(t, filepath.Join(root, "src", "barrel.ts"), `export { Service } from "./service";`)
	write(t, filepath.Join(root, "src", "app.ts"), `
import { Service } from "@app/barrel";
export function execute() { const service = new Service(); service.run(); }
`)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	typeScriptParser := typescriptparser.New()
	service := indexer.NewService(repository, parserapi.NewRegistry(typeScriptParser, configparser.New()))
	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 5 {
		t.Fatalf("unexpected first TypeScript report: %#v", first)
	}
	if metrics := typeScriptParser.ResolutionMetrics(); metrics.CatalogLoads != 1 || metrics.CacheHits != 3 || metrics.CacheEntries != 1 || metrics.CachedModules != 4 {
		t.Fatalf("TypeScript catalog cache was not bounded and reused: %#v", metrics)
	}
	assertOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/service.Service.run")

	write(t, filepath.Join(root, "src", "barrel.ts"), `export { Other as Service } from "./other";`)
	second, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 4 { // all TypeScript modules, but not unchanged tsconfig JSON
		t.Fatalf("barrel change did not invalidate the TypeScript catalog: %#v", second)
	}
	assertOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/other.Other.run")
	assertNoOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/service.Service.run")

	write(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["missing/*"]}}}`)
	third, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Updated) != 5 {
		t.Fatalf("config change did not invalidate TypeScript modules: %#v", third)
	}
	assertNoOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/other.Other.run")

	clean, err := service.Run(ctx, project, indexer.Options{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Updated) != 5 {
		t.Fatalf("forced clean rebuild did not converge: %#v", clean)
	}
	assertNoOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/other.Other.run")
}

func TestServiceIndexesOnlyChangedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "main.go"), "package main\nfunc main() { helper() }\nfunc helper() {}\n")
	write(t, filepath.Join(root, "worker.py"), "def run():\n    return True\n")
	write(t, filepath.Join(root, "player.gd"), "class_name Player\nfunc run():\n\tpass\n")
	write(t, filepath.Join(root, "web.ts"), "export function handler() { return process.env.API_URL }\n")
	write(t, filepath.Join(root, "schema.sql"), "CREATE TABLE events (id bigint PRIMARY KEY);\n")
	write(t, filepath.Join(root, ".env"), "API_URL=http://localhost:8080\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	registry := parserapi.NewRegistry(gdscriptparser.New(), golangparser.New(), pythonparser.New(), typescriptparser.New(), sqlparser.New(postgresparser.New()), configparser.New())
	service := indexer.NewService(repository, registry)

	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 6 || first.Unchanged != 0 {
		t.Fatalf("unexpected initial report: %#v", first)
	}
	second, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 0 || second.Unchanged != 6 {
		t.Fatalf("expected incremental no-op: %#v", second)
	}

	write(t, filepath.Join(root, "main.go"), "package main\nfunc main() { helper() }\nfunc helper() { println(\"changed\") }\n")
	if err := os.Remove(filepath.Join(root, "web.ts")); err != nil {
		t.Fatal(err)
	}
	third, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Updated) != 1 || len(third.Removed) != 1 || third.Removed[0] != "web.ts" {
		t.Fatalf("unexpected incremental update: %#v", third)
	}
}

func TestServiceReindexesGoDependentsWhenTypeEvidenceChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, "contract"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\ntype Runner interface { Run() }\n")
	write(t, filepath.Join(root, "worker", "worker.go"), "package worker\ntype Worker struct{}\nfunc (Worker) Run() {}\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	goParser := golangparser.New()
	service := indexer.NewService(repository, parserapi.NewRegistry(goParser))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 2 {
		t.Fatalf("initial index: report=%#v err=%v", report, err)
	}
	if metrics := goParser.SemanticLoadMetrics(); metrics.Loads != 1 || metrics.PeakConcurrent != 1 {
		t.Fatalf("semantic package loading was not bounded and shared: %#v", metrics)
	}

	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\ntype Runner interface { Run(); Stop() }\n")
	reindexed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reindexed.Updated) != 2 {
		t.Fatalf("expected changed package and semantic dependents to reindex, got %#v", reindexed)
	}
	if reindexed.Counts.ByEdge[string(graph.EdgeImplements)] != 0 {
		t.Fatalf("stale implements edge survived interface edit: %#v", reindexed.Counts.ByEdge)
	}
	if metrics := goParser.SemanticLoadMetrics(); metrics.Loads != 2 {
		t.Fatalf("semantic cache did not invalidate after interface edit: %#v", metrics)
	}
	changedGOOS := "linux"
	if runtime.GOOS == changedGOOS {
		changedGOOS = "darwin"
	}
	t.Setenv("GOOS", changedGOOS)
	contextChanged, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(contextChanged.Updated) != 2 {
		t.Fatalf("build-context change did not invalidate Go semantic evidence: %#v", contextChanged)
	}
}

func TestServiceSurfacesTrackedSymlinksWithoutFollowingThem(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 1 }\n")
	if err := os.Symlink("main.go", filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "main.go", "alias.go")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "fixture")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	report, err := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New())).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 1 || report.Updated[0] != "main.go" || len(report.Skipped) != 1 || report.Skipped[0] != "alias.go" {
		t.Fatalf("unexpected symlink report: %#v", report)
	}
	if report.Counts.Files != 1 {
		t.Fatalf("symlink was followed into the graph: %#v", report.Counts)
	}
}

func TestServiceIndexesAndLinksGodotProjectSources(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "project.godot"), "config_version=5\n\n[application]\nrun/main_scene=\"res://scenes/main.tscn\"\n")
	write(t, filepath.Join(root, "settings.gd"), "class_name Settings\nfunc main_scene():\n\treturn ProjectSettings.get_setting(\"application/run/main_scene\")\nfunc bind_button():\n\treturn %StartButton\nfunc lookup_button():\n\treturn get_node(\"Main/StartButton\")\n")
	if err := os.MkdirAll(filepath.Join(root, "scenes"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "scenes", "main.tscn"), "[gd_scene format=3]\n\n[node name=\"Main\" type=\"Node\"]\n\n[node name=\"StartButton\" type=\"Button\" parent=\".\" unique_name_in_owner=true]\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 3 || len(report.Diagnostics) != 0 {
		t.Fatalf("unexpected Godot index report: %#v", report)
	}

	queries := query.NewService(repository)
	setting, err := queries.Neighborhood(ctx, "config:project.godot:application/run/main_scene", 1,
		query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, setting, "scenes/main")
	reader, err := queries.Neighborhood(ctx, "Settings.main_scene", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, reader, "config:project.godot:application/run/main_scene")
	lookup, err := queries.Neighborhood(ctx, "Settings.bind_button", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, lookup, "scenes/main:Main/StartButton")
	lookup, err = queries.Neighborhood(ctx, "Settings.lookup_button", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, lookup, "scenes/main:Main/StartButton")
}

func TestServiceReindexesSQLWhenDialectConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "schema.sql"), "CREATE TABLE events (id bigint PRIMARY KEY);\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	registry := parserapi.NewRegistry(sqlparser.New(postgresparser.New()), configparser.New())
	service := indexer.NewService(repository, registry)

	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 1 || first.Updated[0] != "schema.sql" {
		t.Fatalf("unexpected initial report: %#v", first)
	}

	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  default_dialect: postgres\n")
	second, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 2 || second.Updated[0] != "grafo.yaml" || second.Updated[1] != "schema.sql" {
		t.Fatalf("expected config and unchanged SQL to be reindexed: %#v", second)
	}

	third, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Updated) != 0 || third.Unchanged != 2 {
		t.Fatalf("expected incremental no-op: %#v", third)
	}
}

func TestServiceTracksGitDirtyPathsAcrossRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 1 }\n")
	runGit(t, root, "add", "main.go")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 1 || report.Checked != 1 {
		t.Fatalf("initial index: report=%#v err=%v", report, err)
	}

	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 2 }\n")
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 1 || report.Checked != 1 {
		t.Fatalf("dirty index: report=%#v err=%v", report, err)
	}
	runGit(t, root, "checkout", "--", "main.go")
	restored, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Updated) != 1 || restored.Updated[0] != "main.go" || restored.Checked != 1 {
		t.Fatalf("restored tracked file was not reindexed: %#v", restored)
	}
	stable, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stable.Updated) != 0 || stable.Unchanged != 1 || stable.Checked != 0 {
		t.Fatalf("clean worktree was not an incremental no-op: %#v", stable)
	}
}

func TestServicePropagatesGitConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "schema.sql"), "CREATE TABLE events (id bigint PRIMARY KEY);\n")
	runGit(t, root, "add", "schema.sql")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := indexer.NewService(repository, parserapi.NewRegistry(sqlparser.New(postgresparser.New()), configparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  default_dialect: postgres\n")
	reconfigured, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reconfigured.Updated) != 2 || reconfigured.Updated[0] != "grafo.yaml" || reconfigured.Updated[1] != "schema.sql" || reconfigured.Checked != 2 {
		t.Fatalf("semantic dependency did not propagate through Git change selection: %#v", reconfigured)
	}
}

func TestServiceResumesSemanticRebuildWithoutReplacingCompletedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 1 }\n")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 1 {
		t.Fatalf("initial index: report=%#v err=%v", report, err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", "interrupted-version"); err != nil {
		t.Fatal(err)
	}

	resumed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Rebuild == "" || len(resumed.Updated) != 0 || resumed.Unchanged != 1 || resumed.Checked != 1 {
		t.Fatalf("semantic rebuild did not resume from per-file hashes: %#v", resumed)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertReached(t *testing.T, traversal query.Traversal, qualified string) {
	t.Helper()
	for _, reached := range traversal.Nodes {
		if reached.Node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("did not reach %q: %#v", qualified, traversal)
}

func assertOutgoingTarget(t *testing.T, ctx context.Context, repository graph.Repository, from string, kind graph.EdgeKind, target string) {
	t.Helper()
	service := query.NewService(repository)
	node, err := service.Resolve(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind != kind {
			continue
		}
		to, err := repository.Node(ctx, edge.ToID)
		if err != nil {
			t.Fatal(err)
		}
		if to.QualifiedName == target {
			return
		}
	}
	t.Fatalf("missing %s edge from %s to %s; got %#v", kind, from, target, edges)
}

func assertNoOutgoingTarget(t *testing.T, ctx context.Context, repository graph.Repository, from string, kind graph.EdgeKind, target string) {
	t.Helper()
	service := query.NewService(repository)
	node, err := service.Resolve(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind != kind {
			continue
		}
		to, err := repository.Node(ctx, edge.ToID)
		if err != nil {
			t.Fatal(err)
		}
		if to.QualifiedName == target {
			t.Fatalf("unexpected %s edge from %s to %s", kind, from, target)
		}
	}
}
