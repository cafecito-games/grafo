package indexer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	pythonparser "github.com/cafecito-games/grafo/internal/parser/python"
	sqlparser "github.com/cafecito-games/grafo/internal/parser/sql"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestServiceIndexesOnlyChangedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "main.go"), "package main\nfunc main() { helper() }\nfunc helper() {}\n")
	write(t, filepath.Join(root, "worker.py"), "def run():\n    return True\n")
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
	registry := parserapi.NewRegistry(golangparser.New(), pythonparser.New(), typescriptparser.New(), sqlparser.New(postgresparser.New()), configparser.New())
	service := indexer.NewService(repository, registry)

	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 5 || first.Unchanged != 0 {
		t.Fatalf("unexpected initial report: %#v", first)
	}
	second, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 0 || second.Unchanged != 5 {
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

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
