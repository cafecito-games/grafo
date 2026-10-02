package golang

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

func writePackageFile(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestPackageContentDigestReusesAnUnchangedPackage covers the reuse evidence
// directly: making the sources unreadable after the first digest leaves the
// listing untouched, so a second call has to answer from what it remembered
// rather than reading the package again.
func TestPackageContentDigestReusesAnUnchangedPackage(t *testing.T) {
	root := testtemp.Dir(t)
	directory := filepath.Join(root, "service")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create package directory: %v", err)
	}
	writePackageFile(t, directory, "service.go", "package service\n\nfunc Serve() {}\n")
	writePackageFile(t, directory, "helper.go", "package service\n\nfunc help() {}\n")

	first, err := packageContentDigest(root, "service")
	if err != nil {
		t.Fatalf("digest package: %v", err)
	}

	unreadable := filepath.Join(directory, "service.go")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("make package source unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	second, err := packageContentDigest(root, "service")
	if err != nil {
		t.Fatalf("digest unchanged package: %v", err)
	}
	if second != first {
		t.Fatalf("digest of an unchanged package changed: %q then %q", first, second)
	}
}

// TestPackageContentDigestFollowsPackageChanges covers the other half: edited,
// added, and removed sources all have to change the key, because the key is what
// decides whether the package's files are reparsed.
func TestPackageContentDigestFollowsPackageChanges(t *testing.T) {
	root := testtemp.Dir(t)
	directory := filepath.Join(root, "service")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create package directory: %v", err)
	}
	writePackageFile(t, directory, "service.go", "package service\n\nfunc Serve() {}\n")

	baseline, err := packageContentDigest(root, "service")
	if err != nil {
		t.Fatalf("digest package: %v", err)
	}

	writePackageFile(t, directory, "service.go", "package service\n\nfunc Serve(port int) {}\n")
	edited, err := packageContentDigest(root, "service")
	if err != nil {
		t.Fatalf("digest edited package: %v", err)
	}
	if edited == baseline {
		t.Fatal("editing a package source did not change its scope digest")
	}

	writePackageFile(t, directory, "extra.go", "package service\n\nfunc extra() {}\n")
	added, err := packageContentDigest(root, "service")
	if err != nil {
		t.Fatalf("digest grown package: %v", err)
	}
	if added == edited {
		t.Fatal("adding a package source did not change its scope digest")
	}

	if err := os.Remove(filepath.Join(directory, "extra.go")); err != nil {
		t.Fatalf("remove package source: %v", err)
	}
	removed, err := packageContentDigest(root, "service")
	if err != nil {
		t.Fatalf("digest shrunk package: %v", err)
	}
	if removed != edited {
		t.Fatalf("removing a source did not restore the previous digest: %q want %q", removed, edited)
	}
}
