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

func discoverFiles(ctx context.Context, project Project, registry *parserapi.Registry) ([]string, error) {
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
			return nil, err
		}
	}
	result := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, path := range candidates {
		if seen[path] || ignoredPath(path) || ignoredFiles[strings.ToLower(filepath.Base(path))] {
			continue
		}
		if _, ok := registry.For(path); !ok {
			continue
		}
		info, err := os.Stat(filepath.Join(project.Root, filepath.FromSlash(path)))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		seen[path] = true
		result = append(result, path)
	}
	sort.Strings(result)
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
