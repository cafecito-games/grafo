package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/agentguide"
	"github.com/cafecito-games/grafo/internal/agentinstall"
	embeddingcache "github.com/cafecito-games/grafo/internal/embedding/cache"
	"github.com/cafecito-games/grafo/internal/embedding/ollama"
	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	branchindexes "github.com/cafecito-games/grafo/internal/indexes"
	"github.com/cafecito-games/grafo/internal/indexseed"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/search"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/service"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/updatecheck"
	"github.com/cafecito-games/grafo/internal/version"
	"github.com/mattn/go-isatty"
)

var Version = version.Value

// foregroundIndexLockWait bounds how long a foreground index waits for a
// background service pass on the same branch index to finish. It is a variable
// so tests can observe contention without waiting out the production budget.
var foregroundIndexLockWait = 2 * time.Minute

// acquireIndexLock serializes one branch-index mutation behind the same lock
// `grafo index`, `grafo watch` and the background supervisor take, and renders
// contention as something the caller can act on. Without the translation a
// waiter reports a lock-file path, or later a raw SQLITE_BUSY, for the ordinary
// situation of another grafo process holding the index.
func acquireIndexLock(indexPath string) (service.Unlock, error) {
	unlock, err := service.IndexLock(indexPath, foregroundIndexLockWait)
	if err != nil {
		if errors.Is(err, service.ErrLockBusy) {
			return nil, fmt.Errorf("another grafo process is refreshing this branch index; "+
				"waited %s, retry once it finishes", foregroundIndexLockWait)
		}
		return nil, err
	}
	return unlock, nil
}

type App struct {
	stdout           io.Writer
	stderr           io.Writer
	stderrIsTerminal func(io.Writer) bool
	progressDelay    time.Duration
	checker          *updatecheck.Checker
}

func New(stdout, stderr io.Writer) *App {
	return &App{
		stdout: stdout, stderr: stderr, progressDelay: 500 * time.Millisecond,
		stderrIsTerminal: func(writer io.Writer) bool {
			file, ok := writer.(*os.File)
			return ok && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()))
		},
	}
}

// print, printf, println and errorf are the only way this package writes to the
// terminal. A failed write to stdout or stderr leaves nothing to fall back on
// and no exit code the caller has not already decided, so the error is dropped
// here once instead of at every call site.
func (a *App) print(text string) { _, _ = fmt.Fprint(a.stdout, text) }

func (a *App) printf(format string, arguments ...any) {
	_, _ = fmt.Fprintf(a.stdout, format, arguments...)
}

func (a *App) println(arguments ...any) { _, _ = fmt.Fprintln(a.stdout, arguments...) }

func (a *App) errorf(format string, arguments ...any) {
	_, _ = fmt.Fprintf(a.stderr, format, arguments...)
}

func (a *App) Run(ctx context.Context, arguments []string) int {
	parsed, err := parseArguments(arguments)
	if err != nil {
		a.fail(err)
		return 2
	}
	if parsed.command == "" || parsed.command == "help" || parsed.flags["help"] {
		a.print(helpText)
		return 0
	}
	if parsed.command == "version" {
		return a.version(ctx, parsed)
	}
	if parsed.flags["check"] {
		a.fail(fmt.Errorf("--check is not supported by %s", parsed.command))
		return 2
	}
	if _, present := parsed.values["path-prefix"]; present && !pathPrefixCommands[parsed.command] {
		a.fail(fmt.Errorf("--path-prefix is not supported by %s", parsed.command))
		return 2
	}
	if parsed.flags["no-seed"] && !seedCommands[parsed.command] {
		a.fail(fmt.Errorf("--no-seed is not supported by %s", parsed.command))
		return 2
	}
	updateLookup := a.startUpdateCheck(ctx, parsed)
	defer a.reportUpdate(ctx, updateLookup)
	var runErr error
	if handler, dispatched := commandHandlers[parsed.command]; dispatched {
		runErr = handler(a, ctx, parsed)
	} else if namespace, known := toolchainNamespaces[parsed.command]; known {
		runErr = a.toolchainCommand(ctx, namespace, parsed)
	} else {
		runErr = unknownCommandError(parsed.command)
	}
	if runErr != nil {
		var rendered *progressRenderedError
		switch {
		case errors.As(runErr, &rendered):
			// The command already rendered its own failure on the terminal.
		case runCanceled(ctx, runErr):
			// A signalled run surfaces whatever the innermost dependency was
			// doing when the context was cancelled, wrapped by every layer on
			// the way out. Rendering that cause blames an embedding provider,
			// an HTTP endpoint or the database for an interruption none of them
			// caused, so the interruption is reported instead.
			a.errorf("grafo: interrupted\n")
		default:
			a.fail(runErr)
		}
		var ambiguous *query.AmbiguousError
		if errors.As(runErr, &ambiguous) {
			a.printNodes(ambiguous.Candidates)
			if ambiguous.Total > len(ambiguous.Candidates) {
				a.errorf("grafo: %d further matches are not listed; narrow the selector or add --kind\n",
					ambiguous.Total-len(ambiguous.Candidates))
			}
		}
		return 1
	}
	return 0
}

// installTargets merges the legacy positional client names with --client so
// both spellings keep resolving to the same clients.
func installTargets(args parsedArguments) agentinstall.Options {
	targets := append([]string{}, args.positionals...)
	targets = append(targets, splitList(args.values["client"])...)
	if args.flags["all"] {
		targets = nil
	}
	return agentinstall.Options{
		Targets: targets,
		All:     args.flags["all"],
		DryRun:  args.flags["dry-run"],
		MCPOnly: args.flags["mcp-only"],
		Hooks:   args.flags["hooks"],
		Refresh: args.flags["refresh"],
	}
}

// announcer reports every planned target and action before anything is mutated.
// Dry runs and JSON output already print the full plan, so they pass nil.
func (a *App) announcer(args parsedArguments) func([]agentinstall.Action) {
	if args.flags["dry-run"] || args.flags["json"] {
		return nil
	}
	return func(planned []agentinstall.Action) {
		if len(planned) == 0 {
			return
		}
		a.println("planned changes:")
		for _, action := range planned {
			action.DryRun = true
			a.printAgentAction(action, "  ")
		}
		a.println("applying:")
	}
}

func (a *App) install(ctx context.Context, args parsedArguments) error {
	environment := agentinstall.NewOSEnvironment()
	if args.flags["list"] {
		statuses, err := agentinstall.Detect(ctx, environment, installTargets(args).Targets)
		if err != nil {
			return err
		}
		if args.flags["json"] {
			return writeJSON(a.stdout, statuses)
		}
		for _, status := range statuses {
			state := "not installed"
			if status.Installed && status.Registered {
				state = "registered"
			} else if status.Installed {
				state = "installed, grafo not registered"
			}
			a.printf("%-14s  %-18s  %-32s  %s\n", status.Client.Name, status.Client.Display, state, status.Path)
		}
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate grafo executable: %w", err)
	}
	if strings.Contains(executable, string(os.PathSeparator)+"go-build") {
		return fmt.Errorf("cannot install from a temporary 'go run' binary; install grafo with 'go install github.com/cafecito-games/grafo/cmd/grafo@latest' first")
	}
	options := installTargets(args)
	options.Announce = a.announcer(args)
	actions, installErr := agentinstall.Install(ctx, environment, executable, options)
	if err := a.printAgentActions(actions, args.flags["json"]); err != nil {
		return err
	}
	if installErr != nil {
		return installErr
	}
	if !args.flags["dry-run"] && !args.flags["json"] {
		a.println("run 'grafo index .' once in each repository before using the MCP tools")
	}
	return nil
}

func (a *App) uninstall(ctx context.Context, args parsedArguments) error {
	options := installTargets(args)
	options.Announce = a.announcer(args)
	actions, uninstallErr := agentinstall.Uninstall(ctx, agentinstall.NewOSEnvironment(), options)
	if err := a.printAgentActions(actions, args.flags["json"]); err != nil {
		return err
	}
	return uninstallErr
}

func (a *App) printAgentActions(actions []agentinstall.Action, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, actions)
	}
	for _, action := range actions {
		a.printAgentAction(action, "")
	}
	return nil
}

// plannedVerbs render a dry-run or announced row as an intention.
var plannedVerbs = map[string]string{
	"installed": "install", "updated": "update", "removed": "remove",
	"unchanged": "leave unchanged", "skipped": "skip",
}

func (a *App) printAgentAction(action agentinstall.Action, indent string) {
	change := action.Change
	if action.DryRun {
		verb, known := plannedVerbs[change]
		if !known {
			verb = change
		}
		change = "would " + verb
	}
	a.printf("%s%s %s", indent, change, action.Client.Display)
	if action.Kind != "" {
		a.printf(" %s", action.Kind)
	}
	if action.Scope != "" {
		a.printf(" (%s scope)", action.Scope)
	}
	if action.Target != "" {
		a.printf(" · %s", action.Target)
	}
	if action.Detail != "" {
		a.printf(" · %s", action.Detail)
	}
	a.println()
}

// guidance prints Grafo's canonical agent guidance, or one advisory hook hint.
//
// Hook mode is advisory only: it never fails and never returns a nonzero exit
// status, so a client hook cannot block a tool call when Grafo is unavailable or
// the repository has no index.
func (a *App) guidance(ctx context.Context, args parsedArguments) error {
	root := repoPath(args)
	if phase := strings.TrimSpace(args.values["hook"]); phase != "" {
		a.println(advisoryHint(phase, a.indexSummary(ctx, root)))
		return nil
	}
	a.print(agentguide.Text())
	a.printf("\n%s\n", a.indexSummary(ctx, root))
	return nil
}

// advisoryHint renders the short context a pre-search or pre-edit hook injects.
func advisoryHint(phase, index string) string {
	switch phase {
	case "pre-search":
		return "Grafo advisory (pre-search): " + index +
			" For symbol, call, endpoint, event, data, or impact questions, resolve the symbol with" +
			" Grafo's graph tools before searching text."
	case "pre-edit":
		return "Grafo advisory (pre-edit): " + index +
			" Run get_blast_radius (grafo impact <symbol>) before a behaviour-changing edit, and" +
			" find_reusable_code before adding new code."
	default:
		return "Grafo advisory: " + index
	}
}

// indexSummary reports whether the current repository and branch have an index,
// so guidance never recommends the graph for an unindexed branch.
func (a *App) indexSummary(ctx context.Context, root string) string {
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		return "Index status: unknown (" + err.Error() + "); use native tools."
	}
	if _, statErr := os.Stat(project.IndexPath); statErr != nil {
		return fmt.Sprintf("Index status: none for %s on branch %s; use native tools, or run 'grafo index %s' first.",
			project.Name, project.Branch, root)
	}
	return fmt.Sprintf("Index status: ready for %s on branch %s; prefer Grafo's graph tools.",
		project.Name, project.Branch)
}

func (a *App) index(ctx context.Context, args parsedArguments) error {
	root, err := optionalPath(args.positionals)
	if err != nil {
		return err
	}
	mode, err := parseProgressMode(args.values["progress"])
	if err != nil {
		return err
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		return err
	}
	// The same lock the background supervisor takes, so a foreground index and a
	// service pass can never write one branch index concurrently.
	unlock, err := acquireIndexLock(project.IndexPath)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	// Adoption runs before the index is opened, so a database it leaves behind
	// faces the same migration and compatibility checks as any other. That is
	// what keeps it incapable of introducing a failure the cold path would not
	// already handle.
	seed := a.seedBranchIndex(ctx, project, args.flags["no-seed"])
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		return err
	}
	defer func() { _ = repository.Close() }()
	return a.runIndex(ctx, project, repository, mode, args, seed)
}

// seedBranchIndex adopts a sibling worktree's index when this branch has none.
// The caller must already hold the branch index lock. A declined adoption is
// reported on stderr and is never an error: the indexing pass that follows
// builds the index from scratch exactly as it did before.
func (a *App) seedBranchIndex(ctx context.Context, project indexer.Project, disabled bool) *indexer.SeedProvenance {
	if disabled {
		return nil
	}
	if _, err := os.Stat(project.IndexPath); err == nil {
		return nil
	}
	result, err := indexseed.Seed(ctx, project, indexseed.Options{TryLock: service.TryDonorIndexLock})
	if err != nil {
		return nil
	}
	if !result.Seeded {
		// A repository with a single checkout declines on every cold index, so
		// only a decline that had something to reject is worth a line.
		if result.Considered > 0 {
			a.errorf("grafo: indexing from scratch: %s\n", result.Reason)
		}
		return nil
	}
	a.errorf("grafo: adopted the index of %s at %s; every file is re-read to verify it\n",
		result.Provenance.DonorRoot, shortCommit(result.Provenance.DonorCommit))
	return result.Provenance
}

// shortCommit abbreviates a commit for human-facing provenance without implying
// the abbreviation is unique.
func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

// runIndex indexes one discovered project, rendering progress on stderr so the
// report on stdout stays machine-readable, and reporting the phase timings a
// cancelled run already measured instead of discarding them.
func (a *App) runIndex(ctx context.Context, project indexer.Project, repository graph.IndexRepository,
	mode progressMode, args parsedArguments, seed *indexer.SeedProvenance) error {
	maxSize, err := int64Option(args, "max-file-size", 5<<20)
	if err != nil {
		return err
	}
	// Full-graph counts are pure reporting work whose cost scales with total
	// graph size rather than with what changed, so the interactive index command
	// collects them only when the invocation asked for the summary.
	detail := indexer.ReportWithoutCounts
	if args.flags["counts"] {
		detail = indexer.ReportComplete
	}
	renderer := newProgressRenderer(a.stderr, mode, a.stderrIsTerminal(a.stderr), a.progressDelay)
	report, runErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{
		Force: args.flags["force"], MaxFileSize: maxSize, ReportDetail: detail,
		ProgressObserver: nonFatalObserver(renderer.Observer()), Seed: seed,
	})
	_ = renderer.Close()
	if runErr != nil {
		if runCanceled(ctx, runErr) {
			a.printInterruptedIndexReport(report, args.flags["json"])
		}
		if renderer.terminalRendered() {
			return &progressRenderedError{runErr}
		}
		return runErr
	}
	return a.printIndexReport(report, args.flags["json"], args.flags["counts"])
}

// nonFatalObserver keeps index progress strictly non-load-bearing. The indexer
// propagates observer errors into the run, so a closed pipe or a full disk on
// stderr would otherwise fail an index whose graph is already complete.
func nonFatalObserver(observer indexer.ProgressObserver) indexer.ProgressObserver {
	if observer == nil {
		return nil
	}
	return func(event indexer.ProgressEvent) error {
		_ = observer(event)
		return nil
	}
}

// runCanceled reports whether a failed run was interrupted rather than broken.
// ctx is consulted too because an interrupted dependency can surface a wrapped
// error that no longer unwraps to a context cause.
func runCanceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
}

func (a *App) indexes(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) == 0 {
		return fmt.Errorf("usage: grafo indexes list|prune|compact [path]")
	}
	subcommand := args.positionals[0]
	root, err := optionalPath(args.positionals[1:])
	if err != nil {
		return fmt.Errorf("usage: grafo indexes %s [path]: %w", subcommand, err)
	}
	switch subcommand {
	case "list":
		if err := rejectUnsupportedOptions(args, "indexes "+subcommand, map[string]bool{"json": true}, nil); err != nil {
			return err
		}
		inventory, err := branchindexes.List(ctx, root)
		if err != nil {
			return err
		}
		return a.printIndexInventory(inventory, args.flags["json"])
	case "prune":
		if err := rejectUnsupportedOptions(args, "indexes "+subcommand,
			map[string]bool{"json": true, "dry-run": true, "yes": true},
			map[string]bool{"older-than": true, "keep": true}); err != nil {
			return err
		}
		policy, err := indexPrunePolicy(args)
		if err != nil {
			return err
		}
		report, pruneErr := branchindexes.Prune(ctx, root, policy)
		if err := a.printIndexPruneReport(report, args.flags["json"]); err != nil {
			return err
		}
		return pruneErr
	case "compact":
		if err := rejectUnsupportedOptions(args, "indexes "+subcommand,
			map[string]bool{"json": true, "dry-run": true, "yes": true}, nil); err != nil {
			return err
		}
		policy := branchindexes.CompactPolicy{DryRun: args.flags["dry-run"], Confirm: args.flags["yes"]}
		if !policy.DryRun && !policy.Confirm {
			return fmt.Errorf("compaction requires --yes (or use --dry-run)")
		}
		report, err := branchindexes.Compact(ctx, root, policy)
		if err != nil {
			return err
		}
		return a.printIndexCompactReport(report, args.flags["json"])
	default:
		return fmt.Errorf("usage: grafo indexes list|prune|compact [path]")
	}
}

func indexPrunePolicy(args parsedArguments) (branchindexes.Policy, error) {
	policy := branchindexes.Policy{DryRun: args.flags["dry-run"], Confirm: args.flags["yes"]}
	if raw, present := args.values["older-than"]; present {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return policy, fmt.Errorf("--older-than must be a duration such as 720h")
		}
		if value < 0 {
			return policy, fmt.Errorf("--older-than must not be negative")
		}
		policy.OlderThan = &value
	}
	if raw, present := args.values["keep"]; present {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return policy, fmt.Errorf("--keep must be a non-negative integer")
		}
		if value < 0 {
			return policy, fmt.Errorf("--keep must not be negative")
		}
		policy.Keep = &value
	}
	if policy.OlderThan == nil && policy.Keep == nil {
		return policy, fmt.Errorf("at least one of --older-than or --keep is required")
	}
	if !policy.DryRun && !policy.Confirm {
		return policy, fmt.Errorf("pruning requires --yes (or use --dry-run)")
	}
	return policy, nil
}

func rejectUnsupportedOptions(args parsedArguments, command string, allowedFlags, allowedValues map[string]bool) error {
	var unsupported []string
	for name, enabled := range args.flags {
		if enabled && !allowedFlags[name] {
			unsupported = append(unsupported, "--"+name)
		}
	}
	for name := range args.values {
		if !allowedValues[name] {
			unsupported = append(unsupported, "--"+name)
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	sort.Strings(unsupported)
	return fmt.Errorf("%s is not supported by grafo %s", strings.Join(unsupported, ", "), command)
}

func (a *App) printIndexInventory(inventory branchindexes.Inventory, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, inventory)
	}
	a.println("CURRENT\tBRANCH\tCOMMIT\tINDEXED_AT\tREPOSITORY_ID\tROOT\tCOMPATIBILITY\tDATABASE\tWAL\tSHM\tTOTAL\tPAGE_SIZE\tPAGE_COUNT\tFREELIST\tLIVE_ALLOCATED\tRECLAIMABLE\tRECLAIMABLE_PERCENT\tCOMPACT_RECOMMENDED\tFILENAME\tPATH")
	for _, candidate := range inventory.Indexes {
		current := ""
		if candidate.Current {
			current = "*"
		}
		metrics := sqlite.StorageMetrics{}
		if candidate.Metrics != nil {
			metrics = *candidate.Metrics
		}
		a.printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\t%t\t%s\t%s\n",
			current, candidate.Branch, candidate.Commit, candidate.IndexedAt,
			candidate.RepositoryID, candidate.Root, candidate.Compatibility,
			candidate.Sizes.Database, candidate.Sizes.WAL, candidate.Sizes.SHM, candidate.Sizes.Total,
			metrics.PageSize, metrics.PageCount, metrics.FreelistCount, metrics.LiveAllocatedBytes,
			metrics.ReclaimableBytes, metrics.ReclaimablePercent, candidate.CompactRecommended,
			candidate.Filename, candidate.Path)
		if candidate.Diagnostic != "" {
			a.printf("  diagnostic: %s\n", candidate.Diagnostic)
		}
	}
	a.printf("totals database=%d wal=%d shm=%d total=%d\n",
		inventory.Totals.Database, inventory.Totals.WAL, inventory.Totals.SHM, inventory.Totals.Total)
	return nil
}

func (a *App) printIndexCompactReport(report branchindexes.CompactReport, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, report)
	}
	a.printf("index=%s dry_run=%t expected_upper_bound=%d\n", report.Path, report.DryRun, report.ExpectedUpperBoundBytes)
	a.printCompactState("before", report.Before)
	if report.After != nil {
		a.printCompactState("after", *report.After)
		a.printf("reclaimed database=%d wal=%d shm=%d total=%d\n",
			report.Reclaimed.Database, report.Reclaimed.WAL, report.Reclaimed.SHM, report.Reclaimed.Total)
	}
	return nil
}

func (a *App) printCompactState(label string, state branchindexes.CompactState) {
	a.printf("%s database=%d wal=%d shm=%d total=%d page_size=%d page_count=%d freelist=%d live_allocated=%d reclaimable=%d reclaimable_percent=%.2f\n",
		label, state.Sizes.Database, state.Sizes.WAL, state.Sizes.SHM, state.Sizes.Total,
		state.Metrics.PageSize, state.Metrics.PageCount, state.Metrics.FreelistCount,
		state.Metrics.LiveAllocatedBytes, state.Metrics.ReclaimableBytes, state.Metrics.ReclaimablePercent)
}

func (a *App) printIndexPruneReport(report branchindexes.PruneReport, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, report)
	}
	for _, result := range report.Results {
		a.printf("%s\t%s\t%s\t%s\n", result.Status, result.Index.Branch, result.Index.Filename, result.Reason)
	}
	a.printf("reclaimed database=%d wal=%d shm=%d total=%d\n",
		report.Reclaimed.Database, report.Reclaimed.WAL, report.Reclaimed.SHM, report.Reclaimed.Total)
	return nil
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
		unlock, err := acquireIndexLock(project.IndexPath)
		if err != nil {
			return err
		}
		defer func() { _ = unlock() }()
		seed := a.seedBranchIndex(ctx, project, args.flags["no-seed"])
		repository, err := sqlite.Open(ctx, project.IndexPath)
		if err != nil {
			return err
		}
		detail := indexer.ReportWithoutCounts
		if args.flags["counts"] {
			detail = indexer.ReportComplete
		}
		report, runErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project,
			indexer.Options{ReportDetail: detail, Seed: seed})
		closeErr := repository.Close()
		if runErr != nil {
			return runErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(report.Updated) > 0 || len(report.Removed) > 0 || len(report.Diagnostics) > 0 {
			return a.printIndexReport(report, args.flags["json"], args.flags["counts"])
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
				a.errorf("grafo watch: %v\n", err)
			}
		}
	}
}

type statusOutput struct {
	Projects  []indexer.Project `json:"projects"`
	IndexedAt string            `json:"indexed_at"`
	Commit    string            `json:"indexed_commit,omitempty"`
	Counts    graph.Counts      `json:"counts"`
	Refresh   []refreshSummary  `json:"refresh"`
}

type refreshSummary struct {
	RepositoryID                 string                 `json:"repository_id"`
	RepositoryName               string                 `json:"repository_name"`
	Branch                       string                 `json:"branch"`
	RebuildReason                string                 `json:"rebuild_reason,omitempty"`
	ElapsedMS                    int64                  `json:"elapsed_ms"`
	Checked                      int                    `json:"content_checked"`
	Updated                      int                    `json:"updated"`
	Removed                      int                    `json:"removed"`
	ReconciliationBatches        int                    `json:"reconciliation_batches"`
	ReconciliationPendingAtStart bool                   `json:"reconciliation_pending_at_start"`
	Phases                       indexer.PhaseDurations `json:"phases"`
	Writes                       graph.WriteStats       `json:"writes"`
}

func summarizeRefresh(reports []indexer.Report) []refreshSummary {
	result := make([]refreshSummary, 0, len(reports))
	for _, report := range reports {
		result = append(result, refreshSummary{
			RepositoryID: report.Project.ID, RepositoryName: report.Project.Name, Branch: report.Project.Branch,
			RebuildReason: report.Rebuild, ElapsedMS: report.ElapsedMS, Checked: report.Checked,
			Updated: len(report.Updated), Removed: len(report.Removed), ReconciliationBatches: report.ReconciliationBatches,
			ReconciliationPendingAtStart: report.ReconciliationPendingAtStart, Phases: report.Phases, Writes: report.Writes,
		})
	}
	return result
}

func (a *App) status(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) > 1 || (len(args.positionals) == 1 && args.values["repos"] != "") {
		return fmt.Errorf("usage: grafo status [path] [--repos pathA,pathB]")
	}
	if len(args.positionals) == 1 {
		args.values["repo"] = args.positionals[0]
	}
	mode, err := parseProgressMode(args.values["progress"])
	if err != nil {
		return err
	}
	renderer := newProgressRenderer(a.stderr, mode, a.stderrIsTerminal(a.stderr), a.progressDelay)
	repository, projects, reports, closeRepository, err := openStatusRead(ctx, args, renderer.Observer())
	if err != nil {
		if !renderer.hasTerminal() {
			state := indexer.ProgressError
			sensitivePaths := []string{repoPath(args)}
			if raw := args.values["repos"]; raw != "" {
				sensitivePaths = append(sensitivePaths, splitList(raw)...)
			}
			message := indexer.ProgressErrorMessage(err, sensitivePaths...)
			if runCanceled(ctx, err) {
				state = indexer.ProgressCanceled
			}
			observeErr := renderer.Observe(indexer.ProgressEvent{
				Schema: indexer.ProgressSchemaV1, Phase: indexer.ProgressComplete, State: state, Error: message,
			})
			if observeErr != nil {
				err = errors.Join(err, observeErr)
			}
		}
		closeErr := renderer.Close()
		if closeErr != nil {
			return errors.Join(err, closeErr)
		}
		if renderer.terminalRendered() {
			return &progressRenderedError{err}
		}
		return err
	}
	defer func() { _ = closeRepository() }()
	if err := renderer.Close(); err != nil {
		_ = closeRepository()
		return err
	}
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
	output := statusOutput{Projects: projects, IndexedAt: indexedAt, Commit: commit, Counts: counts, Refresh: summarizeRefresh(reports)}
	if args.flags["json"] {
		return writeJSON(a.stdout, output)
	}
	for _, project := range projects {
		a.printf("%s · branch %s\n", project.Name, project.Branch)
	}
	a.printf("%d files · %d nodes · %d edges · %d unresolved\n", counts.Files, counts.Nodes, counts.Edges, counts.External)
	a.printf("indexed %s\n", indexedAt)
	return nil
}

func (a *App) mcp(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo mcp [--repo path | --repos pathA,pathB]")
	}
	roots, err := mcpRoots(args)
	if err != nil {
		return err
	}
	// The handshake must not wait on an index refresh: a stale index would
	// otherwise hold `initialize` past the client's connect timeout and the
	// whole server would be reported as failed. Service.ready acquires and
	// rebinds a refreshed generation on every tool call, so deferring startup
	// refresh costs nothing in result freshness. See issue #173.
	coordinator, generation, err := mcpserver.NewFreshnessCoordinator(ctx, roots, parserdefaults.NewRegistry(),
		mcpserver.FreshnessCoordinatorOptions{DeferStartupRefresh: true})
	if err != nil {
		return err
	}
	defer func() { _ = coordinator.Close() }()
	service := mcpserver.NewFederated(generation.Repository, generation.Projects).WithFreshness(coordinator)
	sourceService, err := newSourceService(generation.Repository, generation.Projects)
	if err != nil {
		return err
	}
	service.WithSource(sourceService.ReadKind).WithSourceFactory(func(repository graph.ReadRepository, projects []indexer.Project) (func(context.Context, string, graph.NodeKind, int, int) (sourcecontext.Excerpt, error), error) {
		reader, factoryErr := newSourceService(repository, projects)
		if factoryErr != nil {
			return nil, factoryErr
		}
		return reader.ReadKind, nil
	})
	searchService, err := newSearchService(generation.Repository, generation.Projects)
	if err != nil {
		return err
	}
	service.WithSearch(searchService).WithSearchFactory(newSearchService)
	// The tool call must not block on a full embedding backfill: SearchWith
	// bounds backfill, ranking, and graph resolution inside its budget and
	// reports coverage, so a cold cache answers instead of timing out.
	service.WithReusableFactory(func(_ graph.ReadRepository, projects []indexer.Project) mcpserver.ReusableSearch {
		return func(searchContext context.Context, request semantic.SearchRequest) (semantic.SearchResult, error) {
			repository, closeRepository, openErr := openReadOnlyProjects(searchContext, projects)
			if openErr != nil {
				return semantic.SearchResult{}, openErr
			}
			defer func() { _ = closeRepository() }()
			semanticService, closeCache, serviceErr := newSemanticService(searchContext, repository, args)
			if serviceErr != nil {
				return semantic.SearchResult{}, serviceErr
			}
			defer func() { _ = closeCache() }()
			return semanticService.SearchWith(searchContext, request)
		}
	})
	return service.Run(ctx, Version)
}

func mcpRoots(args parsedArguments) ([]string, error) {
	if args.values["repo"] != "" && args.values["repos"] != "" {
		return nil, fmt.Errorf("--repo and --repos cannot be used together")
	}
	if raw := args.values["repos"]; raw != "" {
		roots := splitList(raw)
		if len(roots) < 2 {
			return nil, fmt.Errorf("federation requires at least two repository paths")
		}
		return roots, nil
	}
	return []string{repoPath(args)}, nil
}

func openReadOnlyProjects(ctx context.Context, projects []indexer.Project) (graph.ReadRepository, func() error, error) {
	if len(projects) == 1 {
		repository, err := sqlite.OpenReadOnly(ctx, projects[0].IndexPath)
		if err != nil {
			return nil, nil, err
		}
		return repository, repository.Close, nil
	}
	repository, err := federation.OpenReadOnlyProjects(ctx, projects)
	if err != nil {
		return nil, nil, err
	}
	return repository, repository.Close, nil
}

func (a *App) embed(ctx context.Context, args parsedArguments) error {
	root, err := optionalPath(args.positionals)
	if err != nil {
		return fmt.Errorf("usage: grafo embed [path] [--model name] [--ollama-url url] [--force]")
	}
	project, repository, unlock, err := openExisting(ctx, root)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	defer func() { _ = repository.Close() }()
	service, closeCache, err := newSemanticService(ctx, repository, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeCache() }()
	report, err := service.Sync(ctx)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	a.printf("embedded %s · branch %s · model %s\n", project.Name, project.Branch, report.Model)
	a.printf("%d updated · %d unchanged · %d removed · %d candidates\n",
		report.Updated, report.Unchanged, report.Removed, report.Candidates)
	return nil
}

func (a *App) reusable(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) == 0 {
		return fmt.Errorf("usage: grafo reusable <description> [--repo path | --repos pathA,pathB] [--limit 5] [--budget 45s] [--language name] [--path-prefix dir]")
	}
	limit, err := intOption(args, "limit", 5)
	if err != nil {
		return err
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	service, closeCache, err := newSemanticService(ctx, repository, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeCache() }()
	budget, err := durationOption(args, "budget")
	if err != nil {
		return err
	}
	result, err := service.SearchWith(ctx, semantic.SearchRequest{Query: strings.Join(args.positionals, " "),
		Limit: limit, Budget: budget, Languages: splitList(args.values["language"]),
		PathPrefixes: splitList(args.values["path-prefix"])})
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, match := range result.Matches {
		a.printf("%.4f  %-12s  %-48s  %s · %d connected nodes\n",
			match.Score, match.Node.Kind, match.Node.QualifiedName, formatLocation(match.Node.Location), len(match.Context.Nodes)-1)
	}
	if result.Status != semantic.StatusComplete {
		a.printf("%s · %d/%d documents embedded · %dms\n", result.Status,
			result.Coverage.Embedded, result.Coverage.Documents, result.Timings.TotalMilliseconds)
		for _, note := range result.Notes {
			a.printf("  %s\n", note)
		}
		if result.NextAction != "" {
			a.printf("  next: %s\n", result.NextAction)
		}
	}
	return nil
}

// durationOption reads an optional Go duration option such as --budget 30s.
func durationOption(args parsedArguments, name string) (time.Duration, error) {
	raw := strings.TrimSpace(args.values[name])
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("--%s expects a positive Go duration such as 30s", name)
	}
	return value, nil
}

func newSemanticService(ctx context.Context, repository graph.ReadRepository, args parsedArguments) (*semantic.Service, func() error, error) {
	candidates, ok := repository.(semantic.CandidateRepository)
	if !ok {
		return nil, nil, fmt.Errorf("repository does not support semantic candidate discovery")
	}
	batchSize, err := intOption(args, "batch-size", 32)
	if err != nil {
		return nil, nil, err
	}
	model := firstValue(args.values["model"], os.Getenv("GRAFO_EMBED_MODEL"), ollama.DefaultModel)
	baseURL := firstValue(args.values["ollama-url"], os.Getenv("GRAFO_OLLAMA_URL"), ollama.DefaultURL)
	embedder, err := ollama.New(baseURL, model)
	if err != nil {
		return nil, nil, err
	}
	cache, err := embeddingcache.OpenDefault(ctx)
	if err != nil {
		return nil, nil, err
	}
	return semantic.NewService(candidates, repository, cache, embedder).
		WithBatchSize(batchSize).
		WithForce(args.command == "embed" && args.flags["force"]), cache.Close, nil
}

func (a *App) embedCache(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 || (args.positionals[0] != "status" && args.positionals[0] != "prune") {
		return fmt.Errorf("usage: grafo embed-cache status [--json] | grafo embed-cache prune [--model name] [--older-than duration] [--max-bytes n] [--dry-run] [--yes] [--json]")
	}
	operation := args.positionals[0]
	allowedFlags := map[string]bool{"json": true}
	var allowedValues map[string]bool
	if operation == "prune" {
		allowedFlags["dry-run"] = true
		allowedFlags["yes"] = true
		allowedValues = map[string]bool{"model": true, "older-than": true, "max-bytes": true}
	}
	if err := rejectUnsupportedOptions(args, "embed-cache "+operation, allowedFlags, allowedValues); err != nil {
		return err
	}
	path, err := embeddingcache.ResolvePath()
	if err != nil {
		return err
	}
	if operation == "status" {
		store, err := embeddingcache.OpenReadOnly(ctx, path)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		status, err := store.Status(ctx)
		if err != nil {
			return err
		}
		if args.flags["json"] {
			return writeJSON(a.stdout, status)
		}
		a.printf("embedding cache %s · schema %d\n", status.Path, status.SchemaVersion)
		a.printf("%d rows · %d vector bytes · %d database bytes · %d WAL bytes · %d SHM bytes · %d reclaimable bytes\n",
			status.Rows, status.BlobBytes, status.DatabaseBytes, status.WALBytes, status.SHMBytes, status.ReclaimableBytes)
		for _, model := range status.Models {
			a.printf("%s · %d rows · dimensions %v · %d vector bytes · used %s..%s\n",
				model.Model, model.Rows, model.Dimensions, model.BlobBytes, model.OldestUsedAt, model.NewestUsedAt)
		}
		return nil
	}
	options, err := pruneOptions(args)
	if err != nil {
		return err
	}
	if !options.DryRun && !args.flags["yes"] {
		return errors.New("embedding cache prune requires --yes (or use --dry-run)")
	}
	var store *embeddingcache.Cache
	if options.DryRun {
		store, err = embeddingcache.OpenReadOnly(ctx, path)
	} else {
		store, err = embeddingcache.Open(ctx, path)
	}
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	report, err := store.Prune(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	verb := "pruned"
	if report.DryRun {
		verb = "would prune"
	}
	a.printf("%s %d rows · %d logical vector bytes\n", verb, report.DeletedRows, report.FreedBlobBytes)
	a.printf("%d rows · %d vector bytes remain · %d database bytes · %d reclaimable bytes\n",
		report.RemainingRows, report.RemainingBlobBytes, report.DatabaseBytes, report.ReclaimableBytes)
	return nil
}

func pruneOptions(args parsedArguments) (embeddingcache.PruneOptions, error) {
	result := embeddingcache.PruneOptions{DryRun: args.flags["dry-run"]}
	if model, present := args.values["model"]; present {
		if strings.TrimSpace(model) == "" {
			return result, errors.New("--model cannot be empty")
		}
		result.Model = model
	}
	if value := args.values["older-than"]; value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return result, errors.New("--older-than must be a positive duration such as 24h")
		}
		result.OlderThan = duration
	}
	if value := args.values["max-bytes"]; value != "" {
		maximum, err := strconv.ParseInt(value, 10, 64)
		if err != nil || maximum < 0 {
			return result, errors.New("--max-bytes must be a non-negative integer")
		}
		result.MaxBytes = &maximum
	}
	return result, nil
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
	defer func() { _ = closeRepository() }()
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
		return fmt.Errorf("usage: grafo show <symbol-or-id> [--kind function]")
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	node, err := query.NewService(repository).ResolveKind(ctx, args.positionals[0], kind)
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
		return fmt.Errorf("usage: grafo source <symbol-or-id> [--kind function] [--context-lines 2] [--max-lines 200]")
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
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
	defer func() { _ = closeRepository() }()
	service, err := newSourceService(repository, projects)
	if err != nil {
		return err
	}
	excerpt, err := service.ReadKind(ctx, args.positionals[0], kind, contextLines, maxLines)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, excerpt)
	}
	a.printf("%s · %s · %s:%d-%d\n", excerpt.Repository, excerpt.Branch, excerpt.Path, excerpt.StartLine, excerpt.EndLine)
	a.println(excerpt.Content)
	if excerpt.Truncated {
		a.println("… truncated")
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
		return fmt.Errorf("usage: grafo %s <symbol-or-id> [--kind function]", args.command)
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
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
	defer func() { _ = closeRepository() }()
	result, err := query.NewService(repository).Neighborhood(ctx, args.positionals[0], kind, depth, direction, relations, limit)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	a.printf("%s [%s]\n", result.Root.QualifiedName, result.Root.Kind)
	for _, reached := range result.Nodes {
		if reached.Depth == 0 {
			continue
		}
		a.printf("%s↳ %s [%s] %s\n", strings.Repeat("  ", reached.Depth-1), reached.Node.QualifiedName, reached.Node.Kind, formatLocation(reached.Node.Location))
	}
	a.printf("%d nodes · %d edges", len(result.Nodes), len(result.Edges))
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
	return nil
}

func (a *App) path(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 2 {
		return fmt.Errorf("usage: grafo path <from> <to> [--kind function]")
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
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
	defer func() { _ = closeRepository() }()
	result, err := query.NewService(repository).ShortestPath(ctx, args.positionals[0], args.positionals[1], kind, direction, parseRelations(args.values["relation"]), limit)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for index, node := range result.Nodes {
		if index > 0 {
			a.printf("  --%s-->\n", result.Edges[index-1].Kind)
		}
		a.printf("%s [%s] %s\n", node.QualifiedName, node.Kind, formatLocation(node.Location))
	}
	return nil
}

func (a *App) testCoverage(ctx context.Context, args parsedArguments, find bool) error {
	if len(args.positionals) != 1 {
		if find {
			return fmt.Errorf("usage: grafo find-tests <production-symbol-or-id> [--kind function] [--depth 8] [--limit 100] [--json]")
		}
		return fmt.Errorf("usage: grafo test-coverage <test-or-id> [--depth 8] [--limit 100] [--json]")
	}
	depth, err := intOption(args, "depth", query.DefaultTestCoverageDepth)
	if err != nil {
		return err
	}
	limit, err := intOption(args, "limit", query.DefaultTestCoverageLimit)
	if err != nil {
		return err
	}
	options := query.TestCoverageOptions{Depth: depth, Limit: limit}
	if find {
		options.Kind, err = nodeKindOption(args)
		if err != nil {
			return err
		}
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	service := query.NewService(repository)
	var report query.TestCoverageReport
	if find {
		report, err = service.FindTests(ctx, args.positionals[0], options)
	} else {
		report, err = service.TestCoverage(ctx, args.positionals[0], options)
	}
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	a.printf("%s [%s] · structural evidence only (not runtime coverage)\n", report.Root.QualifiedName, report.Root.Kind)
	for _, match := range report.Matches {
		label := "helper-expanded"
		if match.Direct {
			label = "direct"
		}
		if find {
			a.printf("%s [%s] · %s · depth %d · %s\n", match.Test.QualifiedName, match.Test.Kind, label, match.Depth,
				formatLocation(match.Test.Location))
		} else {
			a.printf("%s [%s] · %s · depth %d · %s\n", match.Target.QualifiedName, match.Target.Kind, label, match.Depth,
				formatLocation(match.Target.Location))
		}
	}
	a.printf("%d structural matches", len(report.Matches))
	if report.Truncated {
		a.print(" · truncated")
	}
	a.println()
	return nil
}

func (a *App) impact(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo %s <symbol-or-id> [--kind method] [--depth 4] [--upstream-depth n] [--downstream-depth n] [--source]", args.command)
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
	}
	depth, err := intOption(args, "depth", 4)
	if err != nil {
		return err
	}
	limit, err := intOption(args, "limit", 1000)
	if err != nil {
		return err
	}
	options := query.ImpactOptions{
		Kind:          kind,
		UpstreamDepth: depth, DownstreamDepth: depth,
		UpstreamLimit: limit, DownstreamLimit: limit,
		IncludeSource: args.flags["source"],
	}
	for name, field := range map[string]*int{
		"upstream-depth": &options.UpstreamDepth, "downstream-depth": &options.DownstreamDepth,
		"upstream-limit": &options.UpstreamLimit, "downstream-limit": &options.DownstreamLimit,
		"source-limit": &options.SourceLimit, "max-lines": &options.SourceMaxLines,
	} {
		if args.values[name] == "" {
			continue
		}
		value, err := intOption(args, name, 0)
		if err != nil {
			return err
		}
		*field = value
	}
	options.SourceContextLines, err = nonNegativeIntOption(args, "context-lines", 2)
	if err != nil {
		return err
	}
	repository, projects, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	service := query.NewService(repository)
	if options.IncludeSource {
		sourceService, err := newSourceService(repository, projects)
		if err != nil {
			return err
		}
		service = service.WithSourceReader(sourceService)
	}
	report, err := service.Impact(ctx, args.positionals[0], options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	a.printImpactReport(report)
	return nil
}

func (a *App) failureFlow(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo %s <function-method-error-or-id> [--kind function] [--direction both] [--limit 1000]", args.command)
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
	}
	direction, err := directionOption(args)
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
	defer func() { _ = closeRepository() }()
	report, err := query.NewService(repository).FailureFlow(ctx, args.positionals[0], query.FailureFlowOptions{
		Kind: kind, Direction: direction, Limit: limit,
	})
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	a.printFailureFlow(report)
	return nil
}

func (a *App) printFailureFlow(report query.FailureFlow) {
	a.printf("%s [%s] %s\n", report.Root.QualifiedName, report.Root.Kind, formatLocation(report.Root.Location))
	for _, section := range []struct {
		label string
		facts []query.FailureFlowFact
	}{
		{label: "error returns", facts: report.ErrorReturns},
		{label: "escaping failures", facts: report.Escaping},
		{label: "handled failures", facts: report.Handled},
		{label: "panics", facts: report.Panics},
		{label: "recoveries", facts: report.Recoveries},
		{label: "deferred cleanup", facts: report.DeferredCleanup},
	} {
		if len(section.facts) == 0 {
			continue
		}
		a.printf("\n%s (%d)\n", section.label, len(section.facts))
		for _, fact := range section.facts {
			marker := ""
			if fact.Form != "" {
				marker += " · " + fact.Form
			}
			if fact.Conditional {
				marker += " · conditional"
			}
			if fact.Unresolved {
				marker += " · unresolved"
			}
			if fact.Federated {
				marker += " · federated"
			}
			a.printf("  %s --%s--> %s [%s]%s %s\n", fact.Direction, fact.Edge.Kind,
				fact.Node.QualifiedName, fact.Node.Kind, marker, formatLocation(fact.Edge.Location))
		}
	}
	if report.Unresolved > 0 {
		a.printf("\nunresolved facts: %d\n", report.Unresolved)
	}
	if report.Truncated {
		a.println("\ntruncated")
	}
}

func (a *App) godotComposition(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo %s <scene-resource-script-or-autoload> [--kind godot_scene] [--depth 8] [--limit 1000]", args.command)
	}
	depth, err := intOption(args, "depth", 8)
	if err != nil {
		return err
	}
	limit, err := intOption(args, "limit", 1000)
	if err != nil {
		return err
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	report, err := query.NewService(repository).GodotComposition(ctx, args.positionals[0],
		query.GodotCompositionOptions{Depth: depth, Limit: limit, Kind: kind})
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	a.printGodotComposition(report)
	return nil
}

func (a *App) printGodotComposition(report query.GodotComposition) {
	a.printf("%s [%s] %s\n", report.Root.QualifiedName, report.Root.Kind,
		formatLocation(report.Root.Location))
	if len(report.SceneNodes) > 0 {
		a.printf("\nscene nodes (%d)\n", len(report.SceneNodes))
	}
	for _, group := range []struct {
		label     string
		relations []query.GodotRelation
	}{
		{label: "instantiates", relations: report.OutboundInstances},
		{label: "instantiated by", relations: report.InboundInstances},
		{label: "attached scripts", relations: report.AttachedScripts},
		{label: "attached to", relations: report.ScriptAttachments},
		{label: "autoload targets", relations: report.AutoloadTargets},
		{label: "exposed as autoload", relations: report.AutoloadExposures},
	} {
		if len(group.relations) == 0 {
			continue
		}
		a.printf("\n%s (%d)\n", group.label, len(group.relations))
		for _, relation := range group.relations {
			via := ""
			if relation.Via != nil {
				via = " via " + relation.Via.QualifiedName
			}
			marker := ""
			if relation.Federated {
				marker = " · federated"
			}
			if resource := relation.Edge.Properties["resource"]; resource != "" {
				marker += " · " + resource
			}
			if relation.Node.External {
				marker += " · unresolved"
			}
			a.printf("  %s [%s]%s%s\n", relation.Node.QualifiedName, relation.Node.Kind, via, marker)
		}
	}
	if report.Truncated {
		a.println("\ntruncated")
	}
}

func (a *App) godotInteractions(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo %s <scene-node-script-action-group-or-signal> "+
			"[--filter action,group,signal] [--direction both] [--kind godot_scene] "+
			"[--depth 8] [--limit 1000]", args.command)
	}
	depth, err := intOption(args, "depth", 8)
	if err != nil {
		return err
	}
	limit, err := intOption(args, "limit", 1000)
	if err != nil {
		return err
	}
	kind, err := nodeKindOption(args)
	if err != nil {
		return err
	}
	direction, err := directionOption(args)
	if err != nil {
		return err
	}
	categories, err := interactionCategories(args)
	if err != nil {
		return err
	}
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	report, err := query.NewService(repository).GodotInteractions(ctx, args.positionals[0],
		query.GodotInteractionsOptions{Depth: depth, Limit: limit, Kind: kind,
			Direction: direction, Categories: categories})
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, report)
	}
	a.printGodotInteractions(report)
	return nil
}

func (a *App) printGodotInteractions(report query.GodotInteractions) {
	a.printf("%s [%s] %s\n", report.Root.QualifiedName, report.Root.Kind,
		formatLocation(report.Root.Location))
	if len(report.SceneNodes) > 0 {
		a.printf("\nscene nodes (%d)\n", len(report.SceneNodes))
	}
	for _, group := range []struct {
		label        string
		interactions []query.GodotInteraction
	}{
		{label: "outbound", interactions: report.Outbound},
		{label: "inbound", interactions: report.Inbound},
	} {
		if len(group.interactions) == 0 {
			continue
		}
		a.printf("\n%s (%d)\n", group.label, len(group.interactions))
		for _, interaction := range group.interactions {
			via := ""
			if interaction.Via != nil {
				via = " via " + interaction.Via.QualifiedName
			}
			marker := ""
			if interaction.Form != "" {
				marker += " · " + interaction.Form
			}
			if method := interaction.Edge.Properties["method"]; method != "" {
				marker += " · method " + method
			}
			if interaction.Edge.Properties["method_dynamic"] == "true" {
				marker += " · dynamic method"
			}
			if interaction.Federated {
				marker += " · federated"
			}
			if interaction.Node.External {
				marker += " · unresolved"
			}
			a.printf("  %s %s [%s]%s%s\n", interaction.Category,
				interaction.Node.QualifiedName, interaction.Node.Kind, via, marker)
		}
	}
	if report.Unresolved > 0 {
		a.printf("\nunresolved interactions: %d\n", report.Unresolved)
	}
	if report.Truncated {
		a.println("\ntruncated")
	}
}

// directionOption reads --direction strictly. A misspelled direction is an error
// rather than a silent fall back to "both", which would answer a question the
// caller did not ask.
func directionOption(args parsedArguments) (query.Direction, error) {
	switch value := strings.TrimSpace(args.values["direction"]); value {
	case "":
		return query.Both, nil
	case string(query.Outgoing), string(query.Incoming), string(query.Both):
		return query.Direction(value), nil
	default:
		return "", fmt.Errorf("--direction must be one of %s, %s, %s",
			query.Outgoing, query.Incoming, query.Both)
	}
}

// interactionCategories reads the --filter list. An unknown category is an error
// rather than a filter that can never match.
func interactionCategories(args parsedArguments) ([]query.GodotInteractionCategory, error) {
	var categories []query.GodotInteractionCategory
	for _, value := range splitList(args.values["filter"]) {
		category, err := query.ParseGodotInteractionCategory(value)
		if err != nil {
			return nil, err
		}
		if category != "" {
			categories = append(categories, category)
		}
	}
	return categories, nil
}

func (a *App) printImpactReport(report query.ImpactReport) {
	a.printf("%s [%s]\n", report.Root.QualifiedName, report.Root.Kind)
	for _, section := range []query.ImpactSection{report.Upstream, report.Downstream} {
		label := "depends on this"
		if section.Direction == query.Downstream {
			label = "this depends on"
		}
		a.printf("\n%s · %s (depth %d · %d nodes · %d edges)",
			section.Direction, label, section.Depth, len(section.Nodes), len(section.Edges))
		if section.Truncated {
			a.print(" · truncated")
		}
		a.println()
		for _, reached := range section.Nodes {
			if reached.Depth == 0 {
				continue
			}
			a.printf("%s↳ %s [%s] %s\n", strings.Repeat("  ", reached.Depth-1),
				reached.Node.QualifiedName, reached.Node.Kind, formatLocation(reached.Node.Location))
		}
	}
	if len(report.ImpactedFiles) > 0 {
		a.printf("\nimpacted files (%d)\n", len(report.ImpactedFiles))
		for _, file := range report.ImpactedFiles {
			marker := ""
			if file.Federated {
				marker = " · federated"
			}
			a.printf("  %s%s [%s]%s\n", prefixRepository(file.Repository), file.Path,
				strings.Join(file.Directions, ","), marker)
		}
	}
	for _, group := range []struct {
		label     string
		relations []query.ImpactRelation
	}{
		{label: "config relations", relations: report.Config},
		{label: "data relations", relations: report.Data},
		{label: "event relations", relations: report.Events},
	} {
		if len(group.relations) == 0 {
			continue
		}
		a.printf("\n%s (%d)\n", group.label, len(group.relations))
		for _, relation := range group.relations {
			a.printf("  %s --%s--> %s [%s]\n", relation.Direction, relation.Edge.Kind,
				relation.Node.QualifiedName, relation.Node.Kind)
		}
	}
	if len(report.CrossRepository) > 0 {
		a.printf("\ncross-repository hops (%d)\n", len(report.CrossRepository))
		for _, hop := range report.CrossRepository {
			a.printf("  %s --%s--> %s [%s]\n", hop.Direction, hop.Edge.Kind,
				hop.Node.QualifiedName, hop.Node.Kind)
		}
	}
	for _, excerpt := range report.Sources {
		a.printf("\n%s%s:%d-%d\n", prefixRepository(excerpt.Repository), excerpt.Path,
			excerpt.StartLine, excerpt.EndLine)
		a.println(excerpt.Content)
		if excerpt.Truncated {
			a.println("… truncated")
		}
	}
}

func prefixRepository(name string) string {
	if name == "" {
		return ""
	}
	return name + " · "
}

func (a *App) search(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) == 0 {
		return fmt.Errorf("usage: grafo search <pattern>... [--regex] [--case-sensitive] [--path-prefix dir] [--language go] [--context-lines 0] [--max-matches 500]")
	}
	prefixes, err := pathPrefixOption(args)
	if err != nil {
		return err
	}
	request := search.Request{
		Patterns:      args.positionals,
		Regex:         args.flags["regex"],
		CaseSensitive: args.flags["case-sensitive"],
		PathPrefixes:  prefixes,
		Languages:     splitList(args.values["language"]),
		Repositories:  splitList(args.values["repo-name"]),
	}
	contextLines, err := nonNegativeIntOption(args, "context-lines", 0)
	if err != nil {
		return err
	}
	request.ContextLines = contextLines
	for name, field := range map[string]*int{
		"max-matches": &request.MaxMatches, "max-matches-per-file": &request.MaxMatchesPerFile,
		"max-matches-per-pattern": &request.MaxMatchesPattern,
	} {
		if args.values[name] == "" {
			continue
		}
		value, err := intOption(args, name, 0)
		if err != nil {
			return err
		}
		*field = value
	}
	if args.values["max-file-size"] != "" {
		request.MaxFileBytes, err = int64Option(args, "max-file-size", 0)
		if err != nil {
			return err
		}
	}
	repository, projects, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	service, err := newSearchService(repository, projects)
	if err != nil {
		return err
	}
	result, err := service.Search(ctx, request)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, match := range result.Matches {
		for offset, line := range match.Before {
			a.printf("%s%s-%d- %s\n", prefixRepository(match.Repository), match.Path,
				match.Line-len(match.Before)+offset, line)
		}
		a.printf("%s%s:%d:%d: %s\n", prefixRepository(match.Repository), match.Path,
			match.Line, match.Column, match.Text)
		for offset, line := range match.After {
			a.printf("%s%s-%d- %s\n", prefixRepository(match.Repository), match.Path,
				match.Line+offset+1, line)
		}
	}
	a.printf("%d matches · %d files searched · %d skipped", len(result.Matches),
		result.FilesSearched, result.FilesSkipped)
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
	for _, note := range result.Notes {
		a.errorf("grafo search: %s\n", note)
	}
	return nil
}

// newSearchService binds search to the file catalogs of the opened indexes.
// Index membership, not the filesystem, decides what is searchable.
func newSearchService(repository graph.ReadRepository, projects []indexer.Project) (*search.Service, error) {
	if federated, ok := repository.(interface{ Members() []federation.Member }); ok {
		members := federated.Members()
		sources := make([]search.Source, 0, len(members))
		for _, member := range members {
			sources = append(sources, search.Source{Project: member.Project, Catalog: member.Files})
		}
		return search.NewService(sources), nil
	}
	catalog, ok := repository.(graph.FileCatalog)
	if !ok || len(projects) != 1 {
		return nil, fmt.Errorf("repository does not expose an indexed file catalog")
	}
	return search.NewService([]search.Source{{Project: projects[0], Catalog: catalog}}), nil
}

func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	var result []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func pathPrefixOption(args parsedArguments) ([]string, error) {
	raw, present := args.values["path-prefix"]
	if !present {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("--path-prefix values must be non-empty")
		}
		values = append(values, part)
	}
	return query.NormalizePathPrefixes(values)
}

func (a *App) catalogOptions(args parsedArguments) (query.CatalogOptions, error) {
	limit, err := intOption(args, "limit", query.DefaultCatalogLimit)
	if err != nil {
		return query.CatalogOptions{}, err
	}
	prefixes, err := pathPrefixOption(args)
	if err != nil {
		return query.CatalogOptions{}, err
	}
	return query.CatalogOptions{Repository: args.values["repo-name"],
		Name: args.values["name"], PathPrefixes: prefixes, Limit: limit}, nil
}

func openCatalog(ctx context.Context, args parsedArguments) (*query.Catalog, func() error, error) {
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return nil, nil, err
	}
	catalogRepository, ok := repository.(graph.CatalogRepository)
	if !ok {
		_ = closeRepository()
		return nil, nil, errors.New("repository does not support catalog queries")
	}
	return query.NewCatalog(catalogRepository), closeRepository, nil
}

func (a *App) topologyOptions(args parsedArguments) (query.TopologyOptions, error) {
	limit, err := intOption(args, "limit", query.DefaultCatalogLimit)
	if err != nil {
		return query.TopologyOptions{}, err
	}
	prefixes, err := pathPrefixOption(args)
	if err != nil {
		return query.TopologyOptions{}, err
	}
	return query.TopologyOptions{
		Repository: args.values["repo-name"], Component: args.values["component"], Method: args.values["method"],
		Route: args.values["route"], Event: args.values["event"],
		Direction: query.Direction(args.values["direction"]), PathPrefixes: prefixes, Limit: limit,
	}, nil
}

func openTopology(ctx context.Context, args parsedArguments) (*query.Topology, func() error, error) {
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return nil, nil, err
	}
	topologyRepository, ok := repository.(graph.TopologyRepository)
	if !ok {
		_ = closeRepository()
		return nil, nil, errors.New("repository does not support topology queries")
	}
	return query.NewTopology(topologyRepository), closeRepository, nil
}

func openMessageFlow(ctx context.Context, args parsedArguments) (*query.MessageFlowService, func() error, error) {
	repository, _, closeRepository, err := openRead(ctx, args)
	if err != nil {
		return nil, nil, err
	}
	topologyRepository, ok := repository.(graph.TopologyRepository)
	if !ok {
		_ = closeRepository()
		return nil, nil, errors.New("repository does not support message-flow queries")
	}
	return query.NewMessageFlow(topologyRepository), closeRepository, nil
}

func (a *App) messageFlowOptions(args parsedArguments) (query.MessageFlowOptions, error) {
	if _, present := args.values["path-prefix"]; present {
		return query.MessageFlowOptions{}, fmt.Errorf("--path-prefix applies only to list queries, not message-flow")
	}
	limit, err := intOption(args, "limit", query.DefaultCatalogLimit)
	if err != nil {
		return query.MessageFlowOptions{}, err
	}
	return query.MessageFlowOptions{Repository: args.values["repo-name"], Component: args.values["component"],
		Direction: query.Direction(args.values["direction"]), Limit: limit}, nil
}

func (a *App) messageFlow(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 1 {
		return fmt.Errorf("usage: grafo message-flow <message-or-id> [--repo-name name] [--component name] [--direction incoming|outgoing|both] [--limit 100] [--json]")
	}
	options, err := a.messageFlowOptions(args)
	if err != nil {
		return err
	}
	service, closeRepository, err := openMessageFlow(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	result, err := service.Flow(ctx, args.positionals[0], options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	a.printf("%-18s %-16s %s\n", result.Status, result.Message.Repository, result.Message.QualifiedName)
	for _, member := range result.Members {
		a.printf("  %-18s %-36s producers=%d consumers=%d", member.Status, member.Field.QualifiedName, len(member.Producers), len(member.Consumers))
		if member.Oneof != "" {
			a.printf(" oneof=%s", member.Oneof)
		}
		a.println()
	}
	for _, send := range result.Sends {
		a.printf("  send    channel=%-6s reliability=%-12s %s\n", send.Channel, send.Reliability, send.Evidence.Node.QualifiedName)
	}
	for _, receive := range result.Receives {
		a.printf("  receive channel=%-6s reliability=%-12s %s\n", receive.Channel, receive.Reliability, receive.Evidence.Node.QualifiedName)
	}
	a.printf("%d bindings · %d encoders · %d decoders · %d gaps", len(result.Bindings), len(result.Encoders), len(result.Decoders), len(result.Gaps))
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
	return nil
}

func (a *App) messageCoverage(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo message-coverage [--package name] [--message name] [--oneof name] [--direction incoming|outgoing|both] [--component name] [--status resolved|missing_evidence|unknown] [--repo-name name] [--path-prefix dir] [--limit 100] [--json]")
	}
	limit, err := intOption(args, "limit", query.DefaultCatalogLimit)
	if err != nil {
		return err
	}
	prefixes, err := pathPrefixOption(args)
	if err != nil {
		return err
	}
	service, closeRepository, err := openMessageFlow(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	result, err := service.Coverage(ctx, query.MessageCoverageOptions{Repository: args.values["repo-name"],
		Package: args.values["package"], Message: args.values["message"], Oneof: args.values["oneof"],
		Direction: query.Direction(args.values["direction"]), Component: args.values["component"],
		Status: query.CoverageStatus(args.values["status"]), PathPrefixes: prefixes, Limit: limit})
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, item := range result.Messages {
		a.printf("%-18s %-16s %-48s gaps=%d unknown=%d\n", item.Status, item.Message.Repository,
			item.Message.QualifiedName, len(item.Gaps), len(item.UnknownEvidence))
	}
	a.printf("%d messages", len(result.Messages))
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
	return nil
}

func (a *App) endpoints(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo endpoints [--method GET] [--route path] [--repo-name name] [--path-prefix dir] [--limit 100] [--json]")
	}
	options, err := a.topologyOptions(args)
	if err != nil {
		return err
	}
	service, closeRepository, err := openTopology(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	result, err := service.Endpoints(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, endpoint := range append(append([]query.Endpoint{}, result.Endpoints...), result.Unresolved...) {
		a.printf("%-8s %-36s %-12s %-20s %s\n", endpoint.Method, endpoint.Route,
			endpoint.HandlerStatus, endpoint.Repository, formatLocation(endpoint.Location))
		for _, handler := range endpoint.Handlers {
			a.printf("    handler      %-48s %s\n", handler.Node.QualifiedName, formatLocation(handler.Location))
		}
		for _, middleware := range endpoint.Middleware {
			a.printf("    middleware %3s %-42s %-8s %s\n", middleware.Evidence["order"],
				middleware.Node.QualifiedName, middleware.Evidence["form"], formatLocation(middleware.Location))
		}
		for _, middleware := range endpoint.UnresolvedMiddleware {
			a.printf("    middleware?%3s %-42s %-8s %s\n", middleware.Evidence["order"],
				middleware.Node.QualifiedName, middleware.Evidence["form"], formatLocation(middleware.Location))
		}
		if endpoint.MiddlewareTruncated {
			a.println("    middleware evidence truncated")
		}
	}
	a.printCatalogSummary(len(result.Endpoints), len(result.Unresolved), "endpoints", result.Truncated)
	return nil
}

func (a *App) outboundRequests(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo outbound-requests [--method GET] [--route path] [--repo-name name] [--path-prefix dir] [--limit 100] [--json]")
	}
	options, err := a.topologyOptions(args)
	if err != nil {
		return err
	}
	service, closeRepository, err := openTopology(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	result, err := service.OutboundRequests(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, request := range result.Requests {
		destination := request.Destination.Repository
		if request.Status == query.BoundaryAmbiguous {
			destination = fmt.Sprintf("%d candidates", len(request.Candidates))
		} else if destination == "" {
			destination = "external"
		}
		a.printf("%-12s %-8s %-36s %-20s -> %-20s %s\n", request.Status, request.Method,
			request.Route, request.Source.Repository, destination, formatLocation(request.Evidence.Location))
	}
	a.printf("%d outbound requests", len(result.Requests))
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
	return nil
}

func (a *App) findHandler(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo find-handler [--method GET] [--route path | --event name] [--repo-name name] [--limit 100] [--json]")
	}
	options, err := a.topologyOptions(args)
	if err != nil {
		return err
	}
	if _, present := args.values["path-prefix"]; present {
		return fmt.Errorf("--path-prefix applies only to list queries, not find-handler")
	}
	service, closeRepository, err := openTopology(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	result, err := service.Handlers(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, match := range result.Matches {
		name := match.Event
		if match.Kind == query.HandlerHTTP {
			name = match.Method + " " + match.Route
		}
		a.printf("%-8s %-12s %-48s\n", match.Kind, match.Status, name)
		for _, handler := range match.Handlers {
			a.printf("    handler      %-48s %s\n", handler.Node.QualifiedName, formatLocation(handler.Location))
		}
	}
	a.printf("%d handler matches", len(result.Matches))
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
	return nil
}

func (a *App) serviceTopology(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo service-topology [--repo-name name] [--component name] [--method GET] [--route path | --event name] [--direction incoming|outgoing|both] [--path-prefix dir] [--limit 100] [--json | --mermaid]")
	}
	if args.flags["json"] && args.flags["mermaid"] {
		return errors.New("--json and --mermaid are mutually exclusive")
	}
	options, err := a.topologyOptions(args)
	if err != nil {
		return err
	}
	service, closeRepository, err := openTopology(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	result, err := service.ServiceTopology(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	if args.flags["mermaid"] {
		a.print(query.RenderMermaid(result))
		return nil
	}
	for _, service := range result.Services {
		state := "service"
		if service.External {
			state = "external"
		}
		a.printf("%-10s %-32s %s\n", state, service.ID, service.Label)
	}
	for _, link := range result.Links {
		a.printf("%-8s %-12s %-28s -> %-28s %s\n", link.Kind, link.Status,
			link.FromServiceID, link.ToServiceID, link.Name)
	}
	a.printf("%d services · %d links", len(result.Services), len(result.Links))
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
	return nil
}

func (a *App) dataResources(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo data-resources [--kind table,view] [--name text] [--repo-name name] [--path-prefix dir] [--limit 100] [--json]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	kinds, err := parseNodeKinds(args.values["kind"])
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
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
		return fmt.Errorf("usage: grafo data-usage <table-view-or-id> [--repo-name name] [--limit 100] [--json]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	if _, present := args.values["path-prefix"]; present {
		return fmt.Errorf("--path-prefix applies only to list queries, not data-usage")
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
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
		a.println("… truncated")
	}
	return nil
}

func (a *App) configKeys(ctx context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo config-keys [--name text] [--repo-name name] [--path-prefix dir] [--limit 100] [--json]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
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
		return fmt.Errorf("usage: grafo events [--name text] [--repo-name name] [--path-prefix dir] [--limit 100] [--json]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
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
		return fmt.Errorf("usage: grafo orphaned-events [--name text] [--repo-name name] [--path-prefix dir] [--limit 100] [--json]")
	}
	options, err := a.catalogOptions(args)
	if err != nil {
		return err
	}
	catalog, closeRepository, err := openCatalog(ctx, args)
	if err != nil {
		return err
	}
	defer func() { _ = closeRepository() }()
	result, err := catalog.OrphanedEvents(ctx, options)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, result)
	}
	for _, orphan := range result.Events {
		a.printf("%-12s  %-10s  %-48s  %s\n", orphan.Category, orphan.Status,
			orphan.Event.QualifiedName, formatLocation(orphan.Event.Location))
		if orphan.UnresolvedProducers > 0 || orphan.UnresolvedConsumers > 0 {
			a.printf("    %d unresolved producers · %d unresolved consumers\n",
				orphan.UnresolvedProducers, orphan.UnresolvedConsumers)
		}
	}
	a.printf("%d events", len(result.Events))
	if result.Truncated {
		a.print(" · truncated")
	}
	a.println()
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
	a.printf("%-12s  %-20s  %-48s  %s\n", resource.Kind, state,
		resource.QualifiedName, formatLocation(resource.Location))
}

func (a *App) printUsage(label string, sites []query.UsageSite) {
	for _, site := range sites {
		a.printf("    %-12s %-48s %s\n", label, site.Node.QualifiedName, formatLocation(site.Location))
	}
}

func (a *App) printCatalogSummary(declared, unresolved int, noun string, truncated bool) {
	a.printf("%d %s · %d unresolved", declared, noun, unresolved)
	if truncated {
		a.print(" · truncated")
	}
	a.println()
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

// openExisting opens an existing branch index read-write and refreshes it. The
// refresh is a real write, and migrations run inside sqlite.Open, so the index
// lock is taken for the whole span: query commands used to race each other as
// concurrent writers and fail on SQLITE_BUSY or a half-applied migration
// instead of serializing. The returned unlock is released by the caller after
// the repository is closed, never before.
func openExisting(ctx context.Context, root string) (indexer.Project, graph.Repository, service.Unlock, error) {
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		return project, nil, nil, err
	}
	if _, err := os.Stat(project.IndexPath); errors.Is(err, os.ErrNotExist) {
		return project, nil, nil, fmt.Errorf("branch %q has no index; run 'grafo index %s'", project.Branch, project.Root)
	} else if err != nil {
		return project, nil, nil, err
	}
	unlock, err := acquireIndexLock(project.IndexPath)
	if err != nil {
		return project, nil, nil, err
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		_ = unlock()
		return project, nil, nil, err
	}
	if _, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{ReportDetail: indexer.ReportWithoutCounts}); err != nil {
		_ = repository.Close()
		_ = unlock()
		return project, nil, nil, fmt.Errorf("refresh index: %w", err)
	}
	return project, repository, unlock, nil
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
			_ = repository.Close()
			return nil, nil, nil, err
		}
		projects := repository.Projects()
		if requiresWritableRead(args.command) {
			return repository, projects, repository.Close, nil
		}
		if err := repository.Close(); err != nil {
			return nil, nil, nil, fmt.Errorf("close refreshed indexes: %w", err)
		}
		readRepository, err := federation.OpenReadOnly(ctx, paths)
		if err != nil {
			return nil, nil, nil, err
		}
		return readRepository, projects, readRepository.Close, nil
	}
	project, repository, unlock, err := openExisting(ctx, repoPath(args))
	if err != nil {
		return nil, nil, nil, err
	}
	if requiresWritableRead(args.command) {
		return repository, []indexer.Project{project}, func() error {
			return errors.Join(repository.Close(), unlock())
		}, nil
	}
	if err := repository.Close(); err != nil {
		_ = unlock()
		return nil, nil, nil, fmt.Errorf("close refreshed index: %w", err)
	}
	// The read-only handle is opened while the lock still stands, so it can
	// never observe the schema of a migration another process is mid-way
	// through applying.
	readRepository, err := sqlite.OpenReadOnly(ctx, project.IndexPath)
	if err != nil {
		_ = unlock()
		return nil, nil, nil, err
	}
	if err := unlock(); err != nil {
		return nil, nil, nil, errors.Join(err, readRepository.Close())
	}
	return readRepository, []indexer.Project{project}, readRepository.Close, nil
}

// openStatusRead keeps refresh reporting on the status/counts path while all
// other query commands retain their existing open/refresh behavior.
func openStatusRead(ctx context.Context, args parsedArguments, observer indexer.ProgressObserver) (graph.ReadRepository, []indexer.Project, []indexer.Report, func() error, error) {
	if args.values["repo"] != "" && args.values["repos"] != "" {
		return nil, nil, nil, nil, fmt.Errorf("--repo and --repos cannot be used together")
	}
	if raw := args.values["repos"]; raw != "" {
		paths := strings.Split(raw, ",")
		repository, err := federation.Open(ctx, paths)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		reports, refreshErr := repository.RefreshReports(ctx, parserdefaults.NewRegistry(), indexer.Options{
			ProgressObserver: observer, ReportDetail: indexer.ReportWithoutCounts,
		})
		projects := repository.Projects()
		closeErr := repository.Close()
		if refreshErr != nil {
			return nil, nil, nil, nil, errors.Join(refreshErr, closeErr)
		}
		if closeErr != nil {
			return nil, nil, nil, nil, fmt.Errorf("close refreshed indexes: %w", closeErr)
		}
		readRepository, err := federation.OpenReadOnly(ctx, paths)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return readRepository, projects, reports, readRepository.Close, nil
	}
	project, err := indexer.DiscoverProject(ctx, repoPath(args))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if _, err := os.Stat(project.IndexPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, nil, fmt.Errorf("branch %q has no index; run 'grafo index %s'", project.Branch, project.Root)
	} else if err != nil {
		return nil, nil, nil, nil, err
	}
	// Status refreshes the index exactly like every other query command, so it
	// serializes behind the same lock instead of racing as a second writer.
	unlock, err := acquireIndexLock(project.IndexPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		_ = unlock()
		return nil, nil, nil, nil, err
	}
	report, refreshErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{
		ProgressObserver: observer, ReportDetail: indexer.ReportWithoutCounts,
	})
	closeErr := repository.Close()
	if refreshErr != nil {
		return nil, nil, nil, nil, errors.Join(fmt.Errorf("refresh index: %w", refreshErr), closeErr, unlock())
	}
	if closeErr != nil {
		return nil, nil, nil, nil, errors.Join(fmt.Errorf("close refreshed index: %w", closeErr), unlock())
	}
	readRepository, err := sqlite.OpenReadOnly(ctx, project.IndexPath)
	if err != nil {
		return nil, nil, nil, nil, errors.Join(err, unlock())
	}
	if err := unlock(); err != nil {
		return nil, nil, nil, nil, errors.Join(err, readRepository.Close())
	}
	return readRepository, []indexer.Project{project}, []indexer.Report{report}, readRepository.Close, nil
}

func requiresWritableRead(command string) bool {
	return false
}

func (a *App) printIndexReport(report indexer.Report, asJSON, countsRequested bool) error {
	if asJSON {
		return writeJSON(a.stdout, report)
	}
	a.printf("indexed %s · branch %s\n", report.Project.Name, report.Project.Branch)
	a.printf("%d updated · %d unchanged · %d removed · %d skipped · %d scoped out\n",
		len(report.Updated), report.Unchanged, len(report.Removed), len(report.Skipped), report.ScopedOut)
	a.printf("%d file contents checked\n", report.Checked)
	a.printf("edge reconciliation: %dms\n", report.ReconcileMS)
	if report.Rebuild != "" {
		a.printf("rebuild: %s\n", report.Rebuild)
	}
	switch {
	case report.CountsCollected:
		a.printf("%d files · %d nodes · %d edges · %d unresolved · %dms\n",
			report.Counts.Files, report.Counts.Nodes, report.Counts.Edges, report.Counts.External, report.ElapsedMS)
	case countsRequested:
		// The run itself succeeded, so the requested summary is reported as
		// unavailable and the cause is carried by the run's diagnostics.
		a.printf("%dms · full-graph counts unavailable (see diagnostics)\n", report.ElapsedMS)
	default:
		a.printf("%dms · full-graph counts not collected (add --counts)\n", report.ElapsedMS)
	}
	for _, diagnostic := range report.Diagnostics {
		a.errorf("%s:%d: %s: %s\n", diagnostic.Path, diagnostic.Line, diagnostic.Level, diagnostic.Message)
	}
	return nil
}

// printInterruptedIndexReport reports what an interrupted run had already
// measured. A cold index on a large repository runs for minutes, so a CI
// timeout or a Ctrl-C used to discard the only phase attribution that explains
// where the time went.
func (a *App) printInterruptedIndexReport(report indexer.Report, asJSON bool) {
	if asJSON {
		_ = writeJSON(a.stdout, report)
		return
	}
	phases := report.Phases
	a.printf("interrupted index of %s · branch %s · %dms\n", report.Project.Name, report.Project.Branch, report.ElapsedMS)
	a.printf("git probe %s · membership %s · change probe %s · discovery %s\n",
		formatPhaseDuration(phases.GitProbeNS), formatPhaseDuration(phases.MembershipNS),
		formatPhaseDuration(phases.ChangeProbeNS), formatPhaseDuration(phases.DiscoveryNS))
	a.printf("read+hash %s · parse %s · persistence %s · reconciliation %s\n",
		formatPhaseDuration(phases.ReadHashNS), formatPhaseDuration(phases.ParseNS),
		formatPhaseDuration(phases.PersistenceNS), formatPhaseDuration(phases.ReconciliationNS))
	if phases.SemanticNS > 0 {
		// Printed as a share of parse rather than beside it, because it is part
		// of the parse figure above and not an additional phase.
		a.printf("  of which semantic load %s · view derivation %s\n",
			formatPhaseDuration(phases.SemanticNS), formatPhaseDuration(phases.SemanticDerivationNS))
	}
	a.printf("%d updated · %d unchanged · %d removed · %d file contents checked\n",
		len(report.Updated), report.Unchanged, len(report.Removed), report.Checked)
}

func formatPhaseDuration(nanoseconds int64) string {
	return fmt.Sprintf("%dms", time.Duration(nanoseconds).Milliseconds())
}

func (a *App) printNodes(nodes []graph.Node) {
	for _, node := range nodes {
		a.printf("%-12s  %-48s  %-24s  %s\n", node.Kind, node.QualifiedName, formatLocation(node.Location), node.ID)
	}
}

func (a *App) fail(err error) { a.errorf("grafo: %v\n", err) }

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

var booleanOptions = map[string]bool{
	"json": true, "force": true, "help": true, "source": true, "regex": true, "case-sensitive": true,
	"list": true, "all": true, "dry-run": true, "mcp-only": true, "hooks": true, "refresh": true,
	"repair": true, "once": true, "paused": true, "mermaid": true, "yes": true,
	"counts": true, "no-seed": true, "check": true,
}
var valueOptions = map[string]bool{
	"repo": true, "repos": true, "depth": true, "direction": true, "relation": true,
	"limit": true, "interval": true, "max-file-size": true, "model": true,
	"ollama-url": true, "batch-size": true, "context-lines": true, "max-lines": true,
	"upstream-depth": true, "downstream-depth": true, "upstream-limit": true,
	"downstream-limit": true, "source-limit": true, "path-prefix": true, "language": true,
	"repo-name": true, "max-matches": true, "max-matches-per-file": true,
	"max-matches-per-pattern": true, "client": true, "hook": true,
	"kind": true, "name": true, "state-dir": true, "lines": true, "concurrency": true,
	"filter": true, "method": true, "route": true, "event": true, "component": true,
	"package": true, "message": true, "oneof": true, "status": true,
	"progress": true, "older-than": true, "max-bytes": true, "keep": true,
	"budget": true,
}

// progressCommands bounds the globally parsed --progress option to the
// commands that render it, so every other command still rejects the flag
// before opening a repository.
var progressCommands = map[string]bool{"index": true, "status": true, "counts": true}

// seedCommands bounds --no-seed to the commands that create a branch index, so
// every other command still rejects the flag before opening a repository.
var seedCommands = map[string]bool{"index": true, "watch": true}

// pathPrefixCommands is the adapter boundary for the one globally parsed
// option that is intentionally available to only a bounded command set. Keep
// aliases here so unsupported commands fail before opening any repository.
var pathPrefixCommands = map[string]bool{
	"search":          true,
	"data-resources":  true,
	"config-keys":     true,
	"events":          true,
	"orphaned-events": true,
	"endpoints":       true, "list-endpoints": true, "list_endpoints": true,
	"outbound-requests": true, "list-outbound-requests": true, "list_outbound_requests": true,
	"service-topology": true, "get-service-topology": true, "get_service_topology": true,
	"message-coverage": true, "list-message-coverage": true, "list_message_coverage": true,
	"reusable": true, "find-reusable-code": true,
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
		if previous, present := result.values[name]; name == "path-prefix" && present {
			result.values[name] = previous + "," + value
		} else {
			result.values[name] = value
		}
	}
	if result.values["progress"] != "" && !progressCommands[result.command] {
		return result, fmt.Errorf("--progress is only supported by index, status, and counts")
	}
	if progressCommands[result.command] {
		if _, err := parseProgressMode(result.values["progress"]); err != nil {
			return result, err
		}
	}
	return result, nil
}

// nodeKindOption reads the optional --kind filter shared by every command that
// takes a selector. An unknown kind fails here rather than resolving to a filter
// that can never match.
func nodeKindOption(args parsedArguments) (graph.NodeKind, error) {
	return graph.ParseNodeKind(args.values["kind"])
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
  grafo install [client...] [--client a,b] [--all] [--list] [--dry-run] [--json]
                 [--mcp-only] [--hooks] [--refresh]
  grafo uninstall [client...] [--client a,b] [--all] [--dry-run] [--json]
  grafo guidance [--repo path] [--hook pre-search|pre-edit]
  grafo index [path] [--force] [--counts] [--no-seed] [--json] [--progress auto|human|json|off]
  grafo indexes list [path] [--json]
  grafo indexes prune [path] [--older-than duration] [--keep n] [--dry-run] [--yes] [--json]
  grafo indexes compact [path] [--dry-run] [--yes] [--json]
  grafo watch [path] [--interval 1s] [--counts] [--no-seed] [--json]
  grafo service add [path] [--interval 10s] [--paused] [--json]
  grafo service remove [path]
  grafo service list [--json]
  grafo service install [--dry-run] [--json]
  grafo service status [--json]
  grafo service logs [--lines 50] [--json]
  grafo service uninstall [--dry-run] [--json]
  grafo service run [--once] [--state-dir dir] [--concurrency n] [--json]
  grafo doctor [--repair] [--json]
  grafo status [path] [--repos pathA,pathB] [--json] [--progress auto|human|json|off]
  grafo mcp [--repo path | --repos pathA,pathB] [--model embeddinggemma]
  grafo embed [path] [--model embeddinggemma] [--ollama-url http://localhost:11434] [--force]
  grafo embed-cache status [--json]
  grafo embed-cache prune [--model name] [--older-than duration] [--max-bytes n] [--dry-run] [--yes] [--json]
  grafo reusable <description> [--repo path | --repos pathA,pathB] [--limit 5] [--budget 45s] [--language name] [--path-prefix dir] [--json]
  grafo find <text> [--limit 20] [--repo path | --repos pathA,pathB] [--json]
  grafo show <symbol-or-id> [--kind function] [--repo path | --repos pathA,pathB] [--json]
  grafo source <symbol-or-id> [--kind function] [--context-lines 2] [--max-lines 200] [--json]
  grafo neighbors <symbol-or-id> [--kind function] [--depth 1] [--direction both]
  grafo callers <symbol-or-id> [--kind function] [--depth 3]
  grafo callees <symbol-or-id> [--kind function] [--depth 3]
  grafo impact <symbol-or-id> [--kind method] [--depth 4] [--upstream-depth n] [--downstream-depth n]
                              [--upstream-limit n] [--downstream-limit n]
                              [--source] [--context-lines 2] [--max-lines 200] [--source-limit 10]
  grafo failure-flow <function-method-error-or-id> [--kind function] [--direction both] [--limit 1000]
  grafo godot composition <scene-resource-script-or-autoload> [--kind godot_scene]
                          [--depth 8] [--limit 1000]
  grafo godot interactions <scene-node-script-action-group-or-signal>
                          [--filter action,group,signal] [--direction both]
                          [--kind godot_scene] [--depth 8] [--limit 1000]
  grafo search <pattern>... [--regex] [--case-sensitive] [--path-prefix dir,...]
                            [--language go,...] [--repo-name name,...] [--context-lines 0]
                            [--max-matches 500] [--max-matches-per-file 50]
                            [--max-matches-per-pattern 200] [--max-file-size 1048576]
  grafo path <from> <to> [--kind function] [--direction outgoing] [--relation calls,...]
  grafo data-resources [--kind table,view] [--name text] [--repo-name name]
                       [--path-prefix dir,...] [--limit 100] [--json]
  grafo data-usage <table-view-or-id> [--repo-name name] [--limit 100] [--json]
  grafo config-keys [--name text] [--repo-name name] [--path-prefix dir,...] [--limit 100] [--json]
  grafo events [--name text] [--repo-name name] [--path-prefix dir,...] [--limit 100] [--json]
  grafo orphaned-events [--name text] [--repo-name name] [--path-prefix dir,...] [--limit 100] [--json]
  grafo endpoints [--method GET] [--route path] [--repo-name name] [--path-prefix dir,...] [--limit 100] [--json]
  grafo outbound-requests [--method GET] [--route path] [--repo-name name] [--path-prefix dir,...] [--limit 100] [--json]
  grafo find-handler [--method GET] [--route path | --event name] [--repo-name name]
                     [--limit 100] [--json]
  grafo service-topology [--repo-name name] [--component name] [--method GET] [--route path | --event name]
                         [--direction incoming|outgoing|both] [--path-prefix dir,...] [--limit 100]
                         [--json | --mermaid]
  grafo message-flow <message-or-id> [--repo-name name] [--component name]
                     [--direction incoming|outgoing|both] [--limit 100] [--json]
  grafo message-coverage [--package name] [--message name] [--oneof name]
                         [--direction incoming|outgoing|both] [--component name]
                         [--status resolved|missing_evidence|unknown] [--repo-name name]
                         [--path-prefix dir,...] [--limit 100] [--json]
  grafo find-tests <production-symbol-or-id> [--kind function] [--depth 8] [--limit 100] [--json]
  grafo test-coverage <test-or-id> [--depth 8] [--limit 100] [--json]
  grafo version [--check]

Options may appear before or after positional arguments. All query commands
accept --repo or a comma-separated --repos list. Commands that take a selector
accept --kind to restrict resolution to one node kind, so a selector shared by a
function and its own parameter resolves without guessing. Active branch indexes are
refreshed incrementally before queries and never substituted across branches.
Test reports are bounded structural call/reference evidence, not runtime coverage.

'grafo index' and 'grafo watch' report what the run changed. Full-graph counts
cost a scan of every node, fact, and edge regardless of what changed, so
'--counts' requests them; without it the report says counts were not collected
rather than reporting zero totals. 'grafo status' always reports the full counts
summary.

'grafo index' renders phase progress on stderr so stdout stays machine-readable;
'--progress auto' is silent unless stderr is a terminal. An interrupted index
reports the phase timings it had already measured on stdout and exits nonzero, so
a cancelled run still explains where its time went. The human form is labelled
'interrupted index of'; the '--json' form is a partial report, distinguished only
by the exit status, because an interrupted run never reaches the counts query and
so reports 'counts_collected' false even under '--counts'.

'grafo indexes list' inventories the physical database, WAL, and SHM footprint
of every branch index for one repository. 'indexes prune' requires a retention
selector and either '--dry-run' or '--yes'; the current index and any index whose
identity, timestamp, compatibility, or lock cannot be verified are protected.
'grafo indexes compact' reports or reclaims freelist pages in only the current
branch index; mutation requires '--yes' and exclusive index maintenance access.

'grafo install --list' only detects clients and never writes; '--dry-run'
reports every file and command a real run would touch. 'grafo install' also installs
Grafo's agent guidance as an isolated skill file or a delimited managed block and
reports every target before mutating anything; '--mcp-only' registers the server
alone, '--refresh' updates only artifacts that already exist, and '--hooks' opts
in to advisory pre-search and pre-edit hooks for clients that document a safe
hook API. 'grafo uninstall' removes only Grafo's own registration, skill, managed
block, and hooks, and leaves anything whose ownership it cannot prove.

'grafo service' keeps every registered repository root indexed without a
foreground terminal: 'add'/'remove'/'list' own the user-level registry of watched
roots, and 'install'/'uninstall' generate and remove a macOS launchd agent or a
Linux user systemd unit that runs 'grafo service run'. Exactly one supervisor
runs at a time, every indexing run holds the branch index's lock, and a killed
supervisor leaves committed indexes intact. A definition Grafo cannot prove it
wrote is reported, never replaced. 'grafo watch' remains available everywhere.

'grafo doctor' reports the binary, the registry, every root's branch and index,
the service, the supervisor, and the agent registrations, and mutates nothing.
'--repair' performs only four documented repairs: unregister a definitively
missing, unshared root; refresh Grafo-owned agent registrations and guidance;
recreate a service definition Grafo installed; and restart a stale service.

'grafo guidance' prints that canonical guidance plus the index status of the
current repository and branch; '--hook' prints one advisory hint and always exits
successfully, so a client hook can never block a tool call.

Commands that answer questions about one engine or language live under a parent
command named after it, so 'grafo godot composition' names both the engine and
the question, and another toolchain's commands group the same way.

'grafo godot composition' reports Godot runtime composition for one scene,
scene node, resource, script, or autoload: outbound and inbound scene
instances, attached scripts, and autoload availability, each with the resource
evidence that produced it.

'grafo godot interactions' reports Godot gameplay wiring for one scene, scene
node, script symbol, input action, node group, or signal: the input actions it
uses, the node groups it joins, inspects, and dispatches to, and the signal
routes it takes part in, whether a scene declared them or a script established
them. --filter narrows to action, group, or signal and --direction to one side;
an unresolved action, group, or signal stays in the report and is counted, so
missing wiring is visible rather than absent.

'grafo impact' reports both directions: what depends on the symbol and what it
depends on, plus impacted files, cross-repository hops, and config, data, and
event relationships. 'grafo failure-flow' separates typed error-return
declarations, escaping and wrapped errors, handlers, panics and recoveries,
and deferred cleanup while keeping conditional and unresolved evidence explicit.
'grafo embed-cache status' inspects the user-level content-addressed vector
cache without contacting the provider. 'grafo embed-cache prune' evicts the
oldest rows matching every supplied filter; dry runs are read-only and actual
deletion requires --yes. Pruning checkpoints but never vacuums the cache.
'grafo search' reads only files that belong to a refreshed
index and never persists source text.

The catalog commands accept --repo-name to restrict results to one indexed
repository, and report truncation whenever a bound is reached. A --name fragment
is matched literally and is trimmed, so a blank one narrows nothing.

The endpoint and topology commands use indexed component ownership when present
and retain each indexed repository as the fallback service boundary for
unassigned files. --component selects that exact component name across the
selected repositories. HTTP method matching is exact, route filters use
canonical template compatibility, event filters are literal fragments, and
unresolved or ambiguous destinations remain explicit.
Service-topology JSON contains the endpoint/event node IDs and edge evidence;
--mermaid renders that same result without replacing the structured evidence.
`
