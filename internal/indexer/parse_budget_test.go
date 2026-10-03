package indexer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// budgetProbeParser records how many preparations are inside the parse stage at
// the same time across every root sharing it. Two devices make that peak mean
// something. Each parse first waits until target of them coexist, so a run that
// is allowed to reach the target reaches it rather than merely being able to;
// and each parse then lingers for probeHold, so a budget that admits more than
// the target is observed at a higher peak instead of slipping through on how
// rarely instant parses overlap.
type budgetProbeParser struct {
	mutex       sync.Mutex
	active      int
	peak        int
	target      int
	release     chan struct{}
	releaseOnce sync.Once
}

// probeHold is long enough that every worker a budget admits is inside the
// probe at once, and short enough that the whole corpus still parses quickly.
const probeHold = 50 * time.Millisecond

func newBudgetProbeParser(target int) *budgetProbeParser {
	return &budgetProbeParser{target: target, release: make(chan struct{})}
}

func (*budgetProbeParser) Language() string { return "go" }

func (*budgetProbeParser) Supports(path string) bool {
	for _, suffix := range []string{".go", ".py", ".ts", ".gd", ".sql", ".md"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func (p *budgetProbeParser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	p.mutex.Lock()
	p.active++
	if p.active > p.peak {
		p.peak = p.active
	}
	reached := p.active >= p.target
	p.mutex.Unlock()
	if reached {
		p.releaseOnce.Do(func() { close(p.release) })
	} else {
		// The wait is bounded so a budget that cannot reach the target fails the
		// assertion instead of hanging until the package test timeout.
		select {
		case <-p.release:
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	select {
	case <-time.After(probeHold):
	case <-ctx.Done():
	}
	p.mutex.Lock()
	p.active--
	p.mutex.Unlock()
	return parserapi.NewBuilder(input, "go").Finish(), nil
}

func (p *budgetProbeParser) observedPeak() int {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.peak
}

// indexConcurrentRoots indexes that many separate repositories at once, every
// run metered by budget, and reports the peak number of preparations that
// coexisted across all of them.
func indexConcurrentRoots(t *testing.T, roots, workers, barrier int, budget *indexer.ParseBudget) int {
	t.Helper()
	probe := newBudgetProbeParser(barrier)
	registry := parserapi.NewRegistry(probe, configparser.New())
	projects := make([]indexer.Project, 0, roots)
	for index := range roots {
		root := testtemp.Dir(t)
		writePipelineCorpus(t, root)
		project, err := indexer.DiscoverProject(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		project.ID = fmt.Sprintf("repo:budget-%d", index)
		projects = append(projects, project)
	}
	var group sync.WaitGroup
	failures := make([]error, roots)
	for index, project := range projects {
		group.Add(1)
		go func(index int, project indexer.Project) {
			defer group.Done()
			service := indexer.NewService(newRecordingRepository(), registry)
			_, failures[index] = service.Run(context.Background(), project,
				indexer.Options{ParseWorkers: workers, ParseBudget: budget})
		}(index, project)
	}
	group.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("root %d: %v", index, err)
		}
	}
	return probe.observedPeak()
}

// TestProcessParseBudgetMatchesTheDerivedPoolSize pins the policy: the
// process-wide ceiling is the same number as one pool's derived size, so the
// budget and the per-pool cap in pipeline.go cannot drift apart.
func TestProcessParseBudgetMatchesTheDerivedPoolSize(t *testing.T) {
	if got, want := indexer.ProcessParseBudget().Limit(), indexer.ResolveParseWorkers(0); got != want {
		t.Fatalf("process budget admits %d preparations, want the derived pool size %d", got, want)
	}
}

// TestProcessParseBudgetIgnoresGOMAXPROCS records that the ceiling is derived
// from the machine's cores like the pool size it mirrors, and not from
// GOMAXPROCS, which the supervisor uses for its own root concurrency.
func TestProcessParseBudgetIgnoresGOMAXPROCS(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	if got, want := indexer.ProcessParseBudget().Limit(), indexer.ResolveParseWorkers(0); got != want {
		t.Fatalf("process budget admits %d preparations under GOMAXPROCS=1, want %d", got, want)
	}
	if indexer.ProcessParseBudget().Limit() < 1 {
		t.Fatal("process budget admits nothing, so no run could make progress")
	}
}

func TestNewParseBudgetLimits(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		requested int
		want      int
	}{
		{name: "negative is unbounded", requested: -1, want: 0},
		{name: "zero is unbounded", requested: 0, want: 0},
		{name: "one serializes", requested: 1, want: 1},
		{name: "derived default", requested: indexer.ResolveParseWorkers(0), want: indexer.ResolveParseWorkers(0)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := indexer.NewParseBudget(testCase.requested).Limit(); got != testCase.want {
				t.Fatalf("NewParseBudget(%d).Limit() = %d, want %d", testCase.requested, got, testCase.want)
			}
		})
	}
	if indexer.UnboundedParseBudget.Limit() != 0 {
		t.Fatalf("the unbounded budget reports a limit of %d", indexer.UnboundedParseBudget.Limit())
	}
}

// TestParseBudgetBoundsConcurrentRootsByTheBudgetNotTheRootCount is the
// acceptance criterion: the preparations of concurrently indexing roots sum to
// the budget however many roots there are, and however large each pool is.
func TestParseBudgetBoundsConcurrentRootsByTheBudgetNotTheRootCount(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("preparations cannot overlap on a single-core machine")
	}
	for _, testCase := range []struct {
		name    string
		roots   int
		workers int
		limit   int
	}{
		{name: "one root under a small budget", roots: 1, workers: 4, limit: 2},
		{name: "two roots share one budget", roots: 2, workers: 4, limit: 3},
		{name: "more roots than the budget admits", roots: 4, workers: 4, limit: 2},
		{name: "explicit worker override is still metered", roots: 2, workers: 6, limit: 3},
		{name: "a single permit serializes every root", roots: 3, workers: 4, limit: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			budget := indexer.NewParseBudget(testCase.limit)
			peak := indexConcurrentRoots(t, testCase.roots, testCase.workers, testCase.limit, budget)
			if peak > testCase.limit {
				t.Fatalf("%d roots of %d workers reached %d concurrent preparations, which exceeds the budget of %d",
					testCase.roots, testCase.workers, peak, testCase.limit)
			}
			if peak != testCase.limit {
				t.Fatalf("%d roots of %d workers reached only %d concurrent preparations, so the budget of %d was never used",
					testCase.roots, testCase.workers, peak, testCase.limit)
			}
		})
	}
}

// TestParseBudgetLeavesALoneRootAtItsFullPoolSize covers the criterion that the
// budget must not slow a root that indexes alone. The probe only releases once
// the whole derived pool is preparing at the same time, so the test passes only
// if a lone root holds as many permits as it has workers and therefore never
// waits on the budget.
func TestParseBudgetLeavesALoneRootAtItsFullPoolSize(t *testing.T) {
	workers := indexer.ResolveParseWorkers(0)
	if workers < 2 {
		t.Skip("the derived pool is one worker on this machine, so preparations cannot overlap")
	}
	peak := indexConcurrentRoots(t, 1, 0, workers, nil)
	if peak != workers {
		t.Fatalf("a lone root reached %d concurrent preparations under the process-wide budget, want its full pool of %d",
			peak, workers)
	}
}

// TestParseBudgetDoesNotChangeWhatARunIndexes keeps the budget a throttle: a
// fully serialized run and an unmetered one must produce the same durable calls
// in the same order, exactly as the worker count already must.
func TestParseBudgetDoesNotChangeWhatARunIndexes(t *testing.T) {
	observe := func(budget *indexer.ParseBudget) string {
		t.Helper()
		root := testtemp.Dir(t)
		writePipelineCorpus(t, root)
		project, err := indexer.DiscoverProject(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		project.ID = "repo:budget"
		project.Name = "corpus"
		repository := newRecordingRepository()
		report, err := indexer.NewService(repository, pipelineRegistry()).Run(context.Background(), project,
			indexer.Options{ParseWorkers: 4, MaxFileSize: pipelineMaxFileSize, ParseBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(struct {
			Updated []string         `json:"updated"`
			Skipped []string         `json:"skipped"`
			Calls   []repositoryCall `json:"calls"`
		}{Updated: report.Updated, Skipped: report.Skipped, Calls: repository.calls()})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	serialized := observe(indexer.NewParseBudget(1))
	unmetered := observe(indexer.UnboundedParseBudget)
	if serialized != unmetered {
		t.Fatalf("the budget changed what the run indexed:\nserialized=%s\nunmetered=%s", serialized, unmetered)
	}
}

// blockingParser holds every parse until it is released, so a run using it
// occupies its permits for as long as the test needs them occupied.
type blockingParser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingParser) Language() string { return "go" }

func (*blockingParser) Supports(path string) bool { return strings.HasSuffix(path, ".go") }

func (p *blockingParser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
	}
	return parserapi.NewBuilder(input, "go").Finish(), nil
}

// TestParseBudgetDoesNotStrandARunWhoseContextIsCancelled covers the one place
// the ceiling is deliberately not honored. A cancelled stage still owes an
// outcome to every hand-off slot it reserved, so waiting for a permit that
// another root is holding would hang the writer instead of failing the run.
func TestParseBudgetDoesNotStrandARunWhoseContextIsCancelled(t *testing.T) {
	budget := indexer.NewParseBudget(1)
	holderRoot := testtemp.Dir(t)
	write(t, filepath.Join(holderRoot, "main.go"), "package main\n\nfunc main() {}\n")
	holder, err := indexer.DiscoverProject(context.Background(), holderRoot)
	if err != nil {
		t.Fatal(err)
	}
	holder.ID = "repo:holder"
	blocker := &blockingParser{entered: make(chan struct{}), release: make(chan struct{})}
	var holding sync.WaitGroup
	holding.Add(1)
	go func() {
		defer holding.Done()
		_, _ = indexer.NewService(newRecordingRepository(), parserapi.NewRegistry(blocker)).
			Run(context.Background(), holder, indexer.Options{ParseWorkers: 1, ParseBudget: budget})
	}()
	// The holder is inside a parse, so the budget's only permit is taken and the
	// run below cannot acquire one until it is cancelled.
	<-blocker.entered
	defer func() {
		close(blocker.release)
		holding.Wait()
	}()

	root := testtemp.Dir(t)
	writePipelineCorpus(t, root)
	project, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(250*time.Millisecond, cancel)
	finished := make(chan error, 1)
	go func() {
		_, runErr := indexer.NewService(newRecordingRepository(), pipelineRegistry()).
			Run(ctx, project, indexer.Options{ParseWorkers: 4, ParseBudget: budget})
		finished <- runErr
	}()
	select {
	case runErr := <-finished:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("a cancelled run under an exhausted budget reported %v", runErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled run never returned while another root held every permit")
	}
}
