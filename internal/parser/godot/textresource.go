package godot

import (
	pathpkg "path"
	"strconv"
	"strings"

	"github.com/cafecito-games/gdparser/textresource"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

type resourceOwner struct {
	id        string
	qualified string
	scenePath string
}

type textResourceExtractor struct {
	b             *parserapi.Builder
	input         parserapi.Input
	module        string
	moduleID      string
	extResources  map[string]string
	subResources  map[string]string
	nodes         map[string]resourceOwner
	sectionOwners map[*textresource.Section]resourceOwner
	events        map[string]string
	root          string
}

func extractTextResource(input parserapi.Input, document *textresource.Document) graph.ParseResult {
	b := parserapi.NewBuilder(input, "godot-resource")
	e := &textResourceExtractor{
		b: b, input: input, module: moduleName(input.Path), extResources: map[string]string{},
		subResources: map[string]string{}, nodes: map[string]resourceOwner{},
		sectionOwners: map[*textresource.Section]resourceOwner{}, events: map[string]string{},
	}
	properties, location := e.documentProperties(document)
	e.moduleID = b.Declare(b.FileID(), graph.Node{
		Kind: graph.KindModule, Name: graph.SimpleName(e.module), QualifiedName: e.module,
		Location: location, Properties: properties,
	})
	e.prepare(document)
	e.extract(document)
	return b.Finish()
}

func (e *textResourceExtractor) documentProperties(document *textresource.Document) (map[string]string, graph.Location) {
	properties := map[string]string{"format": strings.TrimPrefix(strings.ToLower(pathExtension(e.input.Path)), ".")}
	location := moduleLocation(e.input.Path)
	for _, item := range document.Items {
		section, ok := item.(*textresource.Section)
		if !ok || (section.Type != "gd_scene" && section.Type != "gd_resource") {
			continue
		}
		location = textLocation(e.input.Path, section)
		properties["form"] = strings.TrimPrefix(section.Type, "gd_")
		for _, name := range []string{"type", "script_class", "uid"} {
			if value := sectionAttribute(section, name); value != "" {
				properties[name] = value
			}
		}
		break
	}
	return properties, location
}

func (e *textResourceExtractor) prepare(document *textresource.Document) {
	for _, item := range document.Items {
		section, ok := item.(*textresource.Section)
		if !ok {
			continue
		}
		if (section.Type == "gd_scene" || section.Type == "gd_resource") && sectionAttribute(section, "uid") != "" {
			uid := sectionAttribute(section, "uid")
			e.b.AddFact(e.moduleID, graph.EdgeReferences, "", uid, graph.KindConfigKey,
				textLocation(e.input.Path, section), map[string]string{"uid": uid})
		}
		if section.Type != "ext_resource" {
			continue
		}
		id := sectionAttribute(section, "id")
		if id == "" {
			continue
		}
		if uid := sectionAttribute(section, "uid"); uid != "" {
			e.b.AddFact(e.moduleID, graph.EdgeReferences, "", uid, graph.KindConfigKey,
				textLocation(e.input.Path, section), map[string]string{"uid": uid, "id": id})
		}
		target := resourceModule(sectionAttribute(section, "path"))
		if target == "" {
			continue
		}
		e.extResources[id] = target
		properties := map[string]string{"resource": sectionAttribute(section, "path"), "id": id}
		if resourceType := sectionAttribute(section, "type"); resourceType != "" {
			properties["type"] = resourceType
		}
		e.b.AddFact(e.moduleID, graph.EdgeImports, "", target, graph.KindModule,
			textLocation(e.input.Path, section), properties)
	}
	for _, item := range document.Items {
		section, ok := item.(*textresource.Section)
		if !ok {
			continue
		}
		switch section.Type {
		case "sub_resource":
			e.prepareSubResource(section)
		case "node":
			e.prepareSceneNode(section)
		case "resource":
			e.sectionOwners[section] = resourceOwner{id: e.moduleID, qualified: e.module}
		}
	}
}

func (e *textResourceExtractor) prepareSubResource(section *textresource.Section) {
	id := sectionAttribute(section, "id")
	if id == "" {
		return
	}
	qualified := e.module + "#" + id
	properties := map[string]string{"form": "sub_resource"}
	if resourceType := sectionAttribute(section, "type"); resourceType != "" {
		properties["type"] = resourceType
	}
	nodeID := e.b.Declare(e.moduleID, graph.Node{
		Kind: graph.KindVariable, Name: id, QualifiedName: qualified,
		Location: textLocation(e.input.Path, section), Properties: properties,
	})
	e.subResources[id] = nodeID
	e.sectionOwners[section] = resourceOwner{id: nodeID, qualified: qualified}
}

func (e *textResourceExtractor) prepareSceneNode(section *textresource.Section) {
	name := sectionAttribute(section, "name")
	if name == "" {
		return
	}
	parent := sectionAttribute(section, "parent")
	var scenePath string
	switch {
	case e.root == "":
		e.root, scenePath = name, name
	case parent == "" || parent == ".":
		scenePath = e.root + "/" + name
	default:
		scenePath = pathpkg.Clean(e.root + "/" + parent + "/" + name)
	}
	qualified := e.module + ":" + scenePath
	properties := map[string]string{"form": "scene_node", "node_path": scenePath}
	for _, attribute := range []string{"type", "parent", "owner", "unique_name_in_owner"} {
		if value := sectionAttribute(section, attribute); value != "" {
			properties[attribute] = value
		}
	}
	parentID := e.moduleID
	if parentPath := pathpkg.Dir(scenePath); parentPath != "." {
		if owner, ok := e.nodes[parentPath]; ok {
			parentID = owner.id
		}
	}
	nodeID := e.b.Declare(parentID, graph.Node{
		Kind: graph.KindVariable, Name: name, QualifiedName: qualified,
		Location: textLocation(e.input.Path, section), Properties: properties,
	})
	owner := resourceOwner{id: nodeID, qualified: qualified, scenePath: scenePath}
	e.nodes[scenePath] = owner
	e.sectionOwners[section] = owner

	if instance := sectionAttributeValue(section, "instance"); instance != nil {
		e.addValueReferences(nodeID, instance, owner)
	}
}

func (e *textResourceExtractor) extract(document *textresource.Document) {
	for _, item := range document.Items {
		section, ok := item.(*textresource.Section)
		if !ok {
			continue
		}
		if section.Type == "connection" {
			e.extractConnection(section)
			continue
		}
		owner, ok := e.sectionOwners[section]
		if !ok {
			continue
		}
		for _, item := range section.Body {
			assignment, ok := item.(*textresource.Assignment)
			if !ok {
				continue
			}
			loc := textLocation(e.input.Path, assignment)
			properties := map[string]string{"form": "resource_property"}
			if valueType := textValueType(assignment.Value); valueType != "" {
				properties["type"] = valueType
			}
			fieldID := e.b.AddNode(graph.Node{
				Kind: graph.KindField, Name: assignment.Property,
				QualifiedName: qualify(owner.qualified, assignment.Property), Location: loc, Properties: properties,
			})
			e.b.AddFact(owner.id, graph.EdgeHasField, fieldID, "", graph.KindField, loc, nil)
			e.addValueReferences(fieldID, assignment.Value, owner)
		}
	}
}

func (e *textResourceExtractor) addValueReferences(fromID string, value textresource.Value, owner resourceOwner) {
	textresource.Inspect(value, func(node textresource.Node) bool {
		switch current := node.(type) {
		case *textresource.CallValue:
			argument := firstTextString(current.Arguments)
			switch current.Name {
			case "ExtResource":
				if target := e.extResources[argument]; target != "" {
					e.b.AddFact(fromID, graph.EdgeReferences, "", target, graph.KindModule,
						textLocation(e.input.Path, current), map[string]string{"resource_id": argument})
				}
			case "SubResource":
				if targetID := e.subResources[argument]; targetID != "" {
					e.b.AddFact(fromID, graph.EdgeReferences, targetID, "", graph.KindVariable,
						textLocation(e.input.Path, current), map[string]string{"resource_id": argument})
				}
			}
		case *textresource.StringValue:
			if current.Kind == textresource.NodePath && owner.scenePath != "" {
				e.addNodePathReference(fromID, current.Value, owner, textLocation(e.input.Path, current))
			}
		}
		return true
	})
}

func (e *textResourceExtractor) addNodePathReference(fromID, value string, owner resourceOwner, loc graph.Location) {
	if value == "" || strings.HasPrefix(value, "%") {
		return
	}
	targetPath := pathpkg.Clean(pathpkg.Join(owner.scenePath, value))
	targetID := ""
	if target, ok := e.nodes[targetPath]; ok {
		targetID = target.id
	}
	e.b.AddFact(fromID, graph.EdgeReferences, targetID, e.module+":"+targetPath, graph.KindVariable,
		loc, map[string]string{"node_path": value})
}

func (e *textResourceExtractor) extractConnection(section *textresource.Section) {
	from := e.connectionNode(sectionAttribute(section, "from"))
	to := e.connectionNode(sectionAttribute(section, "to"))
	signal := sectionAttribute(section, "signal")
	if from.id == "" || to.id == "" || signal == "" {
		return
	}
	qualified := qualify(from.qualified, signal)
	eventID := e.events[qualified]
	if eventID == "" {
		eventID = e.b.Declare(from.id, graph.Node{
			Kind: graph.KindEvent, Name: signal, QualifiedName: qualified,
			Location: textLocation(e.input.Path, section), Properties: map[string]string{"form": "scene_signal"},
		})
		e.events[qualified] = eventID
	}
	properties := map[string]string{"signal": signal}
	if method := sectionAttribute(section, "method"); method != "" {
		properties["method"] = method
		e.b.AddFact(eventID, graph.EdgeHandledBy, "", method, graph.KindMethod,
			textLocation(e.input.Path, section), map[string]string{"receiver": to.qualified})
	}
	e.b.AddFact(to.id, graph.EdgeSubscribes, eventID, "", graph.KindEvent,
		textLocation(e.input.Path, section), properties)
}

func (e *textResourceExtractor) connectionNode(value string) resourceOwner {
	if value == "." || value == "" {
		return e.nodes[e.root]
	}
	return e.nodes[pathpkg.Clean(e.root+"/"+value)]
}

func sectionAttribute(section *textresource.Section, name string) string {
	value := sectionAttributeValue(section, name)
	switch current := value.(type) {
	case *textresource.StringValue:
		return current.Value
	case *textresource.IdentifierValue:
		return current.Name
	case *textresource.IntegerValue:
		if current.Suffix != "" {
			return strconv.FormatInt(current.Value, 10) + current.Suffix
		}
		return strconv.FormatInt(current.Value, 10)
	case *textresource.BoolValue:
		return strconv.FormatBool(current.Value)
	default:
		return ""
	}
}

func sectionAttributeValue(section *textresource.Section, name string) textresource.Value {
	for _, attribute := range section.Attributes {
		if attribute.Name == name {
			return attribute.Value
		}
	}
	return nil
}

func firstTextString(items []textresource.CompositeItem) string {
	for _, item := range items {
		if value, ok := item.(*textresource.StringValue); ok {
			return value.Value
		}
	}
	return ""
}

func textValueType(value textresource.Value) string {
	switch current := value.(type) {
	case *textresource.CallValue:
		return current.Name
	case *textresource.TypedArrayValue:
		return "Array"
	case *textresource.TypedDictionaryValue:
		return "Dictionary"
	case *textresource.ArrayValue:
		return "Array"
	case *textresource.DictionaryValue:
		return "Dictionary"
	case *textresource.StringValue:
		return string(current.Kind)
	default:
		return ""
	}
}

func textLocation(path string, node textresource.Node) graph.Location {
	span := node.Span()
	return graph.Location{Path: path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
}

func pathExtension(path string) string {
	if index := strings.LastIndex(path, "."); index >= 0 {
		return path[index:]
	}
	return ""
}
