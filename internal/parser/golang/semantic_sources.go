package golang

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Grafo asks two different questions about a Go file, and semanticPathIgnored
// used to answer both with one rule:
//
//   - Is it a graph source? Vendored and git-ignored files are not: they are
//     dependency or generated code that no repository has agreed to own, and
//     indexing them would publish nodes nobody wrote.
//   - Is it a semantic input? Vendored and git-ignored files are, because
//     go/packages compiles every .go file in a package directory and compiles
//     against vendor/ whenever a module vendors. Their exported declarations
//     decide how repository code type-checks, and therefore how it extracts.
//
// Answering the second question with the first one's rule left two silent
// staleness gaps: hand-patching a vendored exported signature, or editing a
// git-ignored sibling that declares a type another package uses, changed how
// repository files extract while no key changed and no file was reparsed.
//
// goSemanticSourcePaths answers the second question on its own terms. The files
// it adds enter the Go workspace semantic key only; discovery and index scope
// are untouched, so nothing vendored or ignored becomes a graph node.

// goSemanticSources is the set of Go files whose contents can change how a
// repository file type-checks and extracts, together with the conditions that
// bounded the search.
type goSemanticSources struct {
	// Paths are repository-relative and sorted.
	Paths []string
	// Vendored and Ignored count the files that the git-visible membership
	// snapshot alone would have missed. They exist so the cost of widening the
	// input set is measurable rather than assumed.
	Vendored int
	Ignored  int
	// Unreadable records directories that could not be listed, as sorted
	// "<directory>: <error>" entries. They are digested verbatim so an
	// unlistable package directory changes the key instead of silently
	// narrowing it, and recovering from the condition changes it back.
	Unreadable []string
}

// goSemanticSourcePaths widens a git-visible membership snapshot to the set
// go/packages actually compiles.
//
// Every directory that owns a visible .go file is listed on disk, so a
// git-ignored or generated-but-uncommitted sibling contributes its declarations
// like the toolchain says it does. Every module that vendors has its vendor tree
// walked, because -mod=vendor compiles those files rather than the module cache.
//
// Cost is bounded by construction: the directory listings visit only directories
// that already hold a tracked Go file, and a vendor tree is walked only for a
// module root that has one. Nothing outside a Go package directory or a vendor
// tree is read.
func goSemanticSourcePaths(ctx context.Context, root string, visible []string) (goSemanticSources, error) {
	tracked := make(map[string]bool, len(visible))
	directories := map[string]bool{}
	var moduleRoots []string
	for _, path := range visible {
		switch {
		case isGoSourcePath(path):
			tracked[path] = true
			directories[goPackageDirectory(path)] = true
		case filepath.Base(path) == "go.mod":
			moduleRoots = append(moduleRoots, goPackageDirectory(path))
		}
	}
	sources := make(map[string]bool, len(tracked))
	// A tracked file always stays in the set, even when its directory cannot be
	// listed, so widening the input set can never narrow it.
	for path := range tracked {
		sources[path] = true
	}
	result := goSemanticSources{}
	for _, directory := range sortedSet(directories) {
		if err := ctx.Err(); err != nil {
			return goSemanticSources{}, err
		}
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(directory)))
		if err != nil {
			result.Unreadable = append(result.Unreadable, directory+": "+err.Error())
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !isGoSourcePath(entry.Name()) {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				continue
			}
			path := joinRepositoryPath(directory, entry.Name())
			if !tracked[path] {
				result.Ignored++
			}
			sources[path] = true
		}
	}
	for _, moduleRoot := range moduleRoots {
		vendorRoot := joinRepositoryPath(moduleRoot, "vendor")
		absolute := filepath.Join(root, filepath.FromSlash(vendorRoot))
		info, statErr := os.Stat(absolute)
		if statErr != nil || !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(absolute, func(current string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				relative, relErr := filepath.Rel(root, current)
				if relErr != nil {
					relative = current
				}
				result.Unreadable = append(result.Unreadable, filepath.ToSlash(relative)+": "+walkErr.Error())
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isGoSourcePath(entry.Name()) {
				return nil
			}
			relative, relErr := filepath.Rel(root, current)
			if relErr != nil {
				return nil
			}
			path := filepath.ToSlash(relative)
			if !sources[path] {
				result.Vendored++
			}
			sources[path] = true
			return nil
		})
		if walkErr != nil {
			return goSemanticSources{}, walkErr
		}
	}
	result.Paths = sortedSet(sources)
	sort.Strings(result.Unreadable)
	return result, nil
}

func sortedSet(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func joinRepositoryPath(directory, name string) string {
	if directory == "." || directory == "" {
		return name
	}
	return directory + "/" + name
}
