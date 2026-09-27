package gdscript

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/cafecito-games/gdparser"
	gdast "github.com/cafecito-games/gdparser/ast"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

// Parser extracts semantic graph nodes and relationships from Godot 4
// GDScript source files.
type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "gdscript" }

func (*Parser) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".gd")
}

// SemanticKey makes the owning Godot project's autoload vocabulary part of the
// incremental cache key, so editing project.godot reparses scripts whose
// autoload uses can now resolve differently.
func (*Parser) SemanticKey(_ context.Context, input parserapi.Input) (string, error) {
	project, err := godotid.LoadProject(input.Root, input.Path)
	if err != nil {
		return "", err
	}
	return "gdscript-godot-project-v1:" + project.SemanticKey(), nil
}

// SemanticAffectedPaths reparses every script under a Godot project whose
// project.godot changed. SemanticDependencies matches only repository-root
// paths, and a Godot project can sit in any subdirectory of a monorepo.
func (*Parser) SemanticAffectedPaths(allPaths, changedPaths []string) []string {
	var roots []string
	for _, path := range changedPaths {
		if filepath.Base(path) != godotid.ProjectFileName {
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(path))
		if directory == "." {
			directory = ""
		} else {
			directory += "/"
		}
		roots = append(roots, directory)
	}
	if len(roots) == 0 {
		return nil
	}
	var affected []string
	for _, path := range allPaths {
		if !strings.EqualFold(filepath.Ext(path), ".gd") {
			continue
		}
		for _, root := range roots {
			if root == "" || strings.HasPrefix(filepath.ToSlash(path), root) {
				affected = append(affected, path)
				break
			}
		}
	}
	return affected
}

type signalRef struct {
	id        string
	qualified string
}

type scope struct {
	currentID string
	parentID  string
	container string
	receiver  string
	classID   string
	classBody bool
	symbols   map[string]string
	types     map[string]string
	signals   map[string]signalRef
}

type extractor struct {
	b       *parserapi.Builder
	input   parserapi.Input
	module  string
	methods map[string]string
	// project holds the autoload vocabulary of the Godot project that owns
	// this script. Only exact declarations appear in it, so an autoload name
	// that is declared twice or malformed resolves nothing here.
	project godotid.Project
	// autoloads records identifier nodes already reported as an autoload use,
	// so a call through an autoload is not also reported as a bare reference.
	autoloads map[gdast.Node]bool
	// projectKnown is false when the owning Godot project could not be read, in
	// which case no res:// reference in this script resolves.
	projectKnown bool
	// bases maps every class this file declares to the type it extends, which is
	// the only inheritance evidence a single-file parser holds. It is what lets a
	// receiver typed as a locally declared subclass of an input class still be
	// recognized as one.
	bases map[string]string
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "gdscript")
	if err := ctx.Err(); err != nil {
		return b.Finish(), err
	}
	file, err := gdparser.ParseFile(input.Path, input.Content)
	if err != nil {
		return b.Finish(), fmt.Errorf("parse GDScript: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return b.Finish(), err
	}
	e := &extractor{b: b, input: input, module: parserapi.ModuleName(input.Path),
		methods: map[string]string{}, autoloads: map[gdast.Node]bool{},
		projectKnown: true, bases: map[string]string{}}
	project, projectErr := godotid.LoadProject(input.Root, input.Path)
	if projectErr != nil {
		// The owning project is unknown rather than absent, so res:// references
		// cannot be canonicalized without guessing which project they belong to.
		e.projectKnown = false
		b.Diagnostic(1, "warning", fmt.Sprintf("read Godot project configuration: %v", projectErr))
	}
	e.project = project
	e.extract(file)
	return b.Finish(), nil
}

func (e *extractor) extract(file *gdast.File) {
	className := graph.SimpleName(e.module)
	qualified := e.module
	loc := graph.Location{Path: e.input.Path, Line: 1, Column: 1, EndLine: 1}
	properties := map[string]string{"implicit": "true"}
	for _, statement := range file.Statements {
		directive, ok := statement.(*gdast.Directive)
		if !ok || directive.Name != "class_name" {
			continue
		}
		if name := expressionName(directive.Value); name != "" {
			className, qualified = name, name
			loc = e.location(directive)
			properties = nil
		}
		break
	}
	if className == "" {
		className = strings.TrimSuffix(filepath.Base(e.input.Path), filepath.Ext(e.input.Path))
	}
	if qualified == "" {
		qualified = className
	}
	// The script file is also a Godot resource. Declaring it as a module named
	// by its canonical repository-relative path gives scenes, resources,
	// configuration, and UID sidecars one exact node to attach a script to,
	// including when the script declares a class_name of its own.
	moduleID := e.b.Declare(e.b.FileID(), graph.Node{Kind: graph.KindModule,
		Name: graph.SimpleName(e.module), QualifiedName: e.module,
		Location:   graph.Location{Path: e.input.Path, Line: 1, Column: 1, EndLine: 1},
		Properties: map[string]string{"form": "script", "format": "gd"}})
	classID := e.b.Declare(moduleID, graph.Node{Kind: graph.KindClass, Name: className,
		QualifiedName: qualified, Location: loc, Properties: properties})
	root := scope{currentID: classID, parentID: classID, container: qualified, receiver: qualified,
		classID: classID, classBody: true, symbols: map[string]string{}, types: map[string]string{}, signals: map[string]signalRef{}}
	root.types[className] = qualified
	// The base class is recorded before anything is walked, because a method body
	// earlier in the file may already call through a receiver typed by it.
	for _, statement := range file.Statements {
		directive, ok := statement.(*gdast.Directive)
		if !ok {
			continue
		}
		base := ""
		switch {
		case directive.Name == "extends":
			base = expressionName(directive.Value)
		case directive.Name == "class_name" && directive.Extends != nil:
			base = expressionName(directive.Extends)
		}
		if base != "" {
			e.bases[qualified] = base
		}
	}
	e.prepareClass(file.Statements, root)
	e.walkStatements(file.Statements, root)
}

func (e *extractor) prepareClass(statements []gdast.Statement, current scope) {
	for _, statement := range statements {
		switch node := statement.(type) {
		case *gdast.VariableDeclaration:
			qualified := qualify(current.container, node.Name)
			current.symbols[node.Name] = graph.NodeID(graph.KindField, qualified, e.input.RepoID, e.input.Path)
			if resolved := e.resolveType(node.Type, current); resolved != "" {
				current.types[node.Name] = resolved
			}
		case *gdast.SignalDeclaration:
			qualified := qualify(current.container, node.Name)
			ref := signalRef{id: graph.NodeID(graph.KindEvent, qualified, e.input.RepoID, e.input.Path), qualified: qualified}
			current.signals[node.Name] = ref
			current.signals[qualified] = ref
		case *gdast.FunctionDeclaration:
			qualified := qualify(current.container, node.Name)
			e.methods[qualify(current.receiver, node.Name)] = qualified
		case *gdast.ClassDeclaration:
			qualified := qualify(current.container, node.Name)
			current.types[node.Name] = qualified
			if node.Extends != "" {
				e.bases[qualified] = node.Extends
			}
		case *gdast.EnumDeclaration:
			if node.Name != "" {
				current.types[node.Name] = qualify(current.container, node.Name)
			}
		}
	}
	for _, statement := range statements {
		if node, ok := statement.(*gdast.VariableDeclaration); ok && current.types[node.Name] == "" {
			if inferred := e.inferExpressionType(node.Value, current); inferred != "" {
				current.types[node.Name] = inferred
			}
		}
	}
}

func (e *extractor) walkStatements(statements []gdast.Statement, current scope) {
	var annotations []string
	for _, statement := range statements {
		if annotation, ok := statement.(*gdast.Annotation); ok {
			annotations = append(annotations, annotation.Name)
			for _, argument := range annotation.Arguments {
				e.walkExpression(argument, current)
			}
			continue
		}
		if _, ok := statement.(*gdast.Comment); ok {
			continue
		}
		e.walkStatement(statement, current, annotations)
		annotations = nil
	}
}

func (e *extractor) walkStatement(statement gdast.Statement, current scope, annotations []string) {
	switch node := statement.(type) {
	case *gdast.Directive:
		e.parseDirective(node, current)
	case *gdast.VariableDeclaration:
		e.parseVariable(node, current, annotations)
	case *gdast.Assignment:
		e.parseAssignment(node, current)
	case *gdast.ReturnStatement:
		e.parseReturn(node, current)
	case *gdast.ExpressionStatement:
		e.walkExpression(node.Expression, current)
	case *gdast.FunctionDeclaration:
		e.parseFunction(node, current, annotations)
	case *gdast.ClassDeclaration:
		e.parseClass(node, current)
	case *gdast.SignalDeclaration:
		e.parseSignal(node, current)
	case *gdast.EnumDeclaration:
		e.parseEnum(node, current)
	case *gdast.IfStatement:
		for _, branch := range node.Branches {
			e.walkExpression(branch.Condition, current)
			e.walkStatements(branch.Body, current)
		}
		e.walkStatements(node.Else, current)
	case *gdast.WhileStatement:
		e.walkExpression(node.Condition, current)
		e.walkStatements(node.Body, current)
	case *gdast.ForStatement:
		e.walkExpression(node.Iterable, current)
		e.declareLocal(node.Variable, node.Type, e.location(node), current)
		e.walkStatements(node.Body, current)
	case *gdast.MatchStatement:
		e.walkExpression(node.Value, current)
		for _, matchCase := range node.Cases {
			for _, pattern := range matchCase.Patterns {
				e.walkExpression(pattern, current)
			}
			e.walkExpression(matchCase.Guard, current)
			e.walkStatements(matchCase.Body, current)
		}
	}
}

func (e *extractor) parseDirective(node *gdast.Directive, current scope) {
	if node.Name != "extends" && (node.Name != "class_name" || node.Extends == nil) {
		return
	}
	base := node.Value
	if node.Name == "class_name" {
		base = node.Extends
	}
	target := e.resolveTypeExpression(base, current)
	if target != "" {
		e.b.AddFact(current.classID, graph.EdgeExtends, "", target, graph.KindClass, e.location(node), nil)
	}
	e.walkExpression(base, current)
}

func (e *extractor) parseFunction(node *gdast.FunctionDeclaration, current scope, annotations []string) {
	properties := map[string]string{"parameters": formatParameters(node.Parameters)}
	if node.ReturnType != "" {
		properties["returns"] = node.ReturnType
	}
	if node.Static {
		properties["static"] = "true"
	}
	if node.Abstract {
		properties["abstract"] = "true"
	}
	if len(annotations) > 0 {
		properties["annotations"] = strings.Join(annotations, ",")
	}
	qualified := qualify(current.container, node.Name)
	id := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindMethod, Name: node.Name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	functionScope := scope{currentID: id, parentID: id, container: qualified, receiver: current.receiver,
		classID: current.classID, symbols: cloneMap(current.symbols), types: cloneMap(current.types), signals: cloneSignals(current.signals)}
	for _, parameter := range node.Parameters {
		parameterProperties := map[string]string{}
		if parameter.Type != "" {
			parameterProperties["type"] = parameter.Type
		}
		parameterID := e.b.Declare(id, graph.Node{Kind: graph.KindParameter, Name: parameter.Name,
			QualifiedName: qualified + "." + parameter.Name, Location: e.location(node), Properties: parameterProperties})
		functionScope.symbols[parameter.Name] = parameterID
		if resolved := e.resolveType(parameter.Type, current); resolved != "" {
			functionScope.types[parameter.Name] = resolved
		}
		e.walkExpression(parameter.Default, functionScope)
	}
	e.walkStatements(node.Body, functionScope)
}

func (e *extractor) parseClass(node *gdast.ClassDeclaration, current scope) {
	qualified := qualify(current.container, node.Name)
	id := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindClass, Name: node.Name,
		QualifiedName: qualified, Location: e.location(node)})
	if node.Extends != "" {
		e.b.AddFact(id, graph.EdgeExtends, "", e.resolveType(node.Extends, current), graph.KindClass, e.location(node), nil)
	}
	inner := scope{currentID: id, parentID: id, container: qualified, receiver: qualified, classID: id,
		classBody: true, symbols: map[string]string{}, types: cloneMap(current.types), signals: map[string]signalRef{}}
	inner.types[node.Name] = qualified
	e.prepareClass(node.Body, inner)
	e.walkStatements(node.Body, inner)
}

func (e *extractor) parseVariable(node *gdast.VariableDeclaration, current scope, annotations []string) {
	properties := map[string]string{}
	if node.Type != "" {
		properties["type"] = node.Type
	}
	if node.Constant {
		properties["constant"] = "true"
	}
	if node.Static {
		properties["static"] = "true"
	}
	if node.Inferred {
		properties["inferred"] = "true"
	}
	if len(annotations) > 0 {
		properties["annotations"] = strings.Join(annotations, ",")
	}
	loc := e.location(node)
	kind := graph.KindVariable
	qualified := fmt.Sprintf("%s@%d", qualify(current.container, node.Name), loc.Line)
	if current.classBody {
		kind = graph.KindField
		qualified = qualify(current.container, node.Name)
	}
	graphNode := graph.Node{Kind: kind, Name: node.Name, QualifiedName: qualified, Location: loc, Properties: properties}
	var id string
	if kind == graph.KindField {
		id = e.b.AddNode(graphNode)
		e.b.AddFact(current.classID, graph.EdgeHasField, id, "", kind, loc, nil)
	} else {
		id = e.b.Declare(current.currentID, graphNode)
	}
	current.symbols[node.Name] = id
	if resolved := e.resolveType(node.Type, current); resolved != "" {
		current.types[node.Name] = resolved
	} else if inferred := e.inferExpressionType(node.Value, current); inferred != "" {
		current.types[node.Name] = inferred
	}
	for _, sourceID := range e.referencedVariables(node.Value, current) {
		if sourceID != id {
			e.b.AddFact(sourceID, graph.EdgeAssigns, id, "", kind, loc, nil)
		}
	}
	e.walkExpression(node.Value, current)
	e.walkStatements(node.Getter, current)
	if node.Setter != nil {
		e.walkStatements(node.Setter.Body, current)
	}
}

func (e *extractor) declareLocal(name, typeName string, loc graph.Location, current scope) {
	if name == "" || current.currentID == "" {
		return
	}
	properties := map[string]string{}
	if typeName != "" {
		properties["type"] = typeName
	}
	id := e.b.Declare(current.currentID, graph.Node{Kind: graph.KindVariable, Name: name,
		QualifiedName: fmt.Sprintf("%s.%s@%d", current.container, name, loc.Line), Location: loc, Properties: properties})
	current.symbols[name] = id
	if resolved := e.resolveType(typeName, current); resolved != "" {
		current.types[name] = resolved
	}
}

func (e *extractor) parseAssignment(node *gdast.Assignment, current scope) {
	targetID := e.symbolID(node.Target, current)
	if targetID != "" {
		for _, sourceID := range e.referencedVariables(node.Value, current) {
			e.b.AddFact(sourceID, graph.EdgeAssigns, targetID, "", graph.KindVariable, e.location(node), nil)
		}
	}
	e.walkExpression(node.Target, current)
	e.walkExpression(node.Value, current)
}

func (e *extractor) parseReturn(node *gdast.ReturnStatement, current scope) {
	for _, sourceID := range e.referencedVariables(node.Value, current) {
		e.b.AddFact(sourceID, graph.EdgeReturns, current.currentID, "", "", e.location(node), nil)
	}
	e.walkExpression(node.Value, current)
}

func (e *extractor) parseSignal(node *gdast.SignalDeclaration, current scope) {
	qualified := qualify(current.container, node.Name)
	id := e.b.Declare(current.classID, graph.Node{Kind: graph.KindEvent, Name: node.Name,
		QualifiedName: qualified, Location: e.location(node), Properties: map[string]string{"parameters": formatParameters(node.Parameters)}})
	ref := signalRef{id: id, qualified: qualified}
	current.signals[node.Name] = ref
	current.signals[qualified] = ref
}

func (e *extractor) parseEnum(node *gdast.EnumDeclaration, current scope) {
	loc := e.location(node)
	if node.Name == "" {
		for _, member := range node.Members {
			e.addEnumMember(current.classID, current.container, member, loc)
		}
		return
	}
	qualified := qualify(current.container, node.Name)
	id := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindType, Name: node.Name,
		QualifiedName: qualified, Location: loc, Properties: map[string]string{"form": "enum"}})
	for _, member := range node.Members {
		e.addEnumMember(id, qualified, member, loc)
	}
}

func (e *extractor) addEnumMember(ownerID, container string, member gdast.EnumMember, loc graph.Location) {
	if member.Value != nil {
		loc = e.location(member.Value)
	} else if len(member.Comments) > 0 {
		loc = e.location(member.Comments[0])
	}
	id := e.b.AddNode(graph.Node{Kind: graph.KindField, Name: member.Name,
		QualifiedName: qualify(container, member.Name), Location: loc, Properties: map[string]string{"enum_member": "true"}})
	e.b.AddFact(ownerID, graph.EdgeHasField, id, "", graph.KindField, loc, nil)
}

func (e *extractor) walkExpression(expression gdast.Expression, current scope) {
	if expression == nil {
		return
	}
	switch node := expression.(type) {
	case *gdast.CallExpression:
		e.parseCall(node, current)
	case *gdast.NodePathExpression:
		properties := map[string]string{"form": "node_path", "node_path": node.Path}
		if node.Unique {
			properties["unique"] = "true"
			properties["form"] = "unique_name"
		}
		e.addNodeReference(current.currentID, node.Path, e.location(node), properties)
	case *gdast.Identifier:
		e.addAutoloadUse(node, node.Name, "autoload_reference", "", current)
	case *gdast.LambdaExpression:
		lambdaScope := current
		lambdaScope.symbols = cloneMap(current.symbols)
		lambdaScope.types = cloneMap(current.types)
		for _, parameter := range node.Parameters {
			e.declareLocal(parameter.Name, parameter.Type, e.location(node), lambdaScope)
		}
		e.walkStatements(node.Body, lambdaScope)
	}
	for _, child := range gdast.Children(expression) {
		if childExpression, ok := child.(gdast.Expression); ok {
			e.walkExpression(childExpression, current)
		}
	}
}

func (e *extractor) parseCall(node *gdast.CallExpression, current scope) {
	callee := e.resolveCallee(node.Callee, current)
	if callee == "" {
		return
	}
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	loc := e.location(node)
	if member, ok := node.Callee.(*gdast.MemberExpression); ok {
		if object, ok := member.Object.(*gdast.Identifier); ok {
			e.addAutoloadUse(object, object.Name, "autoload_call", member.Property, current)
		}
	}
	method := strings.ToLower(graph.SimpleName(callee))
	if method == "preload" || method == "load" {
		if len(node.Arguments) > 0 {
			if resource, ok := literalString(node.Arguments[0]); ok {
				target, targetKind := e.resourceModule(resource), graph.KindModule
				properties := map[string]string{"resource": resource}
				if strings.HasPrefix(strings.TrimSpace(resource), "uid://") {
					target, targetKind = strings.TrimSpace(resource), graph.KindConfigKey
					properties["uid"] = target
				}
				if target != "" {
					e.b.AddFact(e.b.FileID(), graph.EdgeImports, "", target, targetKind, loc, properties)
				}
			}
		}
		return
	}
	if callee == "OS.get_environment" && len(node.Arguments) > 0 {
		if key, ok := literalString(node.Arguments[0]); ok && key != "" {
			e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
			return
		}
	}
	if (callee == "ProjectSettings.get_setting" || callee == "ProjectSettings.has_setting") && len(node.Arguments) > 0 {
		if key, ok := literalString(node.Arguments[0]); ok && key != "" {
			e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc,
				map[string]string{"source": "project.godot"})
			return
		}
	}
	// A call that resolved to a method this script declares is that method, never
	// an engine API that happens to share its name - in either spelling.
	if !e.localCall(node.Callee, current) {
		// A bare call is a call on this object; a member call records the
		// receiver expression as evidence, and an expression that names nothing
		// knowable records no receiver rather than an empty one.
		receiver := "self"
		member, _ := node.Callee.(*gdast.MemberExpression)
		if member != nil {
			receiver = e.resolveExpression(member.Object, current)
		}
		e.addInputActionUses(node, member, fromID, method, receiver, current)
		e.addGroupOperations(node, fromID, method, receiver)
	}
	if isNodeLookup(method) && len(node.Arguments) > 0 {
		if nodePath, ok := literalString(node.Arguments[0]); ok {
			properties := map[string]string{"form": "node_lookup", "lookup": method, "node_path": nodePath}
			if strings.HasPrefix(strings.TrimSpace(nodePath), "%") {
				properties["unique"] = "true"
			}
			e.addNodeReference(fromID, nodePath, e.location(node.Arguments[0]), properties)
		}
	}
	if method == "emit_signal" && len(node.Arguments) > 0 {
		if name, ok := literalString(node.Arguments[0]); ok {
			e.addSignalFact(fromID, graph.EdgePublishes, "emit", name, current, loc, nil)
			return
		}
	}
	if _, ok := signalOperations[method]; ok {
		if member, ok := node.Callee.(*gdast.MemberExpression); ok {
			if e.addSignalOperation(node, member, fromID, method, current, loc) {
				return
			}
		}
	}
	for position, argument := range node.Arguments {
		for _, sourceID := range e.referencedVariables(argument, current) {
			e.b.AddFact(sourceID, graph.EdgePasses, "", callee, "", loc, map[string]string{"argument": strconv.Itoa(position)})
		}
	}
	e.b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
}

func isNodeLookup(method string) bool {
	switch method {
	case "get_node", "get_node_or_null", "get_node_and_resource", "has_node":
		return true
	default:
		return false
	}
}

// addAutoloadUse links a script use of a globally available autoload to its
// project.godot declaration. Only an exact, unshadowed, enabled declaration
// resolves: a name declared more than once, a malformed declaration, an
// autoload Godot does not expose as a global singleton, or a local symbol of
// the same name produces nothing rather than a guess. The member of an
// autoload call is kept as evidence and is deliberately not resolved to a
// method, because the autoload target's class name is not knowable here.
func (e *extractor) addAutoloadUse(node gdast.Node, name, form, member string, current scope) {
	if name == "" || e.autoloads[node] {
		return
	}
	declaration, ok := e.project.Singleton(name)
	if !ok || e.shadowed(name, current) {
		return
	}
	e.autoloads[node] = true
	fromID := current.currentID
	if fromID == "" {
		fromID = e.b.FileID()
	}
	properties := map[string]string{"form": form, "autoload": name, "resource": declaration.Reference}
	if member != "" {
		properties["member"] = member
	}
	if declaration.UID != "" {
		properties["uid"] = declaration.UID
	}
	e.b.AddFact(fromID, graph.EdgeReferences, "", e.project.AutoloadQualifiedName(name),
		graph.KindGodotAutoload, e.location(node), properties)
}

// shadowed reports whether a local declaration hides a global autoload name.
func (e *extractor) shadowed(name string, current scope) bool {
	if _, ok := current.symbols[name]; ok {
		return true
	}
	_, ok := current.types[name]
	return ok
}

func (e *extractor) addNodeReference(fromID, nodePath string, loc graph.Location, properties map[string]string) {
	if fromID == "" {
		fromID = e.b.FileID()
	}
	target := nodeReferenceTarget(nodePath)
	if target == "" {
		return
	}
	// Scene nodes are their own kind, so a node-path lookup resolves only
	// against declared scene nodes instead of any same-named variable.
	e.b.AddFact(fromID, graph.EdgeReferences, "", target, graph.KindGodotSceneNode, loc, properties)
}

func nodeReferenceTarget(nodePath string) string {
	nodePath = strings.TrimSpace(nodePath)
	nodePath = strings.TrimSpace(strings.TrimPrefix(nodePath, "%"))
	if index := strings.IndexByte(nodePath, ':'); index >= 0 {
		nodePath = nodePath[:index]
	}
	nodePath = strings.TrimRight(nodePath, "/")
	if index := strings.LastIndexByte(nodePath, '/'); index >= 0 {
		nodePath = nodePath[index+1:]
	}
	if nodePath == "." || nodePath == ".." {
		return ""
	}
	return nodePath
}

// signalOperation is how one signal-routing call form is recorded. Only connect
// establishes a route, so it is the only form that subscribes; a disconnect tears
// a route down and is_connected merely asks about one, and treating either as a
// subscription would make a signal look consumed by a script that removes or
// inspects its own wiring. Both stay references carrying their form, which is
// what the interactions report reads them back by.
type signalOperation struct {
	kind graph.EdgeKind
	form string
	// handler is the index of the Callable argument in the Godot 4 form, or -1
	// when the form routes nothing.
	handler int
	// callable marks a form whose first argument is a Callable in Godot 4, or a
	// signal name in the older Object form. emit is the exception: its first
	// argument is the signal's own payload, which can be any literal.
	callable bool
}

var signalOperations = map[string]signalOperation{
	"emit":         {kind: graph.EdgePublishes, form: "emit", handler: -1},
	"connect":      {kind: graph.EdgeSubscribes, form: "connect", handler: 0, callable: true},
	"disconnect":   {kind: graph.EdgeReferences, form: "signal_disconnect", handler: -1, callable: true},
	"is_connected": {kind: graph.EdgeReferences, form: "signal_connection_test", handler: -1, callable: true},
}

// addSignalOperation records one signal-routing call. Godot 4 spells these on the
// signal itself (pressed.connect(handler)), so the member's object names the
// signal; the older Object form passes the signal name as a literal first
// argument, which is the only reading available when that argument is a string,
// and shifts the Callable one place to the right. Anything else - a computed name,
// a Callable built from another object, a lambda - resolves nothing rather than a
// guess, so the call falls through to the ordinary call edge.
func (e *extractor) addSignalOperation(node *gdast.CallExpression, member *gdast.MemberExpression, fromID, method string, current scope, loc graph.Location) bool {
	operation := signalOperations[method]
	name, handler := "", operation.handler
	if operation.callable && len(node.Arguments) > 0 {
		// Godot 4 passes a Callable here and the older Object form passes the
		// signal's name, so a literal argument decides between the Object form
		// and "this is not a signal connection at all". A project is free to
		// declare a connect(url) of its own, and reading its argument as a
		// signal name would name an event after a URL.
		if literal, ok := node.Arguments[0].(*gdast.Literal); ok {
			text, isString := literalString(literal)
			if !isString || !isIdentifier(text) {
				return false
			}
			name, handler = text, operation.handler+1
		}
	}
	if name == "" {
		// A bare identifier that names no signal this script declares may still
		// be one the script's base class declares, which is resolved by name
		// against the whole graph rather than guessed here.
		name = e.resolveExpression(member.Object, current)
	}
	// The handler is recorded on the routing fact itself as well as on the
	// signal declaration. Most connects name a signal another file declares, so
	// the parser cannot reach that declaration to hang a handled_by edge on it;
	// the property keeps the route's destination visible in exactly those cases
	// without anyone guessing which declaration the name belongs to.
	extra := map[string]string{}
	subscriber := fromID
	if operation.form == "connect" && handler >= 0 {
		if method := e.handlerMethod(node.Arguments, handler, current); method != "" {
			extra["handler"] = method
			// The thing that receives the signal is the handler, not the
			// statement that wired it, and the handler is a node this parser
			// owns. Sourcing the route there is what lets the graph traverse a
			// resolved signal back to its declared handler even when the signal
			// is declared in another file - the case a handled_by edge cannot
			// cover, because that edge's source would have to be the foreign
			// declaration and facts resolve only their target by name. The
			// wiring statement is kept as the site, and the fact's own location
			// still points at the connect call.
			if id := e.handlerNode(method); id != "" {
				subscriber = id
				if current.container != "" {
					extra["site"] = current.container
				}
			}
		}
	}
	ref, emitted := e.addSignalFact(subscriber, operation.kind, operation.form, name, current, loc, extra)
	if !emitted {
		return false
	}
	if operation.form == "connect" && handler >= 0 {
		e.addSignalHandler(ref, node.Arguments, handler, current, loc)
	}
	return true
}

// isIdentifier reports whether a literal could be a GDScript signal name.
func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		switch {
		case character == '_':
		case unicode.IsLetter(character):
		case unicode.IsDigit(character) && index > 0:
		default:
			return false
		}
	}
	return true
}

// handlerNode returns the graph id of a method this script declares. The node is
// owned by this file, so its id is derivable here exactly as the declaration
// site derives it.
func (e *extractor) handlerNode(qualified string) string {
	if qualified == "" {
		return ""
	}
	return graph.NodeID(graph.KindMethod, qualified, e.input.RepoID, e.input.Path)
}

// handlerMethod returns the qualified name of the method a connect routes to,
// or "" when the argument proves nothing. Only a bare identifier that resolves
// to a method this script declares qualifies: a Callable bound to another
// object, a lambda, or a computed name names no method that is knowable here.
func (e *extractor) handlerMethod(arguments []gdast.Expression, index int, current scope) string {
	if index >= len(arguments) {
		return ""
	}
	identifier, ok := arguments[index].(*gdast.Identifier)
	if !ok {
		return ""
	}
	return e.methods[qualify(current.receiver, identifier.Name)]
}

// addSignalHandler names the method a literal connect routes a signal to. Only a
// bare identifier that resolves to a method this script declares is accepted: a
// Callable bound to another object, a lambda, or a computed name proves nothing
// about which method the engine will run, so nothing is recorded. An unresolved
// signal records no handler either, because there is no declaration to hang the
// route on and inventing one would attach the handler to a name rather than to a
// signal.
func (e *extractor) addSignalHandler(ref signalRef, arguments []gdast.Expression, index int, current scope, loc graph.Location) {
	if ref.id == "" {
		return
	}
	method := e.handlerMethod(arguments, index, current)
	if method == "" {
		return
	}
	e.b.AddFact(ref.id, graph.EdgeHandledBy, "", method, graph.KindMethod, loc,
		map[string]string{"form": "connect", "signal": ref.qualified, "receiver": current.receiver})
}

// addSignalFact records one signal fact and reports the declaration it resolved
// to. A returned ref with an empty id means the name matched no declaration in
// this script and the fact names it as an unresolved event, which is what keeps a
// signal whose owner is unknown visible without guessing an owner.
func (e *extractor) addSignalFact(fromID string, kind graph.EdgeKind, form, name string, current scope, loc graph.Location, extra map[string]string) (signalRef, bool) {
	if name == "" {
		return signalRef{}, false
	}
	name = strings.TrimPrefix(name, current.receiver+".")
	ref, ok := current.signals[name]
	if !ok {
		ref, ok = current.signals[qualify(current.receiver, name)]
	}
	properties := map[string]string{"form": form}
	for key, value := range extra {
		properties[key] = value
	}
	if !ok {
		properties["signal"] = name
		e.b.AddFact(fromID, kind, "", name, graph.KindEvent, loc, properties)
		return signalRef{}, true
	}
	properties["signal"] = ref.qualified
	e.b.AddFact(fromID, kind, ref.id, "", graph.KindEvent, loc, properties)
	return ref, true
}

func (e *extractor) referencedVariables(expression gdast.Expression, current scope) []string {
	if expression == nil {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	gdast.Inspect(expression, func(node gdast.Node) bool {
		expression, ok := node.(gdast.Expression)
		if !ok {
			return true
		}
		id := e.symbolID(expression, current)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

func (e *extractor) symbolID(expression gdast.Expression, current scope) string {
	switch node := expression.(type) {
	case *gdast.Identifier:
		return current.symbols[node.Name]
	case *gdast.MemberExpression:
		name := expressionName(node)
		if id := current.symbols[name]; id != "" {
			return id
		}
		if strings.HasPrefix(name, "self.") {
			return current.symbols[strings.TrimPrefix(name, "self.")]
		}
	}
	return ""
}

func (e *extractor) inferExpressionType(expression gdast.Expression, current scope) string {
	call, ok := expression.(*gdast.CallExpression)
	if !ok {
		if identifier, ok := expression.(*gdast.Identifier); ok {
			return current.types[identifier.Name]
		}
		return ""
	}
	callee := e.resolveCallee(call.Callee, current)
	if callee == "preload" || callee == "load" {
		if len(call.Arguments) > 0 {
			if resource, ok := literalString(call.Arguments[0]); ok {
				return e.resourceModule(resource)
			}
		}
		return ""
	}
	if strings.HasSuffix(callee, ".new") {
		return strings.TrimSuffix(callee, ".new")
	}
	if last := graph.SimpleName(callee); last != "" && unicode.IsUpper([]rune(last)[0]) {
		return callee
	}
	return ""
}

func (e *extractor) resolveTypeExpression(expression gdast.Expression, current scope) string {
	if call, ok := expression.(*gdast.CallExpression); ok {
		callee := e.resolveExpression(call.Callee, current)
		if (callee == "preload" || callee == "load") && len(call.Arguments) > 0 {
			if resource, ok := literalString(call.Arguments[0]); ok {
				return e.resourceModule(resource)
			}
		}
	}
	return e.resolveExpression(expression, current)
}

func (e *extractor) resolveType(typeName string, current scope) string {
	typeName = strings.TrimSpace(typeName)
	if typeName == "" || isBuiltinType(typeName) {
		return ""
	}
	if resolved := current.types[typeName]; resolved != "" {
		return resolved
	}
	return typeName
}

func (e *extractor) resolveCallee(expression gdast.Expression, current scope) string {
	if identifier, ok := expression.(*gdast.Identifier); ok {
		if method := e.methods[qualify(current.receiver, identifier.Name)]; method != "" {
			return method
		}
	}
	return e.resolveExpression(expression, current)
}

func (e *extractor) resolveExpression(expression gdast.Expression, current scope) string {
	switch node := expression.(type) {
	case *gdast.Identifier:
		if node.Name == "self" {
			return current.receiver
		}
		if resolved := current.types[node.Name]; resolved != "" {
			return resolved
		}
		return node.Name
	case *gdast.TypeExpression:
		return e.resolveType(node.Name, current)
	case *gdast.MemberExpression:
		object := e.resolveExpression(node.Object, current)
		if object == "" {
			return node.Property
		}
		return object + "." + node.Property
	case *gdast.CallExpression:
		return e.inferExpressionType(node, current)
	default:
		return expressionName(expression)
	}
}

func (e *extractor) location(node gdast.Node) graph.Location {
	span := node.Span()
	return graph.Location{Path: e.input.Path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
}

func expressionName(expression gdast.Expression) string {
	switch node := expression.(type) {
	case *gdast.Identifier:
		return node.Name
	case *gdast.TypeExpression:
		return node.Name
	case *gdast.MemberExpression:
		object := expressionName(node.Object)
		if object == "" {
			return node.Property
		}
		return object + "." + node.Property
	case *gdast.NodePathExpression:
		return node.Path
	default:
		return ""
	}
}

func literalString(expression gdast.Expression) (string, bool) {
	literal, ok := expression.(*gdast.Literal)
	if !ok || (literal.Kind != gdast.StringLiteral && literal.Kind != gdast.StringNameLiteral && literal.Kind != gdast.NodePathLiteral) {
		return "", false
	}
	raw := literal.Raw
	if literal.Kind == gdast.StringNameLiteral || literal.Kind == gdast.NodePathLiteral {
		raw = raw[1:]
	}
	quoteLength := 1
	if len(raw) >= 6 && raw[0] == raw[1] && raw[1] == raw[2] {
		quoteLength = 3
	}
	if len(raw) < quoteLength*2 || raw[0] != raw[len(raw)-1] {
		return "", false
	}
	body := raw[quoteLength : len(raw)-quoteLength]
	var value strings.Builder
	for body != "" {
		char, _, tail, err := strconv.UnquoteChar(body, raw[0])
		if err != nil {
			return "", false
		}
		value.WriteRune(char)
		body = tail
	}
	return value.String(), true
}

// resourceModule canonicalizes a res:// reference against the Godot project
// that owns this script, because such a reference is project-relative and a
// project can sit in any subdirectory of a repository. A reference that leaves
// its project, or one in a script whose project could not be read, resolves to
// nothing rather than to whatever repository path it lands on.
func (e *extractor) resourceModule(resource string) string {
	if !e.projectKnown {
		return ""
	}
	return e.project.Resolve(resource)
}

func formatParameters(parameters []gdast.Parameter) string {
	formatted := make([]string, 0, len(parameters))
	for _, parameter := range parameters {
		value := parameter.Name
		if parameter.Type != "" {
			value += ": " + parameter.Type
		}
		formatted = append(formatted, value)
	}
	return "(" + strings.Join(formatted, ", ") + ")"
}

func qualify(container, name string) string {
	if container == "" {
		return name
	}
	return container + "." + name
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneSignals(source map[string]signalRef) map[string]signalRef {
	result := make(map[string]signalRef, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func isBuiltinType(value string) bool {
	switch strings.TrimSpace(value) {
	case "bool", "int", "float", "String", "StringName", "NodePath", "Array", "Dictionary", "Variant", "void":
		return true
	default:
		return false
	}
}

// inputActionAPI is one Input, InputMap, or InputEvent entry point that names an
// input action in its arguments.
//
// The table is exact method names rather than a name pattern, because a project
// is free to declare is_action_bar_visible() on a control of its own and a
// pattern would read its argument as an action name. Arguments lists every
// position that holds an action name, which is how Input.get_axis and
// Input.get_vector contribute more than one.
type inputActionAPI struct {
	form      string
	arguments []int
	// requireInputReceiver restricts the entry point to a call on Input or
	// InputMap itself. It is set for the names a project could plausibly
	// declare on a type of its own (get_axis, has_action, add_action); the
	// engine-specific names are allowed on any receiver because they also
	// appear on InputEvent instances, where the receiver's type is not knowable
	// here.
	requireInputReceiver bool
}

var inputActionAPIs = map[string]inputActionAPI{
	"is_action_pressed":        {form: "query", arguments: []int{0}},
	"is_action_just_pressed":   {form: "query", arguments: []int{0}},
	"is_action_released":       {form: "query", arguments: []int{0}},
	"is_action_just_released":  {form: "query", arguments: []int{0}},
	"get_action_strength":      {form: "query", arguments: []int{0}},
	"get_action_raw_strength":  {form: "query", arguments: []int{0}},
	"action_press":             {form: "press", arguments: []int{0}},
	"action_release":           {form: "release", arguments: []int{0}},
	"action_get_deadzone":      {form: "configure", arguments: []int{0}},
	"action_set_deadzone":      {form: "configure", arguments: []int{0}},
	"action_get_events":        {form: "configure", arguments: []int{0}},
	"action_add_event":         {form: "configure", arguments: []int{0}},
	"action_erase_event":       {form: "configure", arguments: []int{0}},
	"action_erase_events":      {form: "configure", arguments: []int{0}},
	"action_has_event":         {form: "configure", arguments: []int{0}},
	"event_is_action":          {form: "query", arguments: []int{1}},
	"event_is_action_pressed":  {form: "query", arguments: []int{1}},
	"event_is_action_released": {form: "query", arguments: []int{1}},
	// is_action, is_action_pressed, is_action_released, and get_action_strength
	// are InputEvent methods as well as (for the latter three) Input methods, so
	// none of them may require an Input or InputMap receiver: a call on a typed
	// InputEvent is correct code and has to resolve.
	"is_action":    {form: "query", arguments: []int{0}},
	"has_action":   {form: "query", arguments: []int{0}, requireInputReceiver: true},
	"add_action":   {form: "declare", arguments: []int{0}, requireInputReceiver: true},
	"erase_action": {form: "remove", arguments: []int{0}, requireInputReceiver: true},
	"get_axis":     {form: "query", arguments: []int{0, 1}, requireInputReceiver: true},
	"get_vector":   {form: "query", arguments: []int{0, 1, 2, 3}, requireInputReceiver: true},
}

// groupAPI is one Node or SceneTree entry point that names a node group.
//
// Membership and use are separate relations because they answer different
// questions and point in different directions of meaning: add_to_group and
// remove_from_group both name a membership between a node and a group, while a
// lookup or a dispatch reads the group's members without asserting that the
// caller is one of them. is_in_group is deliberately a use: asking whether a
// node is in a group is not evidence that it is.
type groupAPI struct {
	kind graph.EdgeKind
	form string
	// group is the index of the group-name argument.
	group int
	// method is the index of the dispatched method name, or -1 when the form
	// dispatches nothing. The method is recorded as evidence only: which
	// object's method runs is decided by the group's membership at runtime, so
	// no handler is ever resolved from it.
	method int
}

var groupAPIs = map[string]groupAPI{
	"add_to_group":            {kind: graph.EdgeInGroup, form: "add", group: 0, method: -1},
	"remove_from_group":       {kind: graph.EdgeInGroup, form: "remove", group: 0, method: -1},
	"is_in_group":             {kind: graph.EdgeUsesGroup, form: "membership_test", group: 0, method: -1},
	"has_group":               {kind: graph.EdgeUsesGroup, form: "lookup", group: 0, method: -1},
	"get_nodes_in_group":      {kind: graph.EdgeUsesGroup, form: "lookup", group: 0, method: -1},
	"get_first_node_in_group": {kind: graph.EdgeUsesGroup, form: "lookup", group: 0, method: -1},
	"get_node_count_in_group": {kind: graph.EdgeUsesGroup, form: "lookup", group: 0, method: -1},
	"call_group":              {kind: graph.EdgeUsesGroup, form: "call", group: 0, method: 1},
	"call_group_flags":        {kind: graph.EdgeUsesGroup, form: "call", group: 1, method: 2},
	"notify_group":            {kind: graph.EdgeUsesGroup, form: "notify", group: 0, method: -1},
	"notify_group_flags":      {kind: graph.EdgeUsesGroup, form: "notify", group: 1, method: -1},
}

// addInputActionUses links a literal action name to the project-scoped action it
// names, and keeps the generic configuration read the config catalogs already
// had. A computed action name produces neither: the name is not knowable here,
// and matching it by partial text would attach the use to whichever declaration
// happened to share a substring.
//
// The configuration read is emitted even when the owning project could not be
// determined, because "input/<action>" is a repository-wide configuration key
// rather than a project-scoped identity. The typed action edge is not, so it is
// withheld until the owning project is known.
func (e *extractor) addInputActionUses(node *gdast.CallExpression, member *gdast.MemberExpression, fromID, method, receiver string, current scope) {
	api, ok := inputActionAPIs[method]
	if !ok {
		return
	}
	if api.requireInputReceiver && receiver != "Input" && receiver != "InputMap" {
		return
	}
	if member != nil && e.receiverRefusesInput(member.Object, current) {
		return
	}
	for _, index := range api.arguments {
		if index >= len(node.Arguments) {
			continue
		}
		argument := node.Arguments[index]
		action, ok := literalString(argument)
		if !ok || action == "" {
			continue
		}
		loc := e.location(argument)
		e.b.AddFact(fromID, graph.EdgeReadsConfig, "", godotid.InputSection+"/"+action,
			graph.KindConfigKey, loc, map[string]string{"source": godotid.ProjectFileName, "action": action})
		target := e.inputAction(action)
		if target == "" {
			continue
		}
		properties := map[string]string{"form": api.form, "action": action, "call": method}
		if receiver != "" {
			properties["receiver"] = receiver
		}
		e.b.AddFact(fromID, graph.EdgeUsesInputAction, "", target, graph.KindGodotInputAction, loc,
			properties)
	}
}

// receiverRefusesInput reports whether a call's receiver is typed as something
// that provably is not an input class, in which case the call is not a Godot
// input query at all and neither the typed action edge nor the compatibility
// configuration read may be emitted. A project is free to declare its own
// is_action or is_action_pressed on a class of its own, and reading that call's
// argument as an action name fabricates an edge.
//
// The boundary is deliberate and asymmetric. Refusal requires the receiver's type
// to be KNOWN; a receiver whose type is unknown keeps emitting. Untyped
// parameters are pervasive in GDScript - func _input(event): is the common idiom -
// so requiring a known type would silently destroy most real recall, which is the
// opposite failure and a worse one. Unknown is therefore not refusal here; it is
// the pre-existing behaviour of every unrestricted entry in the table.
func (e *extractor) receiverRefusesInput(object gdast.Expression, current scope) bool {
	typeName, known := e.receiverType(object, current)
	if !known {
		return false
	}
	return !e.isInputReceiverType(typeName)
}

// receiverType returns the declared type of a call's receiver and whether this
// parser knows it. "self" is this script's own class, and a typed local symbol
// resolves through the scope's type table. Anything else - an untyped parameter, a
// chained expression, a call result - is unknown.
func (e *extractor) receiverType(object gdast.Expression, current scope) (string, bool) {
	switch node := object.(type) {
	case *gdast.Identifier:
		if node.Name == "self" {
			return current.receiver, current.receiver != ""
		}
		resolved := current.types[node.Name]
		return resolved, resolved != ""
	case *gdast.TypeExpression:
		if resolved := current.types[node.Name]; resolved != "" {
			return resolved, true
		}
		return node.Name, node.Name != ""
	default:
		return "", false
	}
}

// isInputReceiverType reports whether a type name is one of Godot's input
// classes, following the extends chain this file declares so a locally declared
// subclass of an input class is recognized too.
func (e *extractor) isInputReceiverType(typeName string) bool {
	// A malformed or cyclic extends chain must terminate rather than spin.
	for depth := 0; typeName != "" && depth < 32; depth++ {
		if isInputClassName(typeName) {
			return true
		}
		next, ok := e.bases[typeName]
		if !ok {
			// The name is known but its hierarchy is not, which happens for any
			// class another file declares. Nothing further can be established.
			return false
		}
		typeName = next
	}
	return false
}

// isInputClassName reports whether a name is one of Godot's own input classes.
// Every engine class in the InputEvent hierarchy - InputEventKey,
// InputEventMouseButton, InputEventJoypadMotion and the rest - is named with that
// prefix, so matching it covers the hierarchy without restating it. Matching by
// name can only ever widen compatibility and never cause a refusal, so a project
// class that happens to share the prefix costs nothing beyond the behaviour an
// unrestricted entry already has.
func isInputClassName(name string) bool {
	switch name {
	case "Input", "InputMap":
		return true
	}
	return strings.HasPrefix(name, "InputEvent")
}

// addGroupOperations links a literal group name to the project-scoped group it
// names. A computed group name produces nothing, and a dispatch keeps its method
// as evidence without ever resolving a handler: which object's method runs is
// decided by the group's runtime membership, so naming one would be a guess even
// when the method name is a literal.
func (e *extractor) addGroupOperations(node *gdast.CallExpression, fromID, method, receiver string) {
	api, ok := groupAPIs[method]
	if !ok || api.group >= len(node.Arguments) {
		return
	}
	argument := node.Arguments[api.group]
	group, ok := literalString(argument)
	if !ok || group == "" {
		return
	}
	target := e.nodeGroup(group)
	if target == "" {
		return
	}
	properties := map[string]string{"form": api.form, "group": group, "call": method}
	if receiver != "" {
		properties["receiver"] = receiver
	}
	if api.method >= 0 {
		if api.method < len(node.Arguments) {
			if dispatched, ok := literalString(node.Arguments[api.method]); ok && dispatched != "" {
				properties["method"] = dispatched
			} else {
				properties["method_dynamic"] = "true"
			}
		} else {
			properties["method_dynamic"] = "true"
		}
	}
	e.b.AddFact(fromID, api.kind, "", target, graph.KindGodotNodeGroup, e.location(argument), properties)
}

// localCall reports whether a call resolved to a method this script declares.
// Such a call is that method and never an engine entry point that happens to
// share its name, which is what keeps a project's own add_to_group() or
// is_action_pressed() out of the Godot interaction vocabulary.
//
// Both spellings of a call on this object count. A bare name is one
// (is_action_pressed(...)); a member call whose receiver is this object is the
// other (self.is_action_pressed(...), or the script's own class name). Checking
// only the bare form would let the qualified spelling of the very same local
// method produce a resolved input-action or group edge for a Godot API the
// script never called, which is a guessed edge.
func (e *extractor) localCall(callee gdast.Expression, current scope) bool {
	switch node := callee.(type) {
	case *gdast.Identifier:
		return e.methods[qualify(current.receiver, node.Name)] != ""
	case *gdast.MemberExpression:
		if !e.namesThisObject(node.Object, current) {
			return false
		}
		return e.methods[qualify(current.receiver, node.Property)] != ""
	default:
		return false
	}
}

// namesThisObject reports whether an expression names the object whose script
// this is: "self", or the script's own class. Any other receiver is a different
// object whose type this parser cannot resolve, so a method call on it is not
// evidence of a local method.
func (e *extractor) namesThisObject(expression gdast.Expression, current scope) bool {
	identifier, ok := expression.(*gdast.Identifier)
	if !ok {
		return false
	}
	if identifier.Name == "self" {
		return true
	}
	// A bare class name resolves through the type table to this script's own
	// qualified name only when it is this script's class.
	return current.types[identifier.Name] == current.receiver && current.receiver != ""
}

// inputAction returns the identity an action name has inside the Godot project
// that owns this script, or "" when that project could not be read. Action names
// are a per-project namespace, so an unscoped identity would merge the actions of
// every project in a monorepo; an unknown project therefore resolves nothing
// rather than resolving against an assumed root.
func (e *extractor) inputAction(name string) string {
	if !e.projectKnown || strings.TrimSpace(name) == "" {
		return ""
	}
	return e.project.InputActionQualifiedName(name)
}

// nodeGroup returns the identity a group name has inside the Godot project that
// owns this script, refusing an unknown project for the same reason inputAction
// does.
func (e *extractor) nodeGroup(name string) string {
	if !e.projectKnown || strings.TrimSpace(name) == "" {
		return ""
	}
	return e.project.NodeGroupQualifiedName(name)
}
