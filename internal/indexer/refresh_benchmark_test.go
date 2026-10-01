package indexer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func BenchmarkUnchangedRefresh(b *testing.B) {
	ctx, project, service, closeRepository := benchmarkRefreshFixture(b)
	defer closeRepository()
	var gitCommands, membershipNS, changeProbeNS, persistenceNS int64
	b.ResetTimer()
	for range b.N {
		report, err := service.Run(ctx, project, indexer.Options{ReportDetail: indexer.ReportWithoutCounts})
		if err != nil {
			b.Fatal(err)
		}
		gitCommands += int64(report.GitCommands)
		membershipNS += report.Phases.MembershipNS
		changeProbeNS += report.Phases.ChangeProbeNS
		persistenceNS += report.Phases.PersistenceNS
	}
	b.StopTimer()
	b.ReportMetric(float64(gitCommands)/float64(b.N), "git-commands/op")
	b.ReportMetric(float64(membershipNS)/float64(b.N), "membership-ns/op")
	b.ReportMetric(float64(changeProbeNS)/float64(b.N), "change-probe-ns/op")
	b.ReportMetric(float64(persistenceNS)/float64(b.N), "persistence-ns/op")
}

func BenchmarkQueryTriggeredRefresh(b *testing.B) {
	ctx, initial, _, closeRepository := benchmarkRefreshFixture(b)
	closeRepository()
	var gitCommands, gitProbeNS, membershipNS, changeProbeNS int64
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
		report, runErr := indexer.NewService(repository, parserapi.NewRegistry(markdownparser.New())).Run(
			ctx, project, indexer.Options{ReportDetail: indexer.ReportWithoutCounts})
		closeErr := repository.Close()
		if runErr != nil {
			b.Fatal(runErr)
		}
		if closeErr != nil {
			b.Fatal(closeErr)
		}
		gitCommands += int64(report.GitCommands)
		gitProbeNS += report.Phases.GitProbeNS
		membershipNS += report.Phases.MembershipNS
		changeProbeNS += report.Phases.ChangeProbeNS
	}
	b.StopTimer()
	b.ReportMetric(float64(gitCommands)/float64(b.N), "git-commands/op")
	b.ReportMetric(float64(gitProbeNS)/float64(b.N), "git-probe-ns/op")
	b.ReportMetric(float64(membershipNS)/float64(b.N), "membership-ns/op")
	b.ReportMetric(float64(changeProbeNS)/float64(b.N), "change-probe-ns/op")
}

func benchmarkRefreshFixture(b *testing.B) (context.Context, indexer.Project, *indexer.Service, func()) {
	b.Helper()
	ctx := context.Background()
	root := testtemp.Dir(b)
	runGit(b, root, "init", "-b", "main")
	runGit(b, root, "remote", "add", "origin", "git@example.invalid:fixtures/refresh.git")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		b.Fatal(err)
	}
	for index := range 64 {
		write(b, filepath.Join(root, fmt.Sprintf("docs/file-%03d.md", index)), fmt.Sprintf("# File %d\n", index))
	}
	runGit(b, root, "add", ".")
	runGit(b, root, "-c", "user.name=Grafo Benchmark", "-c", "user.email=grafo@example.invalid", "commit", "-m", "fixture")
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
