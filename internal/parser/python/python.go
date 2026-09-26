package python

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	tspython "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "python" }

func (*Parser) Supports(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".pyi":
		return true
	default:
		return false
	}
}

type scope struct {
	currentID  string
	parentID   string
	container  string
	receiver   string
	receiverID string
	classBody  bool
	symbols    map[string]string
	types      map[string]string
}

type extractor struct {
	b       *parserapi.Builder
	input   parserapi.Input
	source  []byte
	module  string
	imports map[string]string
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "python")
	p := treesitter.NewParser()
	defer p.Close()
	if err := p.SetLanguage(treesitter.NewLanguage(tspython.Language())); err != nil {
		return b.Finish(), fmt.Errorf("load Python grammar: %w", err)
	}
	tree := p.ParseCtx(ctx, input.Content, nil)
	if tree == nil {
		return b.Finish(), fmt.Errorf("Python parser returned no syntax tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		b.Diagnostic(int(root.StartPosition().Row)+1, "warning", "Python contains syntax errors; indexed the recoverable tree")
	}
	e := &extractor{
		b: b, input: input, source: input.Content,
		module: pythonModuleName(input.Path), imports: map[string]string{},
	}
	e.walk(root, scope{currentID: b.FileID(), parentID: b.FileID(), symbols: map[string]string{}, types: map[string]string{}})
	return b.Finish(), nil
}

func (e *extractor) walk(node *treesitter.Node, current scope) {
	if node == nil {
		return
	}
	switch node.Kind() {
	case "import_statement", "import_from_statement", "future_import_statement":
		e.parseImport(node)
		return
	case "decorated_definition":
		e.parseDecoratedDefinition(node, current)
		return
	case "function_definition":
		e.parseFunction(node, current, nil)
		return
	case "class_definition":
		e.parseClass(node, current)
		return
	case "assignment":
		e.parseAssignment(node, current)
	case "augmented_assignment", "named_expression":
		e.parseReassignment(node, current)
	case "return_statement":
		e.parseReturn(node, current)
	case "call":
		e.parseCall(node, current)
	case "subscript":
		e.parseEnvironmentRead(node, current)
	}
	e.walkChildren(node, current)
}

func (e *extractor) walkChildren(node *treesitter.Node, current scope) {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		e.walk(node.NamedChild(i), current)
	}
}

func (e *extractor) parseDecoratedDefinition(node *treesitter.Node, current scope) {
	definition := node.ChildByFieldName("definition")
	if definition == nil {
		return
	}
	switch definition.Kind() {
	case "function_definition":
		e.parseFunction(definition, current, node)
	case "class_definition":
		e.parseClass(definition, current)
	}
}

func (e *extractor) parseImport(node *treesitter.Node) {
	if node.Kind() == "import_statement" {
		for i := uint(0); i < node.NamedChildCount(); i++ {
			child := node.NamedChild(i)
			name, alias := e.importName(child)
			if name == "" {
				continue
			}
			bound := alias
			resolved := name
			if bound == "" {
				bound = strings.Split(name, ".")[0]
				resolved = bound
			}
			e.imports[bound] = resolved
			e.addImportFact(node, name, bound, "")
		}
		return
	}

	moduleNode := node.ChildByFieldName("module_name")
	module := strings.TrimSpace(e.text(moduleNode))
	if node.Kind() == "future_import_statement" {
		module = "__future__"
	}
	skippedModule := false
	found := false
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if !skippedModule && moduleNode != nil && e.sameNode(child, moduleNode) {
			skippedModule = true
			continue
		}
		if child.Kind() == "wildcard_import" {
			e.addImportFact(node, module, "*", "*")
			found = true
			continue
		}
		name, alias := e.importName(child)
		if name == "" {
			continue
		}
		bound := alias
		if bound == "" {
			bound = graph.SimpleName(name)
		}
		resolved := joinPythonName(module, name)
		e.imports[bound] = resolved
		e.addImportFact(node, module, bound, name)
		found = true
	}
	if !found && module != "" {
		e.addImportFact(node, module, "", "")
	}
}

func (e *extractor) importName(node *treesitter.Node) (string, string) {
	if node == nil {
		return "", ""
	}
	if node.Kind() == "aliased_import" {
		return strings.TrimSpace(e.text(node.ChildByFieldName("name"))), strings.TrimSpace(e.text(node.ChildByFieldName("alias")))
	}
	switch node.Kind() {
	case "dotted_name", "identifier":
		return strings.TrimSpace(e.text(node)), ""
	default:
		return "", ""
	}
}

func (e *extractor) addImportFact(node *treesitter.Node, module, alias, symbol string) {
	if module == "" {
		return
	}
	properties := map[string]string{}
	if alias != "" {
		properties["alias"] = alias
	}
	if symbol != "" {
		properties["symbol"] = symbol
	}
	e.b.AddFact(e.b.FileID(), graph.EdgeImports, "", module, graph.KindModule, e.location(node), properties)
}

func (e *extractor) parseFunction(node *treesitter.Node, current scope, decorated *treesitter.Node) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	kind := graph.KindFunction
	if current.classBody {
		kind = graph.KindMethod
	}
	qualified := e.qualify(current.container, name)
	properties := map[string]string{}
	if parameters := node.ChildByFieldName("parameters"); parameters != nil {
		properties["parameters"] = strings.TrimSpace(e.text(parameters))
	}
	if returnType := node.ChildByFieldName("return_type"); returnType != nil {
		properties["returns"] = strings.TrimSpace(e.text(returnType))
	}
	if strings.HasPrefix(strings.TrimSpace(e.text(node)), "async ") {
		properties["async"] = "true"
	}
	functionID := e.b.Declare(current.parentID, graph.Node{Kind: kind, Name: name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	functionScope := scope{currentID: functionID, parentID: functionID, container: qualified,
		receiver: current.receiver, receiverID: current.receiverID, symbols: map[string]string{}, types: map[string]string{}}
	e.declareParameters(node.ChildByFieldName("parameters"), functionScope, kind == graph.KindMethod)
	if decorated != nil {
		e.parseDecorators(decorated, functionID, qualified)
	}
	if body := node.ChildByFieldName("body"); body != nil {
		e.walk(body, functionScope)
	}
}

func (e *extractor) parseClass(node *treesitter.Node, current scope) {
	name := strings.TrimSpace(e.text(node.ChildByFieldName("name")))
	if name == "" {
		return
	}
	qualified := e.qualify(current.container, name)
	classID := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindClass, Name: name,
		QualifiedName: qualified, Location: e.location(node)})
	if superclasses := node.ChildByFieldName("superclasses"); superclasses != nil {
		for _, superclass := range namedChildren(superclasses) {
			if superclass.Kind() == "keyword_argument" {
				continue
			}
			target := e.resolveName(strings.TrimSpace(e.text(superclass)), current)
			if target != "" {
				e.b.AddFact(classID, graph.EdgeExtends, "", target, graph.KindClass, e.location(superclass), nil)
			}
		}
	}
	if body := node.ChildByFieldName("body"); body != nil {
		e.walk(body, scope{currentID: classID, parentID: classID, container: qualified,
			receiver: qualified, receiverID: classID, classBody: true, symbols: map[string]string{}, types: map[string]string{}})
	}
}

func (e *extractor) declareParameters(parameters *treesitter.Node, current scope, method bool) {
	if parameters == nil {
		return
	}
	for _, parameter := range namedChildren(parameters) {
		name := e.parameterName(parameter)
		if name == "" || (method && (name == "self" || name == "cls")) {
			continue
		}
		typeText := strings.TrimSpace(e.text(parameter.ChildByFieldName("type")))
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindParameter, Name: name,
			QualifiedName: current.container + "." + name, Location: e.location(parameter), Properties: properties})
		current.symbols[name] = id
		if resolved := e.resolveType(typeText); resolved != "" {
			current.types[name] = resolved
		}
	}
}

func (e *extractor) parameterName(node *treesitter.Node) string {
	if node == nil {
		return ""
	}
	if name := node.ChildByFieldName("name"); name != nil {
		return strings.TrimLeft(strings.TrimSpace(e.text(name)), "*")
	}
	if node.Kind() == "identifier" {
		return strings.TrimSpace(e.text(node))
	}
	var find func(*treesitter.Node) string
	find = func(current *treesitter.Node) string {
		if current == nil {
			return ""
		}
		if current.Kind() == "identifier" {
			return strings.TrimSpace(e.text(current))
		}
		for i := uint(0); i < current.NamedChildCount(); i++ {
			if name := find(current.NamedChild(i)); name != "" {
				return name
			}
		}
		return ""
	}
	return find(node)
}

func (e *extractor) parseAssignment(node *treesitter.Node, current scope) {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	name := strings.TrimSpace(e.text(left))
	instanceField := false
	if left != nil && left.Kind() == "attribute" && current.receiverID != "" {
		owner := strings.TrimSpace(e.text(left.ChildByFieldName("object")))
		if owner == "self" || owner == "cls" {
			name = strings.TrimSpace(e.text(left.ChildByFieldName("attribute")))
			instanceField = true
		}
	}
	if !isIdentifier(name) || current.currentID == "" {
		return
	}
	typeText := strings.TrimSpace(e.text(node.ChildByFieldName("type")))
	resolvedType := e.resolveType(typeText)
	if resolvedType == "" {
		resolvedType = e.inferExpressionType(right, current)
	}
	properties := map[string]string{}
	if typeText != "" {
		properties["type"] = typeText
	}
	loc := e.location(node)
	kind := graph.KindVariable
	if current.classBody || instanceField {
		kind = graph.KindField
	}
	qualified := e.qualify(current.container, name)
	if instanceField {
		qualified = current.receiver + "." + name
	}
	if kind == graph.KindVariable {
		qualified = fmt.Sprintf("%s@%d", qualified, loc.Line)
	}
	graphNode := graph.Node{Kind: kind, Name: name, QualifiedName: qualified,
		Location: loc, Properties: properties}
	var id string
	if kind == graph.KindField {
		id = e.b.AddNode(graphNode)
		ownerID := current.currentID
		if instanceField {
			ownerID = current.receiverID
		}
		e.b.AddFact(ownerID, graph.EdgeHasField, id, "", kind, loc, nil)
	} else {
		id = e.b.Declare(current.currentID, graphNode)
	}
	current.symbols[name] = id
	if instanceField {
		current.symbols["self."+name] = id
	}
	if resolvedType != "" {
		current.types[name] = resolvedType
	}
	for _, sourceID := range e.referencedVariables(right, current.symbols) {
		if sourceID != id {
			e.b.AddFact(sourceID, graph.EdgeAssigns, id, "", kind, loc, nil)
		}
	}
}

func (e *extractor) parseReassignment(node *treesitter.Node, current scope) {
	left := node.ChildByFieldName("left")
	if left == nil {
		left = node.ChildByFieldName("name")
	}
	right := node.ChildByFieldName("right")
	if right == nil {
		right = node.ChildByFieldName("value")
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
	for _, child := range namedChildren(node) {
		for _, sourceID := range e.referencedVariables(child, current.symbols) {
			e.b.AddFact(sourceID, graph.EdgeReturns, current.currentID, "", "", e.location(node), nil)
		}
	}
}

func (e *extractor) parseCall(node *treesitter.Node, current scope) {
	calleeNode := node.ChildByFieldName("function")
	callee := e.resolveName(strings.TrimSpace(e.text(calleeNode)), current)
	if callee == "" {
		return
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	loc := e.location(node)
	args := namedChildren(node.ChildByFieldName("arguments"))
	method := strings.ToLower(graph.SimpleName(callee))

	if e.parseConfigCall(fromID, callee, args, loc) {
		return
	}
	if method == "request" && len(args) > 1 && isHTTPClient(callee) {
		httpMethod, methodOK := e.literalString(argumentValue(args[0]))
		route, routeOK := e.literalString(argumentValue(args[1]))
		if methodOK && routeOK {
			e.b.AddFact(fromID, graph.EdgeRequests, "", strings.ToUpper(httpMethod)+" "+route, graph.KindEndpoint, loc, nil)
			return
		}
	}
	if isHTTPMethod(method) && len(args) > 0 && isHTTPClient(callee) {
		if route, ok := e.literalString(argumentValue(args[0])); ok {
			e.b.AddFact(fromID, graph.EdgeRequests, "", strings.ToUpper(method)+" "+route, graph.KindEndpoint, loc, nil)
			return
		}
	}
	if isHTTPMethod(method) && len(args) > 1 {
		if route, ok := e.literalString(argumentValue(args[0])); ok && strings.HasPrefix(route, "/") {
			endpointID := e.addEndpoint(loc, strings.ToUpper(method), route)
			e.b.AddFact(fromID, graph.EdgeExposes, endpointID, "", "", loc, nil)
			handler := strings.TrimSpace(e.text(argumentValue(args[len(args)-1])))
			if handler != "" {
				e.b.AddFact(endpointID, graph.EdgeHandledBy, "", e.resolveName(handler, current), "", loc, nil)
			}
			return
		}
	}
	if len(args) > 0 {
		if event, ok := e.literalString(argumentValue(args[0])); ok {
			switch method {
			case "publish", "publish_event", "emit", "produce":
				e.b.AddFact(fromID, graph.EdgePublishes, "", event, graph.KindEvent, loc, nil)
				return
			case "subscribe", "on", "consume":
				e.b.AddFact(fromID, graph.EdgeSubscribes, "", event, graph.KindEvent, loc, nil)
				return
			}
		}
	}
	for position, argument := range args {
		for _, sourceID := range e.referencedVariables(argument, current.symbols) {
			e.b.AddFact(sourceID, graph.EdgePasses, "", callee, "", loc, map[string]string{"argument": fmt.Sprint(position)})
		}
	}
	e.b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
}

func (e *extractor) parseConfigCall(fromID, callee string, args []*treesitter.Node, loc graph.Location) bool {
	if len(args) == 0 {
		return false
	}
	if callee != "os.getenv" && callee != "os.environ.get" {
		return false
	}
	key, ok := e.literalString(argumentValue(args[0]))
	if !ok || key == "" {
		return false
	}
	e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
	return true
}

func (e *extractor) parseEnvironmentRead(node *treesitter.Node, current scope) {
	value := e.resolveName(strings.TrimSpace(e.text(node.ChildByFieldName("value"))), current)
	if value != "os.environ" {
		return
	}
	key, ok := e.literalString(node.ChildByFieldName("subscript"))
	if !ok || key == "" {
		return
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, e.location(node), nil)
}

func (e *extractor) parseDecorators(node *treesitter.Node, functionID, qualified string) {
	for _, child := range namedChildren(node) {
		if child.Kind() != "decorator" {
			continue
		}
		call := firstDescendant(child, "call")
		if call == nil {
			continue
		}
		callee := e.resolveName(strings.TrimSpace(e.text(call.ChildByFieldName("function"))), scope{})
		method := strings.ToLower(graph.SimpleName(callee))
		args := namedChildren(call.ChildByFieldName("arguments"))
		if len(args) == 0 {
			continue
		}
		value, ok := e.literalString(argumentValue(args[0]))
		if !ok {
			continue
		}
		loc := e.location(child)
		if isHTTPMethod(method) && strings.HasPrefix(value, "/") {
			endpointID := e.addEndpoint(loc, strings.ToUpper(method), value)
			e.b.AddFact(functionID, graph.EdgeExposes, endpointID, "", "", loc, nil)
			e.b.AddFact(endpointID, graph.EdgeHandledBy, functionID, "", graph.KindFunction, loc,
				map[string]string{"handler": qualified})
			continue
		}
		switch method {
		case "subscribe", "on", "consume":
			e.b.AddFact(functionID, graph.EdgeSubscribes, "", value, graph.KindEvent, loc, nil)
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

func (e *extractor) inferExpressionType(node *treesitter.Node, current scope) string {
	if node == nil {
		return ""
	}
	if node.Kind() == "identifier" {
		return current.types[strings.TrimSpace(e.text(node))]
	}
	if node.Kind() != "call" {
		return ""
	}
	callee := e.resolveName(strings.TrimSpace(e.text(node.ChildByFieldName("function"))), current)
	if callee == "" {
		return ""
	}
	last := graph.SimpleName(callee)
	if len(last) > 0 && unicode.IsUpper(rune(last[0])) {
		if !strings.Contains(callee, ".") {
			return e.qualify("", callee)
		}
		return callee
	}
	return ""
}

func (e *extractor) resolveType(typeText string) string {
	typeText = strings.TrimSpace(strings.Trim(typeText, "'\""))
	if typeText == "" {
		return ""
	}
	typeText = strings.TrimPrefix(typeText, "typing.")
	if strings.Contains(typeText, "|") {
		for _, candidate := range strings.Split(typeText, "|") {
			candidate = strings.TrimSpace(candidate)
			if candidate != "None" {
				typeText = candidate
				break
			}
		}
	}
	if open := strings.Index(typeText, "["); open >= 0 {
		outer := typeText[:open]
		inner := strings.TrimSuffix(typeText[open+1:], "]")
		switch outer {
		case "Optional", "ClassVar", "Final", "Annotated":
			typeText = strings.Split(inner, ",")[0]
		default:
			return ""
		}
	}
	typeText = strings.TrimSpace(typeText)
	if isBuiltinPythonType(typeText) {
		return ""
	}
	resolved := e.resolveName(typeText, scope{})
	if resolved == typeText && isIdentifier(typeText) && len(typeText) > 0 && unicode.IsUpper(rune(typeText[0])) {
		return e.qualify("", typeText)
	}
	return resolved
}

func (e *extractor) resolveName(name string, current scope) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if current.receiver != "" && (strings.HasPrefix(name, "self.") || strings.HasPrefix(name, "cls.")) {
		return current.receiver + name[strings.Index(name, "."):]
	}
	first := name
	rest := ""
	if dot := strings.Index(name, "."); dot >= 0 {
		first, rest = name[:dot], name[dot:]
	}
	if resolved := current.types[first]; resolved != "" {
		return resolved + rest
	}
	if imported := e.imports[first]; imported != "" {
		return imported + rest
	}
	return name
}

func (e *extractor) literalString(node *treesitter.Node) (string, bool) {
	if node == nil || (node.Kind() != "string" && node.Kind() != "concatenated_string") {
		return "", false
	}
	if firstDescendant(node, "interpolation") != nil {
		return "", false
	}
	text := strings.TrimSpace(e.text(node))
	prefixEnd := 0
	for prefixEnd < len(text) && strings.ContainsRune("rRuUbBfF", rune(text[prefixEnd])) {
		if text[prefixEnd] == 'f' || text[prefixEnd] == 'F' {
			return "", false
		}
		prefixEnd++
	}
	text = text[prefixEnd:]
	if len(text) >= 6 && ((strings.HasPrefix(text, "\"\"\"") && strings.HasSuffix(text, "\"\"\"")) ||
		(strings.HasPrefix(text, "'''") && strings.HasSuffix(text, "'''"))) {
		return text[3 : len(text)-3], true
	}
	if len(text) >= 2 && ((text[0] == '\'' && text[len(text)-1] == '\'') || (text[0] == '"' && text[len(text)-1] == '"')) {
		return text[1 : len(text)-1], true
	}
	return "", false
}

func (e *extractor) addEndpoint(loc graph.Location, method, route string) string {
	name := method + " " + route
	return e.b.AddNode(graph.Node{Kind: graph.KindEndpoint, Name: name,
		QualifiedName: fmt.Sprintf("endpoint:%s@%s:%d", name, loc.Path, loc.Line), Location: loc,
		Properties: map[string]string{"method": method, "route": route}})
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

func (e *extractor) sameNode(left, right *treesitter.Node) bool {
	return left != nil && right != nil && left.StartByte() == right.StartByte() && left.EndByte() == right.EndByte() && left.Kind() == right.Kind()
}

func pythonModuleName(path string) string {
	path = filepath.ToSlash(path)
	path = strings.TrimSuffix(path, filepath.Ext(path))
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimSuffix(path, "/__init__")
	if path == "__init__" || path == "." {
		return ""
	}
	return strings.ReplaceAll(path, "/", ".")
}

func joinPythonName(module, name string) string {
	module = strings.TrimSuffix(strings.TrimSpace(module), ".")
	name = strings.TrimPrefix(strings.TrimSpace(name), ".")
	if module == "" {
		return name
	}
	if name == "" {
		return module
	}
	return module + "." + name
}

func namedChildren(node *treesitter.Node) []*treesitter.Node {
	if node == nil {
		return nil
	}
	children := make([]*treesitter.Node, 0, node.NamedChildCount())
	for i := uint(0); i < node.NamedChildCount(); i++ {
		children = append(children, node.NamedChild(i))
	}
	return children
}

func argumentValue(node *treesitter.Node) *treesitter.Node {
	if node != nil && node.Kind() == "keyword_argument" {
		return node.ChildByFieldName("value")
	}
	return node
}

func firstDescendant(node *treesitter.Node, kind string) *treesitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child.Kind() == kind {
			return child
		}
		if found := firstDescendant(child, kind); found != nil {
			return found
		}
	}
	return nil
}

func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 && !(r == '_' || unicode.IsLetter(r)) {
			return false
		}
		if i > 0 && !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
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

func isHTTPClient(callee string) bool {
	callee = strings.ToLower(callee)
	return strings.HasPrefix(callee, "requests.") || strings.HasPrefix(callee, "httpx.") ||
		strings.HasPrefix(callee, "aiohttp.") || strings.HasPrefix(callee, "urllib3.")
}

func isBuiltinPythonType(value string) bool {
	switch value {
	case "str", "int", "float", "bool", "bytes", "bytearray", "complex", "list", "tuple", "dict", "set", "frozenset", "range", "object", "None", "Any":
		return true
	default:
		return false
	}
}
