// Package sql routes SQL source files to an explicitly selected dialect.
package sql

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
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
}

func New(dialects ...Dialect) *Router {
	router := &Router{dialects: map[string]Dialect{}, ext: map[string][]Dialect{}}
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
	dialect, err := r.selectDialect(ctx, input, config)
	if err != nil {
		return emptyResult(input), err
	}
	result, err := dialect.Parse(ctx, input)
	decorateDialect(&result, normalizeName(dialect.Name()))
	return result, err
}

// SemanticKey makes repository configuration part of the incremental file
// hash. Changing grafo.yaml therefore reparses unchanged SQL files that may be
// routed to a different dialect.
func (*Router) SemanticKey(_ context.Context, input parserapi.Input) (string, error) {
	config, err := loadConfiguration(input.Root)
	if err != nil {
		return "", err
	}
	return config.semanticKey, nil
}

func (r *Router) selectDialect(ctx context.Context, input parserapi.Input, config configuration) (Dialect, error) {
	configured, err := config.dialectForPath(input.Path)
	if err != nil {
		return nil, fmt.Errorf("select SQL dialect for %s: %w", input.Path, err)
	}
	if configured != "" {
		return r.namedDialect(configured, "path mapping", input.Path)
	}
	extension := strings.ToLower(filepath.Ext(input.Path))
	if extension != ".sql" {
		candidates := r.ext[extension]
		switch len(candidates) {
		case 0:
			return nil, fmt.Errorf("select SQL dialect for %s: no dialect owns extension %q", input.Path, extension)
		case 1:
			return candidates[0], nil
		default:
			return nil, fmt.Errorf("select SQL dialect for %s: extension %q is claimed by %s", input.Path, extension, dialectNames(candidates))
		}
	}
	if config.defaultDialect != "" {
		return r.namedDialect(config.defaultDialect, "default_dialect", input.Path)
	}
	var accepted []Dialect
	var rejected []string
	for _, dialect := range r.ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
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
			return nil, fmt.Errorf("select SQL dialect for %s: no SQL dialects are installed", input.Path)
		}
		return nil, fmt.Errorf("select SQL dialect for %s: no dialect accepted the syntax (%s)", input.Path, strings.Join(rejected, "; "))
	case 1:
		return accepted[0], nil
	default:
		return nil, fmt.Errorf("select SQL dialect for %s: syntax is ambiguous between %s; set sql.default_dialect or sql.paths in %s", input.Path, dialectNames(accepted), configFileName)
	}
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
