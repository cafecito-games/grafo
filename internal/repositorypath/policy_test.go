package repositorypath_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/repositorypath"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestResolveRegularFileRejectsSymlinkedAncestorsAndEscapes(t *testing.T) {
	root := testtemp.Dir(t)
	outside := testtemp.Dir(t)
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

// TestIgnoredClassifiesEverySegment covers the membership boundary itself: an
// ignored directory anywhere in the path excludes it, an ignored file name
// excludes it case-insensitively, and nothing else does.
func TestIgnoredClassifiesEverySegment(t *testing.T) {
	for _, testCase := range []struct {
		path string
		want bool
	}{
		{path: "", want: false},
		{path: "main.go", want: false},
		{path: "client/scripts/player.gd", want: false},
		{path: "node_modules", want: true},
		{path: "node_modules/left-pad/index.js", want: true},
		{path: "apps/server/vendor/pkg/file.go", want: true},
		{path: "client/.godot/imported/thing.res", want: true},
		{path: "deep/nested/path/.git/config", want: true},
		{path: "package-lock.json", want: true},
		{path: "apps/website/PACKAGE-LOCK.JSON", want: true},
		{path: "apps/website/package-lock.json.bak", want: false},
		{path: "buildings/tower.gd", want: false},
		{path: "a//b/main.go", want: false},
		{path: filepath.Join("apps", "server", "node_modules", "dep", "index.js"), want: true},
	} {
		if got := repositorypath.Ignored(testCase.path); got != testCase.want {
			t.Errorf("Ignored(%q) = %t, want %t", testCase.path, got, testCase.want)
		}
	}
}

// TestIgnoredDoesNotAllocate pins the property that made it worth rewriting.
// Repository-wide scanners call Ignored once per candidate path for every file
// they key, so an allocation per call is an allocation per path per file.
func TestIgnoredDoesNotAllocate(t *testing.T) {
	allocations := testing.AllocsPerRun(100, func() {
		if repositorypath.Ignored("client/scripts/units/player_controller.gd") {
			t.Fatal("a plain source path was reported as ignored")
		}
	})
	if allocations != 0 {
		t.Fatalf("Ignored allocated %.1f times per call, want 0", allocations)
	}
}
