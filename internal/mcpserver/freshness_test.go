package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

type coordinatorParser struct{}

func (coordinatorParser) Language() string          { return "coordinator" }
func (coordinatorParser) Supports(path string) bool { return filepath.Ext(path) == ".snap" }
func (coordinatorParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	name := strings.TrimSpace(string(input.Content))
	builder := parserapi.NewBuilder(input, "coordinator")
	builder.AddNode(graph.Node{Kind: graph.KindFunction, Name: name, QualifiedName: "fixture." + name,
		Location: graph.Location{Path: input.Path, Line: 1, Column: 1}})
	if strings.HasPrefix(filepath.Base(input.Path), "bench-") {
		for index := range 32 {
			qualified := "fixture." + name + ".Helper" + fmt.Sprint(index)
			builder.AddNode(graph.Node{Kind: graph.KindFunction, Name: "Helper" + fmt.Sprint(index), QualifiedName: qualified,
				Location: graph.Location{Path: input.Path, Line: index + 2, Column: 1}})
		}
	}
	return builder.Result, nil
}

func TestFreshnessCoordinatorStartupAndUnchangedAcquireSkipWriter(t *testing.T) {
	probe := coordinatorProbe(t, "one")
	var refreshes, opens atomic.Int32
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			return []indexer.FreshnessProbe{probe}, nil
		},
		refresh: func(context.Context, indexer.FreshnessProbe) (indexer.Report, error) {
			refreshes.Add(1)
			return indexer.Report{}, nil
		},
		open: testGenerationOpener(t, &opens),
	}
	coordinator := newFreshnessCoordinator([]string{probe.Project.Root}, 3, operations)
	first, release, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if refreshes.Load() != 1 || opens.Load() != 1 || first.Repository == nil {
		t.Fatalf("startup refreshes=%d opens=%d generation=%#v", refreshes.Load(), opens.Load(), first)
	}
	second, release, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if second != first || refreshes.Load() != 1 || opens.Load() != 1 {
		t.Fatalf("unchanged acquisition replaced generation: refreshes=%d opens=%d", refreshes.Load(), opens.Load())
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProductionFreshnessCoordinatorPublishesQueryOnlyGenerations(t *testing.T) {
	probe := coordinatorProbe(t, "one")
	registry := parserapi.NewRegistry(coordinatorParser{})
	coordinator, startup, err := NewFreshnessCoordinator(context.Background(), []string{probe.Project.Root}, registry, FreshnessCoordinatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := coordinator.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, writable := startup.Repository.(graph.IndexRepository); writable {
		t.Fatal("published generation exposed index-write capability")
	}
	if len(startup.Diagnostics.Reports) != 1 || startup.Diagnostics.Reports[0].CountsCollected {
		t.Fatalf("startup did not use one no-count refresh: %#v", startup.Diagnostics)
	}
	indexedAt, err := startup.Repository.Meta(context.Background(), "indexed_at")
	if err != nil || indexedAt == "" {
		t.Fatalf("startup indexed_at=%q err=%v", indexedAt, err)
	}
	service := NewFederated(startup.Repository, startup.Projects).WithFreshness(coordinator)
	_, firstResult, err := service.findSymbols(context.Background(), nil, FindSymbolsInput{Query: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstResult.Matches) != 1 || firstResult.Matches[0].QualifiedName != "fixture.one" {
		t.Fatalf("startup query = %#v", firstResult.Matches)
	}
	unchanged := coordinator.current
	if unchanged != startup || len(unchanged.Diagnostics.Reports) != 1 {
		t.Fatal("first unchanged tool call replaced or refreshed the startup generation")
	}
	indexedAtAfter, err := unchanged.Repository.Meta(context.Background(), "indexed_at")
	if err != nil || indexedAtAfter != indexedAt {
		t.Fatalf("unchanged call wrote metadata: before=%q after=%q err=%v", indexedAt, indexedAtAfter, err)
	}

	changedProbe := changedCoordinatorProbe(t, probe, "two")
	_, changedResult, err := service.findSymbols(context.Background(), nil, FindSymbolsInput{Queries: []string{"two", "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changedResult.Results) != 2 || changedResult.Results[0].Value == nil || len(*changedResult.Results[0].Value) != 1 || (*changedResult.Results[0].Value)[0].QualifiedName != "fixture.two" {
		t.Fatalf("changed batch query = %#v", changedResult.Results)
	}
	changed := coordinator.current
	if changed == startup || changed.Key == startup.Key || len(changed.Diagnostics.Reports) != 1 {
		t.Fatalf("dirty edit did not publish one refreshed generation: %#v", changed.Diagnostics)
	}
	if !changedProbe.Token.Equal(changed.tokens[projectGenerationKey(changedProbe.Project)]) {
		t.Fatal("published token does not match the dirty source generation")
	}
}

func TestProductionFreshnessCoordinatorUsesConservativeNonGitFallback(t *testing.T) {
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "sample.snap"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	coordinator, startup, err := NewFreshnessCoordinator(context.Background(), []string{root},
		parserapi.NewRegistry(coordinatorParser{}), FreshnessCoordinatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := coordinator.Close(); err != nil {
			t.Error(err)
		}
	}()
	if startup.Diagnostics.Fallbacks[startup.Projects[0].Root] == "" {
		t.Fatalf("missing non-Git fallback diagnostic: %#v", startup.Diagnostics)
	}
	next, release, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if next == startup || len(next.Diagnostics.Reports) != 1 {
		t.Fatal("non-Git acquisition skipped its conservative full refresh")
	}
}

func TestProductionFreshnessCoordinatorPublishesFederationAtomically(t *testing.T) {
	first := coordinatorProbe(t, "alpha")
	second := coordinatorProbe(t, "beta")
	coordinator, startup, err := NewFreshnessCoordinator(context.Background(),
		[]string{second.Project.Root, first.Project.Root}, parserapi.NewRegistry(coordinatorParser{}), FreshnessCoordinatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := coordinator.Close(); err != nil {
			t.Error(err)
		}
	}()
	if len(startup.Projects) != 2 || startup.Projects[0].Root > startup.Projects[1].Root || len(startup.Diagnostics.Refreshed) != 2 {
		t.Fatalf("startup federation = projects %#v diagnostics %#v", startup.Projects, startup.Diagnostics)
	}
	changedCoordinatorProbe(t, second, "gamma")
	changed, release, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if len(changed.Diagnostics.Refreshed) != 1 || changed.Diagnostics.Refreshed[0] != second.Project.Root {
		t.Fatalf("one-member change refreshed %#v", changed.Diagnostics.Refreshed)
	}
	nodes, err := changed.Repository.SearchNodes(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, node := range nodes {
		names[node.QualifiedName] = true
	}
	if !names["fixture.alpha"] || !names["fixture.gamma"] || names["fixture.beta"] {
		t.Fatalf("partially published federation nodes = %#v", nodes)
	}
}

func TestFreshnessCoordinatorActiveCancellationPublishesNothing(t *testing.T) {
	probe := coordinatorProbe(t, "one")
	refreshStarted := make(chan struct{})
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			return []indexer.FreshnessProbe{probe}, nil
		},
		refresh: func(ctx context.Context, _ indexer.FreshnessProbe) (indexer.Report, error) {
			close(refreshStarted)
			<-ctx.Done()
			return indexer.Report{}, ctx.Err()
		},
		open: func(context.Context, []indexer.FreshnessProbe) (graph.ReadRepository, func() error, error) {
			t.Fatal("canceled refresh opened a generation")
			return nil, nil, nil
		},
	}
	coordinator := newFreshnessCoordinator([]string{probe.Project.Root}, 3, operations)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := coordinator.Acquire(ctx)
		done <- err
	}()
	<-refreshStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("active cancellation error = %v", err)
	}
	if coordinator.current != nil {
		t.Fatal("active cancellation published a generation")
	}
}

func TestFreshnessCoordinatorDrainsLeaseBeforeWriter(t *testing.T) {
	firstProbe := coordinatorProbe(t, "one")
	secondProbe := changedCoordinatorProbe(t, firstProbe, "two")
	var current atomic.Pointer[indexer.FreshnessProbe]
	current.Store(&firstProbe)
	refreshStarted := make(chan struct{})
	var refreshes atomic.Int32
	var opens atomic.Int32
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			return []indexer.FreshnessProbe{*current.Load()}, nil
		},
		refresh: func(context.Context, indexer.FreshnessProbe) (indexer.Report, error) {
			if refreshes.Add(1) > 1 {
				select {
				case <-refreshStarted:
				default:
					close(refreshStarted)
				}
			}
			return indexer.Report{}, nil
		},
		open: testGenerationOpener(t, &opens),
	}
	coordinator := newFreshnessCoordinator([]string{firstProbe.Project.Root}, 3, operations)
	_, startupRelease, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	startupRelease()
	_, heldRelease, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	current.Store(&secondProbe)
	done := make(chan error, 1)
	go func() {
		_, release, acquireErr := coordinator.Acquire(context.Background())
		if release != nil {
			release()
		}
		done <- acquireErr
	}()
	select {
	case <-refreshStarted:
		t.Fatal("writer started while the old query generation was leased")
	case <-time.After(40 * time.Millisecond):
	}
	heldRelease()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("changed acquisition did not complete after lease release")
	}
	select {
	case <-refreshStarted:
	default:
		t.Fatal("writer never started")
	}
	if opens.Load() != 2 {
		t.Fatalf("generation opens = %d, want 2", opens.Load())
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFreshnessCoordinatorSingleFlightsAndCanceledWaiterDetaches(t *testing.T) {
	probe := coordinatorProbe(t, "one")
	refreshGate := make(chan struct{})
	refreshStarted := make(chan struct{})
	var refreshes, opens atomic.Int32
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			return []indexer.FreshnessProbe{probe}, nil
		},
		refresh: func(ctx context.Context, _ indexer.FreshnessProbe) (indexer.Report, error) {
			refreshes.Add(1)
			select {
			case <-refreshStarted:
			default:
				close(refreshStarted)
			}
			select {
			case <-ctx.Done():
				return indexer.Report{}, ctx.Err()
			case <-refreshGate:
				return indexer.Report{}, nil
			}
		},
		open: testGenerationOpener(t, &opens),
	}
	coordinator := newFreshnessCoordinator([]string{probe.Project.Root}, 3, operations)
	ownerDone := make(chan error, 1)
	go func() {
		_, release, err := coordinator.Acquire(context.Background())
		if release != nil {
			release()
		}
		ownerDone <- err
	}()
	<-refreshStarted
	waiterContext, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := coordinator.Acquire(waiterContext)
		waiterDone <- err
	}()
	cancelWaiter()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
	close(refreshGate)
	if err := <-ownerDone; err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 || opens.Load() != 1 {
		t.Fatalf("shared flight refreshes=%d opens=%d", refreshes.Load(), opens.Load())
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFreshnessCoordinatorRejectsThreeUnstableAttempts(t *testing.T) {
	probes := []indexer.FreshnessProbe{
		coordinatorProbe(t, "one"), coordinatorProbe(t, "two"), coordinatorProbe(t, "three"),
		coordinatorProbe(t, "four"), coordinatorProbe(t, "five"), coordinatorProbe(t, "six"),
	}
	// All probes must describe one project identity; only their opaque content
	// token differs.
	for index := range probes {
		probes[index].Project = probes[0].Project
	}
	var probeIndex atomic.Int32
	var refreshes, opens atomic.Int32
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			index := int(probeIndex.Add(1) - 1)
			return []indexer.FreshnessProbe{probes[index]}, nil
		},
		refresh: func(context.Context, indexer.FreshnessProbe) (indexer.Report, error) {
			refreshes.Add(1)
			return indexer.Report{}, nil
		},
		open: testGenerationOpener(t, &opens),
	}
	coordinator := newFreshnessCoordinator([]string{probes[0].Project.Root}, 3, operations)
	if _, _, err := coordinator.Acquire(context.Background()); !errors.Is(err, ErrContinuouslyChangingSource) {
		t.Fatalf("unstable error = %v", err)
	}
	if refreshes.Load() != 3 || opens.Load() != 0 || coordinator.current != nil {
		t.Fatalf("unstable publication refreshes=%d opens=%d current=%#v", refreshes.Load(), opens.Load(), coordinator.current)
	}
}

func TestFreshnessCoordinatorFederationFailurePublishesNothingAndRetriesAllMembers(t *testing.T) {
	first := coordinatorProbe(t, "one")
	second := coordinatorProbe(t, "two")
	probes := []indexer.FreshnessProbe{first, second}
	var failSecond atomic.Bool
	failSecond.Store(true)
	var refreshed []string
	var opens atomic.Int32
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			return append([]indexer.FreshnessProbe(nil), probes...), nil
		},
		refresh: func(_ context.Context, probe indexer.FreshnessProbe) (indexer.Report, error) {
			refreshed = append(refreshed, probe.Project.Root)
			if probe.Project.Root == second.Project.Root && failSecond.Swap(false) {
				return indexer.Report{}, errors.New("injected second-member failure")
			}
			return indexer.Report{}, nil
		},
		open: testGenerationOpener(t, &opens),
	}
	coordinator := newFreshnessCoordinator([]string{second.Project.Root, first.Project.Root}, 3, operations)
	if _, _, err := coordinator.Acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("first acquisition error = %v", err)
	}
	if coordinator.current != nil || opens.Load() != 0 {
		t.Fatalf("partial federation was published: current=%#v opens=%d", coordinator.current, opens.Load())
	}
	firstAttemptCalls := len(refreshed)
	generation, release, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if generation == nil || opens.Load() != 1 {
		t.Fatalf("retry generation=%#v opens=%d", generation, opens.Load())
	}
	retryRoots := refreshed[firstAttemptCalls:]
	if len(retryRoots) != 2 || retryRoots[0] != first.Project.Root || retryRoots[1] != second.Project.Root {
		t.Fatalf("retry did not reconverge every member in deterministic order: %#v", retryRoots)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFreshnessCoordinatorOpenFailurePublishesNothing(t *testing.T) {
	probe := coordinatorProbe(t, "one")
	var refreshes, opens atomic.Int32
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			return []indexer.FreshnessProbe{probe}, nil
		},
		refresh: func(context.Context, indexer.FreshnessProbe) (indexer.Report, error) {
			refreshes.Add(1)
			return indexer.Report{}, nil
		},
		open: func(ctx context.Context, probes []indexer.FreshnessProbe) (graph.ReadRepository, func() error, error) {
			if opens.Add(1) == 1 {
				return nil, nil, errors.New("injected query-only open failure")
			}
			return testGenerationOpener(t, new(atomic.Int32))(ctx, probes)
		},
	}
	coordinator := newFreshnessCoordinator([]string{probe.Project.Root}, 3, operations)
	if _, _, err := coordinator.Acquire(context.Background()); err == nil || !strings.Contains(err.Error(), "query-only") {
		t.Fatalf("first acquisition error = %v", err)
	}
	if coordinator.current != nil {
		t.Fatal("failed query-only open published a generation")
	}
	generation, release, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if generation == nil || refreshes.Load() != 2 || opens.Load() != 2 {
		t.Fatalf("retry generation=%#v refreshes=%d opens=%d", generation, refreshes.Load(), opens.Load())
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPHandlerLeaseBlocksChangedGenerationWriter(t *testing.T) {
	firstProbe := coordinatorProbe(t, "one")
	secondProbe := changedCoordinatorProbe(t, firstProbe, "two")
	var current atomic.Pointer[indexer.FreshnessProbe]
	current.Store(&firstProbe)
	queryStarted := make(chan struct{})
	releaseQuery := make(chan struct{})
	refreshStarted := make(chan struct{})
	var refreshes, opens atomic.Int32
	operations := freshnessOperations{
		probe: func(context.Context, []string) ([]indexer.FreshnessProbe, error) {
			return []indexer.FreshnessProbe{*current.Load()}, nil
		},
		refresh: func(context.Context, indexer.FreshnessProbe) (indexer.Report, error) {
			if refreshes.Add(1) > 1 {
				close(refreshStarted)
			}
			return indexer.Report{}, nil
		},
		open: func(ctx context.Context, probes []indexer.FreshnessProbe) (graph.ReadRepository, func() error, error) {
			repository, closeRepository, err := testGenerationOpener(t, &opens)(ctx, probes)
			if err != nil {
				return nil, nil, err
			}
			if opens.Load() == 1 {
				repository = &blockingSearchRepository{ReadRepository: repository, started: queryStarted, release: releaseQuery}
			}
			return repository, closeRepository, nil
		},
	}
	coordinator := newFreshnessCoordinator([]string{firstProbe.Project.Root}, 3, operations)
	startup, startupRelease, err := coordinator.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	startupRelease()
	service := NewFederated(startup.Repository, startup.Projects).WithFreshness(coordinator)
	firstDone := make(chan error, 1)
	go func() {
		_, _, queryErr := service.findSymbols(context.Background(), nil, FindSymbolsInput{Query: "one"})
		firstDone <- queryErr
	}()
	<-queryStarted
	current.Store(&secondProbe)
	secondDone := make(chan error, 1)
	go func() {
		_, release, acquireErr := coordinator.Acquire(context.Background())
		if release != nil {
			release()
		}
		secondDone <- acquireErr
	}()
	select {
	case <-refreshStarted:
		t.Fatal("writer started before the MCP handler finished building its result")
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseQuery)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-refreshStarted:
	default:
		t.Fatal("writer never started after the handler released its lease")
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
}

type blockingSearchRepository struct {
	graph.ReadRepository
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingSearchRepository) SearchNodes(ctx context.Context, query string, limit int) ([]graph.Node, error) {
	r.once.Do(func() { close(r.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.release:
		return r.ReadRepository.SearchNodes(ctx, query, limit)
	}
}

func coordinatorProbe(t *testing.T, content string) indexer.FreshnessProbe {
	t.Helper()
	root := testtemp.Dir(t)
	coordinatorGit(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "sample.snap"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	coordinatorGit(t, root, "add", "sample.snap")
	coordinatorGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	probe, err := indexer.ProbeFreshness(context.Background(), root, parserapi.NewRegistry(coordinatorParser{}), indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return probe
}

func changedCoordinatorProbe(t *testing.T, previous indexer.FreshnessProbe, content string) indexer.FreshnessProbe {
	t.Helper()
	if err := os.WriteFile(filepath.Join(previous.Project.Root, "sample.snap"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	probe, err := indexer.ProbeFreshness(context.Background(), previous.Project.Root, parserapi.NewRegistry(coordinatorParser{}), indexer.FreshnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return probe
}

func coordinatorGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func testGenerationOpener(t *testing.T, opens *atomic.Int32) func(context.Context, []indexer.FreshnessProbe) (graph.ReadRepository, func() error, error) {
	t.Helper()
	return func(ctx context.Context, _ []indexer.FreshnessProbe) (graph.ReadRepository, func() error, error) {
		opens.Add(1)
		repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "generation.sqlite"))
		if err != nil {
			return nil, nil, err
		}
		var once sync.Once
		closeRepository := func() error {
			var closeErr error
			once.Do(func() { closeErr = repository.Close() })
			return closeErr
		}
		return repository, closeRepository, nil
	}
}
