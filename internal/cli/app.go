package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

const Version = "0.1.0-dev"

type App struct {
	stdout io.Writer
	stderr io.Writer
}

func New(stdout, stderr io.Writer) *App { return &App{stdout: stdout, stderr: stderr} }

func (a *App) Run(ctx context.Context, arguments []string) int {
	parsed, err := parseArguments(arguments)
	if err != nil {
		a.fail(err)
		return 2
	}
	if parsed.command == "" || parsed.command == "help" || parsed.flags["help"] {
		fmt.Fprint(a.stdout, helpText)
		return 0
	}
	if parsed.command == "version" {
		fmt.Fprintln(a.stdout, "grafo "+Version)
		return 0
	}
	var runErr error
	switch parsed.command {
	case "index":
		runErr = a.index(ctx, parsed)
	case "watch":
		runErr = a.watch(ctx, parsed)
	case "status", "counts":
		runErr = a.status(ctx, parsed)
	case "find":
		runErr = a.find(ctx, parsed)
	case "show":
		runErr = a.show(ctx, parsed)
	case "neighbors", "query":
		runErr = a.neighbors(ctx, parsed, "")
	case "callers":
		runErr = a.neighbors(ctx, parsed, "callers")
	case "callees":
		runErr = a.neighbors(ctx, parsed, "callees")
	case "impact", "blast-radius":
		runErr = a.neighbors(ctx, parsed, "impact")
	case "path":
		runErr = a.path(ctx, parsed)
	default:
		runErr = fmt.Errorf("unknown command %q (run 'grafo help')", parsed.command)
	}
	if runErr != nil {
		a.fail(runErr)
		var ambiguous *query.AmbiguousError
		if errors.As(runErr, &ambiguous) {
			a.printNodes(ambiguous.Candidates)
		}
		return 1
	}
	return 0
}

func registry() *parserapi.Registry {
	return parserapi.NewRegistry(golangparser.New(), typescriptparser.New(), configparser.New())
}

func (a *App) index(ctx context.Context, args parsedArguments) error {
	root, err := optionalPath(args.positionals)
	if err != nil {
		return err
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		return err
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		return err
	}
	defer repository.Close()
	maxSize, err := int64Option(args, "max-file-size", 5<<20)
	if err != nil {
		return err
	}
	report, err := indexer.NewService(repository, registry()).Run(ctx, project, indexer.Options{
		Force: args.flags["force"], MaxFileSize: maxSize,
	})
	if err != nil {
		return err
	}
	return a.printIndexReport(report, args.flags["json"])
}

func (a *App) watch(ctx context.Context, args parsedArguments) error {
	root, err := optionalPath(args.positionals)
	if err != nil {
		return err
	}
	interval := time.Second
	if value := args.values["interval"]; value != "" {
		interval, err = time.ParseDuration(value)
		if err != nil || interval < 100*time.Millisecond {
			return fmt.Errorf("--interval must be a duration of at least 100ms")
		}
	}
	run := func() error {
		project, err := indexer.DiscoverProject(ctx, root)
		if err != nil {
			return err
		}
		repository, err := sqlite.Open(ctx, project.IndexPath)
		if err != nil {
			return err
		}
		report, runErr := indexer.NewService(repository, registry()).Run(ctx, project, indexer.Options{})
		closeErr := repository.Close()
		if runErr != nil {
			return runErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(report.Updated) > 0 || len(report.Removed) > 0 || len(report.Diagnostics) > 0 {
			return a.printIndexReport(report, args.flags["json"])
		}
		return nil
	}
	if err := run(); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := run(); err != nil {
				fmt.Fprintf(a.stderr, "grafo watch: %v\n", err)
			}
		}
	}
}

type statusOutput struct {
	Project   indexer.Project `json:"project"`
	IndexedAt string          `json:"indexed_at"`
	Commit    string          `json:"indexed_commit,omitempty"`
	Counts    graph.Counts    `json:"counts"`
}

func (a *App) status(ctx context.Context, args parsedArguments) error {
	root, err := optionalPath(args.positionals)
	if err != nil {
		return err
	}
	project, repository, err := openExisting(ctx, root)
	if err != nil {
		return err
	}
	defer repository.Close()
	counts, err := repository.Counts(ctx)
	if err != nil {
		return err
	}
	indexedAt, err := repository.Meta(ctx, "indexed_at")
	if err != nil {
		return err
	}
	commit, err := repository.Meta(ctx, "commit")
	if err != nil {
		return err
	}
	output := statusOutput{Project: project, IndexedAt: indexedAt, Commit: commit, Counts: counts}
	if args.flags["json"] {
		return writeJSON(a.stdout, output)
	}
	fmt.Fprintf(a.stdout, "%s · branch %s\n", project.Name, project.Branch)
	fmt.Fprintf(a.stdout, "%d files · %d nodes · %d edges · %d unresolved\n", counts.Files, counts.Nodes, counts.Edges, counts.External)
	fmt.Fprintf(a.stdout, "indexed %s\n", indexedAt)
	return nil
}

func (a *App) find(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) == 0 {
		return fmt.Errorf("usage: grafo find <text>")
	}
	limit, err := intOption(args, "limit", 20)
	if err != nil {
		return err
	}
	_, repository, err := openExisting(ctx, repoPath(args))
	if err != nil {
		return err
	}
	defer repository.Close()
	nodes, err := query.NewService(repository).Find(ctx, strings.Join(args.positionals, " "), limit)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, nodes)
	}
	a.printNodes(nodes)
	return nil
}

func (a *App) show(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo show <symbol-or-id>")
	}
	_, repository, err := openExisting(ctx, repoPath(args))
	if err != nil {
		return err
	}
	defer repository.Close()
	node, err := query.NewService(repository).Resolve(ctx, args.positionals[0])
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, node)
	}
	a.printNodes([]graph.Node{node})
	return nil
}

func (a *App) neighbors(ctx context.Context, args parsedArguments, mode string) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo %s <symbol-or-id>", args.command)
	}
	depthDefault := 1
	direction := query.Direction(args.values["direction"])
	relations := parseRelations(args.values["relation"])
	switch mode {
	case "callers":
		depthDefault, direction = 3, query.Incoming
		relations = []graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy}
	case "callees":
		depthDefault, direction = 3, query.Outgoing
		relations = []graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy}
	case "impact":
		depthDefault, direction = 4, query.Incoming
		relations = []graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy, graph.EdgeImports,
			graph.EdgeExtends, graph.EdgeImplements, graph.EdgeEmbeds, graph.EdgeReferences}
	}
	depth, err := intOption(args, "depth", depthDefault)
	if err != nil {
		return err
	}
	limit, err := intOption(args, "limit", 1000)
	if err != nil {
		return err
	}
	_, repository, err := openExisting(ctx, repoPath(args))
	if err != nil {
		return err
	}
	defer repository.Close()
	result, err := query.NewService(repository).Neighborhood(ctx, args.positionals[0], depth, direction, relations, limit)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	fmt.Fprintf(a.stdout, "%s [%s]\n", result.Root.QualifiedName, result.Root.Kind)
	for _, reached := range result.Nodes {
		if reached.Depth == 0 {
			continue
		}
		fmt.Fprintf(a.stdout, "%s↳ %s [%s] %s\n", strings.Repeat("  ", reached.Depth-1), reached.Node.QualifiedName, reached.Node.Kind, formatLocation(reached.Node.Location))
	}
	fmt.Fprintf(a.stdout, "%d nodes · %d edges", len(result.Nodes), len(result.Edges))
	if result.Truncated {
		fmt.Fprint(a.stdout, " · truncated")
	}
	fmt.Fprintln(a.stdout)
	return nil
}

func (a *App) path(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 2 {
		return fmt.Errorf("usage: grafo path <from> <to>")
	}
	limit, err := intOption(args, "limit", 10000)
	if err != nil {
		return err
	}
	direction := query.Direction(args.values["direction"])
	_, repository, err := openExisting(ctx, repoPath(args))
	if err != nil {
		return err
	}
	defer repository.Close()
	result, err := query.NewService(repository).ShortestPath(ctx, args.positionals[0], args.positionals[1], direction, parseRelations(args.values["relation"]), limit)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for index, node := range result.Nodes {
		if index > 0 {
			fmt.Fprintf(a.stdout, "  --%s-->\n", result.Edges[index-1].Kind)
		}
		fmt.Fprintf(a.stdout, "%s [%s] %s\n", node.QualifiedName, node.Kind, formatLocation(node.Location))
	}
	return nil
}

func openExisting(ctx context.Context, root string) (indexer.Project, graph.Repository, error) {
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		return project, nil, err
	}
	if _, err := os.Stat(project.IndexPath); errors.Is(err, os.ErrNotExist) {
		return project, nil, fmt.Errorf("branch %q has no index; run 'grafo index %s'", project.Branch, project.Root)
	} else if err != nil {
		return project, nil, err
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	return project, repository, err
}

func (a *App) printIndexReport(report indexer.Report, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, report)
	}
	fmt.Fprintf(a.stdout, "indexed %s · branch %s\n", report.Project.Name, report.Project.Branch)
	fmt.Fprintf(a.stdout, "%d updated · %d unchanged · %d removed · %d skipped\n", len(report.Updated), report.Unchanged, len(report.Removed), len(report.Skipped))
	fmt.Fprintf(a.stdout, "%d files · %d nodes · %d edges · %d unresolved · %dms\n",
		report.Counts.Files, report.Counts.Nodes, report.Counts.Edges, report.Counts.External, report.ElapsedMS)
	for _, diagnostic := range report.Diagnostics {
		fmt.Fprintf(a.stderr, "%s:%d: %s: %s\n", diagnostic.Path, diagnostic.Line, diagnostic.Level, diagnostic.Message)
	}
	return nil
}

func (a *App) printNodes(nodes []graph.Node) {
	for _, node := range nodes {
		fmt.Fprintf(a.stdout, "%-12s  %-48s  %-24s  %s\n", node.Kind, node.QualifiedName, formatLocation(node.Location), node.ID)
	}
}

func (a *App) fail(err error) { fmt.Fprintf(a.stderr, "grafo: %v\n", err) }

func formatLocation(location graph.Location) string {
	if location.Path == "" {
		return "-"
	}
	if location.Line == 0 {
		return location.Path
	}
	return fmt.Sprintf("%s:%d", location.Path, location.Line)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

type parsedArguments struct {
	command     string
	positionals []string
	flags       map[string]bool
	values      map[string]string
}

var booleanOptions = map[string]bool{"json": true, "force": true, "help": true}
var valueOptions = map[string]bool{
	"repo": true, "depth": true, "direction": true, "relation": true,
	"limit": true, "interval": true, "max-file-size": true,
}

func parseArguments(arguments []string) (parsedArguments, error) {
	result := parsedArguments{flags: map[string]bool{}, values: map[string]string{}}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if !strings.HasPrefix(argument, "--") {
			if result.command == "" {
				result.command = argument
			} else {
				result.positionals = append(result.positionals, argument)
			}
			continue
		}
		nameValue := strings.TrimPrefix(argument, "--")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if booleanOptions[name] {
			if hasValue {
				return result, fmt.Errorf("--%s does not take a value", name)
			}
			result.flags[name] = true
			continue
		}
		if !valueOptions[name] {
			return result, fmt.Errorf("unknown option --%s", name)
		}
		if !hasValue {
			index++
			if index >= len(arguments) {
				return result, fmt.Errorf("--%s requires a value", name)
			}
			value = arguments[index]
		}
		result.values[name] = value
	}
	return result, nil
}

func optionalPath(positionals []string) (string, error) {
	if len(positionals) > 1 {
		return "", fmt.Errorf("expected at most one repository path")
	}
	if len(positionals) == 1 {
		return positionals[0], nil
	}
	return ".", nil
}

func repoPath(args parsedArguments) string {
	if value := args.values["repo"]; value != "" {
		return value
	}
	return "."
}

func intOption(args parsedArguments, name string, fallback int) (int, error) {
	value := args.values[name]
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("--%s must be a positive integer", name)
	}
	return parsed, nil
}

func int64Option(args parsedArguments, name string, fallback int64) (int64, error) {
	value := args.values[name]
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("--%s must be a positive integer", name)
	}
	return parsed, nil
}

func parseRelations(raw string) []graph.EdgeKind {
	if raw == "" {
		return nil
	}
	var result []graph.EdgeKind
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, graph.EdgeKind(value))
		}
	}
	return result
}

const helpText = `Grafo builds a deterministic semantic graph of a repository.

Usage:
  grafo index [path] [--force] [--json]
  grafo watch [path] [--interval 1s]
  grafo status [path] [--json]
  grafo find <text> [--limit 20] [--repo path] [--json]
  grafo show <symbol-or-id> [--repo path] [--json]
  grafo neighbors <symbol-or-id> [--depth 1] [--direction both]
  grafo callers <symbol-or-id> [--depth 3]
  grafo callees <symbol-or-id> [--depth 3]
  grafo impact <symbol-or-id> [--depth 4]
  grafo path <from> <to> [--direction outgoing] [--relation calls,...]
  grafo version

Options may appear before or after positional arguments. Query commands refuse
to use another branch's index; run grafo index after switching branches.
`
