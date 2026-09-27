package golang

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

// emitSyntaxFailureFlow is the fail-closed fallback used only when no semantic
// package view exists. A defer statement is structurally certain. panic and
// recover are emitted only when the function does not declare a same-named
// parameter or local that could shadow the predeclared builtin.
func emitSyntaxFailureFlow(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input,
	pkg string, imports map[string]string, functionID string, declaration *goast.FuncDecl,
	packageShadowed map[string]bool) {
	shadowed := syntaxShadowedBuiltins(declaration, packageShadowed)
	body := declaration.Body
	executableClosures := map[*goast.FuncLit]bool{}
	asyncClosures := map[*goast.FuncLit]bool{}
	goast.Inspect(body, func(node goast.Node) bool {
		switch value := node.(type) {
		case *goast.GoStmt:
			if closure := calledFunctionLiteral(value.Call); closure != nil {
				asyncClosures[closure] = true
			}
		case *goast.FuncLit:
			return executableClosures[value] && !asyncClosures[value]
		case *goast.DeferStmt:
			conditional := strconv.FormatBool(nodeWithinConditional(body, value.Pos()))
			target := ""
			targetKind := graph.NodeKind("")
			if _, closure := value.Call.Fun.(*goast.FuncLit); closure {
				loc := location(input.Path, fset, value.Pos(), value.End())
				target = "unresolved:defer@" + input.Path + ":" + strconv.Itoa(loc.Line) + ":" + strconv.Itoa(loc.Column)
				targetKind = graph.KindExternal
			} else {
				target, _, _ = resolvedGoCallee(value.Call, fset, pkg, imports, SemanticView{}, "", "", nil)
			}
			if target != "" {
				b.AddFact(functionID, graph.EdgeDefers, "", target, targetKind,
					location(input.Path, fset, value.Pos(), value.End()),
					map[string]string{"form": "defer", "evidence": "go/ast", "resolution": "syntax", "conditional": conditional})
			}
		case *goast.CallExpr:
			if closure := calledFunctionLiteral(value); closure != nil && !asyncClosures[closure] {
				executableClosures[closure] = true
			}
			identifier, ok := value.Fun.(*goast.Ident)
			if !ok || shadowed[identifier.Name] {
				return true
			}
			loc := location(input.Path, fset, value.Pos(), value.End())
			conditional := strconv.FormatBool(nodeWithinConditional(body, value.Pos()))
			switch identifier.Name {
			case "panic":
				target := "unresolved:panic@" + input.Path + ":" + strconv.Itoa(loc.Line) + ":" + strconv.Itoa(loc.Column)
				b.AddFact(functionID, graph.EdgePanics, "", target, graph.KindExternal, loc,
					map[string]string{"form": "panic", "evidence": "go/ast", "resolution": "syntax", "unresolved": "true", "conditional": conditional})
			case "recover":
				b.AddFact(functionID, graph.EdgeRecovers, "", "builtin.recover", "", loc,
					map[string]string{"form": "recover", "evidence": "go/ast", "resolution": "syntax", "conditional": conditional})
			}
		}
		return true
	})
}

func syntaxShadowedBuiltins(declaration *goast.FuncDecl, packageShadowed map[string]bool) map[string]bool {
	shadowed := map[string]bool{"panic": packageShadowed["panic"], "recover": packageShadowed["recover"]}
	inspectFields := func(fields *goast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, name := range field.Names {
				if name.Name == "panic" || name.Name == "recover" {
					shadowed[name.Name] = true
				}
			}
		}
	}
	inspectFields(declaration.Recv)
	inspectFields(declaration.Type.Params)
	inspectFields(declaration.Type.Results)
	goast.Inspect(declaration.Body, func(node goast.Node) bool {
		switch value := node.(type) {
		case *goast.FuncLit:
			inspectFields(value.Type.Params)
			inspectFields(value.Type.Results)
		case *goast.AssignStmt:
			if value.Tok != token.DEFINE {
				return true
			}
			for _, expression := range value.Lhs {
				if name, ok := expression.(*goast.Ident); ok && (name.Name == "panic" || name.Name == "recover") {
					shadowed[name.Name] = true
				}
			}
		case *goast.ValueSpec:
			for _, name := range value.Names {
				if name.Name == "panic" || name.Name == "recover" {
					shadowed[name.Name] = true
				}
			}
		}
		return true
	})
	return shadowed
}

func syntaxPackageShadowedBuiltins(input parserapi.Input, file *goast.File) map[string]bool {
	shadowed := syntaxFileShadowedBuiltins(file)
	if input.Root == "" || input.Path == "" {
		return shadowed
	}
	directory := filepath.Join(input.Root, filepath.Dir(filepath.FromSlash(input.Path)))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return shadowed
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || entry.Name() == filepath.Base(input.Path) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		sibling, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if parseErr != nil || sibling == nil || sibling.Name.Name != file.Name.Name {
			continue
		}
		for name := range syntaxFileShadowedBuiltins(sibling) {
			shadowed[name] = true
		}
	}
	return shadowed
}

func syntaxFileShadowedBuiltins(file *goast.File) map[string]bool {
	shadowed := map[string]bool{}
	for _, declaration := range file.Decls {
		switch value := declaration.(type) {
		case *goast.FuncDecl:
			if value.Name.Name == "panic" || value.Name.Name == "recover" {
				shadowed[value.Name.Name] = true
			}
		case *goast.GenDecl:
			for _, raw := range value.Specs {
				spec, ok := raw.(*goast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range spec.Names {
					if name.Name == "panic" || name.Name == "recover" {
						shadowed[name.Name] = true
					}
				}
			}
		}
	}
	return shadowed
}
