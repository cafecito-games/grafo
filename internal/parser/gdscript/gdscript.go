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
		methods: map[string]string{}, autoloads: map[gdast.Node]bool{}, projectKnown: true}
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
			current.types[node.Name] = qualify(current.container, node.Name)
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
	if node.Name != "extends" && !(node.Name == "class_name" && node.Extends != nil) {
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
	if strings.HasPrefix(callee, "Input.") || strings.HasPrefix(callee, "InputMap.") {
		for _, argument := range node.Arguments {
			if action, ok := literalString(argument); ok && action != "" {
				e.b.AddFact(fromID, graph.EdgeReadsConfig, "", "input/"+action, graph.KindConfigKey, loc,
					map[string]string{"source": "project.godot"})
			}
		}
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
			e.addSignalFact(fromID, graph.EdgePublishes, name, current, loc)
			return
		}
	}
	if member, ok := node.Callee.(*gdast.MemberExpression); ok {
		signalName := e.resolveExpression(member.Object, current)
		switch method {
		case "emit":
			if e.addSignalFact(fromID, graph.EdgePublishes, signalName, current, loc) {
				return
			}
		case "connect":
			if e.addSignalFact(fromID, graph.EdgeSubscribes, signalName, current, loc) {
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

func (e *extractor) addSignalFact(fromID string, kind graph.EdgeKind, name string, current scope, loc graph.Location) bool {
	if name == "" {
		return false
	}
	name = strings.TrimPrefix(name, current.receiver+".")
	ref, ok := current.signals[name]
	if !ok {
		ref, ok = current.signals[qualify(current.receiver, name)]
	}
	if !ok {
		e.b.AddFact(fromID, kind, "", name, graph.KindEvent, loc, map[string]string{"signal": name})
		return true
	}
	e.b.AddFact(fromID, kind, ref.id, "", graph.KindEvent, loc, map[string]string{"signal": ref.qualified})
	return true
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
