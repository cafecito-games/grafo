package indexseed

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestSeedAdoptsASiblingIndexWithoutCarryingItsWorkingTree is the whole contract
// in one pass: a donor index built over uncommitted edits and an untracked file
// is adopted, and the adopting pass keeps only what this worktree can prove.
func TestSeedAdoptsASiblingIndexWithoutCarryingItsWorkingTree(t *testing.T) {
	ctx := context.Background()
	main := newRepository(t)
	write(t, filepath.Join(main, "shared", "shared.go"), "package shared\n\nfunc SharedByBothWorktrees() {}\n")
	write(t, filepath.Join(main, "edited", "edited.go"), "package edited\n\nfunc CommittedOnly() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-m", "committed state")

	// The donor's index is written over state that exists only in the donor's
	// directory: an uncommitted edit and an untracked file.
	write(t, filepath.Join(main, "edited", "edited.go"),
		"package edited\n\nfunc CommittedOnly() {}\n\nfunc UncommittedInDonorOnly() {}\n")
	write(t, filepath.Join(main, "untracked", "untracked.go"), "package untracked\n\nfunc UntrackedInDonorOnly() {}\n")
	indexProject(t, ctx, main, nil)
	assertMatchTotal(t, ctx, indexPathOf(t, ctx, main), "UncommittedInDonorOnly", 1)
	assertMatchTotal(t, ctx, indexPathOf(t, ctx, main), "UntrackedInDonorOnly", 1)

	worktree := addWorktree(t, main, "issue-163")
	project := discover(t, ctx, worktree)
	if _, err := os.Stat(project.IndexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a new worktree already had an index: %v", err)
	}

	result, err := Seed(ctx, project, Options{TryLock: alwaysLock})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Seeded || result.Provenance == nil {
		t.Fatalf("seed result = %#v", result)
	}
	if result.Provenance.DonorRoot != main {
		t.Fatalf("donor root = %q, want %q", result.Provenance.DonorRoot, main)
	}
	if result.Provenance.ChangedPaths != 0 {
		t.Fatalf("a worktree branched at HEAD reported %d changed paths", result.Provenance.ChangedPaths)
	}

	// The published index must already describe this worktree, because branch
	// index inventory rejects a stored root that disagrees with the project.
	inspection, err := sqlite.InspectIndex(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Metadata.Root != project.Root || inspection.Metadata.Branch != project.Branch {
		t.Fatalf("seeded metadata = %#v, want root %q branch %q",
			inspection.Metadata, project.Root, project.Branch)
	}

	report := indexProject(t, ctx, worktree, result.Provenance)
	if report.Seed == nil || report.Seed.DonorRoot != main {
		t.Fatalf("report seed provenance = %#v", report.Seed)
	}
	// Every discovered file is read and hashed, which is what makes reuse
	// content-proven rather than inherited from the donor's ledger.
	if report.Checked < 2 {
		t.Fatalf("adopting pass content-checked %d files; it must visit the whole membership", report.Checked)
	}
	// The donor's uncommitted function cannot survive: this worktree holds the
	// committed content of that file.
	assertMatchTotal(t, ctx, project.IndexPath, "UncommittedInDonorOnly", 0)
	// The donor's untracked file does not exist here at all, so its facts go.
	assertMatchTotal(t, ctx, project.IndexPath, "UntrackedInDonorOnly", 0)
	if !slices.Contains(report.Removed, "untracked/untracked.go") {
		t.Fatalf("removed = %q, want the donor's untracked file", report.Removed)
	}
	assertMatchTotal(t, ctx, project.IndexPath, "SharedByBothWorktrees", 1)
	t.Logf("donor-with-working-tree-state: updated=%q unchanged=%d checked=%d",
		report.Updated, report.Unchanged, report.Checked)
}

// TestSeedClearsTheDonorsFreshnessToken keeps adoption from carrying a proof of
// freshness across worktrees. The donor's token describes the donor's inputs; an
// adopter that kept it could be served without ever reconciling.
func TestSeedClearsTheDonorsFreshnessToken(t *testing.T) {
	ctx := context.Background()
	main := newRepository(t)
	write(t, filepath.Join(main, "shared", "shared.go"), "package shared\n\nfunc Shared() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-m", "committed state")
	indexProject(t, ctx, main, nil)
	if donorToken := metaValue(t, ctx, indexPathOf(t, ctx, main), indexer.FreshnessTokenMeta); donorToken == "" {
		t.Fatal("the donor index recorded no freshness token, so clearing it would prove nothing")
	}

	worktree := addWorktree(t, main, "issue-240")
	project := discover(t, ctx, worktree)

	result, err := Seed(ctx, project, Options{TryLock: alwaysLock})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Seeded {
		t.Fatalf("seed result = %#v", result)
	}

	if stored := metaValue(t, ctx, project.IndexPath, indexer.FreshnessTokenMeta); stored != "" {
		t.Fatalf("a seeded index carried freshness token %q", stored)
	}
}

// TestSeedFromACleanDonorReusesEveryFile measures where the saving actually
// comes from: a worktree created at the donor's committed state holds an
// identical tree, so every file is proven unchanged by its content hash and
// nothing is parsed again.
func TestSeedFromACleanDonorReusesEveryFile(t *testing.T) {
	ctx := context.Background()
	main := newRepository(t)
	write(t, filepath.Join(main, "shared", "shared.go"), "package shared\n\nfunc SharedByBothWorktrees() {}\n")
	write(t, filepath.Join(main, "other", "other.go"), "package other\n\nfunc AlsoShared() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-m", "committed state")
	indexProject(t, ctx, main, nil)

	worktree := addWorktree(t, main, "clean-donor")
	project := discover(t, ctx, worktree)
	result, err := Seed(ctx, project, Options{TryLock: alwaysLock})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Seeded {
		t.Fatalf("result = %#v", result)
	}
	report := indexProject(t, ctx, worktree, result.Provenance)
	if len(report.Updated) != 0 {
		t.Fatalf("updated = %q, want every file reused from the donor", report.Updated)
	}
	if len(report.Removed) != 0 {
		t.Fatalf("removed = %q, want nothing removed", report.Removed)
	}
	if report.Unchanged != report.Checked || report.Checked == 0 {
		t.Fatalf("unchanged %d of %d checked files", report.Unchanged, report.Checked)
	}
	assertMatchTotal(t, ctx, project.IndexPath, "SharedByBothWorktrees", 1)
	assertMatchTotal(t, ctx, project.IndexPath, "AlsoShared", 1)
}

// TestSeedDeclinesWithoutAnEligibleDonor covers the three declines a user can
// actually hit, and proves each leaves the index absent for a normal cold pass.
func TestSeedDeclinesWithoutAnEligibleDonor(t *testing.T) {
	ctx := context.Background()
	main := newRepository(t)
	write(t, filepath.Join(main, "shared", "shared.go"), "package shared\n\nfunc Shared() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-m", "committed state")

	t.Run("no sibling worktree", func(t *testing.T) {
		project := discover(t, ctx, main)
		_ = os.RemoveAll(filepath.Join(main, ".grafo"))
		result, err := Seed(ctx, project, Options{TryLock: alwaysLock})
		if err != nil {
			t.Fatal(err)
		}
		if result.Seeded || !strings.Contains(result.Reason, "no sibling worktree") {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("sibling holds no index", func(t *testing.T) {
		worktree := addWorktree(t, main, "no-donor-index")
		project := discover(t, ctx, worktree)
		result, err := Seed(ctx, project, Options{TryLock: alwaysLock})
		if err != nil {
			t.Fatal(err)
		}
		if result.Seeded || result.Considered != 0 {
			t.Fatalf("result = %#v", result)
		}
		if _, err := os.Stat(project.IndexPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a declined seed left an index behind: %v", err)
		}
	})

	t.Run("donor index is being written", func(t *testing.T) {
		indexProject(t, ctx, main, nil)
		worktree := addWorktree(t, main, "locked-donor")
		project := discover(t, ctx, worktree)
		result, err := Seed(ctx, project, Options{TryLock: neverLock})
		if err != nil {
			t.Fatal(err)
		}
		if result.Seeded || result.Considered == 0 {
			t.Fatalf("result = %#v", result)
		}
		if !strings.Contains(result.Reason, "locked") {
			t.Fatalf("reason = %q", result.Reason)
		}
		if _, err := os.Stat(project.IndexPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a declined seed left an index behind: %v", err)
		}
	})
}

// TestSeedRejectsAnIndexOfAnotherRepository proves the identity guard, which is
// what keeps a graph whose node identifiers hash a different repository from
// being adopted as this one's.
func TestSeedRejectsAnIndexOfAnotherRepository(t *testing.T) {
	ctx := context.Background()
	main := newRepository(t)
	write(t, filepath.Join(main, "shared", "shared.go"), "package shared\n\nfunc Shared() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-m", "committed state")
	indexProject(t, ctx, main, nil)
	runGit(t, main, "config", "remote.origin.url", "https://example.com/a-different-repository.git")

	worktree := addWorktree(t, main, "foreign-identity")
	project := discover(t, ctx, worktree)
	result, err := Seed(ctx, project, Options{TryLock: alwaysLock})
	if err != nil {
		t.Fatal(err)
	}
	if result.Seeded || result.Considered != 0 {
		t.Fatalf("adopted an index whose repository identity no longer matches: %#v", result)
	}
}

// TestRankPrefersTheDonorThatShareTheMostContent pins the ordering rule without
// depending on a fixture repository's history. A donor at this commit wins, a
// measured donor beats an unmeasurable one, and ties stay deterministic.
func TestRankPrefersTheDonorThatShareTheMostContent(t *testing.T) {
	project := indexer.Project{Root: "/repo", Commit: "head"}
	candidates := []candidate{
		{indexPath: "/a.sqlite", commit: "diverged-far", indexedAt: "2026-10-02T04:00:00Z"},
		{indexPath: "/b.sqlite", commit: "unknown", indexedAt: "2026-10-02T03:00:00Z"},
		{indexPath: "/c.sqlite", commit: "diverged-near", indexedAt: "2026-10-02T02:00:00Z"},
		{indexPath: "/d.sqlite", commit: "head", indexedAt: "2026-10-02T01:00:00Z"},
	}
	git := &scriptedGit{diffs: map[string]int{"diverged-far": 200, "diverged-near": 3}}
	ranked := rank(context.Background(), git, project, candidates)
	order := make([]string, 0, len(ranked))
	for _, item := range ranked {
		order = append(order, item.indexPath)
	}
	want := []string{"/d.sqlite", "/c.sqlite", "/a.sqlite", "/b.sqlite"}
	if !slices.Equal(order, want) {
		t.Fatalf("ranked order = %q, want %q", order, want)
	}
	if ranked[0].changedPaths != 0 || ranked[1].changedPaths != 3 {
		t.Fatalf("ranked divergence = %#v", ranked[:2])
	}
	if git.diffCalls != 3 {
		t.Fatalf("diff calls = %d; a donor at this commit must not be measured", git.diffCalls)
	}
}

// TestRankBoundsTheNumberOfMeasuredCandidates keeps discovery cost independent
// of how many worktrees an agent workflow has left behind.
func TestRankBoundsTheNumberOfMeasuredCandidates(t *testing.T) {
	project := indexer.Project{Root: "/repo", Commit: "head"}
	diffs := map[string]int{}
	candidates := make([]candidate, 0, rankedCandidateLimit+3)
	for index := range rankedCandidateLimit + 3 {
		commit := "diverged-" + string(rune('a'+index))
		diffs[commit] = index + 1
		candidates = append(candidates, candidate{
			indexPath: "/" + commit + ".sqlite", commit: commit,
			indexedAt: "2026-10-0" + string(rune('1'+index)) + "T00:00:00Z",
		})
	}
	git := &scriptedGit{diffs: diffs}
	ranked := rank(context.Background(), git, project, candidates)
	if len(ranked) != len(candidates) {
		t.Fatalf("ranked %d of %d candidates", len(ranked), len(candidates))
	}
	if git.diffCalls != rankedCandidateLimit {
		t.Fatalf("diff calls = %d, want the %d-candidate bound", git.diffCalls, rankedCandidateLimit)
	}
}

func alwaysLock(string) (Unlock, bool, error) { return func() error { return nil }, true, nil }

func neverLock(string) (Unlock, bool, error) { return nil, false, nil }

// scriptedGit answers worktree listing and divergence from a fixed table so
// ranking is asserted on exact command behaviour rather than a real history.
type scriptedGit struct {
	diffs     map[string]int
	diffCalls int
}

func (g *scriptedGit) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	if len(arguments) > 0 && arguments[0] == "diff" {
		g.diffCalls++
		from := arguments[3]
		count, known := g.diffs[from]
		if !known {
			return nil, errors.New("unknown revision")
		}
		return []byte(strings.Repeat("path\x00", count)), nil
	}
	return nil, errors.New("unexpected command")
}

func newRepository(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(testtemp.Dir(t))
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "fixture@example.com")
	runGit(t, root, "config", "user.name", "Fixture")
	// Worktrees of one repository share an index only because they share a
	// repository identity, and identity comes from the origin remote. Without
	// one it falls back to each worktree's own path.
	runGit(t, root, "config", "remote.origin.url", "https://example.com/fixture.git")
	write(t, filepath.Join(root, "go.mod"), "module example.com/fixture\n\ngo 1.26\n")
	return root
}

func addWorktree(t *testing.T, main, branch string) string {
	t.Helper()
	path := filepath.Join(main, ".worktrees", branch)
	runGit(t, main, "worktree", "add", "-b", branch, path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func discover(t *testing.T, ctx context.Context, root string) indexer.Project {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func indexPathOf(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	return discover(t, ctx, root).IndexPath
}

func indexProject(t *testing.T, ctx context.Context, root string, seed *indexer.SeedProvenance) indexer.Report {
	t.Helper()
	project := discover(t, ctx, root)
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	report, err := indexer.NewService(repository, parserdefaults.NewRegistry()).
		Run(ctx, project, indexer.Options{Seed: seed})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func metaValue(t *testing.T, ctx context.Context, indexPath, key string) string {
	t.Helper()
	repository, err := sqlite.OpenReadOnly(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	value, err := repository.Meta(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func assertMatchTotal(t *testing.T, ctx context.Context, indexPath, selector string, want int) {
	t.Helper()
	repository, err := sqlite.OpenReadOnly(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	group, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: selector})
	if err != nil {
		t.Fatal(err)
	}
	if group.Total != want {
		t.Fatalf("matches for %q = %d, want %d", selector, group.Total, want)
	}
}

func runGit(t testing.TB, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
