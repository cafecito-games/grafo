// Package repositorypath defines the hard repository-membership boundary shared
// by source discovery and repository-wide semantic scanners.
package repositorypath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

// ErrUnsafe marks a path that escapes the repository, crosses a symlinked
// component, or does not name the expected filesystem kind.
var ErrUnsafe = errors.New("unsafe repository path")

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
// path is always outside source membership. Repository-wide scanners call it
// once per candidate path per file they key, so it walks the segments in place
// rather than allocating a split: on a large repository the allocation alone
// was a measurable share of an indexing run.
func Ignored(path string) bool {
	normalized := filepath.ToSlash(path)
	for remainder := normalized; ; {
		separator := strings.IndexByte(remainder, '/')
		if separator < 0 {
			if DirectoryIgnored(remainder) {
				return true
			}
			break
		}
		if DirectoryIgnored(remainder[:separator]) {
			return true
		}
		remainder = remainder[separator+1:]
	}
	// ToLower returns its argument unchanged when there is nothing to fold, so
	// the common all-lowercase base name costs no allocation either.
	return ignoredFiles[strings.ToLower(pathpkg.Base(normalized))]
}

// ResolvePath resolves a repository-relative path without following symlinks
// in any ancestor. The leaf may be missing or itself a symlink so callers that
// fingerprint those states can inspect it separately.
func ResolvePath(root, relative string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve root: %v", ErrUnsafe, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve root: %v", ErrUnsafe, err)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes repository root", ErrUnsafe, relative)
	}
	absolute := filepath.Join(root, clean)
	within, err := filepath.Rel(root, absolute)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes repository root", ErrUnsafe, relative)
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return absolute, nil
			}
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%w: ancestor %s is not a real directory", ErrUnsafe, current)
		}
	}
	return absolute, nil
}

// ResolveRegularFile applies ResolvePath and additionally requires the leaf to
// be a real regular file rather than a symlink or special file.
func ResolveRegularFile(root, relative string) (string, fs.FileInfo, error) {
	absolute, err := ResolvePath(root, relative)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%w: %s is not a real regular file", ErrUnsafe, absolute)
	}
	return absolute, info, nil
}
