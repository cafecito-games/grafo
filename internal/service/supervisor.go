package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cafecito-games/grafo/internal/agentinstall"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/indexseed"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

// statusFormat is the on-disk format marker of the supervisor status file.
const statusFormat = "grafo.service.status/1"

// indexLockWait bounds how long a supervisor run waits for a foreground
// `grafo index` to finish with the same index before deferring the root to the
// next pass.
var indexLockWait = 30 * time.Second

// debounce is how long a change hint is coalesced before the root is indexed.
const debounce = 750 * time.Millisecond

// Event is a hint that a root may have changed. Hints are advisory: the indexer
// remains the authority on what actually changed. An overflow hint means the
// platform watcher dropped events, so the root needs a full reconciliation.
type Event struct {
	Root     string
	Overflow bool
}

// Discoverer resolves branch and index identity for a root. Production uses
// indexer.DiscoverProject, which is the single authority for that identity.
type Discoverer func(ctx context.Context, root string) (indexer.Project, error)

// Runner performs one indexing run of one discovered project. The supervisor
// orchestrates roots and never implements a second indexer.
type Runner interface {
	Index(ctx context.Context, project indexer.Project, options indexer.Options) (indexer.Report, error)
}

// RootStatus is the supervisor's per-root state, persisted so that
// `grafo service status` and `grafo doctor` can report it from another process.
type RootStatus struct {
	Root          string `json:"root"`
	Name          string `json:"name,omitempty"`
	Branch        string `json:"branch,omitempty"`
	IndexPath     string `json:"index_path,omitempty"`
	Paused        bool   `json:"paused,omitempty"`
	Unavailable   bool   `json:"unavailable,omitempty"`
	Missing       bool   `json:"missing,omitempty"`
	Runs          int    `json:"runs"`
	Failures      int    `json:"failures"`
	Updated       int    `json:"updated"`
	Removed       int    `json:"removed"`
	LastRunAt     string `json:"last_run_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastErrorAt   string `json:"last_error_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot is the whole supervisor state at one instant.
type Snapshot struct {
	Format    string       `json:"format"`
	PID       int          `json:"pid"`
	Binary    string       `json:"binary,omitempty"`
	StartedAt string       `json:"started_at,omitempty"`
	UpdatedAt string       `json:"updated_at,omitempty"`
	Roots     []RootStatus `json:"roots"`
}

// StatusPath returns the supervisor status file inside the state directory.
func StatusPath(stateDir string) string { return filepath.Join(stateDir, "service-status.json") }

// SupervisorLockPath returns the lock that admits exactly one supervisor.
func SupervisorLockPath(stateDir string) string { return filepath.Join(stateDir, "service.lock") }

// ReadSnapshot reads the persisted supervisor status. A missing file yields an
// empty snapshot, because the service may simply never have run.
func ReadSnapshot(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Snapshot{Format: statusFormat, Roots: []RootStatus{}}, nil
		}
		return Snapshot{}, fmt.Errorf("read service status %s: %w", path, err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("service status %s is malformed: %w; move it aside to continue", path, err)
	}
	if snapshot.Format != statusFormat {
		return Snapshot{}, fmt.Errorf("service status %s uses unsupported format %q (expected %q); move it aside to continue",
			path, snapshot.Format, statusFormat)
	}
	return snapshot, nil
}

// Options configures a supervisor.
type Options struct {
	// Env is the outside-world seam shared with the installer.
	Env agentinstall.Environment
	// Store is the watched-root registry.
	Store *Store
	// Concurrency bounds how many roots are indexed at once.
	Concurrency int
	// Discover resolves branch identity; defaults to indexer.DiscoverProject.
	Discover Discoverer
	// Runner performs indexing runs; defaults to the SQLite-backed indexer.
	Runner Runner
	// Hints is an optional platform-watcher feed. Periodic reconciliation runs
	// regardless, so a missing or lossy watcher only affects latency.
	Hints <-chan Event
	// Logger receives bounded structured records.
	Logger *Logger
	// StateDir is the directory holding the status file and lock.
	StateDir string
	// Binary is the absolute supervisor binary path reported in status.
	Binary string
}

// Supervisor keeps every registered root incrementally indexed.
//
// Concurrency model: exactly one supervisor process per user, admitted by an
// exclusive kernel-backed lock on the state directory; inside it, at most one
// goroutine per root and at most Concurrency roots indexing at once; and every
// indexing run - foreground or background - holds the per-index lock for the
// branch index it writes. A killed supervisor therefore needs no cleanup: the
// kernel drops its locks, SQLite rolls back the partial transaction, and the
// next run rediscovers and reconciles from the committed index.
type Supervisor struct {
	options Options

	mutex    sync.Mutex
	statuses map[string]*RootStatus
	pending  map[string]time.Time
	full     map[string]bool
	inFlight map[string]bool
	started  time.Time
}

// NewSupervisor returns a supervisor with defaults applied.
func NewSupervisor(options Options) *Supervisor {
	if options.Concurrency <= 0 {
		options.Concurrency = min(4, max(1, runtime.GOMAXPROCS(0)))
	}
	if options.Discover == nil {
		options.Discover = indexer.DiscoverProject
	}
	if options.Runner == nil {
		options.Runner = SQLiteRunner{}
	}
	return &Supervisor{
		options:  options,
		statuses: map[string]*RootStatus{},
		pending:  map[string]time.Time{},
		full:     map[string]bool{},
		inFlight: map[string]bool{},
		started:  now(),
	}
}

// Run supervises every registered root until ctx is cancelled. It fails closed
// when another supervisor already holds the lock, so two daemons can never
// schedule the same root.
func (s *Supervisor) Run(ctx context.Context) error {
	stateDir := s.options.StateDir
	if stateDir == "" {
		resolved, err := StateDir(s.options.Env)
		if err != nil {
			return err
		}
		stateDir = resolved
		s.options.StateDir = resolved
	}
	unlock, err := Lock(SupervisorLockPath(stateDir), 0)
	if err != nil {
		return fmt.Errorf("another grafo service supervisor is already running: %w", err)
	}
	defer func() { _ = unlock() }()
	s.options.Logger.Record("info", "supervisor started", map[string]string{
		"pid": fmt.Sprint(os.Getpid()), "state_dir": stateDir,
	})
	defer s.options.Logger.Record("info", "supervisor stopped", nil)

	// A first pass makes the service useful immediately after start and after a
	// restart, without waiting for an interval.
	if _, err := s.Once(ctx); err != nil {
		s.options.Logger.Record("error", "reconciliation pass failed", map[string]string{"error": err.Error()})
	}
	ticker := time.NewTicker(MinimumInterval / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case hint, ok := <-s.options.Hints:
			if !ok {
				s.options.Hints = nil
				continue
			}
			s.Hint(hint)
		case <-ticker.C:
			if _, err := s.Once(ctx); err != nil {
				s.options.Logger.Record("error", "reconciliation pass failed", map[string]string{"error": err.Error()})
			}
		}
	}
}

// Hint records a change hint for one root, debouncing it and, for an overflow,
// scheduling a bounded full reconciliation of that root.
func (s *Supervisor) Hint(event Event) {
	canonical := filepath.Clean(event.Root)
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.pending[canonical] = now().Add(debounce)
	if event.Overflow {
		s.full[canonical] = true
	}
}

// Once performs one reconciliation pass over every registered root that is due,
// and returns the resulting snapshot. A malformed or locked registry stops every
// mutation: nothing is indexed and nothing is pruned.
func (s *Supervisor) Once(ctx context.Context) (Snapshot, error) {
	registry, err := s.options.Store.Load()
	if err != nil {
		return Snapshot{}, err
	}
	due := s.selectDue(registry)
	if len(due) > 0 {
		s.reconcile(ctx, due)
	}
	snapshot := s.Snapshot()
	if err := s.persist(snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// selectDue decides which roots to reconcile now, prunes status for roots the
// user has unregistered, and reserves each selected root so no two goroutines
// index the same root at once.
func (s *Supervisor) selectDue(registry Registry) []Root {
	moment := now()
	s.mutex.Lock()
	defer s.mutex.Unlock()
	registered := make(map[string]bool, len(registry.Roots))
	var due []Root
	for _, entry := range registry.Roots {
		registered[entry.Root] = true
		status, ok := s.statuses[entry.Root]
		if !ok {
			status = &RootStatus{Root: entry.Root, Name: entry.Name}
			s.statuses[entry.Root] = status
		}
		status.Name = entry.Name
		status.Paused = entry.Paused
		if entry.Paused || s.inFlight[entry.Root] {
			continue
		}
		deadline, hinted := s.pending[entry.Root]
		if hinted && moment.Before(deadline) {
			// The hint is still being debounced.
			continue
		}
		if !hinted && status.LastRunAt != "" {
			last, err := time.Parse(time.RFC3339Nano, status.LastRunAt)
			if err == nil && moment.Sub(last) < entry.Every() {
				continue
			}
		}
		delete(s.pending, entry.Root)
		s.inFlight[entry.Root] = true
		due = append(due, entry)
	}
	for root := range s.statuses {
		if !registered[root] {
			delete(s.statuses, root)
			delete(s.pending, root)
			delete(s.full, root)
		}
	}
	return due
}

// reconcile indexes the due roots with bounded concurrency. One failing root
// never stops another: every failure is recorded on its own root.
func (s *Supervisor) reconcile(ctx context.Context, due []Root) {
	semaphore := make(chan struct{}, s.options.Concurrency)
	var group sync.WaitGroup
	for _, entry := range due {
		group.Add(1)
		go func(entry Root) {
			defer group.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			defer func() {
				s.mutex.Lock()
				delete(s.inFlight, entry.Root)
				s.mutex.Unlock()
			}()
			s.reconcileRoot(ctx, entry)
		}(entry)
	}
	group.Wait()
}

// reconcileRoot indexes the active branch of one root.
func (s *Supervisor) reconcileRoot(ctx context.Context, entry Root) {
	started := now()
	s.update(entry.Root, func(status *RootStatus) {
		status.LastRunAt = started.Format(time.RFC3339Nano)
		status.Runs++
	})
	info, err := os.Stat(entry.Root)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		// A deleted root is reported and left registered. Pruning happens only
		// through an explicit `grafo doctor --repair`.
		s.failRoot(entry, "root is missing", err, func(status *RootStatus) {
			status.Missing, status.Unavailable = true, false
		})
		return
	case err != nil:
		// A temporarily unreadable root is paused for this pass; its registration
		// and its indexes are preserved.
		s.failRoot(entry, "root is unavailable", err, func(status *RootStatus) {
			status.Missing, status.Unavailable = false, true
		})
		return
	case !info.IsDir():
		s.failRoot(entry, "root is not a directory", fmt.Errorf("%s is not a directory", entry.Root), nil)
		return
	}
	project, err := s.options.Discover(ctx, entry.Root)
	if err != nil {
		s.failRoot(entry, "branch detection failed", err, nil)
		return
	}
	if strings.TrimSpace(project.Branch) == "" || strings.TrimSpace(project.IndexPath) == "" {
		// Without a proven branch identity there is no safe database to write, so
		// the run stops rather than falling back to another branch's index.
		s.failRoot(entry, "branch detection failed", errors.New("no branch or index path was resolved"), nil)
		return
	}
	s.mutex.Lock()
	// The supervisor never reports graph totals, so it skips the full-graph
	// count queries whose cost scales with total graph size.
	options := indexer.Options{Force: s.full[entry.Root], ReportDetail: indexer.ReportWithoutCounts}
	delete(s.full, entry.Root)
	s.mutex.Unlock()
	report, err := s.options.Runner.Index(ctx, project, options)
	if err != nil {
		if ctx.Err() != nil {
			// Cancellation is not a root failure: the committed index is intact and
			// the next pass resumes from it.
			s.update(entry.Root, func(status *RootStatus) { status.Runs-- })
			if options.Force {
				s.mutex.Lock()
				s.full[entry.Root] = true
				s.mutex.Unlock()
			}
			return
		}
		if options.Force {
			// A failed full reconciliation stays pending so an overflow is not lost.
			s.mutex.Lock()
			s.full[entry.Root] = true
			s.mutex.Unlock()
		}
		s.failRoot(entry, "indexing failed", err, func(status *RootStatus) {
			status.Branch, status.IndexPath = project.Branch, project.IndexPath
		})
		return
	}
	finished := now()
	s.update(entry.Root, func(status *RootStatus) {
		status.Branch, status.IndexPath = project.Branch, project.IndexPath
		status.Missing, status.Unavailable = false, false
		status.Updated, status.Removed = len(report.Updated), len(report.Removed)
		status.LastSuccessAt = finished.Format(time.RFC3339Nano)
		status.LastError, status.LastErrorAt = "", ""
	})
	s.options.Logger.Record("info", "indexed root", map[string]string{
		"root": entry.Root, "branch": project.Branch, "index": project.IndexPath,
		"updated": fmt.Sprint(len(report.Updated)), "removed": fmt.Sprint(len(report.Removed)),
		"ms": fmt.Sprint(finished.Sub(started).Milliseconds()), "full": fmt.Sprint(options.Force),
	})
}

// failRoot records one root's failure without affecting any other root.
func (s *Supervisor) failRoot(entry Root, message string, cause error, extra func(*RootStatus)) {
	moment := now().Format(time.RFC3339Nano)
	s.update(entry.Root, func(status *RootStatus) {
		status.Failures++
		status.LastError = message + ": " + cause.Error()
		status.LastErrorAt = moment
		if extra != nil {
			extra(status)
		}
	})
	s.options.Logger.Record("error", message, map[string]string{
		"root": entry.Root, "error": cause.Error(),
	})
}

func (s *Supervisor) update(root string, change func(*RootStatus)) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	status, ok := s.statuses[root]
	if !ok {
		status = &RootStatus{Root: root}
		s.statuses[root] = status
	}
	change(status)
}

// Snapshot returns the current supervisor state in deterministic root order.
func (s *Supervisor) Snapshot() Snapshot {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	roots := make([]RootStatus, 0, len(s.statuses))
	for _, status := range s.statuses {
		roots = append(roots, *status)
	}
	sort.Slice(roots, func(left, right int) bool { return roots[left].Root < roots[right].Root })
	return Snapshot{
		Format:    statusFormat,
		PID:       os.Getpid(),
		Binary:    s.options.Binary,
		StartedAt: s.started.Format(time.RFC3339),
		UpdatedAt: now().Format(time.RFC3339),
		Roots:     roots,
	}
}

// persist writes the status file atomically at mode 0600.
func (s *Supervisor) persist(snapshot Snapshot) error {
	if s.options.StateDir == "" {
		return nil
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := StatusPath(s.options.StateDir)
	if err := s.options.Env.MkdirAll(s.options.StateDir, 0o700); err != nil {
		return fmt.Errorf("create grafo state directory %s: %w", s.options.StateDir, err)
	}
	if err := s.options.Env.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write service status %s: %w", path, err)
	}
	return nil
}

// SQLiteRunner is the production Runner: it opens the branch index the project
// resolved, under the per-index lock, and runs the shared indexer service.
type SQLiteRunner struct{}

// Index runs one indexing pass. The per-index lock makes a background run and a
// foreground `grafo index` mutually exclusive on the same database.
func (SQLiteRunner) Index(ctx context.Context, project indexer.Project, options indexer.Options) (indexer.Report, error) {
	unlock, err := IndexLock(project.IndexPath, indexLockWait)
	if err != nil {
		return indexer.Report{}, err
	}
	defer func() { _ = unlock() }()
	// A worktree registered before it has ever been indexed pays the same cold
	// cost as a foreground run, so the background pass adopts a sibling index on
	// the same terms. A declined adoption is not an error: the pass below indexes
	// from scratch exactly as it did before.
	if options.Seed == nil {
		if _, statErr := os.Stat(project.IndexPath); errors.Is(statErr, os.ErrNotExist) {
			result, seedErr := indexseed.Seed(ctx, project, indexseed.Options{TryLock: TryDonorIndexLock})
			if seedErr != nil {
				return indexer.Report{}, seedErr
			}
			options.Seed = result.Provenance
		}
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		return indexer.Report{}, err
	}
	report, runErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, options)
	closeErr := repository.Close()
	if runErr != nil {
		return report, runErr
	}
	return report, closeErr
}

// TryDonorIndexLock adapts the branch-index lock to the narrow capability index
// adoption needs. Adoption must never wait on a donor another process is
// refreshing, so contention is reported rather than blocked on, and the seeding
// package stays independent of this one.
func TryDonorIndexLock(indexPath string) (indexseed.Unlock, bool, error) {
	unlock, acquired, err := TryIndexLock(indexPath)
	if err != nil || !acquired {
		return nil, false, err
	}
	return indexseed.Unlock(unlock), true, nil
}

// SharedRoots reports the registered roots, other than root, that contain it or
// are contained by it. A shared root makes pruning ambiguous, so repair refuses.
func SharedRoots(registry Registry, root string) []string {
	var shared []string
	for _, entry := range registry.Roots {
		if entry.Root == root {
			continue
		}
		if within(root, entry.Root) || within(entry.Root, root) {
			shared = append(shared, entry.Root)
		}
	}
	slices.Sort(shared)
	return shared
}

func within(child, parent string) bool {
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, strings.TrimRight(parent, string(filepath.Separator))+string(filepath.Separator))
}
