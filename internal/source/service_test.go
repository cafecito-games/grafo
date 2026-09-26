package source_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestServiceReadsBoundedSymbolSpan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	content := "package sample\n\nfunc Before() {}\n\nfunc Checkout() {\n\tcharge()\n}\n\nfunc After() {}\n"
	if err := os.WriteFile(filepath.Join(root, "checkout.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	node := graph.Node{ID: "checkout", Kind: graph.KindFunction, Name: "Checkout",
		QualifiedName: "sample.Checkout", OwnerFile: "checkout.go",
		Location: graph.Location{Path: "checkout.go", Line: 5, Column: 1, EndLine: 7}}
	if err := repository.ReplaceOwner(ctx, "checkout.go", graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
		t.Fatal(err)
	}
	service := sourcecontext.NewService(repository, sourcecontext.NewSingleProjectLocator(repository,
		indexer.Project{Root: root, Name: "sample", Branch: "main"}))
	excerpt, err := service.Read(ctx, node.ID, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if excerpt.StartLine != 4 || excerpt.EndLine != 6 || excerpt.Content != "\nfunc Checkout() {\n\tcharge()" || !excerpt.Truncated {
		t.Fatalf("unexpected excerpt: %#v", excerpt)
	}
}

func TestServiceRejectsSymlinkEscape(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	node := graph.Node{ID: "linked", Kind: graph.KindFile, Name: "linked.go",
		QualifiedName: "linked.go", OwnerFile: "linked.go", Location: graph.Location{Path: "linked.go", Line: 1}}
	if err := repository.ReplaceOwner(ctx, "linked.go", graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
		t.Fatal(err)
	}
	service := sourcecontext.NewService(repository, sourcecontext.NewSingleProjectLocator(repository,
		indexer.Project{Root: root, Name: "sample", Branch: "main"}))
	if _, err := service.Read(ctx, node.ID, 0, 20); err == nil {
		t.Fatal("expected source path escape to be rejected")
	}
}
