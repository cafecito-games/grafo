// Package sql routes SQL source files to an explicitly selected dialect.
package sql

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/sqlcbinding"
	"github.com/cafecito-games/grafo/internal/parser/sqltemplate"
)

// Dialect is the extension point for a SQL grammar and graph extractor.
// Extensions returns dialect-specific extensions; the router always owns .sql.
type Dialect interface {
	Name() string
	Extensions() []string
	Probe(context.Context, parserapi.Input) error
	Parse(context.Context, parserapi.Input) (graph.ParseResult, error)
}

// Router is the sole top-level parser for SQL-like files. Dialects are sorted
// by name so probing and diagnostics never depend on registration order.
type Router struct {
	dialects map[string]Dialect
	ordered  []Dialect
	ext      map[string][]Dialect
	initErrs []string
	bindings *sqlcbinding.Loader
}

func New(dialects ...Dialect) *Router {
	return NewWithBindingLoader(sqlcbinding.NewLoader(), dialects...)
}

func NewWithBindingLoader(loader *sqlcbinding.Loader, dialects ...Dialect) *Router {
	if loader == nil {
		loader = sqlcbinding.NewLoader()
	}
	router := &Router{dialects: map[string]Dialect{}, ext: map[string][]Dialect{}, bindings: loader}
	for _, dialect := range dialects {
		if dialect == nil {
			router.initErrs = append(router.initErrs, "nil dialect")
			continue
		}
		name := normalizeName(dialect.Name())
		if name == "" {
			router.initErrs = append(router.initErrs, "empty dialect name")
			continue
		}
		if _, exists := router.dialects[name]; exists {
			router.initErrs = append(router.initErrs, fmt.Sprintf("duplicate dialect name %q", name))
			continue
		}
		router.dialects[name] = dialect
		router.ordered = append(router.ordered, dialect)
		for _, extension := range dialect.Extensions() {
			extension = normalizeExtension(extension)
			if extension == ".sql" {
				router.initErrs = append(router.initErrs, fmt.Sprintf("dialect %q claims .sql, which belongs to the router", name))
				continue
			}
			if extension == "" {
				router.initErrs = append(router.initErrs, fmt.Sprintf("dialect %q has an empty extension", name))
				continue
			}
			router.ext[extension] = append(router.ext[extension], dialect)
		}
	}
	sort.Slice(router.ordered, func(i, j int) bool {
		return normalizeName(router.ordered[i].Name()) < normalizeName(router.ordered[j].Name())
	})
	for extension := range router.ext {
		sort.Slice(router.ext[extension], func(i, j int) bool {
			return normalizeName(router.ext[extension][i].Name()) < normalizeName(router.ext[extension][j].Name())
		})
	}
	sort.Strings(router.initErrs)
	return router
}

func (*Router) Language() string { return "sql" }

func (*Router) SemanticDependencies() []string { return []string{configFileName} }

func (*Router) IsSemanticInput(path string) bool { return sqlcbinding.IsSemanticInput(path) }

func (r *Router) Supports(filePath string) bool {
	extension := strings.ToLower(filepath.Ext(filePath))
	return extension == ".sql" || len(r.ext[extension]) > 0
}

func (r *Router) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	if len(r.initErrs) > 0 {
		return emptyResult(input), fmt.Errorf("register SQL dialects: %s", strings.Join(r.initErrs, "; "))
	}
	config, err := loadConfiguration(input.Root)
	if err != nil {
		return emptyResult(input), err
	}
	registry, err := r.bindings.Load(ctx, input)
	if err != nil {
		return emptyResult(input), fmt.Errorf("load sqlc binding registry: %w", err)
	}
	sqlcEngine, sqlcConflict := configuredSQLCEngine(registry.EntriesForSQLPath(input.Path))
	dialect, selection, err := r.selectDialect(ctx, input, config, sqlcEngine)
	if err != nil {
		return emptyResult(input), err
	}
	result, err := dialect.Parse(ctx, input)
	selected := normalizeName(dialect.Name())
	decorateDialect(&result, selected)
	bindingsAllowed := !sqlcConflict
	if sqlcConflict {
		result.Diagnostics = append(result.Diagnostics, graph.Diagnostic{Path: input.Path, Level: "warning", Message: "conflicting sqlc engines own this query file; sqlc bindings disabled"})
	}
	if sqlcEngine != "" && selected != sqlcEngine {
		bindingsAllowed = false
		result.Diagnostics = append(result.Diagnostics, graph.Diagnostic{Path: input.Path, Level: "warning",
			Message: fmt.Sprintf("Grafo SQL %s selected %s but sqlc config selects %s; sqlc bindings disabled", selection, selected, sqlcEngine)})
	}
	allowSQLCSchema := bindingsAllowed && sqlcEngine != "" && selected == sqlcEngine
	expandSQLCWildcards(&result, input, registry, selected, allowSQLCSchema)
	if bindingsAllowed && selected == sqlcEngine {
		addGeneratedBindings(&result, input, registry)
	}
	return result, err
}

func expandSQLCWildcards(result *graph.ParseResult, input parserapi.Input, registry sqlcbinding.Registry, dialect string, allowSchema bool) {
	document := sqltemplate.Analyze(input.Content, dialect)
	if len(document.Diagnostics) > 0 {
		return
	}
	nodes := map[string]graph.Node{}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindSQLQuery {
			nodes[node.QualifiedName] = node
		}
	}
	tablesByQuery := map[string][]string{}
	for _, fact := range result.Facts {
		if (fact.Kind != graph.EdgeReads && fact.Kind != graph.EdgeWrites) || fact.Target == "" || fact.TargetKind == graph.KindColumn {
			continue
		}
		if node, ok := nodeByID(result.Nodes, fact.FromID); ok && node.Kind == graph.KindSQLQuery {
			tablesByQuery[node.ID] = appendUnique(tablesByQuery[node.ID], fact.Target)
		}
	}
	seen := map[string]bool{}
	for _, fact := range result.Facts {
		seen[fact.FromID+"\x00"+string(fact.Kind)+"\x00"+fact.Target] = true
	}
	ordinal := len(result.Facts)
	for _, query := range document.Queries {
		node, ok := nodes[query.QualifiedName(input.Path)]
		if !ok {
			continue
		}
		statement := string(input.Content[query.StatementStart:query.StatementEnd])
		emitColumns := func(table, form, argument string, line, column int) bool {
			if !allowSchema {
				return false
			}
			qualifiedTable, columns, ok := registry.SchemaColumnsForQuery(input.Path, table)
			if !ok {
				return false
			}
			for _, columnName := range columns {
				target := qualifiedTable + "." + columnName
				key := node.ID + "\x00" + string(graph.EdgeReads) + "\x00" + target
				if seen[key] {
					continue
				}
				seen[key] = true
				ordinal++
				location := graph.Location{Path: input.Path, Line: line, Column: column, EndLine: line}
				result.Facts = append(result.Facts, graph.Fact{
					ID:     graph.FactID(input.Path, "id:"+node.ID, graph.EdgeReads, target, line, ordinal),
					FromID: node.ID, Kind: graph.EdgeReads, Producer: "sqlcbinding", Target: target, TargetKind: graph.KindColumn,
					Location: location, Properties: map[string]string{"form": form, "argument": argument}, OwnerFile: input.Path,
				})
			}
			return true
		}
		for _, macro := range document.Macros {
			if !macro.Embed || macro.Start < query.StatementStart || macro.Start >= query.StatementEnd {
				continue
			}
			line, column := document.Position(macro.Start)
			if node.Properties["qualified_wildcard_safe"] != "true" {
				result.Diagnostics = append(result.Diagnostics, graph.Diagnostic{Path: input.Path, Line: line, Level: "warning",
					Message: fmt.Sprintf("sqlc.embed(%s) column expansion is ambiguous in this statement", macro.Argument)})
				continue
			}
			table, ok := resolveEmbedTable(macro.Argument, statement, tablesByQuery[node.ID])
			if !ok {
				result.Diagnostics = append(result.Diagnostics, graph.Diagnostic{Path: input.Path, Line: line, Level: "warning",
					Message: fmt.Sprintf("sqlc.embed(%s) column expansion is ambiguous in this statement", macro.Argument)})
				continue
			}
			if !emitColumns(table, "sqlc.embed", macro.Argument, line, column) {
				result.Diagnostics = append(result.Diagnostics, graph.Diagnostic{Path: input.Path, Line: line, Level: "warning",
					Message: fmt.Sprintf("sqlc.embed(%s) column expansion lacks complete, unique configured schema evidence", macro.Argument)})
			}
		}
		for _, wildcard := range document.Wildcards {
			if wildcard.Start < query.StatementStart || wildcard.Start >= query.StatementEnd {
				continue
			}
			line, column := document.Position(wildcard.Start)
			table := ""
			ok := false
			if wildcard.Qualifier != "" {
				if node.Properties["qualified_wildcard_safe"] == "true" {
					table, ok = resolveEmbedTable(wildcard.Qualifier, statement, tablesByQuery[node.ID])
				}
			} else if node.Properties["unqualified_wildcard_safe"] == "true" && len(tablesByQuery[node.ID]) == 1 {
				table, ok = tablesByQuery[node.ID][0], true
			}
			if !ok || !emitColumns(table, "wildcard", wildcard.Qualifier, line, column) {
				result.Diagnostics = append(result.Diagnostics, graph.Diagnostic{Path: input.Path, Line: line, Level: "warning",
					Message: "SQL query wildcard column usage is unresolved without complete, unique configured schema evidence"})
			}
		}
	}
	sort.Slice(result.Facts, func(i, j int) bool { return result.Facts[i].ID < result.Facts[j].ID })
}

func nodeByID(nodes []graph.Node, id string) (graph.Node, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return graph.Node{}, false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func resolveEmbedTable(argument, statement string, tables []string) (string, bool) {
	var matched string
	pattern := regexp.MustCompile(`(?i)(?:\bfrom|\bjoin)\s+([A-Za-z0-9_."` + "`" + `]+)(?:\s+as)?\s+` + regexp.QuoteMeta(argument) + `\b`)
	for _, match := range pattern.FindAllStringSubmatch(statement, -1) {
		declared := strings.Trim(match[1], "\"`")
		for _, table := range tables {
			if declared != table && graph.SimpleName(declared) != graph.SimpleName(table) {
				continue
			}
			if matched != "" && matched != table {
				return "", false
			}
			matched = table
		}
	}
	if matched != "" {
		for _, table := range tables {
			if (argument == table || argument == graph.SimpleName(table)) && table != matched {
				return "", false
			}
		}
		return matched, true
	}
	for _, table := range tables {
		if argument == table || argument == graph.SimpleName(table) {
			if matched != "" && matched != table {
				return "", false
			}
			matched = table
		}
	}
	return matched, matched != ""
}

func (r *Router) WorkspaceSemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	return r.bindings.SemanticKey(ctx, input)
}

func (*Router) WorkspaceSemanticEvidenceKey(context.Context, parserapi.Input) (string, error) {
	return "sqlc-binding-repository-inputs-v1", nil
}

func (r *Router) SemanticKey(ctx context.Context, input parserapi.Input) (string, error) {
	config, err := loadConfiguration(input.Root)
	if err != nil {
		return "", err
	}
	workspace := input.SemanticKey
	if workspace == "" {
		workspace, err = r.bindings.SemanticKey(ctx, input)
		if err != nil {
			return "", err
		}
	}
	fileBindingKey, err := r.bindings.FileSemanticKey(ctx, input)
	if err != nil {
		return "", err
	}
	return workspace + ":" + fileBindingKey + ":" + config.semanticKey, nil
}

func (r *Router) SemanticAffectedPaths(root string, allPaths, changedPaths []string) []string {
	for _, changed := range changedPaths {
		if sqlcbinding.IsConfigPath(changed) {
			var result []string
			for _, candidate := range allPaths {
				if _, supported := rDialectExtension(candidate); supported {
					result = append(result, candidate)
				}
			}
			return result
		}
	}
	queries, _, known := r.bindings.AffectedPaths(root, changedPaths)
	if known {
		return queries
	}
	for _, changed := range changedPaths {
		if !sqlcbinding.IsBindingInput(changed) {
			continue
		}
		var result []string
		for _, candidate := range allPaths {
			if _, supported := rDialectExtension(candidate); supported {
				result = append(result, candidate)
			}
		}
		return result
	}
	return nil
}

func (r *Router) selectDialect(ctx context.Context, input parserapi.Input, config configuration, sqlcEngine string) (Dialect, string, error) {
	configured, err := config.dialectForPath(input.Path)
	if err != nil {
		return nil, "", fmt.Errorf("select SQL dialect for %s: %w", input.Path, err)
	}
	if configured != "" {
		dialect, err := r.namedDialect(configured, "path mapping", input.Path)
		return dialect, "path mapping", err
	}
	extension := strings.ToLower(filepath.Ext(input.Path))
	if extension != ".sql" {
		candidates := r.ext[extension]
		switch len(candidates) {
		case 0:
			return nil, "", fmt.Errorf("select SQL dialect for %s: no dialect owns extension %q", input.Path, extension)
		case 1:
			return candidates[0], "extension", nil
		default:
			return nil, "", fmt.Errorf("select SQL dialect for %s: extension %q is claimed by %s", input.Path, extension, dialectNames(candidates))
		}
	}
	if config.defaultDialect != "" {
		dialect, err := r.namedDialect(config.defaultDialect, "default_dialect", input.Path)
		return dialect, "default_dialect", err
	}
	if sqlcEngine != "" {
		dialect, err := r.namedDialect(sqlcEngine, "sqlc engine", input.Path)
		return dialect, "sqlc engine", err
	}
	var accepted []Dialect
	var rejected []string
	for _, dialect := range r.ordered {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if err := dialect.Probe(ctx, input); err == nil {
			accepted = append(accepted, dialect)
		} else {
			rejected = append(rejected, fmt.Sprintf("%s: %v", normalizeName(dialect.Name()), err))
		}
	}
	switch len(accepted) {
	case 0:
		if len(rejected) == 0 {
			return nil, "", fmt.Errorf("select SQL dialect for %s: no SQL dialects are installed", input.Path)
		}
		return nil, "", fmt.Errorf("select SQL dialect for %s: no dialect accepted the syntax (%s)", input.Path, strings.Join(rejected, "; "))
	case 1:
		return accepted[0], "grammar probing", nil
	default:
		return nil, "", fmt.Errorf("select SQL dialect for %s: syntax is ambiguous between %s; set sql.default_dialect or sql.paths in %s", input.Path, dialectNames(accepted), configFileName)
	}
}

func configuredSQLCEngine(entries []sqlcbinding.Entry) (string, bool) {
	engines := map[string]bool{}
	for _, entry := range entries {
		if entry.Valid && entry.Engine != "" {
			engines[entry.Engine] = true
		}
	}
	if len(engines) != 1 {
		return "", len(engines) > 1
	}
	for engine := range engines {
		return engine, false
	}
	return "", false
}

func addGeneratedBindings(result *graph.ParseResult, input parserapi.Input, registry sqlcbinding.Registry) {
	ordinal := 0
	for _, node := range result.Nodes {
		if node.Kind != graph.KindSQLQuery {
			continue
		}
		for _, binding := range registry.BindingsForQuery(input.Path, node.Name) {
			ordinal++
			properties := map[string]string{
				"generator": binding.Generator, "generator_version": binding.GeneratorVersion,
				"adapter_version": sqlcbinding.AdapterVersion, "config_path": binding.ConfigPath,
				"config_entry": fmt.Sprint(binding.Entry), "generated_path": binding.GeneratedPath,
				"query_constant": binding.Constant,
			}
			result.Facts = append(result.Facts, graph.Fact{
				ID:     graph.FactID(input.Path, "name:"+binding.Method, graph.EdgeGeneratedFrom, node.ID, binding.Location.Line, ordinal),
				Source: binding.Method, SourceKind: graph.KindMethod, Kind: graph.EdgeGeneratedFrom,
				Producer: "sqlcbinding", TargetID: node.ID, Location: binding.Location,
				Properties: properties, OwnerFile: input.Path,
			})
		}
	}
	sort.Slice(result.Facts, func(i, j int) bool { return result.Facts[i].ID < result.Facts[j].ID })
}

func rDialectExtension(filePath string) (string, bool) {
	extension := strings.ToLower(filepath.Ext(filePath))
	return extension, extension == ".sql" || extension == ".psql" || extension == ".pgsql"
}

func (r *Router) namedDialect(name, source, filePath string) (Dialect, error) {
	if dialect, exists := r.dialects[normalizeName(name)]; exists {
		return dialect, nil
	}
	return nil, fmt.Errorf("select SQL dialect for %s: %s names unavailable dialect %q (installed: %s)", filePath, source, name, dialectNames(r.ordered))
}

func decorateDialect(result *graph.ParseResult, dialect string) {
	for index := range result.Nodes {
		properties := make(map[string]string, len(result.Nodes[index].Properties)+1)
		for key, value := range result.Nodes[index].Properties {
			properties[key] = value
		}
		properties["dialect"] = dialect
		result.Nodes[index].Properties = properties
	}
}

func emptyResult(input parserapi.Input) graph.ParseResult {
	builder := parserapi.NewBuilder(input, "sql")
	return builder.Finish()
}

func normalizeExtension(extension string) string {
	extension = strings.ToLower(strings.TrimSpace(extension))
	if extension != "" && !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}
	return extension
}

func dialectNames(dialects []Dialect) string {
	if len(dialects) == 0 {
		return "none"
	}
	names := make([]string, 0, len(dialects))
	for _, dialect := range dialects {
		names = append(names, normalizeName(dialect.Name()))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
