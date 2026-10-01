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
	"github.com/cafecito-games/grafo/internal/httpmodel"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/calleffect"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
	"github.com/cafecito-games/grafo/internal/parser/protobufbinding"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

// Parser extracts semantic graph nodes and relationships from Godot 4
// GDScript source files.
type Parser struct{ bindings *protobufbinding.Loader }

func New() *Parser { return &Parser{bindings: protobufbinding.NewLoader()} }
func NewWithBindingLoader(loader *protobufbinding.Loader) *Parser {
	if loader == nil {
		loader = protobufbinding.NewLoader()
	}
	return &Parser{bindings: loader}
}
func (*Parser) Language() string { return graph.ProducerGDScript }

func (*Parser) SemanticDependencies() []string { return []string{projectconfig.FileName} }

func (*Parser) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".gd")
}

// SemanticKey makes the owning Godot project, configured HTTP adapters, and
// generated binding vocabulary part of the incremental cache key.
func (p *Parser) SemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	project, err := godotid.LoadProject(input.Root, input.Path, input.SourcePaths)
	if err != nil {
		return "", err
	}
	key := "gdscript-semantic-v4:" + project.SemanticKey()
	configuration, err := projectconfig.Load(input.Root)
	if err != nil {
		return "", err
	}
	key += ":" + configuration.Adapters.SemanticKey()
	key += ":" + configuration.Tests.SemanticKey()
	if p.bindings != nil {
		bindingKey, bindingErr := p.bindings.SemanticKey(ctx, input)
		if bindingErr != nil {
			return "", bindingErr
		}
		key += ":" + bindingKey
	}
	return key, nil
}

// SemanticAffectedPaths reparses every script under a Godot project whose
// project.godot changed. SemanticDependencies matches only repository-root
// paths, and a Godot project can sit in any subdirectory of a monorepo.
func (*Parser) SemanticAffectedPaths(allPaths, changedPaths []string) []string {
	var roots []string
	bindingChanged := false
	for _, path := range changedPaths {
		if protobufbinding.IsSemanticInput(path) {
			bindingChanged = true
		}
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
	if len(roots) == 0 && !bindingChanged {
		return nil
	}
	var affected []string
	for _, path := range allPaths {
		if !strings.EqualFold(filepath.Ext(path), ".gd") {
			continue
		}
		if bindingChanged {
			affected = append(affected, path)
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
	// fields retains class-member types separately from shadowable locals and
	// parameters so self.<field> always resolves the field declaration.
	fields       map[string]string
	fieldSymbols map[string]string
	fieldLocked  map[string]bool
	// locked marks declarations whose explicit annotation controls their type.
	// In particular, a Variant must not become a generated message merely
	// because its initializer happens to be one.
	locked                 map[string]bool
	signals                map[string]signalRef
	transportConstants     map[string]string
	transportPayloads      map[string]protobufAPI
	transportFieldPayloads map[string]protobufAPI
	transportReceives      map[string]string
	values                 map[string]string
	fieldValues            map[string]string
	testBase               string
}

type protobufAPI struct {
	bindingID  string
	targetID   string
	target     string
	targetKind graph.NodeKind
	form       string
	returns    string
	symbol     string
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
	bases              map[string]string
	protobufAPIs       map[string]protobufAPI
	protobufAmbiguous  map[string]bool
	protobufTypes      map[string]bool
	protobufEnabled    bool
	protobufWarned     map[string]bool
	transportSummaries map[string][]gdTransportTemplate
	callEffects        calleffect.Registry
	adapterWarned      map[string]bool
	testBases          map[string]bool
}

func (p *Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "gdscript")
	configuration, err := projectconfig.Load(input.Root)
	if err != nil {
		return b.Finish(), err
	}
	if configuration.Tests.Invalid != "" {
		b.Diagnostic(1, "warning", "invalid test configuration; using built-in GDScript test bases only: "+configuration.Tests.Invalid)
	}
	registry := protobufbinding.Registry{}
	protobufEnabled := true
	if p.bindings != nil && input.Root != "" {
		loaded, bindingErr := p.bindings.Load(ctx, input)
		if bindingErr != nil {
			b.Diagnostic(0, "warning", "load Protobuf binding registry: "+bindingErr.Error())
		} else if generated, ok, reason := loaded.GeneratedFile(input.Path, "gdscript", input.Content); ok {
			b.Result.Nodes[0].Properties = map[string]string{
				"generated": "true", "generator": generated.Generator,
				"generator_version": generated.Version, "source_proto": generated.Source,
			}
			return b.Finish(), nil
		} else if reason != "" {
			b.Diagnostic(1, "warning", "Protobuf generated-file provenance rejected: "+reason)
		}
		registry = loaded
		protobufEnabled = !registry.ConfiguredOutput(input.Path, "gdscript")
	}
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
	protobufAPIs, protobufAmbiguous, protobufTypes := gdscriptProtobufBindings(registry)
	e := &extractor{b: b, input: input, module: parserapi.ModuleName(input.Path),
		methods: map[string]string{}, autoloads: map[gdast.Node]bool{},
		projectKnown: true, bases: map[string]string{}, protobufAPIs: protobufAPIs,
		protobufAmbiguous: protobufAmbiguous, protobufTypes: protobufTypes,
		protobufEnabled: protobufEnabled, protobufWarned: map[string]bool{},
		transportSummaries: map[string][]gdTransportTemplate{}, callEffects: configuration.Adapters,
		adapterWarned: map[string]bool{}, testBases: map[string]bool{"GutTest": true}}
	if configuration.Tests.Invalid == "" {
		for _, base := range configuration.Tests.GDScriptBases {
			e.testBases[base] = true
		}
	}
	project, projectErr := godotid.LoadProject(input.Root, input.Path, input.SourcePaths)
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

func gdscriptProtobufBindings(registry protobufbinding.Registry) (map[string]protobufAPI, map[string]bool, map[string]bool) {
	apis := map[string]protobufAPI{}
	ambiguous := map[string]bool{}
	types := map[string]bool{}
	typeTargets := map[string]string{}
	for _, config := range registry.Configs {
		for _, projection := range config.Projections {
			if projection.Node.Language != "gdscript" {
				continue
			}
			symbol := projection.Node.QualifiedName
			if projection.Node.Kind == graph.KindClass && projection.Properties["projection"] == "message" {
				if previous, ok := typeTargets[symbol]; ok && previous != projection.CanonicalID {
					delete(types, symbol)
					ambiguous[symbol] = true
				} else if !ambiguous[symbol] {
					typeTargets[symbol] = projection.CanonicalID
					types[symbol] = true
				}
			}
			if projection.Node.Kind != graph.KindMethod {
				continue
			}
			candidate := protobufAPI{bindingID: projection.Node.ID, targetID: projection.CanonicalID,
				target: projection.Canonical, targetKind: projection.CanonicalKind,
				form: projection.Properties["accessor"], returns: projection.Properties["returns"], symbol: symbol}
			if candidate.form == "" {
				candidate.form = projection.Properties["codec"]
			}
			if previous, ok := apis[symbol]; ok && (previous.targetID != candidate.targetID || previous.form != candidate.form || previous.returns != candidate.returns) {
				delete(apis, symbol)
				ambiguous[symbol] = true
			} else if !ambiguous[symbol] {
				apis[symbol] = candidate
			}
		}
	}
	// A projected class is the authority for every member beneath it. Even when
	// a field exists in only one colliding schema, its receiver is still not
	// proven, so cascade class ambiguity to all generated member APIs.
	for symbol := range apis {
		owner := strings.TrimSuffix(symbol, "."+graph.SimpleName(symbol))
		if ambiguous[owner] {
			delete(apis, symbol)
			ambiguous[symbol] = true
		}
	}
	return apis, ambiguous, types
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
	hierarchy := scope{container: qualified, receiver: qualified, types: map[string]string{className: qualified}}
	// Register the complete same-file class hierarchy before classifying any
	// test scope. GDScript permits a script or inner class to extend a class
	// declared later in the file, so a source-order walk would miss that chain.
	e.prepareClassHierarchy(file.Statements, hierarchy)
	base := fileBase(file.Statements)
	if resolved := e.resolveType(base, hierarchy); resolved != "" {
		base = resolved
	}
	if base != "" {
		e.bases[qualified] = base
	}
	testBase := e.recognizedTestBase(base)
	if testBase != "" {
		if properties == nil {
			properties = map[string]string{}
		}
		properties["test_role"] = "scope"
		properties["test_framework"] = "gdscript"
		properties["test_base"] = testBase
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
		classID: classID, classBody: true, symbols: map[string]string{}, types: map[string]string{}, fields: map[string]string{},
		fieldSymbols: map[string]string{}, fieldLocked: map[string]bool{}, locked: map[string]bool{}, signals: map[string]signalRef{},
		transportConstants: map[string]string{}, transportPayloads: map[string]protobufAPI{},
		transportFieldPayloads: map[string]protobufAPI{}, transportReceives: map[string]string{},
		values: map[string]string{}, fieldValues: map[string]string{}, testBase: testBase}
	root.types[className] = qualified
	// Register inner class names before resolving the file base, because a script
	// may extend a class declared later in the same file. Resolve that base before
	// field inference so bare inherited generated APIs such as from_bytes remain
	// available to forward-declared fields without bypassing inner-class shadows.
	e.prepareClassSymbols(file.Statements, root)
	e.prepareClassFields(file.Statements, root)
	e.prepareTransportSummaries(file.Statements, root)
	e.walkStatements(file.Statements, root)
}

func (e *extractor) prepareClassHierarchy(statements []gdast.Statement, current scope) {
	for _, statement := range statements {
		if node, ok := statement.(*gdast.ClassDeclaration); ok {
			current.types[node.Name] = qualify(current.container, node.Name)
		}
	}
	for _, statement := range statements {
		node, ok := statement.(*gdast.ClassDeclaration)
		if !ok {
			continue
		}
		qualified := qualify(current.container, node.Name)
		if base := e.resolveType(node.Extends, current); base != "" {
			e.bases[qualified] = base
		}
		inner := scope{container: qualified, receiver: qualified, types: cloneMap(current.types)}
		inner.types[node.Name] = qualified
		e.prepareClassHierarchy(node.Body, inner)
	}
}

func fileBase(statements []gdast.Statement) string {
	base := ""
	for _, statement := range statements {
		directive, ok := statement.(*gdast.Directive)
		if !ok {
			continue
		}
		switch {
		case directive.Name == "extends":
			base = expressionName(directive.Value)
		case directive.Name == "class_name" && directive.Extends != nil:
			base = expressionName(directive.Extends)
		}
	}
	return base
}

func (e *extractor) prepareClass(statements []gdast.Statement, current scope) {
	e.prepareClassSymbols(statements, current)
	e.prepareClassFields(statements, current)
}

func (e *extractor) prepareClassSymbols(statements []gdast.Statement, current scope) {
	for _, statement := range statements {
		switch node := statement.(type) {
		case *gdast.VariableDeclaration:
			value, valueOK := e.scalarString(node.Value, current)
			qualified := qualify(current.container, node.Name)
			fieldID := graph.NodeID(graph.KindField, qualified, e.input.RepoID, e.input.Path)
			current.symbols[node.Name] = fieldID
			current.fieldSymbols[node.Name] = fieldID
			current.fieldLocked[node.Name] = node.Type != ""
			delete(current.types, node.Name)
			delete(current.fields, node.Name)
			if resolved := e.resolveType(node.Type, current); resolved != "" {
				current.types[node.Name] = resolved
				current.fields[node.Name] = resolved
			}
			current.locked[node.Name] = node.Type != ""
			if node.Constant && valueOK {
				current.values[node.Name] = value
				current.fieldValues[node.Name] = value
			} else {
				delete(current.values, node.Name)
				delete(current.fieldValues, node.Name)
			}
			if node.Constant {
				if value, status := e.transportConstant(node.Value, current); status == "proven" {
					current.transportConstants[node.Name] = value
				}
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
		case *gdast.EnumDeclaration:
			if node.Name != "" {
				current.types[node.Name] = qualify(current.container, node.Name)
			}
		}
	}
}

func (e *extractor) prepareClassFields(statements []gdast.Statement, current scope) {
	// Establish every declared inner-class base before inferring fields so a
	// forward initializer can follow the complete local inheritance chain.
	for _, statement := range statements {
		node, ok := statement.(*gdast.ClassDeclaration)
		if ok && node.Extends != "" {
			qualified := qualify(current.container, node.Name)
			e.bases[qualified] = e.resolveType(node.Extends, current)
		}
	}
	for _, statement := range statements {
		node, ok := statement.(*gdast.VariableDeclaration)
		if !ok || node.Type != "" || current.types[node.Name] != "" {
			continue
		}
		if inferred := e.inferExpressionType(node.Value, current); inferred != "" {
			current.types[node.Name] = inferred
			current.fields[node.Name] = inferred
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
		for _, annotation := range gdast.Annotations(statement) {
			annotations = append(annotations, annotation.Name)
			for _, argument := range annotation.Arguments {
				e.walkExpression(argument, current)
			}
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
		baseline := current
		baseline.symbols = cloneMap(current.symbols)
		baseline.types = cloneMap(current.types)
		baseline.fields = cloneMap(current.fields)
		baseline.fieldSymbols = cloneMap(current.fieldSymbols)
		baseline.fieldLocked = cloneBoolMap(current.fieldLocked)
		baseline.locked = cloneBoolMap(current.locked)
		baseline.transportConstants = cloneMap(current.transportConstants)
		baseline.transportPayloads = cloneProtobufAPIMap(current.transportPayloads)
		baseline.transportFieldPayloads = cloneProtobufAPIMap(current.transportFieldPayloads)
		baseline.transportReceives = cloneMap(current.transportReceives)
		baseline.values = cloneMap(current.values)
		baseline.fieldValues = cloneMap(current.fieldValues)
		var alternatives, fieldAlternatives []map[string]string
		var valueAlternatives, fieldValueAlternatives []map[string]string
		var payloadAlternatives, fieldPayloadAlternatives []map[string]protobufAPI
		var receiveAlternatives []map[string]string
		for _, branch := range node.Branches {
			e.walkExpression(branch.Condition, current)
			branchScope := cloneFlowScope(baseline)
			e.walkStatements(branch.Body, branchScope)
			alternatives = append(alternatives, branchScope.types)
			fieldAlternatives = append(fieldAlternatives, branchScope.fields)
			valueAlternatives = append(valueAlternatives, branchScope.values)
			fieldValueAlternatives = append(fieldValueAlternatives, branchScope.fieldValues)
			payloadAlternatives = append(payloadAlternatives, branchScope.transportPayloads)
			fieldPayloadAlternatives = append(fieldPayloadAlternatives, branchScope.transportFieldPayloads)
			receiveAlternatives = append(receiveAlternatives, branchScope.transportReceives)
		}
		if len(node.Else) == 0 {
			alternatives = append(alternatives, baseline.types)
			fieldAlternatives = append(fieldAlternatives, baseline.fields)
			valueAlternatives = append(valueAlternatives, baseline.values)
			fieldValueAlternatives = append(fieldValueAlternatives, baseline.fieldValues)
			payloadAlternatives = append(payloadAlternatives, baseline.transportPayloads)
			fieldPayloadAlternatives = append(fieldPayloadAlternatives, baseline.transportFieldPayloads)
			receiveAlternatives = append(receiveAlternatives, baseline.transportReceives)
		} else {
			elseScope := cloneFlowScope(baseline)
			e.walkStatements(node.Else, elseScope)
			alternatives = append(alternatives, elseScope.types)
			fieldAlternatives = append(fieldAlternatives, elseScope.fields)
			valueAlternatives = append(valueAlternatives, elseScope.values)
			fieldValueAlternatives = append(fieldValueAlternatives, elseScope.fieldValues)
			payloadAlternatives = append(payloadAlternatives, elseScope.transportPayloads)
			fieldPayloadAlternatives = append(fieldPayloadAlternatives, elseScope.transportFieldPayloads)
			receiveAlternatives = append(receiveAlternatives, elseScope.transportReceives)
		}
		mergeFlowTypes(current, alternatives)
		mergeFlowFields(current, fieldAlternatives)
		mergeFlowStrings(current.values, valueAlternatives)
		mergeFlowStrings(current.fieldValues, fieldValueAlternatives)
		mergeFlowPayloads(current.transportPayloads, payloadAlternatives)
		mergeFlowPayloads(current.transportFieldPayloads, fieldPayloadAlternatives)
		mergeFlowStrings(current.transportReceives, receiveAlternatives)
	case *gdast.WhileStatement:
		e.walkExpression(node.Condition, current)
		bodyScope := cloneFlowScope(current)
		e.walkStatements(node.Body, bodyScope)
		mergeFlowTypes(current, []map[string]string{current.types, bodyScope.types})
		mergeFlowFields(current, []map[string]string{current.fields, bodyScope.fields})
		mergeFlowStrings(current.values, []map[string]string{current.values, bodyScope.values})
		mergeFlowStrings(current.fieldValues, []map[string]string{current.fieldValues, bodyScope.fieldValues})
		mergeFlowPayloads(current.transportPayloads, []map[string]protobufAPI{current.transportPayloads, bodyScope.transportPayloads})
		mergeFlowPayloads(current.transportFieldPayloads, []map[string]protobufAPI{current.transportFieldPayloads, bodyScope.transportFieldPayloads})
		mergeFlowStrings(current.transportReceives, []map[string]string{current.transportReceives, bodyScope.transportReceives})
	case *gdast.ForStatement:
		e.walkExpression(node.Iterable, current)
		bodyScope := cloneFlowScope(current)
		e.declareLocal(node.Variable, node.Type, e.location(node), bodyScope)
		e.walkStatements(node.Body, bodyScope)
		mergeFlowTypes(current, []map[string]string{current.types, bodyScope.types})
		mergeFlowFields(current, []map[string]string{current.fields, bodyScope.fields})
		mergeFlowStrings(current.values, []map[string]string{current.values, bodyScope.values})
		mergeFlowStrings(current.fieldValues, []map[string]string{current.fieldValues, bodyScope.fieldValues})
		mergeFlowPayloads(current.transportPayloads, []map[string]protobufAPI{current.transportPayloads, bodyScope.transportPayloads})
		mergeFlowPayloads(current.transportFieldPayloads, []map[string]protobufAPI{current.transportFieldPayloads, bodyScope.transportFieldPayloads})
		mergeFlowStrings(current.transportReceives, []map[string]string{current.transportReceives, bodyScope.transportReceives})
	case *gdast.MatchStatement:
		e.walkExpression(node.Value, current)
		var alternatives, fieldAlternatives []map[string]string
		var valueAlternatives, fieldValueAlternatives []map[string]string
		var payloadAlternatives, fieldPayloadAlternatives []map[string]protobufAPI
		var receiveAlternatives []map[string]string
		exhaustive := false
		for _, matchCase := range node.Cases {
			caseScope := cloneFlowScope(current)
			for _, pattern := range matchCase.Patterns {
				e.walkExpression(pattern, caseScope)
				if matchCase.Guard != nil {
					continue
				}
				switch pattern.(type) {
				case *gdast.WildcardPattern, *gdast.BindingPattern:
					exhaustive = true
				}
			}
			e.walkExpression(matchCase.Guard, caseScope)
			e.walkStatements(matchCase.Body, caseScope)
			alternatives = append(alternatives, caseScope.types)
			fieldAlternatives = append(fieldAlternatives, caseScope.fields)
			valueAlternatives = append(valueAlternatives, caseScope.values)
			fieldValueAlternatives = append(fieldValueAlternatives, caseScope.fieldValues)
			payloadAlternatives = append(payloadAlternatives, caseScope.transportPayloads)
			fieldPayloadAlternatives = append(fieldPayloadAlternatives, caseScope.transportFieldPayloads)
			receiveAlternatives = append(receiveAlternatives, caseScope.transportReceives)
		}
		if !exhaustive {
			// A non-exhaustive match may have no applicable arm. Keep the incoming
			// state as an alternative rather than claiming branch-only evidence.
			alternatives = append(alternatives, current.types)
			fieldAlternatives = append(fieldAlternatives, current.fields)
			valueAlternatives = append(valueAlternatives, current.values)
			fieldValueAlternatives = append(fieldValueAlternatives, current.fieldValues)
			payloadAlternatives = append(payloadAlternatives, current.transportPayloads)
			fieldPayloadAlternatives = append(fieldPayloadAlternatives, current.transportFieldPayloads)
			receiveAlternatives = append(receiveAlternatives, current.transportReceives)
		}
		mergeFlowTypes(current, alternatives)
		mergeFlowFields(current, fieldAlternatives)
		mergeFlowStrings(current.values, valueAlternatives)
		mergeFlowStrings(current.fieldValues, fieldValueAlternatives)
		mergeFlowPayloads(current.transportPayloads, payloadAlternatives)
		mergeFlowPayloads(current.transportFieldPayloads, fieldPayloadAlternatives)
		mergeFlowStrings(current.transportReceives, receiveAlternatives)
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
	kind := graph.KindMethod
	if current.testBase != "" {
		properties["test_framework"] = "gdscript"
		properties["test_base"] = current.testBase
		switch {
		case strings.HasPrefix(node.Name, "test_") && len(node.Name) > len("test_"):
			kind = graph.KindTest
			properties["test_subtype"] = "test"
		case gdscriptLifecycleMethods[node.Name]:
			properties["test_role"] = "lifecycle"
			properties["test_lifecycle"] = node.Name
		default:
			properties["test_role"] = "helper"
		}
	}
	id := e.b.Declare(current.parentID, graph.Node{Kind: kind, Name: node.Name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	functionScope := scope{currentID: id, parentID: id, container: qualified, receiver: current.receiver,
		classID: current.classID, symbols: cloneMap(current.symbols), types: cloneMap(current.types), fields: cloneMap(current.fields),
		fieldSymbols: cloneMap(current.fieldSymbols), fieldLocked: cloneBoolMap(current.fieldLocked), locked: cloneBoolMap(current.locked), signals: cloneSignals(current.signals),
		transportConstants: cloneMap(current.transportConstants), transportPayloads: cloneProtobufAPIMap(current.transportPayloads),
		transportFieldPayloads: cloneProtobufAPIMap(current.transportFieldPayloads), transportReceives: cloneMap(current.transportReceives),
		values: cloneMap(current.values), fieldValues: cloneMap(current.fieldValues), testBase: current.testBase}
	for _, parameter := range node.Parameters {
		parameterProperties := map[string]string{}
		if parameter.Type != "" {
			parameterProperties["type"] = parameter.Type
		}
		parameterID := e.b.Declare(id, graph.Node{Kind: graph.KindParameter, Name: parameter.Name,
			QualifiedName: qualified + "." + parameter.Name, Location: e.location(node), Properties: parameterProperties})
		functionScope.symbols[parameter.Name] = parameterID
		functionScope.locked[parameter.Name] = parameter.Type != ""
		delete(functionScope.types, parameter.Name)
		delete(functionScope.values, parameter.Name)
		clearLocalTransportEvidence(parameter.Name, functionScope)
		if resolved := e.resolveType(parameter.Type, current); resolved != "" {
			functionScope.types[parameter.Name] = resolved
		}
		e.walkExpression(parameter.Default, functionScope)
	}
	e.walkStatements(node.Body, functionScope)
}

func (e *extractor) parseClass(node *gdast.ClassDeclaration, current scope) {
	qualified := qualify(current.container, node.Name)
	base := ""
	if node.Extends != "" {
		base = e.resolveType(node.Extends, current)
		e.bases[qualified] = base
	}
	testBase := e.recognizedTestBase(base)
	properties := map[string]string(nil)
	if testBase != "" {
		properties = map[string]string{"test_role": "scope", "test_framework": "gdscript", "test_base": testBase}
	}
	id := e.b.Declare(current.parentID, graph.Node{Kind: graph.KindClass, Name: node.Name,
		QualifiedName: qualified, Location: e.location(node), Properties: properties})
	if base != "" {
		e.b.AddFact(id, graph.EdgeExtends, "", base, graph.KindClass, e.location(node), nil)
	}
	inner := scope{currentID: id, parentID: id, container: qualified, receiver: qualified, classID: id,
		classBody: true, symbols: map[string]string{}, types: cloneMap(current.types), fields: map[string]string{},
		fieldSymbols: map[string]string{}, fieldLocked: map[string]bool{}, locked: cloneBoolMap(current.locked), signals: map[string]signalRef{},
		transportConstants: cloneMap(current.transportConstants), transportPayloads: map[string]protobufAPI{},
		transportFieldPayloads: map[string]protobufAPI{}, transportReceives: map[string]string{},
		values: cloneMap(current.values), fieldValues: map[string]string{}, testBase: testBase}
	inner.types[node.Name] = qualified
	e.prepareClass(node.Body, inner)
	e.prepareTransportSummaries(node.Body, inner)
	e.walkStatements(node.Body, inner)
}

var gdscriptLifecycleMethods = map[string]bool{
	"before_all": true, "before_each": true, "after_each": true, "after_all": true,
}

func (e *extractor) recognizedTestBase(base string) string {
	seen := map[string]bool{}
	for depth := 0; base != "" && depth < 32 && !seen[base]; depth++ {
		seen[base] = true
		if e.testBases[base] {
			return base
		}
		base = e.bases[base]
	}
	return ""
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
	constant, constantStatus := e.transportConstant(node.Value, current)
	value, valueOK := e.scalarString(node.Value, current)
	current.symbols[node.Name] = id
	current.locked[node.Name] = node.Type != ""
	delete(current.types, node.Name)
	if valueOK {
		current.values[node.Name] = value
	} else {
		delete(current.values, node.Name)
	}
	clearLocalTransportEvidence(node.Name, current)
	if node.Constant && constantStatus == "proven" {
		current.transportConstants[node.Name] = constant
	}
	if current.classBody {
		current.fieldSymbols[node.Name] = id
		current.fieldLocked[node.Name] = node.Type != ""
		if valueOK {
			current.fieldValues[node.Name] = value
		} else {
			delete(current.fieldValues, node.Name)
		}
	}
	e.bindTransportVariable(node.Name, node.Value, current, current.classBody)
	if resolved := e.resolveType(node.Type, current); resolved != "" {
		current.types[node.Name] = resolved
		if current.classBody {
			current.fields[node.Name] = resolved
		}
	} else if node.Type == "" {
		if inferred := e.inferExpressionType(node.Value, current); inferred != "" {
			current.types[node.Name] = inferred
			if current.classBody {
				current.fields[node.Name] = inferred
			}
		}
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
	current.locked[name] = typeName != ""
	delete(current.types, name)
	delete(current.values, name)
	clearLocalTransportEvidence(name, current)
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
	targetName, selfField := "", false
	if target, ok := node.Target.(*gdast.Identifier); ok {
		targetName = target.Name
	} else if target, ok := node.Target.(*gdast.MemberExpression); ok {
		if object, ok := target.Object.(*gdast.Identifier); ok && object.Name == "self" {
			targetName, selfField = target.Property, true
		}
	}
	if targetName != "" && (node.Operator == "" || node.Operator == "=") {
		value, valueOK := e.scalarString(node.Value, current)
		inferred := e.inferExpressionType(node.Value, current)
		fieldID, isField := current.fieldSymbols[targetName]
		actualField := isField && (selfField || current.symbols[targetName] == fieldID)
		updatesBareType := !selfField || actualField && current.symbols[targetName] == fieldID
		if updatesBareType && !current.locked[targetName] {
			setInferredType(current.types, targetName, inferred)
		}
		if updatesBareType {
			setScalarValue(current.values, targetName, value, valueOK)
		}
		if actualField && !current.fieldLocked[targetName] {
			setInferredType(current.fields, targetName, inferred)
		}
		if actualField {
			setScalarValue(current.fieldValues, targetName, value, valueOK)
		}
		if actualField {
			e.bindTransportVariable(targetName, node.Value, current, true)
		}
		if !selfField || !actualField {
			e.bindTransportVariable(targetName, node.Value, current, false)
		}
	} else if targetName != "" && node.Operator == "+=" {
		right, rightOK := e.scalarString(node.Value, current)
		fieldID, isField := current.fieldSymbols[targetName]
		actualField := isField && (selfField || current.symbols[targetName] == fieldID)
		updatesBareValue := !selfField || actualField && current.symbols[targetName] == fieldID
		if updatesBareValue {
			left, leftOK := current.values[targetName]
			setScalarValue(current.values, targetName, left+right,
				leftOK && rightOK && len(left)+len(right) <= maxHTTPScalarLength)
		}
		if actualField {
			left, leftOK := current.fieldValues[targetName]
			setScalarValue(current.fieldValues, targetName, left+right,
				leftOK && rightOK && len(left)+len(right) <= maxHTTPScalarLength)
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
	if span := member.Span(); span.Start.Line > 0 {
		loc = graph.Location{Path: e.input.Path, Line: span.Start.Line, Column: span.Start.Column, EndLine: span.End.Line}
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
	case *gdast.BindingPattern:
		e.declareLocal(node.Name, "", e.location(node), current)
	case *gdast.DictionaryLiteral:
		if node.LuaStyle {
			// A Lua-style key is a string spelled as a name, never a reference.
			for _, entry := range node.Entries {
				e.walkExpression(entry.Value, current)
			}
			return
		}
	case *gdast.LambdaExpression:
		lambdaScope := current
		lambdaScope.symbols = cloneMap(current.symbols)
		lambdaScope.types = cloneMap(current.types)
		lambdaScope.fields = cloneMap(current.fields)
		lambdaScope.fieldSymbols = cloneMap(current.fieldSymbols)
		lambdaScope.fieldLocked = cloneBoolMap(current.fieldLocked)
		lambdaScope.locked = cloneBoolMap(current.locked)
		lambdaScope.transportConstants = cloneMap(current.transportConstants)
		lambdaScope.transportPayloads = cloneProtobufAPIMap(current.transportPayloads)
		lambdaScope.transportFieldPayloads = cloneProtobufAPIMap(current.transportFieldPayloads)
		lambdaScope.transportReceives = cloneMap(current.transportReceives)
		lambdaScope.values = cloneMap(current.values)
		lambdaScope.fieldValues = cloneMap(current.fieldValues)
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
	effects, configuredCall := e.configuredCallEffects(node.Callee, callee, current, loc)
	e.addProtobufUse(node, callee, fromID, current, loc)
	e.addTransportUse(node, callee, fromID, current, loc)
	e.addHTTPRequest(node, callee, effects, fromID, current, loc)
	e.addConfiguredEventEffects(node, callee, effects, fromID, current, loc)
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
		e.addConfiguredOrdinaryCall(configuredCall, fromID, callee, loc)
		return
	}
	if callee == "OS.get_environment" && len(node.Arguments) > 0 {
		if key, ok := literalString(node.Arguments[0]); ok && key != "" {
			e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
			e.addConfiguredOrdinaryCall(configuredCall, fromID, callee, loc)
			return
		}
	}
	if (callee == "ProjectSettings.get_setting" || callee == "ProjectSettings.has_setting") && len(node.Arguments) > 0 {
		if key, ok := literalString(node.Arguments[0]); ok && key != "" {
			e.b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc,
				map[string]string{"source": "project.godot"})
			e.addConfiguredOrdinaryCall(configuredCall, fromID, callee, loc)
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
			e.addSignalFact(fromID, graph.EdgePublishes, "emit", name,
				qualify(current.receiver, name), current, loc, nil)
			e.addConfiguredOrdinaryCall(configuredCall, fromID, callee, loc)
			return
		}
	}
	if _, ok := signalOperations[method]; ok {
		if member, ok := node.Callee.(*gdast.MemberExpression); ok {
			if e.addSignalOperation(node, member, fromID, method, current, loc) {
				e.addConfiguredOrdinaryCall(configuredCall, fromID, callee, loc)
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

func (e *extractor) configuredCallEffects(expression gdast.Expression, callee string, current scope, loc graph.Location) ([]calleffect.Effect, bool) {
	effects := e.callEffects.Lookup(graph.ProducerGDScript, callee)
	if len(effects) == 0 {
		return nil, false
	}
	if !e.untypedLocalReceiver(expression, current) && !e.unresolvedMemberReceiver(expression, current) {
		return effects, true
	}
	if !e.adapterWarned[callee] {
		e.adapterWarned[callee] = true
		message := "configured call adapter could not be resolved uniquely: " + callee
		if len(effects) == 1 && effects[0].Kind == calleffect.HTTPRequest {
			message = "configured HTTP request API could not be resolved uniquely: " + callee
		}
		e.b.Diagnostic(loc.Line, "warning", message)
	}
	return nil, true
}

func (e *extractor) addConfiguredOrdinaryCall(configured bool, fromID, callee string, loc graph.Location) {
	if configured {
		e.b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, nil)
	}
}

func (e *extractor) addHTTPRequest(node *gdast.CallExpression, callee string, effects []calleffect.Effect, fromID string, current scope, loc graph.Location) {
	methodIndex, routeIndex := -1, -1
	configured, builtin := false, false
	apiSymbol := callee
	configurationLine := 0
	if member, ok := node.Callee.(*gdast.MemberExpression); ok && member.Property == "request" && !e.localCall(node.Callee, current) {
		if receiverType, known := e.httpReceiverType(member.Object, current); known && e.isHTTPRequestReceiverType(receiverType) {
			builtin, methodIndex, routeIndex, apiSymbol = true, 2, 0, "HTTPRequest.request"
		}
	} else if identifier, ok := node.Callee.(*gdast.Identifier); ok && identifier.Name == "request" &&
		!e.localCall(node.Callee, current) && e.isHTTPRequestReceiverType(current.receiver) {
		builtin, methodIndex, routeIndex, apiSymbol = true, 2, 0, "HTTPRequest.request"
	}
	for index := range effects {
		effect := effects[index]
		if effect.Kind != calleffect.HTTPRequest {
			continue
		}
		configured = true
		configurationLine = effect.Line
		configuredMethod := effect.Roles[calleffect.RoleMethod].Argument
		configuredRoute := effect.Roles[calleffect.RoleURL].Argument
		if !builtin {
			methodIndex, routeIndex = configuredMethod, configuredRoute
		} else if configuredMethod != methodIndex || configuredRoute != routeIndex {
			// One call cannot safely satisfy two conflicting signatures. The shared
			// loader prevents duplicate configured symbols; a configured override of
			// the built-in adapter is equally ambiguous at the source callsite.
			e.b.Diagnostic(loc.Line, "warning", "configured HTTP request signature conflicts with built-in HTTPRequest.request")
			return
		}
	}
	if !builtin && !configured {
		return
	}
	if routeIndex < 0 || routeIndex >= len(node.Arguments) {
		if configured {
			e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf("configured call adapter %s requires URL argument %d", callee, routeIndex))
		}
		return
	}
	routeExpression := node.Arguments[routeIndex]
	route, routeOK := e.scalarString(routeExpression, current)
	if !routeOK {
		if e.hasUnsupportedKnownPercentFormat(routeExpression, current) {
			e.b.Diagnostic(loc.Line, "warning", "unsupported GDScript HTTP route percent formatting")
		}
		return
	}
	var method string
	methodExpression := "<default>"
	if builtin && methodIndex >= len(node.Arguments) {
		method = "GET"
	} else {
		if methodIndex < 0 || methodIndex >= len(node.Arguments) {
			if configured {
				e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf("configured call adapter %s requires method argument %d", callee, methodIndex))
			}
			return
		}
		methodExpression = e.sourceExpression(node.Arguments[methodIndex])
		method, _ = e.godotHTTPMethod(node.Arguments[methodIndex], current)
		if method == "" {
			return
		}
	}
	normalizedMethod, methodErr := httpmodel.NormalizeMethod(method)
	parsedRoute, routeErr := httpmodel.ParseRoute(route)
	if methodErr != nil || routeErr != nil {
		e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf("invalid outbound HTTP request %q %q", method, route))
		return
	}
	properties := map[string]string{
		"evidence":               "gdscript_scope",
		"http_api":               apiSymbol,
		"http_method":            normalizedMethod,
		"http_method_expression": methodExpression,
		"http_raw_method":        method,
		"http_raw_route":         route,
		"http_route":             parsedRoute.Canonical,
		"http_route_expression":  e.sourceExpression(routeExpression),
		"http_source":            "call",
	}
	identityRoute := parsedRoute.Canonical
	if parsedRoute.Query != "" {
		properties["http_query"] = parsedRoute.Query
	}
	if parsedRoute.Fragment != "" {
		properties["http_fragment"] = parsedRoute.Fragment
	}
	if parsedRoute.Authority != "" {
		properties["http_scheme"] = parsedRoute.Scheme
		properties["http_authority"] = parsedRoute.Authority
		identityRoute = parsedRoute.Scheme + "://" + parsedRoute.Authority + parsedRoute.Canonical
	}
	signatures := make([]string, 0, 2)
	if builtin {
		signatures = append(signatures, "builtin")
	}
	if configured {
		signatures = append(signatures, "configured")
		properties["http_config"] = fmt.Sprintf("%s:%d", projectconfig.FileName, configurationLine)
		properties[calleffect.PropertySource] = fmt.Sprintf("%s:%d", projectconfig.FileName, configurationLine)
		properties[calleffect.PropertySymbol] = callee
	}
	properties["http_signature"] = strings.Join(signatures, ",")
	e.b.AddFact(fromID, graph.EdgeRequests, "", normalizedMethod+" "+identityRoute, graph.KindEndpoint, loc, properties)
}

func (e *extractor) isHTTPRequestReceiverType(typeName string) bool {
	for depth := 0; typeName != "" && depth < 32; depth++ {
		if typeName == "HTTPRequest" {
			return true
		}
		next, ok := e.bases[typeName]
		if !ok {
			return false
		}
		typeName = next
	}
	return false
}

func (e *extractor) httpReceiverType(object gdast.Expression, current scope) (string, bool) {
	if member, ok := object.(*gdast.MemberExpression); ok {
		identifier, identifierOK := member.Object.(*gdast.Identifier)
		if identifierOK && identifier.Name == "self" {
			resolved := current.fields[member.Property]
			return resolved, resolved != ""
		}
	}
	return e.receiverType(object, current)
}

func godotHTTPMethodSymbol(expression gdast.Expression) (string, bool) {
	name := expressionName(expression)
	const prefix = "HTTPClient.METHOD_"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	method := strings.TrimPrefix(name, prefix)
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method, true
	default:
		return "", false
	}
}

func (e *extractor) godotHTTPMethod(expression gdast.Expression, current scope) (string, bool) {
	if method, ok := godotHTTPMethodSymbol(expression); ok {
		return method, true
	}
	method, ok := e.scalarString(expression, current)
	if !ok {
		return "", false
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method, true
	default:
		return "", false
	}
}

func (e *extractor) addProtobufUse(node *gdast.CallExpression, callee, fromID string, current scope, loc graph.Location) {
	if !e.protobufEnabled || callee == "" {
		return
	}
	// A locally declared override is application behavior even when its class
	// extends a generated message. Only inherited generated APIs canonicalize.
	if e.localCall(node.Callee, current) {
		return
	}
	if e.unresolvedMemberReceiver(node.Callee, current) {
		return
	}
	apiCallee := e.protobufCallee(callee, current)
	if e.protobufAmbiguous[apiCallee] {
		if !e.protobufWarned[apiCallee] {
			e.protobufWarned[apiCallee] = true
			e.b.Diagnostic(loc.Line, "warning", "ambiguous generated Protobuf GDScript API: "+apiCallee)
		}
		return
	}
	api, ok := e.protobufAPIs[apiCallee]
	if !ok || api.form == "" {
		return
	}
	if e.protobufClassReceiver(node.Callee, current) && api.form != "from_bytes" {
		return
	}
	// A local symbol without proven type information can spell the same name as
	// a generated class. It is application code, not evidence of generated API
	// use. Typed aliases and parameters have an entry in current.types.
	if e.untypedLocalReceiver(node.Callee, current) {
		return
	}
	var kind graph.EdgeKind
	switch api.form {
	case "get", "has":
		kind = graph.EdgeReads
	case "set", "new", "add":
		kind = graph.EdgeWrites
	case "to_bytes":
		kind = graph.EdgeEncodes
	case "from_bytes":
		kind = graph.EdgeDecodes
	default:
		return
	}
	staticType := current.receiver
	if index := strings.LastIndexByte(callee, '.'); index >= 0 {
		staticType = callee[:index]
	}
	e.b.AddFact(fromID, kind, api.targetID, api.target, api.targetKind, loc, map[string]string{
		"protocol": "protobuf", "form": api.form, "api": api.symbol,
		"static_type": staticType, "binding": api.symbol, "binding_id": api.bindingID,
		"evidence": "gdscript_scope",
	})
}

func (e *extractor) protobufClassReceiver(expression gdast.Expression, current scope) bool {
	member, ok := expression.(*gdast.MemberExpression)
	if !ok {
		return false
	}
	identifier, ok := member.Object.(*gdast.Identifier)
	if !ok {
		return false
	}
	if _, shadowed := current.symbols[identifier.Name]; shadowed {
		return false
	}
	return e.protobufTypes[identifier.Name]
}

func (e *extractor) protobufCallee(callee string, current scope) string {
	index := strings.LastIndexByte(callee, '.')
	if index < 0 {
		if base := e.protobufBase(current.receiver); base != "" {
			return base + "." + callee
		}
		return callee
	}
	if base := e.protobufBase(callee[:index]); base != "" {
		return base + callee[index:]
	}
	return callee
}

func (e *extractor) protobufBase(typeName string) string {
	for depth := 0; typeName != "" && depth < 32; depth++ {
		if e.protobufTypes[typeName] || e.protobufAmbiguous[typeName] {
			return typeName
		}
		next, ok := e.bases[typeName]
		if !ok {
			return ""
		}
		typeName = next
	}
	return ""
}

func (e *extractor) untypedLocalReceiver(expression gdast.Expression, current scope) bool {
	member, ok := expression.(*gdast.MemberExpression)
	if !ok {
		return false
	}
	identifier, ok := member.Object.(*gdast.Identifier)
	if !ok {
		return false
	}
	_, local := current.symbols[identifier.Name]
	return local && current.types[identifier.Name] == ""
}

func (e *extractor) unresolvedMemberReceiver(expression gdast.Expression, current scope) bool {
	member, ok := expression.(*gdast.MemberExpression)
	if !ok {
		return false
	}
	return e.resolveExpression(member.Object, current) == ""
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
	name, handler, handlerSource := "", operation.handler, ""
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
			if owner := e.signalOwner(member.Object, current); owner != "" {
				name = qualify(owner, text)
				handlerSource = name
			}
		}
	}
	if name == "" {
		// A bare identifier that names no signal this script declares may still
		// be one the script's base class declares, which is resolved by name
		// against the whole graph rather than guessed here.
		name = e.resolveExpression(member.Object, current)
		handlerSource = e.signalSource(member.Object, current)
	}
	return e.emitSignalOperation(name, handlerSource, node.Arguments, handler, operation, fromID, current, loc, nil)
}

func (e *extractor) addConfiguredEventEffects(node *gdast.CallExpression, callee string, effects []calleffect.Effect, fromID string, current scope, loc graph.Location) {
	for _, effect := range effects {
		var operation signalOperation
		switch effect.Kind {
		case calleffect.EventPublish:
			operation = signalOperation{kind: graph.EdgePublishes, form: "emit", handler: -1}
		case calleffect.EventSubscribe:
			operation = signalOperation{kind: graph.EdgeSubscribes, form: "connect", handler: effect.Roles[calleffect.RoleHandler].Argument, callable: true}
		case calleffect.EventUnsubscribe:
			operation = signalOperation{kind: graph.EdgeReferences, form: "signal_disconnect", handler: -1, callable: true}
		case calleffect.EventConnectionTest:
			operation = signalOperation{kind: graph.EdgeReferences, form: "signal_connection_test", handler: -1, callable: true}
		default:
			continue
		}
		if member, ok := node.Callee.(*gdast.MemberExpression); ok {
			if _, native := signalOperations[strings.ToLower(member.Property)]; native && !e.localCall(node.Callee, current) {
				e.b.Diagnostic(loc.Line, "warning", "configured event adapter conflicts with built-in signal operation: "+callee)
				continue
			}
		}
		eventIndex := effect.Roles[calleffect.RoleEvent].Argument
		if eventIndex < 0 || eventIndex >= len(node.Arguments) {
			e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf("configured call adapter %s requires event argument %d", callee, eventIndex))
			continue
		}
		if selector, ok := effect.Roles[calleffect.RoleHandler]; ok && (selector.Argument < 0 || selector.Argument >= len(node.Arguments)) {
			e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf("configured call adapter %s requires handler argument %d", callee, selector.Argument))
			continue
		}
		event := node.Arguments[eventIndex]
		name := e.resolveExpression(event, current)
		handlerSource := e.signalSource(event, current)
		extra := map[string]string{
			calleffect.PropertySource: fmt.Sprintf("%s:%d", projectconfig.FileName, effect.Line),
			calleffect.PropertySymbol: callee,
		}
		e.emitSignalOperation(name, handlerSource, node.Arguments, operation.handler, operation, fromID, current, loc, extra)
	}
}

func (e *extractor) emitSignalOperation(name, handlerSource string, arguments []gdast.Expression, handler int, operation signalOperation, fromID string, current scope, loc graph.Location, provenance map[string]string) bool {
	// The handler is recorded on the routing fact itself as well as on the
	// handled_by fact. Most connects name a signal another file declares, so the
	// parser preserves the source name and lets storage decide whether exactly
	// one declaration proves the relation.
	extra := map[string]string{}
	for key, value := range provenance {
		extra[key] = value
	}
	subscriber := fromID
	if operation.form == "connect" && handler >= 0 {
		if method := e.handlerMethod(arguments, handler, current); method != "" {
			extra["handler"] = method
			// The thing that receives the signal is the handler, not the
			// statement that wired it, and the handler is a node this parser
			// owns. Sourcing the route there is what lets the graph traverse a
			// resolved signal back to its declared handler even when the signal
			// is declared in another file. The named-source handled_by edge now
			// expresses that direction directly too; this subscribes edge remains
			// useful for handler-first traversal. The wiring statement is kept as
			// the site, and the fact's own location still points at the connect call.
			if id := e.handlerNode(method); id != "" {
				subscriber = id
				if current.container != "" {
					extra["site"] = current.container
				}
			}
		}
	}
	ref, emitted := e.addSignalFact(subscriber, operation.kind, operation.form, name, handlerSource, current, loc, extra)
	if !emitted {
		return false
	}
	if operation.form == "connect" && handler >= 0 {
		e.addSignalHandler(ref, arguments, handler, current, loc, provenance)
	}
	return true
}

// signalOwner returns only receiver identity supported by structural type
// evidence. A bare unknown variable name is not an owner: qualifying a literal
// signal with it would create a name no declaration owns, while resolving the
// bare signal could attach a handler to an unrelated uniquely named event.
func (e *extractor) signalOwner(expression gdast.Expression, current scope) string {
	switch node := expression.(type) {
	case *gdast.Identifier:
		if node.Name == "self" {
			return current.receiver
		}
		return current.types[node.Name]
	case *gdast.TypeExpression:
		return e.resolveType(node.Name, current)
	case *gdast.CallExpression:
		return e.inferExpressionType(node, current)
	default:
		return ""
	}
}

// signalSource keeps the bare form for a signal identifier on this object, but
// accepts a member receiver only when its owner has structural type evidence.
// The routing fact may still preserve an unknown expression as unresolved
// evidence; handled_by is stricter because resolving its source would assert
// which declaration invokes the handler.
func (e *extractor) signalSource(expression gdast.Expression, current scope) string {
	switch node := expression.(type) {
	case *gdast.Identifier:
		return node.Name
	case *gdast.MemberExpression:
		owner := e.signalOwner(node.Object, current)
		if owner == "" {
			return ""
		}
		return qualify(owner, node.Property)
	default:
		return ""
	}
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
// about which method the engine will run, so nothing is recorded. A foreign
// signal keeps its canonical name so storage can resolve the source without the
// parser inventing an ID or choosing among ambiguous declarations.
func (e *extractor) addSignalHandler(ref signalRef, arguments []gdast.Expression, index int, current scope, loc graph.Location, provenance map[string]string) {
	if ref.qualified == "" {
		return
	}
	method := e.handlerMethod(arguments, index, current)
	if method == "" {
		return
	}
	properties := map[string]string{"form": "connect", "signal": ref.qualified, "receiver": current.receiver}
	for key, value := range provenance {
		properties[key] = value
	}
	if ref.id != "" {
		e.b.AddFact(ref.id, graph.EdgeHandledBy, "", method, graph.KindMethod, loc, properties)
		return
	}
	e.b.AddNamedSourceFact(ref.qualified, graph.KindEvent, graph.EdgeHandledBy,
		"", method, graph.KindMethod, loc, properties)
}

// addSignalFact records one signal fact and reports the declaration it resolved
// to. A returned ref with an empty id means the name matched no declaration in
// this script and the fact names it as an unresolved event, which is what keeps a
// signal whose owner is unknown visible without guessing an owner.
func (e *extractor) addSignalFact(fromID string, kind graph.EdgeKind, form, name, handlerSource string, current scope, loc graph.Location, extra map[string]string) (signalRef, bool) {
	if name == "" {
		return signalRef{}, false
	}
	name = strings.TrimPrefix(name, current.receiver+".")
	ref, ok := signalRef{}, false
	if handlerSource != "" {
		ref, ok = current.signals[name]
		if !ok {
			ref, ok = current.signals[qualify(current.receiver, name)]
		}
	}
	properties := map[string]string{"form": form}
	for key, value := range extra {
		properties[key] = value
	}
	if !ok {
		properties["signal"] = name
		e.b.AddFact(fromID, kind, "", name, graph.KindEvent, loc, properties)
		return signalRef{qualified: handlerSource}, true
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
			if resolved := current.types[identifier.Name]; resolved != "" {
				return resolved
			}
			if e.protobufTypes[identifier.Name] {
				if _, shadowed := current.symbols[identifier.Name]; !shadowed {
					return identifier.Name
				}
			}
		}
		return ""
	}
	callee := e.resolveCallee(call.Callee, current)
	apiCallee := e.protobufCallee(callee, current)
	if !e.localCall(call.Callee, current) && !e.unresolvedMemberReceiver(call.Callee, current) &&
		!e.untypedLocalReceiver(call.Callee, current) && !e.protobufAmbiguous[apiCallee] {
		if api, exists := e.protobufAPIs[apiCallee]; exists && api.returns != "" {
			if !e.protobufClassReceiver(call.Callee, current) || api.form == "" || api.form == "from_bytes" {
				return api.returns
			}
		}
	}
	if callee == "preload" || callee == "load" {
		if len(call.Arguments) > 0 {
			if resource, ok := literalString(call.Arguments[0]); ok {
				return e.resourceModule(resource)
			}
		}
		return ""
	}
	if strings.HasSuffix(callee, ".new") {
		result := strings.TrimSuffix(callee, ".new")
		if !e.protobufTypes[result] && !e.protobufAmbiguous[result] {
			return result
		}
		return ""
	}
	if last := graph.SimpleName(callee); last != "" && unicode.IsUpper([]rune(last)[0]) {
		if e.protobufTypes[callee] {
			return ""
		}
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
		if object, ok := node.Object.(*gdast.Identifier); ok && object.Name == "self" {
			if resolved := current.fields[node.Property]; resolved != "" {
				return resolved
			}
		}
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

const maxHTTPScalarLength = 4096

func (e *extractor) scalarString(expression gdast.Expression, current scope) (string, bool) {
	switch node := expression.(type) {
	case *gdast.Literal:
		if value, ok := literalString(node); ok {
			return value, len(value) <= maxHTTPScalarLength
		}
	case *gdast.Identifier:
		value, ok := current.values[node.Name]
		return value, ok
	case *gdast.MemberExpression:
		if method, ok := godotHTTPMethodSymbol(node); ok {
			return method, true
		}
		if object, ok := node.Object.(*gdast.Identifier); ok && object.Name == "self" {
			value, exists := current.fieldValues[node.Property]
			return value, exists
		}
	case *gdast.BinaryExpression:
		switch node.Operator {
		case "+":
			left, leftOK := e.scalarString(node.Left, current)
			right, rightOK := e.scalarString(node.Right, current)
			if leftOK && rightOK && len(left)+len(right) <= maxHTTPScalarLength {
				return left + right, true
			}
		case "%":
			format, arguments, ok := e.percentFormatOperands(node, current)
			if !ok {
				return "", false
			}
			return boundedPercentFormat(format, arguments)
		}
	}
	return "", false
}

func (e *extractor) percentFormatOperands(expression *gdast.BinaryExpression, current scope) (string, []string, bool) {
	format, formatOK := e.scalarString(expression.Left, current)
	if !formatOK {
		return "", nil, false
	}
	arguments := []string{}
	if array, ok := expression.Right.(*gdast.ArrayLiteral); ok {
		if len(array.Elements) > 16 {
			return "", nil, false
		}
		for _, element := range array.Elements {
			value, ok := e.scalarFormatValue(element, current)
			if !ok {
				return "", nil, false
			}
			arguments = append(arguments, value)
		}
	} else if value, ok := e.scalarFormatValue(expression.Right, current); ok {
		arguments = append(arguments, value)
	} else {
		return "", nil, false
	}
	return format, arguments, true
}

func (e *extractor) hasUnsupportedKnownPercentFormat(expression gdast.Expression, current scope) bool {
	unsupported := false
	gdast.Inspect(expression, func(node gdast.Node) bool {
		binary, ok := node.(*gdast.BinaryExpression)
		if !ok || binary.Operator != "%" {
			return true
		}
		format, arguments, known := e.percentFormatOperands(binary, current)
		if !known {
			return true
		}
		if _, supported := boundedPercentFormat(format, arguments); !supported {
			unsupported = true
			return false
		}
		return true
	})
	return unsupported
}

func (e *extractor) scalarFormatValue(expression gdast.Expression, current scope) (string, bool) {
	if value, ok := e.scalarString(expression, current); ok {
		return value, true
	}
	literal, ok := expression.(*gdast.Literal)
	if !ok || literal.Kind != gdast.IntegerLiteral && literal.Kind != gdast.FloatLiteral {
		return "", false
	}
	value := strings.ReplaceAll(literal.Raw, "_", "")
	if literal.Kind == gdast.IntegerLiteral {
		parsed, err := strconv.ParseInt(value, 0, 64)
		if err != nil {
			return "", false
		}
		return strconv.FormatInt(parsed, 10), true
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(parsed, 'g', -1, 64), true
}

func boundedPercentFormat(format string, arguments []string) (string, bool) {
	var result strings.Builder
	argument := 0
	for index := 0; index < len(format); {
		if format[index] != '%' {
			result.WriteByte(format[index])
			index++
			if result.Len() > maxHTTPScalarLength {
				return "", false
			}
			continue
		}
		if index+1 < len(format) && format[index+1] == '%' {
			result.WriteByte('%')
			index += 2
			continue
		}
		end := index + 1
		for end < len(format) && strings.ContainsRune("+-0 .123456789", rune(format[end])) {
			end++
		}
		if end != index+1 || end >= len(format) || !strings.ContainsRune("sdifxXoc", rune(format[end])) || argument >= len(arguments) {
			return "", false
		}
		value := arguments[argument]
		switch format[end] {
		case 'd', 'i', 'x', 'X', 'o', 'c':
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", false
			}
			switch format[end] {
			case 'd', 'i':
				value = strconv.FormatInt(number, 10)
			case 'x':
				value = strconv.FormatInt(number, 16)
			case 'X':
				value = strings.ToUpper(strconv.FormatInt(number, 16))
			case 'o':
				value = strconv.FormatInt(number, 8)
			case 'c':
				value = string(rune(number))
			}
		case 'f':
			number, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return "", false
			}
			value = strconv.FormatFloat(number, 'f', 6, 64)
		}
		result.WriteString(value)
		if result.Len() > maxHTTPScalarLength {
			return "", false
		}
		argument++
		index = end + 1
	}
	if argument != len(arguments) {
		return "", false
	}
	return result.String(), true
}

func (e *extractor) sourceExpression(expression gdast.Expression) string {
	if expression == nil {
		return ""
	}
	span := expression.Span()
	if span.Start.Offset < 0 || span.End.Offset < span.Start.Offset || span.End.Offset > len(e.input.Content) {
		return expressionName(expression)
	}
	return strings.TrimSpace(string(e.input.Content[span.Start.Offset:span.End.Offset]))
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

func cloneBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneProtobufAPIMap(source map[string]protobufAPI) map[string]protobufAPI {
	result := make(map[string]protobufAPI, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneFlowScope(current scope) scope {
	current.symbols = cloneMap(current.symbols)
	current.types = cloneMap(current.types)
	current.fields = cloneMap(current.fields)
	current.fieldSymbols = cloneMap(current.fieldSymbols)
	current.fieldLocked = cloneBoolMap(current.fieldLocked)
	current.locked = cloneBoolMap(current.locked)
	current.transportConstants = cloneMap(current.transportConstants)
	current.transportPayloads = cloneProtobufAPIMap(current.transportPayloads)
	current.transportFieldPayloads = cloneProtobufAPIMap(current.transportFieldPayloads)
	current.transportReceives = cloneMap(current.transportReceives)
	current.values = cloneMap(current.values)
	current.fieldValues = cloneMap(current.fieldValues)
	return current
}

func setInferredType(types map[string]string, name, inferred string) {
	if inferred == "" {
		delete(types, name)
		return
	}
	types[name] = inferred
}

func setScalarValue(values map[string]string, name, value string, ok bool) {
	if !ok {
		delete(values, name)
		return
	}
	values[name] = value
}

func mergeFlowTypes(current scope, alternatives []map[string]string) {
	if len(alternatives) == 0 {
		return
	}
	merged := cloneMap(alternatives[0])
	for name, value := range merged {
		if _, declared := current.symbols[name]; !declared {
			if _, incoming := current.types[name]; !incoming {
				delete(merged, name)
				continue
			}
		}
		for _, alternative := range alternatives[1:] {
			if alternative[name] != value {
				delete(merged, name)
				break
			}
		}
	}
	for name := range current.types {
		delete(current.types, name)
	}
	for name, value := range merged {
		current.types[name] = value
	}
}

func mergeFlowFields(current scope, alternatives []map[string]string) {
	if len(alternatives) == 0 {
		return
	}
	merged := cloneMap(alternatives[0])
	for name, value := range merged {
		if _, field := current.fieldSymbols[name]; !field {
			delete(merged, name)
			continue
		}
		for _, alternative := range alternatives[1:] {
			if alternative[name] != value {
				delete(merged, name)
				break
			}
		}
	}
	for name := range current.fields {
		delete(current.fields, name)
	}
	for name, value := range merged {
		current.fields[name] = value
	}
}

func mergeFlowPayloads(destination map[string]protobufAPI, alternatives []map[string]protobufAPI) {
	if len(alternatives) == 0 {
		return
	}
	merged := cloneProtobufAPIMap(alternatives[0])
	for name, value := range merged {
		for _, alternative := range alternatives[1:] {
			other, ok := alternative[name]
			if !ok || other.targetID != value.targetID || other.symbol != value.symbol {
				delete(merged, name)
				break
			}
		}
	}
	for name := range destination {
		delete(destination, name)
	}
	for name, value := range merged {
		destination[name] = value
	}
}

func mergeFlowStrings(destination map[string]string, alternatives []map[string]string) {
	if len(alternatives) == 0 {
		return
	}
	merged := cloneMap(alternatives[0])
	for name, value := range merged {
		for _, alternative := range alternatives[1:] {
			if alternative[name] != value {
				delete(merged, name)
				break
			}
		}
	}
	for name := range destination {
		delete(destination, name)
	}
	for name, value := range merged {
		destination[name] = value
	}
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
