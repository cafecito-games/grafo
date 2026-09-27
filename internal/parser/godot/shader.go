package godot

import (
	"fmt"
	pathpkg "path"
	"strings"

	shaderast "github.com/cafecito-games/gdparser/shader/ast"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

type shaderExtractor struct {
	b         *parserapi.Builder
	input     parserapi.Input
	scope     scope
	module    string
	moduleID  string
	globals   map[string]string
	functions map[string]string
}

func extractShader(input parserapi.Input, fileScope scope, file *shaderast.File) graph.ParseResult {
	b := parserapi.NewBuilder(input, "godot-shader")
	e := &shaderExtractor{b: b, input: input, scope: fileScope, module: godotid.Identity(input.Path),
		globals: map[string]string{}, functions: map[string]string{}}
	properties, location := shaderProperties(input.Path, file)
	e.moduleID = b.Declare(b.FileID(), graph.Node{
		Kind: graph.KindModule, Name: graph.SimpleName(e.module), QualifiedName: e.module,
		Location: location, Properties: properties,
	})
	e.prepare(file)
	e.extract(file)
	return b.Finish()
}

func shaderProperties(path string, file *shaderast.File) (map[string]string, graph.Location) {
	properties := map[string]string{"format": strings.TrimPrefix(strings.ToLower(pathExtension(path)), ".")}
	location := moduleLocation(path)
	var renderModes []string
	for _, item := range file.Items {
		switch current := item.(type) {
		case *shaderast.ShaderType:
			properties["shader_type"] = current.Name
			location = shaderLocation(path, current)
		case *shaderast.RenderMode:
			for _, mode := range current.Modes {
				if name := shaderExpressionName(mode); name != "" {
					renderModes = append(renderModes, name)
				}
			}
		}
	}
	if len(renderModes) > 0 {
		properties["render_modes"] = strings.Join(renderModes, ",")
	}
	return properties, location
}

func (e *shaderExtractor) prepare(file *shaderast.File) {
	group := ""
	for _, item := range file.Items {
		switch current := item.(type) {
		case *shaderast.GroupUniforms:
			group = current.Name
			if current.Subgroup != "" {
				group = strings.Trim(group+"."+current.Subgroup, ".")
			}
		case *shaderast.VariableDeclaration:
			e.declareShaderFields(e.moduleID, e.module, current, group)
		case *shaderast.StructDeclaration:
			e.declareShaderStruct(current)
		case *shaderast.FunctionDeclaration:
			qualified := qualify(e.module, current.Name)
			id := e.b.Declare(e.moduleID, graph.Node{
				Kind: graph.KindFunction, Name: current.Name, QualifiedName: qualified,
				Location: shaderLocation(e.input.Path, current), Properties: map[string]string{
					"parameters": shaderParameters(current.Parameters), "returns": current.ReturnType + shaderArrays(current.ReturnArrays),
				},
			})
			e.functions[current.Name] = id
		}
	}
}

func (e *shaderExtractor) declareShaderFields(ownerID, container string, declaration *shaderast.VariableDeclaration, group string) {
	for _, declarator := range declaration.Declarators {
		properties := shaderVariableProperties(declaration, declarator)
		if group != "" {
			properties["group"] = group
		}
		qualified := qualify(container, declarator.Name)
		id := e.b.AddNode(graph.Node{
			Kind: graph.KindField, Name: declarator.Name, QualifiedName: qualified,
			Location: shaderLocation(e.input.Path, declarator), Properties: properties,
		})
		e.b.AddFact(ownerID, graph.EdgeHasField, id, "", graph.KindField, shaderLocation(e.input.Path, declarator), nil)
		if ownerID == e.moduleID {
			e.globals[declarator.Name] = id
		}
	}
}

func (e *shaderExtractor) declareShaderStruct(declaration *shaderast.StructDeclaration) {
	qualified := qualify(e.module, declaration.Name)
	id := e.b.Declare(e.moduleID, graph.Node{
		Kind: graph.KindType, Name: declaration.Name, QualifiedName: qualified,
		Location: shaderLocation(e.input.Path, declaration), Properties: map[string]string{"form": "struct"},
	})
	for _, member := range declaration.Members {
		if field, ok := member.(*shaderast.VariableDeclaration); ok {
			e.declareShaderFields(id, qualified, field, "")
		}
	}
}

func (e *shaderExtractor) extract(file *shaderast.File) {
	for _, item := range file.Items {
		switch current := item.(type) {
		case *shaderast.PreprocessorDirective:
			e.extractShaderDirective(current)
		case *shaderast.FunctionDeclaration:
			e.extractShaderFunction(current)
		case *shaderast.VariableDeclaration:
			for _, declarator := range current.Declarators {
				e.extractShaderExpression(e.globals[declarator.Name], declarator.Value, nil)
			}
		}
	}
}

func (e *shaderExtractor) extractShaderDirective(directive *shaderast.PreprocessorDirective) {
	if directive.Name != "include" {
		return
	}
	value := strings.TrimSpace(directive.Body)
	value = parserapi.Unquote(value)
	if value == "" {
		return
	}
	target := value
	if strings.HasPrefix(target, "res://") || strings.HasPrefix(target, "user://") {
		target = e.scope.resolve(target)
	} else {
		target = godotid.Identity(pathpkg.Clean(pathpkg.Join(pathpkg.Dir(e.input.Path), target)))
	}
	if target == "" {
		reason := "cannot be resolved"
		if e.scope.escapes(value) {
			reason = "leaves its Godot project"
		}
		e.b.Diagnostic(shaderLocation(e.input.Path, directive).Line, "warning", fmt.Sprintf(
			"#include %q %s; keeping it unresolved", value, reason))
		return
	}
	e.b.AddFact(e.moduleID, graph.EdgeImports, "", target, graph.KindModule,
		shaderLocation(e.input.Path, directive), map[string]string{"include": value})
}

func (e *shaderExtractor) extractShaderFunction(function *shaderast.FunctionDeclaration) {
	functionID := e.functions[function.Name]
	qualified := qualify(e.module, function.Name)
	symbols := map[string]string{}
	for _, parameter := range function.Parameters {
		properties := map[string]string{"type": parameter.Type + shaderArrays(parameter.Arrays)}
		if qualifiers := shaderQualifiers(parameter.Qualifiers); qualifiers != "" {
			properties["qualifiers"] = qualifiers
		}
		id := e.b.Declare(functionID, graph.Node{
			Kind: graph.KindParameter, Name: parameter.Name, QualifiedName: qualify(qualified, parameter.Name),
			Location: shaderLocation(e.input.Path, parameter), Properties: properties,
		})
		symbols[parameter.Name] = id
	}
	shaderast.Inspect(function.Body, func(node shaderast.Node) bool {
		declaration, ok := node.(*shaderast.VariableDeclaration)
		if !ok {
			return true
		}
		for _, declarator := range declaration.Declarators {
			loc := shaderLocation(e.input.Path, declarator)
			id := e.b.Declare(functionID, graph.Node{
				Kind: graph.KindVariable, Name: declarator.Name,
				QualifiedName: fmt.Sprintf("%s.%s@%d", qualified, declarator.Name, loc.Line),
				Location:      loc, Properties: shaderVariableProperties(declaration, declarator),
			})
			symbols[declarator.Name] = id
		}
		return true
	})
	shaderast.Inspect(function.Body, func(node shaderast.Node) bool {
		switch current := node.(type) {
		case *shaderast.Identifier:
			if targetID := e.globals[current.Name]; targetID != "" {
				e.b.AddFact(functionID, graph.EdgeReferences, targetID, "", graph.KindField,
					shaderLocation(e.input.Path, current), nil)
			}
		case *shaderast.CallExpression:
			callee := shaderExpressionName(current.Callee)
			if callee == "" {
				break
			}
			if targetID := e.functions[callee]; targetID != "" {
				e.b.AddFact(functionID, graph.EdgeCalls, targetID, "", graph.KindFunction,
					shaderLocation(e.input.Path, current), nil)
			} else {
				e.b.AddFact(functionID, graph.EdgeCalls, "", callee, "",
					shaderLocation(e.input.Path, current), nil)
			}
		case *shaderast.AssignmentExpression:
			targetID := shaderSymbolID(current.Target, symbols, e.globals)
			if targetID != "" {
				for _, sourceID := range shaderReferencedSymbols(current.Value, symbols, e.globals) {
					e.b.AddFact(sourceID, graph.EdgeAssigns, targetID, "", "",
						shaderLocation(e.input.Path, current), nil)
				}
			}
		case *shaderast.ReturnStatement:
			for _, sourceID := range shaderReferencedSymbols(current.Value, symbols, e.globals) {
				e.b.AddFact(sourceID, graph.EdgeReturns, functionID, "", graph.KindFunction,
					shaderLocation(e.input.Path, current), nil)
			}
		}
		return true
	})
}

func (e *shaderExtractor) extractShaderExpression(fromID string, expression shaderast.Expression, symbols map[string]string) {
	if fromID == "" || expression == nil {
		return
	}
	shaderast.Inspect(expression, func(node shaderast.Node) bool {
		call, ok := node.(*shaderast.CallExpression)
		if !ok {
			return true
		}
		callee := shaderExpressionName(call.Callee)
		if targetID := e.functions[callee]; targetID != "" {
			e.b.AddFact(fromID, graph.EdgeCalls, targetID, "", graph.KindFunction, shaderLocation(e.input.Path, call), nil)
		} else if callee != "" {
			e.b.AddFact(fromID, graph.EdgeCalls, "", callee, "", shaderLocation(e.input.Path, call), nil)
		}
		return true
	})
}

func shaderVariableProperties(declaration *shaderast.VariableDeclaration, declarator *shaderast.Declarator) map[string]string {
	properties := map[string]string{"type": declaration.Type + shaderArrays(declarator.Arrays)}
	if qualifiers := shaderQualifiers(declaration.Qualifiers); qualifiers != "" {
		properties["qualifiers"] = qualifiers
	}
	if len(declaration.Hints) > 0 {
		hints := make([]string, 0, len(declaration.Hints))
		for _, hint := range declaration.Hints {
			hints = append(hints, hint.Name)
		}
		properties["hints"] = strings.Join(hints, ",")
	}
	return properties
}

func shaderQualifiers(qualifiers shaderast.Qualifiers) string {
	values := append([]string(nil), qualifiers.Storage...)
	if qualifiers.Interpolation != "" {
		values = append(values, qualifiers.Interpolation)
	}
	if qualifiers.Precision != "" {
		values = append(values, qualifiers.Precision)
	}
	if qualifiers.Invariant {
		values = append(values, "invariant")
	}
	if qualifiers.Const {
		values = append(values, "const")
	}
	return strings.Join(values, ",")
}

func shaderArrays(arrays []*shaderast.ArraySpecifier) string {
	var result strings.Builder
	for _, array := range arrays {
		result.WriteByte('[')
		result.WriteString(shaderExpressionName(array.Size))
		result.WriteByte(']')
	}
	return result.String()
}

func shaderParameters(parameters []*shaderast.Parameter) string {
	values := make([]string, 0, len(parameters))
	for _, parameter := range parameters {
		values = append(values, parameter.Name+": "+parameter.Type+shaderArrays(parameter.Arrays))
	}
	return "(" + strings.Join(values, ", ") + ")"
}

func shaderExpressionName(expression shaderast.Expression) string {
	switch current := expression.(type) {
	case nil:
		return ""
	case *shaderast.Identifier:
		return current.Name
	case *shaderast.Literal:
		return strings.Trim(current.Raw, "\"'")
	case *shaderast.MemberExpression:
		object := shaderExpressionName(current.Object)
		if object == "" {
			return current.Property
		}
		return object + "." + current.Property
	default:
		return ""
	}
}

func shaderSymbolID(expression shaderast.Expression, symbols, globals map[string]string) string {
	name := shaderExpressionName(expression)
	name = strings.SplitN(name, ".", 2)[0]
	if symbols[name] != "" {
		return symbols[name]
	}
	return globals[name]
}

func shaderReferencedSymbols(expression shaderast.Expression, symbols, globals map[string]string) []string {
	seen := map[string]bool{}
	var result []string
	if expression == nil {
		return result
	}
	shaderast.Inspect(expression, func(node shaderast.Node) bool {
		identifier, ok := node.(*shaderast.Identifier)
		if !ok {
			return true
		}
		id := symbols[identifier.Name]
		if id == "" {
			id = globals[identifier.Name]
		}
		if id != "" && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
		return true
	})
	return result
}

func shaderLocation(path string, node shaderast.Node) graph.Location {
	span := node.Span()
	return graph.Location{Path: path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
}
