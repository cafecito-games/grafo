package sql

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/projectconfig"
)

const configFileName = projectconfig.FileName

type configuration struct {
	defaultDialect string
	paths          []pathMapping
	semanticKey    string
}

type pathMapping struct {
	pattern string
	dialect string
}

func loadConfiguration(root string) (configuration, error) {
	loaded, err := projectconfig.Load(root)
	if err != nil {
		return configuration{}, err
	}
	return configurationFromProject(loaded), nil
}

func parseConfiguration(content []byte) (configuration, error) {
	loaded, err := projectconfig.Parse(content)
	if err != nil {
		return configuration{}, err
	}
	return configurationFromProject(loaded), nil
}

func configurationFromProject(loaded projectconfig.Config) configuration {
	result := configuration{
		defaultDialect: loaded.SQL.DefaultDialect,
		paths:          make([]pathMapping, 0, len(loaded.SQL.Paths)),
		semanticKey:    loaded.SQL.SemanticKey(),
	}
	for _, mapping := range loaded.SQL.Paths {
		result.paths = append(result.paths, pathMapping{pattern: mapping.Pattern, dialect: mapping.Dialect})
	}
	sort.Slice(result.paths, func(i, j int) bool {
		return compareSpecificity(result.paths[i].pattern, result.paths[j].pattern) < 0
	})
	return result
}

func normalizeName(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

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
