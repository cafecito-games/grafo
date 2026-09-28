package golang

import (
	"bytes"
	"context"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/protobufbinding"
)

type Parser struct {
	semantic SemanticLoader
	bindings *protobufbinding.Loader
}

func New() *Parser {
	return &Parser{semantic: NewPackageLoader(), bindings: protobufbinding.NewLoader()}
}

func NewWithSemanticLoader(loader SemanticLoader) *Parser {
	return &Parser{semantic: loader, bindings: protobufbinding.NewLoader()}
}

func NewWithBindingLoader(loader *protobufbinding.Loader) *Parser {
	if loader == nil {
		loader = protobufbinding.NewLoader()
	}
	return &Parser{semantic: NewPackageLoader(), bindings: loader}
}

func (p *Parser) SemanticLoadMetrics() SemanticLoadMetrics {
	if provider, ok := p.semantic.(interface{ Metrics() SemanticLoadMetrics }); ok {
		return provider.Metrics()
	}
	return SemanticLoadMetrics{}
}

func (*Parser) Language() string { return "go" }
func (*Parser) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

func (p *Parser) SemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	bindingKey := ""
	if p.bindings != nil {
		key, err := p.bindings.SemanticKey(ctx, input)
		if err != nil {
			return "", err
		}
		bindingKey = ":" + key
	}
	if input.Root == "" {
		return buildContextString() + bindingKey, nil
	}
	key, _, err := semanticWorkspaceKey(input.Root)
	return key + bindingKey, err
}

func (p *Parser) WorkspaceSemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	return p.SemanticKey(ctx, input)
}

func (*Parser) SemanticDependencies() []string { return semanticDependencies() }

func (*Parser) SemanticAffectedPaths(allPaths, changedPaths []string) []string {
	changed := false
	for _, path := range changedPaths {
		if isGoSemanticInput(path) || protobufbinding.IsSemanticInput(path) {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	result := make([]string, 0, len(allPaths))
	for _, path := range allPaths {
		if strings.EqualFold(filepath.Ext(path), ".go") {
			result = append(result, path)
		}
	}
	return result
}

func (p *Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "go")
	semantic := SemanticView{}
	bindingRegistry := protobufbinding.Registry{}
	haveBindingRegistry := false
	if p.semantic != nil {
		loaded, loadErr := p.semantic.Load(ctx, input)
		if loadErr != nil {
			b.Diagnostic(0, "warning", "Go semantic loading failed; using syntax evidence: "+loadErr.Error())
		} else {
			semantic = loaded
			if semantic.BuildContext != "" {
				properties := map[string]string{
					"go_build_context":   semantic.BuildContext,
					"go_semantic_loader": "go/packages",
				}
				if input.GoModule != "" {
					properties["go_module"] = input.GoModule
				}
				if input.Root != "" {
					if workspace := discoverGoWorkspace(input.Root); workspace != "" {
						properties["go_workspace"] = workspace
					}
				}
				b.Result.Nodes[0].Properties = properties
			}
			b.Result.Diagnostics = append(b.Result.Diagnostics, semantic.Diagnostics...)
			if semantic.Available && !semantic.Included {
				b.Result.Nodes[0].Properties["go_build_excluded"] = "true"
				return b.Finish(), nil
			}
		}
	}
	if p.bindings != nil && input.Root != "" {
		registry, bindingErr := p.bindings.Load(ctx, input)
		if bindingErr != nil {
			b.Diagnostic(0, "warning", "load Protobuf binding registry: "+bindingErr.Error())
		} else {
			bindingRegistry, haveBindingRegistry = registry, true
			if generated, ok, reason := registry.GeneratedFile(input.Path, "go", input.Content); ok {
				if b.Result.Nodes[0].Properties == nil {
					b.Result.Nodes[0].Properties = map[string]string{}
				}
				b.Result.Nodes[0].Properties["generated"] = "true"
				b.Result.Nodes[0].Properties["generator"] = generated.Generator
				b.Result.Nodes[0].Properties["generator_version"] = generated.Version
				b.Result.Nodes[0].Properties["source_proto"] = generated.Source
				return b.Finish(), nil
			} else if reason != "" {
				b.Diagnostic(1, "warning", "Protobuf generated-file provenance rejected: "+reason)
			}
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, input.Path, input.Content, parser.ParseComments|parser.SkipObjectResolution)
	if file == nil {
		return b.Finish(), err
	}
	if err != nil {
		b.Diagnostic(0, "warning", err.Error())
	}
	packageName := packageQualified(input, file.Name.Name)
	if semantic.PackagePath != "" {
		packageName = semantic.PackagePath
	}
	imports := map[string]string{}
	var packageShadowedBuiltins map[string]bool
	if !semantic.Available {
		packageShadowedBuiltins = syntaxPackageShadowedBuiltins(input, file)
	}
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		alias := filepath.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		imports[alias] = importPath
		loc := location(input.Path, fset, spec.Pos(), spec.End())
		b.AddFact(b.FileID(), graph.EdgeImports, "", importPath, graph.KindModule, loc,
			map[string]string{"alias": alias})
	}
	for _, declaration := range semantic.ErrorDeclarations {
		if declaration.Kind != graph.KindVariable {
			continue
		}
		identity := "named"
		if declaration.Sentinel {
			identity = "sentinel"
		}
		b.Declare(b.FileID(), graph.Node{
			Kind: declaration.Kind, Name: declaration.Name, QualifiedName: declaration.QualifiedName,
			Location: declaration.Location, Properties: map[string]string{
				"type": declaration.Type, "error_identity": identity, "resolution": "go/types",
			},
		})
	}
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *goast.FuncDecl:
			parseFunction(b, fset, input, packageName, imports, semantic, packageShadowedBuiltins, node)
		case *goast.GenDecl:
			if node.Tok == token.TYPE {
				for _, spec := range node.Specs {
					if ts, ok := spec.(*goast.TypeSpec); ok {
						parseType(b, fset, input, packageName, semantic, ts)
					}
				}
			}
		}
	}
	if haveBindingRegistry {
		emitProtocolUses(b, semantic, bindingRegistry)
	}
	return b.Finish(), nil
}

func packageQualified(input parserapi.Input, packageName string) string {
	dir := filepath.ToSlash(filepath.Dir(input.Path))
	if input.GoModule != "" {
		if dir == "." {
			return input.GoModule
		}
		return strings.TrimSuffix(input.GoModule, "/") + "/" + dir
	}
	if dir == "." {
		return packageName
	}
	return dir
}

func parseFunction(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, semantic SemanticView, packageShadowedBuiltins map[string]bool, decl *goast.FuncDecl) {
	kind := graph.KindFunction
	qualified := pkg + "." + decl.Name.Name
	owner := ""
	receiverVariable := ""
	receiverQualified := ""
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = graph.KindMethod
		owner = receiverName(decl.Recv.List[0].Type)
		qualified = pkg + "." + owner + "." + decl.Name.Name
		receiverQualified = pkg + "." + owner
		if len(decl.Recv.List[0].Names) > 0 {
			receiverVariable = decl.Recv.List[0].Names[0].Name
		}
	}
	loc := location(input.Path, fset, decl.Pos(), decl.End())
	node := graph.Node{Kind: kind, Name: decl.Name.Name, QualifiedName: qualified,
		Location: loc, Properties: map[string]string{"signature": render(fset, decl.Type)}}
	if function, ok := semantic.Functions[qualified]; ok && len(function.ErrorResults) > 0 {
		positions := make([]string, 0, len(function.ErrorResults))
		for _, result := range function.ErrorResults {
			positions = append(positions, strconv.Itoa(result.Position))
		}
		node.Properties["returns_error"] = "true"
		node.Properties["error_result_positions"] = strings.Join(positions, ",")
		node.Properties["error_evidence"] = "go/types"
	}
	if owner != "" {
		node.Properties["receiver"] = owner
	}
	functionID := b.Declare(b.FileID(), node)
	if decl.Body != nil {
		flow := collectFunctionBindings(b, fset, input, pkg, imports, qualified, functionID, decl)
		if receiverVariable != "" {
			flow.types[receiverVariable] = receiverQualified
		}
		goast.Inspect(decl.Body, func(n goast.Node) bool {
			call, ok := n.(*goast.CallExpr)
			if !ok {
				return true
			}
			parseCall(b, fset, input, pkg, imports, semantic, functionID, receiverVariable, receiverQualified, flow, call)
			return true
		})
		emitDataFlow(b, fset, input, pkg, imports, semantic, functionID, receiverVariable, receiverQualified, flow, decl.Body)
		if !semantic.Available {
			emitSyntaxFailureFlow(b, fset, input, pkg, imports, functionID, decl, packageShadowedBuiltins)
		}
	}
	for _, failure := range semantic.Failures {
		if failure.Function == qualified {
			b.AddFact(functionID, failure.Kind, "", failure.Target, failure.TargetKind,
				failure.Location, cloneStringMap(failure.Properties))
		}
	}
}

func emitProtocolUses(b *parserapi.Builder, semantic SemanticView, registry protobufbinding.Registry) {
	path := filepath.ToSlash(filepath.Clean(filepath.FromSlash(b.Input.Path)))
	path = strings.TrimPrefix(path, "./")
	if registry.ConfiguredOutput(path, "go") {
		// A configured generated output remains implementation code even when
		// its corroborating header has drifted. Keep ordinary syntax nodes and
		// the provenance diagnostic, but never count its internals as application
		// protocol producers or consumers.
		return
	}
	sources := map[string]string{"": b.FileID()}
	for _, node := range b.Result.Nodes {
		if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
			sources[node.QualifiedName] = node.ID
		}
	}
	projections := indexProtocolProjections(registry)
	for _, use := range semantic.ProtocolUses {
		fromID := sources[use.Function]
		if fromID == "" {
			fromID = b.FileID()
		}
		projection, count := resolveProtocolProjection(projections, use.Binding)
		if count == 0 {
			continue
		}
		if count > 1 {
			b.Diagnostic(use.Location.Line, "warning", "ambiguous Protobuf binding for protocol usage: "+use.Binding)
			continue
		}
		form := use.Form
		switch use.Form {
		case "marshal", "unmarshal":
			if projection.CanonicalKind != graph.KindType || projection.Properties["projection"] != "message" {
				continue
			}
		case "getter":
			if projection.Properties["projection"] != "accessor" || projection.Properties["accessor"] != "get" {
				continue
			}
		case "composite_literal":
			if projection.Properties["projection"] != "field" {
				continue
			}
		case "composite_literal_type":
			if projection.Properties["projection"] != "oneof_wrapper" {
				continue
			}
			form = "oneof_wrapper"
		case "field_selection":
			if projection.Properties["projection"] != "field" {
				continue
			}
		case "type_switch":
			if projection.Properties["projection"] != "oneof_wrapper" {
				continue
			}
		default:
			continue
		}
		properties := map[string]string{
			"protocol": "protobuf", "form": form, "api": use.API,
			"static_type": use.StaticType, "binding": use.Binding,
			"binding_id": projection.Node.ID, "evidence": "go/types",
		}
		b.AddFact(fromID, use.Kind, projection.CanonicalID, projection.Canonical,
			projection.CanonicalKind, use.Location, properties)
	}
}

type protocolProjectionIndex map[string]map[string]protobufbinding.Projection

func indexProtocolProjections(registry protobufbinding.Registry) protocolProjectionIndex {
	index := protocolProjectionIndex{}
	for _, config := range registry.Configs {
		for _, projection := range config.Projections {
			if projection.Node.Language != "go" {
				continue
			}
			binding := projection.Node.QualifiedName
			byCanonical := index[binding]
			if byCanonical == nil {
				byCanonical = map[string]protobufbinding.Projection{}
				index[binding] = byCanonical
			}
			key := projection.CanonicalID
			if key == "" {
				key = string(projection.CanonicalKind) + ":" + projection.Canonical
			}
			current, exists := byCanonical[key]
			if !exists || projection.Node.ID < current.Node.ID {
				byCanonical[key] = projection
			}
		}
	}
	return index
}

func resolveProtocolProjection(index protocolProjectionIndex, binding string) (protobufbinding.Projection, int) {
	byCanonical := index[binding]
	if len(byCanonical) != 1 {
		return protobufbinding.Projection{}, len(byCanonical)
	}
	for _, projection := range byCanonical {
		return projection, 1
	}
	return protobufbinding.Projection{}, 0
}

type functionBindings struct {
	symbols map[string]string
	types   map[string]string
}

func collectFunctionBindings(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, functionName, functionID string, decl *goast.FuncDecl) functionBindings {
	bindings := functionBindings{symbols: map[string]string{}, types: map[string]string{}}
	declare := func(name string, kind graph.NodeKind, typeText string, loc graph.Location) {
		if name == "" || name == "_" {
			return
		}
		qualified := functionName + "." + name
		if kind == graph.KindVariable {
			qualified += fmt.Sprintf("@%d", loc.Line)
		}
		properties := map[string]string{}
		if typeText != "" {
			properties["type"] = typeText
		}
		id := b.Declare(functionID, graph.Node{Kind: kind, Name: name, QualifiedName: qualified,
			Location: loc, Properties: properties})
		bindings.symbols[name] = id
		if typeText != "" {
			bindings.types[name] = qualifyGoType(typeText, pkg, imports)
		}
	}
	if decl.Type.Params != nil {
		for _, field := range decl.Type.Params.List {
			typeText := render(fset, field.Type)
			for _, name := range field.Names {
				declare(name.Name, graph.KindParameter, typeText, location(input.Path, fset, name.Pos(), name.End()))
			}
		}
	}
	goast.Inspect(decl.Body, func(n goast.Node) bool {
		switch value := n.(type) {
		case *goast.AssignStmt:
			if value.Tok != token.DEFINE {
				return true
			}
			for index, lhs := range value.Lhs {
				ident, ok := lhs.(*goast.Ident)
				if !ok {
					continue
				}
				typeText := ""
				if index < len(value.Rhs) {
					typeText = inferGoExprType(value.Rhs[index], fset, pkg, imports, bindings.types)
				} else if len(value.Rhs) == 1 {
					typeText = inferGoExprType(value.Rhs[0], fset, pkg, imports, bindings.types)
				}
				declare(ident.Name, graph.KindVariable, typeText, location(input.Path, fset, ident.Pos(), ident.End()))
			}
		case *goast.DeclStmt:
			gen, ok := value.Decl.(*goast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				return true
			}
			for _, raw := range gen.Specs {
				spec, ok := raw.(*goast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range spec.Names {
					typeText := render(fset, spec.Type)
					if typeText == "" && index < len(spec.Values) {
						typeText = inferGoExprType(spec.Values[index], fset, pkg, imports, bindings.types)
					}
					declare(name.Name, graph.KindVariable, typeText, location(input.Path, fset, name.Pos(), name.End()))
				}
			}
		}
		return true
	})
	return bindings
}

func emitDataFlow(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, semantic SemanticView, functionID, receiverVariable, receiverQualified string, bindings functionBindings, body *goast.BlockStmt) {
	goast.Inspect(body, func(n goast.Node) bool {
		switch value := n.(type) {
		case *goast.AssignStmt:
			for index, lhs := range value.Lhs {
				ident, ok := lhs.(*goast.Ident)
				if !ok {
					continue
				}
				targetID := bindings.symbols[ident.Name]
				if targetID == "" || len(value.Rhs) == 0 {
					continue
				}
				rhs := value.Rhs[min(index, len(value.Rhs)-1)]
				loc := location(input.Path, fset, value.Pos(), value.End())
				for _, sourceID := range referencedVariables(rhs, bindings.symbols) {
					b.AddFact(sourceID, graph.EdgeAssigns, targetID, "", graph.KindVariable, loc, nil)
				}
			}
		case *goast.ReturnStmt:
			loc := location(input.Path, fset, value.Pos(), value.End())
			for _, result := range value.Results {
				for _, sourceID := range referencedVariables(result, bindings.symbols) {
					b.AddFact(sourceID, graph.EdgeReturns, functionID, "", "", loc, nil)
				}
			}
		case *goast.CallExpr:
			callee, _, ignore := resolvedGoCallee(value, fset, pkg, imports, semantic, receiverVariable, receiverQualified, bindings.types)
			if ignore {
				return true
			}
			loc := location(input.Path, fset, value.Pos(), value.End())
			for position, argument := range value.Args {
				for _, sourceID := range referencedVariables(argument, bindings.symbols) {
					b.AddFact(sourceID, graph.EdgePasses, "", callee, "", loc,
						map[string]string{"argument": strconv.Itoa(position)})
				}
			}
		}
		return true
	})
}

func referencedVariables(expr goast.Expr, symbols map[string]string) []string {
	seen := map[string]bool{}
	var result []string
	goast.Inspect(expr, func(n goast.Node) bool {
		ident, ok := n.(*goast.Ident)
		if !ok {
			return true
		}
		if id := symbols[ident.Name]; id != "" && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
		return true
	})
	return result
}

func parseType(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, semantic SemanticView, spec *goast.TypeSpec) {
	kind := graph.KindType
	if _, ok := spec.Type.(*goast.InterfaceType); ok {
		kind = graph.KindInterface
	}
	qualified := pkg + "." + spec.Name.Name
	loc := location(input.Path, fset, spec.Pos(), spec.End())
	properties := map[string]string{"underlying": render(fset, spec.Type)}
	errorType := false
	for _, declaration := range semantic.ErrorDeclarations {
		if declaration.Kind == graph.KindType && declaration.QualifiedName == qualified {
			errorType = true
			properties["error_type"] = "true"
			properties["error_evidence"] = "go/types"
			break
		}
	}
	typeID := b.Declare(b.FileID(), graph.Node{Kind: kind, Name: spec.Name.Name,
		QualifiedName: qualified, Location: loc, Properties: properties})
	if errorType {
		b.AddFact(typeID, graph.EdgeImplements, "", "builtin.error", graph.KindInterface, loc,
			map[string]string{"resolution": "go/types", "evidence": "go/types", "form": "error_method_set"})
	}
	for _, implementation := range semantic.Implementations {
		if implementation.Concrete == qualified {
			b.AddFact(typeID, graph.EdgeImplements, "", implementation.Interface, graph.KindInterface, loc,
				map[string]string{"resolution": "go/types"})
		}
	}
	switch value := spec.Type.(type) {
	case *goast.StructType:
		parseFields(b, fset, input, qualified, typeID, value.Fields, false, semantic)
	case *goast.InterfaceType:
		parseFields(b, fset, input, qualified, typeID, value.Methods, true, semantic)
	}
}

func parseFields(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, parentName, parentID string, fields *goast.FieldList, methods bool, semantic SemanticView) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		loc := location(input.Path, fset, field.Pos(), field.End())
		typeText := render(fset, field.Type)
		if len(field.Names) == 0 {
			edgeKind := graph.EdgeEmbeds
			if methods {
				edgeKind = graph.EdgeExtends
			}
			b.AddFact(parentID, edgeKind, "", typeText, "", loc, nil)
			continue
		}
		for _, name := range field.Names {
			kind := graph.KindField
			if methods {
				kind = graph.KindMethod
			}
			qualified := parentName + "." + name.Name
			properties := map[string]string{"type": typeText}
			if function, ok := semantic.Functions[qualified]; ok && len(function.ErrorResults) > 0 {
				positions := make([]string, 0, len(function.ErrorResults))
				for _, result := range function.ErrorResults {
					positions = append(positions, strconv.Itoa(result.Position))
				}
				properties["returns_error"] = "true"
				properties["error_result_positions"] = strings.Join(positions, ",")
				properties["error_evidence"] = "go/types"
			}
			nodeID := b.AddNode(graph.Node{Kind: kind, Name: name.Name,
				QualifiedName: qualified, Location: loc, Properties: properties})
			b.AddFact(parentID, graph.EdgeHasField, nodeID, "", "", loc, nil)
			for _, failure := range semantic.Failures {
				if failure.Function == qualified {
					b.AddFact(nodeID, failure.Kind, "", failure.Target, failure.TargetKind,
						failure.Location, cloneStringMap(failure.Properties))
				}
			}
		}
	}
	if methods {
		addPromotedInterfaceMethods(b, parentName, parentID,
			location(input.Path, fset, fields.Pos(), fields.End()), semantic)
	}
}

func addPromotedInterfaceMethods(b *parserapi.Builder, parentName, parentID string, loc graph.Location, semantic SemanticView) {
	prefix := parentName + "."
	var names []string
	for name, function := range semantic.Functions {
		if function.Promoted && strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, qualified := range names {
		function := semantic.Functions[qualified]
		positions := make([]string, 0, len(function.ErrorResults))
		for _, result := range function.ErrorResults {
			positions = append(positions, strconv.Itoa(result.Position))
		}
		properties := map[string]string{"promoted": "true", "error_evidence": "go/types"}
		if len(positions) > 0 {
			properties["returns_error"] = "true"
			properties["error_result_positions"] = strings.Join(positions, ",")
		}
		nodeID := b.AddNode(graph.Node{Kind: graph.KindMethod, Name: graph.SimpleName(qualified),
			QualifiedName: qualified, Location: loc, Properties: properties})
		b.AddFact(parentID, graph.EdgeHasField, nodeID, "", "", loc, map[string]string{"promoted": "true"})
		for _, failure := range semantic.Failures {
			if failure.Function == qualified {
				b.AddFact(nodeID, failure.Kind, "", failure.Target, failure.TargetKind,
					failure.Location, cloneStringMap(failure.Properties))
			}
		}
	}
}

func parseCall(b *parserapi.Builder, fset *token.FileSet, input parserapi.Input, pkg string, imports map[string]string, semantic SemanticView, fromID, receiverVariable, receiverQualified string, bindings functionBindings, call *goast.CallExpr) {
	callee, proven, ignore := resolvedGoCallee(call, fset, pkg, imports, semantic, receiverVariable, receiverQualified, bindings.types)
	if ignore {
		return
	}
	loc := location(input.Path, fset, call.Pos(), call.End())
	if callee == "os.Getenv" || callee == "os.LookupEnv" {
		if key, ok := stringArgument(call.Args, 0); ok {
			b.AddFact(fromID, graph.EdgeReadsConfig, "", key, graph.KindConfigKey, loc, nil)
		}
		return
	}
	if callee == "http.HandleFunc" || callee == "http.Handle" || callee == "net/http.HandleFunc" || callee == "net/http.Handle" {
		if route, ok := stringArgument(call.Args, 0); ok {
			endpointID := addEndpoint(b, loc, "ANY", route)
			b.AddFact(fromID, graph.EdgeExposes, endpointID, "", "", loc, nil)
			if len(call.Args) > 1 {
				b.AddFact(endpointID, graph.EdgeHandledBy, "", render(fset, call.Args[1]), "", loc, nil)
			}
		}
		return
	}
	method := strings.ToLower(graph.SimpleName(callee))
	if isHTTPMethod(method) && len(call.Args) > 0 && (strings.HasPrefix(callee, "net/http.") || strings.HasPrefix(callee, "http.")) {
		if route, ok := stringArgument(call.Args, 0); ok {
			addHTTPRequest(b, fromID, loc, method, route)
			return
		}
	}
	if isHTTPMethod(method) && len(call.Args) > 1 {
		if route, ok := stringArgument(call.Args, 0); ok {
			if strings.HasPrefix(route, "/") {
				endpointID := addEndpoint(b, loc, strings.ToUpper(method), route)
				b.AddFact(fromID, graph.EdgeExposes, endpointID, "", "", loc, nil)
				b.AddFact(endpointID, graph.EdgeHandledBy, "", render(fset, call.Args[len(call.Args)-1]), "", loc, nil)
				return
			}
		}
	}
	if event, ok := stringArgument(call.Args, 0); ok {
		switch method {
		case "publish", "publishevent", "emit", "produce":
			b.AddFact(fromID, graph.EdgePublishes, "", event, graph.KindEvent, loc, nil)
			return
		case "subscribe", "on", "consume":
			b.AddFact(fromID, graph.EdgeSubscribes, "", event, graph.KindEvent, loc, nil)
			return
		}
	}
	properties := map[string]string(nil)
	if proven {
		properties = map[string]string{"resolution": "go/types"}
	}
	b.AddFact(fromID, graph.EdgeCalls, "", callee, "", loc, properties)
}

func resolvedGoCallee(call *goast.CallExpr, fset *token.FileSet, pkg string, imports map[string]string, semantic SemanticView, receiverVariable, receiverQualified string, types map[string]string) (string, bool, bool) {
	if semantic.Available {
		offset := fset.Position(call.Lparen).Offset
		if evidence, exists := semantic.Calls[offset]; exists {
			if evidence.Ignore {
				return "", false, true
			}
			if evidence.Target != "" {
				return evidence.Target, true, false
			}
		}
		// Once a package was type checked, absence of object identity is
		// meaningful. Keep an explicit syntactic target instead of applying
		// receiver-name or constructor-name guesses.
		callee := render(fset, call.Fun)
		if ident, ok := call.Fun.(*goast.Ident); ok && !strings.Contains(ident.Name, ".") {
			callee = pkg + "." + ident.Name
		}
		return callee, false, false
	}
	callee := render(fset, call.Fun)
	if selector, ok := call.Fun.(*goast.SelectorExpr); ok {
		if ident, ok := selector.X.(*goast.Ident); ok {
			if ident.Name == receiverVariable && receiverQualified != "" {
				callee = receiverQualified + "." + selector.Sel.Name
			} else if inferred := types[ident.Name]; inferred != "" {
				callee = inferred + "." + selector.Sel.Name
			} else if imported, exists := imports[ident.Name]; exists {
				callee = imported + "." + selector.Sel.Name
			}
		} else if constructor, ok := selector.X.(*goast.CallExpr); ok {
			// Resolve the common pkg.NewType(...).Method(...) form without a full
			// type checker. The constructor name is structural evidence for the
			// returned receiver; anything less specific stays unresolved.
			if constructorSelector, ok := constructor.Fun.(*goast.SelectorExpr); ok {
				if packageIdent, ok := constructorSelector.X.(*goast.Ident); ok {
					if imported, exists := imports[packageIdent.Name]; exists && strings.HasPrefix(constructorSelector.Sel.Name, "New") {
						typeName := strings.TrimPrefix(constructorSelector.Sel.Name, "New")
						if typeName != "" {
							callee = imported + "." + typeName + "." + selector.Sel.Name
						}
					}
				}
			}
		}
	}
	if ident, ok := call.Fun.(*goast.Ident); ok && !strings.Contains(ident.Name, ".") {
		callee = pkg + "." + ident.Name
	}
	return callee, false, false
}

func inferGoExprType(expr goast.Expr, fset *token.FileSet, pkg string, imports, types map[string]string) string {
	switch value := expr.(type) {
	case *goast.CompositeLit:
		return qualifyGoType(render(fset, value.Type), pkg, imports)
	case *goast.UnaryExpr:
		return inferGoExprType(value.X, fset, pkg, imports, types)
	case *goast.Ident:
		return types[value.Name]
	case *goast.CallExpr:
		if ident, ok := value.Fun.(*goast.Ident); ok {
			if ident.Name == "new" && len(value.Args) > 0 {
				return qualifyGoType(render(fset, value.Args[0]), pkg, imports)
			}
			if strings.HasPrefix(ident.Name, "New") && len(ident.Name) > 3 {
				return pkg + "." + strings.TrimPrefix(ident.Name, "New")
			}
		}
		if selector, ok := value.Fun.(*goast.SelectorExpr); ok {
			if packageIdent, ok := selector.X.(*goast.Ident); ok && strings.HasPrefix(selector.Sel.Name, "New") {
				if imported := imports[packageIdent.Name]; imported != "" {
					return imported + "." + strings.TrimPrefix(selector.Sel.Name, "New")
				}
			}
		}
	}
	return ""
}

func qualifyGoType(typeText, pkg string, imports map[string]string) string {
	typeText = strings.TrimSpace(typeText)
	typeText = strings.TrimLeft(typeText, "*[]")
	if index := strings.Index(typeText, "["); index >= 0 {
		typeText = typeText[:index]
	}
	if prefix, rest, ok := strings.Cut(typeText, "."); ok {
		if imported := imports[prefix]; imported != "" {
			return imported + "." + rest
		}
		return typeText
	}
	if typeText == "" || isBuiltinGoType(typeText) {
		return ""
	}
	return pkg + "." + typeText
}

func isBuiltinGoType(value string) bool {
	switch value {
	case "bool", "byte", "complex64", "complex128", "error", "float32", "float64", "int", "int8", "int16", "int32", "int64", "rune", "string", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "any":
		return true
	default:
		return false
	}
}

func addEndpoint(b *parserapi.Builder, loc graph.Location, method, route string) string {
	normalizedMethod, methodErr := httpmodel.NormalizeMethod(method)
	parsedRoute, routeErr := httpmodel.ParseRoute(route)
	properties := map[string]string{"raw_method": method, "raw_route": route}
	identityMethod, identityRoute := normalizedMethod, route
	if methodErr == nil {
		properties["method"] = normalizedMethod
	} else {
		identityMethod = method
		properties["method"] = method
	}
	if routeErr == nil {
		identityRoute = parsedRoute.Canonical
		properties["route"] = parsedRoute.Canonical
		copyHTTPRouteEvidence(properties, parsedRoute, "")
	} else {
		properties["route"] = route
	}
	if methodErr != nil || routeErr != nil {
		properties["http_invalid"] = "true"
		b.Diagnostic(loc.Line, "warning", fmt.Sprintf("invalid HTTP endpoint %q %q: %v", method, route, firstError(methodErr, routeErr)))
	}
	name := identityMethod + " " + identityRoute
	return b.AddNode(graph.Node{Kind: graph.KindEndpoint, Name: name,
		QualifiedName: fmt.Sprintf("endpoint:%s@%s:%d:%d", name, loc.Path, loc.Line, loc.Column), Location: loc,
		Properties: properties})
}

func addHTTPRequest(b *parserapi.Builder, fromID string, loc graph.Location, method, route string) {
	normalizedMethod, methodErr := httpmodel.NormalizeMethod(method)
	parsedRoute, routeErr := httpmodel.ParseRoute(route)
	properties := map[string]string{"http_raw_method": method, "http_raw_route": route}
	identityMethod := normalizedMethod
	if methodErr != nil {
		identityMethod = method
	} else {
		properties["http_method"] = normalizedMethod
	}
	identityRoute := route
	if routeErr == nil {
		copyHTTPRouteEvidence(properties, parsedRoute, "http_")
		identityRoute = parsedRoute.Canonical
		if parsedRoute.Authority != "" {
			identityRoute = parsedRoute.Scheme + "://" + parsedRoute.Authority + parsedRoute.Canonical
		}
	} else {
		properties["http_invalid"] = "true"
	}
	if methodErr != nil || routeErr != nil {
		properties["http_invalid"] = "true"
		b.Diagnostic(loc.Line, "warning", fmt.Sprintf("invalid outbound HTTP request %q %q: %v", method, route, firstError(methodErr, routeErr)))
	}
	b.AddFact(fromID, graph.EdgeRequests, "", identityMethod+" "+identityRoute, graph.KindEndpoint, loc, properties)
}

func copyHTTPRouteEvidence(properties map[string]string, route httpmodel.Route, prefix string) {
	properties[prefix+"route"] = route.Canonical
	if route.Query != "" {
		properties[prefix+"query"] = route.Query
	}
	if route.Fragment != "" {
		properties[prefix+"fragment"] = route.Fragment
	}
	if route.Authority != "" {
		properties[prefix+"authority"] = route.Authority
		properties[prefix+"scheme"] = route.Scheme
	}
}

func firstError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func isHTTPMethod(method string) bool {
	switch method {
	case "get", "post", "put", "patch", "delete", "head", "options":
		return true
	default:
		return false
	}
}

func stringArgument(args []goast.Expr, index int) (string, bool) {
	if index >= len(args) {
		return "", false
	}
	lit, ok := args[index].(*goast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

func receiverName(expr goast.Expr) string {
	switch value := expr.(type) {
	case *goast.Ident:
		return value.Name
	case *goast.StarExpr:
		return receiverName(value.X)
	case *goast.IndexExpr:
		return receiverName(value.X)
	case *goast.IndexListExpr:
		return receiverName(value.X)
	default:
		return "receiver"
	}
}

func render(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

func location(path string, fset *token.FileSet, start, end token.Pos) graph.Location {
	s := fset.Position(start)
	e := fset.Position(end)
	return graph.Location{Path: path, Line: s.Line, Column: s.Column, EndLine: e.Line}
}
