package typescript

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	tstypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

type Parser struct {
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

func (p *Parser) WorkspaceSemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	return moduleCatalogDigest(ctx, input.Root)
}

func (*Parser) SemanticAffectedPaths(allPaths, changedPaths []string) []string {
	affected := false
	for _, path := range changedPaths {
		if isTypeScriptSemanticInput(path) {
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
	tree := treeParser.ParseCtx(ctx, input.Content, nil)
	if tree == nil {
		return b.Finish(), fmt.Errorf("TypeScript parser returned no syntax tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		b.Diagnostic(int(root.StartPosition().Row)+1, "warning", "TypeScript contains syntax errors; indexed the recoverable tree")
	}
	module := canonicalModuleName(input.Path)
	info := catalog.moduleForPath(input.Path)
	if info == nil {
		info, err = scanModule(input.Path, input.Content)
		if err != nil {
			return b.Finish(), fmt.Errorf("scan current TypeScript module: %w", err)
		}
		catalog.modules[input.Path] = info
		catalog.moduleNames[info.name]++
	}
	moduleID := catalog.moduleID(info)
	b.Declare(b.FileID(), graph.Node{ID: moduleID, Kind: graph.KindModule, Name: module,
		QualifiedName: module, Location: graph.Location{Path: input.Path, Line: 1, Column: 1},
		Properties: map[string]string{"canonical_path": input.Path}})
	e := &extractor{b: b, input: input, source: input.Content, module: module, moduleID: moduleID,
		catalog: catalog, info: info, bindings: map[string]importBinding{}, reported: map[string]bool{}}
	e.reportCatalogDiagnostics()
	e.walk(root, scope{currentID: b.FileID(), parentID: b.FileID(), symbols: map[string]string{}, types: map[string]string{}})
	return b.Finish(), nil
}

func (p *Parser) catalogFor(ctx context.Context, input parserapi.Input) (*moduleCatalog, error) {
	if input.SemanticKey != "" {
		p.cacheMu.Lock()
		if p.cacheCatalog != nil && p.cacheRoot == input.Root && p.cacheKey == input.SemanticKey {
			catalog := p.cacheCatalog.clone()
			p.cacheMu.Unlock()
			p.cacheHits.Add(1)
			return catalog, nil
		}
		p.cacheMu.Unlock()
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
	switch node.Kind() {
	case "import_statement":
		e.parseImport(node)
		return
	case "export_statement":
		e.parseExport(node)
	case "function_declaration", "generator_function_declaration", "function_expression", "generator_function":
		e.parseFunction(node, current, graph.KindFunction)
		return
	case "class", "class_declaration", "abstract_class_declaration":
		e.parseClass(node, current)
		return
	case "interface_declaration":
		e.parseInterface(node, current)
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
	collectModuleExports(statement, text, int(node.StartPosition().Row))
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
	if parameters := node.ChildByFieldName("parameters"); parameters != nil {
		properties["parameters"] = strings.TrimSpace(e.text(parameters))
	}
	nodeID := e.b.Declare(current.parentID, graph.Node{Kind: kind, Name: name,
		QualifiedName: qualified, Location: loc, Properties: properties})
	functionScope := scope{currentID: nodeID, parentID: nodeID, container: qualified,
		receiver: current.receiver, symbols: map[string]string{}, types: map[string]string{}}
	e.declareParameters(node.ChildByFieldName("parameters"), functionScope)
	body := node.ChildByFieldName("body")
	if body != nil {
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
	if parameters := value.ChildByFieldName("parameters"); parameters != nil {
		properties["parameters"] = strings.TrimSpace(e.text(parameters))
	}
	nodeID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindFunction, Name: name,
		QualifiedName: qualified, Location: loc, Properties: properties})
	functionScope := scope{currentID: nodeID, parentID: nodeID, container: qualified,
		receiver: current.receiver, symbols: map[string]string{}, types: map[string]string{}}
	e.declareParameters(value.ChildByFieldName("parameters"), functionScope)
	body := value.ChildByFieldName("body")
	if body == nil {
		body = value
	}
	e.walkChildren(body, functionScope)
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
			receiver: qualified, symbols: map[string]string{}, types: map[string]string{}})
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
			symbols: map[string]string{}, types: map[string]string{}})
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
	if len(args) > 0 && isStringNode(args[0]) && (callee == "fetch" || strings.HasPrefix(callee, "axios.") || strings.HasSuffix(callee, ".request")) {
		route := parserapi.Unquote(e.text(args[0]))
		httpMethod := strings.ToUpper(method)
		if callee == "fetch" || httpMethod == "REQUEST" {
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

func (e *extractor) declareParameters(parameters *treesitter.Node, current scope) {
	if parameters == nil {
		return
	}
	for i := uint(0); i < parameters.NamedChildCount(); i++ {
		parameter := parameters.NamedChild(i)
		nameNode := parameter
		if pattern := parameter.ChildByFieldName("pattern"); pattern != nil {
			nameNode = pattern
		} else if name := parameter.ChildByFieldName("name"); name != nil {
			nameNode = name
		}
		name := strings.TrimSpace(e.text(nameNode))
		if !isIdentifier(name) {
			continue
		}
		typeText := e.typeText(parameter)
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindParameter, Name: name,
			QualifiedName: current.container + "." + name, Location: e.location(parameter), Properties: properties})
		current.symbols[name] = id
		if typeText != "" {
			current.types[name] = e.qualifyType(typeText)
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
		if i == 0 && !(r == '_' || r == '$' || unicode.IsLetter(r)) {
			return false
		}
		if i > 0 && !(r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
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
