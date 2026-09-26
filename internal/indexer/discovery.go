package indexer

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

var ignoredDirectories = map[string]bool{
	".git": true, ".grafo": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "coverage": true, ".next": true, ".turbo": true,
}

var ignoredFiles = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "composer.lock": true,
}

type discoveredFiles struct {
	paths   []string
	skipped []string
}

func discoverFiles(ctx context.Context, project Project, registry *parserapi.Registry) (discoveredFiles, error) {
	var candidates []string
	if project.GitManaged {
		command := exec.CommandContext(ctx, "git", "-C", project.Root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		output, err := command.Output()
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
	result := discoveredFiles{paths: make([]string, 0, len(candidates))}
	seen := map[string]bool{}
	for _, path := range candidates {
		if seen[path] || PathIgnored(path) {
			continue
		}
		if _, ok := registry.For(path); !ok {
			continue
		}
		info, err := os.Lstat(filepath.Join(project.Root, filepath.FromSlash(path)))
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			seen[path] = true
			result.skipped = append(result.skipped, path)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		seen[path] = true
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
