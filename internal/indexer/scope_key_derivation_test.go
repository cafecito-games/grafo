package indexer_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/parser/golang"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// scopeKeyCorpus writes a Git corpus of two Go packages holding several files
// each. Package scope is a property of the directory, so a package of several
// files is what makes per-file derivation visible as a multiple of the file
// count rather than as a single extra call.
func scopeKeyCorpus(t testing.TB) (root string, goFiles, packageDirectories int) {
	t.Helper()
	root = testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/scope\n\ngo 1.26\n")
	areas := []string{"store", "app"}
	const filesPerArea = 4
	for _, area := range areas {
		for index := range filesPerArea {
			write(t, filepath.Join(root, area, fmt.Sprintf("unit%d.go", index)), fmt.Sprintf(`package %s

func Use%d() string { return helper%d() }

func helper%d() string { return %q }
`, area, index, index, index, area))
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid",
		"commit", "-m", "initial")
	return root, len(areas) * filesPerArea, len(areas)
}

// TestColdIndexDerivesEachScopeKeyOncePerFile is the end-to-end guard for the
// pipeline half of the fix. The parser-side tests prove the semantic loader
// consumes the key it is handed, but only a real index proves the pipeline
// still hands it over: without that wiring the loader derives its own and every
// Go file pays the derivation twice.
//
// The bound is one derivation per Go file, which the pipeline performs, plus one
// per package directory, which the workspace load performs for the packages it
// loaded. Reintroducing the second per-file call site puts the count above the
// bound, because it scales with files rather than with directories.
//
// This test reads process-wide counters, so it cannot run in parallel with
// another that does.
func TestColdIndexDerivesEachScopeKeyOncePerFile(t *testing.T) {
	root, goFiles, packageDirectories := scopeKeyCorpus(t)
	ctx := context.Background()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	before := golang.ScopeWorkCounts()
	report, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) == 0 {
		t.Fatal("cold index updated no files, so it proved nothing about per-file work")
	}
	derivations := golang.ScopeWorkCounts().ScopeKeyComputations - before.ScopeKeyComputations

	bound := int64(goFiles + packageDirectories)
	if derivations > bound {
		t.Fatalf("scope key derivations for %d Go files in %d packages = %d, want at most %d; "+
			"a count that scales with files means the key is derived per file more than once",
			goFiles, packageDirectories, derivations, bound)
	}
	if derivations < int64(goFiles) {
		t.Fatalf("scope key derivations = %d, want at least one per Go file (%d); "+
			"fewer means a file was indexed without proving its package scope",
			derivations, goFiles)
	}
}
