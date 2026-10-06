package typescript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	tstypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Resolution is intentionally limited to tracked repository inputs. The
// order below is the supported extension/index precedence; candidates at one
// position are never selected by directory or lexical order.
var moduleSuffixes = []string{".ts", ".tsx", ".d.ts", "/index.ts", "/index.tsx", "/index.d.ts"}

// javaScriptSourceSuffixes maps the extension of an emitted JavaScript file to
// the TypeScript sources that can produce it, in the order TypeScript selects
// them. Node16/NodeNext specifiers name the emitted file rather than the
// source, so `./mailbox.js` is how a module imports `mailbox.ts`. `.mjs` and
// `.cjs` are absent because `.mts` and `.cts` are not tracked inputs yet.
var javaScriptSourceSuffixes = map[string][]string{
	".js":  {".ts", ".tsx", ".d.ts"},
	".jsx": {".tsx"},
}

// assetSpecifierExtensions are the file types a bundler lets a module import
// for its bytes rather than for its bindings. None of them is a TypeScript
// module, so importing one is not a resolution failure. `.json` is absent
// because `resolveJsonModule` makes it a module TypeScript really does resolve.
var assetSpecifierExtensions = map[string]bool{
	".css": true, ".scss": true, ".sass": true, ".less": true, ".styl": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".avif": true, ".ico": true, ".bmp": true,
	".woff": true, ".woff2": true, ".eot": true, ".ttf": true, ".otf": true,
	".mp3": true, ".mp4": true, ".wav": true, ".webm": true, ".ogg": true,
}

// isAssetSpecifier reports whether a module specifier names a bundler asset.
func isAssetSpecifier(specifier string) bool {
	return assetSpecifierExtensions[strings.ToLower(filepath.Ext(specifier))]
}

const maxExportDepth = 128

type symbolRef struct {
	qualified string
	kind      graph.NodeKind
	owner     string
}

type exportRef struct {
	local     string
	specifier string
	imported  string
	namespace bool
	typeOnly  bool
}

type moduleInfo struct {
	path    string
	name    string
	locals  map[string]symbolRef
	methods map[string]map[string]symbolRef
	exports map[string][]exportRef
	stars   []exportRef
}

type compilerConfig struct {
	path             string
	baseURL          string
	paths            map[string][]string
	pathsBase        string
	rootDirs         []string
	moduleResolution string
	digest           string
}

type packageInfo struct {
	dir     string
	name    string
	exports any
	types   string
	module  string
	main    string
}

type moduleCatalog struct {
	root        string
	repoID      string
	modules     map[string]*moduleInfo
	moduleNames map[string]int
	modulePaths []string
	tracked     map[string]bool
	configs     map[string]compilerConfig
	packages    []packageInfo
	diagnostics []string
	// digest fingerprints only the cross-file resolution surface: module
	// identity, the local declarations another module can resolve through,
	// class and interface members, exports, and the full contents of the
	// package and compiler manifests. It deliberately excludes statement
	// bodies and body-level declarations, which cannot change how another
	// module resolves.
	digest string
}

// resolutionSurfaceVersion tags the encoding of the resolution surface digest.
const resolutionSurfaceVersion = "typescript-resolution-surface-v3"

func (c *moduleCatalog) clone() *moduleCatalog {
	copyCatalog := *c
	copyCatalog.modules = make(map[string]*moduleInfo, len(c.modules))
	for path, module := range c.modules {
		copyCatalog.modules[path] = module
	}
	copyCatalog.moduleNames = make(map[string]int, len(c.moduleNames))
	for name, count := range c.moduleNames {
		copyCatalog.moduleNames[name] = count
	}
	copyCatalog.tracked = make(map[string]bool, len(c.tracked))
	for path, tracked := range c.tracked {
		copyCatalog.tracked[path] = tracked
	}
	copyCatalog.modulePaths = append([]string(nil), c.modulePaths...)
	copyCatalog.packages = append([]packageInfo(nil), c.packages...)
	copyCatalog.configs = make(map[string]compilerConfig, len(c.configs))
	for path, config := range c.configs {
		copyCatalog.configs[path] = config
	}
	copyCatalog.diagnostics = append([]string(nil), c.diagnostics...)
	return &copyCatalog
}

var (
	// `type` is a declarator in its own right, for `export type Alias = ...`,
	// and is listed last so the leading `(type\s+)?` modifier is still
	// preferred where both could match. It cannot swallow `export type { ... }`
	// because a brace is not an identifier. The declarator is followed by `\b`
	// rather than `\s+` so a generator star binds to it without `classFoo`
	// reading as a declarator plus a name.
	exportedDeclarationPattern = regexp.MustCompile(`(?m)\bexport\s+(type\s+)?(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?(class|interface|function|const|let|var|type)\b\s*\*?\s*([A-Za-z_$][A-Za-z0-9_$]*)`)
	defaultDeclarationPattern  = regexp.MustCompile(`(?m)\bexport\s+default\s+(?:async\s+)?(?:abstract\s+)?(?:class|function)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	defaultIdentifierPattern   = regexp.MustCompile(`(?m)\bexport\s+default\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*;?\s*$`)
	exportListPattern          = regexp.MustCompile(`(?ms)\bexport\s+(type\s+)?\{([^}]*)\}\s*(?:from\s*["']([^"']+)["'])?`)
	exportStarPattern          = regexp.MustCompile(`(?m)\bexport\s+(type\s+)?\*\s*(?:as\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*)?from\s*["']([^"']+)["']`)
)

func buildModuleCatalog(ctx context.Context, input parserapi.Input) (*moduleCatalog, error) {
	catalog := &moduleCatalog{root: input.Root, repoID: input.RepoID, modules: map[string]*moduleInfo{}, moduleNames: map[string]int{},
		tracked: map[string]bool{}, configs: map[string]compilerConfig{}}
	if catalog.root == "" {
		return catalog, nil
	}
	paths, err := repositoryResolutionFiles(ctx, catalog.root)
	if err != nil {
		return nil, err
	}
	surface := newSurfaceDigest(resolutionSurfaceVersion)
	for _, path := range paths {
		catalog.tracked[path] = true
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		surface.writeField(path)
		body, readErr := os.ReadFile(filepath.Join(catalog.root, filepath.FromSlash(path)))
		if readErr != nil {
			catalog.diagnostics = append(catalog.diagnostics, fmt.Sprintf("read resolution input %s: %v", path, readErr))
			surface.writeField("unreadable:" + readErr.Error())
			continue
		}
		switch {
		case isTypeScriptPath(path):
			info, parseErr := scanModuleCached(path, body)
			if parseErr != nil {
				catalog.diagnostics = append(catalog.diagnostics, fmt.Sprintf("scan module %s: %v", path, parseErr))
				// An unscannable module has no provable surface, so its full
				// contents stay part of the fingerprint.
				surface.writeField("unscannable")
				surface.writeBytes(body)
				continue
			}
			catalog.modules[path] = info
			catalog.moduleNames[info.name]++
			catalog.modulePaths = append(catalog.modulePaths, path)
			surface.writeField("module")
			writeModuleSurface(surface, info)
		case strings.EqualFold(filepath.Base(path), "package.json"):
			catalog.loadPackage(path, body)
			surface.writeField("manifest")
			surface.writeBytes(body)
		default:
			// Compiler configuration selects module resolution for every
			// importer, so it is fingerprinted in full.
			surface.writeField("configuration")
			surface.writeBytes(body)
		}
	}
	sort.Strings(catalog.modulePaths)
	sort.Slice(catalog.packages, func(i, j int) bool {
		if len(catalog.packages[i].name) != len(catalog.packages[j].name) {
			return len(catalog.packages[i].name) > len(catalog.packages[j].name)
		}
		return catalog.packages[i].name < catalog.packages[j].name
	})
	catalog.digest = surface.sum()
	return catalog, nil
}

// writeModuleSurface encodes the parts of a scanned module that another module
// can resolve through. Ordering is canonical so an unchanged surface always
// produces an unchanged digest.
func writeModuleSurface(digest surfaceDigest, info *moduleInfo) {
	digest.writeField(info.name)
	digest.writeField("locals")
	for _, name := range resolutionLocalNames(info) {
		digest.writeField(name)
		symbol, declared := info.locals[name]
		if !declared {
			// An exported name with no matching declaration resolves to an
			// explicit diagnostic rather than a symbol, so the absence is part
			// of the surface and must change the digest when it is filled in.
			digest.writeField("undeclared")
			continue
		}
		digest.writeField("declared")
		writeSymbolSurface(digest, symbol)
	}
	digest.writeField("methods")
	for _, owner := range sortedKeys(info.methods) {
		digest.writeField(owner)
		members := info.methods[owner]
		for _, member := range sortedKeys(members) {
			digest.writeField(member)
			writeSymbolSurface(digest, members[member])
		}
	}
	digest.writeField("exports")
	for _, name := range sortedKeys(info.exports) {
		digest.writeField(name)
		for _, reference := range info.exports[name] {
			writeExportSurface(digest, reference)
		}
	}
	digest.writeField("stars")
	for _, reference := range info.stars {
		writeExportSurface(digest, reference)
	}
}

// resolutionLocalNames returns the sorted local names of a module that another
// module can resolve through, which is a strict subset of the flat symbol table
// scanModule builds.
//
// moduleInfo.locals is keyed by bare identifier and collected at any depth, so
// it also contains declarations made inside function bodies. Only two paths ever
// read another module's locals:
//
//   - resolveExport looks up locals[ref.local] for the names a module's exports
//     name, so every exported local name belongs to the surface. The lookup
//     reads the finished map, so a body-level declaration that shadows an
//     exported name is captured as the value under that name rather than as a
//     separate entry; narrowing by name therefore preserves shadowing exactly.
//   - resolveMember scans every module's locals for a qualified match and then
//     requires methods[localName][member], and member reaches methods through a
//     qualified name directly. Only classes and interfaces get a methods entry,
//     so every name that owns one belongs to the surface too.
//
// Every other local is read only through the extractor's own module
// (e.info.locals), which is scanned from that file's bytes and is therefore
// already covered by the file's own content hash.
//
// A non-function variable's qualified name still embeds its declaration line, so
// an edit that moves an exported variable does change the surface. That is not
// conservatism: the variable's node identity changes with it, so an importer's
// export edge genuinely points somewhere new and must be reparsed.
func resolutionLocalNames(info *moduleInfo) []string {
	names := make(map[string]bool, len(info.exports)+len(info.methods))
	for _, refs := range info.exports {
		for _, ref := range refs {
			if ref.local != "" {
				names[ref.local] = true
			}
		}
	}
	for owner := range info.methods {
		names[owner] = true
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func writeSymbolSurface(digest surfaceDigest, symbol symbolRef) {
	digest.writeField(symbol.qualified)
	digest.writeField(string(symbol.kind))
	digest.writeField(symbol.owner)
}

func writeExportSurface(digest surfaceDigest, reference exportRef) {
	digest.writeField(reference.local)
	digest.writeField(reference.specifier)
	digest.writeField(reference.imported)
	digest.writeField(strconv.FormatBool(reference.namespace))
	digest.writeField(strconv.FormatBool(reference.typeOnly))
}

func sortedKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// surfaceDigest accumulates length-delimited fields so no concatenation of
// inputs can collide with a different field layout.
type surfaceDigest struct {
	hash hash.Hash
}

func newSurfaceDigest(version string) surfaceDigest {
	digest := surfaceDigest{hash: sha256.New()}
	digest.writeField(version)
	return digest
}

func (d surfaceDigest) writeField(value string) { d.writeBytes([]byte(value)) }

func (d surfaceDigest) writeBytes(value []byte) {
	var length [8]byte
	size := uint64(len(value))
	for index := range length {
		length[index] = byte(size >> (8 * (7 - index)))
	}
	_, _ = d.hash.Write(length[:])
	_, _ = d.hash.Write(value)
}

func (d surfaceDigest) sum() string { return hex.EncodeToString(d.hash.Sum(nil)) }

func repositoryResolutionFiles(ctx context.Context, root string) ([]string, error) {
	command := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if output, err := command.Output(); err == nil {
		var result []string
		for _, raw := range bytes.Split(output, []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			path := filepath.ToSlash(string(raw))
			base := strings.ToLower(filepath.Base(path))
			if isTypeScriptPath(path) || base == "tsconfig.json" || strings.HasPrefix(base, "tsconfig.") && strings.HasSuffix(base, ".json") || base == "package.json" {
				info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
				if statErr == nil && info.Mode().IsRegular() {
					result = append(result, path)
				}
			}
		}
		sort.Strings(result)
		return result, nil
	}
	var result []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && ignoredResolutionDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		base := strings.ToLower(filepath.Base(relative))
		if isTypeScriptPath(relative) || base == "tsconfig.json" || strings.HasPrefix(base, "tsconfig.") && strings.HasSuffix(base, ".json") || base == "package.json" {
			result = append(result, relative)
		}
		return nil
	})
	sort.Strings(result)
	return result, err
}

func ignoredResolutionDirectory(name string) bool {
	switch name {
	case ".git", ".grafo", ".worktrees", "node_modules", "vendor", "dist", "build", "coverage", ".next", ".turbo":
		return true
	default:
		return false
	}
}

func isTypeScriptPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".tsx")
}

func canonicalModuleName(path string) string {
	path = filepath.ToSlash(path)
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".d.ts"):
		path = path[:len(path)-len(".d.ts")]
	default:
		path = strings.TrimSuffix(path, filepath.Ext(path))
	}
	path = strings.TrimSuffix(path, "/index")
	return strings.TrimPrefix(path, "./")
}

// moduleScanLimit bounds the in-process scan cache. An entry retains a whole
// module symbol table, and the cache is shared by every root a long-lived
// process indexes, so the bound is about one large repository's worth rather
// than the content-addressed ceiling. Overflowing it only costs a rescan: the
// cache exists so the catalog built for the workspace key and the catalog built
// for parsing do not scan the same bytes twice.
const moduleScanLimit = 1 << 13

type moduleScanKey struct {
	path    string
	content [sha256.Size]byte
}

var (
	moduleScanMu    sync.Mutex
	moduleScanCache = map[moduleScanKey]*moduleInfo{}
)

// scanModuleCached memoizes module scans by exact content. A scanned moduleInfo
// is never mutated after it is built, so sharing one across catalogs is safe.
func scanModuleCached(path string, content []byte) (*moduleInfo, error) {
	key := moduleScanKey{path: path, content: sha256.Sum256(content)}
	moduleScanMu.Lock()
	cached, ok := moduleScanCache[key]
	moduleScanMu.Unlock()
	if ok {
		return cached, nil
	}
	info, err := scanModule(path, content)
	if err != nil {
		return nil, err
	}
	moduleScanMu.Lock()
	if len(moduleScanCache) >= moduleScanLimit {
		moduleScanCache = map[moduleScanKey]*moduleInfo{}
	}
	moduleScanCache[key] = info
	moduleScanMu.Unlock()
	return info, nil
}

func scanModule(path string, content []byte) (*moduleInfo, error) {
	info := &moduleInfo{path: path, name: canonicalModuleName(path), locals: map[string]symbolRef{}, methods: map[string]map[string]symbolRef{}, exports: map[string][]exportRef{}}
	parser := treesitter.NewParser()
	defer parser.Close()
	language := tstypescript.LanguageTypescript()
	if strings.EqualFold(filepath.Ext(path), ".tsx") {
		language = tstypescript.LanguageTSX()
	}
	if err := parser.SetLanguage(treesitter.NewLanguage(language)); err != nil {
		return nil, err
	}
	tree := parser.Parse(content, nil)
	if tree == nil {
		return nil, fmt.Errorf("parser returned no syntax tree")
	}
	defer tree.Close()
	collectModuleDeclarations(tree.RootNode(), content, info, "")
	collectModuleExports(info, string(content))
	collectAnonymousDefaultExports(tree.RootNode(), content, info)
	return info, nil
}

func collectModuleExports(info *moduleInfo, text string) {
	for _, match := range exportedDeclarationPattern.FindAllStringSubmatch(text, -1) {
		name := match[3]
		// A type alias is type-only whether or not the modifier is spelled out.
		typeOnly := strings.TrimSpace(match[1]) != "" || match[2] == "type"
		info.exports[name] = append(info.exports[name], exportRef{local: name, typeOnly: typeOnly})
	}
	for _, match := range defaultDeclarationPattern.FindAllStringSubmatch(text, -1) {
		if match[1] == "extends" || match[1] == "implements" {
			continue
		}
		info.exports["default"] = append(info.exports["default"], exportRef{local: match[1]})
	}
	for _, match := range defaultIdentifierPattern.FindAllStringSubmatch(text, -1) {
		if match[1] != "class" && match[1] != "function" {
			info.exports["default"] = append(info.exports["default"], exportRef{local: match[1]})
		}
	}
	for _, match := range exportListPattern.FindAllStringSubmatch(text, -1) {
		listTypeOnly := strings.TrimSpace(match[1]) != ""
		for _, item := range strings.Split(match[2], ",") {
			item = strings.TrimSpace(item)
			itemTypeOnly := listTypeOnly
			if strings.HasPrefix(item, "type ") {
				itemTypeOnly = true
				item = strings.TrimSpace(strings.TrimPrefix(item, "type "))
			}
			parts := regexp.MustCompile(`\s+as\s+`).Split(item, 2)
			if len(parts) == 0 || !isIdentifier(strings.TrimSpace(parts[0])) {
				continue
			}
			imported, exported := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[0])
			if len(parts) == 2 {
				exported = strings.TrimSpace(parts[1])
			}
			ref := exportRef{local: imported, typeOnly: itemTypeOnly}
			if match[3] != "" {
				ref.local, ref.specifier, ref.imported = "", match[3], imported
			}
			info.exports[exported] = append(info.exports[exported], ref)
		}
	}
	for _, match := range exportStarPattern.FindAllStringSubmatch(text, -1) {
		ref := exportRef{specifier: match[3], imported: "*", typeOnly: strings.TrimSpace(match[1]) != ""}
		if match[2] != "" {
			ref.namespace = true
			info.exports[match[2]] = append(info.exports[match[2]], ref)
		} else {
			info.stars = append(info.stars, ref)
		}
	}
}

func collectAnonymousDefaultExports(node *treesitter.Node, source []byte, info *moduleInfo) {
	if node == nil {
		return
	}
	if node.Kind() == "export_statement" {
		if local := anonymousDefaultLocal(node, source); local != "" {
			info.exports["default"] = append(info.exports["default"], exportRef{local: local})
		}
		return
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		collectAnonymousDefaultExports(node.NamedChild(i), source, info)
	}
}

func anonymousDefaultLocal(export *treesitter.Node, source []byte) string {
	if export == nil || export.Kind() != "export_statement" || !strings.Contains(nodeText(export, source), "default") {
		return ""
	}
	for i := uint(0); i < export.NamedChildCount(); i++ {
		declaration := export.NamedChild(i)
		switch declaration.Kind() {
		case "class", "class_declaration", "abstract_class_declaration", "function_declaration", "function_expression", "generator_function", "generator_function_declaration":
			if nodeText(declaration.ChildByFieldName("name"), source) == "" {
				return fmt.Sprintf("anonymous@%d", declaration.StartPosition().Row+1)
			}
		}
	}
	return ""
}

// namedDeclarationKind reports the graph kind a type-level declaration's name
// resolves to. A type alias resolves like an interface: a named type whose
// object members other modules reach through it. Only a class declaration can
// be anonymous, so only it gets a positional name.
func namedDeclarationKind(kind string) (graph.NodeKind, bool) {
	switch kind {
	case "class", "class_declaration", "abstract_class_declaration":
		return graph.KindClass, true
	case "interface_declaration":
		return graph.KindInterface, true
	case "type_alias_declaration":
		return graph.KindType, true
	default:
		return "", false
	}
}

func collectModuleDeclarations(node *treesitter.Node, source []byte, info *moduleInfo, container string) {
	if node == nil {
		return
	}
	kind := node.Kind()
	if nodeKind, named := namedDeclarationKind(kind); named {
		name := nodeText(node.ChildByFieldName("name"), source)
		if name == "" && nodeKind == graph.KindClass {
			name = fmt.Sprintf("anonymous@%d", node.StartPosition().Row+1)
		}
		if name != "" {
			qualified := info.name + "." + name
			info.locals[name] = symbolRef{qualified: qualified, kind: nodeKind, owner: info.path}
			if info.methods[name] == nil {
				info.methods[name] = map[string]symbolRef{}
			}
			for i := uint(0); i < node.NamedChildCount(); i++ {
				collectModuleDeclarations(node.NamedChild(i), source, info, name)
			}
			return
		}
	}
	if container != "" && (kind == "method_definition" || kind == "method_signature" || kind == "abstract_method_signature") {
		name := nodeText(node.ChildByFieldName("name"), source)
		if name != "" {
			info.methods[container][name] = symbolRef{qualified: info.name + "." + container + "." + name, kind: graph.KindMethod, owner: info.path}
		}
		return
	}
	// function_signature is the ambient form, `declare function f(): void`,
	// which exports a name that importers can resolve like any other.
	if container == "" && (kind == "function_declaration" || kind == "generator_function_declaration" || kind == "function_expression" || kind == "generator_function" || kind == "function_signature") {
		name := nodeText(node.ChildByFieldName("name"), source)
		if name == "" {
			name = fmt.Sprintf("anonymous@%d", node.StartPosition().Row+1)
		}
		if name != "" {
			info.locals[name] = symbolRef{qualified: info.name + "." + name, kind: graph.KindFunction, owner: info.path}
		}
	}
	if container == "" && kind == "variable_declarator" {
		name := nodeText(node.ChildByFieldName("name"), source)
		value := node.ChildByFieldName("value")
		if isIdentifier(name) {
			kind := graph.KindVariable
			qualified := fmt.Sprintf("%s.%s@%d", info.name, name, node.StartPosition().Row+1)
			if value != nil && (value.Kind() == "arrow_function" || value.Kind() == "function_expression" || value.Kind() == "generator_function") {
				kind = graph.KindFunction
				qualified = info.name + "." + name
			}
			info.locals[name] = symbolRef{qualified: qualified, kind: kind, owner: info.path}
		}
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		collectModuleDeclarations(node.NamedChild(i), source, info, container)
	}
}

func nodeText(node *treesitter.Node, source []byte) string {
	if node == nil {
		return ""
	}
	return strings.TrimSpace(node.Utf8Text(source))
}

func (c *moduleCatalog) moduleID(info *moduleInfo) string {
	return graph.NodeID(graph.KindModule, c.repoID+":"+info.path)
}

func (c *moduleCatalog) moduleForPath(path string) *moduleInfo {
	return c.modules[filepath.ToSlash(path)]
}

func (c *moduleCatalog) ambiguousSymbol(symbol symbolRef) bool {
	if symbol.owner == "" {
		return false
	}
	module := c.modules[symbol.owner]
	return module != nil && c.moduleNames[module.name] > 1
}

func (c *moduleCatalog) resolveModule(fromPath, specifier string) (*moduleInfo, string) {
	if specifier == "" {
		return nil, "empty module specifier"
	}
	config := c.configFor(fromPath)
	if strings.HasPrefix(specifier, ".") {
		base := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(fromPath), filepath.FromSlash(specifier))))
		if info := c.resolvePath(base); info != nil {
			return info, ""
		}
		var rootCandidates []*moduleInfo
		for _, rootDir := range config.rootDirs {
			fromRoot := strings.TrimSuffix(filepath.ToSlash(rootDir), "/")
			fromDir := filepath.ToSlash(filepath.Dir(fromPath))
			if fromDir == fromRoot || strings.HasPrefix(fromDir, fromRoot+"/") {
				relativeDir := strings.TrimPrefix(strings.TrimPrefix(fromDir, fromRoot), "/")
				for _, targetRoot := range config.rootDirs {
					candidate := filepath.ToSlash(filepath.Clean(filepath.Join(targetRoot, relativeDir, filepath.FromSlash(specifier))))
					if info := c.resolvePath(candidate); info != nil {
						rootCandidates = append(rootCandidates, info)
					}
				}
			}
		}
		rootCandidates = uniqueModules(rootCandidates)
		if len(rootCandidates) == 1 {
			return rootCandidates[0], ""
		}
		if len(rootCandidates) > 1 {
			return nil, fmt.Sprintf("relative module %q is ambiguous across rootDirs (%d candidates)", specifier, len(rootCandidates))
		}
		return nil, "relative module is not tracked"
	}
	if info, diagnostic := c.resolvePathsAlias(config, specifier); info != nil || diagnostic != "" {
		return info, diagnostic
	}
	if config.baseURL != "" {
		if info := c.resolvePath(filepath.ToSlash(filepath.Join(config.baseURL, filepath.FromSlash(specifier)))); info != nil {
			return info, ""
		}
	}
	if info := c.resolvePackage(specifier); info != nil {
		return info, ""
	}
	return nil, "external module is not tracked"
}

func uniqueModules(values []*moduleInfo) []*moduleInfo {
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		if value != nil && !seen[value.path] {
			seen[value.path] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].path < result[j].path })
	return result
}

func (c *moduleCatalog) resolvePath(base string) *moduleInfo {
	base = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(base)), "./")
	if isTypeScriptPath(base) {
		return c.modules[base]
	}
	// A specifier that names an emitted JavaScript file resolves only to the
	// TypeScript source that produces it; a directory candidate would be a
	// guess, because `./mailbox.js/index.ts` is not a path TypeScript selects.
	if stem, suffixes := javaScriptSourceStem(base); suffixes != nil {
		for _, suffix := range suffixes {
			if info := c.modules[stem+suffix]; info != nil {
				return info
			}
		}
		return nil
	}
	for _, suffix := range moduleSuffixes {
		if info := c.modules[base+suffix]; info != nil {
			return info
		}
	}
	return nil
}

// javaScriptSourceStem splits a path that names an emitted JavaScript file into
// the stem its TypeScript source shares and the suffixes to try against it. The
// suffixes are nil when the path names something else.
func javaScriptSourceStem(base string) (string, []string) {
	extension := strings.ToLower(filepath.Ext(base))
	suffixes, emitted := javaScriptSourceSuffixes[extension]
	if !emitted {
		return "", nil
	}
	return base[:len(base)-len(extension)], suffixes
}

func (c *moduleCatalog) resolvePathsAlias(config compilerConfig, specifier string) (*moduleInfo, string) {
	keys := make([]string, 0, len(config.paths))
	for key := range config.paths {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		iExact, jExact := !strings.Contains(keys[i], "*"), !strings.Contains(keys[j], "*")
		if iExact != jExact {
			return iExact
		}
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys {
		capture, ok := matchPathPattern(key, specifier)
		if !ok {
			continue
		}
		for _, target := range config.paths[key] {
			target = strings.ReplaceAll(target, "*", capture)
			base := config.baseURL
			if base == "" {
				base = config.pathsBase
			}
			if info := c.resolvePath(filepath.ToSlash(filepath.Join(base, filepath.FromSlash(target)))); info != nil {
				return info, ""
			}
		}
		return nil, fmt.Sprintf("tsconfig path %q has no tracked target", key)
	}
	return nil, ""
}

func matchPathPattern(pattern, value string) (string, bool) {
	if !strings.Contains(pattern, "*") {
		return "", pattern == value
	}
	prefix, suffix, _ := strings.Cut(pattern, "*")
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) || len(value) < len(prefix)+len(suffix) {
		return "", false
	}
	return value[len(prefix) : len(value)-len(suffix)], true
}

func (c *moduleCatalog) resolveExport(module *moduleInfo, name string) ([]symbolRef, string) {
	return c.resolveExportDepth(module, name, map[string]bool{}, 0)
}

func (c *moduleCatalog) resolveExportDepth(module *moduleInfo, name string, active map[string]bool, depth int) ([]symbolRef, string) {
	if module == nil {
		return nil, "module is unresolved"
	}
	if depth >= maxExportDepth {
		return nil, "export traversal exceeded bounded depth"
	}
	key := module.path + "\x00" + name
	if active[key] {
		return nil, fmt.Sprintf("cyclic export %s from %s", name, module.name)
	}
	active[key] = true
	defer delete(active, key)
	var result []symbolRef
	var diagnostics []string
	for _, ref := range module.exports[name] {
		switch {
		case ref.local != "":
			if symbol, ok := module.locals[ref.local]; ok {
				result = append(result, symbol)
			} else {
				diagnostics = append(diagnostics, fmt.Sprintf("export %s is absent from %s", ref.local, module.name))
			}
		case ref.namespace:
			target, diagnostic := c.resolveModule(module.path, ref.specifier)
			if target != nil {
				result = append(result, symbolRef{qualified: target.name, kind: graph.KindModule, owner: target.path})
			} else if diagnostic != "" {
				diagnostics = append(diagnostics, diagnostic)
			}
		default:
			target, diagnostic := c.resolveModule(module.path, ref.specifier)
			if target == nil {
				diagnostics = append(diagnostics, diagnostic)
				continue
			}
			resolved, nested := c.resolveExportDepth(target, ref.imported, active, depth+1)
			result = append(result, resolved...)
			if nested != "" {
				diagnostics = append(diagnostics, nested)
			}
		}
	}
	if name != "default" {
		for _, ref := range module.stars {
			target, diagnostic := c.resolveModule(module.path, ref.specifier)
			if target == nil {
				diagnostics = append(diagnostics, diagnostic)
				continue
			}
			resolved, nested := c.resolveExportDepth(target, name, active, depth+1)
			result = append(result, resolved...)
			if nested != "" && len(resolved) == 0 {
				diagnostics = append(diagnostics, nested)
			}
		}
	}
	result = uniqueSymbols(result)
	if len(result) > 1 {
		return nil, fmt.Sprintf("ambiguous export %s from %s (%d candidates)", name, module.name, len(result))
	}
	if len(result) == 0 && len(diagnostics) == 0 {
		diagnostics = append(diagnostics, fmt.Sprintf("export %s is absent from %s", name, module.name))
	}
	return result, strings.Join(uniqueStrings(diagnostics), "; ")
}

func uniqueSymbols(values []symbolRef) []symbolRef {
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		key := string(value.kind) + "\x00" + value.qualified + "\x00" + value.owner
		if !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].qualified < result[j].qualified })
	return result
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func (c *moduleCatalog) member(symbol symbolRef, member string) (symbolRef, bool) {
	if symbol.owner != "" {
		info := c.modules[symbol.owner]
		if info == nil || c.moduleNames[info.name] > 1 {
			return symbolRef{}, false
		}
		typeName := strings.TrimPrefix(symbol.qualified, info.name+".")
		memberSymbol, ok := info.methods[typeName][member]
		return memberSymbol, ok
	}
	parts := strings.Split(symbol.qualified, ".")
	if len(parts) < 2 {
		return symbolRef{}, false
	}
	moduleName := strings.Join(parts[:len(parts)-1], ".")
	typeName := parts[len(parts)-1]
	var result symbolRef
	matches := 0
	for _, info := range c.modules {
		if info.name == moduleName {
			if memberSymbol, ok := info.methods[typeName][member]; ok {
				result = memberSymbol
				matches++
			}
		}
	}
	return result, matches == 1
}

func (c *moduleCatalog) loadPackage(path string, content []byte) {
	var raw struct {
		Name    string `json:"name"`
		Exports any    `json:"exports"`
		Types   string `json:"types"`
		Module  string `json:"module"`
		Main    string `json:"main"`
	}
	if err := json.Unmarshal(stripJSONComments(content), &raw); err != nil {
		c.diagnostics = append(c.diagnostics, fmt.Sprintf("invalid package metadata %s: %v", path, err))
		return
	}
	if raw.Name == "" {
		return
	}
	c.packages = append(c.packages, packageInfo{dir: filepath.ToSlash(filepath.Dir(path)), name: raw.Name, exports: raw.Exports, types: raw.Types, module: raw.Module, main: raw.Main})
}

func (c *moduleCatalog) resolvePackage(specifier string) *moduleInfo {
	for _, pkg := range c.packages {
		if specifier != pkg.name && !strings.HasPrefix(specifier, pkg.name+"/") {
			continue
		}
		subpath := strings.TrimPrefix(specifier, pkg.name)
		key := "."
		if subpath != "" {
			key = "." + subpath
		}
		for _, target := range packageExportTargets(pkg.exports, key) {
			if info := c.resolvePath(filepath.ToSlash(filepath.Join(pkg.dir, filepath.FromSlash(strings.TrimPrefix(target, "./"))))); info != nil {
				return info
			}
		}
		if key == "." {
			for _, target := range []string{pkg.types, pkg.module, pkg.main, "index"} {
				if target != "" {
					if info := c.resolvePath(filepath.ToSlash(filepath.Join(pkg.dir, filepath.FromSlash(strings.TrimPrefix(target, "./"))))); info != nil {
						return info
					}
				}
			}
		} else if info := c.resolvePath(filepath.ToSlash(filepath.Join(pkg.dir, filepath.FromSlash(strings.TrimPrefix(key, "./"))))); info != nil {
			return info
		}
	}
	return nil
}

func packageExportTargets(value any, key string) []string {
	switch current := value.(type) {
	case string:
		if key == "." {
			return []string{current}
		}
	case []any:
		var result []string
		for _, item := range current {
			result = append(result, packageExportTargets(item, ".")...)
		}
		return result
	case map[string]any:
		if selected, ok := current[key]; ok {
			return packageExportTargets(selected, ".")
		}
		for _, condition := range []string{"types", "import", "default", "require"} {
			if selected, ok := current[condition]; ok {
				if targets := packageExportTargets(selected, "."); len(targets) > 0 {
					return targets
				}
			}
		}
	}
	return nil
}

func (c *moduleCatalog) configFor(sourcePath string) compilerConfig {
	directory := filepath.ToSlash(filepath.Dir(sourcePath))
	for {
		configPath := "tsconfig.json"
		if directory != "." && directory != "" {
			configPath = directory + "/tsconfig.json"
		}
		if c.tracked[configPath] {
			config, ok := c.configs[configPath]
			if ok {
				return config
			}
			config, err := c.loadConfig(configPath, map[string]bool{})
			if err != nil {
				c.diagnostics = append(c.diagnostics, err.Error())
				return compilerConfig{path: configPath}
			}
			c.configs[configPath] = config
			return config
		}
		if directory == "." || directory == "" {
			break
		}
		directory = filepath.ToSlash(filepath.Dir(directory))
	}
	return compilerConfig{}
}

func (c *moduleCatalog) loadConfig(path string, active map[string]bool) (compilerConfig, error) {
	path = filepath.ToSlash(filepath.Clean(path))
	if !c.tracked[path] {
		return compilerConfig{path: path}, fmt.Errorf("tsconfig %s is not a tracked resolution input", path)
	}
	if active[path] {
		return compilerConfig{path: path}, fmt.Errorf("cyclic tsconfig extends at %s", path)
	}
	active[path] = true
	defer delete(active, path)
	content, err := os.ReadFile(filepath.Join(c.root, filepath.FromSlash(path)))
	if err != nil {
		return compilerConfig{path: path}, fmt.Errorf("read tsconfig %s: %w", path, err)
	}
	var raw struct {
		Extends         string `json:"extends"`
		CompilerOptions struct {
			BaseURL          *string             `json:"baseUrl"`
			Paths            map[string][]string `json:"paths"`
			RootDirs         []string            `json:"rootDirs"`
			ModuleResolution *string             `json:"moduleResolution"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(stripJSONComments(content), &raw); err != nil {
		return compilerConfig{path: path}, fmt.Errorf("invalid tsconfig %s: %w", path, err)
	}
	config := compilerConfig{path: path, paths: map[string][]string{}}
	if raw.Extends != "" {
		if !strings.HasPrefix(raw.Extends, ".") {
			return config, fmt.Errorf("unsupported non-local tsconfig extends %q in %s", raw.Extends, path)
		}
		parentPath := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(raw.Extends))))
		if filepath.Ext(parentPath) == "" {
			parentPath += ".json"
		}
		parent, err := c.loadConfig(parentPath, active)
		if err != nil {
			return config, err
		}
		config = parent
		config.path = path
	}
	configDir := filepath.ToSlash(filepath.Dir(path))
	if raw.CompilerOptions.BaseURL != nil {
		config.baseURL = filepath.ToSlash(filepath.Clean(filepath.Join(configDir, filepath.FromSlash(*raw.CompilerOptions.BaseURL))))
	}
	if raw.CompilerOptions.Paths != nil {
		config.paths = raw.CompilerOptions.Paths
		config.pathsBase = configDir
	}
	if raw.CompilerOptions.RootDirs != nil {
		config.rootDirs = make([]string, 0, len(raw.CompilerOptions.RootDirs))
		for _, rootDir := range raw.CompilerOptions.RootDirs {
			config.rootDirs = append(config.rootDirs, filepath.ToSlash(filepath.Clean(filepath.Join(configDir, filepath.FromSlash(rootDir)))))
		}
	}
	if raw.CompilerOptions.ModuleResolution != nil {
		config.moduleResolution = *raw.CompilerOptions.ModuleResolution
	}
	hash := sha256.Sum256(content)
	config.digest = hex.EncodeToString(hash[:])
	return config, nil
}

func stripJSONComments(content []byte) []byte {
	// tsconfig is JSONC. Preserve quoted strings while removing line/block
	// comments and trailing commas before standard JSON decoding.
	var output bytes.Buffer
	inString, escaped := false, false
	for i := 0; i < len(content); i++ {
		ch := content[i]
		if inString {
			output.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			output.WriteByte(ch)
			continue
		}
		if ch == '/' && i+1 < len(content) && content[i+1] == '/' {
			for i < len(content) && content[i] != '\n' {
				i++
			}
			output.WriteByte('\n')
			continue
		}
		if ch == '/' && i+1 < len(content) && content[i+1] == '*' {
			i += 2
			for i+1 < len(content) && (content[i] != '*' || content[i+1] != '/') {
				i++
			}
			i++
			continue
		}
		output.WriteByte(ch)
	}
	withoutComments := output.Bytes()
	output.Reset()
	inString, escaped = false, false
	for index := 0; index < len(withoutComments); index++ {
		ch := withoutComments[index]
		if inString {
			output.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			output.WriteByte(ch)
			continue
		}
		if ch == ',' {
			lookahead := index + 1
			for lookahead < len(withoutComments) && (withoutComments[lookahead] == ' ' || withoutComments[lookahead] == '\t' || withoutComments[lookahead] == '\r' || withoutComments[lookahead] == '\n') {
				lookahead++
			}
			if lookahead < len(withoutComments) && (withoutComments[lookahead] == '}' || withoutComments[lookahead] == ']') {
				continue
			}
		}
		output.WriteByte(ch)
	}
	return output.Bytes()
}
