// Package testtemp provides temporary directories for tests whose ancestors
// contain no symlinks.
package testtemp

import (
	"path/filepath"
	"testing"
)

// Dir returns a temporary directory scoped to the test, like
// testing.TB.TempDir, with every symlinked ancestor resolved.
//
// macOS reports the operating system temporary directory under /var, a symlink
// to /private/var. Grafo canonicalizes repository roots, index paths, and
// installed binary paths before reporting them back, and the embedding cache
// refuses to create directories beneath a symlinked ancestor, so a fixture
// built on the raw TempDir value disagrees with the code under test. Resolving
// the directory once, here, keeps fixtures and production on one spelling of
// every path on every platform.
func Dir(t testing.TB) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return directory
}
