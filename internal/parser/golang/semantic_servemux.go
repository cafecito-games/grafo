package golang

import (
	"fmt"
	goast "go/ast"
	"go/constant"
	"go/types"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	"golang.org/x/tools/go/packages"
)

const serveMuxType = "net/http.ServeMux"

type serveMuxPattern struct {
	raw      string
	method   string
	host     string
	route    string
	pathOnly bool
	catchAll bool
}

type serveMuxAnalyzer struct {
	root  string
	pkg   *packages.Package
	views map[string]SemanticView
}

func collectServeMuxPackageViews(root string, pkg *packages.Package, views map[string]SemanticView) {
	if pkg == nil || pkg.TypesInfo == nil || pkg.Fset == nil || pkg.Types == nil {
		return
	}
	analyzer := &serveMuxAnalyzer{root: root, pkg: pkg, views: views}
	for _, file := range pkg.Syntax {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*goast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			object, _ := pkg.TypesInfo.Defs[function.Name].(*types.Func)
			if object == nil {
				continue
			}
			position := pkg.Fset.Position(function.Pos())
			functionPath, ok := relativeSourcePath(root, position.Filename)
			if !ok {
				continue
			}
			functionKind := graph.KindFunction
			if signature, _ := object.Type().(*types.Signature); signature != nil && signature.Recv() != nil {
				functionKind = graph.KindMethod
			}
			goast.Inspect(function.Body, func(node goast.Node) bool {
				call, ok := node.(*goast.CallExpr)
				if !ok {
					return true
				}
				patternIndex, handlerIndex, ok := analyzer.registrationArguments(call)
				if !ok {
					return true
				}
				location := semanticLocation(functionPath, pkg.Fset, call.Pos(), call.End())
				if len(call.Args) <= handlerIndex {
					analyzer.diagnostic(location, "net/http ServeMux registration has insufficient arguments; endpoint omitted")
					return true
				}
				rawPattern, ok := analyzer.constantString(call.Args[patternIndex])
				if !ok {
					analyzer.diagnostic(location, "dynamic net/http ServeMux pattern; endpoint omitted")
					return true
				}
				pattern, err := parseServeMuxPattern(rawPattern)
				if err != nil {
					analyzer.diagnostic(location, "invalid net/http ServeMux pattern: "+err.Error())
					return true
				}
				handler, handlerKind, unresolved := analyzer.handlerEvidence(call.Args[handlerIndex])
				view := analyzer.views[functionPath]
				view.ServeMuxEndpoints = append(view.ServeMuxEndpoints, SemanticServeMuxEndpoint{
					Function: objectTarget(object), FunctionKind: functionKind,
					Method: pattern.method, Route: pattern.route, Host: pattern.host, Pattern: pattern.raw,
					Handler: handler, HandlerKind: handlerKind, Location: location, Unresolved: unresolved,
				})
				if view.ServeMuxEndpointCalls == nil {
					view.ServeMuxEndpointCalls = map[int]bool{}
				}
				view.ServeMuxEndpointCalls[pkg.Fset.Position(call.Lparen).Offset] = true
				analyzer.views[functionPath] = view
				return true
			})
		}
	}
	for sourcePath, view := range analyzer.views {
		sort.Slice(view.ServeMuxEndpoints, func(i, j int) bool {
			left, right := view.ServeMuxEndpoints[i], view.ServeMuxEndpoints[j]
			if left.Location.Line != right.Location.Line {
				return left.Location.Line < right.Location.Line
			}
			if left.Location.Column != right.Location.Column {
				return left.Location.Column < right.Location.Column
			}
			return left.Pattern < right.Pattern
		})
		analyzer.views[sourcePath] = view
	}
}

func (a *serveMuxAnalyzer) registrationArguments(call *goast.CallExpr) (int, int, bool) {
	selector, ok := call.Fun.(*goast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return 0, 0, false
	}
	if selection := a.pkg.TypesInfo.Selections[selector]; selection != nil {
		function, _ := selection.Obj().(*types.Func)
		if !isServeMuxRegistrationFunction(function) || semanticNamedType(a.pkg.TypesInfo.TypeOf(selector.X)) != serveMuxType {
			return 0, 0, false
		}
		if selection.Kind() == types.MethodExpr {
			return 1, 2, true
		}
		return 0, 1, true
	}
	function, _ := a.pkg.TypesInfo.Uses[selector.Sel].(*types.Func)
	if function == nil || function.Pkg() == nil || function.Pkg().Path() != "net/http" ||
		(function.Name() != "Handle" && function.Name() != "HandleFunc") {
		return 0, 0, false
	}
	signature, _ := function.Type().(*types.Signature)
	if signature == nil || signature.Recv() != nil {
		return 0, 0, false
	}
	return 0, 1, true
}

func isServeMuxRegistrationFunction(function *types.Func) bool {
	if function == nil || function.Pkg() == nil || function.Pkg().Path() != "net/http" ||
		(function.Name() != "Handle" && function.Name() != "HandleFunc") {
		return false
	}
	signature, _ := function.Type().(*types.Signature)
	return signature != nil && signature.Recv() != nil && semanticNamedType(signature.Recv().Type()) == serveMuxType
}

func (a *serveMuxAnalyzer) constantString(expression goast.Expr) (string, bool) {
	value := a.pkg.TypesInfo.Types[expression].Value
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
}

func (a *serveMuxAnalyzer) handlerEvidence(expression goast.Expr) (string, graph.NodeKind, bool) {
	for {
		switch value := expression.(type) {
		case *goast.ParenExpr:
			expression = value.X
			continue
		case *goast.CallExpr:
			if len(value.Args) == 1 && semanticNamedType(a.pkg.TypesInfo.TypeOf(value)) == "net/http.HandlerFunc" {
				expression = value.Args[0]
				continue
			}
		}
		break
	}
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
	target := strings.TrimSpace(renderSemanticExpression(a.pkg.Fset, expression))
	if target == "" {
		target = "unresolved-handler"
	}
	return target, graph.KindExternal, true
}

func (a *serveMuxAnalyzer) diagnostic(location graph.Location, message string) {
	view := a.views[location.Path]
	view.Diagnostics = append(view.Diagnostics, graph.Diagnostic{
		Path: location.Path, Line: location.Line, Level: "warning", Message: message,
	})
	a.views[location.Path] = view
}

func parseServeMuxPattern(value string) (serveMuxPattern, error) {
	if value == "" {
		return serveMuxPattern{}, fmt.Errorf("empty pattern")
	}
	method, rest, found := value, "", false
	if index := strings.IndexAny(value, " \t"); index >= 0 {
		method, rest, found = value[:index], strings.TrimLeft(value[index+1:], " \t"), true
	}
	if !found {
		rest, method = method, ""
	}
	normalizedMethod := "ANY"
	if method != "" {
		var err error
		normalizedMethod, err = httpmodel.PreserveMethod(method)
		if err != nil {
			return serveMuxPattern{}, err
		}
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return serveMuxPattern{}, fmt.Errorf("host/path missing /")
	}
	host, route := rest[:slash], rest[slash:]
	if strings.Contains(host, "{") {
		return serveMuxPattern{}, fmt.Errorf("host contains '{'")
	}
	if method != "" && normalizedMethod != "CONNECT" && cleanServeMuxPath(route) != route {
		return serveMuxPattern{}, fmt.Errorf("non-CONNECT pattern with unclean path")
	}
	segments := strings.Split(strings.TrimPrefix(route, "/"), "/")
	canonical := make([]string, 0, len(segments))
	seen := map[string]bool{}
	catchAll := strings.HasSuffix(route, "/") && route != "/"
	for index, segment := range segments {
		if segment == "" {
			if index < len(segments)-1 || route == "/" {
				canonical = append(canonical, "")
			}
			continue
		}
		open := strings.IndexByte(segment, '{')
		if open < 0 {
			literal, err := url.PathUnescape(segment)
			if err != nil {
				literal = segment
			}
			canonical = append(canonical, encodeServeMuxLiteral(literal))
			continue
		}
		if open != 0 || !strings.HasSuffix(segment, "}") {
			return serveMuxPattern{}, fmt.Errorf("bad wildcard segment %q", segment)
		}
		name := segment[1 : len(segment)-1]
		if name == "$" {
			if index != len(segments)-1 {
				return serveMuxPattern{}, fmt.Errorf("{$} not at end")
			}
			canonical = append(canonical, segment)
			continue
		}
		multi := strings.HasSuffix(name, "...")
		name = strings.TrimSuffix(name, "...")
		if multi && index != len(segments)-1 {
			return serveMuxPattern{}, fmt.Errorf("{%s...} wildcard not at end", name)
		}
		if !validServeMuxWildcardName(name) {
			return serveMuxPattern{}, fmt.Errorf("bad wildcard name %q", name)
		}
		if seen[name] {
			return serveMuxPattern{}, fmt.Errorf("duplicate wildcard name %q", name)
		}
		seen[name] = true
		catchAll = catchAll || multi
		canonical = append(canonical, segment)
	}
	route = "/" + strings.Join(canonical, "/")
	if catchAll && strings.HasSuffix(value, "/") {
		route = strings.TrimSuffix(route, "/") + "/{_...}"
	}
	if _, err := httpmodel.ParseRoute(route); err != nil {
		return serveMuxPattern{}, err
	}
	return serveMuxPattern{raw: value, method: normalizedMethod, host: host, route: route,
		pathOnly: method == "", catchAll: catchAll}, nil
}

func encodeServeMuxLiteral(value string) string {
	// Shared request parsing deliberately rejects dot segments. A method-less
	// ServeMux pattern accepts them, so preserve the declaration as inert,
	// round-trippable evidence without making it look like a matchable request.
	if value == "." || value == ".." {
		encoded := strings.Repeat("%2E", len(value))
		return url.PathEscape(encoded)
	}
	return url.PathEscape(value)
}

func cleanServeMuxPath(value string) string {
	if value == "" {
		return "/"
	}
	cleaned := path.Clean(value)
	if strings.HasSuffix(value, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

func validServeMuxWildcardName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if !unicode.IsLetter(character) && character != '_' && (index == 0 || !unicode.IsDigit(character)) {
			return false
		}
	}
	return true
}
