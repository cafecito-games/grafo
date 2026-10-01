package golang

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cafecito-games/grafo/internal/indexversion"
)

// PackageLoader caches derived SemanticViews in process, but every grafo index
// is a fresh process, so any run that parsed even one Go file paid a full
// packages.Load of the owning module with NeedTypes|NeedDeps|NeedSyntax|
// NeedTypesInfo and Tests: true. Scoping the workspace key per package made the
// updated set small without making that load any cheaper.
//
// The views are already compact — they deliberately retain no go/ast or
// go/types objects — so the same entry the in-process cache holds can outlive
// the process. This file persists cachedWorkspace and nothing else: the cached
// views, and the package scope key each one was derived under.
//
// Reuse is fail-closed. A persisted entry is used only when every one of these
// holds, and a real packages.Load runs otherwise:
//
//   - the encoding version matches exactly, so neither an older nor a
//     newer writer's payload is interpreted;
//   - the persisted semantic index version matches, so a graph-schema bump
//     cannot reuse evidence derived for a different one;
//   - the workspace semantic key and build context match, so any repository-wide
//     Go change rebuilds;
//   - the payload's recorded digest matches its bytes, so a truncated or
//     corrupted file is rejected rather than decoded into partial views;
//   - the payload decodes, and decodes to a non-empty view set.
//
// The last condition is the important one. Serving empty views silently drops
// every cross-file edge, which looks like a successful index of a repository
// with no resolvable Go calls. A repository that genuinely has no Go views pays
// one cheap load per process instead, which is the right trade: a cache is
// allowed to be useless, never wrong.
//
// Per-package staleness is not this file's job. PackageLoader.cached already
// revalidates the owning directory's package scope key against disk for every
// path it serves, so a persisted entry whose package changed misses there and
// falls through to a real load exactly like an in-process entry would.

// semanticViewCacheVersion tags the on-disk encoding below. Bump it whenever the
// payload layout or SemanticView's shape changes, so a payload written by a
// different Grafo is never decoded as this one.
const semanticViewCacheVersion = "go-semantic-views-v1"

// semanticViewCacheFile is the whole file. The payload stays an opaque byte
// slice so its digest can be checked before anything inside it is decoded.
type semanticViewCacheFile struct {
	Encoding     string
	IndexVersion string
	WorkspaceKey string
	BuildContext string
	Digest       []byte
	Payload      []byte
}

type semanticViewCachePayload struct {
	Views map[string]SemanticView
	// Scopes is the package scope key of each loaded package directory at the
	// time the views were derived, not at the time they are read back.
	Scopes map[string]string
}

// semanticViewCachePath is the cache file for one repository root. It lives
// under .grafo, which is never committed and already holds derived indexes.
func semanticViewCachePath(root string) string {
	return filepath.Join(root, ".grafo", "cache", "go-semantic-views.gob")
}

var errSemanticViewCacheUnusable = errors.New("persisted Go semantic views are unusable")

// loadSemanticViewCache returns the persisted workspace entry for root, or an
// error describing why it cannot be reused. Every failure is a cache miss; none
// is fatal to indexing.
func loadSemanticViewCache(root, key, buildContext string) (cachedWorkspace, error) {
	content, err := os.ReadFile(semanticViewCachePath(root))
	if err != nil {
		return cachedWorkspace{}, err
	}
	var file semanticViewCacheFile
	if err := gob.NewDecoder(bytes.NewReader(content)).Decode(&file); err != nil {
		return cachedWorkspace{}, fmt.Errorf("%w: decode header: %v", errSemanticViewCacheUnusable, err)
	}
	if file.Encoding != semanticViewCacheVersion {
		return cachedWorkspace{}, fmt.Errorf("%w: encoding is %q, want %q", errSemanticViewCacheUnusable, file.Encoding, semanticViewCacheVersion)
	}
	if file.IndexVersion != indexversion.Semantic {
		return cachedWorkspace{}, fmt.Errorf("%w: semantic index version is %q, want %q", errSemanticViewCacheUnusable, file.IndexVersion, indexversion.Semantic)
	}
	if file.WorkspaceKey != key || file.BuildContext != buildContext {
		return cachedWorkspace{}, fmt.Errorf("%w: workspace fingerprint changed", errSemanticViewCacheUnusable)
	}
	digest := sha256.Sum256(file.Payload)
	if !bytes.Equal(digest[:], file.Digest) {
		return cachedWorkspace{}, fmt.Errorf("%w: payload digest mismatch", errSemanticViewCacheUnusable)
	}
	var payload semanticViewCachePayload
	if err := gob.NewDecoder(bytes.NewReader(file.Payload)).Decode(&payload); err != nil {
		return cachedWorkspace{}, fmt.Errorf("%w: decode payload: %v", errSemanticViewCacheUnusable, err)
	}
	if len(payload.Views) == 0 {
		return cachedWorkspace{}, fmt.Errorf("%w: payload carries no views", errSemanticViewCacheUnusable)
	}
	return cachedWorkspace{key: key, views: payload.Views, scopes: payload.Scopes}, nil
}

// storeSemanticViewCache writes entry for root atomically. Failures are returned
// for tests and diagnostics but are never fatal: a repository whose .grafo is
// unwritable simply keeps recomputing views.
func storeSemanticViewCache(root, buildContext string, entry cachedWorkspace) error {
	if len(entry.views) == 0 {
		// Nothing worth reusing, and an empty payload would be rejected on read.
		return nil
	}
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(semanticViewCachePayload{Views: entry.views, Scopes: entry.scopes}); err != nil {
		return err
	}
	digest := sha256.Sum256(payload.Bytes())
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(semanticViewCacheFile{
		Encoding: semanticViewCacheVersion, IndexVersion: indexversion.Semantic,
		WorkspaceKey: entry.key, BuildContext: buildContext,
		Digest: digest[:], Payload: payload.Bytes(),
	}); err != nil {
		return err
	}
	path := semanticViewCachePath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".go-semantic-views-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(name)
		}
	}()
	if _, err := temporary.Write(encoded.Bytes()); err != nil {
		_ = temporary.Close()
		return err
	}
	// A reader that mapped a partially flushed rename would see a payload whose
	// digest still matched its truncated header, so the bytes are durable before
	// the name is published.
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}
