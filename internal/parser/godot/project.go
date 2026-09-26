package godot

import (
	"path/filepath"
	"strings"

	configast "github.com/cafecito-games/gdparser/configfile/ast"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

func extractConfig(input parserapi.Input, file *configast.File) graph.ParseResult {
	b := parserapi.NewBuilder(input, "godot-config")
	module := moduleName(input.Path)
	format := configFormat(input.Path)
	moduleID := b.Declare(b.FileID(), graph.Node{
		Kind: graph.KindModule, Name: graph.SimpleName(module), QualifiedName: module,
		Location: moduleLocation(input.Path), Properties: map[string]string{"format": format},
	})
	extractConfigAssignments(b, input, moduleID, format, "", file.Preamble)
	for _, section := range file.Sections {
		extractConfigAssignments(b, input, moduleID, format, section.Name, section.Statements)
	}
	return b.Finish()
}

func extractConfigAssignments(b *parserapi.Builder, input parserapi.Input, moduleID, format, section string, statements []configast.Statement) {
	for _, statement := range statements {
		assignment, ok := statement.(*configast.Assignment)
		if !ok {
			continue
		}
		key := assignment.Key
		if section != "" {
			key = section + "/" + key
		}
		loc := configLocation(input.Path, assignment)
		properties := map[string]string{"defined": "true", "format": format}
		if section != "" {
			properties["section"] = section
		}
		id := b.AddNode(graph.Node{
			Kind: graph.KindConfigKey, Name: key, QualifiedName: "config:" + input.Path + ":" + key,
			Location: loc, Properties: properties,
		})
		b.AddFact(moduleID, graph.EdgeDefines, id, "", "", loc, nil)
		configast.Inspect(assignment.Value, func(node configast.Node) bool {
			literal, ok := node.(*configast.StringLiteral)
			if !ok {
				return true
			}
			value := strings.TrimPrefix(strings.TrimSpace(literal.Value), "*")
			if strings.HasPrefix(value, "uid://") {
				b.AddFact(id, graph.EdgeReferences, "", value, graph.KindConfigKey,
					configLocation(input.Path, literal), map[string]string{"uid": value})
				return true
			}
			if !strings.HasPrefix(value, "res://") && !strings.HasPrefix(value, "user://") {
				return true
			}
			target := resourceModule(value)
			if target != "" {
				b.AddFact(id, graph.EdgeReferences, "", target, graph.KindModule,
					configLocation(input.Path, literal), map[string]string{"resource": literal.Value})
			}
			return true
		})
	}
}

func configFormat(path string) string {
	if strings.EqualFold(filepath.Base(path), "project.godot") {
		return "project.godot"
	}
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

func configLocation(path string, node configast.Node) graph.Location {
	span := node.Span()
	return graph.Location{Path: path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
}
