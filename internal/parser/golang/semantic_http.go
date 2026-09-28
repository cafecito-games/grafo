package golang

import (
	goast "go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"golang.org/x/tools/go/packages"
)

const (
	httpSummaryLimit     = 32
	httpAlternativeLimit = 8
)

type httpSemanticFunction struct {
	object    *types.Func
	decl      *goast.FuncDecl
	path      string
	qualified string
	kind      graph.NodeKind
}

type httpStringValue struct {
	text        string
	unknownBase bool
	overflow    bool
}

type httpRequestValue struct {
	method           httpStringValue
	route            httpStringValue
	sink             string
	source           string
	wrapperChain     []string
	location         graph.Location
	conditional      bool
	authorityUnknown bool
	overflow         bool
}

type httpObjectValue struct {
	fields map[types.Object]httpValue
}

type httpValue struct {
	strings       []httpStringValue
	requests      []httpRequestValue
	objects       []*httpObjectValue
	requestObject bool
	client        bool
}

type httpEnvironment map[types.Object]httpValue

type httpExecution struct {
	stack map[*types.Func]bool
}

type httpEvalResult struct {
	values  []httpValue
	effects []httpRequestValue
}

type httpSemanticAnalyzer struct {
	root      string
	pkg       *packages.Package
	views     map[string]SemanticView
	functions map[*types.Func]*httpSemanticFunction
	relevant  map[*types.Func]bool
}

func collectHTTPPackageViews(root string, pkg *packages.Package, views map[string]SemanticView) {
	if pkg == nil || pkg.TypesInfo == nil || pkg.Types == nil || pkg.Fset == nil {
		return
	}
	analyzer := &httpSemanticAnalyzer{
		root: root, pkg: pkg, views: views, functions: map[*types.Func]*httpSemanticFunction{},
		relevant: map[*types.Func]bool{},
	}
	analyzer.collectFunctions()
	analyzer.collectRelevantFunctions()
	analyzer.composeRoots()
}

func (a *httpSemanticAnalyzer) collectFunctions() {
	for _, file := range a.pkg.Syntax {
		for _, declaration := range file.Decls {
			decl, ok := declaration.(*goast.FuncDecl)
			if !ok || decl.Body == nil {
				continue
			}
			object, _ := a.pkg.TypesInfo.Defs[decl.Name].(*types.Func)
			if object == nil {
				continue
			}
			position := a.pkg.Fset.Position(decl.Pos())
			path, ok := relativeSourcePath(a.root, position.Filename)
			if !ok {
				continue
			}
			kind := graph.KindFunction
			if signature, _ := object.Type().(*types.Signature); signature != nil && signature.Recv() != nil {
				kind = graph.KindMethod
			}
			a.functions[object] = &httpSemanticFunction{
				object: object, decl: decl, path: path, qualified: objectTarget(object), kind: kind,
			}
		}
	}
}

func (a *httpSemanticAnalyzer) collectRelevantFunctions() {
	for _, function := range a.functions {
		goast.Inspect(function.decl.Body, func(node goast.Node) bool {
			call, ok := node.(*goast.CallExpr)
			if !ok {
				return true
			}
			target := a.callTarget(call.Fun)
			if isHTTPSemanticTarget(target) || a.isHTTPClientDo(call) {
				a.relevant[function.object] = true
			}
			return true
		})
	}
	changed := true
	for changed {
		changed = false
		for _, function := range a.functions {
			if a.relevant[function.object] {
				continue
			}
			goast.Inspect(function.decl.Body, func(node goast.Node) bool {
				call, ok := node.(*goast.CallExpr)
				if !ok {
					return true
				}
				if local := a.localFunction(call.Fun); local != nil && a.relevant[local.object] {
					a.relevant[function.object] = true
					changed = true
					return false
				}
				return true
			})
		}
	}
}

func (a *httpSemanticAnalyzer) composeRoots() {
	called := map[*types.Func]bool{}
	callees := map[*types.Func][]*types.Func{}
	for _, function := range a.functions {
		goast.Inspect(function.decl.Body, func(node goast.Node) bool {
			call, ok := node.(*goast.CallExpr)
			if !ok {
				return true
			}
			if target := a.localFunction(call.Fun); target != nil {
				called[target.object] = true
				callees[function.object] = append(callees[function.object], target.object)
			}
			return true
		})
	}
	var roots []*httpSemanticFunction
	for object, function := range a.functions {
		if !called[object] && a.relevant[object] {
			roots = append(roots, function)
		}
	}
	covered := map[*types.Func]bool{}
	var markCovered func(*types.Func)
	markCovered = func(object *types.Func) {
		if covered[object] {
			return
		}
		covered[object] = true
		for _, target := range callees[object] {
			markCovered(target)
		}
	}
	for _, root := range roots {
		markCovered(root.object)
	}
	var functions []*httpSemanticFunction
	for _, function := range a.functions {
		if a.relevant[function.object] {
			functions = append(functions, function)
		}
	}
	sort.Slice(functions, func(i, j int) bool { return functions[i].qualified < functions[j].qualified })
	for _, function := range functions {
		if covered[function.object] {
			continue
		}
		roots = append(roots, function)
		markCovered(function.object)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].qualified < roots[j].qualified })
	seen := map[string]bool{}
	for _, root := range roots {
		result := a.executeFunction(&httpExecution{stack: map[*types.Func]bool{}}, root, nil, false)
		for _, request := range result.effects {
			a.materialize(root, request, seen)
		}
	}
	for path, view := range a.views {
		sort.Slice(view.HTTPRequests, func(i, j int) bool {
			left, right := view.HTTPRequests[i], view.HTTPRequests[j]
			if left.Location.Line != right.Location.Line {
				return left.Location.Line < right.Location.Line
			}
			if left.Location.Column != right.Location.Column {
				return left.Location.Column < right.Location.Column
			}
			if left.Method != right.Method {
				return left.Method < right.Method
			}
			return left.Route < right.Route
		})
		a.views[path] = view
	}
}

func (a *httpSemanticAnalyzer) executeFunction(execution *httpExecution, function *httpSemanticFunction,
	arguments []httpValue, conditional bool,
) httpEvalResult {
	if function == nil || function.decl.Body == nil {
		return httpEvalResult{}
	}
	if len(execution.stack) >= httpSummaryLimit || execution.stack[function.object] {
		a.diagnostic(a.location(function.path, function.decl),
			"recursive HTTP wrapper composition exceeded the bounded package summary")
		return httpEvalResult{}
	}
	execution.stack[function.object] = true
	defer delete(execution.stack, function.object)
	environment := httpEnvironment{}
	argument := 0
	bind := func(field *goast.FieldList, receiver bool) {
		if field == nil {
			return
		}
		for _, item := range field.List {
			for _, name := range item.Names {
				object := a.pkg.TypesInfo.Defs[name]
				if argument < len(arguments) {
					environment[object] = cloneHTTPValue(arguments[argument])
				} else if receiver {
					environment[object] = httpValue{objects: []*httpObjectValue{{fields: map[types.Object]httpValue{}}}}
				} else if a.isHTTPClient(a.pkg.TypesInfo.TypeOf(item.Type)) {
					environment[object] = httpValue{client: true}
				}
				argument++
			}
		}
	}
	bind(function.decl.Recv, true)
	bind(function.decl.Type.Params, false)
	result, _ := a.executeStatements(execution, function, function.decl.Body.List, environment, conditional)
	return result
}

func isHTTPSemanticTarget(target string) bool {
	switch target {
	case "net/http.NewRequest", "net/http.NewRequestWithContext", "net/http.Get", "net/http.Head", "net/http.Post", "net/http.PostForm":
		return true
	default:
		return false
	}
}

func (a *httpSemanticAnalyzer) executeStatements(execution *httpExecution, function *httpSemanticFunction,
	statements []goast.Stmt, environment httpEnvironment, conditional bool,
) (httpEvalResult, bool) {
	result := httpEvalResult{}
	for _, statement := range statements {
		switch value := statement.(type) {
		case *goast.AssignStmt:
			evaluated := a.evaluateExpressions(execution, function, value.Rhs, environment, conditional)
			result.effects = append(result.effects, evaluated.effects...)
			for index, lhs := range value.Lhs {
				if index >= len(evaluated.values) {
					break
				}
				a.assign(execution, function, lhs, evaluated.values[index], environment, conditional)
			}
		case *goast.DeclStmt:
			declaration, _ := value.Decl.(*goast.GenDecl)
			if declaration == nil {
				continue
			}
			for _, raw := range declaration.Specs {
				spec, _ := raw.(*goast.ValueSpec)
				if spec == nil {
					continue
				}
				evaluated := a.evaluateExpressions(execution, function, spec.Values, environment, conditional)
				result.effects = append(result.effects, evaluated.effects...)
				for index, name := range spec.Names {
					if index < len(evaluated.values) {
						environment[a.pkg.TypesInfo.Defs[name]] = cloneHTTPValue(evaluated.values[index])
					}
				}
			}
		case *goast.ExprStmt:
			evaluated := a.evaluateExpression(execution, function, value.X, environment, conditional)
			result.effects = append(result.effects, evaluated.effects...)
		case *goast.ReturnStmt:
			evaluated := a.evaluateExpressions(execution, function, value.Results, environment, conditional)
			result.values = append(result.values, evaluated.values...)
			result.effects = append(result.effects, evaluated.effects...)
			return result, true
		case *goast.BlockStmt:
			child, stopped := a.executeStatements(execution, function, value.List, environment, conditional)
			result.effects = append(result.effects, child.effects...)
			if stopped {
				result.values = append(result.values, child.values...)
				return result, true
			}
		case *goast.IfStmt:
			if value.Init != nil {
				initResult, _ := a.executeStatements(execution, function, []goast.Stmt{value.Init}, environment, conditional)
				result.effects = append(result.effects, initResult.effects...)
			}
			left := cloneHTTPEnvironment(environment)
			child, _ := a.executeStatements(execution, function, value.Body.List, left, true)
			markHTTPConditional(child.effects)
			result.effects = append(result.effects, child.effects...)
			right := cloneHTTPEnvironment(environment)
			if value.Else != nil {
				child, _ = a.executeStatements(execution, function, httpStatementList(value.Else), right, true)
				markHTTPConditional(child.effects)
				result.effects = append(result.effects, child.effects...)
			}
			mergeHTTPEnvironments(environment, left, right)
		case *goast.ForStmt:
			branch := cloneHTTPEnvironment(environment)
			child, _ := a.executeStatements(execution, function, value.Body.List, branch, true)
			markHTTPConditional(child.effects)
			result.effects = append(result.effects, child.effects...)
			mergeHTTPEnvironments(environment, environment, branch)
		case *goast.RangeStmt:
			branch := cloneHTTPEnvironment(environment)
			child, _ := a.executeStatements(execution, function, value.Body.List, branch, true)
			markHTTPConditional(child.effects)
			result.effects = append(result.effects, child.effects...)
			mergeHTTPEnvironments(environment, environment, branch)
		}
	}
	return result, false
}

func (a *httpSemanticAnalyzer) evaluateExpressions(execution *httpExecution, function *httpSemanticFunction,
	expressions []goast.Expr, environment httpEnvironment, conditional bool,
) httpEvalResult {
	result := httpEvalResult{}
	for _, expression := range expressions {
		evaluated := a.evaluateExpression(execution, function, expression, environment, conditional)
		result.values = append(result.values, evaluated.values...)
		result.effects = append(result.effects, evaluated.effects...)
	}
	return result
}

func (a *httpSemanticAnalyzer) evaluateExpression(execution *httpExecution, function *httpSemanticFunction,
	expression goast.Expr, environment httpEnvironment, conditional bool,
) httpEvalResult {
	if text, ok := a.constantString(expression); ok {
		return httpEvalResult{values: []httpValue{{strings: []httpStringValue{{text: text}}}}}
	}
	switch value := expression.(type) {
	case *goast.ParenExpr:
		return a.evaluateExpression(execution, function, value.X, environment, conditional)
	case *goast.Ident:
		object := a.pkg.TypesInfo.Uses[value]
		if object == nil {
			object = a.pkg.TypesInfo.Defs[value]
		}
		if current, ok := environment[object]; ok {
			return httpEvalResult{values: []httpValue{cloneHTTPValue(current)}}
		}
	case *goast.BinaryExpr:
		if value.Op == token.ADD {
			left := a.evaluateExpression(execution, function, value.X, environment, conditional)
			right := a.evaluateExpression(execution, function, value.Y, environment, conditional)
			return httpEvalResult{values: []httpValue{{strings: combineHTTPStrings(firstHTTPValue(left.values).strings,
				firstHTTPValue(right.values).strings)}}, effects: append(left.effects, right.effects...)}
		}
	case *goast.CallExpr:
		return a.evaluateCall(execution, function, value, environment, conditional)
	case *goast.UnaryExpr:
		if value.Op == token.AND {
			return a.evaluateExpression(execution, function, value.X, environment, conditional)
		}
	case *goast.CompositeLit:
		return httpEvalResult{values: []httpValue{a.evaluateComposite(execution, function, value, environment, conditional)}}
	case *goast.SelectorExpr:
		return httpEvalResult{values: []httpValue{a.evaluateSelector(execution, function, value, environment, conditional)}}
	}
	return httpEvalResult{values: []httpValue{{}}}
}

func (a *httpSemanticAnalyzer) evaluateCall(execution *httpExecution, function *httpSemanticFunction,
	call *goast.CallExpr, environment httpEnvironment, conditional bool,
) httpEvalResult {
	target := a.callTarget(call.Fun)
	location := a.location(function.path, call)
	switch target {
	case "net/url.PathEscape":
		if len(call.Args) == 0 {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		argument := a.evaluateExpression(execution, function, call.Args[0], environment, conditional)
		values := firstHTTPValue(argument.values).strings
		if len(values) == 0 {
			values = []httpStringValue{{text: "{_}"}}
		} else {
			for index := range values {
				if values[index].unknownBase {
					values[index] = httpStringValue{text: "{_}"}
				} else {
					values[index].text = url.PathEscape(values[index].text)
				}
			}
		}
		return httpEvalResult{values: []httpValue{{strings: values}}, effects: argument.effects}
	case "fmt.Sprintf":
		return a.evaluateSprintf(execution, function, call, environment, conditional)
	case "net/http.NewRequest", "net/http.NewRequestWithContext":
		methodIndex, routeIndex := 0, 1
		if target == "net/http.NewRequestWithContext" {
			methodIndex, routeIndex = 1, 2
		}
		if len(call.Args) <= routeIndex {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		methods := firstHTTPValue(a.evaluateExpression(execution, function, call.Args[methodIndex], environment, conditional).values).strings
		routes := firstHTTPValue(a.evaluateExpression(execution, function, call.Args[routeIndex], environment, conditional).values).strings
		requests := crossHTTPRequests(methods, routes, target, "constructor")
		return httpEvalResult{values: []httpValue{{requests: requests}}, effects: nil}
	case "net/http.Get", "net/http.Head", "net/http.Post", "net/http.PostForm":
		if len(call.Args) == 0 {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		routes := firstHTTPValue(a.evaluateExpression(execution, function, call.Args[0], environment, conditional).values).strings
		method := strings.ToUpper(strings.TrimPrefix(target, "net/http."))
		if method == "POSTFORM" {
			method = "POST"
		}
		var effects []httpRequestValue
		for _, route := range routes {
			if route.overflow {
				effects = append(effects, httpRequestValue{overflow: true, sink: target, source: "convenience",
					location: location, conditional: conditional})
				continue
			}
			effects = append(effects, httpRequestValue{method: httpStringValue{text: method}, route: route,
				sink: target, source: "convenience", location: location, conditional: conditional,
				authorityUnknown: route.unknownBase})
		}
		if len(effects) > 0 {
			a.markHTTPRequestCall(call)
		}
		result := emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		result.effects = effects
		return result
	}
	if a.isHTTPClientDo(call) {
		if len(call.Args) == 0 {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		requestResult := a.evaluateExpression(execution, function, call.Args[0], environment, conditional)
		requestValue := firstHTTPValue(requestResult.values)
		requests := append([]httpRequestValue(nil), requestValue.requests...)
		if requestValue.requestObject {
			for _, object := range requestValue.objects {
				requests = append(requests, requestObjectRequests(object)...)
			}
		}
		var effects []httpRequestValue
		for _, request := range requests {
			request.sink = "net/http.Client.Do"
			request.location = location
			request.conditional = request.conditional || conditional
			effects = append(effects, request)
		}
		result := emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		result.effects = append(requestResult.effects, effects...)
		return result
	}
	local := a.localFunction(call.Fun)
	if local == nil {
		return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
	}
	var arguments []httpValue
	if selector, ok := call.Fun.(*goast.SelectorExpr); ok {
		selection := a.pkg.TypesInfo.Selections[selector]
		if selection != nil && selection.Kind() != types.MethodExpr {
			arguments = append(arguments, firstHTTPValue(a.evaluateExpression(execution, function, selector.X, environment, conditional).values))
		}
	}
	for _, argument := range call.Args {
		arguments = append(arguments, firstHTTPValue(a.evaluateExpression(execution, function, argument, environment, conditional).values))
	}
	result := a.executeFunction(execution, local, arguments, conditional)
	for valueIndex := range result.values {
		for requestIndex := range result.values[valueIndex].requests {
			request := &result.values[valueIndex].requests[requestIndex]
			request.wrapperChain = prependHTTPWrapper(local.qualified, request.wrapperChain)
		}
	}
	for index := range result.effects {
		result.effects[index].wrapperChain = prependHTTPWrapper(local.qualified, result.effects[index].wrapperChain)
		result.effects[index].location = location
	}
	return result
}

func (a *httpSemanticAnalyzer) evaluateSprintf(execution *httpExecution, function *httpSemanticFunction,
	call *goast.CallExpr, environment httpEnvironment, conditional bool,
) httpEvalResult {
	if len(call.Args) == 0 {
		return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
	}
	format, ok := a.constantString(call.Args[0])
	if !ok {
		return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
	}
	values := []httpStringValue{{text: ""}}
	argument := 1
	for index := 0; index < len(format); index++ {
		if format[index] != '%' {
			for item := range values {
				values[item].text += string(format[index])
			}
			continue
		}
		if index+1 < len(format) && format[index+1] == '%' {
			for item := range values {
				values[item].text += "%"
			}
			index++
			continue
		}
		if index+1 >= len(format) || argument >= len(call.Args) {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		verb := format[index+1]
		if verb != 's' && verb != 'd' && verb != 'v' {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		part := firstHTTPValue(a.evaluateExpression(execution, function, call.Args[argument], environment, conditional).values).strings
		if len(part) == 0 && verb == 'd' && isIntegerType(a.pkg.TypesInfo.TypeOf(call.Args[argument])) {
			part = []httpStringValue{{text: "{_}"}}
		}
		if len(part) == 0 {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		values = appendHTTPStrings(values, part)
		argument++
		index++
	}
	if argument != len(call.Args) {
		return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
	}
	return httpEvalResult{values: []httpValue{{strings: values}}}
}

func (a *httpSemanticAnalyzer) evaluateComposite(execution *httpExecution, function *httpSemanticFunction,
	literal *goast.CompositeLit, environment httpEnvironment, conditional bool,
) httpValue {
	object := &httpObjectValue{fields: map[types.Object]httpValue{}}
	for _, element := range literal.Elts {
		keyed, ok := element.(*goast.KeyValueExpr)
		if !ok {
			continue
		}
		identifier, ok := keyed.Key.(*goast.Ident)
		if !ok {
			continue
		}
		field := a.pkg.TypesInfo.Uses[identifier]
		if field == nil {
			continue
		}
		object.fields[field] = firstHTTPValue(a.evaluateExpression(execution, function, keyed.Value, environment, conditional).values)
	}
	name := semanticNamedType(a.pkg.TypesInfo.TypeOf(literal))
	if name == "net/http.Request" {
		return httpValue{objects: []*httpObjectValue{object}, requestObject: true}
	}
	return httpValue{objects: []*httpObjectValue{object}, client: a.isHTTPClient(a.pkg.TypesInfo.TypeOf(literal))}
}

func (a *httpSemanticAnalyzer) evaluateSelector(execution *httpExecution, function *httpSemanticFunction,
	selector *goast.SelectorExpr, environment httpEnvironment, conditional bool,
) httpValue {
	if object := a.pkg.TypesInfo.Uses[selector.Sel]; object != nil && object.Pkg() != nil &&
		object.Pkg().Path() == "net/http" && object.Name() == "DefaultClient" {
		return httpValue{client: true}
	}
	selection := a.pkg.TypesInfo.Selections[selector]
	if selection == nil || selection.Kind() == types.MethodExpr {
		return httpValue{}
	}
	base := firstHTTPValue(a.evaluateExpression(execution, function, selector.X, environment, conditional).values)
	result := httpValue{}
	for _, object := range base.objects {
		if field, ok := object.fields[selection.Obj()]; ok {
			result = mergeHTTPValues(result, field)
		}
	}
	if len(result.strings) == 0 && len(result.requests) == 0 && len(result.objects) == 0 && !result.client {
		if isStringType(selection.Obj().Type()) && isBaseURLField(selection.Obj().Name()) {
			result.strings = []httpStringValue{{unknownBase: true}}
		}
		if a.isHTTPClient(selection.Obj().Type()) {
			result.client = true
		}
	}
	return result
}

func (a *httpSemanticAnalyzer) assign(execution *httpExecution, function *httpSemanticFunction, lhs goast.Expr,
	value httpValue, environment httpEnvironment, conditional bool,
) {
	switch target := lhs.(type) {
	case *goast.Ident:
		if target.Name == "_" {
			return
		}
		object := a.pkg.TypesInfo.Defs[target]
		if object == nil {
			object = a.pkg.TypesInfo.Uses[target]
		}
		if object != nil {
			environment[object] = cloneHTTPValue(value)
		}
	case *goast.SelectorExpr:
		selection := a.pkg.TypesInfo.Selections[target]
		if selection == nil || selection.Kind() != types.FieldVal {
			return
		}
		base := firstHTTPValue(a.evaluateExpression(execution, function, target.X, environment, conditional).values)
		for _, object := range base.objects {
			object.fields[selection.Obj()] = cloneHTTPValue(value)
		}
		if identifier, ok := target.X.(*goast.Ident); ok {
			object := a.pkg.TypesInfo.Uses[identifier]
			current, exists := environment[object]
			if !exists || len(current.requests) == 0 {
				return
			}
			switch selection.Obj().Name() {
			case "Method":
				current.requests = replaceRequestMethods(current.requests, value.strings)
			case "URL":
				current.requests = replaceRequestRoutes(current.requests, urlObjectStrings(value.objects))
			}
			environment[object] = current
		}
	}
}

func (a *httpSemanticAnalyzer) materialize(function *httpSemanticFunction, request httpRequestValue, seen map[string]bool) {
	if request.overflow || request.method.overflow || request.route.overflow {
		a.diagnostic(request.location, "outbound HTTP alternatives exceeded the bounded semantic summary; request omitted")
		return
	}
	method := strings.TrimSpace(request.method.text)
	route := strings.TrimSpace(request.route.text)
	if request.route.unknownBase {
		request.authorityUnknown = true
		if !strings.HasPrefix(route, "/") {
			return
		}
	}
	if method == "" || route == "" {
		return
	}
	key := strings.Join([]string{function.qualified, request.location.Path, strconv.Itoa(request.location.Line),
		strconv.Itoa(request.location.Column), method, route}, "\x00")
	if seen[key] {
		return
	}
	seen[key] = true
	view := a.views[request.location.Path]
	view.HTTPRequests = append(view.HTTPRequests, SemanticHTTPRequest{
		Function: function.qualified, FunctionKind: function.kind, Method: method, Route: route,
		Sink: request.sink, Source: request.source, Wrappers: append([]string(nil), request.wrapperChain...),
		Location: request.location, AuthorityUnknown: request.authorityUnknown, Conditional: request.conditional,
	})
	a.views[request.location.Path] = view
}

func (a *httpSemanticAnalyzer) localFunction(expression goast.Expr) *httpSemanticFunction {
	var object types.Object
	switch value := expression.(type) {
	case *goast.Ident:
		object = a.pkg.TypesInfo.Uses[value]
	case *goast.SelectorExpr:
		if selection := a.pkg.TypesInfo.Selections[value]; selection != nil {
			object = selection.Obj()
		} else {
			object = a.pkg.TypesInfo.Uses[value.Sel]
		}
	}
	function, _ := object.(*types.Func)
	return a.functions[function]
}

func (a *httpSemanticAnalyzer) callTarget(expression goast.Expr) string {
	var object types.Object
	switch value := expression.(type) {
	case *goast.Ident:
		object = a.pkg.TypesInfo.Uses[value]
	case *goast.SelectorExpr:
		if selection := a.pkg.TypesInfo.Selections[value]; selection != nil {
			object = selection.Obj()
		} else {
			object = a.pkg.TypesInfo.Uses[value.Sel]
		}
	}
	return objectTarget(object)
}

func (a *httpSemanticAnalyzer) isHTTPClientDo(call *goast.CallExpr) bool {
	selector, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return false
	}
	selection := a.pkg.TypesInfo.Selections[selector]
	return selection != nil && selection.Obj().Pkg() != nil && selection.Obj().Pkg().Path() == "net/http" &&
		selection.Obj().Name() == "Do"
}

func (a *httpSemanticAnalyzer) isHTTPClient(value types.Type) bool {
	return semanticNamedType(value) == "net/http.Client"
}

func (a *httpSemanticAnalyzer) constantString(expression goast.Expr) (string, bool) {
	value := a.pkg.TypesInfo.Types[expression].Value
	if value == nil {
		return "", false
	}
	if value.Kind() == constant.String {
		return constant.StringVal(value), true
	}
	if value.Kind() == constant.Int {
		return value.ExactString(), true
	}
	return "", false
}

func (a *httpSemanticAnalyzer) markHTTPRequestCall(call *goast.CallExpr) {
	position := a.pkg.Fset.Position(call.Lparen)
	path, ok := relativeSourcePath(a.root, position.Filename)
	if !ok {
		return
	}
	view := a.views[path]
	if view.HTTPRequestCalls == nil {
		view.HTTPRequestCalls = map[int]bool{}
	}
	view.HTTPRequestCalls[position.Offset] = true
	a.views[path] = view
}

func (a *httpSemanticAnalyzer) diagnostic(location graph.Location, message string) {
	view := a.views[location.Path]
	view.Diagnostics = append(view.Diagnostics, graph.Diagnostic{
		Path: location.Path, Line: location.Line, Level: "warning", Message: message,
	})
	a.views[location.Path] = view
}

func (a *httpSemanticAnalyzer) location(path string, node goast.Node) graph.Location {
	return semanticLocation(path, a.pkg.Fset, node.Pos(), node.End())
}

func crossHTTPRequests(methods, routes []httpStringValue, sink, source string) []httpRequestValue {
	if hasHTTPStringOverflow(methods) || hasHTTPStringOverflow(routes) {
		return []httpRequestValue{{sink: sink, source: source, overflow: true}}
	}
	var requests []httpRequestValue
	for _, method := range methods {
		for _, route := range routes {
			if len(requests) >= httpAlternativeLimit {
				return []httpRequestValue{{sink: sink, source: source, overflow: true}}
			}
			requests = append(requests, httpRequestValue{method: method, route: route, sink: sink, source: source,
				authorityUnknown: route.unknownBase})
		}
	}
	return requests
}

func combineHTTPStrings(left, right []httpStringValue) []httpStringValue {
	if hasHTTPStringOverflow(left) || hasHTTPStringOverflow(right) {
		return []httpStringValue{{overflow: true}}
	}
	var result []httpStringValue
	for _, first := range left {
		for _, second := range right {
			if first.unknownBase && second.unknownBase {
				continue
			}
			result = appendUniqueHTTPString(result, httpStringValue{
				text: first.text + second.text, unknownBase: first.unknownBase || second.unknownBase,
			})
		}
	}
	return result
}

func appendHTTPStrings(prefixes, suffixes []httpStringValue) []httpStringValue {
	return combineHTTPStrings(prefixes, suffixes)
}

func appendUniqueHTTPString(values []httpStringValue, candidate httpStringValue) []httpStringValue {
	if candidate.overflow || hasHTTPStringOverflow(values) {
		return []httpStringValue{{overflow: true}}
	}
	for _, value := range values {
		if value == candidate {
			return values
		}
	}
	if len(values) >= httpAlternativeLimit {
		return []httpStringValue{{overflow: true}}
	}
	return append(values, candidate)
}

func hasHTTPStringOverflow(values []httpStringValue) bool {
	return len(values) == 1 && values[0].overflow
}

func httpURLStrings(object *httpObjectValue) []httpStringValue {
	paths := httpObjectStrings(object, "Path")
	if len(paths) == 0 {
		paths = []httpStringValue{{text: "/"}}
	}
	schemes := httpObjectStrings(object, "Scheme")
	hosts := httpObjectStrings(object, "Host")
	queries := httpObjectStrings(object, "RawQuery")
	routes := paths
	if len(schemes) > 0 && len(hosts) > 0 {
		prefixes := combineHTTPStrings(schemes, []httpStringValue{{text: "://"}})
		prefixes = combineHTTPStrings(prefixes, hosts)
		routes = combineHTTPStrings(prefixes, paths)
	}
	if len(queries) == 0 {
		return routes
	}
	var result []httpStringValue
	for _, route := range routes {
		for _, query := range queries {
			candidate := route
			if query.text != "" {
				candidate.text += "?" + query.text
			}
			candidate.overflow = candidate.overflow || query.overflow
			result = appendUniqueHTTPString(result, candidate)
		}
	}
	return result
}

func urlObjectStrings(objects []*httpObjectValue) []httpStringValue {
	var routes []httpStringValue
	for _, object := range objects {
		routes = append(routes, httpURLStrings(object)...)
	}
	return routes
}

func requestObjectRequests(object *httpObjectValue) []httpRequestValue {
	methods := httpObjectStrings(object, "Method")
	var routes []httpStringValue
	for _, value := range httpObjectValues(object, "URL") {
		routes = append(routes, urlObjectStrings(value.objects)...)
	}
	return crossHTTPRequests(methods, routes, "net/http.Request", "request_fields")
}

func replaceRequestMethods(requests []httpRequestValue, methods []httpStringValue) []httpRequestValue {
	if hasHTTPStringOverflow(methods) {
		return []httpRequestValue{{overflow: true}}
	}
	var result []httpRequestValue
	for _, request := range requests {
		for _, method := range methods {
			if len(result) >= httpAlternativeLimit {
				return []httpRequestValue{{overflow: true}}
			}
			request.method = method
			result = append(result, request)
		}
	}
	return result
}

func replaceRequestRoutes(requests []httpRequestValue, routes []httpStringValue) []httpRequestValue {
	if hasHTTPStringOverflow(routes) {
		return []httpRequestValue{{overflow: true}}
	}
	var result []httpRequestValue
	for _, request := range requests {
		for _, route := range routes {
			if len(result) >= httpAlternativeLimit {
				return []httpRequestValue{{overflow: true}}
			}
			request.route = route
			request.authorityUnknown = route.unknownBase
			result = append(result, request)
		}
	}
	return result
}

func httpObjectStrings(object *httpObjectValue, name string) []httpStringValue {
	for field, value := range object.fields {
		if field.Name() == name {
			return append([]httpStringValue(nil), value.strings...)
		}
	}
	return nil
}

func httpObjectValues(object *httpObjectValue, name string) []httpValue {
	for field, value := range object.fields {
		if field.Name() == name {
			return []httpValue{value}
		}
	}
	return nil
}

func cloneHTTPValue(value httpValue) httpValue {
	value.strings = append([]httpStringValue(nil), value.strings...)
	value.requests = append([]httpRequestValue(nil), value.requests...)
	value.objects = append([]*httpObjectValue(nil), value.objects...)
	return value
}

func cloneHTTPEnvironment(environment httpEnvironment) httpEnvironment {
	result := make(httpEnvironment, len(environment))
	for object, value := range environment {
		result[object] = cloneHTTPValue(value)
	}
	return result
}

func mergeHTTPEnvironments(target httpEnvironment, environments ...httpEnvironment) {
	for _, environment := range environments {
		for object, value := range environment {
			target[object] = mergeHTTPValues(target[object], value)
		}
	}
}

func mergeHTTPValues(left, right httpValue) httpValue {
	result := cloneHTTPValue(left)
	for _, value := range right.strings {
		result.strings = appendUniqueHTTPString(result.strings, value)
	}
	for _, request := range right.requests {
		if request.overflow {
			result.requests = []httpRequestValue{{overflow: true}}
			break
		}
		duplicate := false
		for _, current := range result.requests {
			duplicate = duplicate || current.method == request.method && current.route == request.route &&
				current.source == request.source
		}
		if !duplicate {
			if len(result.requests) >= httpAlternativeLimit {
				result.requests = []httpRequestValue{{overflow: true}}
				break
			}
			result.requests = append(result.requests, request)
		}
	}
	for _, object := range right.objects {
		found := false
		for _, current := range result.objects {
			found = found || current == object
		}
		if !found && len(result.objects) < httpAlternativeLimit {
			result.objects = append(result.objects, object)
		}
	}
	result.client = result.client || right.client
	result.requestObject = result.requestObject || right.requestObject
	return result
}

func firstHTTPValue(values []httpValue) httpValue {
	if len(values) == 0 {
		return httpValue{}
	}
	return values[0]
}

func emptyHTTPCallResult(value types.Type) httpEvalResult {
	count := 1
	if tuple, ok := value.(*types.Tuple); ok {
		count = tuple.Len()
	}
	return httpEvalResult{values: make([]httpValue, count)}
}

func prependHTTPWrapper(wrapper string, chain []string) []string {
	if len(chain) > 0 && chain[0] == wrapper {
		return append([]string(nil), chain...)
	}
	return append([]string{wrapper}, chain...)
}

func markHTTPConditional(requests []httpRequestValue) {
	for index := range requests {
		requests[index].conditional = true
	}
}

func httpStatementList(statement goast.Stmt) []goast.Stmt {
	if block, ok := statement.(*goast.BlockStmt); ok {
		return block.List
	}
	return []goast.Stmt{statement}
}

func isBaseURLField(name string) bool {
	name = strings.ToLower(name)
	return name == "baseurl" || name == "baseuri"
}

func isStringType(value types.Type) bool {
	basic, _ := value.Underlying().(*types.Basic)
	return basic != nil && basic.Kind() == types.String
}

func isIntegerType(value types.Type) bool {
	if value == nil {
		return false
	}
	basic, _ := value.Underlying().(*types.Basic)
	return basic != nil && basic.Info()&types.IsInteger != 0
}
