package indexer_test

// This benchmark intentionally uses reflection for the optional report-detail
// field so the exact same source can be copied into the issue baseline checkout
// at 6002838. That keeps before/after samples load-bearing and comparable even
// though the baseline predates count omission.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func BenchmarkCompatUnchangedRefresh(b *testing.B) {
	ctx, project, service, closeRepository := compatRefreshFixture(b)
	defer closeRepository()
	options := compatNoCountOptions()
	b.ResetTimer()
	for range b.N {
		if _, err := service.Run(ctx, project, options); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompatQueryTriggeredRefresh(b *testing.B) {
	ctx, initial, _, closeRepository := compatRefreshFixture(b)
	closeRepository()
	options := compatNoCountOptions()
	b.ResetTimer()
	for range b.N {
		project, err := indexer.DiscoverProject(ctx, initial.Root)
		if err != nil {
			b.Fatal(err)
		}
		repository, err := sqlite.Open(ctx, project.IndexPath)
		if err != nil {
			b.Fatal(err)
		}
		_, runErr := indexer.NewService(repository, parserapi.NewRegistry(markdownparser.New())).Run(ctx, project, options)
		closeErr := repository.Close()
		if runErr != nil {
			b.Fatal(runErr)
		}
		if closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}

func compatNoCountOptions() indexer.Options {
	options := indexer.Options{}
	value := reflect.ValueOf(&options).Elem().FieldByName("ReportDetail")
	if value.IsValid() && value.CanSet() && value.Kind() == reflect.String {
		value.SetString("without_counts")
	}
	return options
}

func compatRefreshFixture(b *testing.B) (context.Context, indexer.Project, *indexer.Service, func()) {
	b.Helper()
	ctx := context.Background()
	root := testtemp.Dir(b)
	compatGit(b, root, "init", "-b", "main")
	compatGit(b, root, "remote", "add", "origin", "git@example.invalid:fixtures/refresh.git")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		b.Fatal(err)
	}
	for index := range 64 {
		compatWrite(b, filepath.Join(root, "docs", fmt.Sprintf("file-%03d.md", index)), fmt.Sprintf("# File %d\n", index))
	}
	compatGit(b, root, "add", ".")
	compatGit(b, root, "-c", "user.name=Grafo Benchmark", "-c", "user.email=grafo@example.invalid", "commit", "-m", "fixture")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		b.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		b.Fatal(err)
	}
	service := indexer.NewService(repository, parserapi.NewRegistry(markdownparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		_ = repository.Close()
		b.Fatal(err)
	}
	return ctx, project, service, func() {
		if err := repository.Close(); err != nil {
			b.Error(err)
		}
	}
}

func compatGit(b *testing.B, directory string, arguments ...string) {
	b.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		b.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func compatWrite(b *testing.B, path, content string) {
	b.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.Fatal(err)
	}
}
