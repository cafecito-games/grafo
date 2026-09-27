package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	goast "go/ast"
	"go/build"
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

type SemanticImplementation struct {
	Concrete  string
	Interface string
}

// SemanticView contains only the evidence needed while parsing one file. It
// deliberately does not retain go/ast or go/types graphs after loading.
type SemanticView struct {
	Available       bool
	Included        bool
	PackagePath     string
	BuildContext    string
	Calls           map[int]SemanticCall
	Implementations []SemanticImplementation
	Diagnostics     []graph.Diagnostic
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
	copyView.Diagnostics = append([]graph.Diagnostic(nil), view.Diagnostics...)
	return copyView
}

func safeLoadWorkspace(ctx context.Context, root, buildContext string) (views map[string]SemanticView, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("Go semantic loader panic: %v", recovered)
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
		views[path] = view
	}
	collectPackageDiagnostics(root, pkg, views, buildContext)
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
		if filepath.Ext(name) != ".go" && name != "go.mod" && name != "go.sum" && name != "go.work" && name != "go.work.sum" && !(name == "modules.txt" && filepath.Base(filepath.Dir(path)) == "vendor") {
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
