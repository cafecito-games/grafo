package golang

import (
	"crypto/sha256"
	"encoding/hex"
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"sort"
	"strconv"
	"sync"
)

// goFileEvidence is everything one Go source contributes to invalidation,
// extracted from a single parse.
type goFileEvidence struct {
	// Surface digests the declaration surface: the file without function bodies.
	// A file that does not parse has no trustworthy surface, so its full
	// contents are digested instead, which is conservative and never skips a
	// file whose extraction could change.
	Surface string
	// Types digests the type declarations that could name an interface, which is
	// the only evidence that has to stay repository wide. It is empty when the
	// file declares none.
	Types string
	// Imports are the file's import paths, sorted and deduplicated.
	Imports []string
}

// goFileSurface extracts one file's evidence. A parse failure is returned with a
// usable Surface so the caller can still digest the file, and with an error so
// the caller knows the imports are unknown.
func goFileSurface(path string, content []byte) (goFileEvidence, error) {
	contentKey := sha256.Sum256(content)
	if cached, ok := loadFileEvidence(contentKey); ok {
		return cached.evidence, cached.err
	}
	evidence, err := extractFileSurface(path, content)
	storeFileEvidence(contentKey, evidence, err)
	return evidence, err
}

func extractFileSurface(path string, content []byte) (goFileEvidence, error) {
	fset := token.NewFileSet()
	file, parseErr := goparser.ParseFile(fset, path, content, goparser.ParseComments|goparser.SkipObjectResolution)
	if parseErr != nil {
		digest := newSemanticDigest(goSemanticSurfaceVersion)
		digest.writeField("unparsed")
		digest.writeBytes(content)
		return goFileEvidence{Surface: digest.sum()}, parseErr
	}
	evidence := goFileEvidence{Surface: declarationSurfaceDigest(path, content)}
	imports := map[string]bool{}
	for _, specification := range file.Imports {
		value, err := strconv.Unquote(specification.Path.Value)
		if err != nil || value == "" {
			continue
		}
		imports[value] = true
	}
	evidence.Imports = make([]string, 0, len(imports))
	for value := range imports {
		evidence.Imports = append(evidence.Imports, value)
	}
	sort.Strings(evidence.Imports)

	types, err := goTypeUniverseSurface(path, content, fset, file)
	if err != nil {
		return evidence, err
	}
	evidence.Types = types
	return evidence, nil
}

// goTypeUniverseSurface digests the type declarations of a file that could name
// an interface.
//
// Only interface satisfaction is matched globally, and only a type declaration
// can introduce or change an interface. A declaration whose right-hand side is
// syntactically incapable of being an interface — a struct, array, map, channel,
// function, or pointer type — cannot change what any other package's concrete
// types implement, so it stays inside its own package's scope and reaches
// importers through the import closure instead. That is what makes a struct
// field edit a package-local event rather than a repository-wide one.
//
// An identifier or qualified identifier is kept, because `type Reader io.Reader`
// and `type Alias = other.Interface` both put an interface in the universe under
// a new name. An instantiated generic type is kept for the same reason.
func goTypeUniverseSurface(path string, content []byte, fset *token.FileSet, file *goast.File) (string, error) {
	var spans []byteSpan
	parsedFile := fset.File(file.Package)
	if parsedFile == nil {
		return "", errNoTokenFile
	}
	for _, declaration := range file.Decls {
		group, ok := declaration.(*goast.GenDecl)
		if !ok || group.Tok != token.TYPE {
			continue
		}
		for _, specification := range group.Specs {
			typeSpecification, ok := specification.(*goast.TypeSpec)
			if !ok || !mayNameInterface(typeSpecification.Type) {
				continue
			}
			start := parsedFile.Offset(typeSpecification.Pos())
			end := parsedFile.Offset(typeSpecification.End())
			if start < 0 || end < start || end > len(content) {
				return "", errUnorderedBody
			}
			spans = append(spans, byteSpan{start: start, end: end})
		}
	}
	if len(spans) == 0 {
		return "", nil
	}
	sort.Slice(spans, func(left, right int) bool { return spans[left].start < spans[right].start })
	digest := newSemanticDigest(goScopeModelVersion)
	digest.writeField(path)
	for _, span := range spans {
		digest.writeBytes(normalizedTokens(path, content[span.start:span.end]))
	}
	return digest.sum(), nil
}

// mayNameInterface reports whether a type expression could denote an interface.
// Being wrong in the permissive direction only widens invalidation; being wrong
// in the restrictive direction would leave a stale `implements` edge, so every
// form that cannot be ruled out syntactically is kept.
func mayNameInterface(expression goast.Expr) bool {
	switch value := expression.(type) {
	case *goast.InterfaceType:
		return true
	case *goast.Ident, *goast.SelectorExpr, *goast.IndexExpr, *goast.IndexListExpr:
		return true
	case *goast.ParenExpr:
		return mayNameInterface(value.X)
	default:
		return false
	}
}

// vendoredSurfaceDigest fingerprints a vendored file by its bytes.
//
// Vendored code is never extracted, so a vendored function body cannot change
// any graph node, and hashing instead of parsing costs only invalidation
// precision: a vendored body edit now invalidates the importers a declaration
// edit already did. In practice that precision is not lost at all, because a
// vendor tree changes through `go mod vendor`, which rewrites
// vendor/modules.txt — already fingerprinted in full by the repository key.
func vendoredSurfaceDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return "vendored:" + hex.EncodeToString(sum[:])
}

// fileEvidenceLimit bounds the in-process evidence cache. The cache only avoids
// reparsing identical bytes, so discarding it is always safe.
const fileEvidenceLimit = 1 << 16

type cachedFileEvidence struct {
	evidence goFileEvidence
	err      error
}

var (
	fileEvidenceMu    sync.Mutex
	fileEvidenceCache = map[[sha256.Size]byte]cachedFileEvidence{}
)

func loadFileEvidence(key [sha256.Size]byte) (cachedFileEvidence, bool) {
	fileEvidenceMu.Lock()
	defer fileEvidenceMu.Unlock()
	value, ok := fileEvidenceCache[key]
	return value, ok
}

func storeFileEvidence(key [sha256.Size]byte, evidence goFileEvidence, err error) {
	fileEvidenceMu.Lock()
	defer fileEvidenceMu.Unlock()
	if len(fileEvidenceCache) >= fileEvidenceLimit {
		fileEvidenceCache = map[[sha256.Size]byte]cachedFileEvidence{}
	}
	fileEvidenceCache[key] = cachedFileEvidence{evidence: evidence, err: err}
}
