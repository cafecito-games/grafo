package indexer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	pythonparser "github.com/cafecito-games/grafo/internal/parser/python"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestServiceScopeNarrowingAndBroadeningConvergeWithoutChangingHead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, relative := range []string{"app/main.py", "internal/eval/testdata/fixture.py"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, relative), []byte("def value():\n    return True\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "internal/eval/testdata/notes.txt"), []byte("not a supported source file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "grafo.yaml")
	config := func(index string) []byte {
		return []byte("components:\n  - name: app\n    roots: [app]\n  - name: fixture\n    roots: [internal/eval/testdata]\n" + index)
	}
	if err := os.WriteFile(configPath, config("index:\n  exclude: [internal/eval/testdata/**]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("grafo.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Grafo Test")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "fixture")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(pythonparser.New(), configparser.New()))

	narrow, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if narrow.ScopedOut != 1 || !reflect.DeepEqual(narrow.Updated, []string{"app/main.py", "grafo.yaml"}) {
		t.Fatalf("narrow report = %#v", narrow)
	}
	fixtureComponent := componentNode(t, ctx, repository, project.Name+"/fixture")
	assertOutgoingQualifiedSet(t, ctx, repository, fixtureComponent.ID, graph.EdgeContains, nil)
	if err := os.WriteFile(configPath, config("index:\n  include: ['**']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previousScopeDigest, err := repository.Meta(ctx, "index_scope_digest")
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after durable file write")
	if _, err := service.Run(ctx, project, indexer.Options{Boundary: func(boundary indexer.Boundary) error {
		if boundary.Kind == indexer.BoundaryFilePersisted {
			return stop
		}
		return nil
	}}); !errors.Is(err, stop) {
		t.Fatalf("interrupted scope change error = %v", err)
	}
	interruptedScopeDigest, err := repository.Meta(ctx, "index_scope_digest")
	if err != nil {
		t.Fatal(err)
	}
	if interruptedScopeDigest != previousScopeDigest {
		t.Fatalf("interrupted run published scope digest %q, want %q", interruptedScopeDigest, previousScopeDigest)
	}
	broad, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if broad.ScopedOut != 0 || !reflect.DeepEqual(broad.Updated, []string{"internal/eval/testdata/fixture.py"}) {
		t.Fatalf("broad report = %#v", broad)
	}
	assertOutgoingQualifiedSet(t, ctx, repository, fixtureComponent.ID, graph.EdgeContains,
		[]string{"internal/eval/testdata/fixture.py"})
	if err := os.WriteFile(configPath, config("index:\n  include: [app/**]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restricted, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if restricted.ScopedOut != 1 || !reflect.DeepEqual(restricted.Removed, []string{"internal/eval/testdata/fixture.py"}) {
		t.Fatalf("restricted report = %#v", restricted)
	}
	if !containsString(restricted.Updated, "grafo.yaml") {
		t.Fatalf("control file was not retained: %#v", restricted)
	}
	assertOutgoingQualifiedSet(t, ctx, repository, fixtureComponent.ID, graph.EdgeContains, nil)
	repeated, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Updated) != 0 || len(repeated.Removed) != 0 || repeated.Unchanged != 2 || repeated.ScopedOut != 1 {
		t.Fatalf("unchanged scoped run = %#v", repeated)
	}
	validFiles, err := repository.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	validScopeDigest, err := repository.Meta(ctx, "index_scope_digest")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config("index:\n  include: [../outside/**]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{}); err == nil {
		t.Fatal("invalid scope unexpectedly indexed")
	}
	afterFiles, err := repository.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterScopeDigest, err := repository.Meta(ctx, "index_scope_digest")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterFiles, validFiles) || afterScopeDigest != validScopeDigest {
		t.Fatalf("invalid scope mutated durable state: files=%#v digest=%q", afterFiles, afterScopeDigest)
	}
}

func TestServiceInterruptedRemovalThenScopeRevertRediscoversMissingFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, relative := range []string{"app/main.py", "fixtures/sample.py"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, relative), []byte("def value():\n    return True\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "grafo.yaml")
	writeScope := func(scope string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte("index:\n  include: ["+scope+"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeScope("'**'")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("grafo.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Grafo Test")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "fixture")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(pythonparser.New(), configparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	writeScope("app/**")
	stop := errors.New("stop after removals")
	if _, err := service.Run(ctx, project, indexer.Options{Boundary: func(boundary indexer.Boundary) error {
		if boundary.Kind == indexer.BoundaryFilesRemoved {
			return stop
		}
		return nil
	}}); !errors.Is(err, stop) {
		t.Fatalf("interrupted narrowing error = %v", err)
	}
	files, err := repository.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := files["fixtures/sample.py"]; exists {
		t.Fatal("interrupted narrowing did not reach durable removal")
	}
	if pending, err := repository.Meta(ctx, "index_scope_pending"); err != nil || pending == "" {
		t.Fatalf("pending scope marker = %q, %v", pending, err)
	}

	writeScope("'**'")
	recovered, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(recovered.Updated, "fixtures/sample.py") {
		t.Fatalf("scope revert reused incomplete membership: %#v", recovered)
	}
	files, err = repository.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := files["fixtures/sample.py"]; !exists {
		t.Fatal("scope revert did not restore removed file")
	}
	if pending, err := repository.Meta(ctx, "index_scope_pending"); err != nil || pending != "" {
		t.Fatalf("successful retry retained pending scope marker = %q, %v", pending, err)
	}

	writeScope("app/**")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	incrementalFiles, err := repository.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	incrementalCounts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if _, err := indexer.NewService(fresh, parserapi.NewRegistry(pythonparser.New(), configparser.New())).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	freshFiles, err := fresh.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	freshCounts, err := fresh.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(filePaths(incrementalFiles), filePaths(freshFiles)) || !reflect.DeepEqual(incrementalCounts, freshCounts) {
		t.Fatalf("incremental narrowing differs from fresh: files %v != %v, counts %#v != %#v",
			filePaths(incrementalFiles), filePaths(freshFiles), incrementalCounts, freshCounts)
	}
}

func TestServiceScopeIncludeCannotReenableUnsafeOrUnsupportedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, relative := range []string{"safe.py", "vendor/hidden.py", "node_modules/hidden.py"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, relative), []byte("def value():\n    return True\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "unsupported.txt"), []byte("not source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oversized.py"), []byte(strings.Repeat("#", 256)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("safe.py", filepath.Join(root, "linked.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "grafo.yaml"), []byte("index:\n  include: ['**']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Grafo Test")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "fixture")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	report, err := indexer.NewService(repository, parserapi.NewRegistry(pythonparser.New(), configparser.New())).Run(ctx, project, indexer.Options{MaxFileSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	files, err := repository.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filePaths(files), []string{"grafo.yaml", "safe.py"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("include re-enabled unsafe or unsupported paths: got %v want %v", got, want)
	}
	if got, want := report.Skipped, []string{"linked.py", "oversized.py"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unsafe/oversized skips = %v, want %v", got, want)
	}
}

func filePaths(files map[string]graph.FileRecord) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
