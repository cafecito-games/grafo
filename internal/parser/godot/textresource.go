package godot

import (
	"fmt"
	pathpkg "path"
	"strconv"
	"strings"

	"github.com/cafecito-games/gdparser/textresource"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

type resourceOwner struct {
	id        string
	qualified string
	scenePath string
}

// extResource is one [ext_resource] declaration. Path evidence is canonical;
// the UID is an alias kept as evidence. Ambiguous marks an id declared twice
// with different paths, which resolves to nothing rather than to a guess.
type extResource struct {
	id           string
	resource     string
	canonical    string
	uid          string
	kind         graph.NodeKind
	declaredType string
	ambiguous    bool
}

type textResourceExtractor struct {
	b             *parserapi.Builder
	input         parserapi.Input
	module        string
	moduleID      string
	moduleKind    graph.NodeKind
	extResources  map[string]extResource
	subResources  map[string]string
	nodes         map[string]resourceOwner
	sectionOwners map[*textresource.Section]resourceOwner
	events        map[string]string
	ambiguousUIDs map[string]bool
	root          string
}

func extractTextResource(input parserapi.Input, document *textresource.Document) graph.ParseResult {
	b := parserapi.NewBuilder(input, "godot-resource")
	e := &textResourceExtractor{
		b: b, input: input, module: godotid.Canonical(input.Path), extResources: map[string]extResource{},
		subResources: map[string]string{}, nodes: map[string]resourceOwner{},
		sectionOwners: map[*textresource.Section]resourceOwner{}, events: map[string]string{},
		ambiguousUIDs: map[string]bool{},
	}
	properties, location := e.documentProperties(document)
	e.moduleKind = graph.KindGodotResource
	if properties["form"] == "scene" {
		e.moduleKind = graph.KindGodotScene
	}
	e.moduleID = b.Declare(b.FileID(), graph.Node{
		Kind: e.moduleKind, Name: graph.SimpleName(e.module), QualifiedName: e.module,
		Location: location, Properties: properties,
	})
	e.declareUID(properties["uid"], location)
	e.prepare(document)
	e.extract(document)
	return b.Finish()
}

func (e *textResourceExtractor) documentProperties(document *textresource.Document) (map[string]string, graph.Location) {
	properties := map[string]string{"format": strings.TrimPrefix(strings.ToLower(pathExtension(e.input.Path)), ".")}
	if godotid.Classify(e.input.Path) == godotid.ClassScene {
		properties["form"] = "scene"
	} else {
		properties["form"] = "resource"
	}
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

// declareUID records the resource's own UID alias as a declaration owned by
// this file, so a uid:// reference from anywhere in the project resolves to
// exactly one alias when the UID is unique and stays unresolved when it is not.
func (e *textResourceExtractor) declareUID(uid string, location graph.Location) {
	if uid == "" {
		return
	}
	e.b.Declare(e.b.FileID(), graph.Node{
		Kind: graph.KindConfigKey, Name: uid, QualifiedName: uid, Location: location,
		Properties: map[string]string{"format": "uid", "form": "resource_uid", "resource": e.input.Path},
	})
	e.b.AddFact(e.moduleID, graph.EdgeReferences, "", uid, graph.KindConfigKey, location,
		map[string]string{"uid": uid})
}

func (e *textResourceExtractor) prepare(document *textresource.Document) {
	e.prepareExtResources(document)
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

func (e *textResourceExtractor) prepareExtResources(document *textresource.Document) {
	uidPaths := map[string]string{}
	for _, item := range document.Items {
		section, ok := item.(*textresource.Section)
		if !ok || section.Type != "ext_resource" {
			continue
		}
		loc := textLocation(e.input.Path, section)
		id := sectionAttribute(section, "id")
		resource := sectionAttribute(section, "path")
		uid := godotid.UID(sectionAttribute(section, "uid"))
		canonical := godotid.Canonical(resource)
		if uid != "" {
			if previous, seen := uidPaths[uid]; seen && previous != canonical {
				e.ambiguousUIDs[uid] = true
				e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf(
					"UID %s maps to %s and %s; keeping the reference unresolved", uid, previous, canonical))
			} else if !seen {
				uidPaths[uid] = canonical
			}
		}
		if canonical == "" {
			if uid != "" {
				e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf(
					"ext_resource %q declares UID %s without a path", id, uid))
			}
			continue
		}
		entry := extResource{
			id: id, resource: resource, canonical: canonical, uid: uid,
			kind: godotid.TargetKind(resource), declaredType: sectionAttribute(section, "type"),
		}
		if id != "" {
			if previous, seen := e.extResources[id]; seen && previous.canonical != canonical {
				entry.ambiguous = true
				e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf(
					"ext_resource id %q declares both %s and %s; keeping its uses unresolved",
					id, previous.canonical, canonical))
			}
			e.extResources[id] = entry
		}
		properties := map[string]string{"resource": resource, "id": id}
		if entry.declaredType != "" {
			properties["type"] = entry.declaredType
		}
		if uid != "" && !e.ambiguousUIDs[uid] {
			properties["uid"] = uid
		}
		e.b.AddFact(e.moduleID, graph.EdgeImports, "", canonical, entry.kind, loc, properties)
	}
	for _, item := range document.Items {
		section, ok := item.(*textresource.Section)
		if !ok || section.Type != "ext_resource" {
			continue
		}
		uid := godotid.UID(sectionAttribute(section, "uid"))
		if uid == "" || e.ambiguousUIDs[uid] {
			continue
		}
		e.b.AddFact(e.moduleID, graph.EdgeReferences, "", uid, graph.KindConfigKey,
			textLocation(e.input.Path, section),
			map[string]string{"uid": uid, "id": sectionAttribute(section, "id")})
	}
}

// resolveExtResource returns the referenced declaration when it is exact. An
// unknown or ambiguous id yields no target so no edge is guessed.
func (e *textResourceExtractor) resolveExtResource(id string) (extResource, bool) {
	entry, ok := e.extResources[id]
	if !ok || entry.ambiguous || entry.canonical == "" {
		return extResource{}, false
	}
	return entry, true
}

func (e *textResourceExtractor) prepareSubResource(section *textresource.Section) {
	id := sectionAttribute(section, "id")
	if id == "" {
		return
	}
	qualified := godotid.SubResourceQualifiedName(e.module, id)
	properties := map[string]string{"form": "sub_resource", "resource": e.module}
	if resourceType := sectionAttribute(section, "type"); resourceType != "" {
		properties["type"] = resourceType
	}
	nodeID := e.b.Declare(e.moduleID, graph.Node{
		Kind: graph.KindGodotResource, Name: id, QualifiedName: qualified,
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
	inheritedRoot := false
	switch {
	case e.root == "":
		e.root, scenePath = name, name
		inheritedRoot = true
	case parent == "" || parent == ".":
		scenePath = e.root + "/" + name
	default:
		scenePath = pathpkg.Clean(e.root + "/" + parent + "/" + name)
	}
	qualified := godotid.SceneNodeQualifiedName(e.module, scenePath)
	properties := map[string]string{"form": "scene_node", "node_path": scenePath, "scene": e.module}
	for _, attribute := range []string{"type", "parent", "owner", "unique_name_in_owner", "index"} {
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
		Kind: graph.KindGodotSceneNode, Name: name, QualifiedName: qualified,
		Location: textLocation(e.input.Path, section), Properties: properties,
	})
	owner := resourceOwner{id: nodeID, qualified: qualified, scenePath: scenePath}
	e.nodes[scenePath] = owner
	e.sectionOwners[section] = owner
	e.addInstance(section, owner, inheritedRoot)
}

// addInstance records a scene instance. A root node that carries an instance
// is Godot scene inheritance, which is reported as an instantiates edge from
// the scene itself as well as from its root node: it is scene composition, not
// language class inheritance, so it never becomes an extends edge.
func (e *textResourceExtractor) addInstance(section *textresource.Section, owner resourceOwner, inheritedRoot bool) {
	form := "nested"
	if inheritedRoot {
		form = "inherited"
	}
	loc := textLocation(e.input.Path, section)
	if value := sectionAttributeValue(section, "instance"); value != nil {
		call, ok := value.(*textresource.CallValue)
		if !ok || call.Name != "ExtResource" {
			e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf(
				"scene node %q has an unsupported instance value; keeping it unresolved", owner.scenePath))
			return
		}
		id := firstTextString(call.Arguments)
		entry, ok := e.resolveExtResource(id)
		if !ok {
			return
		}
		properties := map[string]string{"form": form, "resource_id": id, "resource": entry.resource,
			"node_path": owner.scenePath}
		if entry.uid != "" {
			properties["uid"] = entry.uid
		}
		if entry.declaredType != "" {
			properties["type"] = entry.declaredType
		}
		e.addInstanceFacts(owner, entry.canonical, entry.kind, properties, loc, inheritedRoot)
		return
	}
	placeholder := sectionAttribute(section, "instance_placeholder")
	if placeholder == "" {
		return
	}
	canonical := godotid.Canonical(placeholder)
	if canonical == "" {
		e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf(
			"scene node %q declares instance_placeholder %q without a path; keeping it unresolved",
			owner.scenePath, placeholder))
		return
	}
	e.addInstanceFacts(owner, canonical, godotid.TargetKind(placeholder), map[string]string{
		"form": form, "resource": placeholder, "node_path": owner.scenePath, "instance_placeholder": "true",
	}, loc, inheritedRoot)
}

func (e *textResourceExtractor) addInstanceFacts(owner resourceOwner, target string, kind graph.NodeKind, properties map[string]string, loc graph.Location, inheritedRoot bool) {
	e.b.AddFact(owner.id, graph.EdgeInstantiates, "", target, kind, loc, properties)
	if !inheritedRoot {
		return
	}
	sceneProperties := make(map[string]string, len(properties))
	for key, value := range properties {
		sceneProperties[key] = value
	}
	e.b.AddFact(e.moduleID, graph.EdgeInstantiates, "", target, kind, loc, sceneProperties)
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
			if assignment.Property == "script" && e.addScriptAttachment(owner, assignment) {
				continue
			}
			e.addValueReferences(fieldID, assignment.Value, owner)
		}
	}
}

// addScriptAttachment records a script bound to a scene node, scene, or
// resource as an attaches_script edge from the owner. It reports whether the
// attachment was recorded so the generic property reference is not duplicated.
func (e *textResourceExtractor) addScriptAttachment(owner resourceOwner, assignment *textresource.Assignment) bool {
	call, ok := assignment.Value.(*textresource.CallValue)
	if !ok || call.Name != "ExtResource" {
		return false
	}
	id := firstTextString(call.Arguments)
	entry, ok := e.resolveExtResource(id)
	if !ok {
		return false
	}
	properties := map[string]string{"resource": entry.resource, "resource_id": id}
	if entry.uid != "" {
		properties["uid"] = entry.uid
	}
	if entry.declaredType != "" {
		properties["type"] = entry.declaredType
	}
	if owner.scenePath != "" {
		properties["node_path"] = owner.scenePath
	}
	e.b.AddFact(owner.id, graph.EdgeAttachesScript, "", entry.canonical, entry.kind,
		textLocation(e.input.Path, assignment), properties)
	return true
}

func (e *textResourceExtractor) addValueReferences(fromID string, value textresource.Value, owner resourceOwner) {
	textresource.Inspect(value, func(node textresource.Node) bool {
		switch current := node.(type) {
		case *textresource.CallValue:
			argument := firstTextString(current.Arguments)
			switch current.Name {
			case "ExtResource":
				if entry, ok := e.resolveExtResource(argument); ok {
					properties := map[string]string{"resource_id": argument, "resource": entry.resource}
					if entry.uid != "" {
						properties["uid"] = entry.uid
					}
					e.b.AddFact(fromID, graph.EdgeReferences, "", entry.canonical, entry.kind,
						textLocation(e.input.Path, current), properties)
				}
			case "SubResource":
				if targetID := e.subResources[argument]; targetID != "" {
					e.b.AddFact(fromID, graph.EdgeReferences, targetID, "", graph.KindGodotResource,
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
	e.b.AddFact(fromID, graph.EdgeReferences, targetID, godotid.SceneNodeQualifiedName(e.module, targetPath),
		graph.KindGodotSceneNode, loc, map[string]string{"node_path": value})
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
