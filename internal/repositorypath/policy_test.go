package repositorypath_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/repositorypath"
)

func TestResolveRegularFileRejectsSymlinkedAncestorsAndEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repositorypath.ResolveRegularFile(root, "linked/outside.txt"); err == nil {
		t.Fatal("resolved a regular leaf through a symlinked ancestor")
	}
	if _, _, err := repositorypath.ResolveRegularFile(root, "../outside.txt"); err == nil {
		t.Fatal("resolved a path escaping the repository root")
	}
	if err := os.Mkdir(filepath.Join(root, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "safe", "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repositorypath.ResolveRegularFile(root, "safe/inside.txt"); err != nil {
		t.Fatalf("safe file rejected: %v", err)
	}
}
