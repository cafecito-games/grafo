package godot

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	configast "github.com/cafecito-games/gdparser/configfile/ast"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

func extractConfig(input parserapi.Input, fileScope scope, file *configast.File) graph.ParseResult {
	b := parserapi.NewBuilder(input, "godot-config")
	module := moduleName(input.Path)
	format := configFormat(input.Path)
	moduleID := b.Declare(b.FileID(), graph.Node{
		Kind: graph.KindModule, Name: graph.SimpleName(module), QualifiedName: module,
		Location: moduleLocation(input.Path), Properties: map[string]string{"format": format},
	})
	keys := map[string]string{}
	extractConfigAssignments(b, input, fileScope, moduleID, format, "", file.Preamble, keys)
	for _, section := range file.Sections {
		extractConfigAssignments(b, input, fileScope, moduleID, format, section.Name, section.Statements, keys)
	}
	if format == godotid.ProjectFileName {
		extractAutoloads(b, input, moduleID, file, keys)
	}
	return b.Finish()
}

func extractConfigAssignments(b *parserapi.Builder, input parserapi.Input, fileScope scope, moduleID, format, section string, statements []configast.Statement, keys map[string]string) {
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
		keys[key] = id
		b.AddFact(moduleID, graph.EdgeDefines, id, "", "", loc, nil)
		configast.Inspect(assignment.Value, func(node configast.Node) bool {
			literal, ok := node.(*configast.StringLiteral)
			if !ok {
				return true
			}
			value := strings.TrimPrefix(strings.TrimSpace(literal.Value), "*")
			if godotid.IsUID(value) {
				b.AddFact(id, graph.EdgeReferences, "", godotid.UID(value), graph.KindConfigKey,
					configLocation(input.Path, literal), map[string]string{"uid": godotid.UID(value)})
				return true
			}
			if !strings.HasPrefix(value, "res://") && !strings.HasPrefix(value, "user://") {
				return true
			}
			if target := fileScope.resolve(value); target != "" {
				b.AddFact(id, graph.EdgeReferences, "", target, godotid.TargetKind(value),
					configLocation(input.Path, literal), map[string]string{"resource": literal.Value})
			}
			return true
		})
	}
}

// extractAutoloads promotes the tracked project.godot [autoload] section into
// first-class autoload singletons. The generic configuration key stays exactly
// where it was so config catalogs keep working; the autoload node is the
// queryable runtime declaration layered on top of it.
//
// Declarations that cannot be resolved without guessing produce a diagnostic
// and no autoload node, so a conflicting or malformed entry resolves nothing on
// the script side: see godotid.ParseProject, which owns that decision for every
// Godot producer.
func extractAutoloads(b *parserapi.Builder, input parserapi.Input, moduleID string, file *configast.File, keys map[string]string) {
	project := godotid.ProjectFromFile(input.Path, file)
	for _, name := range project.Conflicts {
		b.Diagnostic(project.Lines[name], "warning", fmt.Sprintf(
			"autoload %q is declared more than once; keeping its uses unresolved", name))
	}
	for _, name := range project.Malformed {
		b.Diagnostic(project.Lines[name], "warning", fmt.Sprintf(
			"autoload %q has no resource path; keeping the configuration key only", name))
	}
	for _, name := range sortedAutoloadNames(project.Autoloads) {
		declaration := project.Autoloads[name]
		properties := map[string]string{
			"form": "autoload", "resource": declaration.Reference,
			"singleton": fmt.Sprint(declaration.Enabled), "enabled": fmt.Sprint(declaration.Enabled),
			"config_key": godotid.AutoloadSection + "/" + name,
		}
		if declaration.UID != "" {
			properties["uid"] = declaration.UID
		}
		loc := graph.Location{Path: input.Path, Line: declaration.Line, Column: 1, EndLine: declaration.Line}
		id := b.Declare(moduleID, graph.Node{
			Kind: graph.KindGodotAutoload, Name: name,
			QualifiedName: project.AutoloadQualifiedName(name), Location: loc, Properties: properties,
		})
		if configKeyID := keys[godotid.AutoloadSection+"/"+name]; configKeyID != "" {
			b.AddFact(configKeyID, graph.EdgeDefines, id, "", graph.KindGodotAutoload, loc, nil)
		}
		if declaration.Target == "" {
			// A UID-only autoload keeps the alias as its evidence; the alias
			// declaration resolves the resource in a second hop.
			b.AddFact(id, graph.EdgeReferences, "", declaration.UID, graph.KindConfigKey, loc,
				map[string]string{"uid": declaration.UID})
			continue
		}
		b.AddFact(id, graph.EdgeAutoloads, "", declaration.Target, godotid.TargetKind(declaration.Reference),
			loc, map[string]string{"autoload": name, "resource": declaration.Reference,
				"enabled": fmt.Sprint(declaration.Enabled)})
	}
}

// sortedAutoloadNames keeps autoload emission deterministic.
func sortedAutoloadNames(declarations map[string]godotid.Autoload) []string {
	names := make([]string, 0, len(declarations))
	for name := range declarations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func configFormat(path string) string {
	if strings.EqualFold(filepath.Base(path), godotid.ProjectFileName) {
		return godotid.ProjectFileName
	}
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

func configLocation(path string, node configast.Node) graph.Location {
	span := node.Span()
	return graph.Location{Path: path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
}
