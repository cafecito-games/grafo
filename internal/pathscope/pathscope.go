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
	candidateParts := strings.Split(candidate, "/")
	type state struct{ pattern, candidate int }
	memo := map[state]bool{}
	visited := map[state]bool{}
	var match func(int, int) bool
	match = func(patternIndex, candidateIndex int) bool {
		current := state{pattern: patternIndex, candidate: candidateIndex}
		if visited[current] {
			return memo[current]
		}
		visited[current] = true
		matched := false
		switch {
		case patternIndex == len(patternParts):
			matched = candidateIndex == len(candidateParts)
		case patternParts[patternIndex] == "**":
			matched = match(patternIndex+1, candidateIndex) ||
				candidateIndex < len(candidateParts) && match(patternIndex, candidateIndex+1)
		case candidateIndex < len(candidateParts):
			segmentMatched, err := path.Match(patternParts[patternIndex], candidateParts[candidateIndex])
			matched = err == nil && segmentMatched && match(patternIndex+1, candidateIndex+1)
		}
		memo[current] = matched
		return matched
	}
	return match(0, 0)
}
