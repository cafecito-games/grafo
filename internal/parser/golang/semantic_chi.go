package golang

import (
	"bytes"
	"fmt"
	goast "go/ast"
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	"golang.org/x/tools/go/packages"
)

const (
	chiPackagePath  = "github.com/go-chi/chi/v5"
	chiSummaryLimit = 32
)

type chiFunction struct {
	object    *types.Func
	decl      *goast.FuncDecl
	path      string
	qualified string
	kind      graph.NodeKind
}

type chiRouter struct {
	endpoints []chiEndpointTemplate
	mounts    []chiMount
	mounted   bool
}

type chiState struct {
	router     *chiRouter
	prefix     string
	middleware []SemanticChiMiddleware
}

type chiEndpointTemplate struct {
	function     string
	functionKind graph.NodeKind
	method       string
	route        string
	handler      string
	handlerKind  graph.NodeKind
	location     graph.Location
	middleware   []SemanticChiMiddleware
	conditional  bool
	unresolved   bool
}

type chiMount struct {
	prefix      string
	middleware  []SemanticChiMiddleware
	child       *chiRouter
	location    graph.Location
	conditional bool
}

type chiExecution struct {
	created         []*chiRouter
	stack           map[*types.Func]bool
	parameterWrites map[*types.Func]map[types.Object]bool
}

type chiCallable struct {
	target   string
	kind     graph.NodeKind
	function *chiFunction
}

type chiAnalyzer struct {
	root      string
	pkg       *packages.Package
	views     map[string]SemanticView
	functions map[*types.Func]*chiFunction
	bindings  map[types.Object]chiCallable
	chiRouter *types.Interface
}

func collectChiPackageViews(root string, pkg *packages.Package, views map[string]SemanticView) {
	if pkg == nil || pkg.TypesInfo == nil || pkg.Fset == nil || pkg.Types == nil {
		return
	}
	chiImport := pkg.Imports[chiPackagePath]
	if chiImport == nil || chiImport.Types == nil {
		return
	}
	object, _ := chiImport.Types.Scope().Lookup("Router").(*types.TypeName)
	if object == nil {
		return
	}
	iface, _ := types.Unalias(object.Type()).Underlying().(*types.Interface)
	if iface == nil {
		return
	}
	iface.Complete()
	analyzer := &chiAnalyzer{
		root: root, pkg: pkg, views: views, functions: map[*types.Func]*chiFunction{},
		bindings: map[types.Object]chiCallable{}, chiRouter: iface,
	}
	analyzer.collectFunctions()
	analyzer.collectCallableBindings()
	analyzer.classifyHTTPCalls()
	analyzer.composeRoots()
}

func (a *chiAnalyzer) collectCallableBindings() {
	writes := map[types.Object]int{}
	candidates := map[types.Object]chiCallable{}
	record := func(identifier *goast.Ident, expression goast.Expr) {
		object := a.pkg.TypesInfo.Defs[identifier]
		if object == nil {
			object = a.pkg.TypesInfo.Uses[identifier]
		}
		if object == nil {
			return
		}
		writes[object]++
		function := a.directFunction(expression)
		if function == nil {
			delete(candidates, object)
			return
		}
		kind := graph.KindFunction
		if signature, _ := function.Type().(*types.Signature); signature != nil && signature.Recv() != nil {
			kind = graph.KindMethod
		}
		candidates[object] = chiCallable{target: objectTarget(function), kind: kind, function: a.functions[function]}
	}
	for _, file := range a.pkg.Syntax {
		goast.Inspect(file, func(node goast.Node) bool {
			switch value := node.(type) {
			case *goast.AssignStmt:
				for index, lhs := range value.Lhs {
					if index >= len(value.Rhs) {
						break
					}
					if identifier, ok := lhs.(*goast.Ident); ok {
						record(identifier, value.Rhs[index])
					}
				}
			case *goast.ValueSpec:
				for index, name := range value.Names {
					if index >= len(value.Values) {
						break
					}
					record(name, value.Values[index])
				}
			}
			return true
		})
	}
	for object, candidate := range candidates {
		if writes[object] == 1 {
			a.bindings[object] = candidate
		}
	}
}

func (a *chiAnalyzer) collectFunctions() {
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
			a.functions[object] = &chiFunction{
				object: object, decl: decl, path: path, qualified: objectTarget(object), kind: kind,
			}
		}
	}
}

func (a *chiAnalyzer) classifyHTTPCalls() {
	for _, file := range a.pkg.Syntax {
		goast.Inspect(file, func(node goast.Node) bool {
			call, ok := node.(*goast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := a.chiMethod(call); ok && chiEndpointMethod(name) {
				a.markCall(call, true)
				return true
			}
			selector, ok := call.Fun.(*goast.SelectorExpr)
			if !ok {
				return true
			}
			selection := a.pkg.TypesInfo.Selections[selector]
			if selection == nil {
				return true
			}
			name := strings.ToUpper(selection.Obj().Name())
			if !chiVerb(name) || len(call.Args) < 2 {
				return true
			}
			if selection.Obj().Pkg() == nil || selection.Obj().Pkg().Path() == chiPackagePath {
				return true
			}
			a.markCall(call, false)
			return true
		})
	}
}

func (a *chiAnalyzer) composeRoots() {
	called := map[*types.Func]bool{}
	for _, function := range a.functions {
		goast.Inspect(function.decl.Body, func(node goast.Node) bool {
			switch value := node.(type) {
			case *goast.CallExpr:
				if target := a.localFunction(value.Fun); target != nil {
					called[target.object] = true
				}
			}
			return true
		})
		// Route and Group callbacks are function values rather than calls.
		goast.Inspect(function.decl.Body, func(node goast.Node) bool {
			call, ok := node.(*goast.CallExpr)
			if !ok {
				return true
			}
			name, ok := a.chiMethod(call)
			if !ok || (name != "Route" && name != "Group") || len(call.Args) == 0 {
				return true
			}
			callback := call.Args[len(call.Args)-1]
			if target := a.localFunction(callback); target != nil {
				called[target.object] = true
			}
			return true
		})
	}

	var roots []*chiFunction
	for object, function := range a.functions {
		if !called[object] {
			roots = append(roots, function)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].qualified < roots[j].qualified })
	seen := map[string]bool{}
	for _, root := range roots {
		execution := &chiExecution{stack: map[*types.Func]bool{}}
		returned, _, _ := a.executeFunction(execution, root, nil, false)
		var routers []*chiRouter
		for _, state := range returned {
			if state.router != nil {
				routers = append(routers, state.router)
			}
		}
		if len(routers) == 0 {
			for _, router := range execution.created {
				if !router.mounted {
					routers = append(routers, router)
				}
			}
		}
		for _, router := range routers {
			a.materialize(router, "/", nil, false, map[*chiRouter]bool{}, seen)
		}
	}
	for path, view := range a.views {
		sort.Slice(view.ChiEndpoints, func(i, j int) bool {
			left, right := view.ChiEndpoints[i], view.ChiEndpoints[j]
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

func (a *chiAnalyzer) executeFunction(execution *chiExecution, function *chiFunction, arguments []chiState, conditional bool) ([]chiState, []chiState, []bool) {
	if function == nil || function.decl.Body == nil {
		return nil, nil, nil
	}
	if len(execution.stack) >= chiSummaryLimit || execution.stack[function.object] {
		a.diagnostic(semanticLocation(function.path, a.pkg.Fset, function.decl.Pos(), function.decl.End()),
			"recursive Chi router composition exceeded the bounded package summary")
		return nil, nil, nil
	}
	execution.stack[function.object] = true
	defer delete(execution.stack, function.object)
	environment := map[types.Object]chiState{}
	if execution.parameterWrites == nil {
		execution.parameterWrites = map[*types.Func]map[types.Object]bool{}
	}
	writes := map[types.Object]bool{}
	execution.parameterWrites[function.object] = writes
	defer delete(execution.parameterWrites, function.object)
	var parameterObjects []types.Object
	argument := 0
	if function.decl.Type.Params != nil {
		for _, field := range function.decl.Type.Params.List {
			for _, name := range field.Names {
				object := a.pkg.TypesInfo.Defs[name]
				parameterObjects = append(parameterObjects, object)
				writes[object] = false
				if argument < len(arguments) {
					environment[object] = cloneChiState(arguments[argument])
				}
				argument++
			}
		}
	}
	returned, _ := a.executeBlock(execution, function, function.decl.Body.List, environment, conditional)
	updated := make([]chiState, len(parameterObjects))
	reassigned := make([]bool, len(parameterObjects))
	for index, object := range parameterObjects {
		updated[index] = cloneChiState(environment[object])
		reassigned[index] = writes[object]
	}
	return returned, updated, reassigned
}

func (a *chiAnalyzer) executeBlock(execution *chiExecution, function *chiFunction, statements []goast.Stmt,
	environment map[types.Object]chiState, conditional bool,
) ([]chiState, bool) {
	var returned []chiState
	for _, statement := range statements {
		switch value := statement.(type) {
		case *goast.AssignStmt:
			for index, lhs := range value.Lhs {
				if index >= len(value.Rhs) {
					break
				}
				identifier, ok := lhs.(*goast.Ident)
				if !ok {
					continue
				}
				object := a.pkg.TypesInfo.Defs[identifier]
				if object == nil {
					object = a.pkg.TypesInfo.Uses[identifier]
				}
				if writes := execution.parameterWrites[function.object]; writes != nil {
					if _, isParameter := writes[object]; isParameter {
						writes[object] = true
					}
				}
				if state, ok := a.evalRouter(execution, function, value.Rhs[index], environment, conditional); ok {
					environment[object] = state
				} else if a.isChiRouter(a.pkg.TypesInfo.TypeOf(value.Rhs[index])) {
					a.diagnostic(a.location(function.path, value.Rhs[index]), "dynamic Chi router alias could not be proven; composition omitted")
				}
			}
		case *goast.DeclStmt:
			declaration, _ := value.Decl.(*goast.GenDecl)
			if declaration == nil {
				continue
			}
			for _, spec := range declaration.Specs {
				values, _ := spec.(*goast.ValueSpec)
				if values == nil {
					continue
				}
				for index, name := range values.Names {
					if index >= len(values.Values) {
						break
					}
					if state, ok := a.evalRouter(execution, function, values.Values[index], environment, conditional); ok {
						environment[a.pkg.TypesInfo.Defs[name]] = state
					}
				}
			}
		case *goast.ExprStmt:
			if call, ok := value.X.(*goast.CallExpr); ok {
				a.executeCall(execution, function, call, environment, conditional)
			}
		case *goast.ReturnStmt:
			for _, expression := range value.Results {
				if state, ok := a.evalRouter(execution, function, expression, environment, conditional); ok {
					returned = append(returned, state)
				}
			}
			return returned, true
		case *goast.BlockStmt:
			child, stopped := a.executeBlock(execution, function, value.List, environment, conditional)
			returned = append(returned, child...)
			if stopped {
				return returned, true
			}
		case *goast.IfStmt:
			if value.Init != nil {
				a.executeBlock(execution, function, []goast.Stmt{value.Init}, environment, conditional)
			}
			branch := cloneChiEnvironment(environment)
			child, _ := a.executeBlock(execution, function, value.Body.List, branch, true)
			a.diagnoseConditionalRouterMutation(function.path, value.Body, environment, branch)
			returned = append(returned, child...)
			if value.Else != nil {
				branch = cloneChiEnvironment(environment)
				child, _ = a.executeBlock(execution, function, statementList(value.Else), branch, true)
				a.diagnoseConditionalRouterMutation(function.path, value.Else, environment, branch)
				returned = append(returned, child...)
			}
		case *goast.ForStmt:
			branch := cloneChiEnvironment(environment)
			child, _ := a.executeBlock(execution, function, value.Body.List, branch, true)
			a.diagnoseConditionalRouterMutation(function.path, value.Body, environment, branch)
			returned = append(returned, child...)
		case *goast.RangeStmt:
			branch := cloneChiEnvironment(environment)
			child, _ := a.executeBlock(execution, function, value.Body.List, branch, true)
			a.diagnoseConditionalRouterMutation(function.path, value.Body, environment, branch)
			returned = append(returned, child...)
		case *goast.SwitchStmt:
			for _, item := range value.Body.List {
				clause, _ := item.(*goast.CaseClause)
				if clause != nil {
					branch := cloneChiEnvironment(environment)
					child, _ := a.executeBlock(execution, function, clause.Body, branch, true)
					a.diagnoseConditionalRouterMutation(function.path, clause, environment, branch)
					returned = append(returned, child...)
				}
			}
		case *goast.TypeSwitchStmt:
			for _, item := range value.Body.List {
				clause, _ := item.(*goast.CaseClause)
				if clause != nil {
					branch := cloneChiEnvironment(environment)
					child, _ := a.executeBlock(execution, function, clause.Body, branch, true)
					a.diagnoseConditionalRouterMutation(function.path, clause, environment, branch)
					returned = append(returned, child...)
				}
			}
		case *goast.SelectStmt:
			for _, item := range value.Body.List {
				clause, _ := item.(*goast.CommClause)
				if clause != nil {
					branch := cloneChiEnvironment(environment)
					child, _ := a.executeBlock(execution, function, clause.Body, branch, true)
					a.diagnoseConditionalRouterMutation(function.path, clause, environment, branch)
					returned = append(returned, child...)
				}
			}
		}
	}
	return returned, false
}

func (a *chiAnalyzer) executeCall(execution *chiExecution, function *chiFunction, call *goast.CallExpr,
	environment map[types.Object]chiState, conditional bool,
) (chiState, bool) {
	if name, ok := a.chiMethod(call); ok {
		if chiEndpointMethod(name) {
			a.markCall(call, true)
		}
		selector := call.Fun.(*goast.SelectorExpr)
		base, valid := a.evalRouter(execution, function, selector.X, environment, conditional)
		if !valid || base.router == nil {
			a.diagnostic(a.location(function.path, selector.X), "Chi router receiver could not be proven; composition omitted")
			return chiState{}, false
		}
		location := a.location(function.path, call)
		switch name {
		case "Use":
			base.middleware = append(base.middleware, a.middleware(call.Args, "use", function.path)...)
			if identifier, ok := selector.X.(*goast.Ident); ok {
				environment[a.pkg.TypesInfo.Uses[identifier]] = base
			}
			return base, true
		case "With":
			base.middleware = append(base.middleware, a.middleware(call.Args, "with", function.path)...)
			return base, true
		case "Group":
			if len(call.Args) > 0 {
				a.executeCallback(execution, function, call.Args[0], base, environment, conditional)
			}
			return base, true
		case "Route":
			if len(call.Args) < 2 {
				return base, true
			}
			prefix, ok := a.constantString(call.Args[0])
			if !ok {
				a.diagnostic(location, "dynamic Chi route prefix could not be proven; composition omitted")
				return base, true
			}
			joined, err := joinChiRoute(base.prefix, prefix)
			if err != nil {
				a.diagnostic(location, "invalid Chi route prefix: "+err.Error())
				return base, true
			}
			child := cloneChiState(base)
			child.prefix = joined
			a.executeCallback(execution, function, call.Args[1], child, environment, conditional)
			return base, true
		case "Mount":
			if len(call.Args) < 2 {
				return base, true
			}
			prefix, ok := a.constantString(call.Args[0])
			if !ok {
				a.diagnostic(location, "dynamic Chi mount prefix could not be proven; composition omitted")
				return base, true
			}
			joined, err := joinChiRoute(base.prefix, prefix)
			if err != nil {
				a.diagnostic(location, "invalid Chi mount prefix: "+err.Error())
				return base, true
			}
			child, ok := a.evalRouter(execution, function, call.Args[1], environment, conditional)
			if !ok || child.router == nil {
				a.diagnostic(location, "Chi mounted router could not be proven; composition omitted")
				return base, true
			}
			child.router.mounted = true
			base.router.mounts = append(base.router.mounts, chiMount{
				prefix: joined, middleware: cloneMiddleware(base.middleware), child: child.router,
				location: location, conditional: conditional,
			})
			return base, true
		default:
			if !chiEndpointMethod(name) {
				return base, true
			}
			if (name == "Method" || name == "MethodFunc") && len(call.Args) >= 3 {
				if _, ok := a.constantString(call.Args[0]); !ok {
					a.diagnostic(location, "dynamic Chi endpoint method could not be proven; endpoint omitted")
					return base, true
				}
			}
			method, routeIndex, handlerIndex, ok := a.endpointArguments(name, call.Args)
			if !ok {
				return base, true
			}
			route, ok := a.constantString(call.Args[routeIndex])
			if !ok {
				a.diagnostic(location, "dynamic Chi endpoint route could not be proven; endpoint omitted")
				return base, true
			}
			joined, err := joinChiRoute(base.prefix, route)
			if err != nil {
				a.diagnostic(location, "invalid Chi endpoint route: "+err.Error())
				return base, true
			}
			handler, handlerKind, unresolved := a.callableEvidence(call.Args[handlerIndex], function.path)
			base.router.endpoints = append(base.router.endpoints, chiEndpointTemplate{
				function: function.qualified, functionKind: function.kind, method: method, route: joined,
				handler: handler, handlerKind: handlerKind, location: location,
				middleware: cloneMiddleware(base.middleware), conditional: conditional, unresolved: unresolved,
			})
			return base, true
		}
	}

	target := a.localFunction(call.Fun)
	if target == nil {
		return chiState{}, false
	}
	arguments := make([]chiState, 0, len(call.Args))
	hasRouter := false
	hasUnprovenRouter := false
	for _, argument := range call.Args {
		state, ok := a.evalRouter(execution, function, argument, environment, conditional)
		arguments = append(arguments, state)
		hasRouter = hasRouter || ok
		if !ok && a.isChiRouter(a.pkg.TypesInfo.TypeOf(argument)) {
			hasUnprovenRouter = true
			a.diagnostic(a.location(function.path, argument), "Chi router receiver could not be proven; composition omitted")
		}
	}
	if hasUnprovenRouter {
		return chiState{}, false
	}
	if !hasRouter && !a.functionReturnsRouter(target) {
		return chiState{}, false
	}
	returned, updated, reassigned := a.executeFunction(execution, target, arguments, conditional)
	for index, state := range updated {
		if index >= len(call.Args) || state.router == nil || index >= len(arguments) {
			continue
		}
		original := arguments[index]
		if original.router == nil || sameChiState(original, state) {
			continue
		}
		if (index < len(reassigned) && reassigned[index]) || state.router != original.router || state.prefix != original.prefix {
			a.diagnostic(a.location(function.path, call.Args[index]),
				"Chi router helper argument state could not be propagated after reassignment; subsequent composition omitted")
			continue
		}
		if !a.storeRouterState(call.Args[index], state, environment) {
			a.diagnostic(a.location(function.path, call.Args[index]),
				"Chi router helper argument state could not be propagated; subsequent composition omitted")
		}
	}
	if len(returned) == 1 {
		return returned[0], true
	}
	if len(returned) > 1 {
		a.diagnostic(a.location(function.path, call), "ambiguous Chi router helper result; composition omitted")
	}
	return chiState{}, true
}

func (a *chiAnalyzer) evalRouter(execution *chiExecution, function *chiFunction, expression goast.Expr,
	environment map[types.Object]chiState, conditional bool,
) (chiState, bool) {
	switch value := expression.(type) {
	case *goast.ParenExpr:
		return a.evalRouter(execution, function, value.X, environment, conditional)
	case *goast.Ident:
		state, ok := environment[a.pkg.TypesInfo.Uses[value]]
		if !ok {
			state, ok = environment[a.pkg.TypesInfo.Defs[value]]
		}
		return cloneChiState(state), ok
	case *goast.CallExpr:
		if a.isChiNewRouter(value) {
			router := &chiRouter{}
			execution.created = append(execution.created, router)
			return chiState{router: router, prefix: "/"}, true
		}
		return a.executeCall(execution, function, value, environment, conditional)
	}
	return chiState{}, false
}

func (a *chiAnalyzer) executeCallback(execution *chiExecution, function *chiFunction, expression goast.Expr,
	state chiState, environment map[types.Object]chiState, conditional bool,
) {
	switch value := expression.(type) {
	case *goast.FuncLit:
		child := cloneChiEnvironment(environment)
		if value.Type.Params != nil && len(value.Type.Params.List) > 0 && len(value.Type.Params.List[0].Names) > 0 {
			child[a.pkg.TypesInfo.Defs[value.Type.Params.List[0].Names[0]]] = cloneChiState(state)
		}
		a.executeBlock(execution, function, value.Body.List, child, conditional)
	default:
		target := a.localFunction(expression)
		if target == nil {
			a.diagnostic(a.location(function.path, expression), "ambiguous Chi router callback; composition omitted")
			return
		}
		a.executeFunction(execution, target, []chiState{state}, conditional)
	}
}

func (a *chiAnalyzer) storeRouterState(expression goast.Expr, state chiState, environment map[types.Object]chiState) bool {
	identifier, ok := expression.(*goast.Ident)
	if !ok {
		return false
	}
	object := a.pkg.TypesInfo.Uses[identifier]
	if object == nil {
		object = a.pkg.TypesInfo.Defs[identifier]
	}
	if object != nil {
		environment[object] = cloneChiState(state)
		return true
	}
	return false
}

func (a *chiAnalyzer) diagnoseConditionalRouterMutation(path string, node goast.Node,
	before, after map[types.Object]chiState,
) {
	for object, original := range before {
		updated, ok := after[object]
		if !ok || sameChiState(original, updated) {
			continue
		}
		a.diagnostic(a.location(path, node), "conditional Chi router state mutation could not be proven; subsequent composition omitted")
		return
	}
}

func (a *chiAnalyzer) materialize(router *chiRouter, outerPrefix string, outerMiddleware []SemanticChiMiddleware,
	conditional bool, stack map[*chiRouter]bool, seen map[string]bool,
) {
	if router == nil {
		return
	}
	if stack[router] || len(stack) >= chiSummaryLimit {
		return
	}
	stack[router] = true
	defer delete(stack, router)
	for _, endpoint := range router.endpoints {
		route, err := joinChiRoute(outerPrefix, endpoint.route)
		if err != nil {
			a.diagnostic(endpoint.location, "compose Chi endpoint: "+err.Error())
			continue
		}
		key := strings.Join([]string{endpoint.location.Path, fmt.Sprint(endpoint.location.Line), fmt.Sprint(endpoint.location.Column), endpoint.method, route}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		view := a.views[endpoint.location.Path]
		view.ChiEndpoints = append(view.ChiEndpoints, SemanticChiEndpoint{
			Function: endpoint.function, FunctionKind: endpoint.functionKind,
			Method: endpoint.method, Route: route, Handler: endpoint.handler, HandlerKind: endpoint.handlerKind,
			Location:    endpoint.location,
			Middleware:  append(cloneMiddleware(outerMiddleware), endpoint.middleware...),
			Conditional: conditional || endpoint.conditional, Unresolved: endpoint.unresolved,
		})
		a.views[endpoint.location.Path] = view
	}
	for _, mount := range router.mounts {
		prefix, err := joinChiRoute(outerPrefix, mount.prefix)
		if err != nil {
			a.diagnostic(mount.location, "compose Chi mount: "+err.Error())
			continue
		}
		middleware := append(cloneMiddleware(outerMiddleware), mount.middleware...)
		a.materialize(mount.child, prefix, middleware, conditional || mount.conditional, stack, seen)
	}
}

func (a *chiAnalyzer) endpointArguments(name string, arguments []goast.Expr) (string, int, int, bool) {
	switch name {
	case "Method", "MethodFunc":
		if len(arguments) < 3 {
			return "", 0, 0, false
		}
		method, ok := a.constantString(arguments[0])
		return strings.ToUpper(method), 1, 2, ok
	case "Handle", "HandleFunc":
		if len(arguments) < 2 {
			return "", 0, 0, false
		}
		return "ANY", 0, 1, true
	default:
		if !chiVerb(strings.ToUpper(name)) || len(arguments) < 2 {
			return "", 0, 0, false
		}
		return strings.ToUpper(name), 0, len(arguments) - 1, true
	}
}

func (a *chiAnalyzer) middleware(arguments []goast.Expr, form, path string) []SemanticChiMiddleware {
	result := make([]SemanticChiMiddleware, 0, len(arguments))
	for _, argument := range arguments {
		target, kind, unresolved := a.callableEvidence(argument, path)
		result = append(result, SemanticChiMiddleware{
			Target: target, TargetKind: kind, Form: form, Location: a.location(path, argument), Unresolved: unresolved,
		})
	}
	return result
}

func (a *chiAnalyzer) callableEvidence(expression goast.Expr, path string) (string, graph.NodeKind, bool) {
	var object types.Object
	switch value := expression.(type) {
	case *goast.Ident:
		object = a.pkg.TypesInfo.Uses[value]
		if object == nil {
			object = a.pkg.TypesInfo.Defs[value]
		}
	case *goast.SelectorExpr:
		if selection := a.pkg.TypesInfo.Selections[value]; selection != nil {
			object = selection.Obj()
		} else {
			object = a.pkg.TypesInfo.Uses[value.Sel]
		}
	}
	if function, ok := object.(*types.Func); ok {
		kind := graph.KindFunction
		if signature, _ := function.Type().(*types.Signature); signature != nil && signature.Recv() != nil {
			kind = graph.KindMethod
		}
		return objectTarget(function), kind, false
	}
	if binding, ok := a.bindings[object]; ok {
		return binding.target, binding.kind, false
	}
	location := a.location(path, expression)
	target := renderSemanticExpression(a.pkg.Fset, expression)
	if strings.TrimSpace(target) == "" {
		target = fmt.Sprintf("callable@%s:%d:%d", path, location.Line, location.Column)
	}
	return target, graph.KindExternal, true
}

func (a *chiAnalyzer) chiMethod(call *goast.CallExpr) (string, bool) {
	selector, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return "", false
	}
	selection := a.pkg.TypesInfo.Selections[selector]
	if selection == nil || selection.Obj().Pkg() == nil || selection.Obj().Pkg().Path() != chiPackagePath {
		return "", false
	}
	name := selection.Obj().Name()
	switch name {
	case "Route", "Mount", "Group", "Use", "With", "Method", "MethodFunc", "Handle", "HandleFunc",
		"Connect", "Delete", "Get", "Head", "Options", "Patch", "Post", "Put", "Trace":
		return name, true
	default:
		return "", false
	}
}

func (a *chiAnalyzer) isChiNewRouter(call *goast.CallExpr) bool {
	selector, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return false
	}
	function, _ := a.pkg.TypesInfo.Uses[selector.Sel].(*types.Func)
	return function != nil && function.Pkg() != nil && function.Pkg().Path() == chiPackagePath && function.Name() == "NewRouter"
}

func (a *chiAnalyzer) localFunction(expression goast.Expr) *chiFunction {
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
	if local := a.functions[function]; local != nil {
		return local
	}
	return a.bindings[object].function
}

func (a *chiAnalyzer) directFunction(expression goast.Expr) *types.Func {
	for {
		switch value := expression.(type) {
		case *goast.ParenExpr:
			expression = value.X
			continue
		case *goast.IndexExpr:
			expression = value.X
			continue
		case *goast.IndexListExpr:
			expression = value.X
			continue
		}
		break
	}
	switch value := expression.(type) {
	case *goast.Ident:
		function, _ := a.pkg.TypesInfo.Uses[value].(*types.Func)
		return function
	case *goast.SelectorExpr:
		if selection := a.pkg.TypesInfo.Selections[value]; selection != nil {
			function, _ := selection.Obj().(*types.Func)
			return function
		}
		function, _ := a.pkg.TypesInfo.Uses[value.Sel].(*types.Func)
		return function
	default:
		return nil
	}
}

func (a *chiAnalyzer) functionReturnsRouter(function *chiFunction) bool {
	signature, _ := function.object.Type().(*types.Signature)
	if signature == nil {
		return false
	}
	for index := 0; index < signature.Results().Len(); index++ {
		if a.isChiRouter(signature.Results().At(index).Type()) {
			return true
		}
	}
	return false
}

func (a *chiAnalyzer) isChiRouter(value types.Type) bool {
	if value == nil || a.chiRouter == nil {
		return false
	}
	return types.Implements(value, a.chiRouter) || types.Implements(types.NewPointer(value), a.chiRouter)
}

func (a *chiAnalyzer) constantString(expression goast.Expr) (string, bool) {
	value := a.pkg.TypesInfo.Types[expression].Value
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
}

func (a *chiAnalyzer) markCall(call *goast.CallExpr, endpoint bool) {
	position := a.pkg.Fset.Position(call.Lparen)
	path, ok := relativeSourcePath(a.root, position.Filename)
	if !ok {
		return
	}
	view := a.views[path]
	if endpoint {
		if view.ChiEndpointCalls == nil {
			view.ChiEndpointCalls = map[int]bool{}
		}
		view.ChiEndpointCalls[position.Offset] = true
	} else {
		if view.NonChiHTTPCalls == nil {
			view.NonChiHTTPCalls = map[int]bool{}
		}
		view.NonChiHTTPCalls[position.Offset] = true
	}
	a.views[path] = view
}

func (a *chiAnalyzer) diagnostic(location graph.Location, message string) {
	view := a.views[location.Path]
	view.Diagnostics = append(view.Diagnostics, graph.Diagnostic{
		Path: location.Path, Line: location.Line, Level: "warning", Message: message,
	})
	a.views[location.Path] = view
}

func (a *chiAnalyzer) location(path string, node goast.Node) graph.Location {
	return semanticLocation(path, a.pkg.Fset, node.Pos(), node.End())
}

func cloneChiState(state chiState) chiState {
	state.middleware = cloneMiddleware(state.middleware)
	return state
}

func sameChiState(left, right chiState) bool {
	if left.router != right.router || left.prefix != right.prefix || len(left.middleware) != len(right.middleware) {
		return false
	}
	for index := range left.middleware {
		if left.middleware[index] != right.middleware[index] {
			return false
		}
	}
	return true
}

func cloneMiddleware(values []SemanticChiMiddleware) []SemanticChiMiddleware {
	return append([]SemanticChiMiddleware(nil), values...)
}

func cloneChiEnvironment(values map[types.Object]chiState) map[types.Object]chiState {
	result := make(map[types.Object]chiState, len(values))
	for object, state := range values {
		result[object] = cloneChiState(state)
	}
	return result
}

func statementList(statement goast.Stmt) []goast.Stmt {
	if block, ok := statement.(*goast.BlockStmt); ok {
		return block.List
	}
	return []goast.Stmt{statement}
}

func joinChiRoute(prefix, leaf string) (string, error) {
	joined, err := httpmodel.Join(prefix, leaf)
	if err != nil {
		return "", err
	}
	return joined.Raw, nil
}

func chiEndpointMethod(name string) bool {
	return name == "Method" || name == "MethodFunc" || name == "Handle" || name == "HandleFunc" || chiVerb(strings.ToUpper(name))
}

func chiVerb(name string) bool {
	switch name {
	case "CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE":
		return true
	default:
		return false
	}
}

func renderSemanticExpression(fset *token.FileSet, expression goast.Expr) string {
	var buffer bytes.Buffer
	if err := format.Node(&buffer, fset, expression); err != nil {
		return ""
	}
	return buffer.String()
}
