package indexer_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
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
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(markdownparser.New(), typescriptparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 2 || report.Counts.ByKind[string(graph.KindDocSection)] != 1 || report.Counts.ByEdge[string(graph.EdgeDocuments)] != 4 {
		t.Fatalf("unexpected documentation index report: %#v", report)
	}

	queries := query.NewService(repository)
	flow, err := queries.Neighborhood(ctx, "README.md#runtime-flow", "", 1, query.Outgoing, []graph.EdgeKind{graph.EdgeDocuments}, 20)
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
	defer func() { _ = repository.Close() }()
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
	defer func() { _ = repository.Close() }()
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
	defer func() { _ = repository.Close() }()
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

func TestServiceFailureFlowEditsConvergeWithCleanRebuild(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/failureedit\n\ngo 1.26\n")
	write(t, filepath.Join(root, "producer.go"), `package failureedit
import "errors"
var ErrLoad = errors.New("load")
func Load() error { return ErrLoad }
`)
	write(t, filepath.Join(root, "consumer.go"), `package failureedit
func Run() error { return Load() }
`)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 2 {
		t.Fatalf("initial failure index: report=%#v err=%v", report, err)
	}
	assertOutgoingTarget(t, ctx, repository, "example.com/failureedit.Run", graph.EdgePropagatesError, "example.com/failureedit.Load")

	write(t, filepath.Join(root, "producer.go"), `package failureedit
import "errors"
var ErrLoad = errors.New("load")
func Load() (int, error) { return 0, ErrLoad }
`)
	write(t, filepath.Join(root, "consumer.go"), `package failureedit
import "fmt"
func Run() error {
	_, err := Load()
	if err != nil { return fmt.Errorf("run: %w", err) }
	return nil
}
`)
	updated, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Updated) != 2 {
		t.Fatalf("signature/body edit did not invalidate both semantic files: %#v", updated)
	}
	assertOutgoingTarget(t, ctx, repository, "example.com/failureedit.Run", graph.EdgeWrapsError, "example.com/failureedit.Load")
	assertNoOutgoingTarget(t, ctx, repository, "example.com/failureedit.Run", graph.EdgePropagatesError, "example.com/failureedit.Load")
	incremental, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	clean, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental.ByKind, clean.ByKind) || !reflect.DeepEqual(incremental.ByEdge, clean.ByEdge) || incremental.External != clean.External {
		t.Fatalf("incremental failure flow differs from clean rebuild:\n%#v\n%#v", incremental, clean)
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
	defer func() { _ = repository.Close() }()
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
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 3 || len(report.Diagnostics) != 0 {
		t.Fatalf("unexpected Godot index report: %#v", report)
	}

	queries := query.NewService(repository)
	setting, err := queries.Neighborhood(ctx, "config:project.godot:application/run/main_scene", "", 1,
		query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, setting, "scenes/main")
	reader, err := queries.Neighborhood(ctx, "Settings.main_scene", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, reader, "config:project.godot:application/run/main_scene")
	lookup, err := queries.Neighborhood(ctx, "Settings.bind_button", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, lookup, "scenes/main:Main/StartButton")
	lookup, err = queries.Neighborhood(ctx, "Settings.lookup_button", "", 1, query.Outgoing, nil, 20)
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
	defer func() { _ = repository.Close() }()
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
	defer func() { _ = repository.Close() }()
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
	defer func() { _ = repository.Close() }()
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
	defer func() { _ = repository.Close() }()
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

// TestServiceModelsGodotCompositionAcrossFiles indexes a small Godot project and
// asserts the composition vocabulary resolves across files, that re-indexing an
// unchanged project is a no-op, and that editing an autoload target reconciles
// to exactly the clean-rebuild result.
func TestServiceModelsGodotCompositionAcrossFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeGodotProject(t, root, "res://scripts/game.gd")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", report.Diagnostics)
	}
	queries := query.NewService(repository)

	scene, err := queries.GodotComposition(ctx, "scenes/main", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, scene.OutboundInstances, "scenes/enemy", graph.KindGodotScene)
	assertComposition(t, scene.AttachedScripts, "scripts/player", graph.KindModule)

	autoload, err := queries.GodotComposition(ctx, "godot:autoload:project.godot:Game", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, autoload.AutoloadTargets, "scripts/game", graph.KindModule)
	use, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, use, "godot:autoload:project.godot:Game")

	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repeat.Updated) != 0 || len(repeat.Removed) != 0 {
		t.Fatalf("re-indexing an unchanged Godot project changed the graph: %#v", repeat)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("counts changed on an unchanged re-index:\n%#v\n%#v", before, after)
	}

	// Toggling only the singleton marker must reparse dependent scripts: a
	// disabled autoload is not a global identifier, so the script-side edge has
	// to disappear even though the script itself did not change.
	writeGodotProjectWithMarker(t, root, "res://scripts/game.gd", false)
	disabled, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(disabled.Updated, "scripts/player.gd") {
		t.Fatalf("disabling an autoload did not reparse its dependent script: %#v", disabled.Updated)
	}
	stale, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, reached := range stale.Nodes {
		if reached.Node.Kind == graph.KindGodotAutoload {
			t.Fatalf("a disabled autoload kept its script-side edge: %#v", reached.Node)
		}
	}
	writeGodotProject(t, root, "res://scripts/game.gd")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	writeGodotProject(t, root, "res://scripts/other.gd")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	moved, err := queries.GodotComposition(ctx, "godot:autoload:project.godot:Game", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, moved.AutoloadTargets, "scripts/other", graph.KindModule)
	if len(moved.AutoloadTargets) != 1 {
		t.Fatalf("autoload target was not reconciled: %#v", moved.AutoloadTargets)
	}
	incremental, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rebuiltRoot := t.TempDir()
	writeGodotProject(t, rebuiltRoot, "res://scripts/other.gd")
	rebuiltProject, err := indexer.DiscoverProject(ctx, rebuiltRoot)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := sqlite.Open(ctx, rebuiltProject.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuilt.Close() }()
	if _, err := indexer.NewService(rebuilt, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New())).
		Run(ctx, rebuiltProject, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	clean, err := rebuilt.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental.ByKind, clean.ByKind) || !reflect.DeepEqual(incremental.ByEdge, clean.ByEdge) {
		t.Fatalf("incremental edit differs from a clean rebuild:\n%#v\n%#v", incremental, clean)
	}
}

func writeGodotProject(t *testing.T, root, autoloadTarget string) {
	t.Helper()
	writeGodotProjectWithMarker(t, root, autoloadTarget, true)
}

func writeGodotProjectWithMarker(t *testing.T, root, autoloadTarget string, enabled bool) {
	t.Helper()
	marker := "*"
	if !enabled {
		marker = ""
	}
	for _, directory := range []string{"scenes", "scripts"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "project.godot"),
		"config_version=5\n\n[autoload]\nGame=\""+marker+autoloadTarget+"\"\n")
	write(t, filepath.Join(root, "scripts", "game.gd"), "class_name Game extends Node\nfunc start() -> void:\n\tpass\n")
	write(t, filepath.Join(root, "scripts", "other.gd"), "extends Node\nfunc start() -> void:\n\tpass\n")
	write(t, filepath.Join(root, "scripts", "player.gd"), "extends Node\nfunc ready() -> void:\n\tGame.start()\n")
	write(t, filepath.Join(root, "scenes", "enemy.tscn"),
		"[gd_scene format=3 uid=\"uid://enemy123\"]\n\n[node name=\"Enemy\" type=\"Node2D\"]\n")
	write(t, filepath.Join(root, "scenes", "main.tscn"),
		"[gd_scene load_steps=3 format=3 uid=\"uid://main123\"]\n\n"+
			"[ext_resource type=\"Script\" path=\"res://scripts/player.gd\" id=\"1_player\"]\n"+
			"[ext_resource type=\"PackedScene\" uid=\"uid://enemy123\" path=\"res://scenes/enemy.tscn\" id=\"2_enemy\"]\n\n"+
			"[node name=\"Main\" type=\"Node\"]\nscript = ExtResource(\"1_player\")\n\n"+
			"[node name=\"Enemy\" parent=\".\" instance=ExtResource(\"2_enemy\")]\n")
}

func assertComposition(t *testing.T, relations []query.GodotRelation, qualified string, kind graph.NodeKind) {
	t.Helper()
	for _, relation := range relations {
		if relation.Node.QualifiedName != qualified {
			continue
		}
		if relation.Node.External {
			t.Fatalf("%q resolved to an external node: %#v", qualified, relation.Node)
		}
		if relation.Node.Kind != kind {
			t.Fatalf("%q resolved to kind %q, want %q", qualified, relation.Node.Kind, kind)
		}
		return
	}
	t.Fatalf("missing composition relation to %q: %#v", qualified, relations)
}

// TestServiceRebuildReplacesLegacyGodotRepresentations proves the semantic
// rebuild leaves no duplicate generic representation of a migrated Godot
// concept: a stale index row that still models a scene as a module and a scene
// node as a variable is replaced, not joined, by the new vocabulary.
func TestServiceRebuildReplacesLegacyGodotRepresentations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeGodotProject(t, root, "res://scripts/game.gd")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	legacy := graph.ParseResult{Nodes: []graph.Node{
		{ID: graph.NodeID(graph.KindModule, "scenes/main", "legacy"), Kind: graph.KindModule,
			Name: "main", QualifiedName: "scenes/main", OwnerFile: "scenes/main.tscn"},
		{ID: graph.NodeID(graph.KindVariable, "scenes/main:Main", "legacy"), Kind: graph.KindVariable,
			Name: "Main", QualifiedName: "scenes/main:Main", OwnerFile: "scenes/main.tscn"},
	}}
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "scenes/main.tscn",
		Hash: "legacy", Language: "godot"}, legacy); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", "legacy"); err != nil {
		t.Fatal(err)
	}
	rebuild, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuild.Rebuild == "" {
		t.Fatalf("stale semantic version did not trigger a rebuild: %#v", rebuild)
	}
	assertNodeKinds(t, repository, "scenes/main", map[graph.NodeKind]int{graph.KindGodotScene: 1})
	assertNodeKinds(t, repository, "scenes/main:Main", map[graph.NodeKind]int{graph.KindGodotSceneNode: 1})
}

func assertNodeKinds(t *testing.T, repository graph.ReadRepository, qualified string, want map[graph.NodeKind]int) {
	t.Helper()
	found, err := repository.SearchNodes(context.Background(), qualified, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[graph.NodeKind]int{}
	for _, node := range found {
		if node.QualifiedName != qualified || node.External {
			continue
		}
		counts[node.Kind]++
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("node kinds for %q = %#v, want %#v", qualified, counts, want)
	}
}

// TestServiceScopesGodotProjectsNestedInOneRepository covers a monorepo whose
// Godot projects live in subdirectories. res:// references are project-relative,
// so each project's relationships must resolve inside its own directory, and two
// sibling projects that both declare an autoload named Game must stay
// independently addressable instead of colliding.
func TestServiceScopesGodotProjectsNestedInOneRepository(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, project := range []string{"client", "tools/probe"} {
		write(t, mkdirFor(t, root, project+"/project.godot"),
			"config_version=5\n\n[autoload]\nGame=\"*res://scripts/game.gd\"\n")
		write(t, mkdirFor(t, root, project+"/scripts/game.gd"),
			"extends Node\nfunc start() -> void:\n\tpass\n")
		write(t, mkdirFor(t, root, project+"/scripts/hud.gd"),
			"extends Node\nfunc ready() -> void:\n\tGame.start()\n")
		write(t, mkdirFor(t, root, project+"/scenes/enemy.tscn"),
			"[gd_scene format=3 uid=\"uid://"+strings.ReplaceAll(project, "/", "_")+"_enemy\"]\n\n[node name=\"Enemy\" type=\"Node\"]\n")
		write(t, mkdirFor(t, root, project+"/scenes/main.tscn"),
			"[gd_scene load_steps=3 format=3]\n\n"+
				"[ext_resource type=\"Script\" path=\"res://scripts/hud.gd\" id=\"1_hud\"]\n"+
				"[ext_resource type=\"PackedScene\" path=\"res://scenes/enemy.tscn\" id=\"2_enemy\"]\n\n"+
				"[node name=\"Main\" type=\"Node\"]\nscript = ExtResource(\"1_hud\")\n\n"+
				"[node name=\"Enemy\" parent=\".\" instance=ExtResource(\"2_enemy\")]\n")
	}

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if _, err := indexer.NewService(repository,
		parserapi.NewRegistry(gdscriptparser.New(), godotparser.New())).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	queries := query.NewService(repository)

	for _, directory := range []string{"client", "tools/probe"} {
		scene, err := queries.GodotComposition(ctx, directory+"/scenes/main", query.GodotCompositionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertComposition(t, scene.OutboundInstances, directory+"/scenes/enemy", graph.KindGodotScene)
		assertComposition(t, scene.AttachedScripts, directory+"/scripts/hud", graph.KindModule)

		// Inbound attribution must also stay inside the project: the instancing
		// scene is found from the scene node's own project-scoped identity.
		instanced, err := queries.GodotComposition(ctx, directory+"/scenes/enemy", query.GodotCompositionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertComposition(t, instanced.InboundInstances, directory+"/scenes/main", graph.KindGodotScene)
		if instanced.InboundInstances[0].Via == nil ||
			instanced.InboundInstances[0].Via.QualifiedName != directory+"/scenes/main:Main/Enemy" {
			t.Fatalf("inbound instance lost its scene node evidence: %#v", instanced.InboundInstances[0])
		}

		autoload, err := queries.GodotComposition(ctx,
			"godot:autoload:"+directory+"/project.godot:Game", query.GodotCompositionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertComposition(t, autoload.AutoloadTargets, directory+"/scripts/game", graph.KindModule)
		use, err := queries.Neighborhood(ctx, directory+"/scripts/hud.ready", "", 1, query.Outgoing, nil, 20)
		if err != nil {
			t.Fatal(err)
		}
		assertReached(t, use, "godot:autoload:"+directory+"/project.godot:Game")
	}

	// Nothing may be attributed to a repository-root identity that no file owns.
	for _, qualified := range []string{"scenes/main", "scenes/enemy", "scripts/game", "godot:autoload:Game"} {
		found, err := repository.SearchNodes(ctx, qualified, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range found {
			if node.QualifiedName == qualified {
				t.Fatalf("project-relative reference leaked to the repository root: %#v", node)
			}
		}
	}
}

func mkdirFor(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestServiceKeepsContradictedGodotUIDsUnresolved proves the cross-file UID
// check runs in the real pipeline: the workspace semantic key builds the alias
// table, so a scene whose ext_resource pairs a UID with a path another resource
// declares produces a diagnostic and no composition edge, and repairing the
// path resolves it on the next incremental run.
func TestServiceKeepsContradictedGodotUIDsUnresolved(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, mkdirFor(t, root, "project.godot"), "config_version=5\n")
	write(t, mkdirFor(t, root, "scenes/a.tscn"),
		"[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"A\" type=\"Node\"]\n")
	write(t, mkdirFor(t, root, "scenes/b.tscn"),
		"[gd_scene format=3 uid=\"uid://other\"]\n\n[node name=\"B\" type=\"Node\"]\n")
	caller := func(path string) string {
		return "[gd_scene load_steps=2 format=3]\n\n" +
			"[ext_resource type=\"PackedScene\" uid=\"uid://shared\" path=\"" + path + "\" id=\"1_x\"]\n\n" +
			"[node name=\"Root\" type=\"Node\"]\n\n" +
			"[node name=\"Child\" parent=\".\" instance=ExtResource(\"1_x\")]\n"
	}
	write(t, mkdirFor(t, root, "scenes/caller.tscn"), caller("res://scenes/b.tscn"))

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	contradiction := false
	for _, diagnostic := range report.Diagnostics {
		if strings.Contains(diagnostic.Message, "uid://shared") {
			contradiction = true
		}
	}
	if !contradiction {
		t.Fatalf("contradicted UID produced no diagnostic: %#v", report.Diagnostics)
	}
	scene, err := query.NewService(repository).GodotComposition(ctx, "scenes/caller", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.OutboundInstances) != 0 {
		t.Fatalf("contradicted UID produced a composition edge: %#v", scene.OutboundInstances)
	}

	// Repairing the path to the resource that actually declares the UID makes
	// the same reference resolve, which shows the check is evidence-driven
	// rather than a blanket refusal.
	write(t, filepath.Join(root, "scenes", "caller.tscn"), caller("res://scenes/a.tscn"))
	repaired, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range repaired.Diagnostics {
		if strings.Contains(diagnostic.Message, "uid://shared") {
			t.Fatalf("repaired reference still diagnosed: %#v", repaired.Diagnostics)
		}
	}
	fixed, err := query.NewService(repository).GodotComposition(ctx, "scenes/caller", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, fixed.OutboundInstances, "scenes/a", graph.KindGodotScene)

	// Moving the UID to another resource contradicts a caller that was not
	// itself edited, so the workspace semantic key must reparse it.
	write(t, filepath.Join(root, "scenes", "a.tscn"),
		"[gd_scene format=3 uid=\"uid://moved\"]\n\n[node name=\"A\" type=\"Node\"]\n")
	write(t, filepath.Join(root, "scenes", "b.tscn"),
		"[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"B\" type=\"Node\"]\n")
	moved, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(moved.Updated, "scenes/caller.tscn") {
		t.Fatalf("a moved UID did not reparse the unchanged caller: %#v", moved.Updated)
	}
	stale, err := query.NewService(repository).GodotComposition(ctx, "scenes/caller", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale.OutboundInstances) != 0 {
		t.Fatalf("moved UID left a stale composition edge: %#v", stale.OutboundInstances)
	}
}

// TestServiceReparsesScriptsWhenAutoloadEnablementChangesUnderGit exercises the
// Git selection path, where only changed paths are re-hashed: toggling an
// autoload's singleton marker must still reach the scripts whose resolution it
// changes, even though those scripts are untouched and unstaged.
func TestServiceReparsesScriptsWhenAutoloadEnablementChangesUnderGit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, mkdirFor(t, root, "project.godot"),
		"config_version=5\n\n[autoload]\nGame=\"*res://scripts/game.gd\"\n")
	write(t, mkdirFor(t, root, "scripts/game.gd"), "extends Node\nfunc start() -> void:\n\tpass\n")
	write(t, mkdirFor(t, root, "scripts/player.gd"), "extends Node\nfunc ready() -> void:\n\tGame.start()\n")
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
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	queries := query.NewService(repository)
	use, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, use, "godot:autoload:project.godot:Game")

	write(t, filepath.Join(root, "project.godot"),
		"config_version=5\n\n[autoload]\nGame=\"res://scripts/game.gd\"\n")
	runGit(t, root, "add", "project.godot")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "disable")

	disabled, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(disabled.Updated, "scripts/player.gd") {
		t.Fatalf("Git selection did not reach the dependent script: %#v", disabled.Updated)
	}
	stale, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, reached := range stale.Nodes {
		if reached.Node.Kind == graph.KindGodotAutoload {
			t.Fatalf("a disabled autoload kept its script-side edge: %#v", reached.Node)
		}
	}
}

// TestServiceReconcilesGodotInteractionsAfterProjectEdits is the integrity test
// for the interaction vocabulary: adding and then removing a [global_group]
// declaration must move the scene's and the script's membership edges between the
// declared group and an unresolved boundary node, and each incremental state must
// equal what a clean rebuild of the same tree produces.
func TestServiceReconcilesGodotInteractionsAfterProjectEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, mkdirFor(t, root, "project.godot"),
		"config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n")
	write(t, mkdirFor(t, root, "scenes/arena.tscn"),
		"[gd_scene format=3]\n\n[node name=\"Arena\" type=\"Node2D\" groups=[\"enemies\"]]\n")
	write(t, mkdirFor(t, root, "scripts/spawner.gd"),
		"extends Node\n\nfunc poll() -> void:\n"+
			"\tif Input.is_action_pressed(\"jump\"):\n\t\tget_tree().call_group(\"enemies\", \"die\")\n")

	service, repository, project := openGodotIndex(t, ctx, root)
	defer func() { _ = repository.Close() }()
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	// Before the project declares the group, every membership and dispatch points
	// at an unresolved boundary node: the wiring is visible and unproven.
	assertGodotInteraction(t, ctx, repository, "scenes/arena", "godot:node_group:project.godot:enemies", true)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:node_group:project.godot:enemies", true)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:input_action:project.godot:jump", false)
	assertCleanRebuildMatches(t, ctx, root, repository)

	write(t, filepath.Join(root, "project.godot"),
		"config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n\n[global_group]\nenemies=\"Hostile\"\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	// The scene was not reparsed, yet its membership now resolves to the
	// declaration: by-name reconciliation is what keeps the two in step.
	assertGodotInteraction(t, ctx, repository, "scenes/arena", "godot:node_group:project.godot:enemies", false)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:node_group:project.godot:enemies", false)
	assertCleanRebuildMatches(t, ctx, root, repository)

	write(t, filepath.Join(root, "project.godot"), "config_version=5\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	// Removing both declarations must leave no stale edge to a deleted node and
	// no stale action either.
	assertGodotInteraction(t, ctx, repository, "scenes/arena", "godot:node_group:project.godot:enemies", true)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:input_action:project.godot:jump", true)
	assertCleanRebuildMatches(t, ctx, root, repository)
}

func TestServiceReconcilesCrossFileSignalHandlers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, mkdirFor(t, root, "backend.gd"), "class_name Backend extends Node\nsignal sign_in_success\n")
	write(t, mkdirFor(t, root, "kit.gd"), `class_name Kit extends Node

var _backend: Backend

func wire() -> void:
	_backend.sign_in_success.connect(_on_sign_in)

func _on_sign_in() -> void:
	pass
`)
	service, repository, project := openGodotIndex(t, ctx, root)
	defer func() { _ = repository.Close() }()
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, false)
	assertCleanRebuildMatches(t, ctx, root, repository)

	write(t, filepath.Join(root, "duplicate.gd"), "class_name Backend extends Node\nsignal sign_in_success\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, true)
	assertCleanRebuildMatches(t, ctx, root, repository)

	if err := os.Remove(filepath.Join(root, "duplicate.gd")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, false)

	write(t, filepath.Join(root, "backend.gd"), "class_name Backend extends Node\nsignal signed_in\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, true)
	assertCleanRebuildMatches(t, ctx, root, repository)
}

func assertCrossFileHandler(t *testing.T, ctx context.Context, repository graph.Repository, external bool) {
	t.Helper()
	handler, err := query.NewService(repository).Resolve(ctx, "Kit._on_sign_in")
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesTo(ctx, handler.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind != graph.EdgeHandledBy {
			continue
		}
		source, err := repository.Node(ctx, edge.FromID)
		if err != nil {
			t.Fatal(err)
		}
		if source.QualifiedName != "Backend.sign_in_success" || source.Kind != graph.KindEvent || source.External != external {
			t.Fatalf("handled_by source = %#v, want event external=%v", source, external)
		}
		return
	}
	t.Fatalf("missing handled_by edge to %#v; inbound=%#v", handler, edges)
}

func openGodotIndex(t *testing.T, ctx context.Context, root string) (*indexer.Service, *sqlite.Repository, indexer.Project) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	return indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New())),
		repository, project
}

// assertGodotInteraction checks that one interaction reaches the named far side
// and that its resolution state is what the evidence licenses.
func assertGodotInteraction(t *testing.T, ctx context.Context, repository graph.Repository, selector, qualified string, external bool) {
	t.Helper()
	report, err := query.NewService(repository).GodotInteractions(ctx, selector,
		query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatalf("interactions for %s: %v", selector, err)
	}
	for _, interaction := range report.Outbound {
		if interaction.Node.QualifiedName != qualified {
			continue
		}
		if interaction.Node.External != external {
			t.Fatalf("%s -> %s external = %v, want %v", selector, qualified,
				interaction.Node.External, external)
		}
		return
	}
	t.Fatalf("%s has no interaction with %s; got %#v", selector, qualified, report.Outbound)
}

// assertCleanRebuildMatches indexes the same tree into a fresh index and compares
// the edge and node census, which is the property an incremental index has to
// preserve for the interaction vocabulary to be trustworthy.
func assertCleanRebuildMatches(t *testing.T, ctx context.Context, root string, incremental graph.Repository) {
	t.Helper()
	clean := t.TempDir()
	if err := copyTree(t, root, clean); err != nil {
		t.Fatal(err)
	}
	service, repository, project := openGodotIndex(t, ctx, clean)
	defer func() { _ = repository.Close() }()
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	want, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := incremental.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want.ByKind, got.ByKind) || !reflect.DeepEqual(want.ByEdge, got.ByEdge) {
		t.Fatalf("incremental index differs from a clean rebuild:\nkinds %v vs %v\nedges %v vs %v",
			got.ByKind, want.ByKind, got.ByEdge, want.ByEdge)
	}
	if want.External != got.External {
		t.Fatalf("external node count = %d, clean rebuild = %d", got.External, want.External)
	}
}

func copyTree(t *testing.T, from, to string) error {
	t.Helper()
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		// The index lives inside the tree; copying it would make the rebuild
		// incremental instead of clean.
		if strings.HasPrefix(relative, ".grafo") {
			return filepath.SkipDir
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
}
