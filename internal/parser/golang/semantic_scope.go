package golang

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	goast "go/ast"
	goparser "go/parser"
	"go/scanner"
	"go/token"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// goSemanticSurfaceVersion tags the encoding of the repository-wide Go
// declaration surface. Bump it whenever the encoding below changes so a
// previously indexed graph cannot be reused against a different fingerprint.
const goSemanticSurfaceVersion = "go-declaration-surface-v1"

// goPackageScopeVersion tags the encoding of the package-local scope key.
const goPackageScopeVersion = "go-package-scope-v1"

// goDeclarationSurface returns the token stream of a Go source file with every
// top-level function body, and every comment that is not a compiler directive,
// removed.
//
// Only the declaration surface can change how an *otherwise untouched* file
// extracts: the cross-file analyzers resolve identifiers through go/types,
// match concrete types against the interfaces declared anywhere in the module,
// and constant-fold package-level values. Statement-level analysis
// (Chi routing, ServeMux registration, outbound requests, ENet transport) only
// ever walks the syntax of the package it belongs to, so a function body is a
// package-local fact rather than a repository-wide one. A documentation comment
// is extracted as evidence of the file that carries it, never of another file,
// so it is covered by that file's own content hash instead.
//
// Everything that selects what the Go toolchain sees stays in the surface:
// build constraints, every //go: directive, and all declaration syntax with its
// literals byte for byte. Emitting tokens rather than source text makes the
// result independent of layout, so reformatting or re-commenting a file cannot
// change its surface while a changed literal always does. A file that does not
// parse has no trustworthy surface, so the caller falls back to its full
// contents.
func goDeclarationSurface(path string, content []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, content, goparser.ParseComments|goparser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	parsedFile := fset.File(file.Package)
	if parsedFile == nil {
		return nil, errNoTokenFile
	}
	var bodies []byteSpan
	for _, declaration := range file.Decls {
		function, ok := declaration.(*goast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		start := parsedFile.Offset(function.Body.Lbrace)
		end := parsedFile.Offset(function.Body.Rbrace)
		if start < 0 || end < start || end > len(content) {
			return nil, errUnorderedBody
		}
		bodies = append(bodies, byteSpan{start: start + 1, end: end})
	}

	scanned := token.NewFileSet()
	scannedFile := scanned.AddFile(path, scanned.Base(), len(content))
	var lexer scanner.Scanner
	lexer.Init(scannedFile, content, nil, scanner.ScanComments)
	var surface bytes.Buffer
	surface.Grow(len(content))
	for {
		position, tokenKind, literal := lexer.Scan()
		if tokenKind == token.EOF {
			break
		}
		offset := scannedFile.Offset(position)
		if withinSpan(bodies, offset) {
			continue
		}
		if tokenKind == token.COMMENT && !isGoDirectiveComment(literal) {
			continue
		}
		surface.WriteString(tokenKind.String())
		surface.WriteByte(0)
		surface.WriteString(literal)
		surface.WriteByte(0)
	}
	return surface.Bytes(), nil
}

type byteSpan struct {
	start int
	end   int
}

// withinSpan reports whether offset falls inside one of the ordered,
// non-overlapping spans. Function bodies never nest at the top level, so a
// linear scan from the first candidate is exact.
func withinSpan(spans []byteSpan, offset int) bool {
	for _, span := range spans {
		if offset < span.start {
			return false
		}
		if offset < span.end {
			return true
		}
	}
	return false
}

// isGoDirectiveComment reports whether a comment can change what the toolchain
// compiles or generates, and therefore how another file extracts.
func isGoDirectiveComment(text string) bool {
	trimmed := strings.TrimPrefix(text, "//")
	return strings.HasPrefix(trimmed, "go:") || strings.HasPrefix(trimmed, " +build") || strings.HasPrefix(trimmed, "+build")
}

// goPackageDirectory is the repository-relative directory that owns path. Go
// packages are directory-scoped, so this is the exact blast radius of a
// body-only edit.
func goPackageDirectory(path string) string {
	directory := filepath.ToSlash(filepath.Dir(filepath.ToSlash(strings.TrimPrefix(path, "./"))))
	if directory == "" {
		return "."
	}
	return directory
}

func isGoSourcePath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

// isGoManifestInput reports whether path is a module or vendor manifest rather
// than a Go source file. Manifest changes alter the load plan for the whole
// repository and stay conservative.
func isGoManifestInput(path string) bool {
	return !isGoSourcePath(path) && isGoSemanticInput(path)
}

// packageScopes is the process-wide memo for package scope digests. It is
// shared by the incremental cache key and the semantic loader so one directory
// is never read twice for the same unchanged contents.
var packageScopes = newPackageScopeCache()

// packageScopeKey digests every Go source file in the directory that owns path.
// Statement-level evidence is attributed to the highest package-local callsite,
// which may live in a sibling file, so a package is the smallest scope whose
// contents can change how one of its files extracts.
func packageScopeKey(root, path string) (string, error) {
	return packageScopes.Key(root, path)
}

// packageScopeCache memoizes package-local scope digests for one process. The
// stat fingerprint of the directory guards reuse so a long-lived server cannot
// serve a digest for contents that have since changed on disk.
type packageScopeCache struct {
	mu      sync.Mutex
	entries map[string]packageScopeEntry
}

type packageScopeEntry struct {
	fingerprint string
	digest      string
}

func newPackageScopeCache() *packageScopeCache {
	return &packageScopeCache{entries: map[string]packageScopeEntry{}}
}

func (c *packageScopeCache) Key(root, path string) (string, error) {
	directory := goPackageDirectory(path)
	absolute := filepath.Join(root, filepath.FromSlash(directory))
	entries, err := os.ReadDir(absolute)
	if err != nil {
		// A directory that cannot be listed has no provable package scope.
		// Fall back to a fingerprint of the error so the key stays stable
		// while the condition lasts without claiming an empty package.
		return goPackageScopeVersion + ":unreadable:" + err.Error(), nil
	}
	names := make([]string, 0, len(entries))
	fingerprint := &bytes.Buffer{}
	fingerprint.WriteString(goPackageScopeVersion)
	fingerprint.WriteString(directory)
	for _, entry := range entries {
		if entry.IsDir() || !isGoSourcePath(entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		info, infoErr := os.Stat(filepath.Join(absolute, name))
		if infoErr != nil {
			fingerprint.WriteString("\x00" + name + "\x00stat-error")
			continue
		}
		fingerprint.WriteString("\x00" + name + "\x00")
		fingerprint.WriteString(strconv.FormatInt(info.Size(), 10))
		fingerprint.WriteString("\x00")
		fingerprint.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10))
	}
	cacheKey := root + "\x00" + directory
	stamp := fingerprint.String()
	c.mu.Lock()
	if entry, ok := c.entries[cacheKey]; ok && entry.fingerprint == stamp {
		c.mu.Unlock()
		return entry.digest, nil
	}
	c.mu.Unlock()

	digest := newSemanticDigest(goPackageScopeVersion)
	digest.writeField(directory)
	for _, name := range names {
		content, readErr := os.ReadFile(filepath.Join(absolute, name))
		digest.writeField(name)
		if readErr != nil {
			digest.writeField("read-error:" + readErr.Error())
			continue
		}
		digest.writeBytes(content)
	}
	result := digest.sum()
	c.mu.Lock()
	if len(c.entries) >= packageScopeLimit {
		c.entries = map[string]packageScopeEntry{}
	}
	c.entries[cacheKey] = packageScopeEntry{fingerprint: stamp, digest: result}
	c.mu.Unlock()
	return result, nil
}

// packageScopeLimit bounds the memo. Discarding it only costs a reread.
const packageScopeLimit = 1 << 14

var (
	errNoTokenFile   = errors.New("parsed Go file has no position information")
	errUnorderedBody = errors.New("parsed Go file reports an out-of-order function body")
)

// semanticDigest accumulates length-delimited fields so no concatenation of
// inputs can collide with a different field layout.
type semanticDigest struct {
	hash hash.Hash
}

func newSemanticDigest(version string) semanticDigest {
	digest := semanticDigest{hash: sha256.New()}
	digest.writeField(version)
	return digest
}

func (d semanticDigest) writeField(value string) { d.writeBytes([]byte(value)) }

func (d semanticDigest) writeBytes(value []byte) {
	var length [8]byte
	size := uint64(len(value))
	for index := range length {
		length[index] = byte(size >> (8 * (7 - index)))
	}
	_, _ = d.hash.Write(length[:])
	_, _ = d.hash.Write(value)
}

func (d semanticDigest) sum() string { return hex.EncodeToString(d.hash.Sum(nil)) }
