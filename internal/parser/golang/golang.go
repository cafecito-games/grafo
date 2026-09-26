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
		goast.Inspect(decl.Body, func(n goast.Node) bool {
			call, ok := n.(*goast.CallExpr)
			if !ok {
				return true
			}
			parseCall(b, fset, input, imports, functionID, receiverVariable, receiverQualified, call)
			return true
		})
	}
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

func parseCall(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, imports map[string]string, fromID, receiverVariable, receiverQualified string, call *goast.CallExpr) {
	callee := render(fset, call.Fun)
	loc := location(input.Path, fset, call.Pos(), call.End())
	if callee == "os.Getenv" || callee == "os.LookupEnv" {
		if key, ok := stringArgument(call.Args, 0); ok {
			b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
		}
		return
	}
	if callee == "http.HandleFunc" || callee == "http.Handle" {
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
	if selector, ok := call.Fun.(*goast.SelectorExpr); ok {
		if ident, ok := selector.X.(*goast.Ident); ok {
			if ident.Name == receiverVariable && receiverQualified != "" {
				callee = receiverQualified + "." + selector.Sel.Name
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
	b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
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
