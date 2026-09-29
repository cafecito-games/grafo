// Package benchmark runs the opt-in production indexing acceptance harness.
package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/kvbench"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/version"
)

const (
	ReportSchemaVersion = 1
	ReportFileName      = "report.json"
	StatusPassed        = "passed"
	StatusFailed        = "failed"
)

var errRequestedInterruption = errors.New("benchmark requested durable interruption")

type Options struct {
	Repository      string
	Output          string
	Baseline        string
	Engine          string
	CleanupDatabase bool
	MaxWALBytes     int64
	MaxRSSBytes     int64
}

type Report struct {
	SchemaVersion        int              `json:"schema_version"`
	SemanticIndexVersion string           `json:"semantic_index_version"`
	GraphSchemaVersion   int              `json:"graph_schema_version"`
	GrafoVersion         string           `json:"grafo_version"`
	GrafoCommit          string           `json:"grafo_commit"`
	GrafoDirty           bool             `json:"grafo_dirty"`
	Storage              Storage          `json:"storage"`
	GeneratedAt          string           `json:"generated_at"`
	Status               string           `json:"status"`
	Error                string           `json:"error,omitempty"`
	Corpus               Corpus           `json:"corpus"`
	Inputs               InputCoverage    `json:"inputs"`
	Scenarios            []ScenarioReport `json:"scenarios"`
	Artifacts            Artifacts        `json:"artifacts"`
	Baseline             *Baseline        `json:"baseline,omitempty"`
}

type Storage struct {
	Engine         string `json:"engine"`
	Library        string `json:"library"`
	LibraryVersion string `json:"library_version"`
	Durability     string `json:"durability"`
}

type Corpus struct {
	Path   string `json:"path"`
	Commit string `json:"commit"`
	Branch string `json:"branch"`
}

type InputStats struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

type InputCoverage struct {
	Tracked          InputStats            `json:"tracked"`
	Supported        InputStats            `json:"supported"`
	Routed           InputStats            `json:"routed"`
	Indexable        InputStats            `json:"indexable"`
	ByLanguage       map[string]InputStats `json:"by_language"`
	Unsupported      InputStats            `json:"unsupported"`
	UnsupportedPaths []string              `json:"unsupported_paths"`
	Skipped          InputStats            `json:"skipped"`
	SkippedPaths     []string              `json:"skipped_paths"`
	SkippedSymlinks  []string              `json:"skipped_symlinks,omitempty"`
}

type ScenarioReport struct {
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	Index     IndexReport     `json:"index"`
	Resources ResourceMetrics `json:"resources"`
}

type IndexReport struct {
	Updated               int                    `json:"updated"`
	UpdatedPaths          []string               `json:"updated_paths"`
	ContentChecked        int                    `json:"content_checked"`
	Unchanged             int                    `json:"unchanged"`
	Removed               int                    `json:"removed"`
	RemovedPaths          []string               `json:"removed_paths"`
	Skipped               int                    `json:"skipped"`
	SkippedPaths          []string               `json:"skipped_paths,omitempty"`
	Counts                graph.Counts           `json:"counts"`
	Phases                indexer.PhaseDurations `json:"phases"`
	Writes                graph.WriteStats       `json:"writes"`
	ReconciliationBatches int                    `json:"reconciliation_batches"`
}

type OptionalBytes struct {
	Supported bool   `json:"supported"`
	Value     *int64 `json:"bytes"`
	Reason    string `json:"reason,omitempty"`
}

type ResourceMetrics struct {
	PeakWALBytes  OptionalBytes `json:"peak_wal"`
	FinalWALBytes OptionalBytes `json:"final_wal"`
	PeakRSSBytes  OptionalBytes `json:"peak_rss"`
	DatabaseBytes OptionalBytes `json:"database_size"`
}

type Artifacts struct {
	OutputDirectory  string `json:"output_directory"`
	Report           string `json:"report"`
	ColdDatabase     string `json:"cold_database"`
	ResumeDatabase   string `json:"resume_database"`
	IsolatedCheckout string `json:"isolated_checkout,omitempty"`
}

type Baseline struct {
	Path          string `json:"path"`
	SchemaVersion int    `json:"schema_version"`
	GrafoCommit   string `json:"grafo_commit"`
}

type sourceSnapshot struct {
	commit string
	branch string
	status string
}

func resolveStorage(requested string) (string, Storage, error) {
	engine := strings.ToLower(strings.TrimSpace(requested))
	if engine == "" {
		engine = "sqlite"
	}
	switch engine {
	case "sqlite":
		return engine, Storage{Engine: engine, Library: "modernc.org/sqlite", LibraryVersion: dependencyVersion("modernc.org/sqlite", "v1.57.0"),
			Durability: "WAL with synchronous=NORMAL; atomic file and reconciliation transactions"}, nil
	case string(kvbench.EngineBolt):
		return engine, Storage{Engine: engine, Library: "go.etcd.io/bbolt", LibraryVersion: dependencyVersion("go.etcd.io/bbolt", "v1.5.0"),
			Durability: "single-writer ACID transactions with two-phase fsync commit"}, nil
	case string(kvbench.EnginePebble):
		return engine, Storage{Engine: engine, Library: "github.com/cockroachdb/pebble/v2", LibraryVersion: dependencyVersion("github.com/cockroachdb/pebble/v2", "v2.1.7"),
			Durability: "atomic indexed batches committed with WAL sync"}, nil
	default:
		if registration, found := lookupEngine(engine); found {
			return engine, registration.Storage, nil
		}
		return "", Storage{}, fmt.Errorf("unsupported benchmark engine %q (want sqlite, bbolt, pebble, or a registered engine)", requested)
	}
}

func dependencyVersion(path, fallback string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fallback
	}
	for _, dependency := range info.Deps {
		if dependency.Path == path {
			if dependency.Replace != nil && dependency.Replace.Version != "" {
				return dependency.Replace.Version
			}
			if dependency.Version != "" {
				return dependency.Version
			}
		}
	}
	return fallback
}

func storageSuffix(engine string) string {
	kind := effectiveStorageKind(engine)
	if kind == "sqlite" || kind == string(kvbench.EngineBolt) {
		return ".db"
	}
	return ".pebble"
}

// EngineOpener opens one registered engine's repository at path. It exists so
// benchmark-only comparisons (the SQLite layout spike) can route their
// adapters through the scenario machinery without this package importing them:
// the production binaries must stay free of benchmark-only dependencies.
type EngineOpener func(ctx context.Context, path string) (graph.Repository, error)

// EngineRegistration pairs the report metadata and the opener of one engine
// added through RegisterEngine.
type EngineRegistration struct {
	Storage Storage
	Opener  EngineOpener
	// StorageKind names the physical storage a registered engine delegates to
	// ("sqlite", "bbolt", or "pebble") so size sampling, artifact naming, and
	// database cleanup follow that storage's on-disk shape. Empty means the
	// engine brings its own shape and its storage metrics stay unsupported.
	StorageKind string
}

var registeredEngines sync.Map

// RegisterEngine makes one additional engine available to Run. Registering
// again with the same name replaces the earlier registration. It returns an
// error for an unusable registration rather than letting Run fail later with
// a confusing engine error.
func RegisterEngine(engine string, registration EngineRegistration) error {
	normalized := strings.ToLower(strings.TrimSpace(engine))
	if normalized == "" {
		return fmt.Errorf("registered engine name must not be empty")
	}
	if registration.Opener == nil {
		return fmt.Errorf("engine %q registered without an opener", normalized)
	}
	switch registration.StorageKind {
	case "", "sqlite", string(kvbench.EngineBolt), string(kvbench.EnginePebble):
	default:
		return fmt.Errorf("engine %q registered with unsupported storage kind %q", normalized, registration.StorageKind)
	}
	registeredEngines.Store(normalized, registration)
	return nil
}

// effectiveStorageKind resolves the physical storage an engine name behaves
// like on disk: a built-in engine maps to itself, a registered engine maps to
// its declared StorageKind, and anything else maps to itself so the callers'
// unsupported fallbacks keep firing.
func effectiveStorageKind(engine string) string {
	switch engine {
	case "", "sqlite", string(kvbench.EngineBolt), string(kvbench.EnginePebble):
		return engine
	}
	if registration, found := lookupEngine(engine); found && registration.StorageKind != "" {
		return registration.StorageKind
	}
	return engine
}

func lookupEngine(engine string) (EngineRegistration, bool) {
	stored, found := registeredEngines.Load(engine)
	if !found {
		return EngineRegistration{}, false
	}
	registration, typed := stored.(EngineRegistration)
	return registration, typed
}

func openRepository(ctx context.Context, engine, path string) (graph.Repository, error) {
	switch engine {
	case "sqlite":
		return sqlite.Open(ctx, path)
	case string(kvbench.EngineBolt), string(kvbench.EnginePebble):
		return kvbench.Open(ctx, kvbench.Engine(engine), path, kvbench.Options{})
	default:
		if registration, found := lookupEngine(engine); found {
			return registration.Opener(ctx, path)
		}
		return nil, fmt.Errorf("unsupported benchmark engine %q", engine)
	}
}

func Run(ctx context.Context, options Options) (report Report, resultErr error) {
	engine, storage, err := resolveStorage(options.Engine)
	if err != nil {
		return Report{}, err
	}
	source, snapshot, err := inspectSource(ctx, options.Repository)
	if err != nil {
		return Report{}, err
	}
	output, err := prepareOutput(source, options.Output)
	if err != nil {
		return Report{}, err
	}
	artifactDirectory, err := os.MkdirTemp(output, "artifacts-")
	if err != nil {
		return Report{}, fmt.Errorf("create artifact directory: %w", err)
	}
	grafoCommit, grafoDirty := buildProvenance()
	report = Report{
		SchemaVersion: ReportSchemaVersion, SemanticIndexVersion: indexer.SemanticIndexVersion,
		GraphSchemaVersion: graph.SchemaVersion, GrafoVersion: version.Value, GrafoCommit: grafoCommit, GrafoDirty: grafoDirty,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: StatusFailed, Storage: storage,
		Corpus:    Corpus{Path: source, Commit: snapshot.commit, Branch: snapshot.branch},
		Scenarios: []ScenarioReport{},
		Artifacts: Artifacts{OutputDirectory: output, Report: filepath.Join(output, ReportFileName),
			ColdDatabase:   filepath.Join(artifactDirectory, "cold"+storageSuffix(engine)),
			ResumeDatabase: filepath.Join(artifactDirectory, "resume"+storageSuffix(engine))},
	}
	defer func() {
		if resultErr != nil {
			report.Status = StatusFailed
			report.Error = resultErr.Error()
		}
		if writeErr := writeReport(report.Artifacts.Report, report); writeErr != nil && resultErr == nil {
			resultErr = writeErr
			report.Status = StatusFailed
			report.Error = writeErr.Error()
		}
	}()
	if options.Baseline != "" {
		baseline, err := loadBaseline(options.Baseline)
		if err != nil {
			return report, err
		}
		report.Baseline = &baseline
	}

	isolation := filepath.Join(artifactDirectory, "corpus")
	report.Artifacts.IsolatedCheckout = isolation
	cleanupIsolation := false
	defer func() {
		if !cleanupIsolation {
			return
		}
		if err := os.RemoveAll(isolation); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("clean isolated checkout %s: %w", isolation, err)
			report.Artifacts.IsolatedCheckout = isolation
		} else if err == nil {
			report.Artifacts.IsolatedCheckout = ""
		}
	}()
	if err := runGit(ctx, "", "clone", "--no-hardlinks", "--no-checkout", "--", source, isolation); err != nil {
		return report, fmt.Errorf("isolate corpus: %w", err)
	}
	if err := runGit(ctx, isolation, "switch", "--detach", snapshot.commit); err != nil {
		return report, fmt.Errorf("checkout corpus commit: %w", err)
	}
	if err := runGit(ctx, isolation, "switch", "-c", "grafo-benchmark-base"); err != nil {
		return report, fmt.Errorf("create isolated base branch: %w", err)
	}
	if err := runGit(ctx, isolation, "config", "user.name", "Grafo Benchmark"); err != nil {
		return report, err
	}
	if err := runGit(ctx, isolation, "config", "user.email", "grafo-benchmark@example.invalid"); err != nil {
		return report, err
	}
	registry := parserdefaults.NewRegistry()
	coverage, supportedPaths, err := collectCoverage(ctx, isolation, registry)
	if err != nil {
		return report, err
	}
	report.Inputs = coverage
	if len(supportedPaths) == 0 {
		return report, fmt.Errorf("corpus has no tracked inputs supported by the production parser registry")
	}
	target := chooseMutationTarget(supportedPaths, registry)
	expectedMutationPaths := registry.SemanticAffectedPaths(supportedPaths, []string{target})
	expectedDeletionPaths := make([]string, 0, len(expectedMutationPaths)-1)
	for _, path := range expectedMutationPaths {
		if path != target {
			expectedDeletionPaths = append(expectedDeletionPaths, path)
		}
	}
	targetPath := filepath.Join(isolation, filepath.FromSlash(target))
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		return report, fmt.Errorf("inspect scenario target %s: %w", target, err)
	}
	original, err := os.ReadFile(targetPath)
	if err != nil {
		return report, fmt.Errorf("read scenario target %s: %w", target, err)
	}

	cold, err := executeScenario(ctx, engine, "cold", isolation, report.Artifacts.ColdDatabase, nil)
	if err == nil && cold.Index.Updated+cold.Index.Skipped != coverage.Routed.Files {
		err = fmt.Errorf("cold scenario accounted for %d inputs, want %d", cold.Index.Updated+cold.Index.Skipped, coverage.Routed.Files)
		cold = withScenarioError(cold, err)
	}
	report.Scenarios = append(report.Scenarios, cold)
	if err != nil {
		return report, err
	}
	unchanged, err := executeScenario(ctx, engine, "unchanged", isolation, report.Artifacts.ColdDatabase, nil)
	if err == nil {
		err = requireUnchanged(unchanged, cold.Index.Counts)
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(unchanged, err))
	if err != nil {
		return report, err
	}
	if err := removeDatabase(engine, report.Artifacts.ColdDatabase); err != nil {
		return report, fmt.Errorf("remove completed control database: %w", err)
	}
	report.Artifacts.ColdDatabase = ""

	interrupted, interruptErr := executeScenario(ctx, engine, "interrupted", isolation, report.Artifacts.ResumeDatabase,
		func(boundary indexer.Boundary) error {
			if boundary.Kind == indexer.BoundaryFilePersisted && boundary.Completed == 1 {
				return errRequestedInterruption
			}
			return nil
		})
	if !errors.Is(interruptErr, errRequestedInterruption) {
		if interruptErr == nil {
			interruptErr = fmt.Errorf("interruption scenario did not stop at the requested durable boundary")
		}
		report.Scenarios = append(report.Scenarios, withScenarioError(interrupted, interruptErr))
		return report, interruptErr
	}
	interrupted.Status, interrupted.Error = StatusPassed, ""
	report.Scenarios = append(report.Scenarios, interrupted)
	resumed, err := executeScenario(ctx, engine, "resumed", isolation, report.Artifacts.ResumeDatabase, nil)
	if err == nil && !reflect.DeepEqual(cold.Index.Counts, resumed.Index.Counts) {
		err = fmt.Errorf("resumed graph counts differ from uninterrupted cold index")
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(resumed, err))
	if err != nil {
		return report, err
	}
	resumeUnchanged, err := executeScenario(ctx, engine, "resume_unchanged", isolation, report.Artifacts.ResumeDatabase, nil)
	if err == nil {
		err = requireUnchanged(resumeUnchanged, cold.Index.Counts)
	}
	if err == nil && resumeUnchanged.Index.ReconciliationBatches != 0 {
		err = fmt.Errorf("resumed database retained reconciliation work")
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(resumeUnchanged, err))
	if err != nil {
		return report, err
	}

	if err := os.WriteFile(targetPath, append(append([]byte{}, original...), '\n'), 0o644); err != nil {
		return report, err
	}
	if err := runMutationPair(ctx, engine, &report, "edit", "edit_unchanged", isolation, report.Artifacts.ResumeDatabase, expectedMutationPaths, 0, cold.Index.Counts, false); err != nil {
		return report, err
	}
	if err := os.Remove(targetPath); err != nil {
		return report, err
	}
	deleted, err := executeScenario(ctx, engine, "delete", isolation, report.Artifacts.ResumeDatabase, nil)
	if err == nil && (deleted.Index.Removed != 1 || len(deleted.Index.RemovedPaths) != 1 || deleted.Index.RemovedPaths[0] != target ||
		!reflect.DeepEqual(deleted.Index.UpdatedPaths, expectedDeletionPaths)) {
		err = fmt.Errorf("delete scenario updated=%v removed=%v, want updated=%v removed=[%s]",
			deleted.Index.UpdatedPaths, deleted.Index.RemovedPaths, expectedDeletionPaths, target)
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(deleted, err))
	if err != nil {
		return report, err
	}
	if err := restoreTrackedFile(targetPath, original, targetInfo.Mode()); err != nil {
		return report, err
	}
	if err := runMutationPair(ctx, engine, &report, "restore", "restore_unchanged", isolation, report.Artifacts.ResumeDatabase, expectedMutationPaths, 0, cold.Index.Counts, true); err != nil {
		return report, err
	}
	if err := runGit(ctx, isolation, "diff", "--quiet", "--", target); err != nil {
		return report, fmt.Errorf("restored target differs from the selected commit: %w", err)
	}

	if err := runGit(ctx, isolation, "switch", "-c", "grafo-benchmark-branch"); err != nil {
		return report, err
	}
	if err := os.WriteFile(targetPath, append(append([]byte{}, original...), '\n'), 0o644); err != nil {
		return report, err
	}
	if err := runGit(ctx, isolation, "add", "--", target); err != nil {
		return report, err
	}
	if err := runGit(ctx, isolation, "commit", "-m", "benchmark branch mutation"); err != nil {
		return report, err
	}
	branchSwitch, err := executeScenario(ctx, engine, "branch_switch", isolation, report.Artifacts.ResumeDatabase, nil)
	if err == nil {
		err = requireUpdatedPaths(branchSwitch, expectedMutationPaths)
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(branchSwitch, err))
	if err != nil {
		return report, err
	}
	if err := runGit(ctx, isolation, "switch", "grafo-benchmark-base"); err != nil {
		return report, err
	}
	branchRestore, err := executeScenario(ctx, engine, "branch_restore", isolation, report.Artifacts.ResumeDatabase, nil)
	if err == nil {
		err = requireUpdatedPaths(branchRestore, expectedMutationPaths)
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(branchRestore, err))
	if err != nil {
		return report, err
	}
	branchUnchanged, err := executeScenario(ctx, engine, "branch_unchanged", isolation, report.Artifacts.ResumeDatabase, nil)
	if err == nil {
		err = requireUnchanged(branchUnchanged, cold.Index.Counts)
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(branchUnchanged, err))
	if err != nil {
		return report, err
	}

	if err := enforceBudgets(report.Scenarios, options); err != nil {
		return report, err
	}
	after, err := snapshotSource(ctx, source)
	if err != nil {
		return report, err
	}
	if snapshot != after {
		return report, fmt.Errorf("source checkout changed during benchmark")
	}
	if options.CleanupDatabase {
		if err := removeDatabase(engine, report.Artifacts.ResumeDatabase); err != nil {
			return report, fmt.Errorf("remove completed benchmark database: %w", err)
		}
		report.Artifacts.ResumeDatabase = ""
	}
	report.Status = StatusPassed
	report.Error = ""
	cleanupIsolation = true
	return report, nil
}

func executeScenario(ctx context.Context, engine, name, root, database string, hook indexer.BoundaryHook) (scenario ScenarioReport, resultErr error) {
	scenario = ScenarioReport{Name: name, Status: StatusFailed}
	defer func() {
		if recovered := recover(); recovered != nil {
			resultErr = fmt.Errorf("scenario %s panicked: %v", name, recovered)
			scenario = withScenarioError(scenario, resultErr)
		}
	}()
	return executeScenarioRun(ctx, engine, scenario, root, database, hook)
}

func executeScenarioRun(ctx context.Context, engine string, scenario ScenarioReport, root, database string, hook indexer.BoundaryHook) (ScenarioReport, error) {
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		return withScenarioError(scenario, err), err
	}
	project.IndexPath = database
	sampler := newResourceSampler(ctx, engine, database)
	sampler.start()
	defer func() { sampler.stop() }()
	repository, err := openRepository(ctx, engine, database)
	if err != nil {
		return withScenarioError(scenario, err), err
	}
	closed := false
	defer func() {
		if !closed {
			_ = repository.Close()
		}
	}()
	sampler.sample()
	wrappedHook := func(boundary indexer.Boundary) error {
		sampler.sampleWAL()
		if hook != nil {
			return hook(boundary)
		}
		return nil
	}
	indexed, runErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{Boundary: wrappedHook})
	if runErr != nil {
		if counts, countErr := repository.Counts(context.WithoutCancel(ctx)); countErr == nil {
			indexed.Counts = counts
		}
	}
	closeErr := repository.Close()
	closed = true
	sampler.stop()
	scenario.Index = summarizeIndex(indexed)
	scenario.Resources = sampler.metrics()
	err = errors.Join(runErr, closeErr)
	if err != nil {
		return withScenarioError(scenario, err), err
	}
	scenario.Status = StatusPassed
	return scenario, nil
}

func summarizeIndex(report indexer.Report) IndexReport {
	return IndexReport{Updated: len(report.Updated), UpdatedPaths: report.Updated, ContentChecked: report.Checked,
		Unchanged: report.Unchanged, Removed: len(report.Removed), RemovedPaths: report.Removed,
		Skipped: len(report.Skipped), SkippedPaths: report.Skipped, Counts: report.Counts,
		Phases: report.Phases, Writes: report.Writes, ReconciliationBatches: report.ReconciliationBatches}
}

func restoreTrackedFile(path string, content []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, content, mode.Perm()); err != nil {
		return fmt.Errorf("restore tracked file: %w", err)
	}
	if err := os.Chmod(path, mode.Perm()); err != nil {
		return fmt.Errorf("restore tracked file mode: %w", err)
	}
	return nil
}

func runMutationPair(ctx context.Context, engine string, report *Report, changedName, stableName, root, database string, expectedUpdated []string, removed int, expectedCounts graph.Counts, zeroReadStable bool) error {
	changed, err := executeScenario(ctx, engine, changedName, root, database, nil)
	if err == nil && (changed.Index.Updated != len(expectedUpdated) || changed.Index.Removed != removed) {
		err = fmt.Errorf("%s changed updated=%d removed=%d, want %d/%d", changedName, changed.Index.Updated, changed.Index.Removed, len(expectedUpdated), removed)
	}
	if err == nil {
		err = requireUpdatedPaths(changed, expectedUpdated)
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(changed, err))
	if err != nil {
		return err
	}
	stable, err := executeScenario(ctx, engine, stableName, root, database, nil)
	if err == nil && (stable.Index.Updated != 0 || stable.Index.Removed != 0) {
		err = fmt.Errorf("%s did not converge", stableName)
	}
	if err == nil && !reflect.DeepEqual(stable.Index.Counts, expectedCounts) {
		err = fmt.Errorf("%s did not converge to control graph counts", stableName)
	}
	if err == nil && zeroReadStable {
		err = requireUnchanged(stable, expectedCounts)
	}
	report.Scenarios = append(report.Scenarios, withScenarioError(stable, err))
	return err
}

func requireUpdatedPaths(scenario ScenarioReport, paths []string) error {
	if !reflect.DeepEqual(scenario.Index.UpdatedPaths, paths) {
		return fmt.Errorf("%s updated paths %v, want %v", scenario.Name, scenario.Index.UpdatedPaths, paths)
	}
	return nil
}

func requireUnchanged(scenario ScenarioReport, counts graph.Counts) error {
	if scenario.Index.ContentChecked != 0 || scenario.Index.Updated != 0 || scenario.Index.Removed != 0 {
		return fmt.Errorf("%s was not a zero-read unchanged refresh", scenario.Name)
	}
	if !reflect.DeepEqual(scenario.Index.Counts, counts) {
		return fmt.Errorf("%s changed graph counts", scenario.Name)
	}
	return nil
}

func withScenarioError(scenario ScenarioReport, err error) ScenarioReport {
	if err == nil {
		return scenario
	}
	scenario.Status = StatusFailed
	scenario.Error = err.Error()
	return scenario
}

func inspectSource(ctx context.Context, path string) (string, sourceSnapshot, error) {
	if strings.TrimSpace(path) == "" {
		return "", sourceSnapshot{}, fmt.Errorf("GRAFO_BENCH_REPO is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", sourceSnapshot{}, fmt.Errorf("resolve corpus path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", sourceSnapshot{}, fmt.Errorf("GRAFO_BENCH_REPO must name an existing directory")
	}
	root, err := gitOutput(ctx, absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", sourceSnapshot{}, fmt.Errorf("GRAFO_BENCH_REPO must be a Git worktree: %w", err)
	}
	root, err = filepath.EvalSymlinks(strings.TrimSpace(root))
	if err != nil {
		return "", sourceSnapshot{}, fmt.Errorf("resolve Git root: %w", err)
	}
	snapshot, err := snapshotSource(ctx, root)
	return root, snapshot, err
}

func snapshotSource(ctx context.Context, root string) (sourceSnapshot, error) {
	commit, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return sourceSnapshot{}, fmt.Errorf("read corpus commit: %w", err)
	}
	branch, err := gitOutput(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		short, shortErr := gitOutput(ctx, root, "rev-parse", "--short=12", "HEAD")
		if shortErr != nil {
			return sourceSnapshot{}, fmt.Errorf("read corpus branch: %w", err)
		}
		branch = "detached-" + strings.TrimSpace(short)
	}
	status, err := gitOutput(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return sourceSnapshot{}, fmt.Errorf("read corpus status: %w", err)
	}
	return sourceSnapshot{commit: strings.TrimSpace(commit), branch: strings.TrimSpace(branch), status: status}, nil
}

func prepareOutput(source, requested string) (string, error) {
	if requested == "" {
		temporaryRoot, err := canonicalDestination(os.TempDir())
		if err != nil {
			return "", fmt.Errorf("resolve default benchmark output: %w", err)
		}
		if err := validateOutputDestination(source, temporaryRoot); err != nil {
			return "", err
		}
		path, err := os.MkdirTemp("", "grafo-benchmark-")
		if err != nil {
			return "", fmt.Errorf("create benchmark output: %w", err)
		}
		if err := validateOutputDestination(source, path); err != nil {
			if removeErr := os.RemoveAll(path); removeErr != nil {
				return "", errors.Join(err, fmt.Errorf("remove unsafe benchmark output: %w", removeErr))
			}
			return "", err
		}
		return path, nil
	}
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return "", err
	}
	absolute, err = canonicalDestination(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve benchmark output: %w", err)
	}
	if err := validateOutputDestination(source, absolute); err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return "", fmt.Errorf("create benchmark output: %w", err)
	}
	return absolute, nil
}

func validateOutputDestination(source, output string) error {
	relative, err := filepath.Rel(source, output)
	if err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
		return fmt.Errorf("benchmark output must be outside the source checkout")
	}
	if sourceInfo, sourceErr := os.Stat(source); sourceErr == nil {
		if outputInfo, outputErr := os.Stat(output); outputErr == nil && os.SameFile(sourceInfo, outputInfo) {
			return fmt.Errorf("benchmark output must be outside the source checkout")
		}
	}
	return nil
}

func canonicalDestination(path string) (string, error) {
	current := path
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %s", path)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func collectCoverage(ctx context.Context, root string, registry *parserapi.Registry) (InputCoverage, []string, error) {
	output, err := gitOutputBytes(ctx, root, "ls-files", "-z")
	if err != nil {
		return InputCoverage{}, nil, fmt.Errorf("list tracked corpus files: %w", err)
	}
	coverage := InputCoverage{ByLanguage: map[string]InputStats{}, UnsupportedPaths: []string{}, SkippedPaths: []string{}}
	var indexable []string
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		path := filepath.ToSlash(string(raw))
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return coverage, nil, fmt.Errorf("inspect tracked input %s: %w", path, err)
		}
		isSymlink := info.Mode()&os.ModeSymlink != 0
		if !isSymlink && !info.Mode().IsRegular() {
			continue
		}
		coverage.Tracked.Files++
		coverage.Tracked.Bytes += info.Size()
		ignored := indexer.PathIgnored(path)
		parser, ok := registry.For(path)
		if !ok {
			coverage.Unsupported.Files++
			coverage.Unsupported.Bytes += info.Size()
			coverage.UnsupportedPaths = append(coverage.UnsupportedPaths, path)
			if ignored || isSymlink {
				coverage.Skipped.Files++
				coverage.Skipped.Bytes += info.Size()
				coverage.SkippedPaths = append(coverage.SkippedPaths, path)
			}
			if isSymlink {
				coverage.SkippedSymlinks = append(coverage.SkippedSymlinks, path)
			}
			continue
		}
		stats := coverage.ByLanguage[parser.Language()]
		stats.Files++
		stats.Bytes += info.Size()
		coverage.ByLanguage[parser.Language()] = stats
		coverage.Supported.Files++
		coverage.Supported.Bytes += info.Size()
		if ignored {
			coverage.Skipped.Files++
			coverage.Skipped.Bytes += info.Size()
			coverage.SkippedPaths = append(coverage.SkippedPaths, path)
			continue
		}
		coverage.Routed.Files++
		coverage.Routed.Bytes += info.Size()
		if isSymlink {
			coverage.Skipped.Files++
			coverage.Skipped.Bytes += info.Size()
			coverage.SkippedPaths = append(coverage.SkippedPaths, path)
			coverage.SkippedSymlinks = append(coverage.SkippedSymlinks, path)
			continue
		}
		coverage.Indexable.Files++
		coverage.Indexable.Bytes += info.Size()
		indexable = append(indexable, path)
	}
	sort.Strings(coverage.UnsupportedPaths)
	sort.Strings(coverage.SkippedPaths)
	sort.Strings(coverage.SkippedSymlinks)
	sort.Strings(indexable)
	return coverage, indexable, nil
}

func chooseMutationTarget(paths []string, registry *parserapi.Registry) string {
	preferences := []string{".go", ".gd", ".py", ".ts", ".tsx", ".sql", ".yaml", ".yml", ".json"}
	fallback := paths[0]
	for _, extension := range preferences {
		for _, path := range paths {
			if strings.EqualFold(filepath.Ext(path), extension) {
				if len(registry.SemanticAffectedPaths(paths, []string{path})) == 1 {
					return path
				}
				fallback = path
				break
			}
		}
	}
	return fallback
}

func loadBaseline(path string) (Baseline, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Baseline{}, err
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return Baseline{}, fmt.Errorf("read baseline: %w", err)
	}
	var report Report
	if err := json.Unmarshal(content, &report); err != nil {
		return Baseline{}, fmt.Errorf("decode baseline: %w", err)
	}
	if report.SchemaVersion != ReportSchemaVersion || report.SemanticIndexVersion != indexer.SemanticIndexVersion || report.GraphSchemaVersion != graph.SchemaVersion {
		return Baseline{}, fmt.Errorf("baseline is incompatible: report/schema/semantic versions differ")
	}
	if report.Status != StatusPassed {
		return Baseline{}, fmt.Errorf("baseline is invalid: report status is %q, want %q", report.Status, StatusPassed)
	}
	return Baseline{Path: absolute, SchemaVersion: report.SchemaVersion, GrafoCommit: report.GrafoCommit}, nil
}

func enforceBudgets(scenarios []ScenarioReport, options Options) error {
	for _, scenario := range scenarios {
		if options.MaxWALBytes > 0 {
			if !scenario.Resources.PeakWALBytes.Supported || scenario.Resources.PeakWALBytes.Value == nil {
				return fmt.Errorf("cannot enforce WAL budget: metric unsupported in %s", scenario.Name)
			}
			if *scenario.Resources.PeakWALBytes.Value > options.MaxWALBytes {
				return fmt.Errorf("%s peak WAL %d exceeds budget %d", scenario.Name, *scenario.Resources.PeakWALBytes.Value, options.MaxWALBytes)
			}
		}
		if options.MaxRSSBytes > 0 {
			if !scenario.Resources.PeakRSSBytes.Supported || scenario.Resources.PeakRSSBytes.Value == nil {
				return fmt.Errorf("cannot enforce RSS budget: metric unsupported in %s", scenario.Name)
			}
			if *scenario.Resources.PeakRSSBytes.Value > options.MaxRSSBytes {
				return fmt.Errorf("%s peak RSS %d exceeds budget %d", scenario.Name, *scenario.Resources.PeakRSSBytes.Value, options.MaxRSSBytes)
			}
		}
	}
	return nil
}

func buildProvenance() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	commit := "unknown"
	dirty := false
	if ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				commit = setting.Value
			}
			if setting.Key == "vcs.modified" {
				dirty = setting.Value == "true"
			}
		}
	}
	if commit != "unknown" {
		return commit, dirty
	}
	_, sourceFile, _, callerOK := runtime.Caller(0)
	if !callerOK || !filepath.IsAbs(sourceFile) {
		return commit, dirty
	}
	rootOutput, err := gitOutput(context.Background(), filepath.Dir(sourceFile), "rev-parse", "--show-toplevel")
	if err != nil {
		return commit, dirty
	}
	root := strings.TrimSpace(rootOutput)
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !bytes.Contains(module, []byte("module github.com/cafecito-games/grafo")) {
		return commit, dirty
	}
	commitOutput, err := gitOutput(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		return commit, dirty
	}
	status, err := gitOutput(context.Background(), root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return strings.TrimSpace(commitOutput), dirty
	}
	commit = strings.TrimSpace(commitOutput)
	dirty = strings.TrimSpace(status) != ""
	return commit, dirty
}

func writeReport(path string, report Report) error {
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode benchmark report: %w", err)
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".report-*.tmp")
	if err != nil {
		return fmt.Errorf("create report temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace benchmark report: %w", err)
	}
	removeTemporary = false
	return nil
}

func runGit(ctx context.Context, root string, arguments ...string) error {
	_, err := gitOutputBytes(ctx, root, arguments...)
	return err
}

func gitOutput(ctx context.Context, root string, arguments ...string) (string, error) {
	output, err := gitOutputBytes(ctx, root, arguments...)
	return string(output), err
}

func gitOutputBytes(ctx context.Context, root string, arguments ...string) ([]byte, error) {
	commandArguments := arguments
	if root != "" {
		commandArguments = append([]string{"-C", root}, arguments...)
	}
	command := exec.CommandContext(ctx, "git", commandArguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type resourceSampler struct {
	ctx      context.Context
	engine   string
	database string
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	peakWAL  int64
	finalWAL int64
	finalDB  int64
	walOK    bool
	peakRSS  int64
	rssOK    bool
}

func newResourceSampler(ctx context.Context, engine, database string) *resourceSampler {
	return &resourceSampler{ctx: ctx, engine: engine, database: database, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

func (s *resourceSampler) start() {
	s.sample()
	go func() {
		defer close(s.doneCh)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.sample()
			case <-s.stopCh:
				return
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

func (s *resourceSampler) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		<-s.doneCh
		s.sample()
		s.mu.Lock()
		s.finalDB, s.finalWAL, s.walOK = storageSizes(s.engine, s.database)
		s.mu.Unlock()
	})
}

func (s *resourceSampler) sample() {
	s.sampleWAL()
	command := exec.CommandContext(s.ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid()))
	output, err := command.Output()
	if err != nil {
		return
	}
	kibibytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.rssOK = true
	if bytes := kibibytes * 1024; bytes > s.peakRSS {
		s.peakRSS = bytes
	}
	s.mu.Unlock()
}

func (s *resourceSampler) sampleWAL() {
	_, bytes, supported := storageSizes(s.engine, s.database)
	s.mu.Lock()
	s.walOK = supported
	if bytes > s.peakWAL {
		s.peakWAL = bytes
	}
	s.mu.Unlock()
}

func (s *resourceSampler) metrics() ResourceMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	peakWAL, finalWAL := s.peakWAL, s.finalWAL
	metrics := ResourceMetrics{
		PeakWALBytes:  OptionalBytes{Supported: true, Value: &peakWAL},
		FinalWALBytes: OptionalBytes{Supported: true, Value: &finalWAL},
		PeakRSSBytes:  OptionalBytes{Supported: false, Reason: "ps RSS sampling unavailable"},
		DatabaseBytes: OptionalBytes{Supported: true, Value: pointer(s.finalDB)},
	}
	if !s.walOK {
		metrics.PeakWALBytes = OptionalBytes{Supported: false, Reason: "engine has no separate journal or WAL"}
		metrics.FinalWALBytes = OptionalBytes{Supported: false, Reason: "engine has no separate journal or WAL"}
	}
	if s.rssOK {
		peakRSS := s.peakRSS
		metrics.PeakRSSBytes = OptionalBytes{Supported: true, Value: &peakRSS}
	}
	return metrics
}

func pointer(value int64) *int64 { return &value }

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// storageSizes reports an engine's database and journal sizes. Registered
// engines resolve through their declared StorageKind, so an engine delegating
// to sqlite reports real WAL bytes instead of an unsupported metric.
func storageSizes(engine, path string) (database, journal int64, journalSupported bool) {
	kind := effectiveStorageKind(engine)
	switch kind {
	case "sqlite":
		return fileSize(path), fileSize(path + "-wal"), true
	case string(kvbench.EngineBolt):
		return fileSize(path), 0, false
	case string(kvbench.EnginePebble):
		_ = filepath.WalkDir(path, func(candidate string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			info, statErr := entry.Info()
			if statErr != nil {
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".log") {
				journal += info.Size()
			} else {
				database += info.Size()
			}
			return nil
		})
		return database, journal, true
	default:
		return 0, 0, false
	}
}

func removeDatabase(engine, path string) error {
	if effectiveStorageKind(engine) == string(kvbench.EnginePebble) {
		return os.RemoveAll(path)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
