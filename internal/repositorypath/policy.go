// Package repositorypath defines the hard repository-membership boundary shared
// by source discovery and repository-wide semantic scanners.
package repositorypath

import (
	"path/filepath"
	"strings"
)

var ignoredDirectories = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	".godot": true, ".grafo": true, ".worktrees": true,
	"node_modules": true, "vendor": true,
	"dist": true, "build": true, "coverage": true, ".next": true, ".turbo": true,
}

var ignoredFiles = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "composer.lock": true,
}

// DirectoryIgnored reports whether name identifies a directory that is always
// outside a repository's source membership.
func DirectoryIgnored(name string) bool { return ignoredDirectories[name] }

// Ignored reports whether a normalized or platform-native repository-relative
// path is always outside source membership.
func Ignored(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if DirectoryIgnored(part) {
			return true
		}
	}
	return ignoredFiles[strings.ToLower(filepath.Base(path))]
}
