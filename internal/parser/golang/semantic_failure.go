package golang

import (
	goast "go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"golang.org/x/tools/go/packages"
)

type failureTarget struct {
	name       string
	kind       graph.NodeKind
	properties map[string]string
}

type failureExtractor struct {
	pkg  *packages.Package
	path string
	view *SemanticView
}

func collectFailureView(_ string, pkg *packages.Package, file *goast.File, path string, view *SemanticView) {
	if pkg == nil || pkg.TypesInfo == nil || pkg.Fset == nil {
		return
	}
	extractor := failureExtractor{pkg: pkg, path: path, view: view}
	if view.Functions == nil {
		view.Functions = map[string]SemanticFunction{}
	}
	extractor.collectDeclarations(file)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*goast.FuncDecl)
		if ok {
			extractor.collectFunction(function)
		}
	}
	sort.Slice(view.ErrorDeclarations, func(i, j int) bool {
		return view.ErrorDeclarations[i].QualifiedName < view.ErrorDeclarations[j].QualifiedName
	})
	sort.Slice(view.Failures, func(i, j int) bool {
		left, right := view.Failures[i], view.Failures[j]
		if left.Function != right.Function {
			return left.Function < right.Function
		}
		if left.Location.Line != right.Location.Line {
			return left.Location.Line < right.Location.Line
		}
		if left.Location.Column != right.Location.Column {
			return left.Location.Column < right.Location.Column
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Target < right.Target
	})
}

func (e failureExtractor) collectDeclarations(file *goast.File) {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*goast.GenDecl)
		if !ok {
			continue
		}
		for _, raw := range general.Specs {
			switch spec := raw.(type) {
			case *goast.TypeSpec:
				if interfaceType, ok := spec.Type.(*goast.InterfaceType); ok {
					for _, field := range interfaceType.Methods.List {
						for _, name := range field.Names {
							method, _ := e.pkg.TypesInfo.Defs[name].(*types.Func)
							if method == nil {
								continue
							}
							signature, _ := method.Type().(*types.Signature)
							e.collectSignature(objectTarget(method), signature, name.Pos())
						}
					}
				}
				object, _ := e.pkg.TypesInfo.Defs[spec.Name].(*types.TypeName)
				if object == nil || !isGoErrorType(object.Type()) && !isGoErrorType(types.NewPointer(object.Type())) {
					continue
				}
				e.view.ErrorDeclarations = append(e.view.ErrorDeclarations, SemanticErrorDeclaration{
					Name: spec.Name.Name, QualifiedName: qualifiedObject(object), Kind: graph.KindType,
					Type: qualifiedType(object.Type()), Location: e.location(spec.Name.Pos(), spec.Name.End()),
				})
			case *goast.ValueSpec:
				for index, name := range spec.Names {
					object, _ := e.pkg.TypesInfo.Defs[name].(*types.Var)
					if object == nil || object.Parent() != e.pkg.Types.Scope() || !isGoErrorType(object.Type()) {
						continue
					}
					sentinel := index < len(spec.Values) && !isNilExpression(spec.Values[index])
					if len(spec.Values) == 1 && len(spec.Names) > 1 {
						sentinel = !isNilExpression(spec.Values[0])
					}
					e.view.ErrorDeclarations = append(e.view.ErrorDeclarations, SemanticErrorDeclaration{
						Name: name.Name, QualifiedName: qualifiedObject(object), Kind: graph.KindVariable,
						Type: qualifiedType(object.Type()), Location: e.location(name.Pos(), name.End()), Sentinel: sentinel,
					})
				}
			}
		}
	}
}

func (e failureExtractor) collectFunction(declaration *goast.FuncDecl) {
	object, _ := e.pkg.TypesInfo.Defs[declaration.Name].(*types.Func)
	functionName := objectTarget(object)
	if functionName == "" {
		return
	}
	signature, _ := object.Type().(*types.Signature)
	if signature == nil {
		return
	}
	e.collectSignature(functionName, signature, declaration.Name.Pos())
	if declaration.Body == nil {
		return
	}
	origins := e.collectOrigins(declaration.Body, functionName)
	e.collectReturns(declaration.Body, functionName, signature, origins)
	e.collectHandlers(declaration.Body, functionName, signature, origins)
	e.collectAbruptAndDeferred(declaration.Body, functionName, origins)
}

func (e failureExtractor) collectSignature(functionName string, signature *types.Signature, fallback token.Pos) {
	if functionName == "" || signature == nil {
		return
	}
	function := SemanticFunction{}
	for position := 0; position < signature.Results().Len(); position++ {
		result := signature.Results().At(position)
		if !isGoErrorType(result.Type()) {
			continue
		}
		target, kind := errorTypeTarget(result.Type())
		function.ErrorResults = append(function.ErrorResults, SemanticErrorResult{Position: position, Type: target})
		resultPosition := result.Pos()
		if !resultPosition.IsValid() {
			resultPosition = fallback
		}
		e.add(functionName, graph.EdgeReturnsError, failureTarget{name: target, kind: kind},
			e.location(resultPosition, resultPosition), map[string]string{
				"form": "signature", "result_position": strconv.Itoa(position), "type": target,
			})
	}
	e.view.Functions[functionName] = function
}

func (e failureExtractor) collectOrigins(body *goast.BlockStmt, functionName string) map[types.Object][]failureTarget {
	origins := map[types.Object][]failureTarget{}
	goast.Inspect(body, func(node goast.Node) bool {
		switch value := node.(type) {
		case *goast.FuncLit:
			return false
		case *goast.AssignStmt:
			if len(value.Rhs) == 1 && len(value.Lhs) > 1 {
				call, ok := value.Rhs[0].(*goast.CallExpr)
				if ok {
					for position, left := range value.Lhs {
						targets := e.callTargets(call, position)
						if nodeWithinConditional(body, value.Pos()) {
							targets = conditionalFailureTargets(targets)
						}
						e.recordOrigin(origins, left, targets)
					}
					return true
				}
			}
			for index, left := range value.Lhs {
				if index < len(value.Rhs) {
					targets := e.expressionTargets(value.Rhs[index], origins, functionName)
					if nodeWithinConditional(body, value.Pos()) {
						targets = conditionalFailureTargets(targets)
					}
					e.recordOrigin(origins, left, targets)
				}
			}
		case *goast.ValueSpec:
			for index, name := range value.Names {
				if index < len(value.Values) {
					targets := e.expressionTargets(value.Values[index], origins, functionName)
					if nodeWithinConditional(body, value.Pos()) {
						targets = conditionalFailureTargets(targets)
					}
					e.recordOrigin(origins, name, targets)
				}
			}
		}
		return true
	})
	return origins
}

func (e failureExtractor) recordOrigin(origins map[types.Object][]failureTarget, expression goast.Expr, targets []failureTarget) {
	identifier, ok := expression.(*goast.Ident)
	if !ok {
		return
	}
	object := e.pkg.TypesInfo.Defs[identifier]
	if object == nil {
		object = e.pkg.TypesInfo.Uses[identifier]
	}
	if object == nil || !isGoErrorType(object.Type()) {
		return
	}
	for _, target := range targets {
		origins[object] = appendUniqueFailureTarget(origins[object], target)
	}
}

func (e failureExtractor) collectReturns(body *goast.BlockStmt, functionName string, signature *types.Signature, origins map[types.Object][]failureTarget) {
	errorPositions := errorResultPositions(signature)
	goast.Inspect(body, func(node goast.Node) bool {
		if _, nested := node.(*goast.FuncLit); nested {
			return false
		}
		statement, ok := node.(*goast.ReturnStmt)
		if !ok {
			return true
		}
		conditional := strconv.FormatBool(nodeWithinConditional(body, statement.Pos()))
		for _, position := range errorPositions {
			expression := returnExpression(statement, position, signature.Results().Len())
			if expression == nil {
				result := signature.Results().At(position)
				for _, target := range origins[result] {
					e.addPropagation(functionName, graph.EdgePropagatesError, target, statement, "return", position, conditional)
				}
				continue
			}
			if isNilExpression(expression) {
				continue
			}
			if call, ok := expression.(*goast.CallExpr); ok {
				callee := callableTarget(call.Fun, e.pkg.TypesInfo, nil)
				switch callee {
				case "fmt.Errorf":
					if wrappedArguments, ok := wrappedErrorArguments(call); ok {
						targets := e.errorArgumentTargets(wrappedArguments, origins, functionName)
						if len(targets) == 0 {
							targets = []failureTarget{e.unresolved("error", call.Pos())}
						}
						for _, target := range targets {
							e.addPropagation(functionName, graph.EdgeWrapsError, target, statement, "wrap", position, conditional)
						}
						continue
					}
				case "errors.Join":
					targets := e.errorArgumentTargets(call.Args, origins, functionName)
					for _, target := range targets {
						e.addPropagation(functionName, graph.EdgePropagatesError, target, statement, "join", position, conditional)
					}
					continue
				}
			}
			targets := e.expressionTargets(expression, origins, functionName)
			if len(targets) == 0 {
				targets = []failureTarget{e.unresolved("error", expression.Pos())}
			}
			form := "return"
			if _, ok := expression.(*goast.CallExpr); ok {
				form = "return_call"
			}
			for _, target := range targets {
				e.addPropagation(functionName, graph.EdgePropagatesError, target, statement, form, position, conditional)
			}
		}
		return true
	})
}

func (e failureExtractor) addPropagation(functionName string, kind graph.EdgeKind, target failureTarget, node goast.Node, form string, position int, conditional string) {
	properties := map[string]string{
		"form": form, "result_position": strconv.Itoa(position), "conditional": conditional,
	}
	for key, value := range target.properties {
		properties[key] = value
	}
	e.add(functionName, kind, target, e.location(node.Pos(), node.End()), properties)
}

func (e failureExtractor) collectHandlers(body *goast.BlockStmt, functionName string, signature *types.Signature, origins map[types.Object][]failureTarget) {
	goast.Inspect(body, func(node goast.Node) bool {
		if _, nested := node.(*goast.FuncLit); nested {
			return false
		}
		switch statement := node.(type) {
		case *goast.IfStmt:
			if !branchConsumesFailure(statement.Body.List, signature) {
				return true
			}
			e.handlersFromCondition(functionName, statement.Cond, statement, origins)
		case *goast.SwitchStmt:
			if statement.Tag == nil || !isGoErrorType(e.pkg.TypesInfo.TypeOf(statement.Tag)) {
				return true
			}
			for _, rawClause := range statement.Body.List {
				clause, ok := rawClause.(*goast.CaseClause)
				if !ok || !branchConsumesFailure(clause.Body, signature) {
					continue
				}
				for _, expression := range clause.List {
					for _, target := range e.expressionTargets(expression, origins, functionName) {
						e.add(functionName, graph.EdgeHandlesError, target,
							e.location(clause.Pos(), clause.End()), map[string]string{
								"form": "switch", "conditional": "true",
							})
					}
				}
			}
		}
		return true
	})
}

func (e failureExtractor) handlersFromCondition(functionName string, condition goast.Expr, evidence goast.Node, origins map[types.Object][]failureTarget) {
	seen := map[string]bool{}
	emit := func(form string, target failureTarget) {
		key := form + "\x00" + target.name
		if target.name == "" || seen[key] {
			return
		}
		seen[key] = true
		e.add(functionName, graph.EdgeHandlesError, target, e.location(evidence.Pos(), evidence.End()),
			map[string]string{"form": form, "conditional": "true"})
	}
	goast.Inspect(condition, func(node goast.Node) bool {
		switch expression := node.(type) {
		case *goast.CallExpr:
			callee := callableTarget(expression.Fun, e.pkg.TypesInfo, nil)
			switch callee {
			case "errors.Is":
				if len(expression.Args) > 1 {
					for _, target := range e.expressionTargets(expression.Args[1], origins, functionName) {
						emit("is", target)
					}
				}
			case "errors.As":
				if len(expression.Args) > 1 {
					target, kind := errorTypeTarget(e.pkg.TypesInfo.TypeOf(expression.Args[1]))
					if target != "" {
						emit("as", failureTarget{name: target, kind: kind})
					}
				}
			}
		case *goast.BinaryExpr:
			if expression.Op != token.EQL && expression.Op != token.NEQ {
				return true
			}
			leftError := isGoErrorType(e.pkg.TypesInfo.TypeOf(expression.X))
			rightError := isGoErrorType(e.pkg.TypesInfo.TypeOf(expression.Y))
			if !leftError && !rightError {
				return true
			}
			errorExpression, other := expression.X, expression.Y
			if !leftError {
				errorExpression, other = expression.Y, expression.X
			}
			targets := e.expressionTargets(other, origins, functionName)
			if isNilExpression(other) {
				targets = e.expressionTargets(errorExpression, origins, functionName)
			}
			for _, target := range targets {
				emit("comparison", target)
			}
		}
		return true
	})
}

func (e failureExtractor) collectAbruptAndDeferred(body *goast.BlockStmt, functionName string, origins map[types.Object][]failureTarget) {
	goast.Inspect(body, func(node goast.Node) bool {
		switch value := node.(type) {
		case *goast.DeferStmt:
			target := e.callTarget(value.Call)
			if target.name == "" {
				target = e.unresolved("defer", value.Pos())
			}
			e.add(functionName, graph.EdgeDefers, target, e.location(value.Pos(), value.End()), map[string]string{
				"form": "defer", "conditional": strconv.FormatBool(nodeWithinConditional(body, value.Pos())),
			})
		case *goast.CallExpr:
			callee := callableTarget(value.Fun, e.pkg.TypesInfo, nil)
			switch callee {
			case "builtin.panic":
				targets := e.errorArgumentTargets(value.Args, origins, functionName)
				if len(targets) == 0 {
					targets = []failureTarget{e.unresolved("panic", value.Pos())}
				}
				for _, target := range targets {
					e.add(functionName, graph.EdgePanics, target, e.location(value.Pos(), value.End()), map[string]string{
						"form": "panic", "conditional": strconv.FormatBool(nodeWithinConditional(body, value.Pos())),
					})
				}
			case "builtin.recover":
				e.add(functionName, graph.EdgeRecovers, failureTarget{name: "builtin.recover"},
					e.location(value.Pos(), value.End()), map[string]string{
						"form": "recover", "conditional": strconv.FormatBool(nodeWithinConditional(body, value.Pos())),
					})
			}
		}
		return true
	})
}

func (e failureExtractor) expressionTargets(expression goast.Expr, origins map[types.Object][]failureTarget, functionName string) []failureTarget {
	originalType := e.pkg.TypesInfo.TypeOf(expression)
	for {
		switch value := expression.(type) {
		case *goast.ParenExpr:
			expression = value.X
			continue
		case *goast.UnaryExpr:
			if value.Op == token.AND || value.Op == token.MUL {
				expression = value.X
				continue
			}
		}
		break
	}
	switch value := expression.(type) {
	case *goast.Ident:
		if value.Name == "nil" {
			return nil
		}
		object := e.pkg.TypesInfo.Uses[value]
		if object == nil {
			object = e.pkg.TypesInfo.Defs[value]
		}
		if targets := origins[object]; len(targets) > 0 {
			result := cloneFailureTargets(targets)
			if len(result) > 1 {
				result = conditionalFailureTargets(result)
			}
			return result
		}
		if variable, ok := object.(*types.Var); ok && variable.Parent() == e.pkg.Types.Scope() && isGoErrorType(variable.Type()) {
			return []failureTarget{{name: qualifiedObject(variable), kind: graph.KindVariable}}
		}
	case *goast.SelectorExpr:
		if variable, ok := e.pkg.TypesInfo.Uses[value.Sel].(*types.Var); ok && isGoErrorType(variable.Type()) {
			return []failureTarget{{name: qualifiedObject(variable), kind: graph.KindVariable}}
		}
	case *goast.CallExpr:
		return e.callTargets(value, -1)
	}
	valueType := e.pkg.TypesInfo.TypeOf(expression)
	if isGoErrorType(originalType) {
		valueType = originalType
	}
	if isGoErrorType(valueType) {
		target, kind := errorTypeTarget(valueType)
		if target != "" {
			return []failureTarget{{name: target, kind: kind}}
		}
	}
	return nil
}

func (e failureExtractor) callTargets(call *goast.CallExpr, requestedPosition int) []failureTarget {
	callee := callableTarget(call.Fun, e.pkg.TypesInfo, nil)
	if callee == "" {
		return nil
	}
	valueType := e.pkg.TypesInfo.TypeOf(call)
	var targets []failureTarget
	if tuple, ok := valueType.(*types.Tuple); ok {
		for position := 0; position < tuple.Len(); position++ {
			if requestedPosition >= 0 && position != requestedPosition || !isGoErrorType(tuple.At(position).Type()) {
				continue
			}
			targets = append(targets, failureTarget{name: callee,
				properties: map[string]string{"callee_result_position": strconv.Itoa(position)}})
		}
	} else if (requestedPosition < 0 || requestedPosition == 0) && isGoErrorType(valueType) {
		targets = append(targets, failureTarget{name: callee,
			properties: map[string]string{"callee_result_position": "0"}})
	}
	return targets
}

func (e failureExtractor) callTarget(call *goast.CallExpr) failureTarget {
	target := callableTarget(call.Fun, e.pkg.TypesInfo, nil)
	if target == "" {
		return failureTarget{}
	}
	return failureTarget{name: target}
}

func (e failureExtractor) errorArgumentTargets(arguments []goast.Expr, origins map[types.Object][]failureTarget, functionName string) []failureTarget {
	var result []failureTarget
	for _, argument := range arguments {
		if !isGoErrorType(e.pkg.TypesInfo.TypeOf(argument)) {
			continue
		}
		for _, target := range e.expressionTargets(argument, origins, functionName) {
			result = appendUniqueFailureTarget(result, target)
		}
	}
	return result
}

func (e failureExtractor) unresolved(category string, position token.Pos) failureTarget {
	location := e.location(position, position)
	return failureTarget{
		name: "unresolved:" + category + "@" + e.path + ":" + strconv.Itoa(location.Line) + ":" + strconv.Itoa(location.Column),
		kind: graph.KindExternal, properties: map[string]string{"unresolved": "true"},
	}
}

func (e failureExtractor) add(functionName string, kind graph.EdgeKind, target failureTarget, location graph.Location, properties map[string]string) {
	if functionName == "" || target.name == "" {
		return
	}
	if properties == nil {
		properties = map[string]string{}
	}
	properties["evidence"] = "go/types"
	properties["resolution"] = "go/types"
	for key, value := range target.properties {
		properties[key] = value
	}
	e.view.Failures = append(e.view.Failures, SemanticFailure{
		Function: functionName, Kind: kind, Target: target.name, TargetKind: target.kind,
		Location: location, Properties: properties,
	})
}

func (e failureExtractor) location(start, end token.Pos) graph.Location {
	startPosition := e.pkg.Fset.Position(start)
	endPosition := e.pkg.Fset.Position(end)
	return graph.Location{Path: e.path, Line: startPosition.Line, Column: startPosition.Column, EndLine: endPosition.Line}
}

func isGoErrorType(value types.Type) bool {
	if value == nil {
		return false
	}
	errorObject := types.Universe.Lookup("error")
	if errorObject == nil {
		return false
	}
	errorType := errorObject.Type()
	return types.AssignableTo(value, errorType) || types.Implements(value, errorType.Underlying().(*types.Interface))
}

func errorTypeTarget(value types.Type) (string, graph.NodeKind) {
	if value == nil {
		return "", ""
	}
	for {
		switch current := value.(type) {
		case *types.Pointer:
			value = current.Elem()
			continue
		}
		break
	}
	value = types.Unalias(value)
	if named, ok := value.(*types.Named); ok && named.Obj() != nil && named.Obj().Pkg() != nil {
		return named.Obj().Pkg().Path() + "." + named.Obj().Name(), graph.KindType
	}
	if isGoErrorType(value) {
		return "builtin.error", graph.KindInterface
	}
	return "", ""
}

func qualifiedObject(object types.Object) string {
	if object == nil {
		return ""
	}
	if object.Pkg() == nil {
		return "builtin." + object.Name()
	}
	return object.Pkg().Path() + "." + object.Name()
}

func qualifiedType(value types.Type) string {
	return types.TypeString(value, func(pkg *types.Package) string { return pkg.Path() })
}

func errorResultPositions(signature *types.Signature) []int {
	var result []int
	if signature == nil || signature.Results() == nil {
		return result
	}
	for position := 0; position < signature.Results().Len(); position++ {
		if isGoErrorType(signature.Results().At(position).Type()) {
			result = append(result, position)
		}
	}
	return result
}

func returnExpression(statement *goast.ReturnStmt, position, resultCount int) goast.Expr {
	if len(statement.Results) == 0 {
		return nil
	}
	if len(statement.Results) == resultCount && position < len(statement.Results) {
		return statement.Results[position]
	}
	if len(statement.Results) == 1 {
		return statement.Results[0]
	}
	return nil
}

func wrappedErrorArguments(call *goast.CallExpr) ([]goast.Expr, bool) {
	if len(call.Args) == 0 {
		return nil, false
	}
	literal, ok := call.Args[0].(*goast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil, false
	}
	format, err := strconv.Unquote(literal.Value)
	if err != nil {
		return nil, false
	}
	indexes, ok := wrapArgumentIndexes(format)
	if !ok || len(indexes) == 0 {
		return nil, false
	}
	arguments := make([]goast.Expr, 0, len(indexes))
	for _, index := range indexes {
		// Indexes are relative to the first formatting operand, after the format.
		if index < 0 || index+1 >= len(call.Args) {
			return nil, false
		}
		arguments = append(arguments, call.Args[index+1])
	}
	return arguments, true
}

// wrapArgumentIndexes parses the deliberately bounded subset of fmt's format
// grammar needed to prove which operands a %w consumes. Star widths/precision
// and unusual index placement remain generic fmt.Errorf propagation rather than
// risking a false wrapping identity.
func wrapArgumentIndexes(format string) ([]int, bool) {
	nextArgument := 0
	var result []int
	for index := 0; index < len(format); index++ {
		if format[index] != '%' {
			continue
		}
		index++
		if index >= len(format) {
			return nil, false
		}
		if format[index] == '%' {
			continue
		}
		argument := -1
		if format[index] == '[' {
			closing := strings.IndexByte(format[index:], ']')
			if closing <= 1 {
				return nil, false
			}
			value, err := strconv.Atoi(format[index+1 : index+closing])
			if err != nil || value < 1 {
				return nil, false
			}
			argument = value - 1
			index += closing + 1
			if index >= len(format) {
				return nil, false
			}
		}
		for index < len(format) && strings.ContainsRune("+#- 0", rune(format[index])) {
			index++
		}
		if index >= len(format) || format[index] == '*' || format[index] == '[' {
			return nil, false
		}
		for index < len(format) && format[index] >= '0' && format[index] <= '9' {
			index++
		}
		if index < len(format) && format[index] == '.' {
			index++
			if index >= len(format) || format[index] == '*' || format[index] == '[' {
				return nil, false
			}
			for index < len(format) && format[index] >= '0' && format[index] <= '9' {
				index++
			}
		}
		if index >= len(format) {
			return nil, false
		}
		if argument < 0 {
			argument = nextArgument
		}
		nextArgument = argument + 1
		if format[index] == 'w' {
			result = append(result, argument)
		}
	}
	return result, true
}

func isNilExpression(expression goast.Expr) bool {
	identifier, ok := expression.(*goast.Ident)
	return ok && identifier.Name == "nil"
}

func branchConsumesFailure(statements []goast.Stmt, signature *types.Signature) bool {
	consumes := false
	for _, statement := range statements {
		goast.Inspect(statement, func(node goast.Node) bool {
			if consumes {
				return false
			}
			if _, nested := node.(*goast.FuncLit); nested {
				return false
			}
			switch value := node.(type) {
			case *goast.BranchStmt:
				consumes = value.Tok == token.CONTINUE || value.Tok == token.BREAK
			case *goast.ReturnStmt:
				positions := errorResultPositions(signature)
				if len(positions) == 0 {
					consumes = true
					return false
				}
				if len(value.Results) == 0 {
					return false
				}
				consumes = true
				for _, position := range positions {
					expression := returnExpression(value, position, signature.Results().Len())
					if expression == nil || !isNilExpression(expression) {
						consumes = false
						break
					}
				}
			}
			return !consumes
		})
		if consumes {
			return true
		}
	}
	return false
}

func nodeWithinConditional(body *goast.BlockStmt, position token.Pos) bool {
	conditional := false
	goast.Inspect(body, func(node goast.Node) bool {
		if node == nil || conditional || position < node.Pos() || position > node.End() {
			return false
		}
		switch node.(type) {
		case *goast.IfStmt, *goast.ForStmt, *goast.RangeStmt, *goast.SwitchStmt,
			*goast.TypeSwitchStmt, *goast.SelectStmt, *goast.CaseClause, *goast.CommClause:
			conditional = true
			return false
		}
		return true
	})
	return conditional
}

func appendUniqueFailureTarget(targets []failureTarget, candidate failureTarget) []failureTarget {
	if candidate.name == "" {
		return targets
	}
	for _, existing := range targets {
		if existing.name == candidate.name && existing.kind == candidate.kind {
			return targets
		}
	}
	return append(targets, candidate)
}

func cloneFailureTargets(targets []failureTarget) []failureTarget {
	result := make([]failureTarget, len(targets))
	for index, target := range targets {
		result[index] = target
		result[index].properties = cloneStringMap(target.properties)
	}
	return result
}

func conditionalFailureTargets(targets []failureTarget) []failureTarget {
	result := cloneFailureTargets(targets)
	for index := range result {
		if result[index].properties == nil {
			result[index].properties = map[string]string{}
		}
		result[index].properties["conditional"] = "true"
	}
	return result
}
