package source_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/query"
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
	defer func() { _ = repository.Close() }()
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
	defer func() { _ = repository.Close() }()
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

// TestReadRefusesAmbiguousSelector pins the reason resolution soundness matters
// here: Read resolves a selector before opening a file, so a selector that
// silently resolved to one of several exact matches returned confident source for
// the wrong symbol.
func TestReadRefusesAmbiguousSelector(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	content := "package sample\n\nfunc Charge() {}\n"
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	for _, owner := range []string{"a.go", "b.go"} {
		node := graph.Node{ID: "charge-" + owner, Kind: graph.KindFunction, Name: "Charge",
			QualifiedName: "sample." + owner + ".Charge", OwnerFile: owner,
			Location: graph.Location{Path: owner, Line: 3, Column: 1, EndLine: 3}}
		if err := repository.ReplaceOwner(ctx, owner, graph.ParseResult{Nodes: []graph.Node{node}}); err != nil {
			t.Fatal(err)
		}
	}
	service := sourcecontext.NewService(repository, sourcecontext.NewSingleProjectLocator(repository,
		indexer.Project{Root: root, Name: "sample", Branch: "main"}))
	excerpt, err := service.Read(ctx, "Charge", 0, 10)
	var ambiguous *query.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected *AmbiguousError, got excerpt %#v and error %v", excerpt, err)
	}
	if ambiguous.Total != 2 || len(ambiguous.Candidates) != 2 {
		t.Fatalf("expected both exact matches, got total %d with %d candidates",
			ambiguous.Total, len(ambiguous.Candidates))
	}
	if excerpt.Content != "" {
		t.Fatalf("an ambiguous selector must not return source: %#v", excerpt)
	}
}

// TestReadKindNarrowsResolution proves the optional kind filter reaches the
// bounded reader, so a caller can ask for the function rather than the field of
// the same name instead of failing on ambiguity.
func TestReadKindNarrowsResolution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	content := "package sample\n\ntype Request struct {\n\tCharge int\n}\n\nfunc Charge() {}\n"
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	field := graph.Node{ID: "field-charge", Kind: graph.KindField, Name: "Charge",
		QualifiedName: "sample.Request.Charge", OwnerFile: "sample.go",
		Location: graph.Location{Path: "sample.go", Line: 4, Column: 2, EndLine: 4}}
	function := graph.Node{ID: "func-charge", Kind: graph.KindFunction, Name: "Charge",
		QualifiedName: "sample.Charge", OwnerFile: "sample.go",
		Location: graph.Location{Path: "sample.go", Line: 7, Column: 1, EndLine: 7}}
	if err := repository.ReplaceOwner(ctx, "sample.go", graph.ParseResult{Nodes: []graph.Node{field, function}}); err != nil {
		t.Fatal(err)
	}
	service := sourcecontext.NewService(repository, sourcecontext.NewSingleProjectLocator(repository,
		indexer.Project{Root: root, Name: "sample", Branch: "main"}))
	if _, err := service.Read(ctx, "Charge", 0, 10); err == nil {
		t.Fatal("expected an unfiltered selector to stay ambiguous")
	}
	excerpt, err := service.ReadKind(ctx, "Charge", graph.KindFunction, 0, 10)
	if err != nil {
		t.Fatalf("kind-filtered read failed: %v", err)
	}
	if excerpt.Node.ID != function.ID || excerpt.Content != "func Charge() {}" {
		t.Fatalf("unexpected excerpt: %#v", excerpt)
	}
}
