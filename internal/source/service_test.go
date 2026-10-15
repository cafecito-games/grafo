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
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestServiceReadsBoundedSymbolSpan(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	content := "package sample\n\nfunc Before() {}\n\nfunc Checkout() {\n\tcharge()\n}\n\nfunc After() {}\n"
	if err := os.WriteFile(filepath.Join(root, "checkout.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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
	root := testtemp.Dir(t)
	outside := filepath.Join(testtemp.Dir(t), "secret.go")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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
	root := testtemp.Dir(t)
	content := "package sample\n\nfunc Charge() {}\n"
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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
	root := testtemp.Dir(t)
	content := "package sample\n\ntype Request struct {\n\tCharge int\n}\n\nfunc Charge() {}\n"
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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

func TestGeneratedSQLMethodReadsCanonicalQueryUnlessIDIsExplicit(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	if err := os.MkdirAll(filepath.Join(root, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "db", "queries.sql"), []byte("-- name: GetUser :one\nSELECT id FROM users WHERE id = ?;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "db", "queries.sql.go"), []byte("package db\n\nfunc (q *Queries) GetUser() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	method := graph.Node{ID: "n:generated", Kind: graph.KindMethod, Name: "GetUser", QualifiedName: "example/db.Queries.GetUser",
		OwnerFile: "db/queries.sql.go", Location: graph.Location{Path: "db/queries.sql.go", Line: 3, Column: 1, EndLine: 3}}
	queryNode := graph.Node{ID: "n:query", Kind: graph.KindSQLQuery, Name: "GetUser", QualifiedName: "db/queries.sql#GetUser",
		OwnerFile: "db/queries.sql", Location: graph.Location{Path: "db/queries.sql", Line: 1, Column: 10, EndLine: 2}}
	if err := repository.ReplaceOwner(ctx, "db/queries.sql", graph.ParseResult{Nodes: []graph.Node{method, queryNode}, Facts: []graph.Fact{{
		ID: "f:binding", FromID: method.ID, Kind: graph.EdgeGeneratedFrom, TargetID: queryNode.ID, OwnerFile: "db/queries.sql",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	service := sourcecontext.NewService(repository, sourcecontext.NewSingleProjectLocator(repository,
		indexer.Project{Root: root, Name: "sample", Branch: "main"}))
	canonical, err := service.Read(ctx, method.QualifiedName, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Node.ID != queryNode.ID || canonical.GeneratedNode == nil || canonical.GeneratedNode.ID != method.ID || canonical.Path != "db/queries.sql" {
		t.Fatalf("canonical excerpt = %#v", canonical)
	}
	explicit, err := service.Read(ctx, method.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Node.ID != method.ID || explicit.GeneratedNode != nil || explicit.Path != "db/queries.sql.go" {
		t.Fatalf("explicit generated excerpt = %#v", explicit)
	}
}
