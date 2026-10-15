// Package sqlcbinding discovers repository-owned sqlc v2 configuration and
// validates generated Go query methods against canonical handwritten SQL.
package sqlcbinding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
	sqliteparser "github.com/cafecito-games/grafo/internal/parser/sql/sqlite"
	"github.com/cafecito-games/grafo/internal/parser/sqltemplate"
	"gopkg.in/yaml.v3"
)

const (
	AdapterVersion       = "2"
	workspaceSemanticKey = "sqlc-binding-workspace-v2"
	defaultMaxFileSize   = int64(5 << 20)
)

type GoConfig struct {
	Package             string
	Out                 string
	EmitPreparedQueries bool
	EmitExportedQueries bool
}

type Entry struct {
	ConfigPath  string
	Index       int
	Engine      string
	SchemaPaths []string
	QueryPaths  []string
	SchemaRoots []string
	QueryRoots  []string
	Go          GoConfig
	Valid       bool
}

type Query struct {
	Path           string
	Name           string
	Command        string
	StatementStart int
	StatementEnd   int
	SQL            string
	Engine         string
	ConfigPath     string
	Entry          int
}

type Binding struct {
	Method           string
	GeneratedPath    string
	Constant         string
	Generator        string
	GeneratorVersion string
	ConfigPath       string
	Entry            int
	Location         graph.Location
}

type Config struct {
	Path        string
	Entries     []Entry
	Diagnostics []graph.Diagnostic
}

type Registry struct {
	Digest              string
	configs             map[string]*Config
	entries             map[string][]Entry
	sqlPaths            map[string][]Entry
	queries             map[string]Query
	bindings            map[string][]Binding
	schemas             map[string]map[string]schemaTable
	invalidEntryQueries map[string]bool
	semanticKeys        map[string]string
}

type schemaTable struct {
	columns    []string
	ambiguous  bool
	incomplete bool
}

func (r Registry) Config(path string) (Config, bool) {
	config, ok := r.configs[clean(path)]
	if !ok {
		return Config{}, false
	}
	return cloneConfig(*config), true
}

func (r Registry) Configs() []Config {
	paths := make([]string, 0, len(r.configs))
	for path := range r.configs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result := make([]Config, 0, len(paths))
	for _, path := range paths {
		result = append(result, cloneConfig(*r.configs[path]))
	}
	return result
}

func (r Registry) EntriesForQuery(path string) []Entry {
	return append([]Entry(nil), r.entries[clean(path)]...)
}

func (r Registry) EntriesForSQLPath(path string) []Entry {
	return append([]Entry(nil), r.sqlPaths[clean(path)]...)
}

func (r Registry) Query(path, name string) (Query, bool) {
	query, ok := r.queries[queryKey(path, name)]
	return query, ok
}

func (r Registry) BindingsForQuery(path, name string) []Binding {
	return append([]Binding(nil), r.bindings[queryKey(path, name)]...)
}

// ColumnsForQuery returns complete, unique configured schema evidence for a
// table in the binding scopes that own queryPath. Compatible overlapping
// entries may agree; missing, conflicting, or ambiguous declarations fail
// closed.
func (r Registry) ColumnsForQuery(queryPath, table string) ([]string, bool) {
	_, columns, ok := r.SchemaColumnsForQuery(queryPath, table)
	return columns, ok
}

func (r Registry) SchemaColumnsForQuery(queryPath, table string) (string, []string, bool) {
	qualified := ""
	var agreed []string
	found := false
	for _, entry := range r.EntriesForQuery(queryPath) {
		tables := r.schemas[entryKey(entry)]
		resolved, candidate, ok := schemaColumns(tables, table, entry.Engine)
		if !ok {
			return "", nil, false
		}
		if !found {
			qualified, agreed, found = resolved, candidate, true
			continue
		}
		if qualified != resolved || strings.Join(agreed, "\x00") != strings.Join(candidate, "\x00") {
			return "", nil, false
		}
	}
	return qualified, append([]string(nil), agreed...), found
}

type Loader struct {
	buildMu sync.Mutex
	mu      sync.Mutex
	root    string
	digest  string
	value   Registry
}

func NewLoader() *Loader { return &Loader{} }

func (l *Loader) Load(ctx context.Context, input parserapi.Input) (Registry, error) {
	if input.Root == "" {
		return emptyRegistry(""), nil
	}
	if registry, ok := l.cached(input.Root, input.SemanticKey); ok {
		return registry, nil
	}
	l.buildMu.Lock()
	defer l.buildMu.Unlock()
	if registry, ok := l.cached(input.Root, input.SemanticKey); ok {
		return registry, nil
	}
	files, digest, err := relevantFiles(ctx, input)
	if err != nil {
		return Registry{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.root == input.Root && l.digest == digest {
		return l.value, nil
	}
	registry := buildRegistry(ctx, input, files, digest)
	l.root, l.digest, l.value = input.Root, digest, registry
	return registry, nil
}

func (l *Loader) cached(root, semanticKey string) (Registry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.root == root && l.digest != "" && semanticKey == workspaceSemanticKey {
		return l.value, true
	}
	return Registry{}, false
}

func (l *Loader) SemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	_, err := l.Load(ctx, input)
	if err != nil {
		return "", err
	}
	return workspaceSemanticKey, nil
}

func (l *Loader) FileSemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	registry, err := l.Load(ctx, input)
	if err != nil {
		return "", err
	}
	if key := registry.semanticKeys[clean(input.Path)]; key != "" {
		return "sqlc-binding-v" + AdapterVersion + ":" + key, nil
	}
	return "sqlc-binding-v" + AdapterVersion + ":unowned", nil
}

// AffectedPaths derives the exact config and query files whose binding evidence
// can change after changedPaths. It consults only a registry already published
// for root by WorkspaceSemanticKey; callers use the false result as unknown and
// may fall back conservatively.
func (l *Loader) AffectedPaths(root string, changedPaths []string) (queries, configs []string, known bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.root != root || l.digest == "" {
		return nil, nil, false
	}
	querySet := map[string]bool{}
	configSet := map[string]bool{}
	all := false
	for _, changed := range changedPaths {
		changed = clean(changed)
		if IsConfigPath(changed) || changed == "go.mod" {
			all = true
			continue
		}
		for _, config := range l.value.configs {
			for _, entry := range config.Entries {
				affects := pathMatchesRoots(entry.SchemaRoots, changed) || pathMatchesRoots(entry.QueryRoots, changed)
				if entry.Go.Out != "" && pathWithin(entry.Go.Out, changed) {
					affects = true
				}
				if !affects {
					continue
				}
				configSet[config.Path] = true
				for _, queryPath := range entry.QueryPaths {
					querySet[queryPath] = true
				}
				for _, schemaPath := range entry.SchemaPaths {
					querySet[schemaPath] = true
				}
			}
		}
	}
	if all {
		for _, config := range l.value.configs {
			configSet[config.Path] = true
			for _, entry := range config.Entries {
				for _, queryPath := range entry.QueryPaths {
					querySet[queryPath] = true
				}
				for _, schemaPath := range entry.SchemaPaths {
					querySet[schemaPath] = true
				}
			}
		}
	}
	for value := range querySet {
		queries = append(queries, value)
	}
	for value := range configSet {
		configs = append(configs, value)
	}
	sort.Strings(queries)
	sort.Strings(configs)
	return queries, configs, true
}

func pathMatchesRoots(roots []string, target string) bool {
	for _, root := range roots {
		if root == target || strings.HasPrefix(target, strings.TrimSuffix(root, "/")+"/") {
			return true
		}
	}
	return false
}

func pathWithin(root, target string) bool {
	root = clean(root)
	target = clean(target)
	return root == "." || root == target || strings.HasPrefix(target, strings.TrimSuffix(root, "/")+"/")
}

func IsConfigPath(filePath string) bool {
	base := strings.ToLower(filepath.Base(filePath))
	return base == "sqlc.yaml" || base == "sqlc.yml" || base == "sqlc.json"
}

func IsSemanticInput(filePath string) bool {
	return IsConfigPath(filePath)
}

func IsBindingInput(filePath string) bool {
	extension := strings.ToLower(filepath.Ext(filePath))
	return IsConfigPath(filePath) || isSQLSourceExtension(extension) || extension == ".go" || clean(filePath) == "go.mod"
}

func isSQLSourceExtension(extension string) bool {
	return extension == ".sql" || extension == ".psql" || extension == ".pgsql"
}

type fileContent struct {
	path      string
	content   []byte
	readError string
}

func relevantFiles(ctx context.Context, input parserapi.Input) ([]fileContent, string, error) {
	paths := append([]string(nil), input.SourcePaths...)
	if paths == nil {
		var err error
		paths, err = repositoryPaths(ctx, input.Root)
		if err != nil {
			return nil, "", err
		}
	}
	for index := range paths {
		paths[index] = clean(paths[index])
	}
	sort.Strings(paths)
	selected := map[string]bool{}
	configContents := map[string][]byte{}
	for _, filePath := range paths {
		if !IsConfigPath(filePath) {
			continue
		}
		selected[filePath] = true
		if content, err := secureRead(input.Root, filePath, input.MaxFileSize); err == nil {
			configContents[filePath] = content
		}
	}
	var outputRoots []string
	for configPath, content := range configContents {
		var raw rawConfig
		if err := yaml.Unmarshal(content, &raw); err != nil {
			continue
		}
		for _, entry := range raw.SQL {
			for _, inputPath := range append(append([]string(nil), entry.Schema...), entry.Queries...) {
				resolved, ok := resolveRelative(configPath, inputPath)
				if !ok {
					continue
				}
				if containsCleanPath(paths, resolved) {
					selected[resolved] = true
					continue
				}
				for _, candidate := range paths {
					if sqlcDirectoryCandidate(resolved, candidate) {
						selected[candidate] = true
					}
				}
			}
			if entry.Gen.Go == nil {
				continue
			}
			out, ok := resolveRelative(configPath, entry.Gen.Go.Out)
			if ok {
				outputRoots = append(outputRoots, out)
			}
		}
	}
	for _, filePath := range paths {
		if !strings.EqualFold(filepath.Ext(filePath), ".go") {
			continue
		}
		for _, root := range outputRoots {
			if pathWithin(root, filePath) {
				selected[filePath] = true
				break
			}
		}
	}
	selectedPaths := make([]string, 0, len(selected))
	for filePath := range selected {
		selectedPaths = append(selectedPaths, filePath)
	}
	sort.Strings(selectedPaths)
	hash := sha256.New()
	var files []fileContent
	for _, filePath := range selectedPaths {
		content, readErr := secureRead(input.Root, filePath, input.MaxFileSize)
		item := fileContent{path: filePath, content: content}
		if readErr != nil {
			item.readError = readErr.Error()
		}
		files = append(files, item)
		_, _ = hash.Write([]byte(filePath))
		_, _ = hash.Write([]byte{0})
		if readErr != nil {
			_, _ = hash.Write([]byte("unreadable:" + readErr.Error()))
		} else {
			_, _ = hash.Write(content)
		}
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write([]byte(AdapterVersion))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(input.GoModule))
	return files, hex.EncodeToString(hash.Sum(nil)), nil
}

func containsCleanPath(paths []string, target string) bool {
	index := sort.SearchStrings(paths, target)
	return index < len(paths) && paths[index] == target
}

func sqlcDirectoryCandidate(directory, candidate string) bool {
	if path.Dir(candidate) != strings.TrimSuffix(directory, "/") {
		return false
	}
	base := path.Base(candidate)
	return !strings.HasPrefix(base, ".") && strings.HasSuffix(strings.ToLower(base), ".sql") && !strings.HasSuffix(strings.ToLower(base), ".down.sql")
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
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if filePath != root && (entry.Name() == ".git" || entry.Name() == ".grafo" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		result = append(result, clean(relative))
		return nil
	})
	sort.Strings(result)
	return result, err
}

func secureRead(root, relative string, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		maximum = defaultMaxFileSize
	}
	full := filepath.Join(root, filepath.FromSlash(relative))
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path %q escapes repository root", relative)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path %q is not a regular file", relative)
	}
	if info.Size() > maximum {
		return nil, fmt.Errorf("path %q exceeds maximum file size of %d bytes", relative, maximum)
	}
	return os.ReadFile(resolved)
}

type stringList []string

func (s *stringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var value string
		if err := node.Decode(&value); err != nil {
			return err
		}
		*s = []string{value}
		return nil
	case yaml.SequenceNode:
		var values []string
		if err := node.Decode(&values); err != nil {
			return err
		}
		*s = values
		return nil
	default:
		return fmt.Errorf("expected a path or list of paths")
	}
}

type rawConfig struct {
	Version string     `yaml:"version"`
	SQL     []rawEntry `yaml:"sql"`
}

type rawEntry struct {
	Engine  string     `yaml:"engine"`
	Schema  stringList `yaml:"schema"`
	Queries stringList `yaml:"queries"`
	Gen     struct {
		Go *struct {
			Package             string `yaml:"package"`
			Out                 string `yaml:"out"`
			EmitPreparedQueries bool   `yaml:"emit_prepared_queries"`
			EmitExportedQueries bool   `yaml:"emit_exported_queries"`
		} `yaml:"go"`
	} `yaml:"gen"`
}

func emptyRegistry(digest string) Registry {
	return Registry{Digest: digest, configs: map[string]*Config{}, entries: map[string][]Entry{}, sqlPaths: map[string][]Entry{}, queries: map[string]Query{}, bindings: map[string][]Binding{}, schemas: map[string]map[string]schemaTable{}, invalidEntryQueries: map[string]bool{}, semanticKeys: map[string]string{}}
}

func buildRegistry(ctx context.Context, input parserapi.Input, files []fileContent, digest string) Registry {
	registry := emptyRegistry(digest)
	contents := map[string]fileContent{}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		contents[file.path] = file
		paths = append(paths, file.path)
	}
	for _, file := range files {
		if !IsConfigPath(file.path) {
			continue
		}
		config := &Config{Path: file.path}
		registry.configs[file.path] = config
		if file.readError != "" {
			addDiagnostic(config, file.path, 1, "read sqlc configuration: "+file.readError)
			continue
		}
		var raw rawConfig
		decoder := yaml.NewDecoder(bytes.NewReader(file.content))
		if err := decoder.Decode(&raw); err != nil {
			addDiagnostic(config, file.path, 1, "parse sqlc configuration: "+err.Error())
			continue
		}
		if raw.Version != "2" {
			addDiagnostic(config, file.path, 1, fmt.Sprintf("sqlc configuration requires version 2, got %q", raw.Version))
			continue
		}
		for index, rawEntry := range raw.SQL {
			entry := buildEntry(config, index, rawEntry, paths, contents)
			config.Entries = append(config.Entries, entry)
			if !entry.Valid {
				continue
			}
			registry.schemas[entryKey(entry)] = parseSchema(ctx, config, entry, input, contents)
			for _, schemaPath := range entry.SchemaPaths {
				registry.sqlPaths[schemaPath] = append(registry.sqlPaths[schemaPath], entry)
			}
			names := map[string][]string{}
			for _, queryPath := range entry.QueryPaths {
				registry.entries[queryPath] = append(registry.entries[queryPath], entry)
				registry.sqlPaths[queryPath] = append(registry.sqlPaths[queryPath], entry)
				for _, name := range parseQueries(&registry, config, entry, contents[queryPath]) {
					folded := strings.ToLower(name)
					names[folded] = append(names[folded], queryPath)
				}
			}
			for name, queryPaths := range names {
				if len(queryPaths) < 2 {
					continue
				}
				sort.Strings(queryPaths)
				addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] query name %q is duplicated across %s; bindings disabled", index, name, strings.Join(queryPaths, ", ")))
				for _, queryPath := range queryPaths {
					registry.invalidEntryQueries[entryQueryKey(entry, queryPath, name)] = true
				}
			}
		}
	}
	configPaths := make([]string, 0, len(registry.configs))
	for configPath := range registry.configs {
		configPaths = append(configPaths, configPath)
	}
	sort.Strings(configPaths)
	for _, configPath := range configPaths {
		config := registry.configs[configPath]
		for _, entry := range config.Entries {
			if entry.Valid && entry.Go.Out != "" {
				scanGenerated(&registry, config, entry, input, paths, contents)
			}
		}
		sort.Slice(config.Diagnostics, func(i, j int) bool {
			if config.Diagnostics[i].Path != config.Diagnostics[j].Path {
				return config.Diagnostics[i].Path < config.Diagnostics[j].Path
			}
			if config.Diagnostics[i].Line != config.Diagnostics[j].Line {
				return config.Diagnostics[i].Line < config.Diagnostics[j].Line
			}
			return config.Diagnostics[i].Message < config.Diagnostics[j].Message
		})
	}
	for key := range registry.bindings {
		sort.Slice(registry.bindings[key], func(i, j int) bool {
			left, right := registry.bindings[key][i], registry.bindings[key][j]
			if left.Method != right.Method {
				return left.Method < right.Method
			}
			if left.ConfigPath != right.ConfigPath {
				return left.ConfigPath < right.ConfigPath
			}
			if left.Entry != right.Entry {
				return left.Entry < right.Entry
			}
			return left.GeneratedPath < right.GeneratedPath
		})
	}
	registry.semanticKeys = buildSemanticKeys(registry, input, paths, contents)
	return registry
}

func buildSemanticKeys(registry Registry, input parserapi.Input, paths []string, contents map[string]fileContent) map[string]string {
	targetInputs := map[string]map[string]bool{}
	add := func(target string, inputs ...string) {
		if targetInputs[target] == nil {
			targetInputs[target] = map[string]bool{}
		}
		for _, inputPath := range inputs {
			if inputPath != "" {
				targetInputs[target][inputPath] = true
			}
		}
	}
	for configPath, config := range registry.configs {
		add(configPath, configPath)
		for _, entry := range config.Entries {
			entryInputs := []string{configPath}
			entryInputs = append(entryInputs, entry.SchemaPaths...)
			entryInputs = append(entryInputs, entry.QueryPaths...)
			if entry.Go.Out != "" {
				for _, candidate := range paths {
					if strings.EqualFold(filepath.Ext(candidate), ".go") && pathWithin(entry.Go.Out, candidate) {
						entryInputs = append(entryInputs, candidate)
					}
				}
			}
			add(configPath, entryInputs...)
			for _, target := range append(append([]string(nil), entry.SchemaPaths...), entry.QueryPaths...) {
				add(target, entryInputs...)
			}
		}
	}
	result := make(map[string]string, len(targetInputs))
	for target, inputSet := range targetInputs {
		inputPaths := make([]string, 0, len(inputSet))
		for inputPath := range inputSet {
			inputPaths = append(inputPaths, inputPath)
		}
		sort.Strings(inputPaths)
		hash := sha256.New()
		for _, inputPath := range inputPaths {
			file := contents[inputPath]
			_, _ = hash.Write([]byte(inputPath))
			_, _ = hash.Write([]byte{0})
			if file.readError != "" {
				_, _ = hash.Write([]byte("unreadable:" + file.readError))
			} else {
				_, _ = hash.Write(file.content)
			}
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(input.GoModule))
		result[target] = hex.EncodeToString(hash.Sum(nil))
	}
	return result
}

func parseSchema(ctx context.Context, config *Config, entry Entry, input parserapi.Input, contents map[string]fileContent) map[string]schemaTable {
	result := map[string]schemaTable{}
	incompleteNames := map[string]bool{}
	incompleteSchemas := map[string]bool{}
	var declaredTables []string
	valid := true
	for _, schemaPath := range entry.SchemaPaths {
		file := contents[schemaPath]
		parseInput := parserapi.Input{Path: schemaPath, Content: file.content, Repository: input.Repository, RepoID: input.RepoID}
		var parsed graph.ParseResult
		var err error
		if entry.Engine == "sqlite" {
			parsed, err = sqliteparser.New().Parse(ctx, parseInput)
		} else {
			parsed, err = postgresparser.New().Parse(ctx, parseInput)
		}
		if err != nil {
			addDiagnostic(config, schemaPath, 1, fmt.Sprintf("parse configured %s schema: %v", entry.Engine, err))
			valid = false
			continue
		}
		if len(parsed.Diagnostics) > 0 {
			valid = false
			for _, diagnostic := range parsed.Diagnostics {
				addDiagnostic(config, schemaPath, diagnostic.Line, fmt.Sprintf("parse configured %s schema: %s", entry.Engine, diagnostic.Message))
			}
			continue
		}
		type locatedColumn struct {
			name         string
			line, column int
		}
		columns := map[string][]locatedColumn{}
		for _, node := range parsed.Nodes {
			for _, schema := range strings.Split(node.Properties["incomplete_schemas"], "\x00") {
				if schema != "" {
					incompleteSchemas[schema] = true
				}
			}
			if node.Kind == graph.KindTable {
				declaredTables = append(declaredTables, node.QualifiedName)
				if node.Properties["columns_incomplete"] == "true" {
					incompleteNames[node.QualifiedName] = true
				}
			}
			if node.Kind == graph.KindColumn {
				table := strings.TrimSuffix(node.QualifiedName, "."+node.Name)
				columns[table] = append(columns[table], locatedColumn{name: node.Name, line: node.Location.Line, column: node.Location.Column})
			}
		}
		for _, fact := range parsed.Facts {
			if fact.Kind == graph.EdgeWrites && fact.Properties["statement"] != "" && fact.Target != "" {
				incompleteNames[fact.Target] = true
			}
		}
		for table, located := range columns {
			sort.Slice(located, func(i, j int) bool {
				if located[i].line != located[j].line {
					return located[i].line < located[j].line
				}
				return located[i].column < located[j].column
			})
			names := make([]string, 0, len(located))
			for _, column := range located {
				names = append(names, column.name)
			}
			if existing, exists := result[table]; exists {
				existing.ambiguous = true
				result[table] = existing
				continue
			}
			result[table] = schemaTable{columns: names}
		}
	}
	for table, value := range result {
		tableSchema, tableName := schemaTableIdentity(table, entry.Engine)
		declarations := 0
		for _, declared := range declaredTables {
			declaredSchema, declaredName := schemaTableIdentity(declared, entry.Engine)
			if identifiersEqual(tableSchema, declaredSchema, entry.Engine) && identifiersEqual(tableName, declaredName, entry.Engine) {
				declarations++
			}
		}
		value.ambiguous = value.ambiguous || declarations > 1
		for incomplete := range incompleteNames {
			incompleteSchema, incompleteName := schemaTableIdentity(incomplete, entry.Engine)
			if identifiersEqual(tableSchema, incompleteSchema, entry.Engine) && identifiersEqual(tableName, incompleteName, entry.Engine) {
				value.incomplete = true
				break
			}
		}
		for incompleteSchema := range incompleteSchemas {
			if identifiersEqual(tableSchema, incompleteSchema, entry.Engine) {
				value.incomplete = true
				break
			}
		}
		result[table] = value
	}
	if !valid {
		return nil
	}
	return result
}

func schemaTableIdentity(name, engine string) (string, string) {
	schema, table := splitTableName(name)
	if schema == "" {
		switch engine {
		case "postgres":
			schema = "public"
		case "sqlite":
			schema = "main"
		}
	}
	return schema, table
}

func schemaColumns(tables map[string]schemaTable, selector, engine string) (string, []string, bool) {
	selectorSchema, selectorName := splitTableName(selector)
	defaultSchema := ""
	switch engine {
	case "postgres":
		defaultSchema = "public"
	case "sqlite":
		defaultSchema = "main"
	}
	selectorUsesDefault := selectorSchema == "" || identifiersEqual(selectorSchema, defaultSchema, engine)
	var matched schemaTable
	qualified := ""
	count := 0
	for candidate, table := range tables {
		candidateSchema, candidateName := splitTableName(candidate)
		if !identifiersEqual(candidateName, selectorName, engine) {
			continue
		}
		if selectorUsesDefault {
			if candidateSchema != "" && !identifiersEqual(candidateSchema, defaultSchema, engine) {
				continue
			}
		} else if !identifiersEqual(candidateSchema, selectorSchema, engine) {
			continue
		}
		matched = table
		qualified = candidate
		count++
	}
	return qualified, matched.columns, count == 1 && !matched.ambiguous && !matched.incomplete && len(matched.columns) > 0
}

func identifiersEqual(left, right, engine string) bool {
	if engine == "sqlite" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func splitTableName(name string) (string, string) {
	index := strings.LastIndexByte(name, '.')
	if index < 0 {
		return "", name
	}
	return name[:index], name[index+1:]
}

func buildEntry(config *Config, index int, raw rawEntry, paths []string, contents map[string]fileContent) Entry {
	entry := Entry{ConfigPath: config.Path, Index: index, Valid: true}
	entry.SchemaRoots = resolvedRoots(config.Path, raw.Schema)
	entry.QueryRoots = resolvedRoots(config.Path, raw.Queries)
	switch strings.ToLower(strings.TrimSpace(raw.Engine)) {
	case "sqlite":
		entry.Engine = "sqlite"
	case "postgres", "postgresql":
		entry.Engine = "postgres"
	default:
		addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] has unsupported engine %q", index, raw.Engine))
		entry.Valid = false
	}
	entry.SchemaPaths = expandPaths(config, index, "schema", raw.Schema, paths, contents)
	entry.QueryPaths = expandPaths(config, index, "queries", raw.Queries, paths, contents)
	if len(entry.SchemaPaths) == 0 || len(entry.QueryPaths) == 0 {
		entry.Valid = false
	}
	if raw.Gen.Go == nil {
		addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] has no built-in gen.go configuration; Go bindings disabled", index))
		return entry
	}
	out, ok := resolveRelative(config.Path, raw.Gen.Go.Out)
	if !ok || out == "." && strings.TrimSpace(raw.Gen.Go.Out) == "" {
		addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] gen.go.out must be repository-relative", index))
		entry.Valid = false
	}
	if strings.TrimSpace(raw.Gen.Go.Package) == "" {
		addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] gen.go.package is required", index))
		entry.Valid = false
	}
	entry.Go = GoConfig{Package: raw.Gen.Go.Package, Out: out, EmitPreparedQueries: raw.Gen.Go.EmitPreparedQueries, EmitExportedQueries: raw.Gen.Go.EmitExportedQueries}
	return entry
}

func expandPaths(config *Config, entry int, field string, values []string, paths []string, contents map[string]fileContent) []string {
	seen := map[string]bool{}
	for _, value := range values {
		resolved, ok := resolveRelative(config.Path, value)
		if !ok {
			addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] %s path %q is absolute or escapes the repository", entry, field, value))
			continue
		}
		if file, exists := contents[resolved]; exists {
			if file.readError != "" {
				addDiagnostic(config, resolved, 1, fmt.Sprintf("sqlc sql[%d] %s input is unavailable: %s", entry, field, file.readError))
				continue
			}
			seen[resolved] = true
			continue
		}
		matched := false
		for _, candidate := range paths {
			if sqlcDirectoryCandidate(resolved, candidate) {
				if contents[candidate].readError != "" {
					continue
				}
				seen[candidate] = true
				matched = true
			}
		}
		if !matched {
			addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] %s path %q is missing or excluded", entry, field, value))
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func resolvedRoots(configPath string, values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		if resolved, ok := resolveRelative(configPath, value); ok {
			seen[resolved] = true
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func resolveRelative(configPath, value string) (string, bool) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" || path.IsAbs(value) || filepath.IsAbs(value) {
		return "", false
	}
	base := path.Dir(clean(configPath))
	joined := path.Clean(path.Join(base, value))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return "", false
	}
	return clean(joined), true
}

func parseQueries(registry *Registry, config *Config, entry Entry, file fileContent) []string {
	if file.readError != "" {
		return nil
	}
	document := sqltemplate.Analyze(file.content, entry.Engine)
	for _, diagnostic := range document.Diagnostics {
		line, _ := document.Position(diagnostic.Offset)
		addDiagnostic(config, file.path, line, "SQL template: "+diagnostic.Message)
	}
	if len(document.Diagnostics) > 0 {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, named := range document.Queries {
		folded := strings.ToLower(named.Name)
		if seen[folded] {
			continue
		}
		seen[folded] = true
		names = append(names, named.Name)
		query := Query{Path: file.path, Name: named.Name, Command: named.Command,
			StatementStart: named.StatementStart, StatementEnd: named.StatementEnd,
			SQL: string(file.content[named.StatementStart:named.StatementEnd]), Engine: entry.Engine,
			ConfigPath: entry.ConfigPath, Entry: entry.Index}
		key := queryKey(file.path, named.Name)
		if existing, ok := registry.queries[key]; ok {
			if existing.Engine != query.Engine {
				addDiagnostic(config, file.path, 1, fmt.Sprintf("query %q has conflicting sqlc engines %s and %s", named.Name, existing.Engine, query.Engine))
				registry.invalidEntryQueries[entryQueryKey(entry, file.path, named.Name)] = true
				registry.invalidEntryQueries[entryQueryKey(Entry{ConfigPath: existing.ConfigPath, Index: existing.Entry}, existing.Path, existing.Name)] = true
			}
			continue
		}
		registry.queries[key] = query
	}
	return names
}

var sourceHeader = regexp.MustCompile(`(?m)^// source:\s*(\S+)\s*$`)
var versionHeader = regexp.MustCompile(`(?m)^//\s+sqlc\s+(v?[^\s]+)\s*$`)
var packageDeclaration = regexp.MustCompile(`(?m)^package\s+[A-Za-z_]`)

func scanGenerated(registry *Registry, config *Config, entry Entry, input parserapi.Input, paths []string, contents map[string]fileContent) {
	found := false
	for _, generatedPath := range paths {
		if !pathWithin(entry.Go.Out, generatedPath) || !strings.EqualFold(filepath.Ext(generatedPath), ".go") {
			continue
		}
		base := strings.ToLower(filepath.Base(generatedPath))
		content := contents[generatedPath]
		if content.readError != "" || (!strings.HasSuffix(base, ".sql.go") && !bytes.Contains(content.content, []byte("// source:"))) {
			continue
		}
		found = true
		firstLine := content.content
		if newline := bytes.IndexByte(firstLine, '\n'); newline >= 0 {
			firstLine = firstLine[:newline]
		}
		firstLine = bytes.TrimSuffix(firstLine, []byte{'\r'})
		if !bytes.Equal(firstLine, []byte("// Code generated by sqlc. DO NOT EDIT.")) {
			addDiagnostic(config, generatedPath, 1, "configured sqlc Go output lacks the supported generated-file header")
			continue
		}
		header := content.content
		if location := packageDeclaration.FindIndex(content.content); location != nil {
			header = content.content[:location[0]]
		}
		match := sourceHeader.FindSubmatch(header)
		if len(match) != 2 {
			addDiagnostic(config, generatedPath, 1, "sqlc generated Go file lacks a source header")
			continue
		}
		queryPath, reason := matchQuerySource(string(match[1]), entry.QueryPaths)
		if reason != "" {
			addDiagnostic(config, generatedPath, 1, reason)
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, generatedPath, content.content, parser.ParseComments)
		if err != nil || file == nil {
			addDiagnostic(config, generatedPath, 1, "parse sqlc generated Go: "+fmt.Sprint(err))
			continue
		}
		if file.Name.Name != entry.Go.Package {
			addDiagnostic(config, generatedPath, 1, fmt.Sprintf("sqlc generated Go package %q does not match configured package %q", file.Name.Name, entry.Go.Package))
			continue
		}
		constants := goStringConstants(file)
		version := ""
		if match := versionHeader.FindSubmatch(header); len(match) == 2 {
			version = string(match[1])
		}
		for _, declaration := range file.Decls {
			method, ok := declaration.(*goast.FuncDecl)
			if !ok || method.Recv == nil || method.Body == nil {
				continue
			}
			query, ok := registry.Query(queryPath, method.Name.Name)
			if !ok || registry.invalidEntryQueries[entryQueryKey(entry, queryPath, method.Name.Name)] {
				continue
			}
			references := referencedIdentifiers(method.Body)
			constant := ""
			for identifier := range references {
				value, ok := constants[identifier]
				if !ok || !equivalentSQL(query.SQL, value, entry.Engine, registry.schemas[entryKey(entry)]) {
					continue
				}
				if constant != "" {
					constant = ""
					break
				}
				constant = identifier
			}
			if constant == "" {
				addDiagnostic(config, generatedPath, fset.Position(method.Pos()).Line, fmt.Sprintf("sqlc method %s does not uniquely reference current canonical SQL for query %q", method.Name.Name, query.Name))
				continue
			}
			receiver := receiverName(method.Recv.List[0].Type)
			if receiver != "Queries" {
				continue
			}
			directory := path.Dir(generatedPath)
			packageName := file.Name.Name
			if input.GoModule != "" {
				packageName = strings.TrimSuffix(input.GoModule, "/")
				if directory != "." {
					packageName += "/" + directory
				}
			} else if directory != "." {
				packageName = directory
			}
			position := fset.Position(method.Pos())
			end := fset.Position(method.End())
			binding := Binding{Method: packageName + "." + receiver + "." + method.Name.Name,
				GeneratedPath: generatedPath, Constant: constant, Generator: "sqlc", GeneratorVersion: version,
				ConfigPath: entry.ConfigPath, Entry: entry.Index,
				Location: graph.Location{Path: generatedPath, Line: position.Line, Column: position.Column, EndLine: end.Line}}
			registry.bindings[queryKey(queryPath, query.Name)] = append(registry.bindings[queryKey(queryPath, query.Name)], binding)
		}
	}
	if !found {
		addDiagnostic(config, config.Path, 1, fmt.Sprintf("sqlc sql[%d] gen.go.out %q has no indexed generated query files", entry.Index, entry.Go.Out))
	}
}

func matchQuerySource(source string, candidates []string) (string, string) {
	source = clean(source)
	var exact, base []string
	for _, candidate := range candidates {
		if candidate == source {
			exact = append(exact, candidate)
		}
		if filepath.Base(candidate) == filepath.Base(source) {
			base = append(base, candidate)
		}
	}
	if len(exact) == 1 {
		return exact[0], ""
	}
	if len(base) == 1 {
		return base[0], ""
	}
	if len(base) > 1 {
		return "", fmt.Sprintf("sqlc source header %q matches more than one configured query file", source)
	}
	return "", fmt.Sprintf("sqlc source header %q does not match a configured query file", source)
}

func goStringConstants(file *goast.File) map[string]string {
	result := map[string]string{}
	for _, declaration := range file.Decls {
		group, ok := declaration.(*goast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, specification := range group.Specs {
			values, ok := specification.(*goast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range values.Names {
				if index >= len(values.Values) {
					continue
				}
				literal, ok := values.Values[index].(*goast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err == nil {
					result[name.Name] = value
				}
			}
		}
	}
	return result
}

func referencedIdentifiers(node goast.Node) map[string]bool {
	result := map[string]bool{}
	goast.Inspect(node, func(node goast.Node) bool {
		if identifier, ok := node.(*goast.Ident); ok {
			result[identifier.Name] = true
		}
		return true
	})
	return result
}

func receiverName(expression goast.Expr) string {
	switch expression := expression.(type) {
	case *goast.Ident:
		return expression.Name
	case *goast.StarExpr:
		return receiverName(expression.X)
	case *goast.IndexExpr:
		return receiverName(expression.X)
	case *goast.IndexListExpr:
		return receiverName(expression.X)
	default:
		return ""
	}
}

func equivalentSQL(handwritten, generated, dialect string, schemas map[string]schemaTable) bool {
	document := sqltemplate.Analyze([]byte(handwritten), dialect)
	if len(document.Diagnostics) > 0 {
		return false
	}
	if comparableSQL(string(document.SQL)) == comparableSQL(generated) {
		return true
	}
	expanded := append([]byte(nil), []byte(handwritten)...)
	for index := len(document.Macros) - 1; index >= 0; index-- {
		macro := document.Macros[index]
		if !macro.Embed {
			continue
		}
		_, columns, ok := schemaForEmbed(schemas, handwritten, macro.Argument, dialect)
		if !ok {
			return false
		}
		values := make([]string, 0, len(columns))
		for _, column := range columns {
			values = append(values, macro.Argument+"."+column)
		}
		replacement := []byte(strings.Join(values, ", "))
		expanded = append(append(append([]byte(nil), expanded[:macro.Start]...), replacement...), expanded[macro.End:]...)
	}
	expandedDocument := sqltemplate.Analyze(expanded, dialect)
	if len(expandedDocument.Diagnostics) > 0 {
		return false
	}
	expandedSQL, ok := expandProjectionWildcards(string(expandedDocument.SQL), dialect, schemas)
	return ok && comparableSQL(expandedSQL) == comparableSQL(generated)
}

func expandProjectionWildcards(source, dialect string, schemas map[string]schemaTable) (string, bool) {
	document := sqltemplate.Analyze([]byte(source), dialect)
	if len(document.Diagnostics) > 0 {
		return "", false
	}
	expanded := []byte(source)
	for index := len(document.Wildcards) - 1; index >= 0; index-- {
		wildcard := document.Wildcards[index]
		_, columns, ok := schemaForWildcard(schemas, source, wildcard.Qualifier, wildcard.Start, dialect)
		if !ok {
			return "", false
		}
		values := make([]string, 0, len(columns))
		for _, column := range columns {
			if wildcard.Qualifier != "" {
				values = append(values, wildcard.Qualifier+"."+column)
			} else {
				values = append(values, column)
			}
		}
		replacement := []byte(strings.Join(values, ", "))
		expanded = append(append(append([]byte(nil), expanded[:wildcard.Start]...), replacement...), expanded[wildcard.End:]...)
	}
	return string(expanded), true
}

func schemaForWildcard(schemas map[string]schemaTable, statement, qualifier string, position int, dialect string) (string, []string, bool) {
	if qualifier != "" {
		return schemaForEmbed(schemas, statement, qualifier, dialect)
	}
	selector, ok := wildcardSource(statement, position, dialect)
	if !ok {
		return "", nil, false
	}
	return schemaColumns(schemas, selector, dialect)
}

type sqlScopeToken struct {
	text       string
	start      int
	depth      int
	identifier bool
}

var sqlIdentifierTokenPattern = regexp.MustCompile(`^` + sqlQualifiedIdentifierPattern)

func wildcardSource(statement string, position int, dialect string) (string, bool) {
	tokens := sqlScopeTokens(statement)
	wildcard := -1
	for index, token := range tokens {
		if token.start == position && token.text == "*" {
			wildcard = index
			break
		}
	}
	if wildcard < 0 {
		return "", false
	}
	depth := tokens[wildcard].depth
	projection := ""
	projectionIndex := -1
	for index := wildcard - 1; index >= 0; index-- {
		if tokens[index].depth != depth {
			continue
		}
		word := strings.ToLower(tokens[index].text)
		if word == "returning" || word == "select" {
			projection, projectionIndex = word, index
			break
		}
		if tokens[index].text == ";" {
			break
		}
	}
	if projection == "returning" {
		return dmlTarget(tokens, projectionIndex, depth, dialect)
	}
	if projection != "select" {
		return "", false
	}
	for index := wildcard + 1; index < len(tokens); index++ {
		token := tokens[index]
		if token.depth != depth {
			continue
		}
		word := strings.ToLower(token.text)
		if word == "from" {
			if index+1 >= len(tokens) || tokens[index+1].depth != depth || !tokens[index+1].identifier {
				return "", false
			}
			if strings.EqualFold(tokens[index+1].text, "only") || strings.EqualFold(tokens[index+1].text, "lateral") {
				return "", false
			}
			if index+2 < len(tokens) && tokens[index+2].depth == depth && tokens[index+2].text == "(" {
				return "", false
			}
			selector, ok := normalizeSQLSelector(tokens[index+1].text, dialect)
			if !ok {
				return "", false
			}
			for cursor := index + 2; cursor < len(tokens); cursor++ {
				if tokens[cursor].depth != depth {
					continue
				}
				boundary := strings.ToLower(tokens[cursor].text)
				if boundary == "," || boundary == "join" {
					return "", false
				}
				if isSelectBoundary(boundary) {
					break
				}
			}
			return selector, true
		}
		if isSelectBoundary(word) || token.text == ";" {
			break
		}
	}
	return "", false
}

func dmlTarget(tokens []sqlScopeToken, returning, depth int, dialect string) (string, bool) {
	for index := returning - 1; index >= 0; index-- {
		if tokens[index].depth != depth {
			continue
		}
		word := strings.ToLower(tokens[index].text)
		var target int
		switch word {
		case "update":
			if index > 0 && tokens[index-1].depth == depth && strings.EqualFold(tokens[index-1].text, "do") {
				continue
			}
			target = index + 1
		case "insert":
			if index+1 >= len(tokens) || !strings.EqualFold(tokens[index+1].text, "into") {
				return "", false
			}
			target = index + 2
		case "delete":
			if index+1 >= len(tokens) || !strings.EqualFold(tokens[index+1].text, "from") {
				return "", false
			}
			target = index + 2
		default:
			if tokens[index].text == ";" {
				return "", false
			}
			continue
		}
		if target >= len(tokens) || tokens[target].depth != depth || !tokens[target].identifier {
			return "", false
		}
		if isDMLTargetKeyword(strings.ToLower(tokens[target].text)) {
			return "", false
		}
		for cursor := target + 1; cursor < returning; cursor++ {
			if tokens[cursor].depth != depth {
				continue
			}
			sourceWord := strings.ToLower(tokens[cursor].text)
			if (word == "update" && sourceWord == "from") || (word == "delete" && sourceWord == "using") {
				return "", false
			}
		}
		return normalizeSQLSelector(tokens[target].text, dialect)
	}
	return "", false
}

func isDMLTargetKeyword(word string) bool {
	switch word {
	case "set", "only", "from", "using", "where", "returning", "into":
		return true
	default:
		return false
	}
}

func isSelectBoundary(word string) bool {
	switch word {
	case "where", "group", "having", "order", "limit", "offset", "union", "intersect", "except", "returning":
		return true
	default:
		return false
	}
}

func sqlScopeTokens(source string) []sqlScopeToken {
	var tokens []sqlScopeToken
	depth := 0
	for offset := 0; offset < len(source); {
		switch {
		case source[offset] == ' ' || source[offset] == '\t' || source[offset] == '\r' || source[offset] == '\n':
			offset++
		case strings.HasPrefix(source[offset:], "--"):
			if end := strings.IndexByte(source[offset+2:], '\n'); end >= 0 {
				offset += end + 2
			} else {
				offset = len(source)
			}
		case strings.HasPrefix(source[offset:], "/*"):
			if end := strings.Index(source[offset+2:], "*/"); end >= 0 {
				offset += end + 4
			} else {
				offset = len(source)
			}
		case source[offset] == '\'':
			offset = sqlStringEnd(source, offset, '\'')
		case source[offset] == '$':
			end := offset + 1
			for end < len(source) && (source[end] == '_' || source[end] >= 'A' && source[end] <= 'Z' || source[end] >= 'a' && source[end] <= 'z' || source[end] >= '0' && source[end] <= '9') {
				end++
			}
			if end < len(source) && source[end] == '$' {
				tag := source[offset : end+1]
				if close := strings.Index(source[end+1:], tag); close >= 0 {
					offset = end + 1 + close + len(tag)
					continue
				}
			}
			tokens = append(tokens, sqlScopeToken{text: "$", start: offset, depth: depth})
			offset++
		case source[offset] == '(':
			tokens = append(tokens, sqlScopeToken{text: "(", start: offset, depth: depth})
			depth++
			offset++
		case source[offset] == ')':
			if depth > 0 {
				depth--
			}
			tokens = append(tokens, sqlScopeToken{text: ")", start: offset, depth: depth})
			offset++
		default:
			match := sqlIdentifierTokenPattern.FindString(source[offset:])
			if match != "" {
				tokens = append(tokens, sqlScopeToken{text: match, start: offset, depth: depth, identifier: true})
				offset += len(match)
				continue
			}
			tokens = append(tokens, sqlScopeToken{text: source[offset : offset+1], start: offset, depth: depth})
			offset++
		}
	}
	return tokens
}

func sqlStringEnd(source string, start int, quote byte) int {
	for offset := start + 1; offset < len(source); offset++ {
		if source[offset] != quote {
			continue
		}
		if offset+1 < len(source) && source[offset+1] == quote {
			offset++
			continue
		}
		return offset + 1
	}
	return len(source)
}

func schemaForEmbed(schemas map[string]schemaTable, statement, argument, dialect string) (string, []string, bool) {
	tables := make([]string, 0, len(schemas))
	for table := range schemas {
		tables = append(tables, table)
	}
	resolved, ok := ResolveTableArgument(argument, statement, tables, dialect)
	if !ok {
		return "", nil, false
	}
	return schemaColumns(schemas, resolved, dialect)
}

// ResolveTableArgument maps a table name or statement alias to one unique
// parser-normalized table without guessing across alias/name collisions.
func ResolveTableArgument(argument, statement string, tables []string, dialect string) (string, bool) {
	normalizedArgument, ok := normalizeSQLSelector(argument, dialect)
	if !ok {
		return "", false
	}
	matchTable := func(selector string) (string, bool) {
		selectorSchema, selectorName := schemaTableIdentity(selector, dialect)
		matched := ""
		for _, table := range tables {
			tableSchema, tableName := schemaTableIdentity(table, dialect)
			if !identifiersEqual(selectorSchema, tableSchema, dialect) || !identifiersEqual(selectorName, tableName, dialect) {
				continue
			}
			if matched != "" && matched != table {
				return "", false
			}
			matched = table
		}
		return matched, matched != ""
	}
	pattern := regexp.MustCompile(`(?i)(?:\bfrom|\bjoin)\s+(` + sqlQualifiedIdentifierPattern + `)(?:\s+as)?\s+` + regexp.QuoteMeta(argument) + `\b`)
	aliasMatch := ""
	for _, match := range pattern.FindAllStringSubmatch(statement, -1) {
		declared, ok := normalizeSQLSelector(match[1], dialect)
		if !ok {
			return "", false
		}
		table, ok := matchTable(declared)
		if !ok {
			return "", false
		}
		if aliasMatch != "" && aliasMatch != table {
			return "", false
		}
		aliasMatch = table
	}
	bareMatch, hasBareMatch := matchTable(normalizedArgument)
	if aliasMatch != "" {
		if hasBareMatch && bareMatch != aliasMatch {
			return "", false
		}
		return aliasMatch, true
	}
	return bareMatch, hasBareMatch
}

const sqlBareIdentifier = `(?:[A-Za-z_]|[\x{80}-\x{10FFFF}])(?:[A-Za-z0-9_$]|[\x{80}-\x{10FFFF}])*`
const sqlIdentifierPart = `(?:"(?:[^"]|"")*"|` + "`(?:[^`]|``)*`" + `|\[[^\]]+\]|` + sqlBareIdentifier + `)`
const sqlQualifiedIdentifierPattern = sqlIdentifierPart + `(?:\.` + sqlIdentifierPart + `){0,2}`

func normalizeSQLSelector(selector, dialect string) (string, bool) {
	var parts []string
	for offset := 0; offset < len(selector); {
		start := offset
		quoted := false
		quote := byte(0)
		close := byte(0)
		switch selector[offset] {
		case '"', '`':
			quoted = true
			quote, close = selector[offset], selector[offset]
			offset++
		case '[':
			quoted = true
			quote, close = '[', ']'
			offset++
		}
		var part strings.Builder
		if quoted {
			closed := false
			for offset < len(selector) {
				if selector[offset] == close {
					if close != ']' && offset+1 < len(selector) && selector[offset+1] == close {
						part.WriteByte(close)
						offset += 2
						continue
					}
					offset++
					closed = true
					break
				}
				part.WriteByte(selector[offset])
				offset++
			}
			if !closed || dialect == "postgres" && quote != '"' {
				return "", false
			}
		} else {
			for offset < len(selector) && selector[offset] != '.' {
				part.WriteByte(selector[offset])
				offset++
			}
			if part.Len() == 0 || offset == start {
				return "", false
			}
		}
		value := part.String()
		if dialect == "postgres" && !quoted {
			value = lowerASCII(value)
		}
		parts = append(parts, value)
		if offset == len(selector) {
			break
		}
		if selector[offset] != '.' || offset+1 == len(selector) {
			return "", false
		}
		offset++
	}
	return strings.Join(parts, "."), len(parts) > 0
}

func lowerASCII(value string) string {
	result := []byte(value)
	for index, char := range result {
		if char >= 'A' && char <= 'Z' {
			result[index] = char + ('a' - 'A')
		}
	}
	return string(result)
}

func comparableSQL(source string) string {
	var tokens []string
	for i := 0; i < len(source); {
		switch {
		case i+1 < len(source) && source[i:i+2] == "--":
			if end := strings.IndexByte(source[i+2:], '\n'); end >= 0 {
				i += end + 2
			} else {
				i = len(source)
			}
		case i+1 < len(source) && source[i:i+2] == "/*":
			if end := strings.Index(source[i+2:], "*/"); end >= 0 {
				i += end + 4
			} else {
				i = len(source)
			}
		case source[i] == '\'' || source[i] == '"' || source[i] == '`':
			end := quotedEnd(source, i, source[i])
			tokens = append(tokens, source[i:end])
			i = end
		case source[i] == '$' && i+1 < len(source) && source[i+1] >= '0' && source[i+1] <= '9':
			tokens = append(tokens, "?")
			i += 2
			for i < len(source) && source[i] >= '0' && source[i] <= '9' {
				i++
			}
		case source[i] == '?':
			tokens = append(tokens, "?")
			i++
			for i < len(source) && source[i] >= '0' && source[i] <= '9' {
				i++
			}
		case source[i] == ' ' || source[i] == '\t' || source[i] == '\r' || source[i] == '\n' || source[i] == '\f':
			i++
		case identifierPart(source[i]):
			start := i
			for i < len(source) && identifierPart(source[i]) {
				i++
			}
			tokens = append(tokens, source[start:i])
		default:
			operator, width := sqlOperator(source[i:])
			tokens = append(tokens, operator)
			i += width
		}
	}
	if len(tokens) > 0 && tokens[len(tokens)-1] == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	return strings.Join(tokens, "\x1f")
}

func sqlOperator(source string) (string, int) {
	for _, operator := range []string{"->>", "#>>", "||", ">=", "<=", "!=", "<>", "::", "->", "#>", ":="} {
		if strings.HasPrefix(source, operator) {
			return operator, len(operator)
		}
	}
	return source[:1], 1
}

func identifierPart(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value >= 0x80
}

func quotedEnd(source string, start int, quote byte) int {
	for i := start + 1; i < len(source); i++ {
		if source[i] != quote {
			continue
		}
		if i+1 < len(source) && source[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(source)
}

func addDiagnostic(config *Config, filePath string, line int, message string) {
	config.Diagnostics = append(config.Diagnostics, graph.Diagnostic{Path: filePath, Line: line, Level: "warning", Message: message})
}

func queryKey(filePath, name string) string { return clean(filePath) + "\x00" + strings.ToLower(name) }

func entryQueryKey(entry Entry, filePath, name string) string {
	return entryKey(entry) + "\x00" + queryKey(filePath, name)
}

func entryKey(entry Entry) string { return entry.ConfigPath + "\x00" + strconv.Itoa(entry.Index) }

func clean(value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "./")
	if value == "" {
		return "."
	}
	return path.Clean(value)
}

func cloneConfig(config Config) Config {
	config.Entries = append([]Entry(nil), config.Entries...)
	config.Diagnostics = append([]graph.Diagnostic(nil), config.Diagnostics...)
	return config
}
