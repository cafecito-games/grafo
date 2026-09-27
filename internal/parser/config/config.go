package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "config" }

func (*Parser) Supports(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	return base == ".env" || strings.HasPrefix(base, ".env.") || ext == ".yaml" || ext == ".yml" || ext == ".properties" || ext == ".json" || ext == ".toml"
}

type entry struct {
	key   string
	value string
	line  int
}

var referencePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.-]*)[^}]*\}`)

func (*Parser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "config")
	var entries []entry
	var err error
	base := strings.ToLower(filepath.Base(input.Path))
	ext := strings.ToLower(filepath.Ext(input.Path))
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env."):
		entries = parseEnv(input.Content)
	case ext == ".properties":
		entries = parseProperties(input.Content)
	case ext == ".yaml" || ext == ".yml":
		entries = parseYAML(input.Content)
	case ext == ".json":
		entries, err = parseJSON(input.Content)
	case ext == ".toml":
		entries, err = parseTOML(input.Content)
	}
	if err != nil {
		line := 0
		var parseErr toml.ParseError
		if errors.As(err, &parseErr) {
			line = parseErr.Position.Line
		}
		b.Diagnostic(line, "warning", err.Error())
	}
	for _, item := range entries {
		loc := graph.Location{Path: input.Path, Line: item.line, Column: 1, EndLine: item.line}
		format := strings.TrimPrefix(ext, ".")
		if base == ".env" || strings.HasPrefix(base, ".env.") {
			format = "env"
		}
		id := b.AddNode(graph.Node{Kind: graph.KindConfigKey, Name: item.key,
			QualifiedName: "config:" + input.Path + ":" + item.key, Location: loc,
			Properties: map[string]string{"defined": "true", "format": format}})
		b.AddFact(b.FileID(), graph.EdgeDefines, id, "", "", loc, nil)
		for _, match := range referencePattern.FindAllStringSubmatch(item.value, -1) {
			b.AddFact(id, graph.EdgeReferences, "", match[1], graph.KindConfigKey, loc, nil)
		}
	}
	return b.Finish(), nil
}

func parseEnv(content []byte) []entry {
	var result []entry
	scanner := bufio.NewScanner(bytes.NewReader(content))
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		if ok && strings.TrimSpace(key) != "" {
			result = append(result, entry{strings.TrimSpace(key), strings.TrimSpace(value), line})
		}
	}
	return result
}

func parseProperties(content []byte) []entry {
	var result []entry
	scanner := bufio.NewScanner(bytes.NewReader(content))
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "!") {
			continue
		}
		index := strings.IndexAny(text, "=:")
		if index < 0 {
			continue
		}
		result = append(result, entry{strings.TrimSpace(text[:index]), strings.TrimSpace(text[index+1:]), line})
	}
	return result
}

func parseYAML(content []byte) []entry {
	type level struct {
		indent int
		key    string
	}
	var stack []level
	var result []entry
	scanner := bufio.NewScanner(bytes.NewReader(content))
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimRight(scanner.Text(), " \t\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		index := strings.Index(trimmed, ":")
		if index <= 0 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(trimmed[:index]), "'\"")
		value := strings.TrimSpace(trimmed[index+1:])
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parts := make([]string, 0, len(stack)+1)
		for _, parent := range stack {
			parts = append(parts, parent.key)
		}
		parts = append(parts, key)
		result = append(result, entry{strings.Join(parts, "."), value, line})
		if value == "" || value == "|" || value == ">" {
			stack = append(stack, level{indent, key})
		}
	}
	return result
}

func parseJSON(content []byte) ([]entry, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	var result []entry
	var walk func(string, any)
	walk = func(prefix string, value any) {
		switch current := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(current))
			for key := range current {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				name := key
				if prefix != "" {
					name = prefix + "." + key
				}
				walk(name, current[key])
			}
		case []any:
			result = append(result, entry{prefix, "[array]", 1})
		default:
			result = append(result, entry{prefix, fmt.Sprint(current), 1})
		}
	}
	walk("", value)
	return result, nil
}

func parseTOML(content []byte) ([]entry, error) {
	var value map[string]any
	if _, err := toml.NewDecoder(bytes.NewReader(content)).Decode(&value); err != nil {
		return nil, fmt.Errorf("parse TOML: %w", err)
	}

	values := map[string][]string{}
	var walk func(string, any)
	walk = func(prefix string, value any) {
		switch current := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(current))
			for key := range current {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				name := key
				if prefix != "" {
					name = prefix + "." + key
				}
				walk(name, current[key])
			}
		case []map[string]any:
			for _, item := range current {
				walk(prefix, item)
			}
		default:
			values[prefix] = append(values[prefix], fmt.Sprint(current))
		}
	}
	walk("", value)

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]entry, 0, len(keys))
	for _, key := range keys {
		result = append(result, entry{key, strings.Join(values[key], "\n"), 1})
	}
	return result, nil
}
