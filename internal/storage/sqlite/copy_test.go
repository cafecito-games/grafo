package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestCopyIndexReproducesGraphAndMetadataFromALiveSource(t *testing.T) {
	ctx := context.Background()
	directory := testtemp.Dir(t)
	source := filepath.Join(directory, "source.sqlite")
	repository, err := Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "root", "/donor/checkout"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "owner.go", graph.ParseResult{Nodes: []graph.Node{{
		ID: "n:copied", Kind: graph.KindFunction, Name: "Copied", QualifiedName: "pkg.Copied",
		OwnerFile: "owner.go",
	}}}); err != nil {
		t.Fatal(err)
	}

	// The source stays open so the copy is proven against a database holding a
	// live writable connection and a WAL, which is the only state a donor index
	// is ever in when seeding runs.
	destination := filepath.Join(directory, "destination.sqlite")
	if err := CopyIndex(ctx, source, destination); err != nil {
		t.Fatalf("copy from a live source: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectIndex(ctx, destination)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Compatibility != CompatibilityCompatible {
		t.Fatalf("copied index compatibility = %q (%s)", inspection.Compatibility, inspection.Diagnostic)
	}
	if inspection.Metadata.Root != "/donor/checkout" {
		t.Fatalf("copied metadata = %#v", inspection.Metadata)
	}
	copied, err := OpenReadOnly(ctx, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copied.Close() }()
	node, err := copied.Node(ctx, "n:copied")
	if err != nil {
		t.Fatal(err)
	}
	if node.QualifiedName != "pkg.Copied" {
		t.Fatalf("copied graph node = %#v", node)
	}
}

func TestCopyIndexRefusesAnExistingDestination(t *testing.T) {
	ctx := context.Background()
	directory := testtemp.Dir(t)
	source := filepath.Join(directory, "source.sqlite")
	repository, err := Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "destination.sqlite")
	if err := os.WriteFile(destination, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CopyIndex(ctx, source, destination); err == nil {
		t.Fatal("copied over an existing destination")
	}
	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "occupied" {
		t.Fatalf("destination was modified: %q", content)
	}
}

func TestSetIndexMetaRewritesAndAddsValuesWithoutMigrating(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "index.sqlite")
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "root", "/donor/checkout"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := SetIndexMeta(ctx, path, map[string]string{
		"root": "/seeded/worktree", "branch": "issue-163", "git_dirty_paths": "",
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	for key, want := range map[string]string{
		"root": "/seeded/worktree", "branch": "issue-163", "git_dirty_paths": "",
	} {
		got, err := reopened.Meta(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("meta %q = %q, want %q", key, got, want)
		}
	}
}
