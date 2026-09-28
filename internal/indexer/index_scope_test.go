package indexer_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
	configPath := filepath.Join(root, "grafo.yaml")
	if err := os.WriteFile(configPath, []byte("index:\n  exclude: [internal/eval/testdata/**]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(configPath, []byte("index:\n  include: ['**']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	broad, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if broad.ScopedOut != 0 || !reflect.DeepEqual(broad.Updated, []string{"grafo.yaml", "internal/eval/testdata/fixture.py"}) {
		t.Fatalf("broad report = %#v", broad)
	}
	if err := os.WriteFile(configPath, []byte("index:\n  include: [app/**]\n"), 0o644); err != nil {
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
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
