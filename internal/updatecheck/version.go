// Package updatecheck reports when a newer Grafo release exists and names the
// command that upgrades the binary the caller is actually running.
//
// Every operation here is advisory. A failed lookup, an unreadable cache or an
// unreachable network leaves the caller with no notice rather than an error:
// an update check that interrupts a command is worse than no update check.
package updatecheck

import (
	"strings"

	"golang.org/x/mod/semver"
)

// Comparable normalizes a Grafo version or Git tag into the form
// golang.org/x/mod/semver accepts. Releases are stamped without the leading
// "v" by GoReleaser while the Git tags carry it, so both spellings have to
// resolve to the same comparable value.
func Comparable(version string) string {
	trimmed := strings.TrimSpace(version)
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "v") {
		trimmed = "v" + trimmed
	}
	if !semver.IsValid(trimmed) {
		return ""
	}
	return trimmed
}

// IsRelease reports whether a version string came from a release build. Local
// builds and `go install` from a branch carry the in-tree default, which has no
// release to compare against, so they are never offered an upgrade.
func IsRelease(version string) bool {
	comparable := Comparable(version)
	if comparable == "" {
		return false
	}
	// A prerelease suffix covers both the in-tree "0.1.0-dev" default and the
	// snapshot versions GoReleaser stamps outside a tagged release.
	return semver.Prerelease(comparable) == ""
}

// Newer reports whether candidate supersedes current. Anything unparsable on
// either side answers false, so a malformed feed response can never produce a
// notice.
func Newer(current, candidate string) bool {
	left, right := Comparable(current), Comparable(candidate)
	if left == "" || right == "" {
		return false
	}
	return semver.Compare(right, left) > 0
}

// Display renders a version for the terminal without the leading "v", matching
// how `grafo version` already prints the running build.
func Display(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}
