package golang

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// The unit of persistence is one package directory, because that is the unit a
// single Go edit invalidates. A whole-workspace payload made the common case
// worse than having no cache at all: a one-line body edit leaves the workspace
// key alone but moves the edited package's scope key, so the run decoded every
// package's views, was correctly rejected for the one package it needed, ran a
// real load anyway, and re-encoded every other package to rewrite them
// unchanged. Both halves of that were pure overhead, and the adopted views
// stayed resident while the fresh load built its package graph, which pushed
// peak memory above the uncached baseline.
//
// Sharding removes both. An index file carries the workspace fingerprint and
// each package's scope key, so a run reads a few kilobytes to learn whether the
// package it wants is current, and decodes that package's segment alone. A store
// re-encodes only the packages whose scope keys moved and carries every other
// segment forward by reference, so a body edit rewrites one package instead of
// the workspace.
//
// Reuse is fail-closed. A persisted package is used only when every one of these
// holds, and a real packages.Load runs otherwise:
//
//   - the encoding version matches exactly in the index and in the segment, so
//     neither an older nor a newer writer's payload is interpreted;
//   - the persisted semantic index version matches, so a graph-schema bump
//     cannot reuse evidence derived for a different one;
//   - the workspace semantic key and build context match, so any repository-wide
//     Go change rebuilds;
//   - the segment names the package directory the index points at it for, so an
//     index that crossed two packages' views cannot serve either;
//   - the payload's recorded digest matches its bytes and the digest the index
//     recorded, so a truncated or corrupted segment is rejected rather than
//     decoded into partial views;
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
// path it serves, so a persisted package whose contents changed misses there and
// falls through to a real load exactly like an in-process entry would.

// semanticViewCacheVersion tags the on-disk encoding below. Bump it whenever the
// payload layout or SemanticView's shape changes, so a payload written by a
// different Grafo is never decoded as this one.
const semanticViewCacheVersion = "go-semantic-views-v2"

// semanticViewSegmentPrefix marks the per-package files inside the cache
// directory, so pruning never considers anything it did not write.
const semanticViewSegmentPrefix = "package-"

// temporaryFilePrefix marks the unpublished files publishFile creates, so a
// killed run's residue is recognizable without being mistaken for a segment.
const temporaryFilePrefix = ".go-semantic-views-"

// semanticViewCacheDirectory holds one repository root's index and package
// segments. It lives under .grafo, which is never committed and already holds
// derived indexes.
func semanticViewCacheDirectory(root string) string {
	return filepath.Join(root, ".grafo", "cache", "go-semantic-views")
}

// semanticViewIndexPath is the entry point of the cache: the only file a run
// that cannot reuse anything has to read.
func semanticViewIndexPath(root string) string {
	return filepath.Join(semanticViewCacheDirectory(root), "index.gob")
}

// legacySemanticViewCachePath is the single whole-workspace payload written
// before the cache was sharded. Nothing reads it any more, so publishing a
// sharded index removes it rather than leaving a stale copy of every package's
// views in .grafo forever.
func legacySemanticViewCachePath(root string) string {
	return filepath.Join(root, ".grafo", "cache", "go-semantic-views.gob")
}

// semanticViewSegmentName addresses a segment by the package it holds and the
// bytes it carries. Content addressing means a store never overwrites a file an
// older index still references, and an unchanged package keeps its name.
func semanticViewSegmentName(packageDirectory string, digest []byte) string {
	name := newSemanticDigest(semanticViewCacheVersion)
	name.writeField(packageDirectory)
	name.writeBytes(digest)
	return semanticViewSegmentPrefix + name.sum() + ".gob"
}

// semanticViewIndexFile binds one workspace fingerprint to the package segments
// derived under it.
type semanticViewIndexFile struct {
	Encoding     string
	IndexVersion string
	WorkspaceKey string
	BuildContext string
	Packages     map[string]semanticViewSegment
}

// semanticViewSegment locates one package directory's views and records the
// package scope key they were derived under, not the key on disk now.
type semanticViewSegment struct {
	ScopeKey string
	Name     string
	Digest   []byte
}

// semanticViewSegmentFile is one package's whole file. The payload stays an
// opaque byte slice so its digest can be checked before anything inside it is
// decoded.
type semanticViewSegmentFile struct {
	Encoding  string
	Directory string
	Digest    []byte
	Payload   []byte
}

var errSemanticViewCacheUnusable = errors.New("persisted Go semantic views are unusable")

// loadSemanticViewIndex returns the persisted package segments for root, or an
// error describing why they cannot be reused. No package payload is read: the
// scope keys alone decide reuse, and a run that cannot reuse a package must not
// pay to decode its views. Every failure is a cache miss; none is fatal to
// indexing.
func loadSemanticViewIndex(root, key, buildContext string) (map[string]semanticViewSegment, error) {
	content, err := os.ReadFile(semanticViewIndexPath(root))
	if err != nil {
		return nil, err
	}
	var file semanticViewIndexFile
	if err := gob.NewDecoder(bytes.NewReader(content)).Decode(&file); err != nil {
		return nil, fmt.Errorf("%w: decode index: %v", errSemanticViewCacheUnusable, err)
	}
	if file.Encoding != semanticViewCacheVersion {
		return nil, fmt.Errorf("%w: encoding is %q, want %q", errSemanticViewCacheUnusable, file.Encoding, semanticViewCacheVersion)
	}
	if file.IndexVersion != indexversion.Semantic {
		return nil, fmt.Errorf("%w: semantic index version is %q, want %q", errSemanticViewCacheUnusable, file.IndexVersion, indexversion.Semantic)
	}
	if file.WorkspaceKey != key || file.BuildContext != buildContext {
		return nil, fmt.Errorf("%w: workspace fingerprint changed", errSemanticViewCacheUnusable)
	}
	if len(file.Packages) == 0 {
		return nil, fmt.Errorf("%w: index references no packages", errSemanticViewCacheUnusable)
	}
	for directory, segment := range file.Packages {
		// An index missing part of a reference cannot be checked against the
		// bytes it points at, and a partially trustworthy index is not one.
		if segment.ScopeKey == "" || segment.Name == "" || len(segment.Digest) != sha256.Size {
			return nil, fmt.Errorf("%w: package %q has an incomplete segment reference", errSemanticViewCacheUnusable, directory)
		}
	}
	return file.Packages, nil
}

// loadSemanticViewSegment decodes the views of one package directory. The caller
// has already matched the package's scope key against disk, so this is the only
// payload the run needs.
func loadSemanticViewSegment(root, directory string, segment semanticViewSegment) (map[string]SemanticView, error) {
	content, err := os.ReadFile(filepath.Join(semanticViewCacheDirectory(root), segment.Name))
	if err != nil {
		return nil, err
	}
	var file semanticViewSegmentFile
	if err := gob.NewDecoder(bytes.NewReader(content)).Decode(&file); err != nil {
		return nil, fmt.Errorf("%w: decode segment for %q: %v", errSemanticViewCacheUnusable, directory, err)
	}
	if file.Encoding != semanticViewCacheVersion {
		return nil, fmt.Errorf("%w: segment for %q has encoding %q, want %q", errSemanticViewCacheUnusable, directory, file.Encoding, semanticViewCacheVersion)
	}
	if file.Directory != directory {
		// A segment names the package it was derived for, so an index that
		// points one directory at another's views serves neither.
		return nil, fmt.Errorf("%w: segment for %q holds package %q", errSemanticViewCacheUnusable, directory, file.Directory)
	}
	digest := sha256.Sum256(file.Payload)
	if !bytes.Equal(digest[:], file.Digest) || !bytes.Equal(digest[:], segment.Digest) {
		return nil, fmt.Errorf("%w: segment for %q has a payload digest mismatch", errSemanticViewCacheUnusable, directory)
	}
	var views map[string]SemanticView
	if err := gob.NewDecoder(bytes.NewReader(file.Payload)).Decode(&views); err != nil {
		return nil, fmt.Errorf("%w: decode views for %q: %v", errSemanticViewCacheUnusable, directory, err)
	}
	if len(views) == 0 {
		return nil, fmt.Errorf("%w: segment for %q carries no views", errSemanticViewCacheUnusable, directory)
	}
	return views, nil
}

// discardSemanticViewSegment deletes a segment that failed to decode. Without
// this the next store would carry the unreadable file forward by reference —
// its scope key still matches — and every later run would keep falling back to a
// full load over a cache that can never heal.
func discardSemanticViewSegment(root string, segment semanticViewSegment) {
	_ = os.Remove(filepath.Join(semanticViewCacheDirectory(root), segment.Name))
}

// semanticViewCacheWrite reports what a store published, so the loader's metrics
// can distinguish a cheap index refresh from a re-encode of real payload.
type semanticViewCacheWrite struct {
	// Index is true when a package index was published, and Segments counts the
	// package segments re-encoded to back it.
	Index    bool
	Segments int
}

// storeSemanticViewCache publishes entry for root. Failures are returned for
// tests and diagnostics but are never fatal: a repository whose .grafo is
// unwritable simply keeps recomputing views.
func storeSemanticViewCache(root, buildContext string, entry cachedWorkspace) (semanticViewCacheWrite, error) {
	var written semanticViewCacheWrite
	// Nothing reads the legacy payload, so it goes whether or not this store has
	// anything to publish. Leaving it for a successful publish would keep a
	// whole workspace of unreadable views in .grafo for every repository whose
	// packages this run could not prove.
	_ = os.Remove(legacySemanticViewCachePath(root))
	if len(entry.views) == 0 {
		// Nothing worth reusing, and an empty payload would be rejected on read.
		return written, nil
	}
	directory := semanticViewCacheDirectory(root)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return written, err
	}
	// A segment the published index already describes under this workspace
	// fingerprint and this scope key holds exactly the views being stored, so it
	// is carried forward by reference instead of re-encoded.
	reusable, _ := loadSemanticViewIndex(root, entry.key, buildContext)
	packages := make(map[string]semanticViewSegment, len(entry.scopes))
	for packageDirectory, views := range groupViewsByPackage(entry.views) {
		scope, known := entry.scopes[packageDirectory]
		if !known {
			// Without a provable scope key these views could never be
			// revalidated, so persisting them would only produce a reference no
			// run can reuse.
			continue
		}
		if segment, ok := reusable[packageDirectory]; ok && segment.ScopeKey == scope && segmentFileExists(directory, segment) {
			packages[packageDirectory] = segment
			continue
		}
		segment, err := writeSemanticViewSegment(directory, packageDirectory, scope, views)
		if err != nil {
			return written, err
		}
		packages[packageDirectory] = segment
		written.Segments++
	}
	if len(packages) == 0 {
		return written, nil
	}
	if err := writeSemanticViewIndex(root, buildContext, entry.key, packages); err != nil {
		return written, err
	}
	written.Index = true
	pruneSemanticViewSegments(directory, packages)
	return written, nil
}

// groupViewsByPackage splits a workspace's views by the package directory that
// owns them. Views are keyed by path and Go packages are directory-scoped, so
// this is the exact blast radius of one edit.
func groupViewsByPackage(views map[string]SemanticView) map[string]map[string]SemanticView {
	grouped := make(map[string]map[string]SemanticView)
	for path, view := range views {
		directory := goPackageDirectory(path)
		owned, ok := grouped[directory]
		if !ok {
			owned = make(map[string]SemanticView, 1)
			grouped[directory] = owned
		}
		owned[path] = view
	}
	return grouped
}

func segmentFileExists(directory string, segment semanticViewSegment) bool {
	_, err := os.Stat(filepath.Join(directory, segment.Name))
	return err == nil
}

// writeSemanticViewSegment encodes one package's views and publishes them under
// their content address. An existing file of the same name is overwritten rather
// than trusted, so a corrupted segment heals as soon as its package is derived
// again.
func writeSemanticViewSegment(directory, packageDirectory, scope string, views map[string]SemanticView) (semanticViewSegment, error) {
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(views); err != nil {
		return semanticViewSegment{}, err
	}
	digest := sha256.Sum256(payload.Bytes())
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(semanticViewSegmentFile{
		Encoding: semanticViewCacheVersion, Directory: packageDirectory,
		Digest: digest[:], Payload: payload.Bytes(),
	}); err != nil {
		return semanticViewSegment{}, err
	}
	segment := semanticViewSegment{
		ScopeKey: scope,
		Name:     semanticViewSegmentName(packageDirectory, digest[:]),
		Digest:   digest[:],
	}
	if err := publishFile(filepath.Join(directory, segment.Name), encoded.Bytes()); err != nil {
		return semanticViewSegment{}, err
	}
	return segment, nil
}

// writeSemanticViewIndex publishes the index last, so a reader either sees the
// complete new set of references or the complete old one.
func writeSemanticViewIndex(root, buildContext, key string, packages map[string]semanticViewSegment) error {
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(semanticViewIndexFile{
		Encoding: semanticViewCacheVersion, IndexVersion: indexversion.Semantic,
		WorkspaceKey: key, BuildContext: buildContext, Packages: packages,
	}); err != nil {
		return err
	}
	return publishFile(semanticViewIndexPath(root), encoded.Bytes())
}

// abandonedTemporaryAge is how long an unpublished temporary must have gone
// untouched before pruning treats it as the residue of a killed run. A store
// publishes within milliseconds of creating one, so nothing this old is still
// being written — including by another process, whose in-flight temporary must
// never be removed under it.
const abandonedTemporaryAge = time.Hour

// pruneSemanticViewSegments removes the segments the freshly published index no
// longer references, along with the temporaries of runs that were killed between
// creating one and publishing it. The index is renamed first, so a concurrent
// reader either sees the new index and the files it names, or sees the old index
// and fails closed on a segment that has gone.
func pruneSemanticViewSegments(directory string, packages map[string]semanticViewSegment) {
	referenced := make(map[string]struct{}, len(packages))
	for _, segment := range packages {
		referenced[segment.Name] = struct{}{}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(name, temporaryFilePrefix) {
			info, err := entry.Info()
			if err == nil && time.Since(info.ModTime()) > abandonedTemporaryAge {
				_ = os.Remove(filepath.Join(directory, name))
			}
			continue
		}
		if !strings.HasPrefix(name, semanticViewSegmentPrefix) {
			continue
		}
		if _, keep := referenced[name]; keep {
			continue
		}
		_ = os.Remove(filepath.Join(directory, name))
	}
}

// publishFile writes content to path atomically.
func publishFile(path string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), temporaryFilePrefix+"*")
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
	if _, err := temporary.Write(content); err != nil {
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
