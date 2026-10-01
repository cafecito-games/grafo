package golang

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// semanticCacheModule is a two-package module whose caller resolves through an
// interface declared in the other package, so its views only exist if a real
// packages.Load produced them.
func semanticCacheModule(t *testing.T) string {
	t.Helper()
	root := testtemp.Dir(t)
	writeCacheFile(t, filepath.Join(root, "go.mod"), "module example.com/service\n\ngo 1.26\n")
	writeCacheFile(t, filepath.Join(root, "store", "store.go"), `package store

type Reader interface{ Get(id string) string }

type Memory struct{}

func (Memory) Get(id string) string { return id }
`)
	writeCacheFile(t, filepath.Join(root, "app", "app.go"), `package app

import "example.com/service/store"

func Run(reader store.Reader) string { return reader.Get("a") }
`)
	return root
}

func writeCacheFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadAppView(t *testing.T, loader *PackageLoader, root string) SemanticView {
	t.Helper()
	view, err := loader.Load(context.Background(), parserapi.Input{Root: root, Path: "app/app.go"})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// editAppBody adds a call inside app.Run. A body edit leaves the workspace
// semantic key alone and moves only the app package's scope key, which is the
// most common incremental change a Go repository sees. The added call also moves
// the package's derived views, so a reuse decision cannot be hidden by two
// different bodies happening to extract identically.
func editAppBody(t *testing.T, root string) {
	t.Helper()
	writeCacheFile(t, filepath.Join(root, "app", "app.go"), `package app

import "example.com/service/store"

func Run(reader store.Reader) string { return reader.Get(reader.Get("a")) }
`)
}

// TestPersistedSemanticViewsSurviveAcrossLoaders is the headline contract: a
// second loader over an unchanged workspace adopts the first loader's derived
// views instead of re-running packages.Load, and sees exactly the same evidence.
func TestPersistedSemanticViewsSurviveAcrossLoaders(t *testing.T) {
	root := semanticCacheModule(t)

	cold := NewPackageLoader()
	coldView := loadAppView(t, cold, root)
	if metrics := cold.Metrics(); metrics.Loads != 1 || metrics.PersistedSaves != 1 {
		t.Fatalf("cold loader metrics = %#v", metrics)
	}
	if len(coldView.Calls) == 0 {
		t.Fatalf("cold load produced no call evidence: %#v", coldView)
	}
	if _, err := os.Stat(semanticViewIndexPath(root)); err != nil {
		t.Fatalf("cold load wrote no view index: %v", err)
	}
	if names := segmentNames(t, root); len(names) != 2 {
		t.Fatalf("cold load wrote %d package segments, want one per package: %v", len(names), names)
	}

	warm := NewPackageLoader()
	warmView := loadAppView(t, warm, root)
	metrics := warm.Metrics()
	if metrics.Loads != 0 {
		t.Fatalf("warm loader re-ran packages.Load: %#v", metrics)
	}
	if metrics.PersistedHits != 1 {
		t.Fatalf("warm loader did not adopt the persisted views: %#v", metrics)
	}
	// The run read one package, so it must have paid for one package. Decoding
	// the store package too would be exactly the whole-workspace cost this cache
	// is sharded to avoid.
	if metrics.PersistedDecodes != 1 {
		t.Fatalf("warm loader decoded %d segments, want only the package it read: %#v", metrics.PersistedDecodes, metrics)
	}
	if !reflect.DeepEqual(coldView, warmView) {
		t.Fatalf("persisted views differ from recomputed views:\ncold=%#v\nwarm=%#v", coldView, warmView)
	}
}

// TestBodyEditRewritesOnlyTheEditedPackage is the regression this layout exists
// for. A one-line body edit must not make the run read or rewrite the views of
// every other package: the entry's workspace key still matches, so every
// unchanged package's segment is already current and is carried forward by
// reference.
func TestBodyEditRewritesOnlyTheEditedPackage(t *testing.T) {
	root := semanticCacheModule(t)
	cold := NewPackageLoader()
	loadAppView(t, cold, root)
	before := segmentNames(t, root)
	if len(before) != 2 {
		t.Fatalf("cold load wrote %d package segments: %v", len(before), before)
	}

	editAppBody(t, root)
	edited := NewPackageLoader()
	view := loadAppView(t, edited, root)
	if len(view.Calls) == 0 {
		t.Fatalf("edited run served empty views: %#v", view)
	}
	metrics := edited.Metrics()
	if metrics.Loads != 1 {
		t.Fatalf("stale package was reused: %#v", metrics)
	}
	// The edited package's scope key moved, so its views cannot be reused. Every
	// other package's can, and neither decoding nor re-encoding them buys
	// anything.
	if metrics.PersistedDecodes != 0 {
		t.Fatalf("body edit decoded %d segments it could not use: %#v", metrics.PersistedDecodes, metrics)
	}
	if metrics.PersistedSegmentWrites != 1 {
		t.Fatalf("body edit rewrote %d segments, want only the edited package: %#v", metrics.PersistedSegmentWrites, metrics)
	}

	after := segmentNames(t, root)
	if len(after) != 2 {
		t.Fatalf("store left %d package segments: %v", len(after), after)
	}
	unchanged := 0
	for _, name := range before {
		if slices.Contains(after, name) {
			unchanged++
		}
	}
	if unchanged != 1 {
		t.Fatalf("%d segments survived the edit, want the one unedited package:\nbefore=%v\nafter=%v", unchanged, before, after)
	}

	// The rewritten package is reusable again, and the carried-forward one still
	// decodes, so a later run over the edited tree pays no load at all.
	warm := NewPackageLoader()
	warmView := loadAppView(t, warm, root)
	if metrics := warm.Metrics(); metrics.Loads != 0 {
		t.Fatalf("the rewritten segment was not reusable: %#v", metrics)
	}
	if !reflect.DeepEqual(view, warmView) {
		t.Fatalf("reused views differ from the edited run's:\nwant=%#v\ngot=%#v", view, warmView)
	}
}

// TestPersistedSemanticViewCacheRemovesTheLegacyPayload keeps a whole-workspace
// payload written by an older Grafo from sitting in .grafo forever. Nothing
// reads it, and on a real corpus it is the largest file in the cache.
func TestPersistedSemanticViewCacheRemovesTheLegacyPayload(t *testing.T) {
	root := semanticCacheModule(t)
	legacy := legacySemanticViewCachePath(root)
	writeCacheFile(t, legacy, "views from a previous encoding")

	loadAppView(t, NewPackageLoader(), root)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy payload survived a store: %v", err)
	}
}

// TestPersistedSemanticViewsFallBackWhenUnusable covers every rejection the
// fail-closed contract requires. Each case must end in a real packages.Load, and
// none may serve a view with no evidence in it.
func TestPersistedSemanticViewsFallBackWhenUnusable(t *testing.T) {
	for _, testCase := range []struct {
		name string
		// corrupt mutates the workspace or its cache after a cold load.
		corrupt func(*testing.T, string)
		// sameEvidence marks a case that changed only the cache, so the
		// recomputed view must equal what the cold load produced.
		sameEvidence bool
	}{
		{
			name:         "truncated index",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				content := readIndex(t, root)
				writeCacheFile(t, semanticViewIndexPath(root), string(content[:len(content)/2]))
			},
		},
		{
			name:         "empty index",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				writeCacheFile(t, semanticViewIndexPath(root), "")
			},
		},
		{
			name:         "index references no packages",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeIndex(t, root)
				file.Packages = nil
				encodeIndex(t, root, file)
			},
		},
		{
			name:         "incomplete segment reference",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeIndex(t, root)
				segment := file.Packages["app"]
				segment.Digest = nil
				file.Packages["app"] = segment
				encodeIndex(t, root, file)
			},
		},
		{
			name:         "future encoding version",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeIndex(t, root)
				file.Encoding = semanticViewCacheVersion + "-next"
				encodeIndex(t, root, file)
			},
		},
		{
			name:         "semantic index version changed",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeIndex(t, root)
				file.IndexVersion = file.IndexVersion + "0"
				encodeIndex(t, root, file)
			},
		},
		{
			name:         "missing segment file",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeIndex(t, root)
				if err := os.Remove(filepath.Join(semanticViewCacheDirectory(root), file.Packages["app"].Name)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:         "truncated segment",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				name := decodeIndex(t, root).Packages["app"].Name
				path := filepath.Join(semanticViewCacheDirectory(root), name)
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				writeCacheFile(t, path, string(content[:len(content)/2]))
			},
		},
		{
			name:         "corrupt segment payload",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				segment := decodeSegmentFile(t, root, "app")
				segment.Payload = append([]byte("not a gob stream"), segment.Payload...)
				encodeSegmentFile(t, root, "app", segment)
			},
		},
		{
			name:         "segment digest mismatch",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				segment := decodeSegmentFile(t, root, "app")
				segment.Digest = bytes.Repeat([]byte{0}, sha256.Size)
				encodeSegmentFile(t, root, "app", segment)
			},
		},
		{
			name:         "segment holds another package",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				segment := decodeSegmentFile(t, root, "app")
				segment.Directory = "store"
				encodeSegmentFile(t, root, "app", segment)
			},
		},
		{
			name:         "segment carries no views",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				segment := decodeSegmentFile(t, root, "app")
				var payload bytes.Buffer
				if err := gob.NewEncoder(&payload).Encode(map[string]SemanticView{}); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(payload.Bytes())
				segment.Payload, segment.Digest = payload.Bytes(), digest[:]
				encodeSegmentFile(t, root, "app", segment)
			},
		},
		{
			name: "workspace key changed",
			corrupt: func(t *testing.T, root string) {
				// A declaration edit in another package is a repository-wide
				// change, so the persisted entry belongs to a different graph.
				writeCacheFile(t, filepath.Join(root, "store", "store.go"), `package store

type Reader interface{ Get(id string) string }

type Memory struct{}

func (Memory) Get(id string) string { return id }

func Extra() {}
`)
			},
		},
		{
			name: "package scope key changed",
			corrupt: func(t *testing.T, root string) {
				// A body-only edit leaves the workspace key alone, so the index
				// is adopted and then rejected for this one package.
				editAppBody(t, root)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := semanticCacheModule(t)
			cold := NewPackageLoader()
			expected := loadAppView(t, cold, root)
			testCase.corrupt(t, root)

			reloaded := NewPackageLoader()
			view := loadAppView(t, reloaded, root)
			if metrics := reloaded.Metrics(); metrics.Loads != 1 {
				t.Fatalf("unusable persisted views were reused: %#v", metrics)
			}
			if len(view.Calls) == 0 {
				t.Fatalf("fallback served empty views: %#v", view)
			}
			if testCase.sameEvidence && !reflect.DeepEqual(expected, view) {
				t.Fatalf("fallback changed the derived evidence:\nwant=%#v\ngot=%#v", expected, view)
			}
		})
	}
}

// TestUnusableSegmentIsRewrittenRatherThanCarriedForward keeps a corrupt cache
// from becoming a permanent one. A segment whose scope key still matches would
// otherwise be carried forward by reference on every later store, so every run
// would pay a full load over a cache that never heals.
func TestUnusableSegmentIsRewrittenRatherThanCarriedForward(t *testing.T) {
	root := semanticCacheModule(t)
	loadAppView(t, NewPackageLoader(), root)
	segment := decodeSegmentFile(t, root, "app")
	segment.Digest = bytes.Repeat([]byte{0}, sha256.Size)
	encodeSegmentFile(t, root, "app", segment)

	recovering := NewPackageLoader()
	recovered := loadAppView(t, recovering, root)
	if metrics := recovering.Metrics(); metrics.Loads != 1 {
		t.Fatalf("corrupt segment was reused: %#v", metrics)
	}

	healed := NewPackageLoader()
	view := loadAppView(t, healed, root)
	metrics := healed.Metrics()
	if metrics.Loads != 0 {
		t.Fatalf("the corrupt segment was not replaced: %#v", metrics)
	}
	if !reflect.DeepEqual(recovered, view) {
		t.Fatalf("healed cache changed the derived evidence:\nwant=%#v\ngot=%#v", recovered, view)
	}
}

// TestPersistedSemanticViewIndexRejectsForeignHeaders pins the header checks
// directly, so a rejection cannot be mistaken for an unrelated miss.
func TestPersistedSemanticViewIndexRejectsForeignHeaders(t *testing.T) {
	root := testtemp.Dir(t)
	entry := cachedWorkspace{
		key:    "key",
		views:  map[string]SemanticView{"a.go": {Available: true, Included: true}},
		scopes: map[string]string{".": "scope"},
	}
	if _, err := storeSemanticViewCache(root, "context", entry); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSemanticViewIndex(root, "key", "context"); err != nil {
		t.Fatalf("a matching header was rejected: %v", err)
	}
	for name, probe := range map[string]struct{ key, buildContext string }{
		"other workspace key": {key: "other", buildContext: "context"},
		"other build context": {key: "key", buildContext: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSemanticViewIndex(root, probe.key, probe.buildContext); !errors.Is(err, errSemanticViewCacheUnusable) {
				t.Fatalf("mismatched header accepted: %v", err)
			}
		})
	}
	if _, err := loadSemanticViewIndex(testtemp.Dir(t), "key", "context"); err == nil {
		t.Fatal("an absent index reported a usable entry")
	}
}

// TestStoreSemanticViewCachePrunesUnreferencedSegments keeps the cache
// proportional to the workspace: a package whose views are rewritten leaves no
// copy of its previous ones behind.
func TestStoreSemanticViewCachePrunesUnreferencedSegments(t *testing.T) {
	root := testtemp.Dir(t)
	entry := cachedWorkspace{
		key:    "key",
		views:  map[string]SemanticView{"pkg/a.go": {Available: true, Included: true}},
		scopes: map[string]string{"pkg": "scope"},
	}
	if _, err := storeSemanticViewCache(root, "context", entry); err != nil {
		t.Fatal(err)
	}
	first := segmentNames(t, root)

	entry.scopes = map[string]string{"pkg": "moved"}
	entry.views = map[string]SemanticView{"pkg/a.go": {Available: true, Included: false}}
	written, err := storeSemanticViewCache(root, "context", entry)
	if err != nil {
		t.Fatal(err)
	}
	if written.Segments != 1 {
		t.Fatalf("a moved scope key rewrote %d segments: %#v", written.Segments, written)
	}
	second := segmentNames(t, root)
	if len(second) != 1 {
		t.Fatalf("store left %d segments for one package: %v", len(second), second)
	}
	if slices.Equal(first, second) {
		t.Fatalf("changed views reused the previous segment: %v", second)
	}
}

// TestStoreSemanticViewCacheSkipsUnprovablePackages keeps a reference nothing can
// revalidate out of the index: without a scope key the package could never be
// reused, so writing its views only costs the encode.
func TestStoreSemanticViewCacheSkipsUnprovablePackages(t *testing.T) {
	root := testtemp.Dir(t)
	entry := cachedWorkspace{
		key: "key",
		views: map[string]SemanticView{
			"pkg/a.go":   {Available: true, Included: true},
			"other/b.go": {Available: true, Included: true},
		},
		scopes: map[string]string{"pkg": "scope"},
	}
	written, err := storeSemanticViewCache(root, "context", entry)
	if err != nil {
		t.Fatal(err)
	}
	if written.Segments != 1 {
		t.Fatalf("store wrote %d segments, want only the provable package: %#v", written.Segments, written)
	}
	packages, err := loadSemanticViewIndex(root, "key", "context")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := packages["other"]; present {
		t.Fatalf("a package with no scope key was indexed: %#v", packages)
	}
}

// TestStoreSemanticViewCacheSkipsEmptyEntries keeps a useless payload off disk:
// an empty view set would be rejected on read, so writing it only hides a real
// failure behind a file that looks like a cache.
func TestStoreSemanticViewCacheSkipsEmptyEntries(t *testing.T) {
	root := testtemp.Dir(t)
	written, err := storeSemanticViewCache(root, "context", cachedWorkspace{key: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if written.Index || written.Segments != 0 {
		t.Fatalf("an empty entry reported a write: %#v", written)
	}
	if _, err := os.Stat(semanticViewIndexPath(root)); !os.IsNotExist(err) {
		t.Fatalf("empty entry was persisted: %v", err)
	}
}

// segmentNames lists the per-package segment files of root's cache, sorted.
func segmentNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(semanticViewCacheDirectory(root))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), semanticViewSegmentPrefix) {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}

func readIndex(t *testing.T, root string) []byte {
	t.Helper()
	content, err := os.ReadFile(semanticViewIndexPath(root))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func decodeIndex(t *testing.T, root string) semanticViewIndexFile {
	t.Helper()
	var file semanticViewIndexFile
	if err := gob.NewDecoder(bytes.NewReader(readIndex(t, root))).Decode(&file); err != nil {
		t.Fatal(err)
	}
	return file
}

func encodeIndex(t *testing.T, root string, file semanticViewIndexFile) {
	t.Helper()
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(semanticViewIndexPath(root), encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// decodeSegmentFile reads the segment the index points at for a package
// directory, so a test can corrupt exactly one package's payload.
func decodeSegmentFile(t *testing.T, root, directory string) semanticViewSegmentFile {
	t.Helper()
	segment, present := decodeIndex(t, root).Packages[directory]
	if !present {
		t.Fatalf("no indexed segment for %q", directory)
	}
	content, err := os.ReadFile(filepath.Join(semanticViewCacheDirectory(root), segment.Name))
	if err != nil {
		t.Fatal(err)
	}
	var file semanticViewSegmentFile
	if err := gob.NewDecoder(bytes.NewReader(content)).Decode(&file); err != nil {
		t.Fatal(err)
	}
	return file
}

// encodeSegmentFile writes a segment back under the name the index already
// records for directory, leaving the index's digest to catch the change.
func encodeSegmentFile(t *testing.T, root, directory string, file semanticViewSegmentFile) {
	t.Helper()
	segment, present := decodeIndex(t, root).Packages[directory]
	if !present {
		t.Fatalf("no indexed segment for %q", directory)
	}
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(file); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(semanticViewCacheDirectory(root), segment.Name)
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
