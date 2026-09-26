package golang

import (
	"bytes"
	"context"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

type Parser struct{}

func New() *Parser { return &Parser{} }

func (*Parser) Language() string { return "go" }
func (*Parser) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

func (*Parser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, input.Path, input.Content, parser.ParseComments|parser.SkipObjectResolution)
	if file == nil {
		return b.Finish(), err
	}
	if err != nil {
		b.Diagnostic(0, "warning", err.Error())
	}
	packageName := packageQualified(input, file.Name.Name)
	imports := map[string]string{}
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		alias := filepath.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		imports[alias] = importPath
		loc := location(input.Path, fset, spec.Pos(), spec.End())
		b.AddFact(b.FileID(), graph.EdgeImports, "", importPath, graph.KindModule, loc,
			map[string]string{"alias": alias})
	}
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *goast.FuncDecl:
			parseFunction(b, fset, input, packageName, imports, node)
		case *goast.GenDecl:
			if node.Tok == token.TYPE {
				for _, spec := range node.Specs {
					if ts, ok := spec.(*goast.TypeSpec); ok {
						parseType(b, fset, input, packageName, ts)
					}
				}
			}
		}
	}
	return b.Finish(), nil
}

func packageQualified(input parserapi.Input, packageName string) string {
	dir := filepath.ToSlash(filepath.Dir(input.Path))
	if input.GoModule != "" {
		if dir == "." {
			return input.GoModule
		}
		return strings.TrimSuffix(input.GoModule, "/") + "/" + dir
	}
	if dir == "." {
		return packageName
	}
	return dir
}

func parseFunction(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, decl *goast.FuncDecl) {
	kind := graph.KindFunction
	qualified := pkg + "." + decl.Name.Name
	owner := ""
	receiverVariable := ""
	receiverQualified := ""
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = graph.KindMethod
		owner = receiverName(decl.Recv.List[0].Type)
		qualified = pkg + "." + owner + "." + decl.Name.Name
		receiverQualified = pkg + "." + owner
		if len(decl.Recv.List[0].Names) > 0 {
			receiverVariable = decl.Recv.List[0].Names[0].Name
		}
	}
	loc := location(input.Path, fset, decl.Pos(), decl.End())
	node := graph.Node{Kind: kind, Name: decl.Name.Name, QualifiedName: qualified,
		Location: loc, Properties: map[string]string{"signature": render(fset, decl.Type)}}
	if owner != "" {
		node.Properties["receiver"] = owner
	}
	functionID := b.Declare(b.FileID(), node)
	if decl.Body != nil {
		flow := collectFunctionBindings(b, fset, input, pkg, imports, qualified, functionID, decl)
		if receiverVariable != "" {
			flow.types[receiverVariable] = receiverQualified
		}
		goast.Inspect(decl.Body, func(n goast.Node) bool {
			call, ok := n.(*goast.CallExpr)
			if !ok {
				return true
			}
			parseCall(b, fset, input, pkg, imports, functionID, receiverVariable, receiverQualified, flow, call)
			return true
		})
		emitDataFlow(b, fset, input, pkg, imports, functionID, receiverVariable, receiverQualified, flow, decl.Body)
	}
}

type functionBindings struct {
	symbols map[string]string
	types   map[string]string
}

func collectFunctionBindings(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, functionName, functionID string, decl *goast.FuncDecl) functionBindings {
	bindings := functionBindings{symbols: map[string]string{}, types: map[string]string{}}
	declare := func(name string, kind graph.NodeKind, typeText string, loc graph.Location) {
		if name == "" || name == "_" {
			return
		}
		qualified := functionName + "." + name
		if kind == graph.KindVariable {
			qualified += fmt.Sprintf("@%d", loc.Line)
		}
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		id := b.Declare(functionID, graph.Node{Kind: kind, Name: name, QualifiedName: qualified,
			Location: loc, Properties: properties})
		bindings.symbols[name] = id
		if typeText != "" {
			bindings.types[name] = qualifyGoType(typeText, pkg, imports)
		}
	}
	if decl.Type.Params != nil {
		for _, field := range decl.Type.Params.List {
			typeText := render(fset, field.Type)
			for _, name := range field.Names {
				declare(name.Name, graph.KindParameter, typeText, location(input.Path, fset, name.Pos(), name.End()))
			}
		}
	}
	goast.Inspect(decl.Body, func(n goast.Node) bool {
		switch value := n.(type) {
		case *goast.AssignStmt:
			if value.Tok != token.DEFINE {
				return true
			}
			for index, lhs := range value.Lhs {
				ident, ok := lhs.(*goast.Ident)
				if !ok {
					continue
				}
				typeText := ""
				if index < len(value.Rhs) {
					typeText = inferGoExprType(value.Rhs[index], fset, pkg, imports, bindings.types)
				} else if len(value.Rhs) == 1 {
					typeText = inferGoExprType(value.Rhs[0], fset, pkg, imports, bindings.types)
				}
				declare(ident.Name, graph.KindVariable, typeText, location(input.Path, fset, ident.Pos(), ident.End()))
			}
		case *goast.DeclStmt:
			gen, ok := value.Decl.(*goast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				return true
			}
			for _, raw := range gen.Specs {
				spec, ok := raw.(*goast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range spec.Names {
					typeText := render(fset, spec.Type)
					if typeText == "" && index < len(spec.Values) {
						typeText = inferGoExprType(spec.Values[index], fset, pkg, imports, bindings.types)
					}
					declare(name.Name, graph.KindVariable, typeText, location(input.Path, fset, name.Pos(), name.End()))
				}
			}
		}
		return true
	})
	return bindings
}

func emitDataFlow(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, functionID, receiverVariable, receiverQualified string, bindings functionBindings, body *goast.BlockStmt) {
	goast.Inspect(body, func(n goast.Node) bool {
		switch value := n.(type) {
		case *goast.AssignStmt:
			for index, lhs := range value.Lhs {
				ident, ok := lhs.(*goast.Ident)
				if !ok {
					continue
				}
				targetID := bindings.symbols[ident.Name]
				if targetID == "" || len(value.Rhs) == 0 {
					continue
				}
				rhs := value.Rhs[min(index, len(value.Rhs)-1)]
				loc := location(input.Path, fset, value.Pos(), value.End())
				for _, sourceID := range referencedVariables(rhs, bindings.symbols) {
					b.AddFact(sourceID, graph.EdgeAssigns, targetID, "", graph.KindVariable, loc, nil)
				}
			}
		case *goast.ReturnStmt:
			loc := location(input.Path, fset, value.Pos(), value.End())
			for _, result := range value.Results {
				for _, sourceID := range referencedVariables(result, bindings.symbols) {
					b.AddFact(sourceID, graph.EdgeReturns, functionID, "", "", loc, nil)
				}
			}
		case *goast.CallExpr:
			callee := resolvedGoCallee(value, fset, pkg, imports, receiverVariable, receiverQualified, bindings.types)
			loc := location(input.Path, fset, value.Pos(), value.End())
			for position, argument := range value.Args {
				for _, sourceID := range referencedVariables(argument, bindings.symbols) {
					b.AddFact(sourceID, graph.EdgePasses, "", callee, "", loc,
						map[string]string{"argument": strconv.Itoa(position)})
				}
			}
		}
		return true
	})
}

func referencedVariables(expr goast.Expr, symbols map[string]string) []string {
	seen := map[string]bool{}
	var result []string
	goast.Inspect(expr, func(n goast.Node) bool {
		ident, ok := n.(*goast.Ident)
		if !ok {
			return true
		}
		if id := symbols[ident.Name]; id != "" && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
		return true
	})
	return result
}

func parseType(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, spec *goast.TypeSpec) {
	kind := graph.KindType
	if _, ok := spec.Type.(*goast.InterfaceType); ok {
		kind = graph.KindInterface
	}
	qualified := pkg + "." + spec.Name.Name
	loc := location(input.Path, fset, spec.Pos(), spec.End())
	typeID := b.Declare(b.FileID(), graph.Node{Kind: kind, Name: spec.Name.Name,
		QualifiedName: qualified, Location: loc, Properties: map[string]string{"underlying": render(fset, spec.Type)}})
	switch value := spec.Type.(type) {
	case *goast.StructType:
		parseFields(b, fset, input, qualified, typeID, value.Fields, false)
	case *goast.InterfaceType:
		parseFields(b, fset, input, qualified, typeID, value.Methods, true)
	}
}

func parseFields(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, parentName, parentID string, fields *goast.FieldList, methods bool) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		loc := location(input.Path, fset, field.Pos(), field.End())
		typeText := render(fset, field.Type)
		if len(field.Names) == 0 {
			edgeKind := graph.EdgeEmbeds
			if methods {
				edgeKind = graph.EdgeExtends
			}
			b.AddFact(parentID, edgeKind, "", typeText, "", loc, nil)
			continue
		}
		for _, name := range field.Names {
			kind := graph.KindField
			if methods {
				kind = graph.KindMethod
			}
			nodeID := b.AddNode(graph.Node{Kind: kind, Name: name.Name,
				QualifiedName: parentName + "." + name.Name, Location: loc,
				Properties: map[string]string{"type": typeText}})
			b.AddFact(parentID, graph.EdgeHasField, nodeID, "", "", loc, nil)
		}
	}
}

func parseCall(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, fromID, receiverVariable, receiverQualified string, bindings functionBindings, call *goast.CallExpr) {
	callee := resolvedGoCallee(call, fset, pkg, imports, receiverVariable, receiverQualified, bindings.types)
	loc := location(input.Path, fset, call.Pos(), call.End())
	if callee == "os.Getenv" || callee == "os.LookupEnv" {
		if key, ok := stringArgument(call.Args, 0); ok {
			b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
		}
		return
	}
	if callee == "http.HandleFunc" || callee == "http.Handle" || callee == "net/http.HandleFunc" || callee == "net/http.Handle" {
		if route, ok := stringArgument(call.Args, 0); ok {
			endpointID := addEndpoint(b, loc, "ANY", route)
			b.AddFact(fromID, graph.EdgeExposes, endpointID, "", "", loc, nil)
			if len(call.Args) > 1 {
				b.AddFact(endpointID, graph.EdgeHandledBy, "", render(fset, call.Args[1]), "", loc, nil)
			}
		}
		return
	}
	method := strings.ToLower(graph.SimpleName(callee))
	if isHTTPMethod(method) && len(call.Args) > 1 {
		if route, ok := stringArgument(call.Args, 0); ok {
			if strings.HasPrefix(route, "/") {
				endpointID := addEndpoint(b, loc, strings.ToUpper(method), route)
				b.AddFact(fromID, graph.EdgeExposes, endpointID, "", "", loc, nil)
				b.AddFact(endpointID, graph.EdgeHandledBy, "", render(fset, call.Args[len(call.Args)-1]), "", loc, nil)
				return
			}
		}
	}
	if event, ok := stringArgument(call.Args, 0); ok {
		switch method {
		case "publish", "publishevent", "emit", "produce":
			b.AddFact(fromID, graph.EdgePublishes, "", event, graph.KindEvent, loc, nil)
			return
		case "subscribe", "on", "consume":
			b.AddFact(fromID, graph.EdgeSubscribes, "", event, graph.KindEvent, loc, nil)
			return
		}
	}
	b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
}

func resolvedGoCallee(call *goast.CallExpr, fset *token.FileSet, pkg string, imports map[string]string, receiverVariable, receiverQualified string, types map[string]string) string {
	callee := render(fset, call.Fun)
	if selector, ok := call.Fun.(*goast.SelectorExpr); ok {
		if ident, ok := selector.X.(*goast.Ident); ok {
			if ident.Name == receiverVariable && receiverQualified != "" {
				callee = receiverQualified + "." + selector.Sel.Name
			} else if inferred := types[ident.Name]; inferred != "" {
				callee = inferred + "." + selector.Sel.Name
			} else if imported, exists := imports[ident.Name]; exists {
				callee = imported + "." + selector.Sel.Name
			}
		} else if constructor, ok := selector.X.(*goast.CallExpr); ok {
			// Resolve the common pkg.NewType(...).Method(...) form without a full
			// type checker. The constructor name is structural evidence for the
			// returned receiver; anything less specific stays unresolved.
			if constructorSelector, ok := constructor.Fun.(*goast.SelectorExpr); ok {
				if packageIdent, ok := constructorSelector.X.(*goast.Ident); ok {
					if imported, exists := imports[packageIdent.Name]; exists && strings.HasPrefix(constructorSelector.Sel.Name, "New") {
						typeName := strings.TrimPrefix(constructorSelector.Sel.Name, "New")
						if typeName != "" {
							callee = imported + "." + typeName + "." + selector.Sel.Name
						}
					}
				}
			}
		}
	}
	if ident, ok := call.Fun.(*goast.Ident); ok && !strings.Contains(ident.Name, ".") {
		callee = pkg + "." + ident.Name
	}
	return callee
}

func inferGoExprType(expr goast.Expr, fset *token.FileSet, pkg string, imports, types map[string]string) string {
	switch value := expr.(type) {
	case *goast.CompositeLit:
		return qualifyGoType(render(fset, value.Type), pkg, imports)
	case *goast.UnaryExpr:
		return inferGoExprType(value.X, fset, pkg, imports, types)
	case *goast.Ident:
		return types[value.Name]
	case *goast.CallExpr:
		if ident, ok := value.Fun.(*goast.Ident); ok {
			if ident.Name == "new" && len(value.Args) > 0 {
				return qualifyGoType(render(fset, value.Args[0]), pkg, imports)
			}
			if strings.HasPrefix(ident.Name, "New") && len(ident.Name) > 3 {
				return pkg + "." + strings.TrimPrefix(ident.Name, "New")
			}
		}
		if selector, ok := value.Fun.(*goast.SelectorExpr); ok {
			if packageIdent, ok := selector.X.(*goast.Ident); ok && strings.HasPrefix(selector.Sel.Name, "New") {
				if imported := imports[packageIdent.Name]; imported != "" {
					return imported + "." + strings.TrimPrefix(selector.Sel.Name, "New")
				}
			}
		}
	}
	return ""
}

func qualifyGoType(typeText, pkg string, imports map[string]string) string {
	typeText = strings.TrimSpace(typeText)
	typeText = strings.TrimLeft(typeText, "*[]")
	if index := strings.Index(typeText, "["); index >= 0 {
		typeText = typeText[:index]
	}
	if prefix, rest, ok := strings.Cut(typeText, "."); ok {
		if imported := imports[prefix]; imported != "" {
			return imported + "." + rest
		}
		return typeText
	}
	if typeText == "" || isBuiltinGoType(typeText) {
		return ""
	}
	return pkg + "." + typeText
}

func isBuiltinGoType(value string) bool {
	switch value {
	case "bool", "byte", "complex64", "complex128", "error", "float32", "float64", "int", "int8", "int16", "int32", "int64", "rune", "string", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "any":
		return true
	default:
		return false
	}
}

func addEndpoint(b *parserapi.Builder, loc graph.Location, method, route string) string {
	name := method + " " + route
	return b.AddNode(graph.Node{Kind: graph.KindEndpoint, Name: name,
		QualifiedName: fmt.Sprintf("endpoint:%s@%s:%d", name, loc.Path, loc.Line), Location: loc,
		Properties: map[string]string{"method": method, "route": route}})
}

func isHTTPMethod(method string) bool {
	switch method {
	case "get", "post", "put", "patch", "delete", "head", "options":
		return true
	default:
		return false
	}
}

func stringArgument(args []goast.Expr, index int) (string, bool) {
	if index >= len(args) {
		return "", false
	}
	lit, ok := args[index].(*goast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

func receiverName(expr goast.Expr) string {
	switch value := expr.(type) {
	case *goast.Ident:
		return value.Name
	case *goast.StarExpr:
		return receiverName(value.X)
	case *goast.IndexExpr:
		return receiverName(value.X)
	case *goast.IndexListExpr:
		return receiverName(value.X)
	default:
		return "receiver"
	}
}

func render(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

func location(path string, fset *token.FileSet, start, end token.Pos) graph.Location {
	s := fset.Position(start)
	e := fset.Position(end)
	return graph.Location{Path: path, Line: s.Line, Column: s.Column, EndLine: e.Line}
}
