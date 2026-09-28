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
	httpExecutionLimit   = 2048
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
	conditional bool
}

type httpRequestValue struct {
	identity         *httpRequestIdentity
	method           httpStringValue
	route            httpStringValue
	sink             string
	source           string
	wrapperChain     []string
	location         graph.Location
	conditional      bool
	authorityUnknown bool
	overflow         bool
	urlObject        *httpObjectValue
}

type httpRequestIdentity struct{ _ byte }

type httpObjectValue struct {
	identity *httpObjectIdentity
	fields   map[types.Object]httpValue
}

type httpObjectIdentity struct{ _ byte }

type httpValue struct {
	strings       []httpStringValue
	requests      []httpRequestValue
	objects       []*httpObjectValue
	requestObject bool
	client        bool
	overflow      bool
}

type httpEnvironment map[types.Object]httpValue

type httpExecution struct {
	stack     map[*types.Func]bool
	frames    []*httpCallFrame
	remaining int
	exhausted bool
	diagnosed bool
}

type httpCallFrame struct {
	bindings map[types.Object]bool
	escaped  map[types.Object]httpValue
}

type httpEvalResult struct {
	values    []httpValue
	effects   []httpRequestValue
	arguments []httpValue
}

type httpSemanticAnalyzer struct {
	root        string
	pkg         *packages.Package
	views       map[string]SemanticView
	functions   map[*types.Func]*httpSemanticFunction
	relevant    map[*types.Func]bool
	diagnostics map[string]bool
}

func collectHTTPPackageViews(root string, pkg *packages.Package, views map[string]SemanticView) {
	if pkg == nil || pkg.TypesInfo == nil || pkg.Types == nil || pkg.Fset == nil {
		return
	}
	analyzer := &httpSemanticAnalyzer{
		root: root, pkg: pkg, views: views, functions: map[*types.Func]*httpSemanticFunction{},
		relevant: map[*types.Func]bool{}, diagnostics: map[string]bool{},
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
			_, _, convenience := a.httpConvenience(call)
			if isHTTPSemanticTarget(target) || convenience || a.isHTTPClientDo(call) {
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
		execution := &httpExecution{stack: map[*types.Func]bool{}, remaining: httpExecutionLimit}
		result := a.executeFunction(execution, root, nil, false)
		if execution.exhausted {
			a.markAbandonedRootCalls(root)
			continue
		}
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
	frame := &httpCallFrame{bindings: map[types.Object]bool{}, escaped: map[types.Object]httpValue{}}
	execution.frames = append(execution.frames, frame)
	defer func() { execution.frames = execution.frames[:len(execution.frames)-1] }()
	environment := httpEnvironment{}
	bindings := make([]types.Object, len(arguments))
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
					bindings[argument] = object
					frame.bindings[object] = true
				} else if receiver {
					environment[object] = httpValue{objects: []*httpObjectValue{{
						identity: &httpObjectIdentity{}, fields: map[types.Object]httpValue{},
					}}}
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
	result.arguments = make([]httpValue, len(arguments))
	for index, object := range bindings {
		if object != nil {
			result.arguments[index] = mergeHTTPValues(frame.escaped[object], environment[object])
		}
	}
	return result
}

func (a *httpSemanticAnalyzer) consumeExecution(execution *httpExecution, function *httpSemanticFunction,
	node goast.Node, cost int,
) bool {
	if execution == nil || execution.exhausted {
		return false
	}
	if execution.remaining >= cost {
		execution.remaining -= cost
		return true
	}
	execution.exhausted = true
	if !execution.diagnosed {
		execution.diagnosed = true
		a.diagnostic(a.location(function.path, node),
			"outbound HTTP execution exceeded the bounded semantic summary; request omitted")
	}
	return false
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
		if !a.consumeExecution(execution, function, statement, 1) {
			return result, false
		}
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
			result.values = mergeHTTPReturnValues(result.values, evaluated.values)
			result.effects = append(result.effects, evaluated.effects...)
			return result, true
		case *goast.BlockStmt:
			child, stopped := a.executeStatements(execution, function, value.List, environment, conditional)
			result.effects = append(result.effects, child.effects...)
			if stopped {
				result.values = mergeHTTPReturnValues(result.values, child.values)
				return result, true
			}
		case *goast.IfStmt:
			if value.Init != nil {
				initResult, _ := a.executeStatements(execution, function, []goast.Stmt{value.Init}, environment, conditional)
				result.effects = append(result.effects, initResult.effects...)
			}
			if !a.consumeExecution(execution, function, value, 2) {
				return result, false
			}
			left := cloneHTTPEnvironment(environment)
			child, leftStopped := a.executeStatements(execution, function, value.Body.List, left, true)
			markHTTPConditional(child.effects)
			result.effects = append(result.effects, child.effects...)
			result.values = mergeHTTPReturnValues(result.values, child.values)
			right := cloneHTTPEnvironment(environment)
			rightStopped := false
			if value.Else != nil {
				child, rightStopped = a.executeStatements(execution, function, httpStatementList(value.Else), right, true)
				markHTTPConditional(child.effects)
				result.effects = append(result.effects, child.effects...)
				result.values = mergeHTTPReturnValues(result.values, child.values)
			}
			if leftStopped && value.Else != nil && rightStopped {
				return result, true
			}
			continuing := []httpEnvironment{}
			if !leftStopped {
				continuing = append(continuing, left)
			}
			if value.Else == nil || !rightStopped {
				continuing = append(continuing, right)
			}
			mergeHTTPEnvironments(environment, continuing...)
		case *goast.ForStmt:
			if !a.consumeExecution(execution, function, value, 1) {
				return result, false
			}
			branch := cloneHTTPEnvironment(environment)
			child, stopped := a.executeStatements(execution, function, value.Body.List, branch, true)
			markHTTPConditional(child.effects)
			result.effects = append(result.effects, child.effects...)
			result.values = mergeHTTPReturnValues(result.values, child.values)
			continuing := []httpEnvironment{environment}
			if !stopped {
				continuing = append(continuing, branch)
			}
			mergeHTTPEnvironments(environment, continuing...)
		case *goast.RangeStmt:
			if !a.consumeExecution(execution, function, value, 1) {
				return result, false
			}
			branch := cloneHTTPEnvironment(environment)
			child, stopped := a.executeStatements(execution, function, value.Body.List, branch, true)
			markHTTPConditional(child.effects)
			result.effects = append(result.effects, child.effects...)
			result.values = mergeHTTPReturnValues(result.values, child.values)
			continuing := []httpEnvironment{environment}
			if !stopped {
				continuing = append(continuing, branch)
			}
			mergeHTTPEnvironments(environment, continuing...)
		case *goast.SwitchStmt:
			if value.Init != nil {
				child, _ := a.executeStatements(execution, function, []goast.Stmt{value.Init}, environment, conditional)
				result.effects = append(result.effects, child.effects...)
			}
			if value.Tag != nil {
				tag := a.evaluateExpression(execution, function, value.Tag, environment, conditional)
				result.effects = append(result.effects, tag.effects...)
			}
			bodies, hasDefault := switchClauseBodies(value.Body.List)
			child, stopped := a.executeConditionalClauses(execution, function, bodies, environment, hasDefault)
			result.effects = append(result.effects, child.effects...)
			result.values = mergeHTTPReturnValues(result.values, child.values)
			if stopped {
				return result, true
			}
		case *goast.TypeSwitchStmt:
			if value.Init != nil {
				child, _ := a.executeStatements(execution, function, []goast.Stmt{value.Init}, environment, conditional)
				result.effects = append(result.effects, child.effects...)
			}
			if value.Assign != nil {
				child, _ := a.executeStatements(execution, function, []goast.Stmt{value.Assign}, environment, conditional)
				result.effects = append(result.effects, child.effects...)
			}
			bodies, hasDefault := switchClauseBodies(value.Body.List)
			child, stopped := a.executeConditionalClauses(execution, function, bodies, environment, hasDefault)
			result.effects = append(result.effects, child.effects...)
			result.values = mergeHTTPReturnValues(result.values, child.values)
			if stopped {
				return result, true
			}
		case *goast.SelectStmt:
			bodies, hasDefault := selectClauseBodies(value.Body.List)
			child, stopped := a.executeConditionalClauses(execution, function, bodies, environment, hasDefault)
			result.effects = append(result.effects, child.effects...)
			result.values = mergeHTTPReturnValues(result.values, child.values)
			if stopped {
				return result, true
			}
		}
	}
	return result, false
}

func (a *httpSemanticAnalyzer) executeConditionalClauses(execution *httpExecution,
	function *httpSemanticFunction, bodies [][]goast.Stmt, environment httpEnvironment, hasDefault bool,
) (httpEvalResult, bool) {
	result := httpEvalResult{}
	if len(bodies) == 0 {
		return result, false
	}
	if !a.consumeExecution(execution, function, function.decl, len(bodies)) {
		return result, false
	}
	continuing := []httpEnvironment{}
	allStopped := true
	for _, body := range bodies {
		branch := cloneHTTPEnvironment(environment)
		child, stopped := a.executeStatements(execution, function, body, branch, true)
		markHTTPConditional(child.effects)
		result.effects = append(result.effects, child.effects...)
		result.values = mergeHTTPReturnValues(result.values, child.values)
		allStopped = allStopped && stopped
		if !stopped {
			continuing = append(continuing, branch)
		}
	}
	if !hasDefault {
		continuing = append(continuing, environment)
		allStopped = false
	}
	if len(continuing) > 0 {
		mergeHTTPEnvironments(environment, continuing...)
	}
	return result, allStopped
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
			return httpEvalResult{values: []httpValue{{strings: combineHTTPStrings(httpValueStrings(firstHTTPValue(left.values)),
				httpValueStrings(firstHTTPValue(right.values)))}}, effects: append(left.effects, right.effects...)}
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
	if sink, method, ok := a.httpConvenience(call); ok {
		if len(call.Args) == 0 {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		routeResult := a.evaluateExpression(execution, function, call.Args[0], environment, conditional)
		routes := httpValueStrings(firstHTTPValue(routeResult.values))
		var effects []httpRequestValue
		for _, route := range routes {
			if route.overflow {
				effects = append(effects, httpRequestValue{overflow: true, sink: sink, source: "convenience",
					location: location, conditional: conditional})
				continue
			}
			effects = append(effects, httpRequestValue{method: httpStringValue{text: method}, route: route,
				sink: sink, source: "convenience", location: location, conditional: conditional || route.conditional,
				authorityUnknown: route.unknownBase})
		}
		if len(effects) > 0 {
			a.markHTTPRequestCall(call)
		}
		result := emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		result.effects = append(routeResult.effects, effects...)
		return result
	}
	switch target {
	case "net/url.PathEscape":
		if len(call.Args) == 0 {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		argument := a.evaluateExpression(execution, function, call.Args[0], environment, conditional)
		values := httpValueStrings(firstHTTPValue(argument.values))
		if len(values) == 0 {
			values = []httpStringValue{{text: "{_}"}}
		} else {
			for index := range values {
				if values[index].unknownBase {
					values[index] = httpStringValue{text: "{_}", conditional: values[index].conditional}
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
		methods := httpValueStrings(firstHTTPValue(a.evaluateExpression(execution, function, call.Args[methodIndex], environment, conditional).values))
		routes := httpValueStrings(firstHTTPValue(a.evaluateExpression(execution, function, call.Args[routeIndex], environment, conditional).values))
		requests := crossHTTPRequests(methods, routes, target, "constructor")
		return httpEvalResult{values: []httpValue{{requests: requests}}, effects: nil}
	}
	if a.isHTTPClientDo(call) {
		if len(call.Args) == 0 {
			return emptyHTTPCallResult(a.pkg.TypesInfo.TypeOf(call))
		}
		requestResult := a.evaluateExpression(execution, function, call.Args[0], environment, conditional)
		requestValue := firstHTTPValue(requestResult.values)
		requests := append([]httpRequestValue(nil), requestValue.requests...)
		if requestValue.overflow {
			requests = []httpRequestValue{{overflow: true}}
		}
		if requestValue.requestObject {
			for _, object := range requestValue.objects {
				requests = append(requests, requestObjectRequests(object)...)
			}
		}
		requests = resolveRequestURLs(requests)
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
	var argumentExpressions []goast.Expr
	if selector, ok := call.Fun.(*goast.SelectorExpr); ok {
		selection := a.pkg.TypesInfo.Selections[selector]
		if selection != nil && selection.Kind() != types.MethodExpr {
			arguments = append(arguments, firstHTTPValue(a.evaluateExpression(execution, function, selector.X, environment, conditional).values))
			argumentExpressions = append(argumentExpressions, selector.X)
		}
	}
	for _, argument := range call.Args {
		arguments = append(arguments, firstHTTPValue(a.evaluateExpression(execution, function, argument, environment, conditional).values))
		argumentExpressions = append(argumentExpressions, argument)
	}
	result := a.executeFunction(execution, local, arguments, conditional)
	for index, updated := range result.arguments {
		if index >= len(argumentExpressions) || !isPointerType(a.pkg.TypesInfo.TypeOf(argumentExpressions[index])) ||
			!isHTTPMutableValue(updated) {
			continue
		}
		propagated, ok := propagateHTTPArgument(arguments[index], updated)
		if ok {
			a.assign(execution, function, argumentExpressions[index], propagated, environment, conditional)
		}
	}
	result.arguments = nil
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
		part := httpValueStrings(firstHTTPValue(a.evaluateExpression(execution, function, call.Args[argument], environment, conditional).values))
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
	object := &httpObjectValue{identity: &httpObjectIdentity{}, fields: map[types.Object]httpValue{}}
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
	if base.overflow {
		return httpValue{overflow: true}
	}
	result := httpValue{}
	if selection.Obj().Name() == "URL" {
		attached := false
		for index := range base.requests {
			request := &base.requests[index]
			if request.overflow {
				return httpValue{overflow: true}
			}
			object := request.urlObject
			if object == nil {
				object = httpURLObjectFromRoute(selection.Obj().Type(), request.route)
				request.urlObject = object
				attached = attached || object != nil
			}
			if object != nil {
				result.objects = appendUniqueHTTPObject(result.objects, object)
			}
		}
		if attached {
			a.assign(execution, function, selector.X, base, environment, conditional)
		}
	}
	for _, object := range base.objects {
		if field, ok := object.fields[selection.Obj()]; ok {
			result = mergeHTTPValues(result, field)
		}
	}
	if result.overflow {
		return result
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
			if current, exists := environment[object]; exists && isHTTPMutableValue(current) {
				if shareHTTPMutableIdentity(current, value) {
					propagateHTTPRequestAliases(execution, environment, current, value, conditional)
				} else {
					captureHTTPArgumentRebind(execution, object, current)
				}
			}
			environment[object] = cloneHTTPValue(value)
		}
	case *goast.SelectorExpr:
		selection := a.pkg.TypesInfo.Selections[target]
		if selection == nil || selection.Kind() != types.FieldVal {
			return
		}
		base := firstHTTPValue(a.evaluateExpression(execution, function, target.X, environment, conditional).values)
		changed := false
		for _, object := range base.objects {
			object.fields[selection.Obj()] = cloneHTTPValue(value)
			changed = true
		}
		if len(base.requests) > 0 {
			switch selection.Obj().Name() {
			case "Method":
				base.requests = replaceRequestMethods(base.requests, httpValueStrings(value))
				changed = true
			case "URL":
				if value.overflow {
					base.requests = []httpRequestValue{{overflow: true}}
				} else {
					base.requests = replaceRequestURLs(base.requests, value.objects)
				}
				changed = true
			}
		}
		if changed {
			a.assign(execution, function, target.X, base, environment, conditional)
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

func (a *httpSemanticAnalyzer) httpConvenience(call *goast.CallExpr) (string, string, bool) {
	target := a.callTarget(call.Fun)
	var name string
	switch target {
	case "net/http.Get":
		name = "Get"
	case "net/http.Head":
		name = "Head"
	case "net/http.Post":
		name = "Post"
	case "net/http.PostForm":
		name = "PostForm"
	default:
		selector, ok := call.Fun.(*goast.SelectorExpr)
		if !ok {
			return "", "", false
		}
		selection := a.pkg.TypesInfo.Selections[selector]
		function, _ := selectionObject(selection).(*types.Func)
		if function == nil || function.Pkg() == nil || function.Pkg().Path() != "net/http" {
			return "", "", false
		}
		signature, _ := function.Type().(*types.Signature)
		if signature == nil || signature.Recv() == nil || !a.isHTTPClient(signature.Recv().Type()) {
			return "", "", false
		}
		name = function.Name()
		if name != "Get" && name != "Head" && name != "Post" && name != "PostForm" {
			return "", "", false
		}
		target = objectTarget(function)
	}
	method := strings.ToUpper(name)
	if name == "PostForm" {
		method = "POST"
	}
	return target, method, true
}

func selectionObject(selection *types.Selection) types.Object {
	if selection == nil {
		return nil
	}
	return selection.Obj()
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

func (a *httpSemanticAnalyzer) markAbandonedRootCalls(root *httpSemanticFunction) {
	visited := map[*types.Func]bool{}
	var visit func(*httpSemanticFunction)
	visit = func(function *httpSemanticFunction) {
		if function == nil || visited[function.object] {
			return
		}
		visited[function.object] = true
		goast.Inspect(function.decl.Body, func(node goast.Node) bool {
			call, ok := node.(*goast.CallExpr)
			if !ok {
				return true
			}
			if _, _, convenience := a.httpConvenience(call); convenience {
				a.markHTTPRequestCall(call)
			}
			visit(a.localFunction(call.Fun))
			return true
		})
	}
	visit(root)
}

func (a *httpSemanticAnalyzer) diagnostic(location graph.Location, message string) {
	key := strings.Join([]string{location.Path, strconv.Itoa(location.Line), message}, "\x00")
	if a.diagnostics[key] {
		return
	}
	a.diagnostics[key] = true
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
	identity := &httpRequestIdentity{}
	for _, method := range methods {
		for _, route := range routes {
			if len(requests) >= httpAlternativeLimit {
				return []httpRequestValue{{sink: sink, source: source, overflow: true}}
			}
			requests = append(requests, httpRequestValue{identity: identity, method: method, route: route, sink: sink, source: source,
				authorityUnknown: route.unknownBase, conditional: method.conditional || route.conditional})
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
				conditional: first.conditional || second.conditional,
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
	for index, value := range values {
		if value.text == candidate.text && value.unknownBase == candidate.unknownBase && value.overflow == candidate.overflow {
			values[index].conditional = value.conditional || candidate.conditional
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

func httpURLObjectFromRoute(valueType types.Type, route httpStringValue) *httpObjectValue {
	if route.overflow {
		return nil
	}
	parsed, err := url.Parse(route.text)
	if err != nil {
		return nil
	}
	object := &httpObjectValue{identity: &httpObjectIdentity{}, fields: map[types.Object]httpValue{}}
	set := func(name, text string, unknownBase bool) {
		field := httpStructField(valueType, name)
		if field == nil {
			return
		}
		object.fields[field] = httpValue{strings: []httpStringValue{{
			text: text, unknownBase: unknownBase, conditional: route.conditional,
		}}}
	}
	set("Path", rawHTTPRoutePath(route.text, parsed), route.unknownBase)
	if parsed.Scheme != "" {
		set("Scheme", parsed.Scheme, false)
	}
	if parsed.Host != "" {
		set("Host", parsed.Host, false)
	}
	if parsed.RawQuery != "" {
		set("RawQuery", parsed.RawQuery, false)
	}
	if separator := strings.IndexByte(route.text, '#'); separator >= 0 {
		set("Fragment", route.text[separator+1:], false)
	}
	return object
}

func rawHTTPRoutePath(raw string, parsed *url.URL) string {
	path := raw
	authority := -1
	if parsed.Scheme != "" {
		prefix := parsed.Scheme + "://"
		if len(raw) >= len(prefix) && strings.EqualFold(raw[:len(prefix)], prefix) {
			authority = len(prefix)
		}
	} else if strings.HasPrefix(raw, "//") {
		authority = 2
	}
	if authority >= 0 {
		rest := raw[authority:]
		separator := strings.IndexAny(rest, "/?#")
		if separator < 0 || rest[separator] != '/' {
			return ""
		}
		path = rest[separator:]
	}
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		path = path[:end]
	}
	return path
}

func httpStructField(value types.Type, name string) types.Object {
	value = types.Unalias(value)
	if pointer, ok := value.(*types.Pointer); ok {
		value = types.Unalias(pointer.Elem())
	}
	if named, ok := value.(*types.Named); ok {
		value = named.Underlying()
	}
	structure, _ := value.(*types.Struct)
	if structure == nil {
		return nil
	}
	for index := 0; index < structure.NumFields(); index++ {
		if field := structure.Field(index); field.Name() == name {
			return field
		}
	}
	return nil
}

func httpURLStrings(object *httpObjectValue) []httpStringValue {
	paths := httpObjectStrings(object, "Path")
	if len(paths) == 0 {
		paths = []httpStringValue{{text: "/"}}
	}
	schemes := httpObjectStrings(object, "Scheme")
	hosts := httpObjectStrings(object, "Host")
	queries := httpObjectStrings(object, "RawQuery")
	fragments := httpObjectStrings(object, "Fragment")
	routes := paths
	if len(hosts) > 0 {
		prefixes := combineHTTPStrings([]httpStringValue{{text: "//"}}, hosts)
		if len(schemes) > 0 {
			prefixes = combineHTTPStrings(schemes, []httpStringValue{{text: "://"}})
			prefixes = combineHTTPStrings(prefixes, hosts)
		}
		routes = combineHTTPStrings(prefixes, paths)
	}
	if len(queries) > 0 {
		var result []httpStringValue
		for _, route := range routes {
			for _, query := range queries {
				candidate := route
				if query.text != "" {
					candidate.text += "?" + query.text
				}
				candidate.overflow = candidate.overflow || query.overflow
				candidate.conditional = candidate.conditional || query.conditional
				result = appendUniqueHTTPString(result, candidate)
			}
		}
		routes = result
	}
	if len(fragments) == 0 {
		return routes
	}
	var result []httpStringValue
	for _, route := range routes {
		for _, fragment := range fragments {
			candidate := route
			if fragment.text != "" {
				candidate.text += "#" + fragment.text
			}
			candidate.overflow = candidate.overflow || fragment.overflow
			candidate.conditional = candidate.conditional || fragment.conditional
			result = appendUniqueHTTPString(result, candidate)
		}
	}
	return result
}

func urlObjectStrings(objects []*httpObjectValue) []httpStringValue {
	var routes []httpStringValue
	for _, object := range objects {
		for _, route := range httpURLStrings(object) {
			routes = appendUniqueHTTPString(routes, route)
		}
	}
	return routes
}

func appendUniqueHTTPObject(objects []*httpObjectValue, candidate *httpObjectValue) []*httpObjectValue {
	for _, object := range objects {
		if object == candidate {
			return objects
		}
	}
	return append(objects, candidate)
}

func requestObjectRequests(object *httpObjectValue) []httpRequestValue {
	methods := httpObjectStrings(object, "Method")
	var routes []httpStringValue
	for _, value := range httpObjectValues(object, "URL") {
		if value.overflow {
			routes = []httpStringValue{{overflow: true}}
			break
		}
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
			request.conditional = request.conditional || method.conditional
			result = append(result, request)
		}
	}
	return result
}

func replaceRequestURLs(requests []httpRequestValue, objects []*httpObjectValue) []httpRequestValue {
	var result []httpRequestValue
	for _, request := range requests {
		for _, object := range objects {
			for _, route := range httpURLStrings(object) {
				if len(result) >= httpAlternativeLimit {
					return []httpRequestValue{{overflow: true}}
				}
				candidate := request
				candidate.urlObject = object
				candidate.route = route
				candidate.authorityUnknown = route.unknownBase
				candidate.conditional = candidate.conditional || route.conditional
				result = append(result, candidate)
			}
		}
	}
	return result
}

func resolveRequestURLs(requests []httpRequestValue) []httpRequestValue {
	var result []httpRequestValue
	for _, request := range requests {
		if request.overflow || request.urlObject == nil {
			result = append(result, request)
			continue
		}
		resolved := replaceRequestURLs([]httpRequestValue{request}, []*httpObjectValue{request.urlObject})
		result = append(result, resolved...)
		if len(result) > httpAlternativeLimit {
			return []httpRequestValue{{overflow: true}}
		}
	}
	return result
}

func httpObjectStrings(object *httpObjectValue, name string) []httpStringValue {
	for field, value := range object.fields {
		if field.Name() == name {
			return append([]httpStringValue(nil), httpValueStrings(value)...)
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
	objects := map[*httpObjectValue]*httpObjectValue{}
	for object, value := range environment {
		result[object] = deepCloneHTTPValue(value, objects)
	}
	return result
}

func deepCloneHTTPValue(value httpValue, objects map[*httpObjectValue]*httpObjectValue) httpValue {
	result := cloneHTTPValue(value)
	result.objects = make([]*httpObjectValue, 0, len(value.objects))
	for _, object := range value.objects {
		result.objects = append(result.objects, deepCloneHTTPObject(object, objects))
	}
	for index := range result.requests {
		if result.requests[index].urlObject != nil {
			result.requests[index].urlObject = deepCloneHTTPObject(result.requests[index].urlObject, objects)
		}
	}
	return result
}

func deepCloneHTTPObject(object *httpObjectValue, objects map[*httpObjectValue]*httpObjectValue) *httpObjectValue {
	if cloned, ok := objects[object]; ok {
		return cloned
	}
	cloned := &httpObjectValue{identity: object.identity, fields: map[types.Object]httpValue{}}
	objects[object] = cloned
	for field, fieldValue := range object.fields {
		cloned.fields[field] = deepCloneHTTPValue(fieldValue, objects)
	}
	return cloned
}

func mergeHTTPEnvironments(target httpEnvironment, environments ...httpEnvironment) {
	merged := httpEnvironment{}
	objects := map[types.Object]bool{}
	for _, environment := range environments {
		for object := range environment {
			objects[object] = true
		}
	}
	for object := range objects {
		values := make([]httpValue, len(environments))
		different := false
		for index, environment := range environments {
			values[index] = cloneHTTPValue(environment[object])
			if index > 0 && !equalHTTPValues(values[0], values[index], map[httpObjectPair]bool{}) {
				different = true
			}
		}
		for index := range values {
			if different {
				markHTTPValueConditional(&values[index], map[*httpObjectValue]bool{})
			}
			merged[object] = mergeHTTPValues(merged[object], values[index])
		}
	}
	clear(target)
	for object, value := range merged {
		target[object] = value
	}
}

func markHTTPValueConditional(value *httpValue, objects map[*httpObjectValue]bool) {
	for index := range value.strings {
		value.strings[index].conditional = true
	}
	for index := range value.requests {
		value.requests[index].conditional = true
		if value.requests[index].urlObject != nil {
			markHTTPObjectConditional(value.requests[index].urlObject, objects)
		}
	}
	for _, object := range value.objects {
		markHTTPObjectConditional(object, objects)
	}
}

type httpObjectPair struct {
	left  *httpObjectValue
	right *httpObjectValue
}

func equalHTTPValues(left, right httpValue, seen map[httpObjectPair]bool) bool {
	if left.client != right.client || left.requestObject != right.requestObject || left.overflow != right.overflow ||
		len(left.strings) != len(right.strings) || len(left.requests) != len(right.requests) ||
		len(left.objects) != len(right.objects) {
		return false
	}
	for index := range left.strings {
		if left.strings[index] != right.strings[index] {
			return false
		}
	}
	for index := range left.requests {
		first, second := left.requests[index], right.requests[index]
		if first.identity != second.identity || first.method != second.method || first.route != second.route ||
			first.sink != second.sink || first.source != second.source || first.conditional != second.conditional ||
			first.authorityUnknown != second.authorityUnknown || first.overflow != second.overflow ||
			!equalHTTPStringSlices(first.wrapperChain, second.wrapperChain) ||
			!equalHTTPObjects(first.urlObject, second.urlObject, seen) {
			return false
		}
	}
	for index := range left.objects {
		if !equalHTTPObjects(left.objects[index], right.objects[index], seen) {
			return false
		}
	}
	return true
}

func equalHTTPObjects(left, right *httpObjectValue, seen map[httpObjectPair]bool) bool {
	if left == nil || right == nil {
		return left == right
	}
	if !sameHTTPObjectIdentity(left, right) || len(left.fields) != len(right.fields) {
		return false
	}
	pair := httpObjectPair{left: left, right: right}
	if seen[pair] {
		return true
	}
	seen[pair] = true
	for field, first := range left.fields {
		second, ok := right.fields[field]
		if !ok || !equalHTTPValues(first, second, seen) {
			return false
		}
	}
	return true
}

func equalHTTPStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func markHTTPObjectConditional(object *httpObjectValue, objects map[*httpObjectValue]bool) {
	if object == nil || objects[object] {
		return
	}
	objects[object] = true
	for field, value := range object.fields {
		markHTTPValueConditional(&value, objects)
		object.fields[field] = value
	}
}

func mergeHTTPValues(left, right httpValue) httpValue {
	result := cloneHTTPValue(left)
	result.overflow = result.overflow || right.overflow
	for _, value := range right.strings {
		result.strings = appendUniqueHTTPString(result.strings, value)
	}
	for _, request := range right.requests {
		if request.overflow {
			result.requests = []httpRequestValue{{overflow: true}}
			break
		}
		duplicate := false
		for index, current := range result.requests {
			if current.identity == request.identity && current.method == request.method && current.route == request.route &&
				current.source == request.source &&
				equalHTTPObjects(current.urlObject, request.urlObject, map[httpObjectPair]bool{}) {
				result.requests[index].conditional = current.conditional || request.conditional
				duplicate = true
			}
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
			found = found || equalHTTPObjects(current, object, map[httpObjectPair]bool{})
		}
		if !found {
			if len(result.objects) >= httpAlternativeLimit {
				result.objects = nil
				result.overflow = true
				break
			}
			result.objects = append(result.objects, object)
		}
	}
	result.client = result.client || right.client
	result.requestObject = result.requestObject || right.requestObject
	return result
}

func mergeHTTPReturnValues(left, right []httpValue) []httpValue {
	if len(left) == 0 {
		result := make([]httpValue, len(right))
		for index := range right {
			result[index] = cloneHTTPValue(right[index])
		}
		return result
	}
	result := make([]httpValue, len(left))
	for index := range left {
		result[index] = cloneHTTPValue(left[index])
	}
	for index, value := range right {
		if index >= len(result) {
			result = append(result, cloneHTTPValue(value))
			continue
		}
		result[index] = mergeHTTPValues(result[index], value)
	}
	return result
}

func firstHTTPValue(values []httpValue) httpValue {
	if len(values) == 0 {
		return httpValue{}
	}
	return values[0]
}

func httpValueStrings(value httpValue) []httpStringValue {
	if value.overflow {
		return []httpStringValue{{overflow: true}}
	}
	return value.strings
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

func switchClauseBodies(statements []goast.Stmt) ([][]goast.Stmt, bool) {
	bodies := make([][]goast.Stmt, 0, len(statements))
	hasDefault := false
	for _, statement := range statements {
		clause, ok := statement.(*goast.CaseClause)
		if !ok {
			continue
		}
		hasDefault = hasDefault || len(clause.List) == 0
		bodies = append(bodies, clause.Body)
	}
	return bodies, hasDefault
}

func selectClauseBodies(statements []goast.Stmt) ([][]goast.Stmt, bool) {
	bodies := make([][]goast.Stmt, 0, len(statements))
	hasDefault := false
	for _, statement := range statements {
		clause, ok := statement.(*goast.CommClause)
		if !ok {
			continue
		}
		hasDefault = hasDefault || clause.Comm == nil
		body := append([]goast.Stmt(nil), clause.Body...)
		if clause.Comm != nil {
			body = append([]goast.Stmt{clause.Comm}, body...)
		}
		bodies = append(bodies, body)
	}
	return bodies, hasDefault
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

func isPointerType(value types.Type) bool {
	if value == nil {
		return false
	}
	_, ok := types.Unalias(value).(*types.Pointer)
	return ok
}

func isHTTPMutableValue(value httpValue) bool {
	return len(value.requests) > 0 || value.requestObject || len(value.objects) > 0 || value.overflow
}

func propagateHTTPArgument(original, updated httpValue) (httpValue, bool) {
	result := cloneHTTPValue(updated)
	result.requests = nil
	for _, request := range updated.requests {
		for _, initial := range original.requests {
			if request.identity != nil && request.identity == initial.identity {
				result.requests = append(result.requests, request)
				break
			}
		}
	}
	result.objects = nil
	for _, object := range updated.objects {
		for _, initial := range original.objects {
			if sameHTTPObjectIdentity(object, initial) {
				result.objects = append(result.objects, object)
				break
			}
		}
	}
	result.requestObject = updated.requestObject && original.requestObject && len(result.objects) > 0
	result.client = updated.client && original.client
	ok := result.overflow || len(result.requests) > 0 || len(result.objects) > 0 || result.requestObject
	return result, ok
}

func sameHTTPObjectIdentity(left, right *httpObjectValue) bool {
	return left != nil && right != nil && (left == right || left.identity != nil && left.identity == right.identity)
}

func shareHTTPMutableIdentity(left, right httpValue) bool {
	for _, first := range left.requests {
		for _, second := range right.requests {
			if first.identity != nil && first.identity == second.identity {
				return true
			}
		}
	}
	for _, first := range left.objects {
		for _, second := range right.objects {
			if sameHTTPObjectIdentity(first, second) {
				return true
			}
		}
	}
	return false
}

func captureHTTPArgumentRebind(execution *httpExecution, object types.Object, current httpValue) {
	for index := len(execution.frames) - 1; index >= 0; index-- {
		frame := execution.frames[index]
		if !frame.bindings[object] {
			continue
		}
		captured := deepCloneHTTPValue(current, map[*httpObjectValue]*httpObjectValue{})
		frame.escaped[object] = mergeHTTPValues(frame.escaped[object], captured)
		return
	}
}

func propagateHTTPRequestAliases(execution *httpExecution, environment httpEnvironment, previous, updated httpValue,
	conditional bool,
) {
	previousIdentities := map[*httpRequestIdentity]bool{}
	for _, request := range previous.requests {
		if request.identity != nil {
			previousIdentities[request.identity] = true
		}
	}
	updates := map[*httpRequestIdentity][]httpRequestValue{}
	for _, request := range updated.requests {
		if previousIdentities[request.identity] {
			updates[request.identity] = append(updates[request.identity], request)
		}
	}
	if len(updates) == 0 {
		return
	}
	seen := map[*httpObjectValue]bool{}
	for object, value := range environment {
		replaceHTTPRequestAliases(&value, updates, seen, false)
		environment[object] = value
	}
	escapedUpdates := updates
	if conditional {
		escapedUpdates = map[*httpRequestIdentity][]httpRequestValue{}
		for identity, requests := range updates {
			for _, request := range requests {
				request.conditional = true
				escapedUpdates[identity] = append(escapedUpdates[identity], request)
			}
		}
	}
	escapedSeen := map[*httpObjectValue]bool{}
	for _, frame := range execution.frames {
		for object, value := range frame.escaped {
			replaceHTTPRequestAliases(&value, escapedUpdates, escapedSeen, conditional)
			frame.escaped[object] = value
		}
	}
}

func replaceHTTPRequestAliases(value *httpValue, updates map[*httpRequestIdentity][]httpRequestValue,
	seen map[*httpObjectValue]bool, preserve bool,
) {
	var requests []httpRequestValue
	replaced := map[*httpRequestIdentity]bool{}
	for _, request := range value.requests {
		if replacements := updates[request.identity]; len(replacements) > 0 {
			if preserve {
				request.conditional = true
				if !appendBoundedHTTPRequest(&requests, request) {
					value.requests = []httpRequestValue{{overflow: true}}
					return
				}
			}
			if replaced[request.identity] {
				continue
			}
			replaced[request.identity] = true
			for _, replacement := range replacements {
				if !appendBoundedHTTPRequest(&requests, replacement) {
					value.requests = []httpRequestValue{{overflow: true}}
					return
				}
			}
		} else {
			if !appendBoundedHTTPRequest(&requests, request) {
				value.requests = []httpRequestValue{{overflow: true}}
				return
			}
		}
	}
	value.requests = requests
	for _, request := range value.requests {
		replaceHTTPRequestAliasesInObject(request.urlObject, updates, seen, preserve)
	}
	for _, object := range value.objects {
		replaceHTTPRequestAliasesInObject(object, updates, seen, preserve)
	}
}

func appendBoundedHTTPRequest(requests *[]httpRequestValue, candidate httpRequestValue) bool {
	if candidate.overflow {
		return false
	}
	for index, current := range *requests {
		if current.identity == candidate.identity && current.method == candidate.method && current.route == candidate.route &&
			current.source == candidate.source &&
			equalHTTPObjects(current.urlObject, candidate.urlObject, map[httpObjectPair]bool{}) {
			(*requests)[index].conditional = current.conditional || candidate.conditional
			return true
		}
	}
	if len(*requests) >= httpAlternativeLimit {
		return false
	}
	*requests = append(*requests, candidate)
	return true
}

func replaceHTTPRequestAliasesInObject(object *httpObjectValue, updates map[*httpRequestIdentity][]httpRequestValue,
	seen map[*httpObjectValue]bool, preserve bool,
) {
	if object == nil || seen[object] {
		return
	}
	seen[object] = true
	for field, value := range object.fields {
		replaceHTTPRequestAliases(&value, updates, seen, preserve)
		object.fields[field] = value
	}
}
