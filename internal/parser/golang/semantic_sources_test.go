package golang_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// vendoringRepository is a Git repository whose single module vendors a
// dependency that repository code calls, which is the exact shape in which a
// hand-patched vendored signature changes how every importer type-checks.
func vendoringRepository(t *testing.T) string {
	t.Helper()
	root := testtemp.Dir(t)
	initTestRepository(t, root)
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/service\n\ngo 1.26\n\nrequire example.com/dep v1.0.0\n")
	writeFile(t, filepath.Join(root, "vendor", "modules.txt"), "# example.com/dep v1.0.0\n## explicit; go 1.26\nexample.com/dep\n")
	writeFile(t, filepath.Join(root, "vendor", "example.com", "dep", "dep.go"),
		"package dep\n\nfunc Format(value string) string { return value }\n")
	writeFile(t, filepath.Join(root, "app", "app.go"),
		"package app\n\nimport \"example.com/dep\"\n\nfunc Run(value string) string { return dep.Format(value) }\n")
	commitTestRepository(t, root)
	return root
}

func initTestRepository(t *testing.T, root string) {
	t.Helper()
	runTestRepositoryGit(t, root, "init", "-q", "-b", "main")
}

func commitTestRepository(t *testing.T, root string) {
	t.Helper()
	runTestRepositoryGit(t, root, "add", ".")
	runTestRepositoryGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-q", "-m", "fixture")
}

func runTestRepositoryGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func workspaceKey(t *testing.T, root string) string {
	t.Helper()
	key, err := golangparser.New().WorkspaceSemanticKey(context.Background(),
		parserapi.Input{Root: root, Repository: "sample", RepoID: "repo:sample"})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestWorkspaceSemanticKeyTracksVendoredDeclarations pins the decision that
// vendored code is resolution evidence even though it is never a graph source.
// go/packages compiles vendor/ whenever a module vendors, so a hand-patched
// exported signature there changes how every importing repository file
// type-checks and extracts.
func TestWorkspaceSemanticKeyTracksVendoredDeclarations(t *testing.T) {
	const dependency = "vendor/example.com/dep/dep.go"
	for _, testCase := range []struct {
		name    string
		mutate  map[string]string
		remove  []string
		changed bool
	}{
		{
			name:    "exported signature changed",
			mutate:  map[string]string{dependency: "package dep\n\nfunc Format(value string, width int) string { return value }\n"},
			changed: true,
		},
		{
			name:    "exported declaration added",
			mutate:  map[string]string{dependency: "package dep\n\nfunc Format(value string) string { return value }\n\nfunc Trim(value string) string { return value }\n"},
			changed: true,
		},
		{
			// A vendored body cannot change any repository file, so invalidating
			// on it is conservative rather than necessary. It is accepted
			// deliberately: parsing a declaration surface out of every vendored
			// file is the most expensive thing the workspace key can do, and a
			// vendor tree changes through `go mod vendor`, which rewrites
			// vendor/modules.txt anyway. See semanticWorkspaceKey.
			name:    "vendored body rewritten",
			mutate:  map[string]string{dependency: "package dep\n\nfunc Format(value string) string { return value + \"\" }\n"},
			changed: true,
		},
		{
			name:    "vendored package added",
			mutate:  map[string]string{"vendor/example.com/other/other.go": "package other\n\nfunc Help() {}\n"},
			changed: true,
		},
		{
			name:    "vendored source removed",
			remove:  []string{dependency},
			changed: true,
		},
		{
			name:    "vendor manifest changed without a source change",
			mutate:  map[string]string{"vendor/modules.txt": "# example.com/dep v1.1.0\n## explicit; go 1.26\nexample.com/dep\n"},
			changed: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := vendoringRepository(t)
			before := workspaceKey(t, root)
			for path, content := range testCase.mutate {
				writeFile(t, filepath.Join(root, filepath.FromSlash(path)), content)
			}
			for _, path := range testCase.remove {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil {
					t.Fatal(err)
				}
			}
			after := workspaceKey(t, root)
			if changed := before != after; changed != testCase.changed {
				t.Fatalf("workspace key changed = %t, want %t", changed, testCase.changed)
			}
		})
	}
}

// TestWorkspaceSemanticKeyIgnoresAbsentVendorTree proves a repository without a
// vendor directory is unaffected by the widened input set: only the files the
// toolchain compiles are read, and there are none to add.
func TestWorkspaceSemanticKeyIgnoresAbsentVendorTree(t *testing.T) {
	root := testtemp.Dir(t)
	initTestRepository(t, root)
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/service\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "app", "app.go"), "package app\n\nfunc Run() int { return 1 }\n")
	commitTestRepository(t, root)

	before := workspaceKey(t, root)
	if again := workspaceKey(t, root); before != again {
		t.Fatal("workspace key is not stable for an unchanged repository without a vendor tree")
	}
	writeFile(t, filepath.Join(root, "app", "app.go"), "package app\n\nfunc Run() int { return 2 }\n")
	if after := workspaceKey(t, root); after != before {
		t.Fatal("a body-only edit changed the workspace key")
	}
	writeFile(t, filepath.Join(root, "app", "app.go"), "package app\n\nfunc Run() int64 { return 1 }\n")
	if after := workspaceKey(t, root); after == before {
		t.Fatal("a declaration edit left the workspace key unchanged")
	}
}

// TestWorkspaceSemanticKeyTracksGitIgnoredSiblings pins the other half of the
// decision: go/packages reads every .go file in a package directory regardless
// of Git status, so a git-ignored or generated-but-uncommitted sibling declares
// symbols another package resolves through and must enter the key.
func TestWorkspaceSemanticKeyTracksGitIgnoredSiblings(t *testing.T) {
	root := testtemp.Dir(t)
	initTestRepository(t, root)
	writeFile(t, filepath.Join(root, ".gitignore"), "*_generated.go\n")
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/service\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "model", "model.go"), "package model\n\nfunc Name() string { return \"model\" }\n")
	writeFile(t, filepath.Join(root, "app", "app.go"), "package app\n\nimport \"example.com/service/model\"\n\nfunc Run() string { return model.Name() }\n")
	commitTestRepository(t, root)

	generated := filepath.Join(root, "model", "schema_generated.go")
	baseline := workspaceKey(t, root)
	assertGitIgnored(t, root, "model/schema_generated.go")

	writeFile(t, generated, "package model\n\ntype Record struct{ ID string }\n")
	added := workspaceKey(t, root)
	if added == baseline {
		t.Fatal("a git-ignored declaration file left the workspace key unchanged")
	}

	writeFile(t, generated, "package model\n\ntype Record struct{ ID string }\n\ntype Second struct{}\n")
	edited := workspaceKey(t, root)
	if edited == added {
		t.Fatal("editing a git-ignored declaration file left the workspace key unchanged")
	}

	if err := os.Remove(generated); err != nil {
		t.Fatal(err)
	}
	if removed := workspaceKey(t, root); removed != baseline {
		t.Fatal("removing a git-ignored declaration file did not restore the workspace key")
	}
}

// TestWorkspaceSemanticKeyIgnoresGitIgnoredBodies keeps the widened input set
// from becoming conservative again: an ignored sibling contributes its
// declaration surface, not its statements.
func TestWorkspaceSemanticKeyIgnoresGitIgnoredBodies(t *testing.T) {
	root := testtemp.Dir(t)
	initTestRepository(t, root)
	writeFile(t, filepath.Join(root, ".gitignore"), "*_generated.go\n")
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/service\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "model", "model.go"), "package model\n\nfunc Name() string { return \"model\" }\n")
	generated := filepath.Join(root, "model", "schema_generated.go")
	writeFile(t, generated, "package model\n\nfunc Build() string { return \"a\" }\n")
	commitTestRepository(t, root)
	assertGitIgnored(t, root, "model/schema_generated.go")

	before := workspaceKey(t, root)
	writeFile(t, generated, "package model\n\nfunc Build() string { return \"b\" }\n")
	if after := workspaceKey(t, root); after != before {
		t.Fatal("a body-only edit to a git-ignored sibling changed the workspace key")
	}
}

func assertGitIgnored(t *testing.T, root, path string) {
	t.Helper()
	command := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "--", path)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	if len(output) != 0 {
		t.Fatalf("fixture path %q is visible to Git membership: %q", path, output)
	}
}
