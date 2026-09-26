package godot

import (
	"strings"

	projectast "github.com/cafecito-games/gdparser/projectconfig/ast"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

func extractProject(input parserapi.Input, file *projectast.File) graph.ParseResult {
	b := parserapi.NewBuilder(input, "godot-project")
	module := moduleName(input.Path)
	moduleID := b.Declare(b.FileID(), graph.Node{
		Kind: graph.KindModule, Name: graph.SimpleName(module), QualifiedName: module,
		Location: moduleLocation(input.Path), Properties: map[string]string{"format": "project.godot"},
	})
	extractProjectAssignments(b, input, moduleID, "", file.Preamble)
	for _, section := range file.Sections {
		extractProjectAssignments(b, input, moduleID, section.Name, section.Statements)
	}
	return b.Finish()
}

func extractProjectAssignments(b *parserapi.Builder, input parserapi.Input, moduleID, section string, statements []projectast.Statement) {
	for _, statement := range statements {
		assignment, ok := statement.(*projectast.Assignment)
		if !ok {
			continue
		}
		key := assignment.Key
		if section != "" {
			key = section + "/" + key
		}
		loc := projectLocation(input.Path, assignment)
		properties := map[string]string{"defined": "true", "format": "project.godot"}
		if section != "" {
			properties["section"] = section
		}
		id := b.AddNode(graph.Node{
			Kind: graph.KindConfigKey, Name: key, QualifiedName: "config:" + input.Path + ":" + key,
			Location: loc, Properties: properties,
		})
		b.AddFact(moduleID, graph.EdgeDefines, id, "", "", loc, nil)
		projectast.Inspect(assignment.Value, func(node projectast.Node) bool {
			literal, ok := node.(*projectast.StringLiteral)
			if !ok {
				return true
			}
			value := strings.TrimPrefix(strings.TrimSpace(literal.Value), "*")
			if !strings.HasPrefix(value, "res://") && !strings.HasPrefix(value, "user://") {
				return true
			}
			target := resourceModule(value)
			if target != "" {
				b.AddFact(id, graph.EdgeReferences, "", target, graph.KindModule,
					projectLocation(input.Path, literal), map[string]string{"resource": literal.Value})
			}
			return true
		})
	}
}

func projectLocation(path string, node projectast.Node) graph.Location {
	span := node.Span()
	return graph.Location{Path: path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
}
