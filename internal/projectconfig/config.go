// Package projectconfig loads and validates Grafo's repository-level
// configuration without claiming sections owned by other consumers.
package projectconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/parser/calleffect"
	"gopkg.in/yaml.v3"
)

const FileName = "grafo.yaml"

var (
	componentNamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	requestSymbolPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	gdscriptTestBasePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	windowsAbsolutePath     = regexp.MustCompile(`^[A-Za-z]:/`)
)

// Config contains Grafo-owned top-level configuration. Unknown top-level
// sections are deliberately ignored so their owning consumers remain free to
// interpret them.
type Config struct {
	Components      []Component
	Adapters        calleffect.Registry
	HTTP            HTTP
	SQL             SQL
	Tests           Tests
	UnknownSections map[string][]yaml.Node
}

// Tests is the fail-closed repository test-framework configuration. Invalid
// settings retain one diagnostic and no custom bases, allowing parsers to keep
// the verified built-in conventions without accepting an unvalidated name.
type Tests struct {
	GDScriptBases []string
	Invalid       string
}

// SemanticKey fingerprints only the test-owned subtree. Invalid input is part
// of the key so fixing grafo.yaml reparses otherwise unchanged scripts.
func (t Tests) SemanticKey() string {
	canonical := append([]string(nil), t.GDScriptBases...)
	sort.Strings(canonical)
	encoded, _ := json.Marshal(struct {
		Bases   []string `json:"gdscript_bases"`
		Invalid string   `json:"invalid,omitempty"`
	}{Bases: canonical, Invalid: t.Invalid})
	digest := sha256.Sum256(encoded)
	return "test-config-v1:" + hex.EncodeToString(digest[:])
}

// HTTP is the shared, validated outbound-HTTP adapter configuration.
type HTTP struct {
	RequestAPIs []HTTPRequestAPI
}

// HTTPRequestAPI identifies one exact callable and its zero-based method and
// URL argument positions. URLArgument also represents route_argument, whose
// spelling is an equivalent configuration alias.
type HTTPRequestAPI struct {
	Language       string
	Symbol         string
	MethodArgument int
	URLArgument    int
	Line           int
}

// SemanticKey returns a deterministic digest of only the HTTP-owned subtree.
func (h HTTP) SemanticKey() string {
	type canonicalAPI struct {
		Language       string `json:"language"`
		Symbol         string `json:"symbol"`
		MethodArgument int    `json:"method_argument"`
		URLArgument    int    `json:"url_argument"`
		Line           int    `json:"line"`
	}
	canonical := make([]canonicalAPI, 0, len(h.RequestAPIs))
	for _, api := range h.RequestAPIs {
		canonical = append(canonical, canonicalAPI(api))
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Language != canonical[j].Language {
			return canonical[i].Language < canonical[j].Language
		}
		if canonical[i].Symbol != canonical[j].Symbol {
			return canonical[i].Symbol < canonical[j].Symbol
		}
		if canonical[i].MethodArgument != canonical[j].MethodArgument {
			return canonical[i].MethodArgument < canonical[j].MethodArgument
		}
		if canonical[i].URLArgument != canonical[j].URLArgument {
			return canonical[i].URLArgument < canonical[j].URLArgument
		}
		return canonical[i].Line < canonical[j].Line
	})
	encoded, _ := json.Marshal(canonical)
	digest := sha256.Sum256(encoded)
	return "http-config-v1:" + hex.EncodeToString(digest[:])
}

// Component declares one explicitly named deployable boundary.
type Component struct {
	Name   string
	Roots  []ComponentRoot
	Line   int
	Column int
}

// ComponentRoot is a normalized repository-relative ownership root together
// with the source location that proves the declaration.
type ComponentRoot struct {
	Path   string
	Line   int
	Column int
}

// SQL is the shared, validated SQL-router configuration.
type SQL struct {
	DefaultDialect string
	Paths          []SQLPath
}

// SQLPath maps one path pattern to a dialect.
type SQLPath struct {
	Pattern string
	Dialect string
	Line    int
}

// SemanticKey returns a deterministic digest of only the SQL-owned subtree.
// Changes to components or unknown top-level sections therefore do not force
// unchanged SQL source files to be reparsed.
func (s SQL) SemanticKey() string {
	canonical := struct {
		DefaultDialect string      `json:"default_dialect"`
		Paths          [][2]string `json:"paths"`
	}{DefaultDialect: s.DefaultDialect, Paths: make([][2]string, 0, len(s.Paths))}
	for _, mapping := range s.Paths {
		canonical.Paths = append(canonical.Paths, [2]string{mapping.Pattern, mapping.Dialect})
	}
	sort.Slice(canonical.Paths, func(i, j int) bool {
		if canonical.Paths[i][0] != canonical.Paths[j][0] {
			return canonical.Paths[i][0] < canonical.Paths[j][0]
		}
		return canonical.Paths[i][1] < canonical.Paths[j][1]
	})
	encoded, _ := json.Marshal(canonical)
	digest := sha256.Sum256(encoded)
	return "sql-config-v1:" + hex.EncodeToString(digest[:])
}

// Load reads grafo.yaml from root. A missing file is an empty configuration.
func Load(root string) (Config, error) {
	if root == "" {
		return Config{}, nil
	}
	content, err := os.ReadFile(filepath.Join(root, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", FileName, err)
	}
	config, err := Parse(content)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", FileName, err)
	}
	return config, nil
}

// Parse validates Grafo-owned configuration while preserving YAML line and
// column evidence for component declarations.
func Parse(content []byte) (Config, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("decode YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return Config{}, fmt.Errorf("line %d: multiple YAML documents are not supported", extra.Line)
	} else if !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode YAML: %w", err)
	}
	if len(document.Content) == 0 {
		return Config{}, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return Config{}, fmt.Errorf("line %d: top-level configuration must be a mapping", root.Line)
	}
	result := Config{UnknownSections: map[string][]yaml.Node{}}
	var adapters []calleffect.Adapter
	seenOwned := map[string]bool{}
	for index := 0; index < len(root.Content); index += 2 {
		keyNode, valueNode := root.Content[index], root.Content[index+1]
		key, err := stringScalar(keyNode)
		if err != nil {
			continue // Unknown non-string keys belong to no Grafo-owned section.
		}
		switch key {
		case "adapters":
			if seenOwned[key] {
				return Config{}, fmt.Errorf("line %d: duplicate top-level section %q", keyNode.Line, key)
			}
			seenOwned[key] = true
			adapters, err = parseAdapters(valueNode)
			if err != nil {
				return Config{}, err
			}
		case "components":
			if seenOwned[key] {
				return Config{}, fmt.Errorf("line %d: duplicate top-level section %q", keyNode.Line, key)
			}
			seenOwned[key] = true
			result.Components, err = parseComponents(valueNode)
			if err != nil {
				return Config{}, err
			}
		case "sql":
			if seenOwned[key] {
				return Config{}, fmt.Errorf("line %d: duplicate top-level section %q", keyNode.Line, key)
			}
			seenOwned[key] = true
			result.SQL, err = parseSQL(valueNode)
			if err != nil {
				return Config{}, err
			}
		case "http":
			if seenOwned[key] {
				return Config{}, fmt.Errorf("line %d: duplicate top-level section %q", keyNode.Line, key)
			}
			seenOwned[key] = true
			result.HTTP, err = parseHTTP(valueNode)
			if err != nil {
				return Config{}, err
			}
		case "tests":
			if seenOwned[key] {
				result.Tests = Tests{Invalid: fmt.Sprintf("line %d: duplicate top-level section %q", keyNode.Line, key)}
				continue
			}
			seenOwned[key] = true
			result.Tests = parseTests(valueNode)
		default:
			value := *valueNode
			result.UnknownSections[key] = append(result.UnknownSections[key], value)
		}
	}
	for _, api := range result.HTTP.RequestAPIs {
		adapters = append(adapters, calleffect.Adapter{Language: api.Language, Symbol: api.Symbol, Line: api.Line, Legacy: true,
			Effects: []calleffect.Effect{{Kind: calleffect.HTTPRequest, Line: api.Line, Roles: map[string]calleffect.Selector{
				calleffect.RoleMethod: {Argument: api.MethodArgument, Line: api.Line},
				calleffect.RoleURL:    {Argument: api.URLArgument, Line: api.Line},
			}}}})
	}
	registry, registryErr := calleffect.New(adapters)
	if registryErr != nil {
		return Config{}, registryErr
	}
	result.Adapters = registry
	return result, nil
}

func parseAdapters(node *yaml.Node) ([]calleffect.Adapter, error) {
	if isEmptyYAMLValue(node) {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: adapters must be a sequence", node.Line)
	}
	result := make([]calleffect.Adapter, 0, len(node.Content))
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("line %d: adapter must be a mapping", item.Line)
		}
		adapter := calleffect.Adapter{Line: item.Line, Column: item.Column}
		seen := map[string]bool{}
		for index := 0; index < len(item.Content); index += 2 {
			keyNode, valueNode := item.Content[index], item.Content[index+1]
			key, err := stringScalar(keyNode)
			if err != nil {
				return nil, fmt.Errorf("line %d: adapter field name must be a string", keyNode.Line)
			}
			if seen[key] {
				return nil, fmt.Errorf("line %d: duplicate adapter field %q", keyNode.Line, key)
			}
			seen[key] = true
			switch key {
			case "match":
				adapter.Language, adapter.Symbol, err = parseAdapterMatch(valueNode)
				if err != nil {
					return nil, err
				}
			case "effects":
				adapter.Effects, err = parseAdapterEffects(valueNode)
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("line %d: unknown adapter field %q", keyNode.Line, key)
			}
		}
		if adapter.Language == "" || adapter.Symbol == "" {
			return nil, fmt.Errorf("line %d: adapter match is required", item.Line)
		}
		if len(adapter.Effects) == 0 {
			return nil, fmt.Errorf("line %d: adapter effects must contain at least one effect", item.Line)
		}
		if adapter.Language == "gdscript" && adapter.Symbol == "HTTPRequest.request" {
			for _, effect := range adapter.Effects {
				if effect.Kind != calleffect.HTTPRequest {
					continue
				}
				if effect.Roles[calleffect.RoleMethod].Argument != 2 || effect.Roles[calleffect.RoleURL].Argument != 0 {
					return nil, fmt.Errorf("line %d: configured signature for %q conflicts with built-in method argument 2 and URL argument 0", effect.Line, adapter.Symbol)
				}
			}
		}
		result = append(result, adapter)
	}
	return result, nil
}

func parseAdapterMatch(node *yaml.Node) (string, string, error) {
	if node.Kind != yaml.MappingNode {
		return "", "", fmt.Errorf("line %d: adapter match must be a mapping", node.Line)
	}
	language, symbol := "", ""
	seen := map[string]bool{}
	for index := 0; index < len(node.Content); index += 2 {
		keyNode, valueNode := node.Content[index], node.Content[index+1]
		key, err := stringScalar(keyNode)
		if err != nil {
			return "", "", fmt.Errorf("line %d: adapter match field name must be a string", keyNode.Line)
		}
		if seen[key] {
			return "", "", fmt.Errorf("line %d: duplicate adapter match field %q", keyNode.Line, key)
		}
		seen[key] = true
		value, valueErr := stringScalar(valueNode)
		if valueErr != nil {
			return "", "", fmt.Errorf("line %d: adapter match %s must be a non-empty string", valueNode.Line, key)
		}
		switch key {
		case "language":
			language = strings.ToLower(strings.TrimSpace(value))
		case "symbol":
			symbol = strings.TrimSpace(value)
		default:
			return "", "", fmt.Errorf("line %d: unknown adapter match field %q", keyNode.Line, key)
		}
	}
	if language != "gdscript" {
		return "", "", fmt.Errorf("line %d: unsupported adapter language %q", node.Line, language)
	}
	if !requestSymbolPattern.MatchString(symbol) {
		return "", "", fmt.Errorf("line %d: adapter symbol %q must be an exact qualified symbol", node.Line, symbol)
	}
	return language, symbol, nil
}

func parseAdapterEffects(node *yaml.Node) ([]calleffect.Effect, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: adapter effects must be a sequence", node.Line)
	}
	result := make([]calleffect.Effect, 0, len(node.Content))
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("line %d: adapter effect must be a mapping", item.Line)
		}
		effect := calleffect.Effect{Line: item.Line, Column: item.Column}
		seen := map[string]bool{}
		for index := 0; index < len(item.Content); index += 2 {
			keyNode, valueNode := item.Content[index], item.Content[index+1]
			key, err := stringScalar(keyNode)
			if err != nil {
				return nil, fmt.Errorf("line %d: adapter effect field name must be a string", keyNode.Line)
			}
			if seen[key] {
				return nil, fmt.Errorf("line %d: duplicate adapter effect field %q", keyNode.Line, key)
			}
			seen[key] = true
			switch key {
			case "kind":
				value, valueErr := stringScalar(valueNode)
				if valueErr != nil {
					return nil, fmt.Errorf("line %d: adapter effect kind must be a non-empty string", valueNode.Line)
				}
				effect.Kind = calleffect.Kind(strings.TrimSpace(value))
			case "roles":
				effect.Roles, err = parseAdapterRoles(valueNode)
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("line %d: unknown adapter effect field %q", keyNode.Line, key)
			}
		}
		if err := calleffect.ValidateEffect(effect); err != nil {
			return nil, err
		}
		result = append(result, effect)
	}
	return result, nil
}

func parseAdapterRoles(node *yaml.Node) (map[string]calleffect.Selector, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: adapter effect roles must be a mapping", node.Line)
	}
	roles := map[string]calleffect.Selector{}
	for index := 0; index < len(node.Content); index += 2 {
		keyNode, selectorNode := node.Content[index], node.Content[index+1]
		role, err := stringScalar(keyNode)
		if err != nil {
			return nil, fmt.Errorf("line %d: adapter role name must be a string", keyNode.Line)
		}
		if _, ok := roles[role]; ok {
			return nil, fmt.Errorf("line %d: duplicate adapter role %q", keyNode.Line, role)
		}
		if selectorNode.Kind != yaml.MappingNode || len(selectorNode.Content) != 2 {
			return nil, fmt.Errorf("line %d: adapter role %q requires exactly one argument selector", selectorNode.Line, role)
		}
		selectorKey, err := stringScalar(selectorNode.Content[0])
		if err != nil || selectorKey != "argument" {
			return nil, fmt.Errorf("line %d: adapter role %q requires exactly one argument selector", selectorNode.Line, role)
		}
		argument, err := nonNegativeYAMLInteger(selectorNode.Content[1], "argument")
		if err != nil {
			return nil, err
		}
		roles[role] = calleffect.Selector{Argument: argument, Line: selectorNode.Line, Column: selectorNode.Column}
	}
	return roles, nil
}

func parseTests(node *yaml.Node) Tests {
	if isEmptyYAMLValue(node) {
		return Tests{}
	}
	if node.Kind != yaml.MappingNode {
		return Tests{Invalid: fmt.Sprintf("line %d: tests must be a mapping", node.Line)}
	}
	seenFields := map[string]bool{}
	result := Tests{}
	for index := 0; index < len(node.Content); index += 2 {
		keyNode, valueNode := node.Content[index], node.Content[index+1]
		key, err := stringScalar(keyNode)
		if err != nil {
			return Tests{Invalid: fmt.Sprintf("line %d: tests field name must be a string", keyNode.Line)}
		}
		if seenFields[key] {
			return Tests{Invalid: fmt.Sprintf("line %d: duplicate tests setting %q", keyNode.Line, key)}
		}
		seenFields[key] = true
		if key != "gdscript_bases" {
			return Tests{Invalid: fmt.Sprintf("line %d: unknown tests setting %q", keyNode.Line, key)}
		}
		if valueNode.Kind != yaml.SequenceNode {
			return Tests{Invalid: fmt.Sprintf("line %d: tests.gdscript_bases must be a sequence", valueNode.Line)}
		}
		seenBases := map[string]bool{}
		for _, baseNode := range valueNode.Content {
			base, baseErr := stringScalar(baseNode)
			base = strings.TrimSpace(base)
			if baseErr != nil || !gdscriptTestBasePattern.MatchString(base) {
				return Tests{Invalid: fmt.Sprintf("line %d: tests.gdscript_bases entry must be a GDScript class name", baseNode.Line)}
			}
			if seenBases[base] {
				return Tests{Invalid: fmt.Sprintf("line %d: duplicate tests.gdscript_bases entry %q", baseNode.Line, base)}
			}
			seenBases[base] = true
			result.GDScriptBases = append(result.GDScriptBases, base)
		}
	}
	sort.Strings(result.GDScriptBases)
	return result
}

func parseHTTP(node *yaml.Node) (HTTP, error) {
	if isEmptyYAMLValue(node) {
		return HTTP{}, nil
	}
	if node.Kind != yaml.MappingNode {
		return HTTP{}, fmt.Errorf("line %d: http must be a mapping", node.Line)
	}
	var result HTTP
	seenFields := map[string]bool{}
	for index := 0; index < len(node.Content); index += 2 {
		keyNode, valueNode := node.Content[index], node.Content[index+1]
		key, err := stringScalar(keyNode)
		if err != nil {
			return HTTP{}, fmt.Errorf("line %d: http field name must be a string", keyNode.Line)
		}
		if seenFields[key] {
			return HTTP{}, fmt.Errorf("line %d: duplicate http setting %q", keyNode.Line, key)
		}
		seenFields[key] = true
		switch key {
		case "request_apis":
			if isEmptyYAMLValue(valueNode) {
				continue
			}
			apis, err := parseHTTPRequestAPIs(valueNode)
			if err != nil {
				return HTTP{}, err
			}
			result.RequestAPIs = apis
		default:
			return HTTP{}, fmt.Errorf("line %d: unknown http setting %q", keyNode.Line, key)
		}
	}
	return result, nil
}

func parseHTTPRequestAPIs(node *yaml.Node) ([]HTTPRequestAPI, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: request_apis must be a sequence", node.Line)
	}
	result := make([]HTTPRequestAPI, 0, len(node.Content))
	seenSymbols := map[string]int{}
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("line %d: request API must be a mapping", item.Line)
		}
		api := HTTPRequestAPI{MethodArgument: -1, URLArgument: -1, Line: item.Line}
		seenFields := map[string]bool{}
		routeField := ""
		for index := 0; index < len(item.Content); index += 2 {
			keyNode, valueNode := item.Content[index], item.Content[index+1]
			key, err := stringScalar(keyNode)
			if err != nil {
				return nil, fmt.Errorf("line %d: request API field name must be a string", keyNode.Line)
			}
			if seenFields[key] {
				return nil, fmt.Errorf("line %d: duplicate request API field %q", keyNode.Line, key)
			}
			seenFields[key] = true
			switch key {
			case "language":
				value, err := stringScalar(valueNode)
				if err != nil {
					return nil, fmt.Errorf("line %d: request API language must be a non-empty string", valueNode.Line)
				}
				api.Language = strings.ToLower(strings.TrimSpace(value))
			case "symbol":
				value, err := stringScalar(valueNode)
				if err != nil {
					return nil, fmt.Errorf("line %d: request API symbol must be a non-empty string", valueNode.Line)
				}
				api.Symbol = strings.TrimSpace(value)
			case "method_argument":
				api.MethodArgument, err = nonNegativeYAMLInteger(valueNode, key)
				if err != nil {
					return nil, err
				}
			case "url_argument", "route_argument":
				if routeField != "" {
					return nil, fmt.Errorf("line %d: only one of url_argument or route_argument may be set", keyNode.Line)
				}
				routeField = key
				api.URLArgument, err = nonNegativeYAMLInteger(valueNode, key)
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("line %d: unknown request API field %q", keyNode.Line, key)
			}
		}
		if api.Language != "gdscript" {
			return nil, fmt.Errorf("line %d: unsupported request API language %q", item.Line, api.Language)
		}
		if !requestSymbolPattern.MatchString(api.Symbol) {
			return nil, fmt.Errorf("line %d: request API symbol %q must be an exact qualified symbol", item.Line, api.Symbol)
		}
		if api.MethodArgument < 0 {
			return nil, fmt.Errorf("line %d: method_argument is required", item.Line)
		}
		if routeField == "" {
			return nil, fmt.Errorf("line %d: one of url_argument or route_argument is required", item.Line)
		}
		if api.MethodArgument == api.URLArgument {
			return nil, fmt.Errorf("line %d: method_argument and %s must use distinct indexes", item.Line, routeField)
		}
		if api.Symbol == "HTTPRequest.request" && (api.MethodArgument != 2 || api.URLArgument != 0) {
			return nil, fmt.Errorf("line %d: configured signature for %q conflicts with built-in method_argument 2 and url_argument 0", item.Line, api.Symbol)
		}
		identity := api.Language + ":" + api.Symbol
		if previous, exists := seenSymbols[identity]; exists {
			return nil, fmt.Errorf("line %d: duplicate request API symbol %q (first declared on line %d)", item.Line, api.Symbol, previous)
		}
		seenSymbols[identity] = item.Line
		result = append(result, api)
	}
	return result, nil
}

func nonNegativeYAMLInteger(node *yaml.Node, field string) (int, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return -1, fmt.Errorf("line %d: %s must be a non-negative integer", node.Line, field)
	}
	value, err := strconv.Atoi(node.Value)
	if err != nil || value < 0 {
		return -1, fmt.Errorf("line %d: %s must be a non-negative integer", node.Line, field)
	}
	return value, nil
}

func parseComponents(node *yaml.Node) ([]Component, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: components must be a sequence", node.Line)
	}
	components := make([]Component, 0, len(node.Content))
	seenNames := map[string]int{}
	type declaredRoot struct {
		path      string
		component string
		line      int
	}
	var declared []declaredRoot
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("line %d: component must be a mapping", item.Line)
		}
		component := Component{Line: item.Line, Column: item.Column}
		seenFields := map[string]bool{}
		for index := 0; index < len(item.Content); index += 2 {
			keyNode, valueNode := item.Content[index], item.Content[index+1]
			key, err := stringScalar(keyNode)
			if err != nil {
				return nil, fmt.Errorf("line %d: component field name must be a string", keyNode.Line)
			}
			if seenFields[key] {
				return nil, fmt.Errorf("line %d: duplicate component field %q", keyNode.Line, key)
			}
			seenFields[key] = true
			switch key {
			case "name":
				name, err := stringScalar(valueNode)
				if err != nil {
					return nil, fmt.Errorf("line %d: component name must be a non-empty scalar", valueNode.Line)
				}
				component.Name = strings.TrimSpace(name)
				component.Line, component.Column = valueNode.Line, valueNode.Column
			case "roots":
				roots, err := parseRoots(valueNode)
				if err != nil {
					return nil, err
				}
				component.Roots = roots
			default:
				return nil, fmt.Errorf("line %d: unknown component field %q", keyNode.Line, key)
			}
		}
		if component.Name == "" {
			return nil, fmt.Errorf("line %d: component name is required", component.Line)
		}
		if !componentNamePattern.MatchString(component.Name) {
			return nil, fmt.Errorf("line %d: invalid component name %q; expected [A-Za-z0-9][A-Za-z0-9._-]*", component.Line, component.Name)
		}
		if previous, exists := seenNames[component.Name]; exists {
			return nil, fmt.Errorf("line %d: duplicate component name %q (first declared on line %d)", component.Line, component.Name, previous)
		}
		seenNames[component.Name] = component.Line
		if len(component.Roots) == 0 {
			return nil, fmt.Errorf("line %d: component %q roots must contain at least one root", item.Line, component.Name)
		}
		for _, root := range component.Roots {
			for _, previous := range declared {
				if rootsOverlap(root.Path, previous.path) {
					if root.Path == previous.path {
						return nil, fmt.Errorf("line %d: duplicate component root %q (first declared for %q on line %d)", root.Line, root.Path, previous.component, previous.line)
					}
					return nil, fmt.Errorf("line %d: component root %q for %q overlaps root %q for %q on line %d", root.Line, root.Path, component.Name, previous.path, previous.component, previous.line)
				}
			}
			declared = append(declared, declaredRoot{path: root.Path, component: component.Name, line: root.Line})
		}
		components = append(components, component)
	}
	return components, nil
}

func parseRoots(node *yaml.Node) ([]ComponentRoot, error) {
	if node.Kind != yaml.SequenceNode || len(node.Content) == 0 {
		return nil, fmt.Errorf("line %d: component roots must contain at least one root", node.Line)
	}
	result := make([]ComponentRoot, 0, len(node.Content))
	for _, valueNode := range node.Content {
		value, err := stringScalar(valueNode)
		if err != nil {
			return nil, fmt.Errorf("line %d: component root must be a non-empty scalar", valueNode.Line)
		}
		normalized, err := normalizeRoot(value)
		if err != nil {
			return nil, fmt.Errorf("line %d: component root %q %w", valueNode.Line, value, err)
		}
		result = append(result, ComponentRoot{Path: normalized, Line: valueNode.Line, Column: valueNode.Column})
	}
	return result, nil
}

func normalizeRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("must be non-empty")
	}
	if strings.Contains(value, `\`) {
		return "", fmt.Errorf("must use slash separators")
	}
	if path.IsAbs(value) || windowsAbsolutePath.MatchString(value) {
		return "", fmt.Errorf("must be repository-relative")
	}
	if strings.ContainsAny(value, "*?[]{}") {
		return "", fmt.Errorf("must not contain glob syntax")
	}
	normalized := path.Clean(value)
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("escapes the repository")
	}
	if normalized == "/" || strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("must be repository-relative")
	}
	return normalized, nil
}

func rootsOverlap(left, right string) bool {
	return left == "." || right == "." || left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func parseSQL(node *yaml.Node) (SQL, error) {
	if isEmptyYAMLValue(node) {
		return SQL{}, nil
	}
	if node.Kind != yaml.MappingNode {
		return SQL{}, fmt.Errorf("line %d: sql must be a mapping", node.Line)
	}
	var result SQL
	seenFields := map[string]bool{}
	for index := 0; index < len(node.Content); index += 2 {
		keyNode, valueNode := node.Content[index], node.Content[index+1]
		key, err := stringScalar(keyNode)
		if err != nil {
			return SQL{}, fmt.Errorf("line %d: sql field name must be a string", keyNode.Line)
		}
		if seenFields[key] {
			return SQL{}, fmt.Errorf("line %d: duplicate sql setting %q", keyNode.Line, key)
		}
		seenFields[key] = true
		switch key {
		case "default_dialect":
			value, err := sqlScalar(valueNode)
			if err != nil || strings.TrimSpace(value) == "" {
				return SQL{}, fmt.Errorf("line %d: default_dialect must be a non-empty scalar", valueNode.Line)
			}
			result.DefaultDialect = strings.ToLower(strings.TrimSpace(value))
		case "paths":
			if isEmptyYAMLValue(valueNode) {
				continue
			}
			paths, err := parseSQLPaths(valueNode)
			if err != nil {
				return SQL{}, err
			}
			result.Paths = paths
		default:
			return SQL{}, fmt.Errorf("line %d: unknown sql setting %q", keyNode.Line, key)
		}
	}
	return result, nil
}

func parseSQLPaths(node *yaml.Node) ([]SQLPath, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: paths must be a mapping", node.Line)
	}
	result := []SQLPath{}
	seen := map[string]string{}
	for index := 0; index < len(node.Content); index += 2 {
		patternNode, dialectNode := node.Content[index], node.Content[index+1]
		pattern, err := sqlScalar(patternNode)
		if err != nil {
			return nil, fmt.Errorf("line %d: path pattern must be a non-empty scalar", patternNode.Line)
		}
		pattern = strings.TrimPrefix(pattern, "./")
		if err := validateSQLPattern(pattern); err != nil {
			return nil, fmt.Errorf("line %d: invalid path pattern %q: %w", patternNode.Line, pattern, err)
		}
		dialect, err := sqlScalar(dialectNode)
		if err != nil || strings.TrimSpace(dialect) == "" {
			return nil, fmt.Errorf("line %d: path dialect must be a non-empty scalar", dialectNode.Line)
		}
		dialect = strings.ToLower(strings.TrimSpace(dialect))
		if previous, exists := seen[pattern]; exists {
			if previous != dialect {
				return nil, fmt.Errorf("line %d: path pattern %q maps to both %q and %q", patternNode.Line, pattern, previous, dialect)
			}
			continue
		}
		seen[pattern] = dialect
		result = append(result, SQLPath{Pattern: pattern, Dialect: dialect, Line: patternNode.Line})
	}
	return result, nil
}

func validateSQLPattern(pattern string) error {
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

func stringScalar(node *yaml.Node) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", fmt.Errorf("expected string scalar")
	}
	return node.Value, nil
}

func sqlScalar(node *yaml.Node) (string, error) {
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("expected scalar")
	}
	return node.Value, nil
}

func isEmptyYAMLValue(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null" && node.Value == ""
}
