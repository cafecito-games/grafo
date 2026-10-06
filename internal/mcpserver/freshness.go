package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

const defaultFreshnessAttempts = 3

var ErrContinuouslyChangingSource = errors.New("source changed during all freshness refresh attempts")

// FreshnessCoordinatorOptions controls bounded freshness work. The zero value
// uses the indexer's production file bound and three stability attempts.
type FreshnessCoordinatorOptions struct {
	MaxFileSize int64
	MaxAttempts int
	// DeferStartupRefresh publishes a provisional generation from the indexes
	// already on disk instead of refreshing before returning. The transport can
	// then answer `initialize` immediately, and the first Acquire performs the
	// refresh. Capability-conditional tool registration still sees a real
	// repository, so the advertised tool list does not depend on refresh
	// timing. See issue #173.
	DeferStartupRefresh bool
}

// FreshnessDiagnostics is retained for debug and benchmark inspection. It is
// deliberately not included in normal MCP tool payloads.
type FreshnessDiagnostics struct {
	Attempts    int
	Probes      int
	GitCommands int
	Refreshed   []string
	Skipped     []string
	Fallbacks   map[string]string
	Reports     []indexer.Report
	Elapsed     time.Duration
}

// FreshnessGeneration is an immutable, query-only repository generation.
type FreshnessGeneration struct {
	Key         string
	Repository  graph.ReadRepository
	Projects    []indexer.Project
	Diagnostics FreshnessDiagnostics
	tokens      map[string]indexer.FreshnessToken
	close       func() error
}

type freshnessOperations struct {
	probe   func(context.Context, []string) ([]indexer.FreshnessProbe, error)
	refresh func(context.Context, indexer.FreshnessProbe) (indexer.Report, error)
	open    func(context.Context, []indexer.FreshnessProbe) (graph.ReadRepository, func() error, error)
}

type freshnessFlight struct {
	done chan struct{}
	err  error
}

// FreshnessCoordinator owns source probes, writer refreshes, publication, and
// complete-handler read leases for one MCP session.
type FreshnessCoordinator struct {
	mu         sync.Mutex
	roots      []string
	operations freshnessOperations
	attempts   int
	current    *FreshnessGeneration
	flight     *freshnessFlight
	leases     int
	drained    chan struct{}
	closed     bool
}

// NewFreshnessCoordinator performs the one required startup refresh and opens
// the first validated query-only generation before returning.
func NewFreshnessCoordinator(ctx context.Context, roots []string, registry *parserapi.Registry, options FreshnessCoordinatorOptions) (*FreshnessCoordinator, *FreshnessGeneration, error) {
	if len(roots) == 0 {
		return nil, nil, fmt.Errorf("freshness coordinator requires at least one project root")
	}
	if registry == nil {
		return nil, nil, fmt.Errorf("freshness coordinator parser registry is required")
	}
	attempts := options.MaxAttempts
	if attempts <= 0 {
		attempts = defaultFreshnessAttempts
	}
	probeOptions := indexer.FreshnessOptions{MaxFileSize: options.MaxFileSize}
	var probeCacheMu sync.Mutex
	probeCache := map[string]indexer.FreshnessProbe{}
	operations := freshnessOperations{
		probe: func(probeContext context.Context, starts []string) ([]indexer.FreshnessProbe, error) {
			result := make([]indexer.FreshnessProbe, 0, len(starts))
			for _, start := range starts {
				probeCacheMu.Lock()
				previous, seen := probeCache[start]
				probeCacheMu.Unlock()
				var probe indexer.FreshnessProbe
				var err error
				if seen {
					probe, err = indexer.ReprobeFreshness(probeContext, previous, registry, probeOptions)
				} else {
					probe, err = indexer.ProbeFreshness(probeContext, start, registry, probeOptions)
				}
				if err != nil {
					return nil, fmt.Errorf("probe freshness for %s: %w", start, err)
				}
				probeCacheMu.Lock()
				probeCache[start] = probe
				probeCacheMu.Unlock()
				result = append(result, probe)
			}
			return canonicalFreshnessProbes(result)
		},
		refresh: func(refreshContext context.Context, probe indexer.FreshnessProbe) (indexer.Report, error) {
			repository, err := sqlite.Open(refreshContext, probe.Project.IndexPath)
			if err != nil {
				return indexer.Report{}, fmt.Errorf("open writer for %s: %w", probe.Project.Root, err)
			}
			report, refreshErr := indexer.NewService(repository, registry).Run(refreshContext, probe.Project, indexer.Options{
				MaxFileSize: options.MaxFileSize, ReportDetail: indexer.ReportWithoutCounts, Token: probe.Token,
			})
			closeErr := repository.Close()
			if refreshErr != nil || closeErr != nil {
				var refreshFailure, closeFailure error
				if refreshErr != nil {
					refreshFailure = fmt.Errorf("refresh %s: %w", probe.Project.Root, refreshErr)
				}
				if closeErr != nil {
					closeFailure = fmt.Errorf("close writer for %s: %w", probe.Project.Root, closeErr)
				}
				return report, errors.Join(refreshFailure, closeFailure)
			}
			return report, nil
		},
		open: openFreshnessGeneration,
	}
	coordinator := newFreshnessCoordinator(roots, attempts, operations)
	if options.DeferStartupRefresh {
		generation, err := coordinator.publishProvisional(ctx)
		if err == nil {
			return coordinator, generation, nil
		}
		if !errors.Is(err, errNoProvisionalIndex) {
			_ = coordinator.Close()
			return nil, nil, err
		}
		// Nothing on disk can answer queries yet, so there is no generation to
		// publish and startup must index before serving. This keeps the index
		// compatibility contract intact at the cost of a slow first start.
	}
	generation, release, err := coordinator.Acquire(ctx)
	if err != nil {
		_ = coordinator.Close()
		return nil, nil, err
	}
	release()
	return coordinator, generation, nil
}

// publishProvisional binds a query-only generation from the indexes already on
// disk without running the indexer. The generation carries no freshness tokens,
// so the first Acquire treats every project as changed and refreshes it; the
// existing drain path closes this repository when the refreshed generation is
// published.
func (c *FreshnessCoordinator) publishProvisional(ctx context.Context) (*FreshnessGeneration, error) {
	projects := make([]indexer.Project, 0, len(c.roots))
	for _, root := range c.roots {
		project, err := indexer.DiscoverProject(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("discover project for %s: %w", root, err)
		}
		projects = append(projects, project)
	}
	projects, err := canonicalProvisionalProjects(projects)
	if err != nil {
		return nil, err
	}
	repository, closeRepository, err := openProvisionalGeneration(ctx, projects)
	if err != nil {
		return nil, err
	}
	generation := &FreshnessGeneration{
		Key:        provisionalGenerationKey(projects),
		Repository: repository,
		Projects:   projects,
		// An empty, non-nil token map never equals a probed project set, so
		// changedFreshnessProbes reports every project as changed.
		tokens: map[string]indexer.FreshnessToken{},
		close:  closeRepository,
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = closeRepository()
		return nil, errors.New("freshness coordinator is closed")
	}
	c.current = generation
	c.mu.Unlock()
	return generation, nil
}

func canonicalProvisionalProjects(projects []indexer.Project) ([]indexer.Project, error) {
	sort.Slice(projects, func(i, j int) bool { return projects[i].Root < projects[j].Root })
	seenRoots := map[string]bool{}
	seenIndexes := map[string]bool{}
	result := projects[:0]
	for _, project := range projects {
		if seenRoots[project.Root] {
			continue
		}
		seenRoots[project.Root] = true
		if seenIndexes[project.IndexPath] {
			return nil, fmt.Errorf("projects %q share freshness index %s", project.Root, project.IndexPath)
		}
		seenIndexes[project.IndexPath] = true
		result = append(result, project)
	}
	return result, nil
}

// errNoProvisionalIndex reports that no existing index can back a provisional
// generation, so the caller must index before serving.
var errNoProvisionalIndex = errors.New("no readable index for a provisional generation")

// openProvisionalGeneration opens the on-disk indexes read-only. Index metadata
// is deliberately not validated against the discovered project: a stale index is
// the expected input here, and the refresh on first Acquire reconciles it. A
// missing or incompatible index yields errNoProvisionalIndex rather than a
// fabricated one, because an index that cannot be opened cannot answer queries.
func openProvisionalGeneration(ctx context.Context, projects []indexer.Project) (graph.ReadRepository, func() error, error) {
	for _, project := range projects {
		if _, err := os.Stat(project.IndexPath); err != nil {
			return nil, nil, fmt.Errorf("%w: %s", errNoProvisionalIndex, project.Root)
		}
	}
	if len(projects) == 1 {
		repository, err := sqlite.OpenReadOnly(ctx, projects[0].IndexPath)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: open %s: %v", errNoProvisionalIndex, projects[0].Root, err)
		}
		return repository, repository.Close, nil
	}
	repository, err := federation.OpenReadOnlyProjects(ctx, projects)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: open federated projects: %v", errNoProvisionalIndex, err)
	}
	return repository, repository.Close, nil
}

func provisionalGenerationKey(projects []indexer.Project) string {
	parts := []string{"provisional"}
	for _, project := range projects {
		parts = append(parts, projectGenerationKey(project))
	}
	return strings.Join(parts, "\x00")
}

func newFreshnessCoordinator(roots []string, attempts int, operations freshnessOperations) *FreshnessCoordinator {
	return &FreshnessCoordinator{roots: append([]string(nil), roots...), attempts: attempts, operations: operations}
}

// Acquire proves source freshness and returns a lease valid until release. The
// release function must be called exactly once after the complete tool result
// has been constructed.
func (c *FreshnessCoordinator) Acquire(ctx context.Context) (*FreshnessGeneration, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, nil, errors.New("freshness coordinator is closed")
		}
		if active := c.flight; active != nil {
			done := active.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-done:
				if active.err != nil {
					return nil, nil, active.err
				}
				continue // Re-probe: this caller may have observed a later candidate.
			}
		}
		flight := &freshnessFlight{done: make(chan struct{})}
		c.flight = flight
		c.mu.Unlock()

		generation, err := c.runFlight(ctx)
		c.mu.Lock()
		flight.err = err
		c.flight = nil
		if err == nil {
			c.leases++
			if c.leases == 1 {
				c.drained = make(chan struct{})
			}
		}
		close(flight.done)
		c.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		var once sync.Once
		return generation, func() { once.Do(c.release) }, nil
	}
}

func (c *FreshnessCoordinator) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leases == 0 {
		return
	}
	c.leases--
	if c.leases == 0 && c.drained != nil {
		close(c.drained)
		c.drained = nil
	}
}

func (c *FreshnessCoordinator) runFlight(ctx context.Context) (*FreshnessGeneration, error) {
	started := time.Now()
	diagnostics := FreshnessDiagnostics{Fallbacks: map[string]string{}}
	for attempt := 1; attempt <= c.attempts; attempt++ {
		diagnostics.Attempts = attempt
		before, err := c.operations.probe(ctx, c.roots)
		diagnostics.Probes++
		if err != nil {
			return nil, err
		}
		for _, probe := range before {
			diagnostics.GitCommands += probe.GitCommands
			if !probe.Supported {
				diagnostics.Fallbacks[probe.Project.Root] = probe.Fallback
			}
		}
		c.mu.Lock()
		current := c.current
		c.mu.Unlock()
		changed := changedFreshnessProbes(current, before)
		if len(changed) == 0 {
			diagnostics.Elapsed = time.Since(started)
			return current, nil
		}
		if current != nil {
			if err := c.drainCurrent(ctx, current); err != nil {
				return nil, err
			}
		}
		for _, probe := range changed {
			report, refreshErr := c.operations.refresh(ctx, probe)
			diagnostics.Reports = append(diagnostics.Reports, report)
			if refreshErr != nil {
				return nil, refreshErr
			}
			diagnostics.Refreshed = append(diagnostics.Refreshed, probe.Project.Root)
		}
		for _, probe := range before {
			if !probeIn(probe, changed) {
				diagnostics.Skipped = append(diagnostics.Skipped, probe.Project.Root)
			}
		}
		after, err := c.operations.probe(ctx, c.roots)
		diagnostics.Probes++
		if err != nil {
			return nil, err
		}
		for _, probe := range after {
			diagnostics.GitCommands += probe.GitCommands
		}
		if !stableFreshnessProbes(before, after) {
			continue
		}
		repository, closeRepository, err := c.operations.open(ctx, after)
		if err != nil {
			return nil, err
		}
		generation := &FreshnessGeneration{
			Key: generationKey(after), Repository: repository, Projects: projectsFromProbes(after),
			Diagnostics: diagnostics, tokens: tokensFromProbes(after), close: closeRepository,
		}
		generation.Diagnostics.Elapsed = time.Since(started)
		c.mu.Lock()
		if c.closed || ctx.Err() != nil {
			c.mu.Unlock()
			_ = closeRepository()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("freshness coordinator is closed")
		}
		c.current = generation
		c.mu.Unlock()
		return generation, nil
	}
	return nil, ErrContinuouslyChangingSource
}

func (c *FreshnessCoordinator) drainCurrent(ctx context.Context, expected *FreshnessGeneration) error {
	c.mu.Lock()
	if c.current != expected {
		c.mu.Unlock()
		return nil
	}
	drained := c.drained
	c.mu.Unlock()
	if drained != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-drained:
		}
	}
	c.mu.Lock()
	if c.current != expected {
		c.mu.Unlock()
		return nil
	}
	c.current = nil
	c.mu.Unlock()
	if expected.close != nil {
		return expected.close()
	}
	return nil
}

// Close drains active leases and closes the published query-only generation.
func (c *FreshnessCoordinator) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	flight := c.flight
	c.mu.Unlock()
	if flight != nil {
		<-flight.done
	}
	c.mu.Lock()
	drained := c.drained
	current := c.current
	c.current = nil
	c.mu.Unlock()
	if drained != nil {
		<-drained
	}
	if current != nil && current.close != nil {
		return current.close()
	}
	return nil
}

func changedFreshnessProbes(current *FreshnessGeneration, probes []indexer.FreshnessProbe) []indexer.FreshnessProbe {
	if current == nil || len(current.tokens) != len(probes) {
		return append([]indexer.FreshnessProbe(nil), probes...)
	}
	var changed []indexer.FreshnessProbe
	for _, probe := range probes {
		key := projectGenerationKey(probe.Project)
		published, ok := current.tokens[key]
		if !probe.Supported || !ok || !published.Equal(probe.Token) {
			changed = append(changed, probe)
		}
	}
	return changed
}

func stableFreshnessProbes(before, after []indexer.FreshnessProbe) bool {
	if len(before) != len(after) {
		return false
	}
	for index := range before {
		if projectGenerationKey(before[index].Project) != projectGenerationKey(after[index].Project) {
			return false
		}
		if before[index].Supported != after[index].Supported {
			return false
		}
		if before[index].Supported && !before[index].Token.Equal(after[index].Token) {
			return false
		}
	}
	return true
}

func canonicalFreshnessProbes(probes []indexer.FreshnessProbe) ([]indexer.FreshnessProbe, error) {
	sort.Slice(probes, func(i, j int) bool { return probes[i].Project.Root < probes[j].Project.Root })
	seenRoots := map[string]bool{}
	seenIndexes := map[string]bool{}
	result := probes[:0]
	for _, probe := range probes {
		if seenRoots[probe.Project.Root] {
			continue
		}
		seenRoots[probe.Project.Root] = true
		if seenIndexes[probe.Project.IndexPath] {
			return nil, fmt.Errorf("projects %q share freshness index %s", probe.Project.Root, probe.Project.IndexPath)
		}
		seenIndexes[probe.Project.IndexPath] = true
		result = append(result, probe)
	}
	return result, nil
}

func openFreshnessGeneration(ctx context.Context, probes []indexer.FreshnessProbe) (graph.ReadRepository, func() error, error) {
	projects := projectsFromProbes(probes)
	if len(projects) == 1 {
		repository, err := sqlite.OpenReadOnly(ctx, projects[0].IndexPath)
		if err != nil {
			return nil, nil, err
		}
		if err := validateFreshnessRepository(ctx, repository, projects[0]); err != nil {
			_ = repository.Close()
			return nil, nil, err
		}
		return repository, repository.Close, nil
	}
	// Validate each member's repository metadata before constructing the exact
	// coordinator-owned federated view.
	for _, project := range projects {
		repository, err := sqlite.OpenReadOnly(ctx, project.IndexPath)
		if err != nil {
			return nil, nil, err
		}
		err = validateFreshnessRepository(ctx, repository, project)
		closeErr := repository.Close()
		if err != nil || closeErr != nil {
			return nil, nil, errors.Join(err, closeErr)
		}
	}
	repository, err := federation.OpenReadOnlyProjects(ctx, projects)
	if err != nil {
		return nil, nil, err
	}
	return repository, repository.Close, nil
}

func validateFreshnessRepository(ctx context.Context, repository graph.ReadRepository, project indexer.Project) error {
	expected := map[string]string{
		"repository_id": project.ID, "root": project.Root, "branch": project.Branch, "commit": project.Commit,
	}
	for key, value := range expected {
		actual, err := repository.Meta(ctx, key)
		if err != nil {
			return fmt.Errorf("validate %s metadata %s: %w", project.Root, key, err)
		}
		if actual != value {
			return fmt.Errorf("validate %s metadata %s: indexed %q, discovered %q", project.Root, key, actual, value)
		}
	}
	return nil
}

func projectGenerationKey(project indexer.Project) string {
	return strings.Join([]string{project.Root, project.ID, project.Branch, project.IndexPath}, "\x00")
}

func generationKey(probes []indexer.FreshnessProbe) string {
	var parts []string
	for _, probe := range probes {
		parts = append(parts, projectGenerationKey(probe.Project), probe.Token.String())
	}
	return strings.Join(parts, "\x00")
}

func projectsFromProbes(probes []indexer.FreshnessProbe) []indexer.Project {
	result := make([]indexer.Project, 0, len(probes))
	for _, probe := range probes {
		result = append(result, probe.Project)
	}
	return result
}

func tokensFromProbes(probes []indexer.FreshnessProbe) map[string]indexer.FreshnessToken {
	result := make(map[string]indexer.FreshnessToken, len(probes))
	for _, probe := range probes {
		result[projectGenerationKey(probe.Project)] = probe.Token
	}
	return result
}

func probeIn(candidate indexer.FreshnessProbe, probes []indexer.FreshnessProbe) bool {
	key := projectGenerationKey(candidate.Project)
	for _, probe := range probes {
		if projectGenerationKey(probe.Project) == key {
			return true
		}
	}
	return false
}
