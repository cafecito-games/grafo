package indexer_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// writeUnchangedRefreshCorpus lays out a committed Go repository carrying the
// root control file, which is the shape every real corpus has.
func writeUnchangedRefreshCorpus(t *testing.T, root string, ignoreConfig bool) {
	t.Helper()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\n\ntype Runner interface{ Run() error }\n")
	write(t, filepath.Join(root, "worker", "worker.go"), `package worker

import "example.com/sample/contract"

type Worker struct{}

func (Worker) Run() error { return nil }

func Drive(runner contract.Runner) error { return runner.Run() }
`)
	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  paths:\n    \"db/**/*.sql\": sqlite\n")
	if ignoreConfig {
		write(t, filepath.Join(root, ".gitignore"), "grafo.yaml\n")
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
}

// refreshUnchangedRefreshCorpus rediscovers root and runs one more pass against
// the same database, which is exactly what a second grafo process does.
// selfProbeGitCommands is what a run spends proving the freshness of the tree
// it indexes when the caller supplied no token. The refresh-cost budgets below
// are about change detection, so they exclude it rather than absorbing it.
func selfProbeGitCommands(t *testing.T, ctx context.Context, root string, registry *parserapi.Registry) int {
	t.Helper()
	probe, err := indexer.ProbeFreshness(ctx, root, registry, indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return probe.GitCommands
}

func refreshUnchangedRefreshCorpus(t *testing.T, ctx context.Context, repository *sqlite.Repository, root string) indexer.Report {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	registry := parserapi.NewRegistry(golangparser.New(), configparser.New())
	report, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// gitPathLines runs one Git command in root and returns the paths it printed on
// stdout, so a test can assert on what Git itself reports. Stdout only: Git sends
// advice about line-ending normalization to stderr, and that text names the very
// path under test.
func gitPathLines(t *testing.T, root string, args ...string) []string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	lines := make([]string, 0)
	for _, line := range strings.Split(string(output), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func openUnchangedRefreshIndex(t *testing.T, ctx context.Context, root string) *sqlite.Repository {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

// TestServiceUnchangedRefreshReadsNoFilesWithGoSources proves that a refresh of
// a repository where nothing changed opens no file at all. Go sources are
// load-bearing: indexing them writes a semantic view cache under the repository
// root, so a run mutates a path inside the tree it just indexed and must still
// not select anything on the next pass.
func TestServiceUnchangedRefreshReadsNoFilesWithGoSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	writeUnchangedRefreshCorpus(t, root, false)
	repository := openUnchangedRefreshIndex(t, ctx, root)

	cold := refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	if cold.Checked == 0 {
		t.Fatalf("cold run read no files: %#v", cold)
	}
	indexed := cold.Checked

	// Two consecutive refreshes: the first proves the steady state is reached,
	// the second proves it is stable rather than a one-run artifact.
	for round := 1; round <= 2; round++ {
		report := refreshUnchangedRefreshCorpus(t, ctx, repository, root)
		if report.Checked != 0 || len(report.Updated) != 0 || len(report.Removed) != 0 {
			t.Fatalf("round %d was not a zero-read unchanged refresh: checked=%d updated=%v removed=%v",
				round, report.Checked, report.Updated, report.Removed)
		}
		if report.Unchanged != indexed {
			t.Fatalf("round %d accounted for %d of %d indexed files as unchanged", round, report.Unchanged, indexed)
		}
		// Proving the control file tracked costs one index lookup on top of the
		// three change probes. Pinning the total keeps that the whole price of
		// reading nothing.
		budget := 4 + selfProbeGitCommands(t, ctx, root, parserapi.NewRegistry(golangparser.New(), configparser.New()))
		if report.GitCommands > budget {
			t.Fatalf("round %d spent %d Git commands on an unchanged refresh, want at most %d", round, report.GitCommands, budget)
		}
	}
}

// TestServiceUnchangedRefreshHashesConfigGitIgnores pins the reason the control
// file may still cost a read: an ignored grafo.yaml reaches neither Git diff nor
// the untracked list, so hashing it is the only proof it has not changed.
func TestServiceUnchangedRefreshHashesConfigGitIgnores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	writeUnchangedRefreshCorpus(t, root, true)
	repository := openUnchangedRefreshIndex(t, ctx, root)

	refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	report := refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	if report.Checked != 1 || len(report.Updated) != 0 {
		t.Fatalf("ignored control file was not hashed on its own: checked=%d updated=%v", report.Checked, report.Updated)
	}

	// The hash has to be load-bearing, not just spent: an edit Git cannot report
	// must still reach the graph.
	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  paths:\n    \"db/**/*.sql\": sqlite\n    \"legacy/**/*.sql\": sqlite\n")
	edited := refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	if !slices.Contains(edited.Updated, "grafo.yaml") {
		t.Fatalf("edit to an ignored control file went unnoticed: %#v", edited.Updated)
	}
}

// TestServiceUnchangedRefreshNoticesConfigEditGitFiltersAway pins the narrowest
// case for treating a tracked control file as covered by Git. Under `text=auto`
// a line-ending change is normalized away by the clean filter, so
// `git diff --name-only HEAD` reports a clean tree while the bytes the indexer
// hashes have changed. Porcelain status still flags the path as worktree-
// modified, which is what change detection reads, so the edit is selected
// anyway. The test exists because that is the evidence the skipped content check
// now leans on, and nothing else states it.
func TestServiceUnchangedRefreshNoticesConfigEditGitFiltersAway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	writeUnchangedRefreshCorpus(t, root, false)
	write(t, filepath.Join(root, ".gitattributes"), "* text=auto\n")
	runGit(t, root, "add", ".gitattributes")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "normalize")
	repository := openUnchangedRefreshIndex(t, ctx, root)

	refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	if report := refreshUnchangedRefreshCorpus(t, ctx, repository, root); report.Checked != 0 {
		t.Fatalf("steady state read %d files under a clean filter", report.Checked)
	}

	// The edit has to reach the graph even though a Git diff of the tree is
	// clean, so the assertion below is about selection, not about the diff.
	write(t, filepath.Join(root, "grafo.yaml"), "sql:\r\n  paths:\r\n    \"db/**/*.sql\": sqlite\r\n")
	if changed := gitPathLines(t, root, "diff", "--name-only", "HEAD", "--"); slices.Contains(changed, "grafo.yaml") {
		t.Skipf("this Git configuration reports the filtered edit, so the gap under test is absent: %v", changed)
	}
	edited := refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	if !slices.Contains(edited.Updated, "grafo.yaml") {
		t.Fatalf("edit Git filtered away went unnoticed: %#v", edited.Updated)
	}
}

// TestServiceUnchangedRefreshHashesConfigGitStoppedReporting covers the other way
// a tracked control file leaves Git's diffs: assume-unchanged tells Git not to
// report modifications to it, so being in the index is not on its own proof that
// a diff would have caught an edit.
func TestServiceUnchangedRefreshHashesConfigGitStoppedReporting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testtemp.Dir(t)
	writeUnchangedRefreshCorpus(t, root, false)
	runGit(t, root, "update-index", "--assume-unchanged", "grafo.yaml")
	repository := openUnchangedRefreshIndex(t, ctx, root)

	refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	report := refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	if report.Checked != 1 || len(report.Updated) != 0 {
		t.Fatalf("control file Git stopped reporting was not hashed: checked=%d updated=%v", report.Checked, report.Updated)
	}

	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  paths:\n    \"db/**/*.sql\": sqlite\n    \"legacy/**/*.sql\": sqlite\n")
	edited := refreshUnchangedRefreshCorpus(t, ctx, repository, root)
	if !slices.Contains(edited.Updated, "grafo.yaml") {
		t.Fatalf("edit Git stopped reporting went unnoticed: %#v", edited.Updated)
	}
}
