package golang

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// resetScopeWork zeroes the process-wide scope counters so one test can measure
// the work a single sequence performs. The counters and the memo they measure
// are process wide, so a test that reads them cannot run in parallel with
// another that does.
func resetScopeWork() {
	scopeKeyComputations.Store(0)
	packageListings.Store(0)
}

// scopeWorkModule is a module whose first package holds several files, so
// per-file work that should be per-directory is visible as a multiple of the
// file count rather than as a single extra call.
func scopeWorkModule(t *testing.T) string {
	t.Helper()
	root := testtemp.Dir(t)
	writeCacheFile(t, filepath.Join(root, "go.mod"), "module example.com/work\n\ngo 1.26\n")
	writeCacheFile(t, filepath.Join(root, "store", "store.go"), `package store

type Reader interface{ Get(id string) string }
`)
	writeCacheFile(t, filepath.Join(root, "app", "app.go"), `package app

import "example.com/work/store"

func Run(reader store.Reader) string { return reader.Get("a") }
`)
	writeCacheFile(t, filepath.Join(root, "app", "helper.go"), `package app

func help() string { return "help" }
`)
	writeCacheFile(t, filepath.Join(root, "app", "extra.go"), `package app

func extra() string { return "extra" }
`)
	return root
}

// indexFile reproduces what the indexing pipeline does for one file: derive the
// scope key, hand it back through Input, refine the workspace key with it, and
// parse. The pipeline's own sequence is at internal/indexer/pipeline.go:136-154.
func indexFile(t *testing.T, parser *Parser, root, path, workspaceKey string) {
	t.Helper()
	ctx := context.Background()
	input := parserapi.Input{Root: root, Path: path, SemanticKey: workspaceKey}
	scopeKey, err := parser.ScopeKey(ctx, input)
	if err != nil {
		t.Fatalf("scope key for %s: %v", path, err)
	}
	input.ScopeKey = scopeKey
	if _, err := parser.SemanticKey(ctx, input); err != nil {
		t.Fatalf("semantic key for %s: %v", path, err)
	}
	if _, err := parser.Parse(ctx, input); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// TestScopeKeyIsDerivedOncePerFile is the regression guard for the duplicate
// derivation: the pipeline computes a file's scope key to build its content
// hash, and the semantic loader needs the same value to decide whether a cached
// view is still valid. The loader must consume the value it was given rather
// than deriving it a second time, so a reintroduced second call site fails here.
func TestScopeKeyIsDerivedOncePerFile(t *testing.T) {
	root := scopeWorkModule(t)
	parser := New()
	ctx := context.Background()
	workspaceKey, err := parser.WorkspaceSemanticKey(ctx, parserapi.Input{Root: root})
	if err != nil {
		t.Fatalf("workspace key: %v", err)
	}

	// The first file of a run pays the workspace load, which derives a scope key
	// for every loaded package. Measure a later file instead, where the only
	// scope work left is the file's own.
	indexFile(t, parser, root, "app/app.go", workspaceKey)

	resetScopeWork()
	indexFile(t, parser, root, "app/helper.go", workspaceKey)

	work := ScopeWorkCounts()
	if work.ScopeKeyComputations != 1 {
		t.Fatalf("scope key derivations for one file = %d, want 1", work.ScopeKeyComputations)
	}
}

// TestScopeKeyDerivationIsPerDirectoryAcrossAFile covers the whole package: the
// scope key is a property of the directory, so indexing every file of a
// three-file package must derive it once per file and no more. Deriving it twice
// per file is what made a cold index stat a package once for every file it
// contains, squared over the package.
func TestScopeKeyDerivationIsPerDirectoryAcrossAFile(t *testing.T) {
	root := scopeWorkModule(t)
	parser := New()
	ctx := context.Background()
	workspaceKey, err := parser.WorkspaceSemanticKey(ctx, parserapi.Input{Root: root})
	if err != nil {
		t.Fatalf("workspace key: %v", err)
	}
	indexFile(t, parser, root, "app/app.go", workspaceKey)

	resetScopeWork()
	for _, path := range []string{"app/helper.go", "app/extra.go"} {
		indexFile(t, parser, root, path, workspaceKey)
	}

	work := ScopeWorkCounts()
	if work.ScopeKeyComputations != 2 {
		t.Fatalf("scope key derivations for 2 files = %d, want 2", work.ScopeKeyComputations)
	}
}

// TestMissingScopeKeyStillDerivesOne is the fail-closed half: a caller that
// offers no scope key must get a derived one, never a cache hit on an absent
// value. Direct parser users and the federation refresh path both call Parse
// without the pipeline's precomputed key.
func TestMissingScopeKeyStillDerivesOne(t *testing.T) {
	root := scopeWorkModule(t)
	parser := New()
	ctx := context.Background()
	input := parserapi.Input{Root: root, Path: "app/app.go"}
	if _, err := parser.Parse(ctx, input); err != nil {
		t.Fatalf("parse without a scope key: %v", err)
	}

	resetScopeWork()
	if _, err := parser.Parse(ctx, parserapi.Input{Root: root, Path: "app/helper.go"}); err != nil {
		t.Fatalf("parse without a scope key: %v", err)
	}
	if work := ScopeWorkCounts(); work.ScopeKeyComputations == 0 {
		t.Fatal("parsing without a scope key derived none, so an absent key was read as a match")
	}
}

// TestScopeKeyMismatchIsACacheMiss proves the key the caller supplies is
// actually compared rather than trusted: a key that does not match the one
// recorded at load time must miss, because serving that view would publish a
// package whose contents no evidence covers.
func TestScopeKeyMismatchIsACacheMiss(t *testing.T) {
	root := scopeWorkModule(t)
	loader := NewPackageLoader()
	ctx := context.Background()
	if _, err := loader.Load(ctx, parserapi.Input{Root: root, Path: "app/app.go"}); err != nil {
		t.Fatalf("seed the loader: %v", err)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.cached(absolute, loader.cache[absolute].key, "app/app.go", "not-the-recorded-scope-key"); ok {
		t.Fatal("a mismatched scope key was served from cache")
	}
}

// TestConcurrentClosureDigestsAgree covers the narrowed closure lock: the
// traversal no longer runs under the mutex, so two callers may duplicate one
// directory's work. Duplicating it is acceptable only if both publish the same
// digest, because that digest gates invalidation.
func TestConcurrentClosureDigestsAgree(t *testing.T) {
	root := scopeWorkModule(t)
	model, err := scopeModelFor(context.Background(), root)
	if err != nil {
		t.Fatalf("build scope model: %v", err)
	}
	const callers = 16
	digests := make([]string, callers)
	var waiting sync.WaitGroup
	start := make(chan struct{})
	for index := range callers {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			<-start
			digests[index] = model.Closure("app")
		}()
	}
	close(start)
	waiting.Wait()
	for index, digest := range digests {
		if digest == "" {
			t.Fatalf("caller %d produced an empty closure digest", index)
		}
		if digest != digests[0] {
			t.Fatalf("caller %d published %q, caller 0 published %q", index, digest, digests[0])
		}
	}
}
