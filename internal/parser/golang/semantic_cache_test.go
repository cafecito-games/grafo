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
	if _, err := os.Stat(semanticViewCachePath(root)); err != nil {
		t.Fatalf("cold load wrote no view cache: %v", err)
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
	if !reflect.DeepEqual(coldView, warmView) {
		t.Fatalf("persisted views differ from recomputed views:\ncold=%#v\nwarm=%#v", coldView, warmView)
	}
}

// TestPersistedSemanticViewsFallBackWhenUnusable covers every rejection the
// fail-closed contract requires. Each case must end in a real packages.Load, and
// none may serve a view with no evidence in it.
func TestPersistedSemanticViewsFallBackWhenUnusable(t *testing.T) {
	for _, testCase := range []struct {
		name string
		// corrupt mutates the workspace or its cache file after a cold load.
		corrupt func(*testing.T, string)
		// sameEvidence marks a case that changed only the cache file, so the
		// recomputed view must equal what the cold load produced.
		sameEvidence bool
	}{
		{
			name:         "truncated file",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				content := readCache(t, root)
				writeCacheFile(t, semanticViewCachePath(root), string(content[:len(content)/2]))
			},
		},
		{
			name:         "empty file",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				writeCacheFile(t, semanticViewCachePath(root), "")
			},
		},
		{
			name:         "corrupt payload",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeCache(t, root)
				file.Payload = append([]byte("not a gob stream"), file.Payload...)
				encodeCache(t, root, file)
			},
		},
		{
			name:         "payload digest mismatch",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeCache(t, root)
				file.Digest = bytes.Repeat([]byte{0}, sha256.Size)
				encodeCache(t, root, file)
			},
		},
		{
			name:         "future encoding version",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeCache(t, root)
				file.Encoding = semanticViewCacheVersion + "-next"
				encodeCache(t, root, file)
			},
		},
		{
			name:         "semantic index version changed",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeCache(t, root)
				file.IndexVersion = file.IndexVersion + "0"
				encodeCache(t, root, file)
			},
		},
		{
			name:         "empty view set",
			sameEvidence: true,
			corrupt: func(t *testing.T, root string) {
				file := decodeCache(t, root)
				var payload bytes.Buffer
				if err := gob.NewEncoder(&payload).Encode(semanticViewCachePayload{}); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(payload.Bytes())
				file.Payload, file.Digest = payload.Bytes(), digest[:]
				encodeCache(t, root, file)
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
				// A body-only edit leaves the workspace key alone, so the entry
				// is adopted and then rejected for this one package.
				writeCacheFile(t, filepath.Join(root, "app", "app.go"), `package app

import "example.com/service/store"

func Run(reader store.Reader) string { return reader.Get("b") }
`)
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

// TestPersistedSemanticViewCacheRejectsForeignHeaders pins the header checks
// directly, so a rejection cannot be mistaken for an unrelated miss.
func TestPersistedSemanticViewCacheRejectsForeignHeaders(t *testing.T) {
	root := testtemp.Dir(t)
	entry := cachedWorkspace{key: "key", views: map[string]SemanticView{"a.go": {Available: true, Included: true}}, scopes: map[string]string{".": "scope"}}
	if err := storeSemanticViewCache(root, "context", entry); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSemanticViewCache(root, "key", "context"); err != nil {
		t.Fatalf("a matching header was rejected: %v", err)
	}
	for name, probe := range map[string]struct{ key, buildContext string }{
		"other workspace key": {key: "other", buildContext: "context"},
		"other build context": {key: "key", buildContext: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSemanticViewCache(root, probe.key, probe.buildContext); !errors.Is(err, errSemanticViewCacheUnusable) {
				t.Fatalf("mismatched header accepted: %v", err)
			}
		})
	}
	if _, err := loadSemanticViewCache(testtemp.Dir(t), "key", "context"); err == nil {
		t.Fatal("an absent cache file reported a usable entry")
	}
}

// TestStoreSemanticViewCacheSkipsEmptyEntries keeps a useless payload off disk:
// an empty view set would be rejected on read, so writing it only hides a real
// failure behind a file that looks like a cache.
func TestStoreSemanticViewCacheSkipsEmptyEntries(t *testing.T) {
	root := testtemp.Dir(t)
	if err := storeSemanticViewCache(root, "context", cachedWorkspace{key: "key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(semanticViewCachePath(root)); !os.IsNotExist(err) {
		t.Fatalf("empty entry was persisted: %v", err)
	}
}

func readCache(t *testing.T, root string) []byte {
	t.Helper()
	content, err := os.ReadFile(semanticViewCachePath(root))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func decodeCache(t *testing.T, root string) semanticViewCacheFile {
	t.Helper()
	var file semanticViewCacheFile
	if err := gob.NewDecoder(bytes.NewReader(readCache(t, root))).Decode(&file); err != nil {
		t.Fatal(err)
	}
	return file
}

func encodeCache(t *testing.T, root string, file semanticViewCacheFile) {
	t.Helper()
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(semanticViewCachePath(root), encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
