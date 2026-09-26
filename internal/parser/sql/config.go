package sql

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const configFileName = "grafo.yaml"

type configuration struct {
	defaultDialect string
	paths          []pathMapping
	raw            []byte
}

type pathMapping struct {
	pattern string
	dialect string
}

func loadConfiguration(root string) (configuration, error) {
	if root == "" {
		return configuration{}, nil
	}
	content, err := os.ReadFile(filepath.Join(root, configFileName))
	if os.IsNotExist(err) {
		return configuration{}, nil
	}
	if err != nil {
		return configuration{}, fmt.Errorf("read %s: %w", configFileName, err)
	}
	config, err := parseConfiguration(content)
	if err != nil {
		return configuration{}, fmt.Errorf("parse %s: %w", configFileName, err)
	}
	config.raw = content
	return config, nil
}

// parseConfiguration intentionally implements only Grafo's small SQL config
// surface. This keeps application configuration independent from source YAML
// indexing and avoids making a general-purpose YAML decoder part of the parser.
func parseConfiguration(content []byte) (configuration, error) {
	var result configuration
	seenPatterns := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	lineNumber := 0
	sqlIndent := -1
	pathsIndent := -1
	seenDefault := false
	for scanner.Scan() {
		lineNumber++
		raw := strings.TrimRight(scanner.Text(), " \r")
		if strings.Contains(raw, "\t") {
			return configuration{}, fmt.Errorf("line %d: tabs are not supported", lineNumber)
		}
		trimmed := strings.TrimSpace(stripComment(raw))
		if trimmed == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		key, value, ok := splitMapping(trimmed)
		if !ok {
			if sqlIndent >= 0 && indent > sqlIndent {
				return configuration{}, fmt.Errorf("line %d: expected a key-value mapping", lineNumber)
			}
			continue
		}
		key, err := scalar(key)
		if err != nil {
			return configuration{}, fmt.Errorf("line %d: invalid key: %w", lineNumber, err)
		}
		if indent == 0 {
			pathsIndent = -1
			if key == "sql" {
				if strings.TrimSpace(value) != "" {
					return configuration{}, fmt.Errorf("line %d: sql must be a mapping", lineNumber)
				}
				sqlIndent = indent
			} else {
				sqlIndent = -1
			}
			continue
		}
		if sqlIndent < 0 || indent <= sqlIndent {
			continue
		}
		if pathsIndent >= 0 && indent > pathsIndent {
			pattern := filepath.ToSlash(strings.TrimPrefix(key, "./"))
			dialect, err := scalar(value)
			if err != nil || dialect == "" {
				return configuration{}, fmt.Errorf("line %d: path dialect must be a non-empty scalar", lineNumber)
			}
			if err := validatePattern(pattern); err != nil {
				return configuration{}, fmt.Errorf("line %d: invalid path pattern %q: %w", lineNumber, pattern, err)
			}
			dialect = normalizeName(dialect)
			if previous, exists := seenPatterns[pattern]; exists && previous != dialect {
				return configuration{}, fmt.Errorf("line %d: path pattern %q maps to both %q and %q", lineNumber, pattern, previous, dialect)
			}
			if _, exists := seenPatterns[pattern]; !exists {
				result.paths = append(result.paths, pathMapping{pattern: pattern, dialect: dialect})
				seenPatterns[pattern] = dialect
			}
			continue
		}
		pathsIndent = -1
		switch key {
		case "default_dialect":
			if seenDefault {
				return configuration{}, fmt.Errorf("line %d: duplicate default_dialect", lineNumber)
			}
			value, err = scalar(value)
			if err != nil || value == "" {
				return configuration{}, fmt.Errorf("line %d: default_dialect must be a non-empty scalar", lineNumber)
			}
			result.defaultDialect = normalizeName(value)
			seenDefault = true
		case "paths":
			if strings.TrimSpace(value) != "" {
				return configuration{}, fmt.Errorf("line %d: paths must be a mapping", lineNumber)
			}
			pathsIndent = indent
		default:
			return configuration{}, fmt.Errorf("line %d: unknown sql setting %q", lineNumber, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return configuration{}, err
	}
	sort.Slice(result.paths, func(i, j int) bool {
		return compareSpecificity(result.paths[i].pattern, result.paths[j].pattern) < 0
	})
	return result, nil
}

func validatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("pattern is empty")
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == "**" {
			continue
		}
		if _, err := path.Match(part, ""); err != nil {
			return err
		}
	}
	return nil
}

func (c configuration) dialectForPath(filePath string) (string, error) {
	filePath = filepath.ToSlash(strings.TrimPrefix(filePath, "./"))
	for _, mapping := range c.paths {
		matched, err := matchPath(mapping.pattern, filePath)
		if err != nil {
			return "", err
		}
		if matched {
			return mapping.dialect, nil
		}
	}
	return "", nil
}

// compareSpecificity orders patterns independently of their YAML declaration
// order: more literal characters, then fewer wildcard tokens, then lexical.
func compareSpecificity(left, right string) int {
	leftLiteral, leftWildcards := patternScore(left)
	rightLiteral, rightWildcards := patternScore(right)
	if leftLiteral != rightLiteral {
		return rightLiteral - leftLiteral
	}
	if leftWildcards != rightWildcards {
		return leftWildcards - rightWildcards
	}
	return strings.Compare(left, right)
}

func patternScore(pattern string) (int, int) {
	literal, wildcards := 0, 0
	for _, character := range pattern {
		switch character {
		case '*', '?', '[':
			wildcards++
		default:
			literal++
		}
	}
	return literal, wildcards
}

func matchPath(pattern, target string) (bool, error) {
	patternParts := strings.Split(filepath.ToSlash(pattern), "/")
	targetParts := strings.Split(filepath.ToSlash(target), "/")
	var walk func(int, int) (bool, error)
	walk = func(patternIndex, targetIndex int) (bool, error) {
		if patternIndex == len(patternParts) {
			return targetIndex == len(targetParts), nil
		}
		if patternParts[patternIndex] == "**" {
			for next := targetIndex; next <= len(targetParts); next++ {
				matched, err := walk(patternIndex+1, next)
				if err != nil || matched {
					return matched, err
				}
			}
			return false, nil
		}
		if targetIndex == len(targetParts) {
			_, err := path.Match(patternParts[patternIndex], "")
			return false, err
		}
		matched, err := path.Match(patternParts[patternIndex], targetParts[targetIndex])
		if err != nil || !matched {
			return false, err
		}
		return walk(patternIndex+1, targetIndex+1)
	}
	return walk(0, 0)
}

func splitMapping(value string) (string, string, bool) {
	quote := rune(0)
	escaped := false
	for index, character := range value {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote == '"' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == ':' {
			return strings.TrimSpace(value[:index]), strings.TrimSpace(value[index+1:]), true
		}
	}
	return "", "", false
}

func stripComment(value string) string {
	quote := rune(0)
	escaped := false
	for index, character := range value {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote == '"' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '#' {
			return value[:index]
		}
	}
	return value
}

func scalar(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "\"") {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return "", err
		}
		return unquoted, nil
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") {
			return "", fmt.Errorf("unterminated quoted scalar")
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	if strings.ContainsAny(value, "{}[]") {
		return "", fmt.Errorf("collection values are not supported")
	}
	return value, nil
}

func normalizeName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
