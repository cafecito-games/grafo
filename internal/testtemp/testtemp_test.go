package testtemp_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// unresolvedTemporaryDirectory matches a direct testing.TB.TempDir call, whose
// result still contains the operating system's symlinked ancestors.
var unresolvedTemporaryDirectory = regexp.MustCompile(`(^|[^\w.])(tb|t|b)\.TempDir\(\)`)

func TestDirResolvesEverySymlinkedAncestor(t *testing.T) {
	directory := testtemp.Dir(t)
	if !filepath.IsAbs(directory) {
		t.Fatalf("directory = %q, want an absolute path", directory)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != directory {
		t.Fatalf("directory = %q, want the resolved %q", directory, resolved)
	}
	if other := testtemp.Dir(t); other == directory {
		t.Fatalf("two calls returned the same directory %q", directory)
	}
}

// TestNoTestBypassesDir keeps every fixture on testtemp.Dir. A direct TempDir
// call passes on Linux and fails on macOS, where the temporary directory sits
// under the /var symlink, so only a repository-wide check catches the mistake
// before it reaches a developer's machine.
func TestNoTestBypassesDir(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	walkError := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (name == "testtemp" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		contents, readError := os.ReadFile(path)
		if readError != nil {
			return readError
		}
		for number, line := range strings.Split(string(contents), "\n") {
			if unresolvedTemporaryDirectory.MatchString(line) {
				relative, relativeError := filepath.Rel(root, path)
				if relativeError != nil {
					relative = path
				}
				offenders = append(offenders, filepath.ToSlash(relative)+":"+strconv.Itoa(number+1))
			}
		}
		return nil
	})
	if walkError != nil {
		t.Fatal(walkError)
	}
	if len(offenders) != 0 {
		t.Fatalf("tests call TempDir directly instead of testtemp.Dir:\n\t%s", strings.Join(offenders, "\n\t"))
	}
}
