package service

import (
	"fmt"
	"strings"

	"github.com/cafecito-games/grafo/internal/agentinstall"
)

// InstallableBinary resolves the executable a service definition may point at and
// refuses one that will not still be there when the init system starts it.
//
// A definition names an absolute path and is read minutes, days, or reboots
// later, so an executable that lives in a directory the toolchain deletes on exit
// produces a permanently broken service. Three locations are refused, and the
// first two are decided structurally by containment in a directory the operating
// system itself reports rather than by matching names:
//
//  1. the temporary directory ($TMPDIR, or $GOTMPDIR which `go run` prefers),
//     which also covers `go test` binaries;
//  2. the Go build cache ($GOCACHE, or the per-user cache directory's go-build
//     subtree), where `go run` and `go build` without -o place their output;
//  3. a path with a "go-build..." element, which is the directory name the Go
//     toolchain creates for a one-shot build. This last rule is a name heuristic
//     and is deliberately the narrowest of the three: it catches a build cache
//     relocated somewhere the first two rules cannot see.
//
// Refusing is the fail-closed choice: a diagnostic naming the cause and the
// remedy is strictly better than writing a unit that is guaranteed to break.
func InstallableBinary(reader agentinstall.Reader, executable string) (string, error) {
	if strings.TrimSpace(executable) == "" {
		return "", fmt.Errorf("locate grafo executable: empty path")
	}
	resolved, err := agentinstall.ResolvePath(reader, executable)
	if err != nil {
		return "", fmt.Errorf("resolve grafo executable %s: %w", executable, err)
	}
	remedy := "install grafo with 'go install github.com/cafecito-games/grafo/cmd/grafo@latest' and run 'grafo service install' from the installed binary"
	for _, candidate := range ephemeralDirectories(reader) {
		if !withinDirectory(reader, candidate.path, resolved) {
			continue
		}
		return "", fmt.Errorf("refusing to point a service definition at %s: it is inside %s, which does not survive the command that built it; %s",
			resolved, candidate.reason, remedy)
	}
	for element := range strings.SplitSeq(resolved, pathSeparatorFor(reader.GOOS())) {
		if strings.HasPrefix(element, "go-build") {
			return "", fmt.Errorf("refusing to point a service definition at %s: it is inside a Go build directory, which is deleted when the build finishes; %s",
				resolved, remedy)
		}
	}
	return resolved, nil
}

// ephemeralLocation is one directory whose contents do not outlive the toolchain.
type ephemeralLocation struct {
	path   string
	reason string
}

// ephemeralDirectories lists the directories an installed binary must not be in,
// resolved through the same reader the rest of the installer uses so the rule is
// testable for every platform layout.
func ephemeralDirectories(reader agentinstall.Reader) []ephemeralLocation {
	locations := []ephemeralLocation{
		{path: reader.TempDir(), reason: "the temporary directory"},
		{path: reader.Getenv("GOTMPDIR"), reason: "the Go temporary directory"},
		{path: reader.Getenv("GOCACHE"), reason: "the Go build cache"},
	}
	if cache := userCacheDir(reader); cache != "" {
		locations = append(locations, ephemeralLocation{
			path:   agentinstall.JoinPath(reader.GOOS(), cache, "go-build"),
			reason: "the Go build cache",
		})
	}
	filtered := make([]ephemeralLocation, 0, len(locations))
	for _, location := range locations {
		if strings.TrimSpace(location.path) == "" {
			continue
		}
		filtered = append(filtered, location)
	}
	return filtered
}

// userCacheDir reports the documented per-user cache directory of the target
// platform, which is where the Go build cache lives by default.
func userCacheDir(reader agentinstall.Reader) string {
	switch reader.GOOS() {
	case "windows":
		for _, name := range []string{"LOCALAPPDATA", "APPDATA"} {
			if value := strings.TrimSpace(reader.Getenv(name)); value != "" {
				return value
			}
		}
		return ""
	case "darwin":
		home, err := reader.HomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return ""
		}
		return agentinstall.JoinPath("darwin", home, "Library", "Caches")
	default:
		if value := strings.TrimSpace(reader.Getenv("XDG_CACHE_HOME")); value != "" {
			return value
		}
		home, err := reader.HomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return ""
		}
		return agentinstall.JoinPath(reader.GOOS(), home, ".cache")
	}
}

// withinDirectory reports whether path is directory itself or below it, comparing
// fully resolved locations so a symlinked temporary directory cannot hide one.
func withinDirectory(reader agentinstall.Reader, directory, path string) bool {
	separator := pathSeparatorFor(reader.GOOS())
	resolved, err := agentinstall.ResolvePath(reader, directory)
	if err != nil {
		return false
	}
	trimmed := strings.TrimRight(resolved, "/\\")
	if trimmed == "" {
		return false
	}
	return path == trimmed || strings.HasPrefix(path, trimmed+separator)
}

// pathSeparatorFor returns the element separator of the target platform.
func pathSeparatorFor(goos string) string {
	if goos == "windows" {
		return `\`
	}
	return "/"
}
