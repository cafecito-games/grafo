package indexer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
