package indexer

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

var ignoredDirectories = map[string]bool{
	".git": true, ".grafo": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "coverage": true, ".next": true, ".turbo": true,
}

var ignoredFiles = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "composer.lock": true,
}

type discoveredFiles struct {
	paths       []string
	skipped     []string
	scopedOut   int
	gitCommands int
}

func discoverFiles(ctx context.Context, project Project, registry *parserapi.Registry) (discoveredFiles, error) {
	return discoverFilesWithCatalog(ctx, project, registry, nil, false, projectconfig.IndexScope{})
}

func discoverFilesWithCatalog(ctx context.Context, project Project, registry *parserapi.Registry, known map[string]graph.FileRecord, reuseKnown bool, scope projectconfig.IndexScope) (discoveredFiles, error) {
	var candidates []string
	gitCommands := 0
	if reuseKnown {
		candidates = make([]string, 0, len(known))
		for path := range known {
			candidates = append(candidates, path)
		}
	} else if project.GitManaged {
		gitCommands++
		runner := gitCommandRunner(execGitRunner{})
		if project.gitSnapshot != nil && project.gitSnapshot.runner != nil {
			runner = project.gitSnapshot.runner
		}
		output, err := runner.Run(ctx, project.Root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if err == nil {
			for _, raw := range bytes.Split(output, []byte{0}) {
				if len(raw) > 0 {
					candidates = append(candidates, filepath.ToSlash(string(raw)))
				}
			}
		}
	}
	if candidates == nil {
		err := filepath.WalkDir(project.Root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && path != project.Root && ignoredDirectories[entry.Name()] {
				return filepath.SkipDir
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(project.Root, path)
			if err != nil {
				return err
			}
			candidates = append(candidates, filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			return discoveredFiles{}, err
		}
	}
	// The root project configuration is a control-plane input even when Git
	// excludes it from the ordinary candidate set. Keep the same regular-file
	// and symlink safety boundary used for all other source membership.
	if info, err := os.Lstat(filepath.Join(project.Root, projectconfig.FileName)); err == nil &&
		info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		candidates = append(candidates, projectconfig.FileName)
	}
	result := discoveredFiles{paths: make([]string, 0, len(candidates)), gitCommands: gitCommands}
	if reuseKnown {
		for _, path := range candidates {
			if PathIgnored(path) {
				continue
			}
			if _, ok := registry.For(path); ok {
				if !scope.Allows(path) {
					result.scopedOut++
					continue
				}
				result.paths = append(result.paths, path)
			}
		}
		sort.Strings(result.paths)
		return result, nil
	}
	seen := map[string]bool{}
	for _, path := range candidates {
		if seen[path] || PathIgnored(path) {
			continue
		}
		seen[path] = true
		if _, ok := registry.For(path); !ok {
			continue
		}
		if !scope.Allows(path) {
			result.scopedOut++
			continue
		}
		info, err := os.Lstat(filepath.Join(project.Root, filepath.FromSlash(path)))
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			result.skipped = append(result.skipped, path)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		result.paths = append(result.paths, path)
	}
	sort.Strings(result.paths)
	sort.Strings(result.skipped)
	return result, nil
}

func ignoredPath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if ignoredDirectories[part] {
			return true
		}
	}
	return false
}

// PathIgnored reports whether the production indexer excludes a repository
// path before parser routing.
func PathIgnored(path string) bool {
	return ignoredPath(path) || ignoredFiles[strings.ToLower(filepath.Base(path))]
}
