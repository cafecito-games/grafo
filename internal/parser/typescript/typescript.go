package typescript

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	tstypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

type Parser struct {
	// buildMu serializes module catalog construction so concurrent parses in one
	// indexing run share a single repository scan.
	buildMu       sync.Mutex
	cacheMu       sync.Mutex
	cacheRoot     string
	cacheKey      string
	cacheCatalog  *moduleCatalog
	catalogLoads  atomic.Int64
	cacheHits     atomic.Int64
	cachedModules atomic.Int64
}

type ResolutionMetrics struct {
	CatalogLoads     int64 `json:"catalog_loads"`
	CacheHits        int64 `json:"cache_hits"`
	CachedModules    int64 `json:"cached_modules"`
	CacheEntries     int   `json:"cache_entries"`
	ExportDepthLimit int   `json:"export_depth_limit"`
}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "typescript" }

func (p *Parser) ResolutionMetrics() ResolutionMetrics {
	entries := 0
	p.cacheMu.Lock()
	if p.cacheCatalog != nil {
		entries = 1
	}
	p.cacheMu.Unlock()
	return ResolutionMetrics{CatalogLoads: p.catalogLoads.Load(), CacheHits: p.cacheHits.Load(),
		CachedModules: p.cachedModules.Load(), CacheEntries: entries, ExportDepthLimit: maxExportDepth}
}

func (*Parser) Supports(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".tsx":
		return true
	default:
		return false
	}
}

// WorkspaceSemanticKey fingerprints only the repository-wide TypeScript facts
// that change how an otherwise untouched module extracts: the package and
// compiler manifests in full, and the resolution surface - module identity,
// local declarations, and exports - of every tracked module. A statement body
// cannot change how another module resolves, so it is excluded.
func (p *Parser) WorkspaceSemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	if input.Root == "" {
		empty := newSurfaceDigest(resolutionSurfaceVersion)
		return empty.sum(), nil
	}
	// The catalog is the single authority for the resolution surface, so the
	// key is read from a real build rather than a parallel encoding that could
	// drift from it. The scan cache makes the catalog that Parse builds cheap,
	// and deliberately not storing this one keeps Parse's detection of inputs
	// that change mid-run intact.
	catalog, err := buildModuleCatalog(ctx, input)
	if err != nil {
		return "", err
	}
	return catalog.digest, nil
}

func (*Parser) WorkspaceSemanticEvidenceKey(context.Context, parserapi.Input) (string, error) {
	return "typescript-worktree-v1", nil
}

func (*Parser) IsSemanticInput(path string) bool { return isTypeScriptSemanticInput(path) }

// SemanticAffectedPaths reparses every TypeScript module when the package or
// compiler manifests change, because those select module resolution repository
// wide. A module edit is not expanded here: anything in it that another module
// can resolve through already changes the workspace semantic key, and therefore
// every module's incremental cache key.
func (*Parser) SemanticAffectedPaths(_ string, allPaths, changedPaths []string) []string {
	affected := false
	for _, path := range changedPaths {
		if isTypeScriptSemanticInput(path) && !isTypeScriptPath(path) {
			affected = true
			break
		}
	}
	if !affected {
		return nil
	}
	var result []string
	for _, path := range allPaths {
		if isTypeScriptPath(path) {
			result = append(result, path)
		}
	}
	return result
}

func isTypeScriptSemanticInput(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return isTypeScriptPath(path) || base == "package.json" || base == "tsconfig.json" || strings.HasPrefix(base, "tsconfig.") && strings.HasSuffix(base, ".json")
}

type scope struct {
	currentID string
	parentID  string
	container string
	receiver  string
	symbols   map[string]string
	types     map[string]string
	lexical   *lexicalEnvironment
}

type lexicalEnvironment struct {
	parent   *lexicalEnvironment
	deferred bool
	bindings map[string]*lexicalBinding
}

type lexicalBinding struct {
	declaration uint
	initializer *treesitter.Node
	constant    bool
	environment *lexicalEnvironment
}

type extractor struct {
	b        *parserapi.Builder
	input    parserapi.Input
	source   []byte
	module   string
	moduleID string
	catalog  *moduleCatalog
	info     *moduleInfo
	bindings map[string]importBinding
	reported map[string]bool
}

type importBinding struct {
	local      string
	imported   string
	specifier  string
	typeOnly   bool
	namespace  bool
	module     *moduleInfo
	symbol     symbolRef
	unresolved bool
}

type fetchMethodState uint8

const (
	fetchMethodAbsent fetchMethodState = iota
	fetchMethodExact
	fetchMethodUnknown
)

type fetchMethodEvidence struct {
	state      fetchMethodState
	raw        string
	expression string
}

func (p *Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "typescript")
	catalog, err := p.catalogFor(ctx, input)
	if err != nil {
		return b.Finish(), fmt.Errorf("build TypeScript module catalog: %w", err)
	}
	treeParser := treesitter.NewParser()
	defer treeParser.Close()
	language := tstypescript.LanguageTypescript()
	if strings.EqualFold(filepath.Ext(input.Path), ".tsx") {
		language = tstypescript.LanguageTSX()
	}
	if err := treeParser.SetLanguage(treesitter.NewLanguage(language)); err != nil {
		return b.Finish(), fmt.Errorf("load TypeScript grammar: %w", err)
	}
	tree := parserapi.ParseTreeSitter(ctx, treeParser, input.Content)
	if tree == nil {
		return b.Finish(), errors.New("parse TypeScript source: parser returned no syntax tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		b.Diagnostic(int(root.StartPosition().Row)+1, "warning", "TypeScript contains syntax errors; indexed the recoverable tree")
	}
	module := canonicalModuleName(input.Path)
	// The content being parsed is the authority for its own module's symbols.
	// A reused catalog only proves the cross-module resolution surface of the
	// other modules, so it can never override this file's own declarations.
	info, err := scanModuleCached(input.Path, input.Content)
	if err != nil {
		return b.Finish(), fmt.Errorf("scan current TypeScript module: %w", err)
	}
	if catalog.moduleForPath(input.Path) == nil {
		catalog.moduleNames[info.name]++
	}
	catalog.modules[input.Path] = info
	moduleID := catalog.moduleID(info)
	b.Declare(b.FileID(), graph.Node{ID: moduleID, Kind: graph.KindModule, Name: module,
		QualifiedName: module, Location: graph.Location{Path: input.Path, Line: 1, Column: 1},
		Properties: map[string]string{"canonical_path": input.Path}})
	e := &extractor{b: b, input: input, source: input.Content, module: module, moduleID: moduleID,
		catalog: catalog, info: info, bindings: map[string]importBinding{}, reported: map[string]bool{}}
	e.reportCatalogDiagnostics()
	rootEnvironment := e.newLexicalEnvironment(root, nil, false)
	e.seedHoistedBindings(root, rootEnvironment)
	e.walk(root, scope{currentID: b.FileID(), parentID: b.FileID(), symbols: map[string]string{}, types: map[string]string{}, lexical: rootEnvironment})
	return b.Finish(), nil
}

func (p *Parser) catalogFor(ctx context.Context, input parserapi.Input) (*moduleCatalog, error) {
	if catalog := p.cachedCatalog(input); catalog != nil {
		return catalog, nil
	}
	// Building the catalog scans the whole repository, so concurrent parses
	// serialize here and probe the cache again once the gate is theirs. Without
	// the gate every parser that started before the first build finished would
	// rescan the same tree.
	p.buildMu.Lock()
	defer p.buildMu.Unlock()
	if catalog := p.cachedCatalog(input); catalog != nil {
		return catalog, nil
	}
	catalog, err := buildModuleCatalog(ctx, input)
	if err != nil {
		return nil, err
	}
	semanticKeyChanged := input.SemanticKey != "" && input.SemanticKey != catalog.digest
	p.storeCatalog(input.Root, catalog)
	result := catalog.clone()
	if semanticKeyChanged {
		result.diagnostics = append(result.diagnostics, "TypeScript resolution inputs changed during parsing; rebuilt the module catalog")
	}
	return result, nil
}

// cachedCatalog returns a private copy of the cached catalog when it was built
// for this root and semantic key. An empty semantic key means the caller has no
// stable key for the run, so nothing may be reused.
func (p *Parser) cachedCatalog(input parserapi.Input) *moduleCatalog {
	if input.SemanticKey == "" {
		return nil
	}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if p.cacheCatalog == nil || p.cacheRoot != input.Root || p.cacheKey != input.SemanticKey {
		return nil
	}
	p.cacheHits.Add(1)
	return p.cacheCatalog.clone()
}

func (p *Parser) storeCatalog(root string, catalog *moduleCatalog) {
	p.cacheMu.Lock()
	p.cacheRoot, p.cacheKey, p.cacheCatalog = root, catalog.digest, catalog
	p.cacheMu.Unlock()
	p.catalogLoads.Add(1)
	p.cachedModules.Store(int64(len(catalog.modules)))
}

func (e *extractor) walk(node *treesitter.Node, current scope) {
	if node == nil {
		return
	}
	if isLexicalScopeNode(node) && node.Kind() != "program" {
		current.lexical = e.newLexicalEnvironment(node, current.lexical, false)
	}
	switch node.Kind() {
	case "import_statement":
		e.parseImport(node)
		return
	case "export_statement":
		e.parseExport(node)
	case "function_declaration", "generator_function_declaration", "function_expression", "generator_function", "function_signature":
		e.parseFunction(node, current, graph.KindFunction)
		return
	case "class", "class_declaration", "abstract_class_declaration":
		e.parseClass(node, current)
		return
	case "interface_declaration":
		e.parseInterface(node, current)
		return
	case "type_alias_declaration":
		e.parseTypeAlias(node, current)
		return
	case "method_definition", "method_signature", "abstract_method_signature":
		e.parseFunction(node, current, graph.KindMethod)
		return
	case "variable_declarator":
		if e.parseFunctionVariable(node, current) {
			return
		}
		e.parseVariable(node, current)
	case "assignment_expression":
		e.parseAssignment(node, current)
	case "return_statement":
		e.parseReturn(node, current)
	case "call_expression", "new_expression":
		e.parseCall(node, current)
	case "member_expression", "subscript_expression":
		e.parseEnvironmentRead(node, current)
	}
	e.walkChildren(node, current)
}

func (e *extractor) walkChildren(node *treesitter.Node, current scope) {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		e.walk(node.NamedChild(i), current)
	}
}

func (e *extractor) parseImport(node *treesitter.Node) {
	source := node.ChildByFieldName("source")
	if source == nil {
		for i := uint(0); i < node.NamedChildCount(); i++ {
			child := node.NamedChild(i)
			if isStringNode(child) {
				source = child
				break
			}
		}
	}
	if source == nil {
		return
	}
	module := parserapi.Unquote(e.text(source))
	// A stylesheet or image is imported for its bytes, so it is neither a
	// module that failed to resolve nor one the graph can say anything about.
	// Declaring a module node for it would assert a module that does not exist.
	if isAssetSpecifier(module) {
		return
	}
	resolvedModule, diagnostic := e.catalog.resolveModule(e.input.Path, module)
	e.reportCatalogDiagnostics()
	if diagnostic != "" && resolvedModule == nil && strings.HasPrefix(module, ".") {
		e.b.Diagnostic(int(node.StartPosition().Row)+1, "warning", diagnostic+": "+module)
	}
	bindings := parseImportBindings(e.text(node), module)
	if len(bindings) == 0 {
		bindings = []importBinding{{specifier: module}}
	}
	for _, binding := range bindings {
		binding.module = resolvedModule
		properties := map[string]string{"specifier": module, "binding_kind": importBindingKind(binding)}
		if binding.local != "" {
			properties["local"] = binding.local
		}
		if binding.imported != "" {
			properties["imported"] = binding.imported
		}
		if binding.typeOnly {
			properties["type_only"] = "true"
		}
		targetID, target := "", module
		if resolvedModule != nil {
			targetID, target = e.catalog.moduleID(resolvedModule), ""
			if binding.namespace {
				binding.symbol = symbolRef{qualified: resolvedModule.name, kind: graph.KindModule, owner: resolvedModule.path}
			} else if binding.imported != "" {
				resolved, exportDiagnostic := e.catalog.resolveExport(resolvedModule, binding.imported)
				e.reportCatalogDiagnostics()
				if exportDiagnostic != "" && strings.Contains(exportDiagnostic, "cyclic export") {
					e.b.Diagnostic(int(node.StartPosition().Row)+1, "warning", exportDiagnostic)
				}
				if len(resolved) == 1 && !e.catalog.ambiguousSymbol(resolved[0]) {
					binding.symbol = resolved[0]
				} else {
					binding.unresolved = true
					if len(resolved) == 1 && e.catalog.ambiguousSymbol(resolved[0]) {
						exportDiagnostic = fmt.Sprintf("module identity %s is ambiguous across tracked source files", resolvedModule.name)
					}
					if exportDiagnostic != "" && !strings.Contains(exportDiagnostic, "cyclic export") {
						e.b.Diagnostic(int(node.StartPosition().Row)+1, "warning", exportDiagnostic)
					}
				}
			}
		} else {
			binding.unresolved = true
		}
		if binding.local != "" {
			e.bindings[binding.local] = binding
		}
		e.b.AddFact(e.b.FileID(), graph.EdgeImports, targetID, target, graph.KindModule, e.location(node), properties)
	}
}

func parseImportBindings(statement, specifier string) []importBinding {
	trimmed := strings.TrimSpace(statement)
	from := regexp.MustCompile(`(?s)^import\s+(type\s+)?(.+?)\s+from\s+["'][^"']+["']`).FindStringSubmatch(trimmed)
	if len(from) == 0 {
		return nil
	}
	statementTypeOnly := strings.TrimSpace(from[1]) != ""
	clause := strings.TrimSpace(from[2])
	var result []importBinding
	if comma := topLevelComma(clause); comma >= 0 {
		name := strings.TrimSpace(clause[:comma])
		if isIdentifier(name) {
			result = append(result, importBinding{local: name, imported: "default", specifier: specifier, typeOnly: statementTypeOnly})
		}
		clause = strings.TrimSpace(clause[comma+1:])
	} else if isIdentifier(clause) {
		return []importBinding{{local: clause, imported: "default", specifier: specifier, typeOnly: statementTypeOnly}}
	}
	if strings.HasPrefix(clause, "*") {
		match := regexp.MustCompile(`^\*\s+as\s+([A-Za-z_$][A-Za-z0-9_$]*)$`).FindStringSubmatch(clause)
		if len(match) == 2 {
			result = append(result, importBinding{local: match[1], imported: "*", specifier: specifier, typeOnly: statementTypeOnly, namespace: true})
		}
		return result
	}
	if strings.HasPrefix(clause, "{") && strings.HasSuffix(clause, "}") {
		for _, item := range strings.Split(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(clause, "{"), "}")), ",") {
			item = strings.TrimSpace(item)
			typeOnly := statementTypeOnly
			if strings.HasPrefix(item, "type ") {
				typeOnly = true
				item = strings.TrimSpace(strings.TrimPrefix(item, "type "))
			}
			parts := regexp.MustCompile(`\s+as\s+`).Split(item, 2)
			imported := strings.TrimSpace(parts[0])
			local := imported
			if len(parts) == 2 {
				local = strings.TrimSpace(parts[1])
			}
			if isIdentifier(imported) && isIdentifier(local) {
				result = append(result, importBinding{local: local, imported: imported, specifier: specifier, typeOnly: typeOnly})
			}
		}
	}
	return result
}

func topLevelComma(value string) int {
	depth := 0
	for i, r := range value {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func importBindingKind(binding importBinding) string {
	switch {
	case binding.local == "":
		return "side_effect"
	case binding.namespace:
		return "namespace"
	case binding.imported == "default":
		return "default"
	default:
		return "named"
	}
}

func (e *extractor) parseExport(node *treesitter.Node) {
	text := e.text(node)
	loc := e.location(node)
	if match := exportStarPattern.FindStringSubmatch(text); len(match) > 0 {
		if target, _ := e.catalog.resolveModule(e.input.Path, match[3]); target != nil {
			properties := map[string]string{"specifier": match[3], "exported": "*", "reexport": "true"}
			if match[2] != "" {
				properties["exported"] = match[2]
				properties["namespace"] = "true"
			}
			e.b.AddFact(e.moduleID, graph.EdgeExports, e.catalog.moduleID(target), "", graph.KindModule, loc, properties)
		} else {
			e.b.AddFact(e.moduleID, graph.EdgeExports, "", match[3], graph.KindModule, loc,
				map[string]string{"specifier": match[3], "exported": "*", "reexport": "true"})
		}
		return
	}
	statement := &moduleInfo{path: e.info.path, name: e.info.name, locals: e.info.locals,
		methods: e.info.methods, exports: map[string][]exportRef{}}
	collectModuleExports(statement, text)
	if local := anonymousDefaultLocal(node, e.source); local != "" {
		statement.exports["default"] = append(statement.exports["default"], exportRef{local: local})
	}
	exportedNames := make([]string, 0, len(statement.exports))
	for exported := range statement.exports {
		exportedNames = append(exportedNames, exported)
	}
	sort.Strings(exportedNames)
	for _, exported := range exportedNames {
		refs := statement.exports[exported]
		for _, ref := range refs {
			resolved := e.resolveExportRef(ref)
			properties := map[string]string{"exported": exported}
			if ref.local != "" {
				properties["local"] = ref.local
			}
			if ref.specifier != "" {
				properties["specifier"] = ref.specifier
				properties["imported"] = ref.imported
				properties["reexport"] = "true"
			}
			if ref.typeOnly {
				properties["type_only"] = "true"
			}
			for _, symbol := range resolved {
				e.b.AddFact(e.moduleID, graph.EdgeExports, "", symbol.qualified, symbol.kind, loc, properties)
			}
		}
	}
}

func (e *extractor) resolveExportRef(ref exportRef) []symbolRef {
	switch {
	case ref.local != "":
		if symbol, ok := e.info.locals[ref.local]; ok {
			return []symbolRef{symbol}
		}
	case ref.namespace:
		if target, _ := e.catalog.resolveModule(e.info.path, ref.specifier); target != nil {
			return []symbolRef{{qualified: target.name, kind: graph.KindModule, owner: target.path}}
		}
	default:
		if target, _ := e.catalog.resolveModule(e.info.path, ref.specifier); target != nil {
			resolved, _ := e.catalog.resolveExport(target, ref.imported)
			return resolved
		}
	}
	return nil
}

func (e *extractor) reportCatalogDiagnostics() {
	for _, diagnostic := range e.catalog.diagnostics {
		if diagnostic != "" && !e.reported[diagnostic] {
			e.reported[diagnostic] = true
			e.b.Diagnostic(0, "warning", diagnostic)
		}
	}
}

func (e *extractor) parseFunction(node *treesitter.Node, current scope, kind graph.NodeKind) {
	nameNode := node.ChildByFieldName("name")
	name := e.text(nameNode)
	if name == "" {
		name = fmt.Sprintf("anonymous@%d", node.StartPosition().Row+1)
	}
	qualified := e.qualify(current.container, name)
	loc := e.location(node)
	properties := map[string]string{}
	if strings.HasPrefix(strings.TrimSpace(e.text(node)), "async ") {
		properties["async"] = "true"
	}
	if parameters := functionParameters(node); parameters != nil {
		properties["parameters"] = strings.TrimSpace(e.text(parameters))
	}
	nodeID := e.b.Declare(current.parentID, graph.Node{Kind: kind, Name: name,
		QualifiedName: qualified, Location: loc, Properties: properties})
	functionEnvironment := &lexicalEnvironment{parent: current.lexical, deferred: true, bindings: map[string]*lexicalBinding{}}
	if kind == graph.KindFunction && isIdentifier(name) {
		functionEnvironment.bind(name, node, nil, false)
	}
	functionScope := scope{currentID: nodeID, parentID: nodeID, container: qualified,
		receiver: current.receiver, symbols: map[string]string{}, types: map[string]string{}, lexical: functionEnvironment}
	e.declareParameters(node, functionScope)
	body := node.ChildByFieldName("body")
	if body != nil {
		e.seedHoistedBindings(body, functionEnvironment)
		e.walk(body, functionScope)
	}
}

func (e *extractor) parseFunctionVariable(node *treesitter.Node, current scope) bool {
	value := node.ChildByFieldName("value")
	if value == nil || (value.Kind() != "arrow_function" && value.Kind() != "function_expression" && value.Kind() != "generator_function") {
		return false
	}
	nameNode := node.ChildByFieldName("name")
	name := strings.TrimSpace(e.text(nameNode))
	if name == "" || !isIdentifier(name) {
		return false
	}
	qualified := e.qualify(current.container, name)
	loc := e.location(node)
	properties := map[string]string{"form": value.Kind()}
	if parameters := functionParameters(value); parameters != nil {
		properties["parameters"] = strings.TrimSpace(e.text(parameters))
	}
	nodeID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindFunction, Name: name,
		QualifiedName: qualified, Location: loc, Properties: properties})
	functionEnvironment := &lexicalEnvironment{parent: current.lexical, deferred: true, bindings: map[string]*lexicalBinding{}}
	if innerName := strings.TrimSpace(e.text(value.ChildByFieldName("name"))); isIdentifier(innerName) {
		functionEnvironment.bind(innerName, value, nil, false)
	}
	functionScope := scope{currentID: nodeID, parentID: nodeID, container: qualified,
		receiver: current.receiver, symbols: map[string]string{}, types: map[string]string{}, lexical: functionEnvironment}
	e.declareParameters(value, functionScope)
	body := value.ChildByFieldName("body")
	if body != nil {
		e.seedHoistedBindings(body, functionEnvironment)
		e.walk(body, functionScope)
	} else {
		e.walkChildren(value, functionScope)
	}
	return true
}

func (e *extractor) parseClass(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		name = fmt.Sprintf("anonymous@%d", node.StartPosition().Row+1)
	}
	qualified := e.qualify(current.container, name)
	loc := e.location(node)
	classID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindClass, Name: name,
		QualifiedName: qualified, Location: loc})
	e.parseHeritage(node, classID)
	body := node.ChildByFieldName("body")
	if body != nil {
		e.walkChildren(body, scope{currentID: classID, parentID: classID, container: qualified,
			receiver: qualified, symbols: map[string]string{}, types: map[string]string{}, lexical: current.lexical})
	}
}

func (e *extractor) parseInterface(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	qualified := e.qualify(current.container, name)
	loc := e.location(node)
	interfaceID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindInterface, Name: name,
		QualifiedName: qualified, Location: loc})
	e.parseHeritage(node, interfaceID)
	body := node.ChildByFieldName("body")
	if body != nil {
		e.walkChildren(body, scope{currentID: interfaceID, parentID: interfaceID, container: qualified,
			symbols: map[string]string{}, types: map[string]string{}, lexical: current.lexical})
	}
}

// parseTypeAlias declares the alias itself and then walks its value, so object
// members written inside it are owned by the alias the way interface members
// are owned by their interface instead of by the module.
func (e *extractor) parseTypeAlias(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	qualified := e.qualify(current.container, name)
	aliasID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindType, Name: name,
		QualifiedName: qualified, Location: e.location(node)})
	value := node.ChildByFieldName("value")
	if value != nil {
		e.walkChildren(value, scope{currentID: aliasID, parentID: aliasID, container: qualified,
			symbols: map[string]string{}, types: map[string]string{}, lexical: current.lexical})
	}
}

func (e *extractor) parseHeritage(node *treesitter.Node, fromID string) {
	var visit func(*treesitter.Node)
	visit = func(current *treesitter.Node) {
		if current == nil {
			return
		}
		kind := current.Kind()
		if kind == "extends_clause" || kind == "implements_clause" {
			text := strings.TrimSpace(e.text(current))
			keyword := "extends"
			edgeKind := graph.EdgeExtends
			if kind == "implements_clause" {
				keyword = "implements"
				edgeKind = graph.EdgeImplements
			}
			text = strings.TrimSpace(strings.TrimPrefix(text, keyword))
			for _, target := range strings.Split(text, ",") {
				target = strings.TrimSpace(target)
				if target != "" {
					resolvedTarget := e.resolveTypeName(target)
					e.b.AddFact(fromID, edgeKind, "", resolvedTarget, "", e.location(current),
						map[string]string{"source_name": target})
				}
			}
			return
		}
		for i := uint(0); i < current.NamedChildCount(); i++ {
			visit(current.NamedChild(i))
		}
	}
	visit(node.ChildByFieldName("heritage"))
	if heritage := node.ChildByFieldName("heritage"); heritage == nil {
		for i := uint(0); i < node.NamedChildCount(); i++ {
			child := node.NamedChild(i)
			if child.Kind() == "class_heritage" || child.Kind() == "extends_type_clause" {
				visit(child)
			}
		}
	}
}

func (e *extractor) parseCall(node *treesitter.Node, current scope) {
	isConstructor := node.Kind() == "new_expression"
	calleeNode := node.ChildByFieldName("function")
	if calleeNode == nil {
		calleeNode = node.ChildByFieldName("constructor")
	}
	callee := strings.TrimSpace(e.text(calleeNode))
	if callee == "" {
		return
	}
	if current.receiver != "" && strings.HasPrefix(callee, "this.") {
		callee = current.receiver + strings.TrimPrefix(callee, "this")
	} else if receiver, method, ok := strings.Cut(callee, "."); ok {
		if inferred := current.types[receiver]; inferred != "" {
			if resolved, ok := e.resolveMember(inferred, method); ok {
				callee = resolved
			} else {
				callee = inferred + "." + method
			}
		} else if binding, exists := e.bindings[receiver]; exists && !binding.unresolved {
			if binding.namespace && binding.module != nil {
				if resolved, _ := e.catalog.resolveExport(binding.module, method); len(resolved) == 1 {
					callee = resolved[0].qualified
				}
			} else if resolved, ok := e.catalog.member(binding.symbol, method); ok {
				callee = resolved.qualified
			}
		} else if local, exists := e.info.locals[receiver]; exists {
			if resolved, ok := e.catalog.member(local, method); ok {
				callee = resolved.qualified
			}
		}
	} else if binding, exists := e.bindings[callee]; exists && !binding.unresolved {
		if binding.symbol.kind == graph.KindFunction || isConstructor && binding.symbol.kind == graph.KindClass {
			callee = binding.symbol.qualified
		}
	} else if isConstructor {
		if local, exists := e.info.locals[callee]; exists && local.kind == graph.KindClass {
			callee = local.qualified
		}
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	loc := e.location(node)
	args := namedArguments(node.ChildByFieldName("arguments"))
	method := strings.ToLower(graph.SimpleName(strings.TrimSuffix(callee, "?")))
	if len(args) > 0 && callee == "fetch" && e.isUnshadowedGlobal("fetch", current) {
		if route, ok := e.staticString(args[0], current); ok {
			e.addFetchRequest(fromID, loc, route, args, current)
			return
		}
	}
	if len(args) > 0 && isStringNode(args[0]) && (strings.HasPrefix(callee, "axios.") || strings.HasSuffix(callee, ".request")) {
		route := parserapi.Unquote(e.text(args[0]))
		httpMethod := strings.ToUpper(method)
		if httpMethod == "REQUEST" {
			httpMethod = "ANY"
		}
		e.b.AddFact(fromID, graph.EdgeRequests, "", httpMethod+" "+route, graph.KindEndpoint, loc, nil)
		return
	}
	if isHTTPMethod(method) && len(args) > 1 {
		route := parserapi.Unquote(e.text(args[0]))
		if isStringNode(args[0]) && strings.HasPrefix(route, "/") {
			endpointID := e.b.AddNode(graph.Node{Kind: graph.KindEndpoint,
				Name:          strings.ToUpper(method) + " " + route,
				QualifiedName: fmt.Sprintf("endpoint:%s %s@%s:%d", strings.ToUpper(method), route, loc.Path, loc.Line),
				Location:      loc, Properties: map[string]string{"method": strings.ToUpper(method), "route": route}})
			e.b.AddFact(fromID, graph.EdgeExposes, endpointID, "", "", loc, nil)
			if len(args) > 1 {
				handler := strings.TrimSpace(e.text(args[len(args)-1]))
				if isIdentifier(handler) || strings.Contains(handler, ".") {
					e.b.AddFact(endpointID, graph.EdgeHandledBy, "", handler, "", loc, nil)
				}
			}
			return
		}
	}
	if len(args) > 0 && isStringNode(args[0]) {
		event := parserapi.Unquote(e.text(args[0]))
		switch method {
		case "publish", "publishevent", "emit", "produce":
			e.b.AddFact(fromID, graph.EdgePublishes, "", event, graph.KindEvent, loc, nil)
			return
		case "subscribe", "on", "consume":
			e.b.AddFact(fromID, graph.EdgeSubscribes, "", event, graph.KindEvent, loc, nil)
			return
		}
	}
	for position, argument := range args {
		for _, sourceID := range e.referencedVariables(argument, current.symbols) {
			e.b.AddFact(sourceID, graph.EdgePasses, "", callee, "", loc,
				map[string]string{"argument": fmt.Sprint(position)})
		}
	}
	targetKind := graph.NodeKind("")
	properties := map[string]string(nil)
	if isConstructor {
		targetKind = graph.KindClass
		properties = map[string]string{"constructor": "true"}
	}
	e.b.AddFact(fromID, graph.EdgeCalls, "", callee, targetKind, loc, properties)
}

func (e *extractor) addFetchRequest(fromID string, loc graph.Location, route string, args []*treesitter.Node, current scope) {
	method := fetchMethodEvidence{state: fetchMethodAbsent, raw: "GET", expression: "<default>"}
	if len(args) > 1 {
		method = e.fetchRequestInitMethod(args[1], current, 0)
	}
	properties := map[string]string{
		"http_api":               "fetch",
		"http_method_expression": method.expression,
		"http_raw_route":         route,
		"http_route_expression":  strings.TrimSpace(e.text(args[0])),
		"http_source":            "call",
	}
	httpMethod := "ANY"
	switch method.state {
	case fetchMethodAbsent:
		method.raw, method.expression = "GET", "<default>"
		fallthrough
	case fetchMethodExact:
		normalized, err := httpmodel.NormalizeMethod(method.raw)
		if err != nil {
			properties["http_invalid"] = "true"
			properties["http_raw_method"] = method.raw
			e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf("invalid fetch method %q: %v", method.raw, err))
		} else {
			httpMethod = normalized
			properties["http_method"] = normalized
			properties["http_raw_method"] = method.raw
		}
	case fetchMethodUnknown:
		properties["http_invalid"] = "true"
		properties["http_method_unknown"] = "true"
	}
	properties["http_method_expression"] = method.expression
	if httpMethod == "ANY" {
		properties["http_method"] = httpMethod
	}
	if parsed, err := httpmodel.ParseRoute(route); err == nil {
		properties["http_route"] = parsed.Canonical
		if parsed.Query != "" {
			properties["http_query"] = parsed.Query
		}
		if parsed.Fragment != "" {
			properties["http_fragment"] = parsed.Fragment
		}
		if parsed.Authority != "" {
			properties["http_scheme"] = parsed.Scheme
			properties["http_authority"] = parsed.Authority
		}
	}
	e.b.AddFact(fromID, graph.EdgeRequests, "", httpMethod+" "+route, graph.KindEndpoint, loc, properties)
}

func (e *extractor) fetchRequestInitMethod(node *treesitter.Node, current scope, depth int) fetchMethodEvidence {
	if depth >= 16 {
		return fetchMethodEvidence{state: fetchMethodUnknown, expression: strings.TrimSpace(e.text(node))}
	}
	node = unwrapTypeScriptExpression(node)
	if node == nil {
		return fetchMethodEvidence{state: fetchMethodUnknown}
	}
	if node.Kind() == "null" ||
		(node.Kind() == "undefined" || node.Kind() == "identifier") && strings.TrimSpace(e.text(node)) == "undefined" &&
			e.isUnshadowedGlobal("undefined", current) {
		return fetchMethodEvidence{state: fetchMethodAbsent, raw: "GET", expression: "<default>"}
	}
	if node.Kind() != "object" {
		return fetchMethodEvidence{state: fetchMethodUnknown, expression: strings.TrimSpace(e.text(node))}
	}
	result := fetchMethodEvidence{state: fetchMethodAbsent, raw: "GET", expression: "<default>"}
	for index := uint(0); index < node.NamedChildCount(); index++ {
		member := node.NamedChild(index)
		switch member.Kind() {
		case "pair":
			key, known := e.requestInitPropertyKey(member.ChildByFieldName("key"), current)
			if !known {
				result = fetchMethodEvidence{state: fetchMethodUnknown, expression: strings.TrimSpace(e.text(member))}
				continue
			}
			if key != "method" {
				continue
			}
			value := member.ChildByFieldName("value")
			if raw, ok := e.staticString(value, current); ok {
				result = fetchMethodEvidence{state: fetchMethodExact, raw: raw, expression: strings.TrimSpace(e.text(value))}
			} else {
				result = fetchMethodEvidence{state: fetchMethodUnknown, expression: strings.TrimSpace(e.text(value))}
			}
		case "shorthand_property_identifier":
			if strings.TrimSpace(e.text(member)) != "method" {
				continue
			}
			if raw, ok := e.staticString(member, current); ok {
				result = fetchMethodEvidence{state: fetchMethodExact, raw: raw, expression: strings.TrimSpace(e.text(member))}
			} else {
				result = fetchMethodEvidence{state: fetchMethodUnknown, expression: strings.TrimSpace(e.text(member))}
			}
		case "spread_element":
			spread := namedChild(member, 0)
			spreadMethod := e.fetchRequestInitMethod(spread, current, depth+1)
			if spreadMethod.state != fetchMethodAbsent {
				result = spreadMethod
				if result.state == fetchMethodUnknown {
					result.expression = strings.TrimSpace(e.text(member))
				}
			}
		case "method_definition":
			name, known := e.requestInitPropertyKey(member.ChildByFieldName("name"), current)
			if !known || name == "method" {
				result = fetchMethodEvidence{state: fetchMethodUnknown, expression: strings.TrimSpace(e.text(member))}
			}
		}
	}
	return result
}

func (e *extractor) requestInitPropertyKey(node *treesitter.Node, current scope) (string, bool) {
	if node == nil {
		return "", false
	}
	if node.Kind() == "computed_property_name" {
		node = namedChild(node, 0)
		if value, ok := e.staticString(node, current); ok {
			return value, true
		}
		return "", false
	}
	if node.Kind() == "string" {
		return parserapi.Unquote(e.text(node)), true
	}
	return strings.TrimSpace(e.text(node)), true
}

func (e *extractor) staticString(node *treesitter.Node, current scope) (string, bool) {
	return e.staticStringSeen(node, current, map[*lexicalBinding]bool{})
}

func (e *extractor) staticStringSeen(node *treesitter.Node, current scope, seen map[*lexicalBinding]bool) (string, bool) {
	node = unwrapTypeScriptExpression(node)
	if node == nil {
		return "", false
	}
	switch node.Kind() {
	case "string":
		return parserapi.Unquote(e.text(node)), true
	case "template_string":
		text := strings.TrimSpace(e.text(node))
		if strings.Contains(text, "${") {
			return "", false
		}
		return parserapi.Unquote(text), true
	case "identifier", "shorthand_property_identifier":
		return e.lexicalConstant(strings.TrimSpace(e.text(node)), node, current.lexical, seen)
	default:
		return "", false
	}
}

func (e *extractor) isUnshadowedGlobal(name string, current scope) bool {
	if _, imported := e.bindings[name]; imported {
		return false
	}
	return current.lexical == nil || !current.lexical.has(name)
}

func (e *extractor) lexicalConstant(name string, use *treesitter.Node, environment *lexicalEnvironment,
	seen map[*lexicalBinding]bool,
) (string, bool) {
	crossedDeferred := false
	for current := environment; current != nil; current = current.parent {
		binding, ok := current.bindings[name]
		if ok {
			if !binding.constant || binding.initializer == nil || seen[binding] ||
				binding.declaration >= use.StartByte() && !crossedDeferred {
				return "", false
			}
			seen[binding] = true
			value, exact := e.staticStringSeen(binding.initializer, scope{lexical: binding.environment}, seen)
			delete(seen, binding)
			return value, exact
		}
		crossedDeferred = crossedDeferred || current.deferred
	}
	return "", false
}

func (environment *lexicalEnvironment) has(name string) bool {
	for current := environment; current != nil; current = current.parent {
		if _, ok := current.bindings[name]; ok {
			return true
		}
	}
	return false
}

func (environment *lexicalEnvironment) bind(name string, declaration, initializer *treesitter.Node, constant bool) {
	if environment == nil || name == "" {
		return
	}
	if existing, ok := environment.bindings[name]; ok {
		existing.initializer = nil
		existing.constant = false
		return
	}
	start := uint(0)
	if declaration != nil {
		start = declaration.StartByte()
	}
	environment.bindings[name] = &lexicalBinding{declaration: start, initializer: initializer,
		constant: constant, environment: environment}
}

func (environment *lexicalEnvironment) markDynamic(name string) {
	for current := environment; current != nil; current = current.parent {
		if binding, ok := current.bindings[name]; ok {
			binding.initializer = nil
			binding.constant = false
			return
		}
	}
}

func isLexicalScopeNode(node *treesitter.Node) bool {
	switch node.Kind() {
	case "program", "statement_block", "for_statement", "for_in_statement", "for_of_statement", "catch_clause", "switch_body":
		return true
	default:
		return false
	}
}

func (e *extractor) newLexicalEnvironment(node *treesitter.Node, parent *lexicalEnvironment,
	deferred bool,
) *lexicalEnvironment {
	environment := &lexicalEnvironment{parent: parent, deferred: deferred, bindings: map[string]*lexicalBinding{}}
	if node == nil {
		return environment
	}
	for index := uint(0); index < node.NamedChildCount(); index++ {
		e.seedLexicalBinding(node.NamedChild(index), environment)
	}
	if node.Kind() == "catch_clause" {
		parameter := node.ChildByFieldName("parameter")
		for _, identifier := range e.bindingIdentifiers(parameter) {
			environment.bind(strings.TrimSpace(e.text(identifier)), identifier, nil, false)
		}
	}
	return environment
}

func (e *extractor) seedLexicalBinding(node *treesitter.Node, environment *lexicalEnvironment) {
	if node == nil {
		return
	}
	if node.Kind() == "export_statement" {
		for index := uint(0); index < node.NamedChildCount(); index++ {
			child := node.NamedChild(index)
			switch child.Kind() {
			case "lexical_declaration", "function_declaration", "generator_function_declaration",
				"class_declaration", "abstract_class_declaration":
				e.seedLexicalBinding(child, environment)
			}
		}
		return
	}
	switch node.Kind() {
	case "lexical_declaration":
		constant := strings.HasPrefix(strings.TrimSpace(e.text(node)), "const ")
		for index := uint(0); index < node.NamedChildCount(); index++ {
			declarator := node.NamedChild(index)
			if declarator.Kind() != "variable_declarator" {
				continue
			}
			nameNode := declarator.ChildByFieldName("name")
			identifiers := e.bindingIdentifiers(nameNode)
			simple := nameNode != nil && nameNode.Kind() == "identifier" && len(identifiers) == 1
			for _, identifier := range identifiers {
				initializer := (*treesitter.Node)(nil)
				if simple && constant {
					initializer = declarator.ChildByFieldName("value")
				}
				environment.bind(strings.TrimSpace(e.text(identifier)), identifier, initializer, simple && constant)
			}
		}
	case "switch_case", "switch_default":
		for index := uint(0); index < node.NamedChildCount(); index++ {
			e.seedLexicalBinding(node.NamedChild(index), environment)
		}
	case "function_declaration", "generator_function_declaration", "class_declaration", "abstract_class_declaration":
		nameNode := node.ChildByFieldName("name")
		name := strings.TrimSpace(e.text(nameNode))
		if isIdentifier(name) {
			environment.bind(name, node, nil, false)
		}
	}
}

func (e *extractor) seedHoistedBindings(node *treesitter.Node, environment *lexicalEnvironment) {
	if node == nil {
		return
	}
	if node.Kind() == "variable_declaration" {
		for index := uint(0); index < node.NamedChildCount(); index++ {
			declarator := node.NamedChild(index)
			if declarator.Kind() != "variable_declarator" {
				continue
			}
			for _, identifier := range e.bindingIdentifiers(declarator.ChildByFieldName("name")) {
				environment.bind(strings.TrimSpace(e.text(identifier)), identifier, nil, false)
			}
		}
		return
	}
	switch node.Kind() {
	case "function_declaration", "generator_function_declaration", "function_expression", "generator_function",
		"arrow_function", "method_definition", "class", "class_declaration", "abstract_class_declaration":
		return
	}
	for index := uint(0); index < node.NamedChildCount(); index++ {
		e.seedHoistedBindings(node.NamedChild(index), environment)
	}
}

func functionParameters(function *treesitter.Node) *treesitter.Node {
	if function == nil {
		return nil
	}
	if parameters := function.ChildByFieldName("parameters"); parameters != nil {
		return parameters
	}
	return function.ChildByFieldName("parameter")
}

func (e *extractor) bindingIdentifiers(pattern *treesitter.Node) []*treesitter.Node {
	if pattern == nil {
		return nil
	}
	switch pattern.Kind() {
	case "identifier", "shorthand_property_identifier_pattern":
		return []*treesitter.Node{pattern}
	case "required_parameter", "optional_parameter":
		if child := pattern.ChildByFieldName("pattern"); child != nil {
			return e.bindingIdentifiers(child)
		}
		if child := pattern.ChildByFieldName("name"); child != nil {
			return e.bindingIdentifiers(child)
		}
		for index := uint(0); index < pattern.NamedChildCount(); index++ {
			if identifiers := e.bindingIdentifiers(pattern.NamedChild(index)); len(identifiers) > 0 {
				return identifiers
			}
		}
		return nil
	case "assignment_pattern":
		return e.bindingIdentifiers(pattern.ChildByFieldName("left"))
	case "pair_pattern":
		return e.bindingIdentifiers(pattern.ChildByFieldName("value"))
	case "object_assignment_pattern":
		if child := pattern.ChildByFieldName("left"); child != nil {
			return e.bindingIdentifiers(child)
		}
		return e.bindingIdentifiers(pattern.ChildByFieldName("name"))
	case "rest_pattern":
		if child := pattern.ChildByFieldName("argument"); child != nil {
			return e.bindingIdentifiers(child)
		}
		return e.bindingIdentifiers(namedChild(pattern, 0))
	case "object_pattern", "array_pattern":
		var identifiers []*treesitter.Node
		for index := uint(0); index < pattern.NamedChildCount(); index++ {
			identifiers = append(identifiers, e.bindingIdentifiers(pattern.NamedChild(index))...)
		}
		return identifiers
	default:
		if pattern.NamedChildCount() == 0 && isIdentifier(strings.TrimSpace(e.text(pattern))) {
			return []*treesitter.Node{pattern}
		}
		return nil
	}
}

func unwrapTypeScriptExpression(node *treesitter.Node) *treesitter.Node {
	for depth := 0; node != nil && depth < 16; depth++ {
		switch node.Kind() {
		case "as_expression", "satisfies_expression", "parenthesized_expression", "non_null_expression":
			node = namedChild(node, 0)
		case "type_assertion":
			node = namedChild(node, node.NamedChildCount()-1)
		default:
			return node
		}
	}
	return node
}

func namedChild(node *treesitter.Node, index uint) *treesitter.Node {
	if node == nil || index >= node.NamedChildCount() {
		return nil
	}
	return node.NamedChild(index)
}

func (e *extractor) declareParameters(function *treesitter.Node, current scope) {
	parameters := functionParameters(function)
	if parameters == nil {
		return
	}
	parameterNodes := []*treesitter.Node{parameters}
	if parameters.Kind() == "formal_parameters" {
		parameterNodes = parameterNodes[:0]
		for i := uint(0); i < parameters.NamedChildCount(); i++ {
			parameterNodes = append(parameterNodes, parameters.NamedChild(i))
		}
	}
	for _, parameter := range parameterNodes {
		typeText := e.typeText(parameter)
		for _, identifier := range e.bindingIdentifiers(parameter) {
			name := strings.TrimSpace(e.text(identifier))
			properties := map[string]string{}
			if typeText != "" {
				properties["type"] = typeText
			}
			id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindParameter, Name: name,
				QualifiedName: current.container + "." + name, Location: e.location(identifier), Properties: properties})
			current.symbols[name] = id
			current.lexical.bind(name, identifier, nil, false)
			if typeText != "" {
				current.types[name] = e.qualifyType(typeText)
			}
		}
	}
}

func (e *extractor) parseVariable(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if !isIdentifier(name) || current.currentID == "" {
		return
	}
	typeText := e.typeText(node)
	value := node.ChildByFieldName("value")
	if typeText == "" {
		typeText = e.inferExpressionType(value, current)
	}
	properties := map[string]string{}
	if typeText != "" {
		properties["type"] = typeText
	}
	loc := e.location(node)
	qualified := e.qualify(current.container, name)
	id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindVariable, Name: name,
		QualifiedName: fmt.Sprintf("%s@%d", qualified, loc.Line), Location: loc, Properties: properties})
	if current.symbols == nil {
		current.symbols = map[string]string{}
	}
	if current.types == nil {
		current.types = map[string]string{}
	}
	current.symbols[name] = id
	if typeText != "" {
		current.types[name] = e.qualifyType(typeText)
	}
	for _, sourceID := range e.referencedVariables(value, current.symbols) {
		if sourceID != id {
			e.b.AddFact(sourceID, graph.EdgeAssigns, id, "", graph.KindVariable, loc, nil)
		}
	}
}

func (e *extractor) parseAssignment(node *treesitter.Node, current scope) {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil || right == nil {
		return
	}
	targetID := current.symbols[strings.TrimSpace(e.text(left))]
	if targetID == "" {
		return
	}
	current.lexical.markDynamic(strings.TrimSpace(e.text(left)))
	for _, sourceID := range e.referencedVariables(right, current.symbols) {
		e.b.AddFact(sourceID, graph.EdgeAssigns, targetID, "", graph.KindVariable, e.location(node), nil)
	}
}

func (e *extractor) parseReturn(node *treesitter.Node, current scope) {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		for _, sourceID := range e.referencedVariables(node.NamedChild(i), current.symbols) {
			e.b.AddFact(sourceID, graph.EdgeReturns, current.currentID, "", "", e.location(node), nil)
		}
	}
}

func (e *extractor) referencedVariables(node *treesitter.Node, symbols map[string]string) []string {
	if node == nil || len(symbols) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var result []string
	var visit func(*treesitter.Node)
	visit = func(current *treesitter.Node) {
		if current == nil {
			return
		}
		if current.Kind() == "identifier" {
			if id := symbols[strings.TrimSpace(e.text(current))]; id != "" && !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
		for i := uint(0); i < current.NamedChildCount(); i++ {
			visit(current.NamedChild(i))
		}
	}
	visit(node)
	return result
}

func (e *extractor) typeText(node *treesitter.Node) string {
	if node == nil {
		return ""
	}
	if typed := node.ChildByFieldName("type"); typed != nil {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e.text(typed)), ":"))
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child.Kind() == "type_annotation" {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e.text(child)), ":"))
		}
	}
	return ""
}

func (e *extractor) inferExpressionType(node *treesitter.Node, current scope) string {
	if node == nil {
		return ""
	}
	if node.Kind() == "new_expression" {
		constructor := node.ChildByFieldName("constructor")
		return strings.TrimSpace(e.text(constructor))
	}
	if node.Kind() == "identifier" {
		return current.types[strings.TrimSpace(e.text(node))]
	}
	return ""
}

func (e *extractor) qualifyType(typeText string) string {
	typeText = strings.TrimSpace(strings.TrimSuffix(typeText, "[]"))
	typeText = strings.TrimSuffix(typeText, " | undefined")
	if typeText == "" || strings.ContainsAny(typeText, "<>{}[]|&") || isTypeScriptPrimitive(typeText) || strings.Contains(typeText, ".") {
		return typeText
	}
	return e.resolveTypeName(typeText)
}

func (e *extractor) resolveTypeName(typeName string) string {
	typeName = strings.TrimSpace(typeName)
	if binding, ok := e.bindings[typeName]; ok && !binding.unresolved && binding.symbol.qualified != "" {
		return binding.symbol.qualified
	}
	if local, ok := e.info.locals[typeName]; ok && (local.kind == graph.KindClass || local.kind == graph.KindInterface || local.kind == graph.KindType) {
		return local.qualified
	}
	return e.module + "." + typeName
}

func (e *extractor) resolveMember(typeName, member string) (string, bool) {
	resolved := ""
	matches := 0
	for _, info := range e.catalog.modules {
		for localName, symbol := range info.locals {
			if symbol.qualified == typeName {
				if method, ok := info.methods[localName][member]; ok {
					resolved = method.qualified
					matches++
				}
			}
		}
	}
	return resolved, matches == 1
}

func isTypeScriptPrimitive(value string) bool {
	switch value {
	case "string", "number", "boolean", "bigint", "symbol", "unknown", "any", "never", "void", "object", "undefined", "null":
		return true
	default:
		return false
	}
}

func (e *extractor) parseEnvironmentRead(node *treesitter.Node, current scope) {
	text := strings.TrimSpace(e.text(node))
	key := ""
	if strings.HasPrefix(text, "process.env.") {
		key = strings.TrimPrefix(text, "process.env.")
	} else if strings.HasPrefix(text, "process.env[") && strings.HasSuffix(text, "]") {
		key = parserapi.Unquote(strings.TrimSuffix(strings.TrimPrefix(text, "process.env["), "]"))
	}
	if key == "" || strings.ContainsAny(key, ".[]() ") {
		return
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, e.location(node), nil)
}

func (e *extractor) qualify(container, name string) string {
	if container != "" {
		return container + "." + name
	}
	if e.module == "" {
		return name
	}
	return e.module + "." + name
}

func (e *extractor) text(node *treesitter.Node) string {
	if node == nil {
		return ""
	}
	return node.Utf8Text(e.source)
}

func (e *extractor) location(node *treesitter.Node) graph.Location {
	start, end := node.StartPosition(), node.EndPosition()
	return graph.Location{Path: e.input.Path, Line: int(start.Row) + 1, Column: int(start.Column) + 1, EndLine: int(end.Row) + 1}
}

func namedArguments(arguments *treesitter.Node) []*treesitter.Node {
	if arguments == nil {
		return nil
	}
	result := make([]*treesitter.Node, 0, arguments.NamedChildCount())
	for i := uint(0); i < arguments.NamedChildCount(); i++ {
		result = append(result, arguments.NamedChild(i))
	}
	return result
}

func isStringNode(node *treesitter.Node) bool {
	if node == nil {
		return false
	}
	switch node.Kind() {
	case "string", "template_string":
		return true
	default:
		return false
	}
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 && r != '_' && r != '$' && !unicode.IsLetter(r) {
			return false
		}
		if i > 0 && r != '_' && r != '$' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func isHTTPMethod(method string) bool {
	switch method {
	case "get", "post", "put", "patch", "delete", "head", "options":
		return true
	default:
		return false
	}
}
