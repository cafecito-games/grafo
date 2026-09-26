package manifest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"golang.org/x/mod/modfile"
)

type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "manifest" }

func (*Parser) Supports(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == "go.mod" || base == "package.json" || isRequirementsFile(base)
}

func (*Parser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	switch strings.ToLower(filepath.Base(input.Path)) {
	case "go.mod":
		return parseGoModule(input)
	case "package.json":
		return parsePackageJSON(input)
	default:
		return parseRequirements(input), nil
	}
}

func parseGoModule(input parserapi.Input) (graph.ParseResult, error) {
	builder := parserapi.NewBuilder(input, "go-module")
	file, err := modfile.Parse(input.Path, input.Content, nil)
	if err != nil {
		return builder.Finish(), fmt.Errorf("parse go.mod: %w", err)
	}
	sourceID := builder.FileID()
	if file.Module != nil && file.Module.Mod.Path != "" {
		location := modLocation(input.Path, file.Module.Syntax)
		properties := map[string]string{"ecosystem": "go"}
		if file.Module.Mod.Version != "" {
			properties["version"] = file.Module.Mod.Version
		}
		sourceID = builder.Declare(builder.FileID(), graph.Node{Kind: graph.KindModule,
			Name: graph.SimpleName(file.Module.Mod.Path), QualifiedName: file.Module.Mod.Path,
			Location: location, Properties: properties})
	}
	replacements := map[string]*modfile.Replace{}
	for _, replacement := range file.Replace {
		key := replacement.Old.Path + "@" + replacement.Old.Version
		replacements[key] = replacement
		if replacement.Old.Version == "" {
			replacements[replacement.Old.Path+"@"] = replacement
		}
	}
	for _, requirement := range file.Require {
		properties := map[string]string{"ecosystem": "go", "version": requirement.Mod.Version, "scope": "runtime"}
		if requirement.Indirect {
			properties["indirect"] = "true"
		}
		replacement := replacements[requirement.Mod.Path+"@"+requirement.Mod.Version]
		if replacement == nil {
			replacement = replacements[requirement.Mod.Path+"@"]
		}
		if replacement != nil {
			properties["replacement"] = replacement.New.Path
			if replacement.New.Version != "" {
				properties["replacement_version"] = replacement.New.Version
			}
		}
		location := modLocation(input.Path, requirement.Syntax)
		builder.AddFact(sourceID, graph.EdgeDependsOn, "", requirement.Mod.Path, graph.KindModule, location, properties)
	}
	return builder.Finish(), nil
}

type packageJSON struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

func parsePackageJSON(input parserapi.Input) (graph.ParseResult, error) {
	builder := parserapi.NewBuilder(input, "npm-manifest")
	var manifest packageJSON
	if err := json.Unmarshal(input.Content, &manifest); err != nil {
		return builder.Finish(), fmt.Errorf("parse package.json: %w", err)
	}
	sourceID := builder.FileID()
	if strings.TrimSpace(manifest.Name) != "" {
		properties := map[string]string{"ecosystem": "npm"}
		if manifest.Version != "" {
			properties["version"] = manifest.Version
		}
		sourceID = builder.Declare(builder.FileID(), graph.Node{Kind: graph.KindModule,
			Name: graph.SimpleName(manifest.Name), QualifiedName: manifest.Name,
			Location: graph.Location{Path: input.Path, Line: 1, Column: 1, EndLine: 1}, Properties: properties})
	}
	type dependency struct {
		version string
		scope   string
	}
	dependencies := map[string]dependency{}
	for _, group := range []struct {
		values map[string]string
		scope  string
	}{
		{manifest.Dependencies, "runtime"},
		{manifest.OptionalDependencies, "optional"},
		{manifest.PeerDependencies, "peer"},
		{manifest.DevDependencies, "development"},
	} {
		for name, version := range group.values {
			if _, exists := dependencies[name]; !exists {
				dependencies[name] = dependency{version: version, scope: group.scope}
			}
		}
	}
	names := make([]string, 0, len(dependencies))
	for name := range dependencies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dependency := dependencies[name]
		builder.AddFact(sourceID, graph.EdgeDependsOn, "", name, graph.KindModule,
			graph.Location{Path: input.Path, Line: 1, Column: 1, EndLine: 1},
			map[string]string{"ecosystem": "npm", "version": dependency.version, "scope": dependency.scope})
	}
	return builder.Finish(), nil
}

var requirementName = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^]]+\])?(.*)$`)

func parseRequirements(input parserapi.Input) graph.ParseResult {
	builder := parserapi.NewBuilder(input, "python-requirements")
	sourceID := input.RepoID
	if sourceID == "" {
		sourceID = builder.FileID()
	}
	scanner := bufio.NewScanner(bytes.NewReader(input.Content))
	scope := requirementScope(input.Path)
	line := 0
	for scanner.Scan() {
		line++
		value := strings.TrimSpace(scanner.Text())
		if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, "-") || strings.Contains(value, "://") {
			continue
		}
		if index := strings.Index(value, " #"); index >= 0 {
			value = strings.TrimSpace(value[:index])
		}
		if index := strings.Index(value, " --hash="); index >= 0 {
			value = strings.TrimSpace(value[:index])
		}
		match := requirementName.FindStringSubmatch(value)
		if match == nil {
			builder.Diagnostic(line, "warning", "unsupported Python requirement")
			continue
		}
		name := normalizePythonDistribution(match[1])
		properties := map[string]string{"ecosystem": "pypi", "scope": scope}
		if constraint := strings.TrimSpace(match[2]); constraint != "" {
			properties["version"] = constraint
		}
		location := graph.Location{Path: input.Path, Line: line, Column: 1, EndLine: line}
		builder.AddFact(sourceID, graph.EdgeDependsOn, "", name, graph.KindModule, location, properties)
	}
	if err := scanner.Err(); err != nil {
		builder.Diagnostic(line, "warning", err.Error())
	}
	return builder.Finish()
}

func requirementScope(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if strings.Contains(base, "dev") || strings.Contains(base, "test") {
		return "development"
	}
	return "runtime"
}

func isRequirementsFile(base string) bool {
	return base == "requirements.txt" || (strings.HasPrefix(base, "requirements-") && strings.HasSuffix(base, ".txt"))
}

func normalizePythonDistribution(name string) string {
	name = strings.ToLower(name)
	var result strings.Builder
	separator := false
	for _, character := range name {
		if character == '-' || character == '_' || character == '.' {
			if !separator {
				result.WriteByte('-')
				separator = true
			}
			continue
		}
		result.WriteRune(character)
		separator = false
	}
	return result.String()
}

func modLocation(path string, line *modfile.Line) graph.Location {
	location := graph.Location{Path: path, Line: 1, Column: 1, EndLine: 1}
	if line != nil {
		location.Line = line.Start.Line
		location.Column = line.Start.LineRune
		location.EndLine = line.End.Line
	}
	return location
}
