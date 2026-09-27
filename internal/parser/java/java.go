package java

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "java" }

func (*Parser) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".java")
}

type scope struct {
	currentID   string
	parentID    string
	container   string
	receiver    string
	superclass  string
	routePrefix string
	classBody   bool
	symbols     map[string]string
	types       map[string]string
}

type extractor struct {
	b             *parserapi.Builder
	input         parserapi.Input
	source        []byte
	packageName   string
	imports       map[string]string
	staticImports map[string]string
	declarations  map[string]string
	declKinds     map[string]graph.NodeKind
	methods       map[string]bool
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "java")
	p := treesitter.NewParser()
	defer p.Close()
	if err := p.SetLanguage(treesitter.NewLanguage(tsjava.Language())); err != nil {
		return b.Finish(), fmt.Errorf("load Java grammar: %w", err)
	}
	tree := parserapi.ParseTreeSitter(ctx, p, input.Content)
	if tree == nil {
		return b.Finish(), errors.New("parse Java source: parser returned no syntax tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		b.Diagnostic(int(root.StartPosition().Row)+1, "warning", "Java contains syntax errors; indexed the recoverable tree")
	}
	e := &extractor{
		b: b, input: input, source: input.Content,
		imports: map[string]string{}, staticImports: map[string]string{},
		declarations: map[string]string{}, declKinds: map[string]graph.NodeKind{}, methods: map[string]bool{},
	}
	e.collectPackage(root)
	e.collectDeclarations(root, e.packageName)
	e.walk(root, scope{currentID: b.FileID(), parentID: b.FileID(), symbols: map[string]string{}, types: map[string]string{}})
	return b.Finish(), nil
}

func (e *extractor) collectPackage(root *treesitter.Node) {
	for i := uint(0); i < root.NamedChildCount(); i++ {
		child := root.NamedChild(i)
		if child.Kind() != "package_declaration" {
			continue
		}
		for j := uint(0); j < child.NamedChildCount(); j++ {
			candidate := child.NamedChild(j)
			if candidate.Kind() == "identifier" || candidate.Kind() == "scoped_identifier" {
				e.packageName = strings.TrimSpace(e.text(candidate))
				return
			}
		}
	}
}

func (e *extractor) collectDeclarations(node *treesitter.Node, container string) {
	if node == nil {
		return
	}
	if kind, ok := javaTypeKind(node.Kind()); ok {
		name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
		if name != "" {
			qualified := qualify(container, name)
			e.rememberDeclaration(name, qualified, kind)
			container = qualified
		}
	} else if node.Kind() == "method_declaration" || node.Kind() == "constructor_declaration" || node.Kind() == "compact_constructor_declaration" {
		name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
		if name != "" && container != "" {
			e.methods[qualify(container, name)] = true
		}
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		e.collectDeclarations(node.NamedChild(i), container)
	}
}

func (e *extractor) rememberDeclaration(name, qualified string, kind graph.NodeKind) {
	if previous, exists := e.declarations[name]; exists && previous != qualified {
		e.declarations[name] = ""
	} else if !exists {
		e.declarations[name] = qualified
	}
	e.declarations[qualified] = qualified
	e.declKinds[qualified] = kind
	if _, exists := e.declKinds[name]; !exists {
		e.declKinds[name] = kind
	} else if e.declarations[name] == "" {
		delete(e.declKinds, name)
	}
}

func (e *extractor) walk(node *treesitter.Node, current scope) {
	if node == nil {
		return
	}
	switch node.Kind() {
	case "package_declaration":
		return
	case "import_declaration":
		e.parseImport(node)
		return
	case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration", "annotation_type_declaration":
		e.parseType(node, current)
		return
	case "method_declaration":
		e.parseMethod(node, current, false)
		return
	case "constructor_declaration", "compact_constructor_declaration":
		e.parseMethod(node, current, true)
		return
	case "field_declaration", "constant_declaration":
		e.parseVariables(node, current, graph.KindField)
		return
	case "local_variable_declaration":
		e.parseVariables(node, current, graph.KindVariable)
		return
	case "enhanced_for_statement":
		e.parseEnhancedFor(node, current)
	case "assignment_expression":
		e.parseAssignment(node, current)
	case "return_statement":
		e.parseReturn(node, current)
	case "method_invocation":
		e.parseCall(node, current)
	}
	e.walkChildren(node, current)
}

func (e *extractor) walkChildren(node *treesitter.Node, current scope) {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		e.walk(node.NamedChild(i), current)
	}
}

func (e *extractor) parseImport(node *treesitter.Node) {
	value := strings.TrimSpace(e.text(node))
	value = strings.TrimSuffix(value, ";")
	value = strings.TrimSpace(strings.TrimPrefix(value, "import"))
	isStatic := strings.HasPrefix(value, "static ")
	if isStatic {
		value = strings.TrimSpace(strings.TrimPrefix(value, "static"))
	}
	wildcard := strings.HasSuffix(value, ".*")
	target := strings.TrimSuffix(value, ".*")
	if target == "" {
		return
	}
	properties := map[string]string{}
	if isStatic {
		properties["static"] = "true"
	}
	if wildcard {
		properties["wildcard"] = "true"
	}
	e.b.AddFact(e.b.FileID(), graph.EdgeImports, "", target, graph.KindModule, e.location(node), properties)
	if wildcard {
		return
	}
	name := graph.SimpleName(target)
	if isStatic {
		if previous, exists := e.staticImports[name]; exists && previous != target {
			e.staticImports[name] = ""
		} else if !exists {
			e.staticImports[name] = target
		}
		return
	}
	if previous, exists := e.imports[name]; exists && previous != target {
		e.imports[name] = ""
	} else if !exists {
		e.imports[name] = target
	}
}

func (e *extractor) parseType(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	kind, _ := javaTypeKind(node.Kind())
	qualified := qualify(current.container, name)
	if current.container == "" {
		qualified = qualify(e.packageName, name)
	}
	declaration := strings.TrimSuffix(node.Kind(), "_declaration")
	properties := map[string]string{"declaration": declaration}
	typeID := e.b.Declare(current.parentID, graph.Node{Kind: kind, Name: name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	superclass := e.parseHeritage(node, typeID, kind)
	typeScope := scope{
		currentID: typeID, parentID: typeID, container: qualified, receiver: qualified,
		superclass: superclass, routePrefix: joinRoute(current.routePrefix, e.annotationPath(node)),
		classBody: true, symbols: map[string]string{}, types: map[string]string{},
	}
	if node.Kind() == "record_declaration" {
		e.declareRecordFields(node.ChildByFieldName("parameters"), typeScope)
	}
	if body := node.ChildByFieldName("body"); body != nil {
		e.walkChildren(body, typeScope)
	}
}

func (e *extractor) parseHeritage(node *treesitter.Node, fromID string, kind graph.NodeKind) string {
	var superclass string
	if super := node.ChildByFieldName("superclass"); super != nil {
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e.text(super)), "extends"))
		if resolved := e.resolveType(name); resolved != "" {
			superclass = resolved
			e.b.AddFact(fromID, graph.EdgeExtends, "", resolved, graph.KindClass, e.location(super), nil)
		}
	}
	if interfaces := node.ChildByFieldName("interfaces"); interfaces != nil {
		e.addHeritageList(fromID, interfaces, graph.EdgeImplements)
	}
	if kind == graph.KindInterface {
		for i := uint(0); i < node.NamedChildCount(); i++ {
			child := node.NamedChild(i)
			if child.Kind() == "extends_interfaces" {
				e.addHeritageList(fromID, child, graph.EdgeExtends)
			}
		}
	}
	return superclass
}

func (e *extractor) addHeritageList(fromID string, node *treesitter.Node, edgeKind graph.EdgeKind) {
	text := strings.TrimSpace(e.text(node))
	for _, prefix := range []string{"implements", "extends"} {
		text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
	}
	for _, name := range splitTopLevel(text, ',') {
		if resolved := e.resolveType(name); resolved != "" {
			targetKind := e.declKinds[resolved]
			e.b.AddFact(fromID, edgeKind, "", resolved, targetKind, e.location(node), nil)
		}
	}
}

func (e *extractor) parseMethod(node *treesitter.Node, current scope, constructor bool) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	properties := map[string]string{}
	if constructor {
		properties["constructor"] = "true"
	} else if returnType := node.ChildByFieldName("type"); returnType != nil {
		properties["returns"] = strings.TrimSpace(e.text(returnType))
	}
	if parameters := node.ChildByFieldName("parameters"); parameters != nil {
		properties["parameters"] = strings.TrimSpace(e.text(parameters))
	}
	qualified := qualify(current.container, name)
	methodID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindMethod, Name: name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	methodScope := scope{
		currentID: methodID, parentID: methodID, container: qualified, receiver: current.receiver,
		superclass: current.superclass, routePrefix: current.routePrefix,
		symbols: cloneMap(current.symbols), types: cloneMap(current.types),
	}
	e.declareParameters(node.ChildByFieldName("parameters"), methodScope)
	e.parseRouteAnnotations(node, methodID, qualified, current.routePrefix)
	if body := node.ChildByFieldName("body"); body != nil {
		e.walk(body, methodScope)
	}
}

func (e *extractor) declareParameters(parameters *treesitter.Node, current scope) {
	if parameters == nil {
		return
	}
	for i := uint(0); i < parameters.NamedChildCount(); i++ {
		parameter := parameters.NamedChild(i)
		if parameter.Kind() == "receiver_parameter" {
			continue
		}
		name := strings.TrimSpace(e.text(parameter.ChildByFieldName("name")))
		if name == "" && parameter.Kind() == "identifier" {
			name = strings.TrimSpace(e.text(parameter))
		}
		if !isIdentifier(name) {
			continue
		}
		typeText := strings.TrimSpace(e.text(parameter.ChildByFieldName("type")))
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindParameter, Name: name,
			QualifiedName: qualify(current.container, name), Location: e.location(parameter), Properties: properties})
		current.symbols[name] = id
		if resolved := e.resolveType(typeText); resolved != "" {
			current.types[name] = resolved
		}
	}
}

func (e *extractor) declareRecordFields(parameters *treesitter.Node, current scope) {
	if parameters == nil {
		return
	}
	for i := uint(0); i < parameters.NamedChildCount(); i++ {
		parameter := parameters.NamedChild(i)
		name := strings.TrimSpace(e.text(parameter.ChildByFieldName("name")))
		if !isIdentifier(name) {
			continue
		}
		typeText := strings.TrimSpace(e.text(parameter.ChildByFieldName("type")))
		properties := map[string]string{"record_component": "true"}
		if typeText != "" {
			properties["type"] = typeText
		}
		id := e.b.AddNode(graph.Node{Kind: graph.KindField, Name: name, QualifiedName: qualify(current.container, name),
			Location: e.location(parameter), Properties: properties})
		e.b.AddFact(current.currentID, graph.EdgeHasField, id, "", graph.KindField, e.location(parameter), nil)
		current.symbols[name], current.symbols["this."+name] = id, id
		if resolved := e.resolveType(typeText); resolved != "" {
			current.types[name] = resolved
		}
	}
}

func (e *extractor) parseVariables(node *treesitter.Node, current scope, kind graph.NodeKind) {
	typeText := strings.TrimSpace(e.text(node.ChildByFieldName("type")))
	for i := uint(0); i < node.NamedChildCount(); i++ {
		declarator := node.NamedChild(i)
		if declarator.Kind() != "variable_declarator" {
			continue
		}
		name := strings.TrimSpace(e.text(declarator.ChildByFieldName("name")))
		if !isIdentifier(name) {
			continue
		}
		value := declarator.ChildByFieldName("value")
		resolvedType := e.resolveType(typeText)
		if resolvedType == "" || baseType(typeText) == "var" {
			resolvedType = e.inferType(value, current)
		}
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		loc := e.location(declarator)
		qualified := qualify(current.container, name)
		if kind == graph.KindVariable {
			qualified = fmt.Sprintf("%s@%d", qualified, loc.Line)
		}
		graphNode := graph.Node{Kind: kind, Name: name, QualifiedName: qualified, Location: loc, Properties: properties}
		var id string
		if kind == graph.KindField {
			id = e.b.AddNode(graphNode)
			e.b.AddFact(current.currentID, graph.EdgeHasField, id, "", graph.KindField, loc, nil)
			current.symbols["this."+name] = id
		} else {
			id = e.b.Declare(current.currentID, graphNode)
		}
		current.symbols[name] = id
		if resolvedType != "" {
			current.types[name] = resolvedType
		}
		for _, sourceID := range e.referencedVariables(value, current.symbols) {
			if sourceID != id {
				e.b.AddFact(sourceID, graph.EdgeAssigns, id, "", kind, loc, nil)
			}
		}
		if value != nil {
			e.walk(value, current)
		}
	}
}

func (e *extractor) parseEnhancedFor(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if !isIdentifier(name) {
		return
	}
	typeText := strings.TrimSpace(e.text(node.ChildByFieldName("type")))
	loc := e.location(node.ChildByFieldName("name"))
	id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindVariable, Name: name,
		QualifiedName: fmt.Sprintf("%s@%d", qualify(current.container, name), loc.Line), Location: loc,
		Properties: map[string]string{"type": typeText}})
	current.symbols[name] = id
	if resolved := e.resolveType(typeText); resolved != "" {
		current.types[name] = resolved
	}
}

func (e *extractor) parseAssignment(node *treesitter.Node, current scope) {
	left, right := node.ChildByFieldName("left"), node.ChildByFieldName("right")
	targetID := current.symbols[strings.TrimSpace(e.text(left))]
	if targetID == "" && left != nil && left.Kind() == "field_access" {
		field := strings.TrimSpace(e.text(left.ChildByFieldName("field")))
		targetID = current.symbols[field]
	}
	if targetID == "" {
		return
	}
	for _, sourceID := range e.referencedVariables(right, current.symbols) {
		if sourceID != targetID {
			e.b.AddFact(sourceID, graph.EdgeAssigns, targetID, "", graph.KindVariable, e.location(node), nil)
		}
	}
}

func (e *extractor) parseReturn(node *treesitter.Node, current scope) {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		for _, sourceID := range e.referencedVariables(node.NamedChild(i), current.symbols) {
			e.b.AddFact(sourceID, graph.EdgeReturns, current.currentID, "", "", e.location(node), nil)
		}
	}
}

func (e *extractor) parseCall(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	object := strings.TrimSpace(e.text(node.ChildByFieldName("object")))
	callee := e.resolveCallee(object, name, current)
	if callee == "" {
		return
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	loc := e.location(node)
	args := namedChildren(node.ChildByFieldName("arguments"))
	if e.parseConfigRead(fromID, object, name, args, loc) {
		return
	}
	method := strings.ToLower(name)
	if len(args) > 0 {
		if route, ok := e.stringValue(args[0]); ok {
			if httpMethod := javaHTTPMethod(callee, method); httpMethod != "" {
				e.b.AddFact(fromID, graph.EdgeRequests, "", httpMethod+" "+route, graph.KindEndpoint, loc, nil)
				return
			}
			switch method {
			case "publish", "publishevent", "emit", "produce", "sendmessage":
				e.b.AddFact(fromID, graph.EdgePublishes, "", route, graph.KindEvent, loc, nil)
				return
			case "subscribe", "register", "consume":
				e.b.AddFact(fromID, graph.EdgeSubscribes, "", route, graph.KindEvent, loc, nil)
				return
			}
		}
	}
	for position, argument := range args {
		for _, sourceID := range e.referencedVariables(argument, current.symbols) {
			e.b.AddFact(sourceID, graph.EdgePasses, "", callee, "", loc,
				map[string]string{"argument": fmt.Sprint(position)})
		}
	}
	e.b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
}

func (e *extractor) parseConfigRead(fromID, object, method string, args []*treesitter.Node, loc graph.Location) bool {
	if len(args) == 0 || (method != "getenv" && method != "getProperty") {
		return false
	}
	if object != "System" && object != "java.lang.System" {
		return false
	}
	key, ok := e.stringValue(args[0])
	if !ok || key == "" {
		return false
	}
	e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
	return true
}

func (e *extractor) parseRouteAnnotations(node *treesitter.Node, methodID, qualified, prefix string) {
	annotations := e.annotations(node)
	var route, httpMethod string
	var routeNode *treesitter.Node
	for _, annotation := range annotations {
		name := strings.ToLower(graph.SimpleName(strings.TrimSpace(e.text(annotation.ChildByFieldName("name")))))
		value, _ := e.firstStringValue(annotation.ChildByFieldName("arguments"))
		switch name {
		case "getmapping":
			httpMethod, route, routeNode = "GET", value, annotation
		case "postmapping":
			httpMethod, route, routeNode = "POST", value, annotation
		case "putmapping":
			httpMethod, route, routeNode = "PUT", value, annotation
		case "patchmapping":
			httpMethod, route, routeNode = "PATCH", value, annotation
		case "deletemapping":
			httpMethod, route, routeNode = "DELETE", value, annotation
		case "requestmapping":
			httpMethod, route, routeNode = requestMappingMethod(e.text(annotation)), value, annotation
		case "path":
			route, routeNode = value, annotation
		case "get", "post", "put", "patch", "delete", "head", "options":
			httpMethod, routeNode = strings.ToUpper(name), annotation
		}
	}
	if route == "" {
		return
	}
	if httpMethod == "" {
		httpMethod = "ANY"
	}
	route = joinRoute(prefix, route)
	loc := e.location(routeNode)
	endpointID := e.b.AddNode(graph.Node{Kind: graph.KindEndpoint, Name: httpMethod + " " + route,
		QualifiedName: fmt.Sprintf("endpoint:%s %s@%s:%d", httpMethod, route, loc.Path, loc.Line),
		Location:      loc, Properties: map[string]string{"method": httpMethod, "route": route}})
	e.b.AddFact(methodID, graph.EdgeExposes, endpointID, "", graph.KindEndpoint, loc, nil)
	e.b.AddFact(endpointID, graph.EdgeHandledBy, methodID, "", graph.KindMethod, loc,
		map[string]string{"handler": qualified})
}

func (e *extractor) annotationPath(node *treesitter.Node) string {
	for _, annotation := range e.annotations(node) {
		name := strings.ToLower(graph.SimpleName(strings.TrimSpace(e.text(annotation.ChildByFieldName("name")))))
		if name == "requestmapping" || name == "path" {
			value, _ := e.firstStringValue(annotation.ChildByFieldName("arguments"))
			return value
		}
	}
	return ""
}

func (e *extractor) annotations(node *treesitter.Node) []*treesitter.Node {
	var result []*treesitter.Node
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		switch child.Kind() {
		case "annotation", "marker_annotation":
			result = append(result, child)
		case "modifiers":
			for j := uint(0); j < child.NamedChildCount(); j++ {
				candidate := child.NamedChild(j)
				if candidate.Kind() == "annotation" || candidate.Kind() == "marker_annotation" {
					result = append(result, candidate)
				}
			}
		}
	}
	return result
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
			name := strings.TrimSpace(e.text(current))
			if id := symbols[name]; id != "" && !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
		for i := uint(0); i < current.NamedChildCount(); i++ {
			visit(current.NamedChild(i))
		}
	}
	visit(node)
	sort.Strings(result)
	return result
}

func (e *extractor) inferType(node *treesitter.Node, current scope) string {
	if node == nil {
		return ""
	}
	switch node.Kind() {
	case "object_creation_expression":
		return e.resolveType(e.text(node.ChildByFieldName("type")))
	case "identifier":
		return current.types[strings.TrimSpace(e.text(node))]
	}
	return ""
}

func (e *extractor) resolveCallee(object, method string, current scope) string {
	if object == "" {
		if imported := e.staticImports[method]; imported != "" {
			return imported
		}
		if current.receiver != "" && e.methods[qualify(current.receiver, method)] {
			return current.receiver + "." + method
		}
		return method
	}
	if object == "this" {
		return qualify(current.receiver, method)
	}
	if object == "super" && current.superclass != "" {
		return qualify(current.superclass, method)
	}
	object = strings.TrimPrefix(object, "this.")
	if inferred := current.types[object]; inferred != "" {
		return inferred + "." + method
	}
	first, rest, _ := strings.Cut(object, ".")
	if inferred := current.types[first]; inferred != "" {
		if rest != "" {
			return inferred + "." + rest + "." + method
		}
		return inferred + "." + method
	}
	if resolved := e.resolveType(object); resolved != "" && resolved != object {
		return resolved + "." + method
	}
	return object + "." + method
}

func (e *extractor) resolveType(value string) string {
	name := baseType(value)
	if name == "" || isJavaPrimitive(name) || name == "var" {
		return ""
	}
	if strings.Contains(name, ".") {
		return name
	}
	if imported := e.imports[name]; imported != "" {
		return imported
	}
	if declared := e.declarations[name]; declared != "" {
		return declared
	}
	if isJavaLangType(name) {
		return "java.lang." + name
	}
	return name
}

func (e *extractor) firstStringValue(node *treesitter.Node) (string, bool) {
	if value, ok := e.stringValue(node); ok {
		return value, true
	}
	if node == nil {
		return "", false
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		if value, ok := e.firstStringValue(node.NamedChild(i)); ok {
			return value, true
		}
	}
	return "", false
}

func (e *extractor) stringValue(node *treesitter.Node) (string, bool) {
	if node == nil {
		return "", false
	}
	if node.Kind() != "string_literal" && node.Kind() != "multiline_string_literal" {
		return "", false
	}
	value := strings.TrimSpace(e.text(node))
	if strings.Contains(value, `\{`) {
		return "", false
	}
	return strings.Trim(value, "\""), true
}

func (e *extractor) text(node *treesitter.Node) string {
	if node == nil {
		return ""
	}
	return node.Utf8Text(e.source)
}

func (e *extractor) location(node *treesitter.Node) graph.Location {
	if node == nil {
		return graph.Location{Path: e.input.Path, Line: 1, Column: 1}
	}
	start, end := node.StartPosition(), node.EndPosition()
	return graph.Location{Path: e.input.Path, Line: int(start.Row) + 1, Column: int(start.Column) + 1, EndLine: int(end.Row) + 1}
}

func javaTypeKind(kind string) (graph.NodeKind, bool) {
	switch kind {
	case "class_declaration":
		return graph.KindClass, true
	case "interface_declaration", "annotation_type_declaration":
		return graph.KindInterface, true
	case "enum_declaration", "record_declaration":
		return graph.KindType, true
	default:
		return "", false
	}
}

func namedChildren(node *treesitter.Node) []*treesitter.Node {
	if node == nil {
		return nil
	}
	result := make([]*treesitter.Node, 0, node.NamedChildCount())
	for i := uint(0); i < node.NamedChildCount(); i++ {
		result = append(result, node.NamedChild(i))
	}
	return result
}

func qualify(container, name string) string {
	if container == "" {
		return name
	}
	if name == "" {
		return container
	}
	return container + "." + name
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func splitTopLevel(value string, separator rune) []string {
	var result []string
	depth, start := 0, 0
	for index, current := range value {
		switch current {
		case '<', '(', '[':
			depth++
		case '>', ')', ']':
			if depth > 0 {
				depth--
			}
		default:
			if current == separator && depth == 0 {
				result = append(result, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
		}
	}
	result = append(result, strings.TrimSpace(value[start:]))
	return result
}

func baseType(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "? extends ")
	value = strings.TrimPrefix(value, "? super ")
	value = strings.TrimSuffix(value, "...")
	value = strings.TrimRight(value, "[]")
	if index := strings.IndexByte(value, '<'); index >= 0 {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, current := range value {
		if index == 0 {
			if current != '_' && current != '$' && !unicode.IsLetter(current) {
				return false
			}
		} else if current != '_' && current != '$' && !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			return false
		}
	}
	return true
}

func isJavaPrimitive(value string) bool {
	switch value {
	case "boolean", "byte", "char", "double", "float", "int", "long", "short", "void":
		return true
	default:
		return false
	}
}

func isJavaLangType(value string) bool {
	switch value {
	case "Boolean", "Byte", "Character", "Class", "Double", "Enum", "Exception", "Float", "Integer", "Iterable", "Long", "Math", "Number", "Object", "Record", "RuntimeException", "Short", "String", "StringBuilder", "System", "Thread", "Throwable", "Void":
		return true
	default:
		return false
	}
}

func javaHTTPMethod(callee, method string) string {
	if !strings.Contains(callee, "RestTemplate.") {
		return ""
	}
	switch method {
	case "getforobject", "getforentity":
		return "GET"
	case "postforobject", "postforentity":
		return "POST"
	case "put":
		return "PUT"
	case "delete":
		return "DELETE"
	case "exchange":
		return "ANY"
	default:
		return ""
	}
}

func requestMappingMethod(value string) string {
	upper := strings.ToUpper(value)
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		if strings.Contains(upper, "REQUESTMETHOD."+method) {
			return method
		}
	}
	return "ANY"
}

func joinRoute(prefix, route string) string {
	if prefix == "" {
		return route
	}
	if route == "" {
		return prefix
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(route, "/")
}
