package indexer_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
)

func TestDiscoverProjectUsesSeparateIndexPerBranch(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	mainProject, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "checkout", "-b", "feature/semantic-graph")
	featureProject, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if mainProject.Branch != "main" || featureProject.Branch != "feature/semantic-graph" {
		t.Fatalf("unexpected branches: %q and %q", mainProject.Branch, featureProject.Branch)
	}
	if mainProject.IndexPath == featureProject.IndexPath {
		t.Fatalf("branches share an index path: %s", mainProject.IndexPath)
	}
	if mainProject.ID != featureProject.ID {
		t.Fatalf("repository identity should survive branch changes")
	}
}

func TestDiscoverProjectPreservesDetachedHeadAndRejectsRemoteIdentityChange(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	runGit(t, root, "remote", "add", "origin", "git@example.com:team/first.git")
	first, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "remote", "set-url", "origin", "git@example.com:team/longer-second.git")
	changed, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == changed.ID {
		t.Fatal("remote identity change was hidden by the discovery cache")
	}

	runGit(t, root, "checkout", "--detach")
	detached, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(detached.Branch, "detached-") || detached.IndexPath == changed.IndexPath {
		t.Fatalf("detached project identity was not isolated: %#v", detached)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
