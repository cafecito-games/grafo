package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/service"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// gitCommand runs one git command against a fixture repository. Freshness is
// only provable for a Git-managed worktree, so these fixtures cannot reuse the
// package's plain temporary-directory repository.
func gitCommand(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func writeFixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// indexedGitRepository is indexedRepository's Git-managed sibling: the same
// sources, committed, so a freshness probe can prove the index current.
func indexedGitRepository(t *testing.T) string {
	t.Helper()
	root := testtemp.Dir(t)
	gitCommand(t, root, "init", "-q", "-b", "main")
	writeFixtureFile(t, root, "go.mod", "module sample\n\ngo 1.26\n")
	writeFixtureFile(t, root, "charge.go", "package sample\n\n// Charge settles a payment.\nfunc Charge() error { return nil }\n")
	writeFixtureFile(t, root, "checkout.go", "package sample\n\nfunc Checkout() error {\n\treturn Charge()\n}\n")
	gitCommand(t, root, "add", ".")
	gitCommand(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-q", "-m", "initial")
	if code := run(t, "index", root); code != 0 {
		t.Fatalf("index exited with %d", code)
	}
	return root
}

func discoveredProject(t *testing.T, root string) indexer.Project {
	t.Helper()
	project, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// storedFreshnessToken reads the token an index claims through a query-only
// handle, which is the same evidence the gate consults.
func storedFreshnessToken(t *testing.T, indexPath string) string {
	t.Helper()
	ctx := context.Background()
	repository, err := sqlite.OpenReadOnly(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	token, err := repository.Meta(ctx, indexer.FreshnessTokenMeta)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func setIndexMeta(t *testing.T, indexPath, key, value string) {
	t.Helper()
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, key, value); err != nil {
		t.Fatal(err)
	}
}

// TestFreshnessGateServesAFreshIndexWhileTheLockIsHeld is the contract in one
// pass. Holding the branch index lock makes the refreshing path impossible, so
// a query that still answers can only have been served read-only from an index
// the stored token proved current.
func TestFreshnessGateServesAFreshIndexWhileTheLockIsHeld(t *testing.T) {
	root := indexedGitRepository(t)
	project := discoveredProject(t, root)
	// Without a stored token the gate could never engage, and this test would
	// pass for the wrong reason.
	if storedFreshnessToken(t, project.IndexPath) == "" {
		t.Fatal("grafo index stored no freshness token; the fast path cannot be exercised")
	}
	// A short budget keeps a regression a fast failure instead of a two-minute
	// wait on the lock this test deliberately holds.
	shortIndexLockWait(t, 50*time.Millisecond)

	unlock, err := service.IndexLock(project.IndexPath, 0)
	if err != nil {
		t.Fatalf("hold the index lock: %v", err)
	}
	defer func() { _ = unlock() }()

	stdout, stderr, code := output(t, "find", "Charge", "--repo", root)
	if code != 0 {
		t.Fatalf("find exited with %d while the index lock was held\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Charge") {
		t.Fatalf("find did not name the symbol\nstdout: %s", stdout)
	}
}

// TestFreshnessGateRefreshesAfterAnEdit pins the other half of the gate: a tree
// the token no longer describes must still be reconciled before it answers.
func TestFreshnessGateRefreshesAfterAnEdit(t *testing.T) {
	t.Parallel()
	root := indexedGitRepository(t)
	writeFixtureFile(t, root, "refund.go", "package sample\n\nfunc Refund() error { return nil }\n")

	stdout, stderr, code := output(t, "find", "Refund", "--repo", root)
	if code != 0 {
		t.Fatalf("find exited with %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Refund") {
		t.Fatalf("find missed the edit made after indexing\nstdout: %s", stdout)
	}
}

// TestFreshnessGateAlwaysRefreshesANonGitProject keeps the conservatism the MCP
// path applies: without Git there is no snapshot to prove anything about, so
// every query reconciles exactly as it did before the gate existed. Asserting
// that an edit is found would not discriminate - an invalidated token produces
// the same answer - so this holds the lock and requires the query to fail,
// which only the refreshing path can do.
func TestFreshnessGateAlwaysRefreshesANonGitProject(t *testing.T) {
	root := indexedRepository(t)
	project := discoveredProject(t, root)
	if project.GitManaged {
		t.Fatalf("fixture at %s is Git-managed; the non-Git branch is not exercised", root)
	}
	shortIndexLockWait(t, 50*time.Millisecond)

	unlock, err := service.IndexLock(project.IndexPath, 0)
	if err != nil {
		t.Fatalf("hold the index lock: %v", err)
	}
	defer func() { _ = unlock() }()

	stdout, stderr, code := output(t, "find", "Charge", "--repo", root)
	if code == 0 {
		t.Fatalf("find answered a non-Git project without refreshing it\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "another grafo process is refreshing this branch index") {
		t.Fatalf("contended non-Git find stderr = %q, want the refresh-contention message", stderr)
	}
}

// TestFreshnessGateRefusesAnIndexBuiltForAnotherRepository pins that a matching
// token is not on its own licence to answer: the index must also claim this
// repository's identity. "repository identity changed" is an error Run raises,
// and the fast path never reaches Run, so the invariant has to be checked where
// the gate decides. Today the token digest folds the project id in as well, but
// that is an encoder detail; were it ever dropped as derivable, an identity
// mismatch would quietly become a wrong answer with nothing else failing.
func TestFreshnessGateRefusesAnIndexBuiltForAnotherRepository(t *testing.T) {
	root := indexedGitRepository(t)
	project := discoveredProject(t, root)
	other := discoveredProject(t, indexedGitRepository(t))
	if other.ID == project.ID {
		t.Fatalf("the two fixtures share the repository id %q", project.ID)
	}
	setIndexMeta(t, project.IndexPath, "repository_id", other.ID)
	if storedFreshnessToken(t, project.IndexPath) == "" {
		t.Fatal("the fixture lost its freshness token; the gate would refuse for the wrong reason")
	}
	shortIndexLockWait(t, 50*time.Millisecond)

	unlock, err := service.IndexLock(project.IndexPath, 0)
	if err != nil {
		t.Fatalf("hold the index lock: %v", err)
	}
	defer func() { _ = unlock() }()

	stdout, stderr, code := output(t, "find", "Charge", "--repo", root)
	if code == 0 {
		t.Fatalf("find answered from an index belonging to another repository\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "another grafo process is refreshing this branch index") {
		t.Fatalf("contended find stderr = %q, want the refresh-contention message", stderr)
	}
}

// TestFreshnessGateRefreshesWithoutAStoredToken fixes the meaning of an empty
// token: no proof, never freshness. The query must take the refreshing path,
// which re-proves the index, so a token is stored again afterwards.
func TestFreshnessGateRefreshesWithoutAStoredToken(t *testing.T) {
	t.Parallel()
	root := indexedGitRepository(t)
	project := discoveredProject(t, root)
	setIndexMeta(t, project.IndexPath, indexer.FreshnessTokenMeta, "")

	stdout, stderr, code := output(t, "find", "Charge", "--repo", root)
	if code != 0 {
		t.Fatalf("find exited with %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Charge") {
		t.Fatalf("find did not name the symbol\nstdout: %s", stdout)
	}
	stored := storedFreshnessToken(t, project.IndexPath)
	if stored == "" {
		t.Fatal("the query answered without re-proving the index it refreshed")
	}
	probe, err := indexer.ProbeFreshness(context.Background(), root, parserdefaults.NewRegistry(), indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored != probe.Token.String() {
		t.Fatalf("stored token = %q, want the current probe's %q", stored, probe.Token.String())
	}
}
