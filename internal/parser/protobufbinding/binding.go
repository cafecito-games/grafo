// Package protobufbinding projects supported generated-language APIs onto
// canonical Protobuf declarations. It reads only repository-owned schema and
// generator configuration; it never executes a generator or reads ignored
// output as an authority.
package protobufbinding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	protobufparser "github.com/cafecito-games/grafo/internal/parser/protobuf"
	"gopkg.in/yaml.v3"
)

const adapterVersion = "1"

type Projection struct {
	Node          graph.Node
	CanonicalID   string
	Canonical     string
	CanonicalKind graph.NodeKind
	Properties    map[string]string
}

type GeneratedFile struct {
	Path       string
	Language   string
	Generator  string
	Version    string
	Adapter    string
	Source     string
	ConfigPath string
}

type Config struct {
	Path        string
	Line        int
	Projections []Projection
	Diagnostics []graph.Diagnostic
}

type Registry struct {
	Digest  string
	Configs map[string]Config
	Outputs map[string]GeneratedFile
}

func (r Registry) Config(path string) (Config, bool) {
	config, ok := r.Configs[clean(path)]
	return config, ok
}

// GeneratedFile returns corroborated output provenance for path. Content is
// deliberately checked here so a path configured as output is not enough to
// suppress an ordinary handwritten file.
func (r Registry) GeneratedFile(path, language string, content []byte) (GeneratedFile, bool, string) {
	path = clean(path)
	expected, ok := r.Outputs[path]
	if !ok || expected.Language != language {
		return GeneratedFile{}, false, ""
	}
	var source, generator string
	switch language {
	case "go":
		source, generator = goHeader(content)
	case "gdscript":
		source, generator = gdscriptHeader(content)
	default:
		return GeneratedFile{}, false, "unsupported generated binding language"
	}
	if generator == "" {
		return GeneratedFile{}, false, "configured output lacks a supported generated-file header"
	}
	if generator != expected.Generator {
		return GeneratedFile{}, false, fmt.Sprintf("generated-file header names %q, configuration expects %q", generator, expected.Generator)
	}
	matched, reason := r.matchSource(source)
	if reason != "" {
		return GeneratedFile{}, false, reason
	}
	if matched != clean(expected.Source) {
		return GeneratedFile{}, false, fmt.Sprintf("generated-file source %q does not match configured schema %q", source, expected.Source)
	}
	return expected, true, ""
}

// gdproto writes only the schema basename in its standard header, whereas
// protoc-gen-go writes the module-relative source path. A basename is evidence
// only when it identifies exactly one configured schema source.
func (r Registry) matchSource(source string) (string, string) {
	source = clean(source)
	exact := map[string]bool{}
	base := map[string]bool{}
	for _, output := range r.Outputs {
		candidate := clean(output.Source)
		if candidate == source {
			exact[candidate] = true
		}
		if filepath.Base(candidate) == source {
			base[candidate] = true
		}
	}
	if len(exact) == 1 {
		for candidate := range exact {
			return candidate, ""
		}
	}
	if len(base) == 1 {
		for candidate := range base {
			return candidate, ""
		}
	}
	if len(base) > 1 {
		return "", fmt.Sprintf("generated-file source %q matches more than one configured schema", source)
	}
	return "", fmt.Sprintf("generated-file source %q does not match a configured schema", source)
}

type Loader struct {
	mu       sync.Mutex
	root     string
	digest   string
	registry Registry
}

func NewLoader() *Loader { return &Loader{} }

func (l *Loader) Load(ctx context.Context, input parserapi.Input) (Registry, error) {
	if input.Root == "" {
		return Registry{Configs: map[string]Config{}, Outputs: map[string]GeneratedFile{}}, nil
	}
	// Production parsing receives the workspace semantic key computed before
	// file selection. Reuse that exact immutable snapshot instead of re-reading
	// every schema for every Go/GDScript file in the same run.
	l.mu.Lock()
	if l.root == input.Root && l.digest != "" && strings.Contains(input.SemanticKey, l.digest) {
		registry := l.registry
		l.mu.Unlock()
		return registry, nil
	}
	l.mu.Unlock()
	files, digest, err := relevantFiles(ctx, input.Root)
	if err != nil {
		return Registry{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.root == input.Root && l.digest == digest {
		return l.registry, nil
	}
	registry := buildRegistry(ctx, input, files, digest)
	l.root, l.digest, l.registry = input.Root, digest, registry
	return registry, nil
}

func (l *Loader) SemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	registry, err := l.Load(ctx, input)
	if err != nil {
		return "", err
	}
	return "protobuf-binding-v" + adapterVersion + ":" + registry.Digest, nil
}

func IsSemanticInput(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.EqualFold(filepath.Ext(path), ".proto") || base == "buf.gen.yaml" || base == "buf.gen.yml" || base == "buf.yaml" || base == "buf.yml"
}

type fileContent struct {
	path      string
	content   []byte
	readError string
}

func relevantFiles(ctx context.Context, root string) ([]fileContent, string, error) {
	paths, err := repositoryPaths(ctx, root)
	if err != nil {
		return nil, "", err
	}
	var files []fileContent
	hash := sha256.New()
	for _, path := range paths {
		if !IsSemanticInput(path) {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil {
			files = append(files, fileContent{path: clean(path), readError: readErr.Error()})
			_, _ = hash.Write([]byte(path))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write([]byte("unreadable"))
			_, _ = hash.Write([]byte{0})
			continue
		}
		files = append(files, fileContent{path: clean(path), content: content})
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(content)
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write([]byte(adapterVersion))
	return files, hex.EncodeToString(hash.Sum(nil)), nil
}

func repositoryPaths(ctx context.Context, root string) ([]string, error) {
	command := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if output, err := command.Output(); err == nil {
		var result []string
		for _, raw := range bytes.Split(output, []byte{0}) {
			if len(raw) > 0 {
				result = append(result, clean(string(raw)))
			}
		}
		sort.Strings(result)
		return result, nil
	}
	var result []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == ".grafo" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		result = append(result, clean(relative))
		return nil
	})
	sort.Strings(result)
	return result, err
}

type declaration struct {
	id         string
	canonical  string
	kind       graph.NodeKind
	form       string
	name       string
	file       string
	pkg        string
	goPackage  string
	properties map[string]string
}

func buildRegistry(ctx context.Context, input parserapi.Input, files []fileContent, digest string) Registry {
	result := Registry{Digest: digest, Configs: map[string]Config{}, Outputs: map[string]GeneratedFile{}}
	byPath := map[string][]byte{}
	var inputDiagnostics []graph.Diagnostic
	var declarations []declaration
	for _, file := range files {
		byPath[file.path] = file.content
		if file.readError != "" {
			inputDiagnostics = append(inputDiagnostics, diagnostic(file.path, 0, "read Protobuf binding input: "+file.readError))
			continue
		}
		if !strings.EqualFold(filepath.Ext(file.path), ".proto") {
			continue
		}
		parsed, err := protobufparser.New().Parse(ctx, parserapi.Input{Root: input.Root, Path: file.path, Content: file.content, Repository: input.Repository, RepoID: input.RepoID})
		if err != nil || len(parsed.Diagnostics) != 0 {
			continue
		}
		pkg, goPackage := "", ""
		for _, node := range parsed.Nodes {
			if node.Kind == graph.KindFile {
				pkg, goPackage = node.Properties["package"], node.Properties["go_package"]
				break
			}
		}
		for _, node := range parsed.Nodes {
			form := node.Properties["declaration"]
			if node.Kind != graph.KindType && node.Kind != graph.KindField {
				continue
			}
			if node.Kind == graph.KindType && form != "message" && form != "enum" {
				continue
			}
			declarations = append(declarations, declaration{id: node.ID, canonical: node.QualifiedName, kind: node.Kind, form: form,
				name: node.Name, file: file.path, pkg: pkg, goPackage: goPackage, properties: node.Properties})
		}
	}
	blockedOutputs := map[string]bool{}
	for _, file := range files {
		base := strings.ToLower(filepath.Base(file.path))
		if base != "buf.gen.yaml" && base != "buf.gen.yml" {
			continue
		}
		if file.readError != "" {
			continue
		}
		config, parsed := parseConfig(input, file, byPath, declarations)
		config.Diagnostics = append(config.Diagnostics, inputDiagnostics...)
		result.Configs[file.path] = config
		if parsed.outputs == nil {
			continue
		}
		for path, generated := range parsed.outputs {
			if blockedOutputs[path] {
				continue
			}
			if previous, exists := result.Outputs[path]; exists && previous != generated {
				config.Diagnostics = append(config.Diagnostics, graph.Diagnostic{Path: file.path, Line: generatedLine(parsed.line), Level: "warning", Message: "generated output path is configured more than once: " + path})
				delete(result.Outputs, path)
				blockedOutputs[path] = true
				result.Configs[file.path] = config
				continue
			}
			result.Outputs[path] = generated
		}
	}
	return result
}

type stringList []string

func (s *stringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*s = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if child.Kind != yaml.ScalarNode {
				return fmt.Errorf("options must be strings")
			}
			*s = append(*s, child.Value)
		}
		return nil
	default:
		return fmt.Errorf("options must be a string or sequence")
	}
}

type generationConfig struct {
	Version string `yaml:"version"`
	Managed struct {
		Enabled  bool `yaml:"enabled"`
		Override []struct {
			FileOption string `yaml:"file_option"`
			Value      string `yaml:"value"`
			Path       string `yaml:"path"`
			Module     string `yaml:"module"`
		} `yaml:"override"`
	} `yaml:"managed"`
	Plugins []struct {
		Remote   string     `yaml:"remote"`
		Local    string     `yaml:"local"`
		Out      string     `yaml:"out"`
		Opt      stringList `yaml:"opt"`
		Strategy string     `yaml:"strategy"`
	} `yaml:"plugins"`
}

type bufConfig struct {
	Version string `yaml:"version"`
	Modules []struct {
		Path string `yaml:"path"`
	} `yaml:"modules"`
}

type plannedConfig struct {
	line    int
	outputs map[string]GeneratedFile
}

func parseConfig(input parserapi.Input, file fileContent, all map[string][]byte, declarations []declaration) (Config, plannedConfig) {
	config := Config{Path: file.path, Line: 1}
	var raw generationConfig
	decoder := yaml.NewDecoder(bytes.NewReader(file.content))
	if err := decoder.Decode(&raw); err != nil {
		config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 0, "parse Buf generation configuration: "+err.Error()))
		return config, plannedConfig{}
	}
	if raw.Version != "v2" {
		config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, "unsupported Buf generation configuration version "+fmt.Sprintf("%q", raw.Version)))
		return config, plannedConfig{}
	}
	moduleRoots, err := moduleRoots(file.path, all)
	if err != nil {
		config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, err.Error()))
		return config, plannedConfig{}
	}
	canonicalCounts := map[string]int{}
	for _, declaration := range declarations {
		if _, _, ok := schemaSource(declaration.file, moduleRoots); ok {
			canonicalCounts[declaration.canonical]++
		}
	}
	prefix := ""
	unsupportedGoPackageOverride := false
	if raw.Managed.Enabled {
		for _, override := range raw.Managed.Override {
			if override.FileOption != "go_package_prefix" {
				continue
			}
			if override.Path != "" || override.Module != "" {
				config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, "path- or module-scoped go_package_prefix overrides are not supported"))
				unsupportedGoPackageOverride = true
				continue
			}
			if prefix != "" && prefix != override.Value {
				config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, "ambiguous go_package_prefix overrides"))
				return config, plannedConfig{}
			}
			prefix = strings.TrimRight(override.Value, "/")
		}
	}
	plan := plannedConfig{line: 1, outputs: map[string]GeneratedFile{}}
	for _, plugin := range raw.Plugins {
		generator, version, language, supported := supportedPlugin(plugin.Remote, plugin.Local)
		if !supported {
			name := plugin.Remote
			if name == "" {
				name = plugin.Local
			}
			config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, "unsupported Protobuf generator "+fmt.Sprintf("%q", name)))
			continue
		}
		if strings.TrimSpace(plugin.Out) == "" {
			config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, generator+" output path is required"))
			continue
		}
		if language == "go" && !hasOption(plugin.Opt, "paths=source_relative") {
			config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, "protoc-gen-go currently requires paths=source_relative"))
			continue
		}
		if language == "go" && unsupportedGoPackageOverride {
			continue
		}
		if unsupported := unsupportedOptions(plugin.Opt, language); unsupported != "" {
			config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, generator+" option is not supported: "+unsupported))
			continue
		}
		for _, declaration := range declarations {
			_, source, ok := schemaSource(declaration.file, moduleRoots)
			if !ok {
				continue
			}
			if canonicalCounts[declaration.canonical] != 1 {
				config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, "ambiguous canonical Protobuf declaration "+fmt.Sprintf("%q", declaration.canonical)))
				continue
			}
			outRoot := resolveRelative(filepath.Dir(file.path), plugin.Out)
			switch language {
			case "go":
				goPackage := goImportPath(declaration, prefix)
				if goPackage == "" {
					config.Diagnostics = append(config.Diagnostics, diagnostic(file.path, 1, "cannot determine Go package for "+source))
					continue
				}
				projections := goProjection(input, declaration, goPackage, generator, version, file.path)
				config.Projections = append(config.Projections, projections...)
				output := clean(filepath.Join(outRoot, strings.TrimSuffix(source, ".proto")+".pb.go"))
				plan.outputs[output] = GeneratedFile{Path: output, Language: language, Generator: generator, Version: version, Adapter: adapterVersion, Source: source, ConfigPath: file.path}
			case "gdscript":
				projections, className := gdscriptProjection(input, declaration, declarations, generator, version, file.path)
				config.Projections = append(config.Projections, projections...)
				if declaration.kind == graph.KindType {
					output := clean(filepath.Join(outRoot, className+".pb.gd"))
					plan.outputs[output] = GeneratedFile{Path: output, Language: language, Generator: generator, Version: version, Adapter: adapterVersion, Source: source, ConfigPath: file.path}
				}
			}
		}
	}
	sort.Slice(config.Projections, func(i, j int) bool {
		if config.Projections[i].Node.QualifiedName != config.Projections[j].Node.QualifiedName {
			return config.Projections[i].Node.QualifiedName < config.Projections[j].Node.QualifiedName
		}
		return config.Projections[i].Node.ID < config.Projections[j].Node.ID
	})
	config.Projections = uniqueProjections(config.Projections)
	return config, plan
}

func supportedPlugin(remote, local string) (generator, version, language string, ok bool) {
	if (remote == "") == (local == "") {
		return "", "", "", false
	}
	if remote != "" {
		parts := strings.Split(remote, ":")
		if len(parts) != 2 {
			return "", "", "", false
		}
		switch parts[0] {
		case "buf.build/protocolbuffers/go":
			if strings.HasPrefix(parts[1], "v1.") {
				return "protoc-gen-go", parts[1], "go", true
			}
		case "buf.build/cafecito-games/gdproto":
			if strings.HasPrefix(parts[1], "v0.6.") {
				return "protoc-gen-gdscript", parts[1], "gdscript", true
			}
		}
		return "", "", "", false
	}
	switch filepath.Base(local) {
	case "protoc-gen-go":
		return "protoc-gen-go", "local", "go", true
	case "protoc-gen-gdscript":
		return "protoc-gen-gdscript", "local", "gdscript", true
	default:
		return "", "", "", false
	}
}

func moduleRoots(configPath string, all map[string][]byte) ([]string, error) {
	dir := filepath.ToSlash(filepath.Dir(configPath))
	bufPath := clean(filepath.Join(dir, "buf.yaml"))
	content, ok := all[bufPath]
	if !ok {
		return []string{clean(dir)}, nil
	}
	var config bufConfig
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", bufPath, err)
	}
	if config.Version != "v2" {
		return nil, fmt.Errorf("unsupported Buf module configuration version %q", config.Version)
	}
	if len(config.Modules) == 0 {
		return []string{clean(dir)}, nil
	}
	var roots []string
	for _, module := range config.Modules {
		if module.Path == "" {
			return nil, fmt.Errorf("Buf module path must not be empty")
		}
		roots = append(roots, resolveRelative(dir, module.Path))
	}
	return roots, nil
}

func schemaSource(path string, roots []string) (string, string, bool) {
	path = clean(path)
	for _, root := range roots {
		prefix := strings.TrimSuffix(clean(root), "/")
		if prefix == "." {
			prefix = ""
		}
		if prefix == "" {
			return root, path, true
		}
		if strings.HasPrefix(path, prefix+"/") {
			return root, strings.TrimPrefix(path, prefix+"/"), true
		}
	}
	return "", "", false
}

func goImportPath(d declaration, prefix string) string {
	if prefix != "" {
		if d.pkg == "" {
			return prefix
		}
		return prefix + "/" + strings.ReplaceAll(d.pkg, ".", "/")
	}
	value := d.goPackage
	if before, _, ok := strings.Cut(value, ";"); ok {
		value = before
	}
	return strings.TrimSpace(value)
}

func goProjection(input parserapi.Input, d declaration, pkg, generator, version, configPath string) []Projection {
	path := schemaTypePath(d)
	if len(path) == 0 {
		return nil
	}
	typeName := goTypeName(path)
	qualified := pkg + "." + typeName
	if d.kind == graph.KindType {
		return []Projection{projection(input, graph.KindType, qualified, d.id, d.canonical, graph.KindType, "go", generator, version, configPath, map[string]string{"projection": d.form})}
	}
	ownerCanonical := strings.TrimSuffix(d.canonical, "."+d.name)
	owner := goTypeName(schemaTypePath(declaration{canonical: ownerCanonical, pkg: d.pkg}))
	if owner == "" {
		return nil
	}
	field := goCamel(d.name)
	if d.form == "enum_value" {
		return []Projection{projection(input, graph.KindField, pkg+"."+owner+"_"+goCamel(d.name), d.id, d.canonical, graph.KindField, "go", generator, version, configPath, map[string]string{"projection": "enum_value"})}
	}
	base := pkg + "." + owner
	result := []Projection{
		projection(input, graph.KindField, base+"."+field, d.id, d.canonical, graph.KindField, "go", generator, version, configPath, map[string]string{"projection": "field"}),
		projection(input, graph.KindMethod, base+".Get"+field, d.id, d.canonical, graph.KindField, "go", generator, version, configPath, map[string]string{"projection": "accessor", "accessor": "get"}),
	}
	if d.properties["oneof"] != "" {
		result = append(result, projection(input, graph.KindType, base+"_"+field, d.id, d.canonical, graph.KindField, "go", generator, version, configPath, map[string]string{"projection": "oneof_wrapper", "oneof": d.properties["oneof"]}))
	}
	return result
}

func gdscriptProjection(input parserapi.Input, d declaration, declarations []declaration, generator, version, configPath string) ([]Projection, string) {
	path := schemaTypePath(d)
	className := gdCamel(d.pkg) + gdCamel(strings.TrimSuffix(filepath.Base(d.file), filepath.Ext(d.file)))
	for _, part := range path {
		className += gdCamel(part)
	}
	if d.kind == graph.KindType {
		if d.form == "enum" {
			return []Projection{
				projection(input, graph.KindClass, className, d.id, d.canonical, graph.KindType, "gdscript", generator, version, configPath, map[string]string{"projection": "enum_container"}),
				projection(input, graph.KindType, className+"."+d.name, d.id, d.canonical, graph.KindType, "gdscript", generator, version, configPath, map[string]string{"projection": "enum"}),
			}, className
		}
		return []Projection{
			projection(input, graph.KindClass, className, d.id, d.canonical, graph.KindType, "gdscript", generator, version, configPath, map[string]string{"projection": d.form}),
			projection(input, graph.KindMethod, className+".new", d.id, d.canonical, graph.KindType, "gdscript", generator, version, configPath, map[string]string{"projection": "constructor"}),
		}, className
	}
	ownerCanonical := strings.TrimSuffix(d.canonical, "."+d.name)
	ownerPath := schemaTypePath(declaration{canonical: ownerCanonical, pkg: d.pkg})
	ownerClass := gdCamel(d.pkg) + gdCamel(strings.TrimSuffix(filepath.Base(d.file), filepath.Ext(d.file)))
	for _, part := range ownerPath {
		ownerClass += gdCamel(part)
	}
	if d.form == "enum_value" {
		return []Projection{projection(input, graph.KindField, ownerClass+"."+graph.SimpleName(ownerCanonical)+"."+d.name, d.id, d.canonical, graph.KindField, "gdscript", generator, version, configPath, map[string]string{"projection": "enum_value"})}, ownerClass
	}
	get := projection(input, graph.KindMethod, ownerClass+".get_"+d.name, d.id, d.canonical, graph.KindField, "gdscript", generator, version, configPath, map[string]string{"projection": "accessor", "accessor": "get"})
	result := []Projection{get}
	switch {
	case d.properties["cardinality"] == "repeated" || d.properties["cardinality"] == "map":
		result = append(result, projection(input, graph.KindMethod, ownerClass+".add_"+d.name, d.id, d.canonical, graph.KindField, "gdscript", generator, version, configPath, map[string]string{"projection": "accessor", "accessor": "add"}))
	case d.properties["oneof"] != "":
		result = append(result,
			projection(input, graph.KindMethod, ownerClass+".set_"+d.name, d.id, d.canonical, graph.KindField, "gdscript", generator, version, configPath, map[string]string{"projection": "accessor", "accessor": "set"}),
			projection(input, graph.KindMethod, ownerClass+".has_"+d.name, d.id, d.canonical, graph.KindField, "gdscript", generator, version, configPath, map[string]string{"projection": "accessor", "accessor": "has"}),
		)
	case declarationForm(declarations, d.pkg, d.properties["type"]) == "message":
		result = append(result, projection(input, graph.KindMethod, ownerClass+".new_"+d.name, d.id, d.canonical, graph.KindField, "gdscript", generator, version, configPath, map[string]string{"projection": "accessor", "accessor": "new"}))
	default:
		result = append(result, projection(input, graph.KindMethod, ownerClass+".set_"+d.name, d.id, d.canonical, graph.KindField, "gdscript", generator, version, configPath, map[string]string{"projection": "accessor", "accessor": "set"}))
	}
	return result, ownerClass
}

func declarationForm(declarations []declaration, pkg, canonical string) string {
	candidates := map[string]bool{canonical: true}
	if pkg != "" && !strings.HasPrefix(canonical, pkg+".") {
		candidates[pkg+"."+canonical] = true
	}
	for _, declaration := range declarations {
		if candidates[declaration.canonical] && declaration.kind == graph.KindType {
			return declaration.form
		}
	}
	return ""
}

func projection(input parserapi.Input, kind graph.NodeKind, symbol, canonicalID, canonical string, canonicalKind graph.NodeKind, language, generator, version, configPath string, extra map[string]string) Projection {
	properties := map[string]string{"generated_binding": "true", "generator": generator, "generator_version": version, "adapter_version": adapterVersion, "canonical": canonical, "config": configPath}
	for key, value := range extra {
		properties[key] = value
	}
	return Projection{Node: graph.Node{ID: graph.NodeID(kind, "binding:"+language+":"+symbol, input.RepoID, generator, canonical), Kind: kind, Name: graph.SimpleName(symbol), QualifiedName: symbol, Language: language, Location: graph.Location{Path: configPath, Line: 1, Column: 1, EndLine: 1}, Properties: properties}, CanonicalID: canonicalID, Canonical: canonical, CanonicalKind: canonicalKind, Properties: properties}
}

func schemaTypePath(d declaration) []string {
	name := strings.TrimPrefix(d.canonical, d.pkg)
	name = strings.TrimPrefix(name, ".")
	if name == "" {
		return nil
	}
	return strings.Split(name, ".")
}

func goTypeName(path []string) string {
	result := make([]string, 0, len(path))
	for _, part := range path {
		result = append(result, goCamel(part))
	}
	return strings.Join(result, "_")
}

func uniqueProjections(values []Projection) []Projection {
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		if seen[value.Node.ID] {
			continue
		}
		seen[value.Node.ID] = true
		result = append(result, value)
	}
	return result
}

func hasOption(options []string, wanted string) bool {
	for _, option := range options {
		for _, part := range strings.Split(option, ",") {
			if strings.TrimSpace(part) == wanted {
				return true
			}
		}
	}
	return false
}

func unsupportedOptions(options []string, language string) string {
	for _, option := range options {
		for _, part := range strings.Split(option, ",") {
			part = strings.TrimSpace(part)
			if part == "" || language == "go" && part == "paths=source_relative" {
				continue
			}
			return part
		}
	}
	return ""
}

func resolveRelative(base, value string) string {
	return clean(filepath.Join(base, filepath.FromSlash(value)))
}
func clean(value string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(value, "./"))))
}
func diagnostic(path string, line int, message string) graph.Diagnostic {
	return graph.Diagnostic{Path: path, Line: line, Level: "warning", Message: message}
}
func generatedLine(line int) int {
	if line < 1 {
		return 1
	}
	return line
}

func goCamel(value string) string {
	// Match the protoc-gen-go v1 GoCamelCase contract exactly. In particular,
	// underscores are retained unless followed by a lowercase letter, and a
	// leading underscore becomes X so distinct protobuf names stay distinct.
	result := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		current := value[i]
		switch {
		case current == '.' && i+1 < len(value) && asciiLower(value[i+1]):
			continue
		case current == '.':
			result = append(result, '_')
		case current == '_' && (i == 0 || value[i-1] == '.'):
			result = append(result, 'X')
		case current == '_' && i+1 < len(value) && asciiLower(value[i+1]):
			continue
		case current >= '0' && current <= '9':
			result = append(result, current)
		default:
			if asciiLower(current) {
				current -= 'a' - 'A'
			}
			result = append(result, current)
			for i+1 < len(value) && asciiLower(value[i+1]) {
				i++
				result = append(result, value[i])
			}
		}
	}
	return string(result)
}

func asciiLower(value byte) bool { return value >= 'a' && value <= 'z' }

func gdCamel(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == '/' })
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(goCamel(part))
	}
	return b.String()
}

var goSourcePattern = regexp.MustCompile(`(?m)^// source: ([^\r\n]+)$`)

func goHeader(content []byte) (string, string) {
	text := string(content)
	if !strings.HasPrefix(text, "// Code generated by protoc-gen-go. DO NOT EDIT.") {
		return "", ""
	}
	match := goSourcePattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return "", ""
	}
	return strings.TrimSpace(match[1]), "protoc-gen-go"
}

var gdSourcePattern = regexp.MustCompile(`(?m)^# Source: ([^\r\n]+)$`)

func gdscriptHeader(content []byte) (string, string) {
	text := string(content)
	if !strings.Contains(text, "# Generated by gdproto") || !strings.Contains(text, "# DO NOT EDIT") {
		return "", ""
	}
	match := gdSourcePattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return "", ""
	}
	return strings.TrimSpace(match[1]), "protoc-gen-gdscript"
}
