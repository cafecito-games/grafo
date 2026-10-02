package golang

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Go invalidation is asymmetric, and this file is where that asymmetry lives.
//
// A Go fact in package P can be changed by three different things, and each one
// has a different blast radius:
//
//   - P's own contents. Statement-level analyzers only walk the package they
//     belong to, so a function body is a package-local fact. packageScopeKey
//     covers this by digesting the whole directory.
//   - The declarations of the packages P imports, transitively. Identifier
//     resolution, call targets, promoted method sets, and constant folding all
//     read them. A package P cannot reach is irrelevant to P, which is what
//     bounds this to the import closure.
//   - The repository's type universe. This one is genuinely global:
//     collectImplementations matches every concrete type against the interfaces
//     declared in *every* loaded package, so adding a method to a type in P can
//     create an `implements` edge to an interface in a package P never imports.
//     An interface cannot be reached by import edges, so it stays repository
//     wide.
//
// Digesting all three into one repository-wide key, as this package used to,
// is correct but makes every Go declaration edit reparse every Go file: on a
// 451-file repository, adding one exported function reparsed 312 files and took
// longer than a cold index. Splitting them keeps correctness and makes the cost
// proportional to what an edit can actually reach.

// goScopeModelVersion tags the encoding of the model below.
const goScopeModelVersion = "go-scope-model-v1"

// goScopeModel is one snapshot of the Go semantic inputs of a repository.
//
// It is built by WorkspaceSemanticKey, which already has to read every Go source
// to fingerprint them, so the import graph it records costs nothing beyond the
// parse that was happening anyway.
type goScopeModel struct {
	// RepositoryKey covers the inputs no import edge can bound: the build
	// context, every module and vendor manifest, the Go workspace files, and the
	// repository's type universe.
	RepositoryKey string
	// BuildContext is returned alongside the key by the legacy call shape.
	BuildContext string
	// packageSurface maps a package directory to the digest of its files'
	// declaration surfaces. Package-local bodies are deliberately absent: they
	// belong to packageScopeKey.
	packageSurface map[string]string
	// imports and importers are the local package graph. Imports that leave the
	// repository are dropped: the manifests that select them are digested in
	// full by RepositoryKey.
	imports   map[string][]string
	importers map[string][]string
	// opaque marks a package whose imports could not be read, because one of its
	// files does not parse. Such a package may depend on anything, so every
	// closure that reaches it falls back to the whole repository.
	opaque map[string]bool
	// wholeRepository is the digest of every package surface, used as the
	// closure of an opaque package.
	wholeRepository string

	closureMu sync.Mutex
	closure   map[string]string
}

// Closure returns the digest of the declaration surfaces that package directory
// can reach by importing, excluding the package itself: its own contents are
// already covered in full by packageScopeKey.
//
// An opaque package, or a closure that reaches one, degrades to the whole
// repository rather than guessing at a narrower answer.
func (m *goScopeModel) Closure(directory string) string {
	if m == nil {
		return ""
	}
	m.closureMu.Lock()
	defer m.closureMu.Unlock()
	if cached, ok := m.closure[directory]; ok {
		return cached
	}
	reached := map[string]bool{}
	opaque := m.collect(directory, reached)
	delete(reached, directory)
	digest := newSemanticDigest(goScopeModelVersion)
	if opaque {
		digest.writeField("opaque")
		digest.writeField(m.wholeRepository)
	} else {
		digest.writeField("closure")
		for _, reachedDirectory := range sortedSet(reached) {
			digest.writeField(reachedDirectory)
			digest.writeField(m.packageSurface[reachedDirectory])
		}
	}
	result := digest.sum()
	if m.closure == nil {
		m.closure = map[string]string{}
	}
	m.closure[directory] = result
	return result
}

// collect walks the import edges reachable from directory, reporting whether an
// opaque package was reached.
func (m *goScopeModel) collect(directory string, reached map[string]bool) bool {
	if reached[directory] {
		return false
	}
	reached[directory] = true
	if m.opaque[directory] {
		return true
	}
	opaque := false
	for _, imported := range m.imports[directory] {
		if m.collect(imported, reached) {
			opaque = true
		}
	}
	return opaque
}

// AffectedPackages returns every package directory that can observe a change in
// one of the given directories: the directories themselves plus their
// transitive importers. An opaque package is treated as importing everything, so
// it is always included.
func (m *goScopeModel) AffectedPackages(directories []string) []string {
	if m == nil {
		return nil
	}
	affected := map[string]bool{}
	frontier := make([]string, 0, len(directories))
	for _, directory := range directories {
		if !affected[directory] {
			affected[directory] = true
			frontier = append(frontier, directory)
		}
	}
	for len(frontier) > 0 {
		current := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, importer := range m.importers[current] {
			if affected[importer] {
				continue
			}
			affected[importer] = true
			frontier = append(frontier, importer)
		}
	}
	for directory := range m.opaque {
		affected[directory] = true
	}
	return sortedSet(affected)
}

// scopeModelCacheLimit bounds the number of repositories whose model is held.
// The model is a pure function of the files it read, so discarding one only
// costs the work of rebuilding it.
const scopeModelCacheLimit = 8

var (
	scopeModelMu    sync.Mutex
	scopeModelCache = map[string]*goScopeModel{}
)

// storeScopeModel publishes the model for root. WorkspaceSemanticKey calls this
// once per indexing run, which is what makes the model and the keys derived from
// it describe the same instant.
func storeScopeModel(root string, model *goScopeModel) {
	scopeModelMu.Lock()
	defer scopeModelMu.Unlock()
	if len(scopeModelCache) >= scopeModelCacheLimit {
		scopeModelCache = map[string]*goScopeModel{}
	}
	scopeModelCache[root] = model
}

func loadScopeModel(root string) (*goScopeModel, bool) {
	scopeModelMu.Lock()
	defer scopeModelMu.Unlock()
	model, ok := scopeModelCache[root]
	return model, ok
}

// scopeModelFor returns the model for root, building it when no run has
// published one. Callers that need the model to agree with a specific moment
// publish it themselves through WorkspaceSemanticKey.
func scopeModelFor(ctx context.Context, root string) (*goScopeModel, error) {
	if model, ok := loadScopeModel(root); ok {
		return model, nil
	}
	model, err := buildScopeModel(ctx, root)
	if err != nil {
		return nil, err
	}
	storeScopeModel(root, model)
	return model, nil
}

// buildScopeModel reads every Go source the toolchain compiles and splits its
// evidence into the three scopes described at the top of this file.
func buildScopeModel(ctx context.Context, root string) (*goScopeModel, error) {
	buildContext := buildContextString(root)
	repository := newSemanticDigest(goSemanticSurfaceVersion)
	repository.writeField(buildContext)
	paths, err := semanticRepositoryPaths(ctx, root)
	if err != nil {
		return nil, err
	}
	// Manifests select the module graph and load mode for every package, so they
	// remain fingerprinted in full, and only tracked manifests select anything.
	repository.writeField("manifests")
	for _, relative := range paths {
		if !isGoManifestInput(relative) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			continue
		}
		repository.writeField(relative)
		repository.writeBytes(content)
	}
	sources, err := goSemanticSourcePaths(ctx, root, paths)
	if err != nil {
		return nil, err
	}
	repository.writeField("unreadable")
	for _, condition := range sources.Unreadable {
		repository.writeField(condition)
	}

	model := &goScopeModel{
		BuildContext:   buildContext,
		packageSurface: map[string]string{},
		imports:        map[string][]string{},
		importers:      map[string][]string{},
		opaque:         map[string]bool{},
	}
	surfaces := map[string][]string{}
	importPaths := map[string]map[string]bool{}
	// The type universe is repository wide because interface satisfaction is
	// matched globally. It is accumulated in path order so the digest is stable.
	universe := newSemanticDigest(goScopeModelVersion)
	universe.writeField("types")
	for _, relative := range sources.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := goPackageDirectory(relative)
		content, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if readErr != nil {
			// A file listed on disk but unreadable now has no provable surface.
			// Recording the condition keeps it out of the silent-staleness class:
			// the key changes, and changes again when the file comes back.
			surfaces[directory] = append(surfaces[directory], relative+"\x00unreadable:"+readErr.Error())
			model.opaque[directory] = true
			continue
		}
		if isVendoredPath(relative) {
			// A vendor tree can dwarf the repository, and parsing a declaration
			// surface for each of its files is the single most expensive thing
			// this model does. Vendored code is never extracted and is never a
			// base package, so it contributes no interface to the universe; its
			// declarations still reach importers, so it keeps a package surface.
			surfaces[directory] = append(surfaces[directory], relative+"\x00"+vendoredSurfaceDigest(content))
			continue
		}
		surface, err := goFileSurface(relative, content)
		if err != nil {
			// A file that does not parse has no trustworthy surface and no
			// readable imports, so its package may depend on anything.
			surfaces[directory] = append(surfaces[directory], relative+"\x00"+surface.Surface)
			model.opaque[directory] = true
			universe.writeField(relative)
			universe.writeField(surface.Surface)
			continue
		}
		surfaces[directory] = append(surfaces[directory], relative+"\x00"+surface.Surface)
		if surface.Types != "" {
			universe.writeField(relative)
			universe.writeField(surface.Types)
		}
		if _, tracked := importPaths[directory]; !tracked {
			importPaths[directory] = map[string]bool{}
		}
		for _, imported := range surface.Imports {
			importPaths[directory][imported] = true
		}
	}
	repository.writeField("universe")
	repository.writeField(universe.sum())
	repository.writeField("workspace")
	for _, relative := range goWorkspaceManifestPaths(root, paths) {
		content, readErr := os.ReadFile(relative)
		if readErr != nil {
			continue
		}
		repository.writeField(filepath.Clean(relative))
		repository.writeBytes(content)
	}

	for directory, entries := range surfaces {
		sort.Strings(entries)
		digest := newSemanticDigest(goScopeModelVersion)
		digest.writeField(directory)
		for _, entry := range entries {
			digest.writeField(entry)
		}
		model.packageSurface[directory] = digest.sum()
	}
	resolver := newGoImportResolver(root, paths, model.packageSurface)
	for directory, imported := range importPaths {
		targets := map[string]bool{}
		for importPath := range imported {
			target, ok := resolver.resolve(directory, importPath)
			if !ok || target == directory {
				continue
			}
			targets[target] = true
		}
		model.imports[directory] = sortedSet(targets)
		for target := range targets {
			model.importers[target] = append(model.importers[target], directory)
		}
	}
	for target := range model.importers {
		sort.Strings(model.importers[target])
	}
	repository.writeField("broken-local-imports")
	sort.Strings(resolver.broken)
	for _, broken := range resolver.broken {
		repository.writeField(broken)
	}
	model.RepositoryKey = repository.sum()

	whole := newSemanticDigest(goScopeModelVersion)
	whole.writeField("whole-repository")
	for _, directory := range sortedKeys(model.packageSurface) {
		whole.writeField(directory)
		whole.writeField(model.packageSurface[directory])
	}
	model.wholeRepository = whole.sum()
	return model, nil
}

// goImportResolver maps an import path to the repository package directory that
// satisfies it, or reports that it leaves the repository.
type goImportResolver struct {
	// modules maps a module's repository-relative directory to its module path,
	// ordered longest module path first so a nested module wins over its parent.
	modules  []goModuleLocation
	packages map[string]string
	root     string
	// broken records imports that name a package inside this repository's own
	// modules which holds no Go files. They are repository-wide evidence: the
	// toolchain cannot type-check the importing package, and a failed load
	// changes which interfaces the type universe contains, so a concrete type in
	// an unrelated package can gain or lose an `implements` edge. Nothing
	// reachable by import edges can express that, which is why it belongs in the
	// repository key.
	broken []string
}

type goModuleLocation struct {
	directory  string
	modulePath string
}

func newGoImportResolver(root string, paths []string, packages map[string]string) *goImportResolver {
	resolver := &goImportResolver{packages: packages, root: root}
	for _, relative := range paths {
		if filepath.Base(relative) != "go.mod" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			continue
		}
		modulePath := goModulePath(content)
		if modulePath == "" {
			continue
		}
		resolver.modules = append(resolver.modules, goModuleLocation{
			directory: goPackageDirectory(relative), modulePath: modulePath,
		})
	}
	sort.Slice(resolver.modules, func(left, right int) bool {
		if len(resolver.modules[left].modulePath) != len(resolver.modules[right].modulePath) {
			return len(resolver.modules[left].modulePath) > len(resolver.modules[right].modulePath)
		}
		return resolver.modules[left].modulePath < resolver.modules[right].modulePath
	})
	return resolver
}

// resolve answers which repository package directory an import path names.
//
// A module path prefix is tried first, longest first so a nested module wins.
// Vendored imports are then tried against the vendor tree of every module that
// contains the importing directory, because -mod=vendor compiles those files
// rather than the module cache. An import that matches nothing in the
// repository leaves it, and the manifests that select it are already digested
// in full.
func (r *goImportResolver) resolve(from, importPath string) (string, bool) {
	if importPath == "" || importPath == "C" || !strings.Contains(importPath, ".") {
		// A path with no dot in its first segment is a standard library package,
		// which is selected by the toolchain version in the build context.
		if first, _, _ := strings.Cut(importPath, "/"); !strings.Contains(first, ".") {
			return "", false
		}
	}
	for _, module := range r.modules {
		if importPath != module.modulePath && !strings.HasPrefix(importPath, module.modulePath+"/") {
			continue
		}
		suffix := strings.TrimPrefix(strings.TrimPrefix(importPath, module.modulePath), "/")
		// The directory is returned whether or not it currently holds Go files.
		// An import inside this module's path names a local package, and a
		// package that was just deleted must keep its edges: they are what
		// identify the importers whose facts the deletion invalidates. A
		// directory with no surface digests as empty, so it also changes when
		// the package appears for the first time.
		candidate := joinRepositoryPath(module.directory, suffix)
		if _, ok := r.packages[candidate]; !ok {
			r.broken = append(r.broken, from+"\x00"+importPath)
		}
		return candidate, true
	}
	for _, module := range r.modules {
		if !withinRepositoryDirectory(module.directory, from) {
			continue
		}
		candidate := joinRepositoryPath(joinRepositoryPath(module.directory, "vendor"), importPath)
		if _, ok := r.packages[candidate]; ok {
			return candidate, true
		}
	}
	return "", false
}

// withinRepositoryDirectory reports whether candidate is parent or a descendant
// of it, both repository-relative slash paths.
func withinRepositoryDirectory(parent, candidate string) bool {
	if parent == "." || parent == "" {
		return true
	}
	return candidate == parent || strings.HasPrefix(candidate, parent+"/")
}

// goModulePath reads the module path from go.mod without depending on the
// module file parser: only the first `module` directive is meaningful here, and
// a malformed file is reported as having no module path.
func goModulePath(content []byte) string {
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		rest, found := strings.CutPrefix(trimmed, "module")
		if !found || (rest != "" && !strings.ContainsAny(rest[:1], " \t(\"")) {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" || strings.HasPrefix(rest, "(") {
			continue
		}
		if unquoted, err := strconv.Unquote(rest); err == nil {
			return unquoted
		}
		return strings.Fields(rest)[0]
	}
	return ""
}

// goWorkspaceManifestPaths returns the absolute workspace manifests that select
// the load plan, and the vendor manifest of every module, so the repository key
// covers everything outside a package directory.
func goWorkspaceManifestPaths(root string, paths []string) []string {
	var result []string
	for _, relative := range paths {
		if filepath.Base(relative) != "go.mod" {
			continue
		}
		result = append(result, filepath.Join(root,
			filepath.FromSlash(joinRepositoryPath(goPackageDirectory(relative), "vendor/modules.txt"))))
	}
	workspace := discoverGoWorkspace(root)
	if workspace != "" && workspace != "off" {
		result = append(result,
			workspace,
			workspace+".sum",
			filepath.Join(filepath.Dir(workspace), "vendor", "modules.txt"))
	}
	sort.Strings(result)
	return result
}

func sortedKeys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
