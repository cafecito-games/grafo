package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	goast "go/ast"
	"go/build"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

// SemanticLoader separates package/type loading from syntax extraction. Tests
// can supply compact evidence without invoking the Go toolchain.
type SemanticLoader interface {
	Load(context.Context, parserapi.Input) (SemanticView, error)
}

type SemanticCall struct {
	Target string
	Ignore bool
}

// SemanticChiMiddleware is compact, go/types-backed evidence for one
// middleware inherited by a composed Chi endpoint.
type SemanticChiMiddleware struct {
	Target     string
	TargetKind graph.NodeKind
	Form       string
	Location   graph.Location
	Unresolved bool
}

// SemanticChiEndpoint is the final package-local composition of a Chi route.
// It deliberately retains no AST or types objects after package loading.
type SemanticChiEndpoint struct {
	Function     string
	FunctionKind graph.NodeKind
	Method       string
	Route        string
	Handler      string
	HandlerKind  graph.NodeKind
	Location     graph.Location
	Middleware   []SemanticChiMiddleware
	Conditional  bool
	Unresolved   bool
}

// SemanticHTTPRequest is compact, go/types-backed evidence for one proven
// outbound request at its highest package-local application callsite.
type SemanticHTTPRequest struct {
	Function         string
	FunctionKind     graph.NodeKind
	Method           string
	Route            string
	Sink             string
	Source           string
	Wrappers         []string
	Location         graph.Location
	AuthorityUnknown bool
	Conditional      bool
}

// SemanticProtocolUse is compact go/types evidence for one possible protocol
// operation. Binding names a generated Go symbol; the parser maps it through
// the Protobuf binding registry before any canonical fact is emitted.
type SemanticProtocolUse struct {
	Function   string
	Kind       graph.EdgeKind
	Binding    string
	Form       string
	API        string
	StaticType string
	Location   graph.Location
}

type SemanticImplementation struct {
	Concrete  string
	Interface string
}

type SemanticErrorResult struct {
	Position int
	Type     string
}

type SemanticFunction struct {
	ErrorResults []SemanticErrorResult
	Promoted     bool
}

type SemanticErrorDeclaration struct {
	Name          string
	QualifiedName string
	Kind          graph.NodeKind
	Type          string
	Location      graph.Location
	Sentinel      bool
}

// SemanticFailure is compact, evidence-backed output from the Go failure
// extractor. Function maps back to the declaration node owned by the syntax
// parser; Target is a stable go/types identity or an explicit unresolved
// boundary.
type SemanticFailure struct {
	Function   string
	Kind       graph.EdgeKind
	Target     string
	TargetKind graph.NodeKind
	Location   graph.Location
	Properties map[string]string
}

// SemanticView contains only the evidence needed while parsing one file. It
// deliberately does not retain go/ast or go/types graphs after loading.
type SemanticView struct {
	Available         bool
	Included          bool
	PackagePath       string
	BuildContext      string
	Calls             map[int]SemanticCall
	ProtocolUses      []SemanticProtocolUse
	Implementations   []SemanticImplementation
	Functions         map[string]SemanticFunction
	ErrorDeclarations []SemanticErrorDeclaration
	Failures          []SemanticFailure
	ChiEndpoints      []SemanticChiEndpoint
	ChiEndpointCalls  map[int]bool
	NonChiHTTPCalls   map[int]bool
	HTTPRequests      []SemanticHTTPRequest
	HTTPRequestCalls  map[int]bool
	Diagnostics       []graph.Diagnostic
}

type SemanticLoadMetrics struct {
	Loads          int64
	CacheHits      int64
	PeakConcurrent int64
	LastDurationMS int64
}

type cachedWorkspace struct {
	key   string
	views map[string]SemanticView
}

// PackageLoader is the production go/packages adapter. A single load at a time
// bounds the large syntax/type graph, and cached entries retain compact views
// rather than package objects.
type PackageLoader struct {
	mu    sync.Mutex
	cache map[string]cachedWorkspace

	loads          atomic.Int64
	cacheHits      atomic.Int64
	active         atomic.Int64
	peakConcurrent atomic.Int64
	lastDurationMS atomic.Int64
}

var packageLoadGate = make(chan struct{}, 1)

func NewPackageLoader() *PackageLoader {
	return &PackageLoader{cache: map[string]cachedWorkspace{}}
}

func (l *PackageLoader) Metrics() SemanticLoadMetrics {
	return SemanticLoadMetrics{
		Loads: l.loads.Load(), CacheHits: l.cacheHits.Load(),
		PeakConcurrent: l.peakConcurrent.Load(), LastDurationMS: l.lastDurationMS.Load(),
	}
}

func (l *PackageLoader) Load(ctx context.Context, input parserapi.Input) (SemanticView, error) {
	if input.Root == "" || input.Path == "" {
		return SemanticView{}, nil
	}
	root, err := filepath.Abs(input.Root)
	if err != nil {
		return SemanticView{}, err
	}
	key := input.SemanticKey
	buildContext := buildContextString(root)
	if key == "" {
		var err error
		key, buildContext, err = semanticWorkspaceKey(root)
		if err != nil {
			return SemanticView{}, err
		}
	}
	path := filepath.ToSlash(filepath.Clean(input.Path))
	if view, ok := l.cached(root, key, path); ok {
		return view, nil
	}
	select {
	case packageLoadGate <- struct{}{}:
		defer func() { <-packageLoadGate }()
	case <-ctx.Done():
		return SemanticView{}, ctx.Err()
	}
	if view, ok := l.cached(root, key, path); ok {
		return view, nil
	}

	started := time.Now()
	active := l.active.Add(1)
	for peak := l.peakConcurrent.Load(); active > peak && !l.peakConcurrent.CompareAndSwap(peak, active); peak = l.peakConcurrent.Load() {
	}
	views, loadErr := safeLoadWorkspace(ctx, root, buildContext)
	l.active.Add(-1)
	l.loads.Add(1)
	l.lastDurationMS.Store(time.Since(started).Milliseconds())
	if loadErr != nil {
		return SemanticView{}, loadErr
	}
	l.mu.Lock()
	l.cache[root] = cachedWorkspace{key: key, views: views}
	l.mu.Unlock()
	if view, ok := views[path]; ok {
		return cloneSemanticView(view), nil
	}
	return unloadedSemanticView(root, path, buildContext), nil
}

func (l *PackageLoader) cached(root, key, path string) (SemanticView, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.cache[root]
	if !ok || entry.key != key {
		return SemanticView{}, false
	}
	l.cacheHits.Add(1)
	view, exists := entry.views[path]
	if !exists {
		view = unloadedSemanticView(root, path, buildContextString(root))
	}
	return cloneSemanticView(view), true
}

func unloadedSemanticView(root, path, buildContext string) SemanticView {
	included, err := matchesBuildContext(root, path)
	if err == nil && !included {
		return SemanticView{Available: true, Included: false, BuildContext: buildContext}
	}
	message := "Go semantic package omitted source; using syntax evidence"
	if err != nil {
		message += ": " + err.Error()
	}
	return SemanticView{
		Included: true, BuildContext: buildContext,
		Diagnostics: []graph.Diagnostic{{Path: path, Level: "warning", Message: message}},
	}
}

func matchesBuildContext(root, path string) (bool, error) {
	context := build.Default
	if value := strings.TrimSpace(os.Getenv("GOOS")); value != "" {
		context.GOOS = value
	}
	if value := strings.TrimSpace(os.Getenv("GOARCH")); value != "" {
		context.GOARCH = value
	}
	if value := strings.TrimSpace(os.Getenv("CGO_ENABLED")); value != "" {
		context.CgoEnabled = value != "0"
	}
	context.BuildTags = goBuildTags(os.Getenv("GOFLAGS"))
	absolute := filepath.Join(root, filepath.FromSlash(path))
	return context.MatchFile(filepath.Dir(absolute), filepath.Base(absolute))
}

func goBuildTags(flags string) []string {
	fields := strings.Fields(flags)
	var tags []string
	for index := 0; index < len(fields); index++ {
		value := ""
		if strings.HasPrefix(fields[index], "-tags=") {
			value = strings.TrimPrefix(fields[index], "-tags=")
		} else if fields[index] == "-tags" && index+1 < len(fields) {
			index++
			value = fields[index]
		}
		value = strings.Trim(value, "'\"")
		for _, tag := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' }) {
			if tag != "" {
				tags = append(tags, tag)
			}
		}
	}
	return tags
}

func cloneSemanticView(view SemanticView) SemanticView {
	copyView := view
	copyView.Calls = make(map[int]SemanticCall, len(view.Calls))
	for offset, call := range view.Calls {
		copyView.Calls[offset] = call
	}
	copyView.Implementations = append([]SemanticImplementation(nil), view.Implementations...)
	copyView.ProtocolUses = append([]SemanticProtocolUse(nil), view.ProtocolUses...)
	copyView.Functions = make(map[string]SemanticFunction, len(view.Functions))
	for name, function := range view.Functions {
		function.ErrorResults = append([]SemanticErrorResult(nil), function.ErrorResults...)
		copyView.Functions[name] = function
	}
	copyView.ErrorDeclarations = append([]SemanticErrorDeclaration(nil), view.ErrorDeclarations...)
	copyView.Failures = make([]SemanticFailure, len(view.Failures))
	for index, failure := range view.Failures {
		copyView.Failures[index] = failure
		copyView.Failures[index].Properties = cloneStringMap(failure.Properties)
	}
	copyView.ChiEndpoints = make([]SemanticChiEndpoint, len(view.ChiEndpoints))
	for index, endpoint := range view.ChiEndpoints {
		copyView.ChiEndpoints[index] = endpoint
		copyView.ChiEndpoints[index].Middleware = append([]SemanticChiMiddleware(nil), endpoint.Middleware...)
	}
	copyView.ChiEndpointCalls = cloneBoolMap(view.ChiEndpointCalls)
	copyView.NonChiHTTPCalls = cloneBoolMap(view.NonChiHTTPCalls)
	copyView.HTTPRequests = make([]SemanticHTTPRequest, len(view.HTTPRequests))
	for index, request := range view.HTTPRequests {
		copyView.HTTPRequests[index] = request
		copyView.HTTPRequests[index].Wrappers = append([]string(nil), request.Wrappers...)
	}
	copyView.HTTPRequestCalls = cloneBoolMap(view.HTTPRequestCalls)
	copyView.Diagnostics = append([]graph.Diagnostic(nil), view.Diagnostics...)
	return copyView
}

func cloneBoolMap(values map[int]bool) map[int]bool {
	if len(values) == 0 {
		return nil
	}
	result := make(map[int]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func safeLoadWorkspace(ctx context.Context, root, buildContext string) (views map[string]SemanticView, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("recovered from Go semantic loader panic: %v", recovered)
		}
	}()
	return loadWorkspace(ctx, root, buildContext)
}

func loadWorkspace(ctx context.Context, root, buildContext string) (map[string]SemanticView, error) {
	environment := append([]string(nil), os.Environ()...)
	environment = setEnvironment(environment, "GOPROXY", "off")
	environment = setEnvironment(environment, "GOSUMDB", "off")
	environment = setEnvironment(environment, "GOTOOLCHAIN", "local")
	flags := strings.TrimSpace(os.Getenv("GOFLAGS"))
	if !strings.Contains(flags, "-mod=") {
		mode := "readonly"
		if info, err := os.Stat(filepath.Join(root, "vendor")); err == nil && info.IsDir() {
			mode = "vendor"
		}
		flags = strings.TrimSpace(flags + " -mod=" + mode)
	}
	environment = setEnvironment(environment, "GOFLAGS", flags)
	config := &packages.Config{
		Context: ctx,
		Dir:     root,
		Env:     environment,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes | packages.NeedModule,
		Tests: false,
	}
	patterns, err := workspacePackagePatterns(root)
	if err != nil {
		return nil, err
	}
	loaded, err := packages.Load(config, patterns...)
	if err != nil {
		return nil, err
	}
	views := map[string]SemanticView{}
	for _, pkg := range loaded {
		collectPackageViews(root, buildContext, pkg, views)
	}
	interfaces := loadedInterfaces(loaded)
	for _, pkg := range loaded {
		collectImplementations(root, pkg, interfaces, views)
	}
	for path, view := range views {
		sort.Slice(view.Implementations, func(i, j int) bool {
			if view.Implementations[i].Concrete == view.Implementations[j].Concrete {
				return view.Implementations[i].Interface < view.Implementations[j].Interface
			}
			return view.Implementations[i].Concrete < view.Implementations[j].Concrete
		})
		views[path] = view
	}
	return views, nil
}

func collectPackageViews(root, buildContext string, pkg *packages.Package, views map[string]SemanticView) {
	if pkg == nil {
		return
	}
	for index, file := range pkg.Syntax {
		if index >= len(pkg.CompiledGoFiles) {
			continue
		}
		path, ok := relativeSourcePath(root, pkg.CompiledGoFiles[index])
		if !ok {
			continue
		}
		view := views[path]
		view.Available = true
		view.Included = true
		view.PackagePath = pkg.PkgPath
		view.BuildContext = buildContext
		if view.Calls == nil {
			view.Calls = map[int]SemanticCall{}
		}
		collectCalls(pkg, file, view.Calls)
		// Protocol facts require a fully type-checked package. Other semantic
		// evidence remains useful for partially checked packages, but protocol
		// classification must never fill gaps with symbol spelling.
		if len(pkg.Errors) == 0 {
			collectProtocolUses(pkg, file, path, &view)
		}
		collectFailureView(root, pkg, file, path, &view)
		views[path] = view
	}
	if len(pkg.Errors) == 0 {
		collectChiPackageViews(root, pkg, views)
		collectHTTPPackageViews(root, pkg, views)
	}
	collectPackageDiagnostics(root, pkg, views, buildContext)
}

func collectProtocolUses(pkg *packages.Package, file *goast.File, path string, view *SemanticView) {
	if pkg.TypesInfo == nil || pkg.Fset == nil {
		return
	}
	for _, declaration := range file.Decls {
		switch value := declaration.(type) {
		case *goast.FuncDecl:
			function := objectTarget(pkg.TypesInfo.Defs[value.Name])
			if value.Body != nil {
				collectProtocolUsesInNode(pkg, path, function, value.Body, &view.ProtocolUses)
			}
		case *goast.GenDecl:
			collectProtocolUsesInNode(pkg, path, "", value, &view.ProtocolUses)
		}
	}
	sort.Slice(view.ProtocolUses, func(i, j int) bool {
		left, right := view.ProtocolUses[i], view.ProtocolUses[j]
		if left.Location.Line != right.Location.Line {
			return left.Location.Line < right.Location.Line
		}
		if left.Location.Column != right.Location.Column {
			return left.Location.Column < right.Location.Column
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Binding != right.Binding {
			return left.Binding < right.Binding
		}
		return left.Form < right.Form
	})
}

func collectProtocolUsesInNode(pkg *packages.Package, path, function string, node goast.Node, uses *[]SemanticProtocolUse) {
	info := pkg.TypesInfo
	writes := map[token.Pos]bool{}
	callBindings := map[types.Object]string{}
	callBindingWrites := map[types.Object]int{}
	goast.Inspect(node, func(current goast.Node) bool {
		switch value := current.(type) {
		case *goast.AssignStmt:
			for _, expression := range value.Lhs {
				markProtocolWrites(expression, writes)
			}
			for index, lhs := range value.Lhs {
				if index >= len(value.Rhs) {
					break
				}
				identifier, ok := lhs.(*goast.Ident)
				if !ok {
					continue
				}
				target := callableTarget(value.Rhs[index], info, callBindings)
				object := info.Defs[identifier]
				if object == nil {
					object = info.Uses[identifier]
				}
				if object != nil {
					callBindingWrites[object]++
					if target != "" {
						callBindings[object] = target
					}
				}
			}
		case *goast.IncDecStmt:
			markProtocolWrites(value.X, writes)
		case *goast.ValueSpec:
			for index, name := range value.Names {
				if index >= len(value.Values) {
					break
				}
				if target := callableTarget(value.Values[index], info, callBindings); target != "" {
					if object := info.Defs[name]; object != nil {
						callBindingWrites[object]++
						callBindings[object] = target
					}
				} else if object := info.Defs[name]; object != nil {
					callBindingWrites[object]++
				}
			}
		}
		return true
	})
	for object, count := range callBindingWrites {
		if count != 1 {
			delete(callBindings, object)
		}
	}
	add := func(kind graph.EdgeKind, binding, form, api string, static types.Type, start, end token.Pos) {
		if binding == "" || static == nil {
			return
		}
		*uses = append(*uses, SemanticProtocolUse{
			Function: function, Kind: kind, Binding: binding, Form: form, API: api,
			StaticType: semanticTypeString(static), Location: semanticLocation(path, pkg.Fset, start, end),
		})
	}
	goast.Inspect(node, func(current goast.Node) bool {
		switch value := current.(type) {
		case *goast.CallExpr:
			target := callableTarget(value.Fun, info, callBindings)
			switch target {
			case "google.golang.org/protobuf/proto.Marshal":
				if len(value.Args) > 0 {
					typeValue := info.TypeOf(value.Args[0])
					add(graph.EdgeEncodes, semanticNamedType(typeValue), "marshal", target, typeValue, value.Pos(), value.End())
				}
			case "google.golang.org/protobuf/proto.Unmarshal":
				if len(value.Args) > 1 {
					typeValue := info.TypeOf(value.Args[1])
					add(graph.EdgeDecodes, semanticNamedType(typeValue), "unmarshal", target, typeValue, value.Pos(), value.End())
				}
			default:
				selector, ok := value.Fun.(*goast.SelectorExpr)
				if !ok || info.Selections[selector] == nil {
					break
				}
				if _, ok := info.Selections[selector].Obj().(*types.Func); ok {
					typeValue := info.TypeOf(selector.X)
					add(graph.EdgeReads, target, "getter", target, typeValue, value.Pos(), value.End())
				}
			}
		case *goast.CompositeLit:
			typeValue := info.TypeOf(value)
			owner := semanticNamedType(typeValue)
			add(graph.EdgeWrites, owner, "composite_literal_type", "go.composite_literal", typeValue, value.Pos(), value.End())
			for _, element := range value.Elts {
				keyed, ok := element.(*goast.KeyValueExpr)
				if !ok {
					continue
				}
				identifier, ok := keyed.Key.(*goast.Ident)
				if !ok {
					continue
				}
				field, ok := info.Uses[identifier].(*types.Var)
				if !ok || !field.IsField() {
					continue
				}
				add(graph.EdgeWrites, owner+"."+field.Name(), "composite_literal", "go.composite_literal", typeValue, keyed.Pos(), keyed.End())
			}
		case *goast.SelectorExpr:
			selection := info.Selections[value]
			if selection == nil {
				break
			}
			field, ok := selection.Obj().(*types.Var)
			if !ok || !field.IsField() {
				break
			}
			typeValue := info.TypeOf(value.X)
			kind := graph.EdgeReads
			if writes[value.Pos()] {
				kind = graph.EdgeWrites
			}
			add(kind, semanticNamedType(selection.Recv())+"."+field.Name(), "field_selection", "go.field", typeValue, value.Pos(), value.End())
		case *goast.TypeSwitchStmt:
			for _, statement := range value.Body.List {
				clause, ok := statement.(*goast.CaseClause)
				if !ok {
					continue
				}
				for _, expression := range clause.List {
					typeValue := info.TypeOf(expression)
					add(graph.EdgeReads, semanticNamedType(typeValue), "type_switch", "go.type_switch", typeValue, expression.Pos(), expression.End())
				}
			}
		}
		return true
	})
}

func markProtocolWrites(expression goast.Expr, writes map[token.Pos]bool) {
	switch value := expression.(type) {
	case *goast.SelectorExpr:
		writes[value.Pos()] = true
	case *goast.IndexExpr:
		markProtocolWrites(value.X, writes)
	case *goast.ParenExpr:
		markProtocolWrites(value.X, writes)
	}
}

func semanticNamedType(value types.Type) string {
	named := namedType(value)
	if named == nil || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path() + "." + named.Obj().Name()
}

func semanticTypeString(value types.Type) string {
	return types.TypeString(value, func(pkg *types.Package) string { return pkg.Path() })
}

func semanticLocation(path string, fset *token.FileSet, start, end token.Pos) graph.Location {
	from, to := fset.Position(start), fset.Position(end)
	return graph.Location{Path: path, Line: from.Line, Column: from.Column, EndLine: to.Line}
}

func workspacePackagePatterns(root string) ([]string, error) {
	workspace := discoverGoWorkspace(root)
	if workspace == "" || workspace == "off" {
		return []string{"./..."}, nil
	}
	content, err := os.ReadFile(workspace)
	if err != nil {
		return nil, err
	}
	parsed, err := modfile.ParseWork(workspace, content, nil)
	if err != nil {
		return nil, err
	}
	workspaceDir := filepath.Dir(workspace)
	seen := map[string]bool{}
	var patterns []string
	for _, use := range parsed.Use {
		moduleDir := use.Path
		if !filepath.IsAbs(moduleDir) {
			moduleDir = filepath.Join(workspaceDir, moduleDir)
		}
		relative, err := filepath.Rel(root, moduleDir)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		pattern := "./..."
		if relative != "." {
			pattern = "./" + filepath.ToSlash(relative) + "/..."
		}
		if !seen[pattern] {
			seen[pattern] = true
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		return []string{"./..."}, nil
	}
	sort.Strings(patterns)
	return patterns, nil
}

func collectCalls(pkg *packages.Package, file *goast.File, calls map[int]SemanticCall) {
	if pkg.TypesInfo == nil || pkg.Fset == nil {
		return
	}
	bindings := map[types.Object]string{}
	goast.Inspect(file, func(node goast.Node) bool {
		switch value := node.(type) {
		case *goast.AssignStmt:
			for index, lhs := range value.Lhs {
				if index >= len(value.Rhs) {
					break
				}
				target := callableTarget(value.Rhs[index], pkg.TypesInfo, bindings)
				ident, ok := lhs.(*goast.Ident)
				if !ok || target == "" {
					continue
				}
				object := pkg.TypesInfo.Defs[ident]
				if object == nil {
					object = pkg.TypesInfo.Uses[ident]
				}
				if object != nil {
					bindings[object] = target
				}
			}
		case *goast.ValueSpec:
			for index, name := range value.Names {
				if index >= len(value.Values) {
					break
				}
				if target := callableTarget(value.Values[index], pkg.TypesInfo, bindings); target != "" {
					if object := pkg.TypesInfo.Defs[name]; object != nil {
						bindings[object] = target
					}
				}
			}
		}
		return true
	})
	goast.Inspect(file, func(node goast.Node) bool {
		call, ok := node.(*goast.CallExpr)
		if !ok {
			return true
		}
		target := callableTarget(call.Fun, pkg.TypesInfo, bindings)
		position := pkg.Fset.Position(call.Lparen)
		if target == "" && pkg.TypesInfo.Types[call.Fun].IsType() {
			calls[position.Offset] = SemanticCall{Ignore: true}
			return true
		}
		if target == "" {
			return true
		}
		calls[position.Offset] = SemanticCall{Target: target}
		return true
	})
}

func callableTarget(expression goast.Expr, info *types.Info, bindings map[types.Object]string) string {
	for {
		switch value := expression.(type) {
		case *goast.IndexExpr:
			expression = value.X
			continue
		case *goast.IndexListExpr:
			expression = value.X
			continue
		case *goast.ParenExpr:
			expression = value.X
			continue
		}
		break
	}
	switch value := expression.(type) {
	case *goast.Ident:
		object := info.Uses[value]
		if target := bindings[object]; target != "" {
			return target
		}
		return objectTarget(object)
	case *goast.SelectorExpr:
		if selection := info.Selections[value]; selection != nil {
			return objectTarget(selection.Obj())
		}
		return objectTarget(info.Uses[value.Sel])
	default:
		return ""
	}
}

func objectTarget(object types.Object) string {
	switch value := object.(type) {
	case *types.Builtin:
		return "builtin." + value.Name()
	case *types.Func:
		pkg := value.Pkg()
		if pkg == nil {
			return "builtin." + value.Name()
		}
		signature, _ := value.Type().(*types.Signature)
		if signature == nil || signature.Recv() == nil {
			return pkg.Path() + "." + value.Name()
		}
		if receiver := namedType(signature.Recv().Type()); receiver != nil && receiver.Obj().Pkg() != nil {
			return receiver.Obj().Pkg().Path() + "." + receiver.Obj().Name() + "." + value.Name()
		}
		return pkg.Path() + "." + value.Name()
	default:
		return ""
	}
}

func namedType(value types.Type) *types.Named {
	if pointer, ok := value.(*types.Pointer); ok {
		value = pointer.Elem()
	}
	value = types.Unalias(value)
	named, _ := value.(*types.Named)
	if named != nil && named.Origin() != nil {
		return named.Origin()
	}
	return named
}

type semanticInterface struct {
	qualified string
	typeValue *types.Interface
}

func loadedInterfaces(packagesToInspect []*packages.Package) []semanticInterface {
	var result []semanticInterface
	for _, pkg := range packagesToInspect {
		if pkg == nil || pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			object, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			iface, ok := types.Unalias(object.Type()).Underlying().(*types.Interface)
			if !ok || iface.NumMethods() == 0 {
				continue
			}
			iface.Complete()
			result = append(result, semanticInterface{qualified: pkg.PkgPath + "." + name, typeValue: iface})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].qualified < result[j].qualified })
	return result
}

func collectImplementations(root string, pkg *packages.Package, interfaces []semanticInterface, views map[string]SemanticView) {
	if pkg.Types == nil || pkg.Fset == nil || len(interfaces) == 0 {
		return
	}
	scope := pkg.Types.Scope()
	for _, name := range scope.Names() {
		object, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || object.IsAlias() {
			continue
		}
		named, ok := object.Type().(*types.Named)
		if !ok {
			continue
		}
		if _, isInterface := named.Underlying().(*types.Interface); isInterface {
			continue
		}
		position := pkg.Fset.Position(object.Pos())
		path, ok := relativeSourcePath(root, position.Filename)
		if !ok {
			continue
		}
		concrete := pkg.PkgPath + "." + name
		view := views[path]
		for _, iface := range interfaces {
			if types.Implements(named, iface.typeValue) || types.Implements(types.NewPointer(named), iface.typeValue) {
				view.Implementations = append(view.Implementations, SemanticImplementation{
					Concrete: concrete, Interface: iface.qualified,
				})
			}
		}
		views[path] = view
	}
}

func collectPackageDiagnostics(root string, pkg *packages.Package, views map[string]SemanticView, buildContext string) {
	for _, packageError := range pkg.Errors {
		path, line := diagnosticPosition(root, packageError.Pos)
		if path == "" {
			for _, filename := range pkg.CompiledGoFiles {
				if relative, ok := relativeSourcePath(root, filename); ok {
					path = relative
					break
				}
			}
		}
		if path == "" {
			continue
		}
		view := views[path]
		view.Available = true
		view.Included = true
		view.BuildContext = buildContext
		view.Diagnostics = append(view.Diagnostics, graph.Diagnostic{
			Path: path, Line: line, Level: "warning", Message: packageError.Msg,
		})
		views[path] = view
	}
}

func diagnosticPosition(root, raw string) (string, int) {
	if raw == "" || raw == "-" {
		return "", 0
	}
	filename := raw
	line := 0
	if columnSeparator := strings.LastIndex(filename, ":"); columnSeparator >= 0 {
		if _, err := strconv.Atoi(filename[columnSeparator+1:]); err == nil {
			withoutColumn := filename[:columnSeparator]
			if lineSeparator := strings.LastIndex(withoutColumn, ":"); lineSeparator >= 0 {
				if parsed, err := strconv.Atoi(withoutColumn[lineSeparator+1:]); err == nil {
					line = parsed
					filename = withoutColumn[:lineSeparator]
				}
			}
		}
	}
	path, _ := relativeSourcePath(root, filename)
	return path, line
}

func relativeSourcePath(root, filename string) (string, bool) {
	if filename == "" {
		return "", false
	}
	relative, err := filepath.Rel(root, filename)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func semanticWorkspaceKey(root string) (string, string, error) {
	buildContext := buildContextString(root)
	digest := sha256.New()
	_, _ = digest.Write([]byte(buildContext))
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".grafo", ".worktrees":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		name := entry.Name()
		if filepath.Ext(name) != ".go" && name != "go.mod" && name != "go.sum" && name != "go.work" && name != "go.work.sum" && (name != "modules.txt" || filepath.Base(filepath.Dir(path)) != "vendor") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = digest.Write([]byte(filepath.ToSlash(relative)))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write(content)
		_, _ = digest.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", "", err
	}
	workspace := discoverGoWorkspace(root)
	if workspace != "" && workspace != "off" {
		for _, path := range []string{workspace, workspace + ".sum"} {
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			_, _ = digest.Write([]byte(filepath.Clean(path)))
			_, _ = digest.Write([]byte{0})
			_, _ = digest.Write(content)
			_, _ = digest.Write([]byte{0})
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), buildContext, nil
}

func buildContextString(root ...string) string {
	goos := strings.TrimSpace(os.Getenv("GOOS"))
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := strings.TrimSpace(os.Getenv("GOARCH"))
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	cgo := strings.TrimSpace(os.Getenv("CGO_ENABLED"))
	if cgo == "" {
		if build.Default.CgoEnabled {
			cgo = "1"
		} else {
			cgo = "0"
		}
	}
	workspace := strings.TrimSpace(os.Getenv("GOWORK"))
	if len(root) > 0 {
		if discovered := discoverGoWorkspace(root[0]); discovered != "" {
			workspace = discovered
		}
	}
	return strings.Join([]string{
		"goos=" + goos,
		"goarch=" + goarch,
		"cgo=" + cgo,
		"goflags=" + strings.TrimSpace(os.Getenv("GOFLAGS")),
		"gowork=" + workspace,
		"toolchain=" + runtime.Version(),
	}, ";")
}

func discoverGoWorkspace(root string) string {
	configured := strings.TrimSpace(os.Getenv("GOWORK"))
	if configured == "off" {
		return "off"
	}
	if configured != "" && configured != "auto" {
		if absolute, err := filepath.Abs(configured); err == nil {
			return filepath.Clean(absolute)
		}
		return configured
	}
	directory, err := filepath.Abs(root)
	if err != nil {
		return configured
	}
	for {
		candidate := filepath.Join(directory, "go.work")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return configured
		}
		directory = parent
	}
}

func setEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	for index, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			environment[index] = prefix + value
			return environment
		}
	}
	return append(environment, prefix+value)
}

func semanticDependencies() []string {
	return []string{"go.mod", "go.sum", "go.work", "go.work.sum", "vendor/modules.txt"}
}

func isGoSemanticInput(path string) bool {
	path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
	if strings.EqualFold(filepath.Ext(path), ".go") {
		return true
	}
	base := filepath.Base(path)
	for _, dependency := range semanticDependencies() {
		if path == dependency || base == dependency {
			return true
		}
	}
	return false
}

var _ SemanticLoader = (*PackageLoader)(nil)
