package typescript

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	tstypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "typescript" }

func (*Parser) Supports(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".tsx":
		return true
	default:
		return false
	}
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
	b      *parserapi.Builder
	input  parserapi.Input
	source []byte
	module string
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "typescript")
	p := treesitter.NewParser()
	defer p.Close()
	language := tstypescript.LanguageTypescript()
	if strings.EqualFold(filepath.Ext(input.Path), ".tsx") {
		language = tstypescript.LanguageTSX()
	}
	if err := p.SetLanguage(treesitter.NewLanguage(language)); err != nil {
		return b.Finish(), fmt.Errorf("load TypeScript grammar: %w", err)
	}
	tree := p.ParseCtx(ctx, input.Content, nil)
	if tree == nil {
		return b.Finish(), fmt.Errorf("TypeScript parser returned no syntax tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		b.Diagnostic(int(root.StartPosition().Row)+1, "warning", "TypeScript contains syntax errors; indexed the recoverable tree")
	}
	e := &extractor{b: b, input: input, source: input.Content, module: parserapi.ModuleName(input.Path)}
	e.walk(root, scope{currentID: b.FileID(), parentID: b.FileID(), symbols: map[string]string{}, types: map[string]string{}})
	return b.Finish(), nil
}

func (e *extractor) walk(node *treesitter.Node, current scope) {
	if node == nil {
		return
	}
	switch node.Kind() {
	case "import_statement":
		e.parseImport(node)
		return
	case "function_declaration", "generator_function_declaration":
		e.parseFunction(node, current, graph.KindFunction)
		return
	case "class_declaration", "abstract_class_declaration":
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
	e.b.AddFact(e.b.FileID(), graph.EdgeImports, "", module, graph.KindModule, e.location(node), nil)
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
		return
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
					e.b.AddFact(fromID, edgeKind, "", target, "", e.location(current), nil)
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
			callee = inferred + "." + method
		}
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	loc := e.location(node)
	args := namedArguments(node.ChildByFieldName("arguments"))
	method := strings.ToLower(graph.SimpleName(strings.TrimSuffix(callee, "?")))
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
	e.b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
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
		return strings.TrimPrefix(strings.TrimSpace(e.text(typed)), ":")
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child.Kind() == "type_annotation" {
			return strings.TrimPrefix(strings.TrimSpace(e.text(child)), ":")
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
	return e.module + "." + typeText
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
