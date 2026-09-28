// Package pathscope owns repository-relative path normalization and matching.
package pathscope

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var windowsAbsolute = regexp.MustCompile(`^[A-Za-z]:/`)

// NormalizePrefix validates and canonicalizes one repository-relative segment
// prefix. The returned value never has a leading or trailing slash.
func NormalizePrefix(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path prefix must be non-empty")
	}
	if strings.Contains(value, `\`) {
		return "", fmt.Errorf("path prefix must use slash separators")
	}
	if path.IsAbs(value) || windowsAbsolute.MatchString(value) {
		return "", fmt.Errorf("path prefix must be repository-relative")
	}
	if strings.ContainsAny(value, "*?[]{}") {
		return "", fmt.Errorf("path prefix must not contain glob syntax")
	}
	if hasTraversalSegment(value) {
		return "", fmt.Errorf("path prefix contains a traversal segment")
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("path prefix escapes or does not name a repository path")
	}
	return cleaned, nil
}

// HasPrefix reports segment-aware membership under a normalized prefix.
func HasPrefix(candidate, prefix string) bool {
	candidate = path.Clean(strings.TrimPrefix(strings.ReplaceAll(candidate, `\`, "/"), "./"))
	return candidate == prefix || strings.HasPrefix(candidate, prefix+"/")
}

// NormalizeGlob validates the deliberately small repository glob grammar.
// Recursive ** is supported only as an entire segment; other segments use
// literal characters plus * and ?.
func NormalizeGlob(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("pattern is empty")
	}
	if strings.Contains(value, `\`) {
		return "", fmt.Errorf("pattern must use slash separators")
	}
	if path.IsAbs(value) || windowsAbsolute.MatchString(value) {
		return "", fmt.Errorf("pattern must be repository-relative")
	}
	if hasTraversalSegment(value) {
		return "", fmt.Errorf("pattern contains a traversal segment")
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("pattern escapes the repository")
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if strings.ContainsAny(segment, "[]{}") {
			return "", fmt.Errorf("pattern contains unsupported glob syntax")
		}
		if strings.Contains(segment, "**") && segment != "**" {
			return "", fmt.Errorf("recursive ** must occupy an entire segment")
		}
		if segment != "**" {
			if _, err := path.Match(segment, ""); err != nil {
				return "", fmt.Errorf("invalid glob segment %q: %w", segment, err)
			}
		}
	}
	return cleaned, nil
}

func hasTraversalSegment(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// MatchGlob applies a normalized glob to a repository-relative slash path.
func MatchGlob(pattern, candidate string) bool {
	patternParts := strings.Split(pattern, "/")
	candidate = path.Clean(strings.TrimPrefix(strings.ReplaceAll(candidate, `\`, "/"), "./"))
	if candidate == "." || strings.HasPrefix(candidate, "../") || strings.HasPrefix(candidate, "/") {
		return false
	}
	return matchParts(patternParts, strings.Split(candidate, "/"))
}

func matchParts(pattern, candidate []string) bool {
	if len(pattern) == 0 {
		return len(candidate) == 0
	}
	if pattern[0] == "**" {
		if matchParts(pattern[1:], candidate) {
			return true
		}
		return len(candidate) > 0 && matchParts(pattern, candidate[1:])
	}
	if len(candidate) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], candidate[0])
	return err == nil && matched && matchParts(pattern[1:], candidate[1:])
}
