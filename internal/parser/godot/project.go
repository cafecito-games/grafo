package godot

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
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
		// One parse of the project vocabulary feeds every declaration kind, so
		// godotid stays the single authority on which entries are exact.
		project := godotid.ProjectFromFile(input.Path, file)
		extractAutoloads(b, input, moduleID, project, keys)
		extractInputActions(b, input, moduleID, project, keys)
		extractNodeGroups(b, input, moduleID, project, keys)
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
			target := fileScope.resolve(value)
			if target == "" {
				if fileScope.escapes(value) {
					b.Diagnostic(configLocation(input.Path, literal).Line, "warning", fmt.Sprintf(
						"configuration key %q points at %s, which leaves its Godot project; "+
							"keeping the reference unresolved", key, value))
				}
				return true
			}
			b.AddFact(id, graph.EdgeReferences, "", target, godotid.TargetKind(value),
				configLocation(input.Path, literal), map[string]string{"resource": literal.Value})
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
func extractAutoloads(b *parserapi.Builder, input parserapi.Input, moduleID string, project godotid.Project, keys map[string]string) {
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

// extractInputActions promotes the tracked project.godot [input] section into
// first-class input actions, the declarations that Input and InputMap calls read
// back by name. The generic configuration key stays where it was so config
// catalogs keep working; the action node is the queryable declaration layered on
// top of it.
//
// Only non-secret declaration metadata reaches the graph: the deadzone as
// written and how many input events the declaration lists. Device bindings are
// deliberately not modelled, so no keycode or joypad index is ever recorded.
//
// A name declared more than once, or one whose value is not an action
// dictionary, produces a diagnostic and no action node. Uses of that name then
// resolve to an unresolved boundary node rather than to a declaration nothing
// could read: godotid.ProjectFromFile owns that verdict for every producer.
func extractInputActions(b *parserapi.Builder, input parserapi.Input, moduleID string, project godotid.Project, keys map[string]string) {
	for _, name := range project.Inputs.Conflicts {
		b.Diagnostic(project.Inputs.Lines[name], "warning", fmt.Sprintf(
			"input action %q is declared more than once; keeping its uses unresolved", name))
	}
	for _, name := range project.Inputs.Malformed {
		b.Diagnostic(project.Inputs.Lines[name], "warning", fmt.Sprintf(
			"input action %q is not an action dictionary; keeping the configuration key only", name))
	}
	for _, name := range project.Inputs.Names() {
		action := project.Inputs.Declarations[name]
		configKey := godotid.InputSection + "/" + name
		properties := map[string]string{
			"form": "input_action", "events": strconv.Itoa(action.Events), "config_key": configKey,
		}
		if action.Deadzone != "" {
			properties["deadzone"] = action.Deadzone
		}
		loc := graph.Location{Path: input.Path, Line: action.Line, Column: 1, EndLine: action.Line}
		id := b.Declare(moduleID, graph.Node{
			Kind: graph.KindGodotInputAction, Name: name,
			QualifiedName: project.InputActionQualifiedName(name), Location: loc, Properties: properties,
		})
		if configKeyID := keys[configKey]; configKeyID != "" {
			b.AddFact(configKeyID, graph.EdgeDefines, id, "", graph.KindGodotInputAction, loc, nil)
		}
	}
}

// extractNodeGroups promotes the tracked project.godot [global_group] section
// into first-class node groups.
//
// Godot records a group here only when the project declares it globally, so a
// group that a scene or a script merely uses is a reference and never a
// declaration: it stays an unresolved boundary node until the project declares
// it, which is what keeps missing wiring visible instead of letting the first
// use silently define the vocabulary.
func extractNodeGroups(b *parserapi.Builder, input parserapi.Input, moduleID string, project godotid.Project, keys map[string]string) {
	for _, name := range project.Groups.Conflicts {
		b.Diagnostic(project.Groups.Lines[name], "warning", fmt.Sprintf(
			"node group %q is declared more than once; keeping its uses unresolved", name))
	}
	for _, name := range project.Groups.Malformed {
		b.Diagnostic(project.Groups.Lines[name], "warning", fmt.Sprintf(
			"node group %q has no description string; keeping the configuration key only", name))
	}
	for _, name := range project.Groups.Names() {
		group := project.Groups.Declarations[name]
		configKey := godotid.GlobalGroupSection + "/" + name
		properties := map[string]string{"form": "node_group", "global": "true", "config_key": configKey}
		if group.Description != "" {
			properties["description"] = group.Description
		}
		loc := graph.Location{Path: input.Path, Line: group.Line, Column: 1, EndLine: group.Line}
		id := b.Declare(moduleID, graph.Node{
			Kind: graph.KindGodotNodeGroup, Name: name,
			QualifiedName: project.NodeGroupQualifiedName(name), Location: loc, Properties: properties,
		})
		if configKeyID := keys[configKey]; configKeyID != "" {
			b.AddFact(configKeyID, graph.EdgeDefines, id, "", graph.KindGodotNodeGroup, loc, nil)
		}
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
