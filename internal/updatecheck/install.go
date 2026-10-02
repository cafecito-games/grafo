package updatecheck

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Method is how the running binary was installed, which decides the upgrade
// command the caller is told to run.
type Method string

const (
	// MethodHomebrew is an install from the cafecito-games/homebrew-tap cask.
	MethodHomebrew Method = "homebrew"
	// MethodGoInstall is an install from `go install`, which lands in GOBIN.
	MethodGoInstall Method = "go-install"
	// MethodArchive is an install from a release archive, or anything this
	// package cannot attribute to a package manager.
	MethodArchive Method = "archive"
)

// homebrewPrefixes are the directories Homebrew installs into on macOS (Apple
// silicon and Intel) and on Linux. Matching the executable path against them
// keeps the detection free of a `brew --prefix` subprocess on a path that runs
// before every command.
var homebrewPrefixes = []string{
	"/opt/homebrew",
	"/usr/local/Caskroom",
	"/usr/local/Homebrew",
	"/home/linuxbrew/.linuxbrew",
}

// Detect classifies the binary at executablePath. The caller passes the path so
// tests can cover every layout, and so a failure to locate the running
// executable degrades to MethodArchive instead of guessing.
//
// The path is resolved through its symlinks first: Homebrew links
// <prefix>/bin/grafo at a versioned file inside the Caskroom, so an unresolved
// path reports every cask install as an archive install.
func Detect(executablePath string) Method {
	resolved := resolveSymlinks(executablePath)
	if resolved == "" {
		return MethodArchive
	}
	if underHomebrew(resolved) {
		return MethodHomebrew
	}
	if underGoBin(resolved) {
		return MethodGoInstall
	}
	return MethodArchive
}

// DetectRunning classifies the currently running binary.
func DetectRunning() Method {
	executablePath, err := os.Executable()
	if err != nil {
		return MethodArchive
	}
	return Detect(executablePath)
}

func resolveSymlinks(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		// A binary that has since been moved or deleted still tells us which
		// directory it was launched from, which is all the classification needs.
		return absolute

	}
	return resolved
}

func underHomebrew(path string) bool {
	prefixes := homebrewPrefixes
	if configured := strings.TrimSpace(os.Getenv("HOMEBREW_PREFIX")); configured != "" {
		prefixes = append([]string{configured}, prefixes...)
	}
	for _, prefix := range prefixes {
		if withinDirectory(prefix, path) {
			return true
		}
	}
	return false
}

func underGoBin(path string) bool {
	if binary := strings.TrimSpace(os.Getenv("GOBIN")); binary != "" && withinDirectory(binary, path) {
		return true
	}
	for _, root := range goPathRoots() {
		if withinDirectory(filepath.Join(root, "bin"), path) {
			return true
		}
	}
	return false
}

// goPathRoots lists the GOPATH entries to test, falling back to the default
// location Go uses when GOPATH is unset.
func goPathRoots() []string {
	if configured := strings.TrimSpace(os.Getenv("GOPATH")); configured != "" {
		return filepath.SplitList(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "go")}
}

// withinDirectory reports whether path sits inside directory. Both sides are
// resolved through their symlinks, because the caller has already resolved the
// executable and a prefix reached through a link would otherwise never match
// it. The separator guard keeps "/usr/local/Caskroom-backup/grafo" from
// matching "/usr/local/Caskroom".
func withinDirectory(directory, path string) bool {
	cleaned := resolveSymlinks(directory)
	if cleaned == "" || cleaned == "." || cleaned == string(filepath.Separator) {
		return false
	}
	if path == cleaned {
		return true
	}
	return strings.HasPrefix(path, cleaned+string(filepath.Separator))
}

// archiveName is the release asset for the running platform. It mirrors the
// name_template in .goreleaser.yaml, so the name printed here is the file that
// actually exists on the release rather than one the reader has to translate.
func archiveName(version string) string {
	return "grafo_" + Display(version) + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
}
