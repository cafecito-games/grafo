package golang

import (
	"bytes"
	"context"
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
	"strings"
)

// goSemanticSurfaceVersion tags the encoding of the repository-wide Go
// declaration surface. Bump it whenever the encoding below changes so a
// previously indexed graph cannot be reused against a different fingerprint.
const goSemanticSurfaceVersion = "go-declaration-surface-v2"

// goPackageScopeVersion tags the encoding of the package scope key.
const goPackageScopeVersion = "go-package-scope-v2"

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
// so it is covered by that file's own content hash instead. A file that imports
// "C" is the exception: its preamble comment is compiler input that declares the
// C types cgo projects into Go, so every comment of such a file is retained.
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
	// cgo turns the preamble comment into Go declarations that other packages
	// type-check against, so no comment of a cgo file is droppable.
	keepComments := importsC(file)
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
		if tokenKind == token.COMMENT && !keepComments && !isGoDirectiveComment(literal) {
			continue
		}
		surface.WriteString(tokenKind.String())
		surface.WriteByte(0)
		surface.WriteString(literal)
		surface.WriteByte(0)
	}
	return surface.Bytes(), nil
}

// normalizedTokens emits the token stream of a Go source fragment, dropping
// comments. Emitting tokens rather than source text makes the result
// independent of layout, so reformatting a declaration cannot change it while a
// changed literal always does. Comments are dropped because this is only used
// for the type universe, where no comment can change which concrete types
// satisfy an interface; the directive comments that do select what the toolchain
// sees are retained by goDeclarationSurface, which covers the same bytes.
func normalizedTokens(path string, fragment []byte) []byte {
	fset := token.NewFileSet()
	scannedFile := fset.AddFile(path, fset.Base(), len(fragment))
	var lexer scanner.Scanner
	lexer.Init(scannedFile, fragment, nil, 0)
	var tokens bytes.Buffer
	tokens.Grow(len(fragment))
	for {
		_, tokenKind, literal := lexer.Scan()
		if tokenKind == token.EOF {
			break
		}
		tokens.WriteString(tokenKind.String())
		tokens.WriteByte(0)
		tokens.WriteString(literal)
		tokens.WriteByte(0)
	}
	return tokens.Bytes()
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

// importsC reports whether the file is a cgo file.
func importsC(file *goast.File) bool {
	for _, imported := range file.Imports {
		if imported.Path != nil && imported.Path.Value == `"C"` {
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

// packageScopeKey digests every Go source file in the directory that owns path.
// Statement-level evidence is attributed to the highest package-local callsite,
// which may live in a sibling file, so a package is the smallest scope whose
// contents can change how one of its files extracts.
//
// The digest is always computed from the bytes on disk. A stat fast path would
// make an unchanged size and modification time stand in for unchanged content,
// and this key is load bearing for invalidation: a sibling edit it missed would
// leave a stale graph.
func packageScopeKey(root, path string) (string, error) {
	directory := goPackageDirectory(path)
	own, err := packageContentDigest(root, directory)
	if err != nil {
		return "", err
	}
	// The packages this one imports, transitively, are the rest of what can
	// change how its files extract. They are digested by their declaration
	// surfaces rather than their contents, because a body in an imported package
	// is that package's own business.
	model, err := scopeModelFor(context.Background(), root)
	if err != nil {
		return "", err
	}
	digest := newSemanticDigest(goPackageScopeVersion)
	digest.writeField("own")
	digest.writeField(own)
	digest.writeField("closure")
	digest.writeField(model.Closure(directory))
	return digest.sum(), nil
}

// packageContentDigest digests every Go source file in a package directory.
func packageContentDigest(root, directory string) (string, error) {
	absolute := filepath.Join(root, filepath.FromSlash(directory))
	entries, err := os.ReadDir(absolute)
	if err != nil {
		// A directory that cannot be listed has no provable package scope. The
		// sentinel carries the exact condition, so recovering from it changes
		// the key and reparses the package.
		return goPackageScopeVersion + ":unreadable:" + err.Error(), nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isGoSourcePath(entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
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
	return digest.sum(), nil
}

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
