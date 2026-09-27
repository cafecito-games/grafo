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

	"github.com/cafecito-games/grafo/internal/agentinstall"
	"github.com/cafecito-games/grafo/internal/embedding/ollama"
	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/semantic"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/version"
)

const Version = version.Value

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
	case "install":
		runErr = a.install(ctx, parsed)
	case "index":
		runErr = a.index(ctx, parsed)
	case "watch":
		runErr = a.watch(ctx, parsed)
	case "status", "counts":
		runErr = a.status(ctx, parsed)
	case "mcp":
		runErr = a.mcp(ctx, parsed)
	case "embed":
		runErr = a.embed(ctx, parsed)
	case "reusable", "find-reusable-code":
		runErr = a.reusable(ctx, parsed)
	case "find":
		runErr = a.find(ctx, parsed)
	case "show":
		runErr = a.show(ctx, parsed)
	case "source":
		runErr = a.source(ctx, parsed)
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
	case "data-resources":
		runErr = a.dataResources(ctx, parsed)
	case "data-usage":
		runErr = a.dataResourceUsage(ctx, parsed)
	case "config-keys":
		runErr = a.configKeys(ctx, parsed)
	case "events":
		runErr = a.events(ctx, parsed)
	case "orphaned-events":
		runErr = a.orphanedEvents(ctx, parsed)
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

func (a *App) install(ctx context.Context, args parsedArguments) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate grafo executable: %w", err)
	}
	if strings.Contains(executable, string(os.PathSeparator)+"go-build") {
		return fmt.Errorf("cannot install from a temporary 'go run' binary; install grafo with 'go install github.com/cafecito-games/grafo/cmd/grafo@latest' first")
	}

	results, installErr := agentinstall.Install(ctx, agentinstall.ExecRunner{}, executable, args.positionals)
	for _, result := range results {
		fmt.Fprintf(a.stdout, "configured grafo for %s (%s scope)\n", result.Agent, result.Scope)
	}
	if installErr != nil {
		return installErr
	}
	fmt.Fprintln(a.stdout, "run 'grafo index .' once in each repository before using the MCP tools")
	return nil
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
	report, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{
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
		report, runErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{})
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
	Projects  []indexer.Project `json:"projects"`
	IndexedAt string            `json:"indexed_at"`
	Commit    string            `json:"indexed_commit,omitempty"`
	Counts    graph.Counts      `json:"counts"`
}

func (a *App) status(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) > 1 || (len(args.positionals) == 1 && args.values["repos"] != "") {
		return fmt.Errorf("usage: grafo status [path] [--repos pathA,pathB]")
	}
	if len(args.positionals) == 1 {
		args.values["repo"] = args.positionals[0]
	}
	repository, projects, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
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
	output := statusOutput{Projects: projects, IndexedAt: indexedAt, Commit: commit, Counts: counts}
	if args.flags["json"] {
		return writeJSON(a.stdout, output)
	}
	for _, project := range projects {
		fmt.Fprintf(a.stdout, "%s · branch %s\n", project.Name, project.Branch)
	}
	fmt.Fprintf(a.stdout, "%d files · %d nodes · %d edges · %d unresolved\n", counts.Files, counts.Nodes, counts.Edges, counts.External)
	fmt.Fprintf(a.stdout, "indexed %s\n", indexedAt)
	return nil
}

func (a *App) mcp(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo mcp [--repo path | --repos pathA,pathB]")
	}
	repository, projects, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	service := mcpserver.NewFederated(repository, projects).WithRefresh(func(refreshContext context.Context) error {
		return refreshRead(refreshContext, repository, projects)
	})
	semanticService, err := newSemanticService(repository, args)
	if err != nil {
		return err
	}
	service.WithReusable(func(searchContext context.Context, text string, limit int) (semantic.SearchResult, error) {
		if _, err := semanticService.Sync(searchContext); err != nil {
			return semantic.SearchResult{}, err
		}
		return semanticService.Search(searchContext, text, limit)
	})
	sourceService, err := newSourceService(repository, projects)
	if err != nil {
		return err
	}
	service.WithSource(sourceService.Read)
	return service.Run(ctx, Version)
}

func (a *App) embed(ctx context.Context, args parsedArguments) error {
	root, err := optionalPath(args.positionals)
	if err != nil {
		return fmt.Errorf("usage: grafo embed [path] [--model name] [--ollama-url url] [--force]")
	}
	project, repository, err := openExisting(ctx, root)
	if err != nil {
		return err
	}
	defer repository.Close()
	service, err := newSemanticService(repository, args)
	if err != nil {
		return err
	}
	report, err := service.Sync(ctx)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	fmt.Fprintf(a.stdout, "embedded %s · branch %s · model %s\n", project.Name, project.Branch, report.Model)
	fmt.Fprintf(a.stdout, "%d updated · %d unchanged · %d removed · %d candidates\n",
		report.Updated, report.Unchanged, report.Removed, report.Candidates)
	return nil
}

func (a *App) reusable(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) == 0 {
		return fmt.Errorf("usage: grafo reusable <description> [--repo path | --repos pathA,pathB] [--limit 5]")
	}
	limit, err := intOption(args, "limit", 5)
	if err != nil {
		return err
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	service, err := newSemanticService(repository, args)
	if err != nil {
		return err
	}
	if _, err := service.Sync(ctx); err != nil {
		return err
	}
	result, err := service.Search(ctx, strings.Join(args.positionals, " "), limit)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, match := range result.Matches {
		fmt.Fprintf(a.stdout, "%.4f  %-12s  %-48s  %s · %d connected nodes\n",
			match.Score, match.Node.Kind, match.Node.QualifiedName, formatLocation(match.Node.Location), len(match.Context.Nodes)-1)
	}
	return nil
}

func newSemanticService(repository graph.ReadRepository, args parsedArguments) (*semantic.Service, error) {
	semanticRepository, ok := repository.(semantic.Repository)
	if !ok {
		return nil, fmt.Errorf("repository does not support semantic candidate discovery")
	}
	model := firstValue(args.values["model"], os.Getenv("GRAFO_EMBED_MODEL"), ollama.DefaultModel)
	baseURL := firstValue(args.values["ollama-url"], os.Getenv("GRAFO_OLLAMA_URL"), ollama.DefaultURL)
	embedder, err := ollama.New(baseURL, model)
	if err != nil {
		return nil, err
	}
	batchSize, err := intOption(args, "batch-size", 32)
	if err != nil {
		return nil, err
	}
	return semantic.NewService(semanticRepository, repository, embedder).
		WithBatchSize(batchSize).
		WithForce(args.command == "embed" && args.flags["force"]), nil
}

func firstValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (a *App) find(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) == 0 {
		return fmt.Errorf("usage: grafo find <text>")
	}
	limit, err := intOption(args, "limit", 20)
	if err != nil {
		return err
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
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
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
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

func (a *App) source(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo source <symbol-or-id> [--context-lines 2] [--max-lines 200]")
	}
	contextLines, err := nonNegativeIntOption(args, "context-lines", 2)
	if err != nil {
		return err
	}
	maxLines, err := intOption(args, "max-lines", 200)
	if err != nil {
		return err
	}
	repository, projects, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	service, err := newSourceService(repository, projects)
	if err != nil {
		return err
	}
	excerpt, err := service.Read(ctx, args.positionals[0], contextLines, maxLines)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, excerpt)
	}
	fmt.Fprintf(a.stdout, "%s · %s · %s:%d-%d\n", excerpt.Repository, excerpt.Branch, excerpt.Path, excerpt.StartLine, excerpt.EndLine)
	fmt.Fprintln(a.stdout, excerpt.Content)
	if excerpt.Truncated {
		fmt.Fprintln(a.stdout, "… truncated")
	}
	return nil
}

func newSourceService(repository graph.ReadRepository, projects []indexer.Project) (*sourcecontext.Service, error) {
	if locator, ok := repository.(sourcecontext.ProjectLocator); ok {
		return sourcecontext.NewService(repository, locator), nil
	}
	if len(projects) != 1 {
		return nil, fmt.Errorf("cannot locate source repository")
	}
	return sourcecontext.NewService(repository, sourcecontext.NewSingleProjectLocator(repository, projects[0])), nil
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
			graph.EdgeExtends, graph.EdgeImplements, graph.EdgeEmbeds, graph.EdgeReferences,
			graph.EdgeReads, graph.EdgeWrites, graph.EdgeAssigns, graph.EdgeReturns,
			graph.EdgePasses, graph.EdgeRequests, graph.EdgeDependsOn}
	}
	depth, err := intOption(args, "depth", depthDefault)
	if err != nil {
		return err
	}
	limit, err := intOption(args, "limit", 1000)
	if err != nil {
		return err
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
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
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
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

func (a *App) catalogOptions(args parsedArguments) (query.CatalogOptions, error) {
	limit, err := intOption(args, "limit", query.DefaultCatalogLimit)
	if err != nil {
		return query.CatalogOptions{}, err
	}
	return query.CatalogOptions{Repository: args.values["repository"],
		Name: args.values["name"], Limit: limit}, nil
}

func openCatalog(ctx context.Context, args parsedArguments) (*query.Catalog, func() error, error) {
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return nil, nil, err
	}
	catalogRepository, ok := repository.(graph.CatalogRepository)
	if !ok {
		closeRepository()
		return nil, nil, fmt.Errorf("repository does not support catalog queries")
	}
	return query.NewCatalog(catalogRepository), closeRepository, nil
}

func (a *App) dataResources(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo data-resources [--kind table,view] [--name text] [--limit 100]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	kinds, err := parseNodeKinds(args.values["kind"])
	if err != nil {
		return err
	}
	result, err := catalog.DataResources(ctx, kinds, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, resource := range result.Resources {
		a.printResource(resource)
	}
	for _, resource := range result.Unresolved {
		a.printResource(resource)
	}
	a.printCatalogSummary(len(result.Resources), len(result.Unresolved), "resources", result.Truncated)
	return nil
}

func (a *App) dataResourceUsage(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo data-usage <table-view-or-id> [--limit 100]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	result, err := catalog.DataResourceUsage(ctx, args.positionals[0], options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	a.printResource(result.Resource)
	a.printUsage("reader", result.Readers)
	a.printUsage("writer", result.Writers)
	a.printUsage("reference", result.References)
	if result.Truncated {
		fmt.Fprintln(a.stdout, "… truncated")
	}
	return nil
}

func (a *App) configKeys(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo config-keys [--name text] [--limit 100]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	result, err := catalog.ConfigKeys(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, key := range append(append([]query.ConfigKey{}, result.Keys...), result.Unresolved...) {
		a.printResource(key.Resource)
		a.printUsage("definition", key.Definitions)
		a.printUsage("reader", key.Readers)
		a.printUsage("reference", key.References)
	}
	a.printCatalogSummary(len(result.Keys), len(result.Unresolved), "config keys", result.Truncated)
	return nil
}

func (a *App) events(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo events [--name text] [--limit 100]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	result, err := catalog.Events(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, event := range append(append([]query.Event{}, result.Events...), result.Unresolved...) {
		a.printResource(event.Resource)
		a.printUsage("declaration", event.Declarations)
		a.printUsage("producer", event.Producers)
		a.printUsage("consumer", event.Consumers)
		a.printUsage("handler", event.Handlers)
	}
	a.printCatalogSummary(len(result.Events), len(result.Unresolved), "events", result.Truncated)
	return nil
}

func (a *App) orphanedEvents(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo orphaned-events [--name text] [--limit 100]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer closeRepository()
	result, err := catalog.OrphanedEvents(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, orphan := range result.Events {
		fmt.Fprintf(a.stdout, "%-12s  %-10s  %-48s  %s\n", orphan.Category, orphan.Status,
			orphan.Event.QualifiedName, formatLocation(orphan.Event.Location))
		if orphan.UnresolvedProducers > 0 || orphan.UnresolvedConsumers > 0 {
			fmt.Fprintf(a.stdout, "    %d unresolved producers · %d unresolved consumers\n",
				orphan.UnresolvedProducers, orphan.UnresolvedConsumers)
		}
	}
	fmt.Fprintf(a.stdout, "%d events", len(result.Events))
	if result.Truncated {
		fmt.Fprint(a.stdout, " · truncated")
	}
	fmt.Fprintln(a.stdout)
	return nil
}

func (a *App) printResource(resource query.Resource) {
	state := resource.ObjectKind
	if state == "" {
		state = string(resource.Kind)
	}
	if resource.Unresolved {
		state += " (unresolved)"
	}
	fmt.Fprintf(a.stdout, "%-12s  %-20s  %-48s  %s\n", resource.Kind, state,
		resource.QualifiedName, formatLocation(resource.Location))
}

func (a *App) printUsage(label string, sites []query.UsageSite) {
	for _, site := range sites {
		fmt.Fprintf(a.stdout, "    %-12s %-48s %s\n", label, site.Node.QualifiedName, formatLocation(site.Location))
	}
}

func (a *App) printCatalogSummary(declared, unresolved int, noun string, truncated bool) {
	fmt.Fprintf(a.stdout, "%d %s · %d unresolved", declared, noun, unresolved)
	if truncated {
		fmt.Fprint(a.stdout, " · truncated")
	}
	fmt.Fprintln(a.stdout)
}

// parseNodeKinds rejects a --kind value that names no kind. Degrading it to the
// default would answer a malformed request with a full catalog.
func parseNodeKinds(raw string) ([]graph.NodeKind, error) {
	if raw == "" {
		return nil, nil
	}
	var result []graph.NodeKind
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, graph.NodeKind(value))
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("--kind must name at least one node kind")
	}
	return result, nil
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
	if err != nil {
		return project, nil, err
	}
	if _, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{}); err != nil {
		repository.Close()
		return project, nil, fmt.Errorf("refresh index: %w", err)
	}
	return project, repository, nil
}

func openRead(ctx context.Context, args parsedArguments) (graph.ReadRepository, []indexer.Project, func() error, error) {
	if args.values["repo"] != "" && args.values["repos"] != "" {
		return nil, nil, nil, fmt.Errorf("--repo and --repos cannot be used together")
	}
	if raw := args.values["repos"]; raw != "" {
		paths := strings.Split(raw, ",")
		repository, err := federation.Open(ctx, paths)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := repository.Refresh(ctx, parserdefaults.NewRegistry()); err != nil {
			repository.Close()
			return nil, nil, nil, err
		}
		return repository, repository.Projects(), repository.Close, nil
	}
	project, repository, err := openExisting(ctx, repoPath(args))
	if err != nil {
		return nil, nil, nil, err
	}
	return repository, []indexer.Project{project}, repository.Close, nil
}

func refreshRead(ctx context.Context, repository graph.ReadRepository, projects []indexer.Project) error {
	if federated, ok := repository.(*federation.Repository); ok {
		return federated.Refresh(ctx, parserdefaults.NewRegistry())
	}
	indexed, ok := repository.(graph.IndexRepository)
	if !ok || len(projects) != 1 {
		return fmt.Errorf("repository does not support index refresh")
	}
	_, err := indexer.NewService(indexed, parserdefaults.NewRegistry()).Run(ctx, projects[0], indexer.Options{})
	return err
}

func (a *App) printIndexReport(report indexer.Report, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, report)
	}
	fmt.Fprintf(a.stdout, "indexed %s · branch %s\n", report.Project.Name, report.Project.Branch)
	fmt.Fprintf(a.stdout, "%d updated · %d unchanged · %d removed · %d skipped\n", len(report.Updated), report.Unchanged, len(report.Removed), len(report.Skipped))
	fmt.Fprintf(a.stdout, "%d file contents checked\n", report.Checked)
	fmt.Fprintf(a.stdout, "edge reconciliation: %dms\n", report.ReconcileMS)
	if report.Rebuild != "" {
		fmt.Fprintf(a.stdout, "rebuild: %s\n", report.Rebuild)
	}
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
	"repo": true, "repos": true, "depth": true, "direction": true, "relation": true,
	"limit": true, "interval": true, "max-file-size": true, "model": true,
	"ollama-url": true, "batch-size": true, "context-lines": true, "max-lines": true,
	"kind": true, "name": true, "repository": true,
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

func nonNegativeIntOption(args parsedArguments, name string, fallback int) (int, error) {
	value := args.values[name]
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("--%s must be a non-negative integer", name)
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
  grafo install [claude|codex|opencode|all]
  grafo index [path] [--force] [--json]
  grafo watch [path] [--interval 1s]
  grafo status [path] [--repos pathA,pathB] [--json]
  grafo mcp [--repo path | --repos pathA,pathB] [--model embeddinggemma]
  grafo embed [path] [--model embeddinggemma] [--ollama-url http://localhost:11434] [--force]
  grafo reusable <description> [--repo path | --repos pathA,pathB] [--limit 5] [--json]
  grafo find <text> [--limit 20] [--repo path | --repos pathA,pathB] [--json]
  grafo show <symbol-or-id> [--repo path | --repos pathA,pathB] [--json]
  grafo source <symbol-or-id> [--context-lines 2] [--max-lines 200] [--json]
  grafo neighbors <symbol-or-id> [--depth 1] [--direction both]
  grafo callers <symbol-or-id> [--depth 3]
  grafo callees <symbol-or-id> [--depth 3]
  grafo impact <symbol-or-id> [--depth 4]
  grafo path <from> <to> [--direction outgoing] [--relation calls,...]
  grafo data-resources [--kind table,view] [--name text] [--limit 100] [--json]
  grafo data-usage <table-view-or-id> [--limit 100] [--json]
  grafo config-keys [--name text] [--limit 100] [--json]
  grafo events [--name text] [--limit 100] [--json]
  grafo orphaned-events [--name text] [--limit 100] [--json]
  grafo version

Options may appear before or after positional arguments. All query commands
accept --repo or a comma-separated --repos list. Active branch indexes are
refreshed incrementally before queries and never substituted across branches.

Catalog commands accept --repository to restrict results to one indexed
repository by name, and report truncation whenever a bound is reached.
`
