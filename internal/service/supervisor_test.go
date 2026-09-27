package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
)

// fakeRunner records the projects it was asked to index. It stands in for the
// real indexer so branch isolation and scheduling are observable without
// building a repository.
type fakeRunner struct {
	mutex   sync.Mutex
	calls   []indexer.Project
	forced  []bool
	failFor map[string]error
	report  indexer.Report
}

func (f *fakeRunner) Index(_ context.Context, project indexer.Project, options indexer.Options) (indexer.Report, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, project)
	f.forced = append(f.forced, options.Force)
	if err, ok := f.failFor[project.Root]; ok {
		return indexer.Report{}, err
	}
	return f.report, nil
}

func (f *fakeRunner) indexPaths() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	paths := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		paths = append(paths, call.IndexPath)
	}
	return paths
}

// fakeProject builds the identity a discoverer would return for a root.
func fakeProject(root, branch string) indexer.Project {
	return indexer.Project{
		Root: root, Name: filepath.Base(root), ID: "repo:" + filepath.Base(root),
		Branch: branch, IndexPath: filepath.Join(root, ".grafo", "indexes", branch+".sqlite"),
		GitManaged: true,
	}
}

func newTestSupervisor(t *testing.T, runner Runner, discover Discoverer) (*Supervisor, *Store) {
	t.Helper()
	env := isolatedEnvironment(t)
	store := NewStore(env)
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := OpenLogger(LogPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	return NewSupervisor(Options{
		Env: env, Store: store, StateDir: stateDir, Logger: logger,
		Concurrency: 2, Runner: runner, Discover: discover, Binary: "/usr/local/bin/grafo",
	}), store
}

func TestSupervisorIndexesEveryRegisteredRoot(t *testing.T) {
	runner := &fakeRunner{report: indexer.Report{Updated: []string{"a.go"}}}
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, "main"), nil
	})
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		if _, _, err := store.Add(root, Settings{}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := supervisor.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(snapshot.Roots) != 2 {
		t.Fatalf("expected two roots in status, got %#v", snapshot.Roots)
	}
	for _, status := range snapshot.Roots {
		if status.LastSuccessAt == "" || status.Updated != 1 || status.LastError != "" {
			t.Fatalf("root %#v was not indexed successfully", status)
		}
	}
	// The status file is what another process reads, so it must round-trip.
	persisted, err := ReadSnapshot(StatusPath(supervisor.options.StateDir))
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if len(persisted.Roots) != 2 {
		t.Fatalf("persisted status = %#v", persisted)
	}
	// A second pass inside the interval is a no-op, so restarting the pass loop
	// cannot re-index everything in a tight loop.
	if _, err := supervisor.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(runner.indexPaths()); got != 2 {
		t.Fatalf("expected 2 indexing runs, got %d", got)
	}
}

func TestBranchSwitchIndexesOnlyTheDiscoveredBranch(t *testing.T) {
	runner := &fakeRunner{}
	branch := "main"
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, branch), nil
	})
	root := t.TempDir()
	if _, _, err := store.Add(root, Settings{Interval: MinimumInterval}); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	branch = "feature"
	supervisor.Hint(Event{Root: root})
	waitForHint(t, supervisor)
	if _, err := supervisor.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	paths := runner.indexPaths()
	if len(paths) != 2 {
		t.Fatalf("expected one run per branch, got %#v", paths)
	}
	if paths[0] == paths[1] {
		t.Fatalf("both branches wrote the same index %q", paths[0])
	}
	if filepath.Base(paths[0]) != "main.sqlite" || filepath.Base(paths[1]) != "feature.sqlite" {
		t.Fatalf("branch indexes are not isolated: %#v", paths)
	}
}

func TestBranchDetectionFailureNeverIndexes(t *testing.T) {
	runner := &fakeRunner{}
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return indexer.Project{Root: root}, nil
	})
	root := t.TempDir()
	if _, _, err := store.Add(root, Settings{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := supervisor.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.indexPaths()) != 0 {
		t.Fatalf("indexed without a proven branch: %#v", runner.indexPaths())
	}
	if len(snapshot.Roots) != 1 || snapshot.Roots[0].LastError == "" {
		t.Fatalf("expected a recorded branch failure, got %#v", snapshot.Roots)
	}
}

func TestMissingRootIsReportedAndNeverPruned(t *testing.T) {
	runner := &fakeRunner{}
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, "main"), nil
	})
	root := filepath.Join(t.TempDir(), "repository")
	if err := mkdir(root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add(root, Settings{}); err != nil {
		t.Fatal(err)
	}
	if err := removeAll(root); err != nil {
		t.Fatal(err)
	}
	snapshot, err := supervisor.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Roots) != 1 || !snapshot.Roots[0].Missing {
		t.Fatalf("expected the root to be reported missing, got %#v", snapshot.Roots)
	}
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Roots) != 1 {
		t.Fatalf("a temporary outage pruned the registry: %#v", registry.Roots)
	}
}

func TestOneFailingRootDoesNotStopOthers(t *testing.T) {
	broken, healthy := t.TempDir(), t.TempDir()
	runner := &fakeRunner{failFor: map[string]error{broken: errors.New("disk on fire")}}
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, "main"), nil
	})
	for _, root := range []string{broken, healthy} {
		if _, _, err := store.Add(root, Settings{}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := supervisor.Once(context.Background())
	if err != nil {
		t.Fatalf("a failing root must not fail the pass: %v", err)
	}
	statuses := map[string]RootStatus{}
	for _, status := range snapshot.Roots {
		statuses[status.Root] = status
	}
	if statuses[broken].LastError == "" {
		t.Fatalf("expected a per-root failure for %s", broken)
	}
	if statuses[healthy].LastSuccessAt == "" || statuses[healthy].LastError != "" {
		t.Fatalf("healthy root was not indexed: %#v", statuses[healthy])
	}
}

func TestOverflowHintForcesOneFullReconciliation(t *testing.T) {
	runner := &fakeRunner{}
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, "main"), nil
	})
	root := t.TempDir()
	if _, _, err := store.Add(root, Settings{Interval: MinimumInterval}); err != nil {
		t.Fatal(err)
	}
	supervisor.Hint(Event{Root: root, Overflow: true})
	waitForHint(t, supervisor)
	if _, err := supervisor.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	supervisor.Hint(Event{Root: root})
	waitForHint(t, supervisor)
	if _, err := supervisor.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	if len(runner.forced) != 2 || !runner.forced[0] || runner.forced[1] {
		t.Fatalf("expected exactly one bounded full reconciliation, got %#v", runner.forced)
	}
}

func TestSecondSupervisorFailsClosed(t *testing.T) {
	supervisor, _ := newTestSupervisor(t, &fakeRunner{}, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, "main"), nil
	})
	unlock, err := Lock(SupervisorLockPath(supervisor.options.StateDir), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	if err := supervisor.Run(context.Background()); err == nil {
		t.Fatal("expected a second supervisor to refuse to start")
	}
}

func TestMalformedRegistryStopsEveryMutation(t *testing.T) {
	runner := &fakeRunner{}
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, "main"), nil
	})
	path, err := store.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := mkdir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(path, "{\"format\":\"grafo.service.registry/9\"}"); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Once(context.Background()); err == nil {
		t.Fatal("expected an unsupported registry format to stop the pass")
	}
	if len(runner.indexPaths()) != 0 {
		t.Fatal("indexed from a registry Grafo cannot read")
	}
}

func TestForegroundIndexLockBlocksTheSupervisorRun(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), ".grafo", "indexes", "main.sqlite")
	unlock, err := IndexLock(indexPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	previous := indexLockWait
	indexLockWait = 50 * time.Millisecond
	defer func() { indexLockWait = previous }()
	if _, err := (SQLiteRunner{}).Index(context.Background(), indexer.Project{IndexPath: indexPath}, indexer.Options{}); err == nil {
		t.Fatal("expected the background run to defer to the foreground index lock")
	}
}

// waitForHint lets the debounce window elapse so the next pass sees the hint.
func waitForHint(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		supervisor.mutex.Lock()
		ready := true
		for _, moment := range supervisor.pending {
			if time.Now().Before(moment) {
				ready = false
			}
		}
		supervisor.mutex.Unlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("debounce window never elapsed")
}

func TestSupervisorIndexesARealRepositoryIdempotentlyAcrossRestarts(t *testing.T) {
	env := isolatedEnvironment(t)
	store := NewStore(env)
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := "package app\n\nfunc Greet() string { return \"hi\" }\n"
	if err := writeFile(filepath.Join(root, "app.go"), source); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add(root, Settings{Interval: MinimumInterval}); err != nil {
		t.Fatal(err)
	}
	build := func() *Supervisor {
		logger, err := OpenLogger(LogPath(stateDir))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = logger.Close() })
		return NewSupervisor(Options{Env: env, Store: store, StateDir: stateDir, Logger: logger, Concurrency: 1})
	}
	first, err := build().Once(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Roots) != 1 || first.Roots[0].LastError != "" {
		t.Fatalf("first pass status = %#v", first.Roots)
	}
	if first.Roots[0].Updated == 0 {
		t.Fatalf("nothing was indexed: %#v", first.Roots[0])
	}
	indexPath := first.Roots[0].IndexPath
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("branch index was not created: %v", err)
	}
	// A restart re-runs the same pass over the committed index: the unchanged file
	// is recognized, so the second pass writes nothing new.
	restarted := build()
	restarted.Hint(Event{Root: first.Roots[0].Root})
	waitForHint(t, restarted)
	second, err := restarted.Once(context.Background())
	if err != nil {
		t.Fatalf("pass after restart: %v", err)
	}
	if len(second.Roots) != 1 || second.Roots[0].LastError != "" {
		t.Fatalf("second pass status = %#v", second.Roots)
	}
	if second.Roots[0].Updated != 0 || second.Roots[0].Removed != 0 {
		t.Fatalf("restart re-indexed unchanged files: %#v", second.Roots[0])
	}
	if second.Roots[0].IndexPath != indexPath {
		t.Fatalf("restart chose a different index: %q then %q", indexPath, second.Roots[0].IndexPath)
	}
}

func TestConcurrentHintsAndPassesAreRaceFree(t *testing.T) {
	runner := &fakeRunner{}
	supervisor, store := newTestSupervisor(t, runner, func(_ context.Context, root string) (indexer.Project, error) {
		return fakeProject(root, "main"), nil
	})
	for index := 0; index < 3; index++ {
		if _, _, err := store.Add(t.TempDir(), Settings{Interval: MinimumInterval}); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for pass := 0; pass < 5; pass++ {
				if _, err := supervisor.Once(context.Background()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		group.Add(1)
		go func() {
			defer group.Done()
			for _, entry := range registry.Roots {
				supervisor.Hint(Event{Root: entry.Root, Overflow: true})
			}
		}()
	}
	group.Wait()
	if len(supervisor.Snapshot().Roots) != 3 {
		t.Fatalf("expected three roots in the snapshot, got %#v", supervisor.Snapshot().Roots)
	}
}
