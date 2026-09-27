package swift

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	tsswift "github.com/tree-sitter/tree-sitter-swift/bindings/go"
)

type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "swift" }

func (*Parser) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".swift")
}

type scope struct {
	currentID string
	parentID  string
	container string
	receiver  string
	typeKind  string
	classBody bool
	symbols   map[string]string
	types     map[string]string
}

type extractor struct {
	b         *parserapi.Builder
	input     parserapi.Input
	source    []byte
	module    string
	imports   map[string]string
	declKinds map[string]graph.NodeKind
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "swift")
	p := treesitter.NewParser()
	defer p.Close()
	if err := p.SetLanguage(treesitter.NewLanguage(tsswift.Language())); err != nil {
		return b.Finish(), fmt.Errorf("load Swift grammar: %w", err)
	}
	tree := parserapi.ParseTreeSitter(ctx, p, input.Content)
	if tree == nil {
		return b.Finish(), errors.New("parse Swift source: parser returned no syntax tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		b.Diagnostic(int(root.StartPosition().Row)+1, "warning", "Swift contains syntax errors; indexed the recoverable tree")
	}
	e := &extractor{
		b: b, input: input, source: input.Content, module: parserapi.ModuleName(input.Path),
		imports: map[string]string{}, declKinds: map[string]graph.NodeKind{},
	}
	e.collectDeclarationKinds(root, "")
	e.walk(root, scope{currentID: b.FileID(), parentID: b.FileID(), symbols: map[string]string{}, types: map[string]string{}})
	return b.Finish(), nil
}

func (e *extractor) collectDeclarationKinds(node *treesitter.Node, container string) {
	if node == nil {
		return
	}
	if node.Kind() == "class_declaration" {
		name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
		declarationKind := e.declarationKind(node)
		if name != "" && declarationKind != "extension" {
			kind := graph.KindType
			if declarationKind == "class" || declarationKind == "actor" {
				kind = graph.KindClass
			}
			qualified := e.qualify(container, name)
			e.declKinds[name] = kind
			e.declKinds[qualified] = kind
			container = qualified
		}
	} else if node.Kind() == "protocol_declaration" {
		name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
		if name != "" {
			qualified := e.qualify(container, name)
			e.declKinds[name] = graph.KindInterface
			e.declKinds[qualified] = graph.KindInterface
			container = qualified
		}
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		e.collectDeclarationKinds(node.NamedChild(i), container)
	}
}

func (e *extractor) walk(node *treesitter.Node, current scope) {
	if node == nil {
		return
	}
	switch node.Kind() {
	case "import_declaration":
		e.parseImport(node)
		return
	case "class_declaration":
		e.parseNominalType(node, current)
		return
	case "protocol_declaration":
		e.parseProtocol(node, current)
		return
	case "function_declaration":
		e.parseFunction(node, current, graph.KindFunction, "")
		return
	case "protocol_function_declaration":
		e.parseFunction(node, current, graph.KindMethod, "")
		return
	case "init_declaration":
		e.parseFunction(node, current, graph.KindMethod, "init")
		return
	case "deinit_declaration":
		e.parseFunction(node, current, graph.KindMethod, "deinit")
		return
	case "property_declaration", "protocol_property_declaration":
		e.parseProperty(node, current)
	case "assignment":
		e.parseAssignment(node, current)
	case "control_transfer_statement":
		e.parseReturn(node, current)
	case "call_expression":
		e.parseCall(node, current)
	case "navigation_expression":
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
	var identifier *treesitter.Node
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child.Kind() == "identifier" {
			identifier = child
			break
		}
	}
	name := strings.TrimSpace(e.text(identifier))
	if name == "" {
		return
	}
	parts := strings.Split(name, ".")
	module := parts[0]
	e.imports[module] = module
	properties := map[string]string{}
	if len(parts) > 1 {
		properties["symbol"] = strings.Join(parts[1:], ".")
	}
	e.b.AddFact(e.b.FileID(), graph.EdgeImports, "", module, graph.KindModule, e.location(node), properties)
}

func (e *extractor) parseNominalType(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	declarationKind := e.declarationKind(node)
	qualified := e.qualify(current.container, name)
	if declarationKind == "extension" {
		qualified = e.qualifyType(name)
		if body := node.ChildByFieldName("body"); body != nil {
			e.walkChildren(body, scope{currentID: current.currentID, parentID: current.parentID,
				container: qualified, receiver: qualified, typeKind: declarationKind, classBody: true,
				symbols: map[string]string{}, types: map[string]string{}})
		}
		return
	}
	kind := graph.KindType
	if declarationKind == "class" || declarationKind == "actor" {
		kind = graph.KindClass
	}
	properties := map[string]string{"declaration": declarationKind}
	typeID := e.b.Declare(current.parentID, graph.Node{Kind: kind, Name: name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	e.parseInheritance(node, typeID, declarationKind)
	if body := node.ChildByFieldName("body"); body != nil {
		e.walkChildren(body, scope{currentID: typeID, parentID: typeID, container: qualified,
			receiver: qualified, typeKind: declarationKind, classBody: true,
			symbols: map[string]string{}, types: map[string]string{}})
	}
}

func (e *extractor) parseProtocol(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	qualified := e.qualify(current.container, name)
	protocolID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindInterface, Name: name,
		QualifiedName: qualified, Location: e.location(node), Properties: map[string]string{"declaration": "protocol"}})
	e.parseInheritance(node, protocolID, "protocol")
	if body := node.ChildByFieldName("body"); body != nil {
		e.walkChildren(body, scope{currentID: protocolID, parentID: protocolID, container: qualified,
			receiver: qualified, typeKind: "protocol", classBody: true,
			symbols: map[string]string{}, types: map[string]string{}})
	}
}

func (e *extractor) parseInheritance(node *treesitter.Node, fromID, declarationKind string) {
	index := 0
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child.Kind() != "inheritance_specifier" {
			continue
		}
		target := strings.TrimSpace(e.text(child.ChildByFieldName("inherits_from")))
		if target == "" {
			target = strings.TrimSpace(e.text(child))
		}
		target = strings.TrimSpace(strings.TrimPrefix(target, "@unchecked"))
		target = strings.TrimSpace(strings.TrimPrefix(target, "@unsafe"))
		if target == "" {
			continue
		}
		target = e.qualifyInheritanceTarget(target)
		edgeKind := graph.EdgeImplements
		if declarationKind == "protocol" {
			edgeKind = graph.EdgeExtends
		} else if declarationKind == "class" && index == 0 && e.declKinds[target] != graph.KindInterface && e.declKinds[graph.SimpleName(target)] != graph.KindInterface {
			edgeKind = graph.EdgeExtends
		}
		e.b.AddFact(fromID, edgeKind, "", target, "", e.location(child), nil)
		index++
	}
}

func (e *extractor) parseFunction(node *treesitter.Node, current scope, kind graph.NodeKind, fixedName string) {
	name := fixedName
	if name == "" {
		name = strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	}
	if name == "" {
		name = fmt.Sprintf("anonymous@%d", node.StartPosition().Row+1)
	}
	if current.classBody {
		kind = graph.KindMethod
	}
	qualified := e.qualify(current.container, name)
	properties := map[string]string{}
	if returnType := node.ChildByFieldName("return_type"); returnType != nil {
		properties["returns"] = strings.TrimSpace(e.text(returnType))
	}
	text := strings.TrimSpace(e.text(node))
	if strings.Contains(text[:min(len(text), 120)], " async") {
		properties["async"] = "true"
	}
	functionID := e.b.Declare(current.parentID, graph.Node{Kind: kind, Name: name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	functionScope := scope{currentID: functionID, parentID: functionID, container: qualified,
		receiver: current.receiver, typeKind: current.typeKind,
		symbols: cloneMap(current.symbols), types: cloneMap(current.types)}
	e.declareParameters(node, functionScope)
	if body := node.ChildByFieldName("body"); body != nil {
		e.walk(body, functionScope)
	}
}

func (e *extractor) declareParameters(node *treesitter.Node, current scope) {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		parameter := node.NamedChild(i)
		if parameter.Kind() != "parameter" {
			continue
		}
		nameNode := parameter.ChildByFieldName("name")
		name := strings.TrimSpace(e.text(nameNode))
		if name == "" || name == "_" {
			continue
		}
		typeText := strings.TrimSpace(e.text(parameter.ChildByFieldName("type")))
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		if external := strings.TrimSpace(e.text(parameter.ChildByFieldName("external_name"))); external != "" {
			properties["external_name"] = external
		}
		id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindParameter, Name: name,
			QualifiedName: current.container + "." + name, Location: e.location(parameter), Properties: properties})
		current.symbols[name] = id
		if typeText != "" {
			current.types[name] = e.qualifyType(typeText)
		}
	}
}

func (e *extractor) parseProperty(node *treesitter.Node, current scope) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	names := e.simpleIdentifiers(nameNode)
	if len(names) == 0 {
		return
	}
	typeText := e.typeText(node)
	value := node.ChildByFieldName("value")
	if typeText == "" {
		typeText = e.inferExpressionType(value, current)
	}
	for _, named := range names {
		name := named.name
		if name == "_" {
			continue
		}
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		loc := e.location(named.node)
		qualified := e.qualify(current.container, name)
		var id string
		if current.classBody {
			id = e.b.AddNode(graph.Node{Kind: graph.KindField, Name: name, QualifiedName: qualified,
				Location: loc, Properties: properties})
			e.b.AddFact(current.currentID, graph.EdgeHasField, id, "", "", loc, nil)
		} else {
			id = e.b.Declare(current.currentID, graph.Node{Kind: graph.KindVariable, Name: name,
				QualifiedName: fmt.Sprintf("%s@%d", qualified, loc.Line), Location: loc, Properties: properties})
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
}

func (e *extractor) parseAssignment(node *treesitter.Node, current scope) {
	target := node.ChildByFieldName("target")
	result := node.ChildByFieldName("result")
	if target == nil || result == nil {
		return
	}
	name := strings.TrimSpace(e.text(target))
	targetID := current.symbols[name]
	if targetID == "" {
		return
	}
	for _, sourceID := range e.referencedVariables(result, current.symbols) {
		e.b.AddFact(sourceID, graph.EdgeAssigns, targetID, "", graph.KindVariable, e.location(node), nil)
	}
}

func (e *extractor) parseReturn(node *treesitter.Node, current scope) {
	if !strings.HasPrefix(strings.TrimSpace(e.text(node)), "return") {
		return
	}
	result := node.ChildByFieldName("result")
	for _, sourceID := range e.referencedVariables(result, current.symbols) {
		e.b.AddFact(sourceID, graph.EdgeReturns, current.currentID, "", "", e.location(node), nil)
	}
}

func (e *extractor) parseCall(node *treesitter.Node, current scope) {
	calleeNode := e.callCallee(node)
	callee := strings.TrimSpace(e.text(calleeNode))
	if callee == "" {
		return
	}
	callee = e.resolveCallee(callee, current)
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	loc := e.location(node)
	arguments := e.callArguments(node)
	method := strings.ToLower(graph.SimpleName(strings.TrimRight(callee, "?!")))
	if callee == "ProcessInfo.processInfo.environment" && len(arguments) > 0 {
		if key, ok := e.stringValue(arguments[0]); ok && key != "" {
			e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
			return
		}
	}
	if e.parseHTTPRequest(fromID, callee, method, arguments, loc, current) {
		return
	}
	if len(arguments) > 0 {
		if event, ok := e.stringValue(arguments[0]); ok {
			switch method {
			case "publish", "publishevent", "emit", "produce", "postnotification":
				e.b.AddFact(fromID, graph.EdgePublishes, "", event, graph.KindEvent, loc, nil)
				return
			case "subscribe", "on", "consume", "addobserver":
				e.b.AddFact(fromID, graph.EdgeSubscribes, "", event, graph.KindEvent, loc, nil)
				return
			}
		}
	}
	for position, argument := range arguments {
		for _, sourceID := range e.referencedVariables(argument, current.symbols) {
			e.b.AddFact(sourceID, graph.EdgePasses, "", callee, "", loc,
				map[string]string{"argument": strconv.Itoa(position)})
		}
	}
	e.b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
}

func (e *extractor) parseHTTPRequest(fromID, callee, method string, arguments []*treesitter.Node, loc graph.Location, current scope) bool {
	if len(arguments) == 0 {
		return false
	}
	url, ok := e.firstStringValue(arguments[0])
	if !ok {
		return false
	}
	absolute := strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
	receiver := strings.Split(callee, ".")[0]
	receiverType := current.types[receiver]
	isClient := strings.Contains(callee, "URLSession") || strings.HasPrefix(callee, "AF.") ||
		strings.Contains(receiverType, "URLSession") || strings.Contains(receiverType, "HTTPClient") || absolute
	if isClient && (method == "data" || method == "datatask" || method == "request" || isHTTPMethod(method)) {
		httpMethod := "ANY"
		if isHTTPMethod(method) {
			httpMethod = strings.ToUpper(method)
		}
		for _, argument := range arguments[1:] {
			text := strings.ToLower(strings.TrimSpace(e.text(argument)))
			if strings.HasPrefix(text, "method:") {
				candidate := strings.Trim(strings.TrimSpace(strings.TrimPrefix(text, "method:")), ".")
				if isHTTPMethod(candidate) {
					httpMethod = strings.ToUpper(candidate)
				}
			}
		}
		e.b.AddFact(fromID, graph.EdgeRequests, "", httpMethod+" "+url, graph.KindEndpoint, loc, nil)
		return true
	}
	if isHTTPMethod(method) && strings.HasPrefix(url, "/") {
		base := strings.ToLower(strings.Split(callee, ".")[0])
		if base == "app" || base == "router" || base == "routes" {
			httpMethod := strings.ToUpper(method)
			endpointID := e.b.AddNode(graph.Node{Kind: graph.KindEndpoint, Name: httpMethod + " " + url,
				QualifiedName: fmt.Sprintf("endpoint:%s %s@%s:%d", httpMethod, url, loc.Path, loc.Line),
				Location:      loc, Properties: map[string]string{"method": httpMethod, "route": url}})
			e.b.AddFact(fromID, graph.EdgeExposes, endpointID, "", "", loc, nil)
			return true
		}
	}
	return false
}

func (e *extractor) callCallee(node *treesitter.Node) *treesitter.Node {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child.Kind() != "call_suffix" {
			return child
		}
	}
	return nil
}

func (e *extractor) callArguments(node *treesitter.Node) []*treesitter.Node {
	var result []*treesitter.Node
	var visit func(*treesitter.Node)
	visit = func(current *treesitter.Node) {
		if current == nil {
			return
		}
		if current.Kind() == "value_arguments" {
			for i := uint(0); i < current.NamedChildCount(); i++ {
				argument := current.NamedChild(i)
				value := argument.ChildByFieldName("value")
				if value == nil {
					value = argument
				}
				result = append(result, value)
			}
			return
		}
		for i := uint(0); i < current.NamedChildCount(); i++ {
			child := current.NamedChild(i)
			if child.Kind() == "call_suffix" || current.Kind() == "call_suffix" {
				visit(child)
			}
		}
	}
	visit(node)
	return result
}

func (e *extractor) parseEnvironmentRead(node *treesitter.Node, current scope) {
	text := strings.TrimSpace(e.text(node))
	const prefix = "ProcessInfo.processInfo.environment["
	start := strings.Index(text, prefix)
	if start < 0 {
		return
	}
	remainder := text[start+len(prefix):]
	end := strings.Index(remainder, "]")
	if end < 0 {
		return
	}
	key := parserapi.Unquote(strings.TrimSpace(remainder[:end]))
	if key == "" || strings.ContainsAny(key, "[]() ") {
		return
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, e.location(node), nil)
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
		if current.Kind() == "simple_identifier" {
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

type namedNode struct {
	name string
	node *treesitter.Node
}

func (e *extractor) simpleIdentifiers(node *treesitter.Node) []namedNode {
	var result []namedNode
	var visit func(*treesitter.Node)
	visit = func(current *treesitter.Node) {
		if current == nil {
			return
		}
		if current.Kind() == "simple_identifier" {
			result = append(result, namedNode{name: strings.Trim(strings.TrimSpace(e.text(current)), "`"), node: current})
			return
		}
		for i := uint(0); i < current.NamedChildCount(); i++ {
			visit(current.NamedChild(i))
		}
	}
	visit(node)
	return result
}

func (e *extractor) typeText(node *treesitter.Node) string {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child.Kind() == "type_annotation" {
			typeNode := child.ChildByFieldName("type")
			if typeNode != nil {
				return strings.TrimSpace(e.text(typeNode))
			}
			return strings.TrimSpace(strings.TrimPrefix(e.text(child), ":"))
		}
	}
	return ""
}

func (e *extractor) inferExpressionType(node *treesitter.Node, current scope) string {
	if node == nil {
		return ""
	}
	if node.Kind() == "call_expression" {
		callee := strings.TrimSpace(e.text(e.callCallee(node)))
		if isSwiftTypeName(callee) {
			return callee
		}
	}
	if node.Kind() == "simple_identifier" {
		return current.types[strings.TrimSpace(e.text(node))]
	}
	return ""
}

func (e *extractor) resolveCallee(callee string, current scope) string {
	callee = strings.TrimSpace(strings.TrimRight(callee, "?!"))
	if strings.HasPrefix(callee, "self.") && current.receiver != "" {
		return current.receiver + strings.TrimPrefix(callee, "self")
	}
	if receiver, method, ok := strings.Cut(callee, "."); ok {
		if inferred := current.types[receiver]; inferred != "" {
			return inferred + "." + method
		}
		return callee
	}
	if isSwiftBuiltin(callee) || isSwiftTypeName(callee) {
		return callee
	}
	return e.qualify("", callee)
}

func (e *extractor) qualifyType(typeText string) string {
	typeText = strings.TrimSpace(strings.TrimRight(typeText, "?!"))
	if typeText == "" || strings.ContainsAny(typeText, "<>{}[]()& ") || isSwiftPrimitive(typeText) || strings.Contains(typeText, ".") {
		return typeText
	}
	return e.qualify("", typeText)
}

func (e *extractor) qualifyInheritanceTarget(target string) string {
	if target == "" || strings.ContainsAny(target, "<>{}[]()& ") || strings.Contains(target, ".") {
		return target
	}
	if _, local := e.declKinds[target]; local {
		return e.qualify("", target)
	}
	return target
}

func (e *extractor) declarationKind(node *treesitter.Node) string {
	if kind := node.ChildByFieldName("declaration_kind"); kind != nil {
		return strings.TrimSpace(e.text(kind))
	}
	text := strings.TrimSpace(e.text(node))
	for _, candidate := range []string{"class", "struct", "enum", "actor", "extension"} {
		if strings.HasPrefix(text, candidate+" ") {
			return candidate
		}
	}
	return "type"
}

func (e *extractor) firstStringValue(node *treesitter.Node) (string, bool) {
	if value, ok := e.stringValue(node); ok {
		return value, true
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
	switch node.Kind() {
	case "line_string_literal", "multi_line_string_literal", "raw_string_literal":
		value := strings.TrimSpace(e.text(node))
		if strings.Contains(value, "\\(") {
			return "", false
		}
		return strings.Trim(value, "#\""), true
	default:
		return "", false
	}
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

func isHTTPMethod(method string) bool {
	switch method {
	case "get", "post", "put", "patch", "delete", "head", "options":
		return true
	default:
		return false
	}
}

func isSwiftTypeName(value string) bool {
	for _, r := range value {
		return unicode.IsUpper(r)
	}
	return false
}

func isSwiftBuiltin(value string) bool {
	switch value {
	case "assert", "debugPrint", "dump", "fatalError", "precondition", "preconditionFailure", "print", "readLine", "repeatElement", "sequence", "stride", "swap", "type", "withUnsafePointer", "withUnsafeMutablePointer":
		return true
	default:
		return false
	}
}

func isSwiftPrimitive(value string) bool {
	switch value {
	case "Any", "AnyObject", "Bool", "Character", "Double", "Float", "Int", "String", "Substring", "UInt", "Void", "Never":
		return true
	default:
		return false
	}
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
