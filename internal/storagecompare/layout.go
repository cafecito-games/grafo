package storagecompare

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cafecito-games/grafo/internal/benchmark"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/storage/layoutbench"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

const (
	LayoutReportSchemaVersion = 2
	LayoutReportFileName      = "storage-layout.json"
	layoutControlName         = "control"
	// layoutDefaultFixtureScale keeps the default run CI-safe while exercising
	// every fixture shape: minimumFileCount population files, a hub fan, and
	// the ambiguity, test, and external probes.
	layoutDefaultFixtureScale = 150
	layoutSizeGateMinimum     = 0.25
	layoutPerfRatioMaximum    = 1.20
	// layoutControlObservation records the standing caveat the issue demands:
	// the Coder workspace observation was never reproduced, so every ratio in
	// this report compares same-machine runs only.
	layoutControlObservation = "uncorroborated, no Coder run performed; owner directed local substitution"
)

const (
	attributionScopeFixturePreCompact  = "fixture-final-pre-compact"
	attributionScopeFixturePostCompact = "fixture-final-post-compact"
	attributionScopeCorpusPreCompact   = "uzir-final-pre-compact"
	attributionScopeCorpusPostCompact  = "uzir-final-post-compact"
)

// LayoutOptions configures one layout comparison run. Zero values take the
// CI-safe defaults; the corpus path opts into the Uzir scenario machinery.
type LayoutOptions struct {
	Repository             string
	Output                 string
	Samples                int
	FixtureSeed            int
	FixtureScale           int
	EquivalenceScale       int
	BatchInterruptionScale int
	QueryRepetitions       int
}

// HostLimits records the resource ceiling the run executed under. Cgroup
// fields stay empty on hosts without cgroup v2 (every darwin machine) and the
// substitution is recorded in Reason rather than guessed.
type HostLimits struct {
	CPUCores             int64  `json:"cpu_cores"`
	MemoryTotalBytes     int64  `json:"memory_total_bytes"`
	DiskFreeBytes        int64  `json:"disk_free_bytes_at_output"`
	CgroupCPUMax         string `json:"cgroup_cpu_max,omitempty"`
	CgroupMemoryMaxBytes int64  `json:"cgroup_memory_max_bytes,omitempty"`
	LimitSource          string `json:"limit_source"`
	Reason               string `json:"reason,omitempty"`
}

// LayoutReport is the schema_version 2 artifact of the storage layout spike.
type LayoutReport struct {
	SchemaVersion        int              `json:"schema_version"`
	GeneratedAt          string           `json:"generated_at"`
	Status               string           `json:"status"`
	Error                string           `json:"error,omitempty"`
	Machine              Machine          `json:"machine"`
	HostLimits           HostLimits       `json:"host_limits"`
	Samples              int              `json:"samples"`
	FixtureRows          int              `json:"fixture_rows"`
	GrafoCommit          string           `json:"grafo_commit"`
	GrafoDirty           bool             `json:"grafo_dirty"`
	Corpus               benchmark.Corpus `json:"corpus"`
	SemanticIndexVersion string           `json:"semantic_index_version"`
	GraphSchemaVersion   int              `json:"graph_schema_version"`
	ControlObservation   string           `json:"coder_observation_disposition"`
	Layouts              []LayoutResult   `json:"layouts"`
	Artifact             string           `json:"-"`
}

// LayoutResult holds one layout's measurements: the control production
// adapter or one candidate schema.
type LayoutResult struct {
	Name        string                    `json:"name"`
	Correctness LayoutCorrectness         `json:"correctness"`
	Fixture     FixtureResult             `json:"fixture"`
	Corpus      CorpusResult              `json:"corpus"`
	Attribution []AttributionSample       `json:"attribution"`
	Plans       []layoutbench.PlanCapture `json:"plans"`
	QuerySuite  []layoutbench.QueryMetric `json:"query_suite"`
	Gates       GateEvaluation            `json:"gates"`
}

// LayoutCorrectness folds the control-versus-candidate equivalence result into
// the report. The control layout is definitionally equivalent to itself.
type LayoutCorrectness struct {
	Valid           bool   `json:"valid"`
	Equivalent      bool   `json:"equivalent"`
	ControlDigest   string `json:"control_digest"`
	VariantDigest   string `json:"variant_digest"`
	ControlProbes   int    `json:"control_probes"`
	VariantProbes   int    `json:"variant_probes"`
	FirstDifference string `json:"first_difference,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// FixtureResult summarizes the generated-fixture samples of one layout.
type FixtureResult struct {
	Counts                   graph.Counts       `json:"counts"`
	ColdPersistenceNS        LayoutMetric       `json:"cold_persistence_ns"`
	ReconciliationNS         LayoutMetric       `json:"reconciliation_ns"`
	ColdTotalNS              LayoutMetric       `json:"cold_total_ns"`
	DatabaseBytesPreCompact  LayoutMetric       `json:"database_bytes_pre_compact"`
	DatabaseBytesPostCompact LayoutMetric       `json:"database_bytes_post_compact"`
	Writes                   []graph.WriteStats `json:"writes"`
}

// LayoutMetric is one measured quantity: the raw per-sample values, their
// median, and the median's ratio against the control layout's median.
type LayoutMetric struct {
	Raw            []int64 `json:"raw"`
	Median         int64   `json:"median"`
	RatioToControl float64 `json:"ratio_to_control"`
}

// AttributionSample is one attribution capture at a named lifecycle point of
// one sample's final database.
type AttributionSample struct {
	Scope   string                  `json:"scope"`
	Sample  int                     `json:"sample"`
	Capture layoutbench.Attribution `json:"capture"`
}

// GateEvaluation applies the issue's retain gates to one layout.
type GateEvaluation struct {
	SizeReduction    float64  `json:"size_reduction_ratio"`
	MeetsSizeGate    bool     `json:"meets_size_gate"`
	MeetsPerfGates   bool     `json:"meets_perf_gates"`
	MeetsCorrectness bool     `json:"meets_correctness_gate"`
	MeetsPlanGate    bool     `json:"meets_plan_gate"`
	Valid            bool     `json:"valid"`
	Reasons          []string `json:"reasons,omitempty"`
}

// layoutDescriptor pairs one layout's identity with its opener and spec.
type layoutDescriptor struct {
	name      string
	engine    string
	spec      layoutbench.LayoutSpec
	opener    layoutbench.RepositoryOpener
	isControl bool
}

func controlLayoutOpener(ctx context.Context, path string) (layoutbench.EquivalenceRepository, error) {
	return sqlite.Open(ctx, path)
}

// layoutDescriptors lists the run's layouts in report order: control first,
// then every candidate.
func layoutDescriptors() []layoutDescriptor {
	slimEdgesSpec := layoutbench.SlimEdgesSpec("slim-edges")
	integerKeysSpec := layoutbench.IntegerKeysSpec("integer-keys")
	indexReformSpec := layoutbench.IndexReformSpec("index-reform")
	return []layoutDescriptor{
		{name: layoutControlName, engine: layoutControlName, spec: layoutbench.ProductionSpec(layoutControlName),
			opener: controlLayoutOpener, isControl: true},
		{name: "slim-edges", engine: "layout:slim-edges", spec: slimEdgesSpec,
			opener: func(ctx context.Context, path string) (layoutbench.EquivalenceRepository, error) {
				return layoutbench.OpenVariant(ctx, slimEdgesSpec, path)
			}},
		{name: "integer-keys", engine: "layout:integer-keys", spec: integerKeysSpec,
			opener: func(ctx context.Context, path string) (layoutbench.EquivalenceRepository, error) {
				return layoutbench.OpenVariant(ctx, integerKeysSpec, path)
			}},
		{name: "index-reform", engine: "layout:index-reform", spec: indexReformSpec,
			opener: func(ctx context.Context, path string) (layoutbench.EquivalenceRepository, error) {
				return layoutbench.OpenPreSeeded(ctx, indexReformSpec.Migrations, path,
					layoutbench.IndexReformClusteredKeyTables())
			}},
	}
}

// registerLayoutEngines routes the layouts through the benchmark scenario
// machinery without internal/benchmark importing layoutbench: the engine
// names resolve through RegisterEngine at run time.
func registerLayoutEngines(descriptors []layoutDescriptor) error {
	for _, descriptor := range descriptors {
		opener := descriptor.opener
		storage := benchmark.Storage{Engine: descriptor.engine, Library: "modernc.org/sqlite", LibraryVersion: "v1.57.0",
			Durability: "WAL with synchronous=NORMAL; atomic file and reconciliation transactions"}
		if err := benchmark.RegisterEngine(descriptor.engine, benchmark.EngineRegistration{Storage: storage,
			Opener: func(ctx context.Context, path string) (graph.Repository, error) {
				return opener(ctx, path)
			}}); err != nil {
			return fmt.Errorf("register engine %s: %w", descriptor.engine, err)
		}
	}
	return nil
}

type layoutFixtureSample struct {
	counts           graph.Counts
	persistenceNS    int64
	reconciliationNS int64
	totalNS          int64
	writes           graph.WriteStats
	preCompact       layoutbench.Attribution
	postCompact      layoutbench.Attribution
	queries          []layoutbench.QueryMetric
}

type layoutCorpusSample struct {
	report      benchmark.Report
	preCompact  layoutbench.Attribution
	postCompact layoutbench.Attribution
	queries     []layoutbench.QueryMetric
}

// RunLayout executes the storage layout comparison and writes the schema-v2
// storage-layout.json artifact. The run is fail-closed: a dirty corpus
// worktree, a failing sample, pending reconciliation at compaction time, or a
// provenance mismatch aborts or rejects ratios rather than degrading the
// medians.
func RunLayout(ctx context.Context, options LayoutOptions) (report LayoutReport, resultErr error) {
	options = options.withDefaults()
	output, err := prepareOutput(options.Output)
	if err != nil {
		return report, err
	}
	grafoCommit, grafoDirty := layoutProvenance(ctx)
	report = LayoutReport{SchemaVersion: LayoutReportSchemaVersion,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: StatusFailed,
		Machine: inspectMachine(), HostLimits: inspectHostLimits(output), Samples: options.Samples,
		GrafoCommit: grafoCommit, GrafoDirty: grafoDirty,
		SemanticIndexVersion: indexer.SemanticIndexVersion, GraphSchemaVersion: graph.SchemaVersion,
		ControlObservation: layoutControlObservation, Layouts: []LayoutResult{},
		Artifact: filepath.Join(output, LayoutReportFileName)}
	defer func() {
		if resultErr != nil {
			report.Status = StatusFailed
			report.Error = resultErr.Error()
		}
		if report.Artifact != "" {
			if writeErr := writeJSONArtifact(report.Artifact, ".storage-layout-*.tmp", report); writeErr != nil && resultErr == nil {
				resultErr = writeErr
			}
		}
	}()

	corpusEnabled := strings.TrimSpace(options.Repository) != ""
	if corpusEnabled {
		corpus, err := requirePinnedCleanWorktree(ctx, options.Repository)
		if err != nil {
			return report, err
		}
		report.Corpus = corpus
	}

	// Every sample's fixture is generated up front so the control and every
	// candidate index byte-identical per-sample input.
	fixtures := make([]layoutbench.Fixture, options.Samples)
	for sample := range options.Samples {
		fixtures[sample] = layoutbench.GenerateFixture(options.FixtureSeed+sample, options.FixtureScale)
	}
	report.FixtureRows = countFixtureRows(fixtures[0])

	descriptors := layoutDescriptors()
	if err := registerLayoutEngines(descriptors); err != nil {
		return report, err
	}
	equivalenceFixture := layoutbench.GenerateFixture(options.FixtureSeed, options.EquivalenceScale)
	batchInterruptionFixture := layoutbench.GenerateFixture(options.FixtureSeed, options.BatchInterruptionScale)

	results := make([]LayoutResult, len(descriptors))
	corpusReports := make([][]benchmark.Report, len(descriptors))
	for index, descriptor := range descriptors {
		result, reports, err := runOneLayout(ctx, descriptor, options, fixtures,
			equivalenceFixture, batchInterruptionFixture, output, corpusEnabled)
		if err != nil {
			return report, err
		}
		results[index] = result
		corpusReports[index] = reports
	}
	finalizeLayoutReport(results, corpusReports, corpusEnabled)
	report.Layouts = results
	_ = os.RemoveAll(filepath.Join(output, "fixture"))
	_ = os.RemoveAll(filepath.Join(output, "equivalence"))
	_ = os.RemoveAll(filepath.Join(output, "corpus"))
	report.Status = StatusPassed
	return report, nil
}

func (options LayoutOptions) withDefaults() LayoutOptions {
	if options.Samples <= 0 {
		options.Samples = 1
	}
	if options.FixtureSeed < 0 {
		options.FixtureSeed = 0
	}
	if options.FixtureScale <= 0 {
		options.FixtureScale = layoutDefaultFixtureScale
	}
	if options.EquivalenceScale <= 0 {
		options.EquivalenceScale = 60
	}
	if options.BatchInterruptionScale <= 0 {
		options.BatchInterruptionScale = 900
	}
	if options.QueryRepetitions <= 0 {
		options.QueryRepetitions = 7
	}
	return options
}

func runOneLayout(ctx context.Context, descriptor layoutDescriptor, options LayoutOptions,
	fixtures []layoutbench.Fixture, equivalenceFixture, batchInterruptionFixture layoutbench.Fixture,
	output string, corpusEnabled bool) (LayoutResult, []benchmark.Report, error) {
	result := LayoutResult{Name: descriptor.name, Correctness: LayoutCorrectness{},
		Fixture:     FixtureResult{Writes: []graph.WriteStats{}},
		Corpus:      CorpusResult{Scenarios: map[string]ScenarioSamples{}},
		Attribution: []AttributionSample{}, Plans: []layoutbench.PlanCapture{},
		QuerySuite: []layoutbench.QueryMetric{}}

	fixtureSamples := make([]layoutFixtureSample, 0, options.Samples)
	for sample := range options.Samples {
		path := filepath.Join(output, "fixture", descriptor.name,
			fmt.Sprintf("sample-%03d", sample+1), "graph.db")
		measured, err := runLayoutFixtureSample(ctx, descriptor, fixtures[sample], path, options.QueryRepetitions)
		if err != nil {
			return result, nil, fmt.Errorf("%s fixture sample %d: %w", descriptor.name, sample+1, err)
		}
		fixtureSamples = append(fixtureSamples, measured)
		result.Attribution = append(result.Attribution,
			AttributionSample{Scope: attributionScopeFixturePreCompact, Sample: sample + 1, Capture: measured.preCompact},
			AttributionSample{Scope: attributionScopeFixturePostCompact, Sample: sample + 1, Capture: measured.postCompact})
		if sample == 0 {
			plans, err := captureLayoutPlans(ctx, descriptor, path)
			if err != nil {
				return result, nil, fmt.Errorf("%s plan capture: %w", descriptor.name, err)
			}
			result.Plans = plans
		}
	}
	result.Fixture = summarizeFixtureSamples(fixtureSamples)
	perSampleQueries := make([][]layoutbench.QueryMetric, len(fixtureSamples))
	for index, measured := range fixtureSamples {
		perSampleQueries[index] = measured.queries
	}
	result.QuerySuite = combineQuerySuites(perSampleQueries)

	result.Correctness = runLayoutEquivalence(ctx, descriptor, equivalenceFixture, batchInterruptionFixture,
		filepath.Join(output, "equivalence", descriptor.name))

	corpusReports := []benchmark.Report{}
	if corpusEnabled {
		for sample := range options.Samples {
			directory := filepath.Join(output, "corpus", descriptor.name, fmt.Sprintf("sample-%03d", sample+1))
			measured, err := runLayoutCorpusSample(ctx, descriptor, options, directory)
			if err != nil {
				return result, corpusReports, fmt.Errorf("%s corpus sample %d: %w", descriptor.name, sample+1, err)
			}
			corpusReports = append(corpusReports, measured.report)
			result.Attribution = append(result.Attribution,
				AttributionSample{Scope: attributionScopeCorpusPreCompact, Sample: sample + 1, Capture: measured.preCompact},
				AttributionSample{Scope: attributionScopeCorpusPostCompact, Sample: sample + 1, Capture: measured.postCompact})
		}
		result.Corpus = summarizeCorpus(corpusReports)
	}
	return result, corpusReports, nil
}

// runLayoutFixtureSample indexes one fixture into one isolated empty database,
// requires zero pending reconciliation, captures attribution before and after
// compaction, and times the query workload over the final compacted database.
func runLayoutFixtureSample(ctx context.Context, descriptor layoutDescriptor, fixture layoutbench.Fixture,
	path string, queryRepetitions int) (layoutFixtureSample, error) {
	measured := layoutFixtureSample{}
	repository, err := descriptor.opener(ctx, path)
	if err != nil {
		return measured, fmt.Errorf("open %s: %w", descriptor.name, err)
	}
	started := time.Now()
	persistenceStarted := time.Now()
	for _, file := range fixture.Files {
		if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
			_ = repository.Close()
			return measured, fmt.Errorf("persist %s: %w", file.Record.Path, err)
		}
	}
	measured.persistenceNS = time.Since(persistenceStarted).Nanoseconds()
	reconciliationStarted := time.Now()
	if err := repository.Reconcile(ctx); err != nil {
		_ = repository.Close()
		return measured, fmt.Errorf("reconcile: %w", err)
	}
	measured.reconciliationNS = time.Since(reconciliationStarted).Nanoseconds()
	measured.totalNS = time.Since(started).Nanoseconds()
	if pending, err := repository.ReconciliationPending(ctx); err != nil {
		_ = repository.Close()
		return measured, fmt.Errorf("read reconciliation status: %w", err)
	} else if pending {
		_ = repository.Close()
		return measured, fmt.Errorf("%s left pending reconciliation after reconcile; refusing to compact", descriptor.name)
	}
	if measured.counts, err = repository.Counts(ctx); err != nil {
		_ = repository.Close()
		return measured, fmt.Errorf("read counts: %w", err)
	}
	measured.writes = repository.WriteStats()
	if err := repository.Close(); err != nil {
		return measured, fmt.Errorf("close %s: %w", descriptor.name, err)
	}

	measured.preCompact, measured.postCompact, err = maintainAndCapture(ctx, path)
	if err != nil {
		return measured, fmt.Errorf("%s maintenance: %w", descriptor.name, err)
	}

	repository, err = descriptor.opener(ctx, path)
	if err != nil {
		return measured, fmt.Errorf("reopen %s: %w", descriptor.name, err)
	}
	queries, suiteErr := layoutbench.RunQuerySuite(ctx, repository,
		layoutbench.BuildQueryWorkload(layoutbench.FixtureWorkloadParameters()), queryRepetitions)
	if closeErr := repository.Close(); closeErr != nil {
		return measured, fmt.Errorf("close %s: %w", descriptor.name, closeErr)
	}
	if suiteErr != nil {
		return measured, fmt.Errorf("%s query workload: %w", descriptor.name, suiteErr)
	}
	measured.queries = queries
	return measured, nil
}

// runLayoutCorpusSample runs the benchmark scenario suite for one layout on
// one isolated clone of the corpus, then measures the retained final database
// exactly like a fixture sample: pending check, checkpoint, pre-compaction
// attribution, compaction, post-compaction attribution, query workload.
func runLayoutCorpusSample(ctx context.Context, descriptor layoutDescriptor, options LayoutOptions,
	directory string) (layoutCorpusSample, error) {
	measured := layoutCorpusSample{}
	report, err := benchmark.Run(ctx, benchmark.Options{Repository: options.Repository, Output: directory,
		Engine: descriptor.engine, CleanupDatabase: false})
	if err != nil {
		return measured, fmt.Errorf("%s corpus benchmark: %w", descriptor.name, err)
	}
	measured.report = report
	database := report.Artifacts.ResumeDatabase
	if database == "" {
		return measured, fmt.Errorf("%s corpus benchmark retained no database", descriptor.name)
	}

	repository, err := descriptor.opener(ctx, database)
	if err != nil {
		return measured, fmt.Errorf("open %s corpus database: %w", descriptor.name, err)
	}
	if pending, err := repository.ReconciliationPending(ctx); err != nil {
		_ = repository.Close()
		return measured, fmt.Errorf("read corpus reconciliation status: %w", err)
	} else if pending {
		_ = repository.Close()
		return measured, fmt.Errorf("%s corpus database left pending reconciliation; refusing to compact", descriptor.name)
	}
	if err := repository.Close(); err != nil {
		return measured, fmt.Errorf("close %s corpus database: %w", descriptor.name, err)
	}

	if measured.preCompact, measured.postCompact, err = maintainAndCapture(ctx, database); err != nil {
		return measured, fmt.Errorf("%s corpus maintenance: %w", descriptor.name, err)
	}

	repository, err = descriptor.opener(ctx, database)
	if err != nil {
		return measured, fmt.Errorf("reopen %s corpus database: %w", descriptor.name, err)
	}
	parameters, deriveErr := layoutbench.DeriveWorkloadParameters(ctx, repository)
	var queries []layoutbench.QueryMetric
	if deriveErr == nil {
		queries, deriveErr = layoutbench.RunQuerySuite(ctx, repository,
			layoutbench.BuildQueryWorkload(parameters), options.QueryRepetitions)
	}
	if closeErr := repository.Close(); closeErr != nil {
		return measured, fmt.Errorf("close %s corpus database: %w", descriptor.name, closeErr)
	}
	if deriveErr != nil {
		return measured, fmt.Errorf("%s corpus query workload: %w", descriptor.name, deriveErr)
	}
	measured.queries = queries
	if err := os.RemoveAll(directory); err != nil {
		return measured, fmt.Errorf("remove summarized corpus sample: %w", err)
	}
	return measured, nil
}

// maintainAndCapture checkpoints the WAL, captures attribution, compacts, and
// captures attribution again, so pre- and post-compaction sizes are separate
// report rows over the same database.
func maintainAndCapture(ctx context.Context, path string) (layoutbench.Attribution, layoutbench.Attribution, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return layoutbench.Attribution{}, layoutbench.Attribution{}, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if err := layoutbench.Checkpoint(ctx, db, true); err != nil {
		return layoutbench.Attribution{}, layoutbench.Attribution{}, err
	}
	preCompact, err := layoutbench.CaptureAttribution(ctx, db, path)
	if err != nil {
		return layoutbench.Attribution{}, layoutbench.Attribution{}, err
	}
	if err := layoutbench.Compact(ctx, db); err != nil {
		return layoutbench.Attribution{}, layoutbench.Attribution{}, err
	}
	if err := layoutbench.Checkpoint(ctx, db, true); err != nil {
		return layoutbench.Attribution{}, layoutbench.Attribution{}, err
	}
	postCompact, err := layoutbench.CaptureAttribution(ctx, db, path)
	if err != nil {
		return layoutbench.Attribution{}, layoutbench.Attribution{}, err
	}
	return preCompact, postCompact, nil
}

func captureLayoutPlans(ctx context.Context, descriptor layoutDescriptor, path string) ([]layoutbench.PlanCapture, error) {
	queries, err := layoutbench.PlanInventoryForSpec(descriptor.spec)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	captures, err := layoutbench.CapturePlans(ctx, db, queries)
	if err != nil {
		return nil, err
	}
	return layoutbench.ValidatePlans(captures), nil
}

// runLayoutEquivalence drives the exact-equivalence scenarios once per
// candidate (not per sample). A scenario error or probe disagreement folds
// into an invalid correctness entry instead of aborting the report, so a
// broken candidate still yields attributable measurements and a failed gate.
func runLayoutEquivalence(ctx context.Context, descriptor layoutDescriptor, fixture, batchFixture layoutbench.Fixture,
	directory string) LayoutCorrectness {
	if descriptor.isControl {
		return LayoutCorrectness{Valid: true, Equivalent: true,
			Reason: "control layout runs the production adapter; equivalence against itself is definitionally satisfied"}
	}
	result, err := layoutbench.RunEquivalence(ctx, layoutbench.EquivalenceInput{
		Control:                  controlLayoutOpener,
		Variant:                  descriptor.opener,
		Fixture:                  fixture,
		BatchInterruptionFixture: batchFixture,
		ControlDirectory:         filepath.Join(directory, "control"),
		VariantDirectory:         filepath.Join(directory, "variant"),
	})
	correctness := LayoutCorrectness{Equivalent: result.Equivalent, ControlDigest: result.ControlDigest,
		VariantDigest: result.VariantDigest, ControlProbes: result.ControlProbes,
		VariantProbes: result.VariantProbes, FirstDifference: result.FirstDifference}
	switch {
	case err != nil:
		correctness.Reason = err.Error()
	case !result.Equivalent:
		correctness.Reason = "probe outputs diverged from the control adapter"
	default:
		correctness.Valid = true
	}
	return correctness
}

func summarizeFixtureSamples(samples []layoutFixtureSample) FixtureResult {
	result := FixtureResult{Writes: make([]graph.WriteStats, 0, len(samples))}
	for _, sample := range samples {
		result.Counts = sample.counts
		result.ColdPersistenceNS.Raw = append(result.ColdPersistenceNS.Raw, sample.persistenceNS)
		result.ReconciliationNS.Raw = append(result.ReconciliationNS.Raw, sample.reconciliationNS)
		result.ColdTotalNS.Raw = append(result.ColdTotalNS.Raw, sample.totalNS)
		result.DatabaseBytesPreCompact.Raw = append(result.DatabaseBytesPreCompact.Raw, sample.preCompact.PrimaryBytes)
		result.DatabaseBytesPostCompact.Raw = append(result.DatabaseBytesPostCompact.Raw, sample.postCompact.PrimaryBytes)
		result.Writes = append(result.Writes, sample.writes)
	}
	finalizeLayoutMetric(&result.ColdPersistenceNS)
	finalizeLayoutMetric(&result.ReconciliationNS)
	finalizeLayoutMetric(&result.ColdTotalNS)
	finalizeLayoutMetric(&result.DatabaseBytesPreCompact)
	finalizeLayoutMetric(&result.DatabaseBytesPostCompact)
	return result
}

// combineQuerySuites reduces per-sample suite medians to one median per
// pattern, preserving the workload's pattern order.
func combineQuerySuites(perSample [][]layoutbench.QueryMetric) []layoutbench.QueryMetric {
	order := []string{}
	medians := map[string][]int64{}
	repetitions := map[string]int{}
	for _, suite := range perSample {
		for _, metric := range suite {
			if _, seen := medians[metric.Pattern]; !seen {
				order = append(order, metric.Pattern)
			}
			medians[metric.Pattern] = append(medians[metric.Pattern], metric.MedianNS)
			repetitions[metric.Pattern] = metric.Repetitions
		}
	}
	combined := make([]layoutbench.QueryMetric, 0, len(order))
	for _, pattern := range order {
		combined = append(combined, layoutbench.QueryMetric{Pattern: pattern,
			Repetitions: repetitions[pattern], MedianNS: medianInt64(medians[pattern])})
	}
	return combined
}

// finalizeLayoutReport computes every ratio against the control layout and
// evaluates the gates. A corpus provenance mismatch between layouts rejects
// the ratios instead of comparing runs of different code.
func finalizeLayoutReport(results []LayoutResult, corpusReports [][]benchmark.Report, corpusEnabled bool) {
	if len(results) == 0 {
		return
	}
	control := &results[0]
	provenanceConsistent := true
	if corpusEnabled && len(corpusReports) > 0 && len(corpusReports[0]) > 0 {
		expected := corpusReports[0][0].Corpus
		for _, reports := range corpusReports {
			for _, report := range reports {
				if report.Corpus.Commit != expected.Commit || report.Corpus.Branch != expected.Branch {
					provenanceConsistent = false
				}
			}
		}
	}
	sizeScope := attributionScopeFixturePostCompact
	if corpusEnabled {
		sizeScope = attributionScopeCorpusPostCompact
	}
	controlSizeBytes := attributionPrimaryMedian(*control, sizeScope)
	for index := range results {
		candidate := &results[index]
		setLayoutRatio(&candidate.Fixture.ColdPersistenceNS, control.Fixture.ColdPersistenceNS.Median)
		setLayoutRatio(&candidate.Fixture.ReconciliationNS, control.Fixture.ReconciliationNS.Median)
		setLayoutRatio(&candidate.Fixture.ColdTotalNS, control.Fixture.ColdTotalNS.Median)
		setLayoutRatio(&candidate.Fixture.DatabaseBytesPreCompact, control.Fixture.DatabaseBytesPreCompact.Median)
		setLayoutRatio(&candidate.Fixture.DatabaseBytesPostCompact, control.Fixture.DatabaseBytesPostCompact.Median)
		applyQuerySuiteRatios(candidate, control)
		if corpusEnabled {
			for name, scenario := range candidate.Corpus.Scenarios {
				controlScenario := control.Corpus.Scenarios[name]
				setRatio(&scenario.TotalNS, controlScenario.TotalNS.Median)
				setRatio(&scenario.PersistenceNS, controlScenario.PersistenceNS.Median)
				setRatio(&scenario.ReconciliationNS, controlScenario.ReconciliationNS.Median)
				setRatio(&scenario.PeakRSSBytes, controlScenario.PeakRSSBytes.Median)
				candidate.Corpus.Scenarios[name] = scenario
			}
		}
		plansValid := true
		for _, plan := range candidate.Plans {
			if !plan.Valid {
				plansValid = false
			}
		}
		candidate.Gates = evaluateGates(layoutGateInput{
			controlSizeBytes:   controlSizeBytes,
			candidateSizeBytes: attributionPrimaryMedian(*candidate, sizeScope),
			sizeScope:          sizeScope,
			perfRatios:         layoutPerfRatios(*candidate, corpusEnabled),
			correctnessValid:   candidate.Correctness.Valid,
			plansValid:         plansValid,
			provenanceRejected: !provenanceConsistent,
		})
	}
}

func applyQuerySuiteRatios(candidate, control *LayoutResult) {
	controlMedians := make(map[string]int64, len(control.QuerySuite))
	for _, metric := range control.QuerySuite {
		controlMedians[metric.Pattern] = metric.MedianNS
	}
	for index := range candidate.QuerySuite {
		controlMedian := controlMedians[candidate.QuerySuite[index].Pattern]
		if controlMedian > 0 {
			candidate.QuerySuite[index].RatioToControl =
				float64(candidate.QuerySuite[index].MedianNS) / float64(controlMedian)
		}
	}
}

// layoutPerfRatios aggregates every ratio the perf gate covers: fixture cold
// total, persistence, reconciliation, each query-suite pattern, and (when the
// corpus ran) each scenario's total and peak RSS.
func layoutPerfRatios(result LayoutResult, corpusEnabled bool) []float64 {
	ratios := []float64{
		result.Fixture.ColdTotalNS.RatioToControl,
		result.Fixture.ColdPersistenceNS.RatioToControl,
		result.Fixture.ReconciliationNS.RatioToControl,
	}
	ratios = append(ratios, querySuiteRatios(result)...)
	if !corpusEnabled {
		return ratios
	}
	names := make([]string, 0, len(result.Corpus.Scenarios))
	for name := range result.Corpus.Scenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		scenario := result.Corpus.Scenarios[name]
		ratios = append(ratios, scenario.TotalNS.RatioToSQLite, scenario.PeakRSSBytes.RatioToSQLite)
	}
	return ratios
}

func querySuiteRatios(result LayoutResult) []float64 {
	ratios := make([]float64, 0, len(result.QuerySuite))
	for _, metric := range result.QuerySuite {
		ratios = append(ratios, metric.RatioToControl)
	}
	return ratios
}

func attributionPrimaryMedian(result LayoutResult, scope string) int64 {
	values := []int64{}
	for _, sample := range result.Attribution {
		if sample.Scope == scope {
			values = append(values, sample.Capture.PrimaryBytes)
		}
	}
	return medianInt64(values)
}

type layoutGateInput struct {
	controlSizeBytes   int64
	candidateSizeBytes int64
	sizeScope          string
	perfRatios         []float64
	correctnessValid   bool
	plansValid         bool
	provenanceRejected bool
}

// evaluateGates applies the retain gates: at least a 25% median size
// reduction, no perf ratio above 1.20 (a missing ratio counts as failed),
// equivalence, and plan validation. Provenance rejection suppresses the size
// ratio as well, because sizes of different-code runs are not comparable.
func evaluateGates(input layoutGateInput) GateEvaluation {
	gates := GateEvaluation{}
	reasons := []string{}
	controlSizeBytes := input.controlSizeBytes
	if input.provenanceRejected {
		controlSizeBytes = 0
		reasons = append(reasons, "corpus provenance mismatch between layouts: ratios rejected")
	}
	if controlSizeBytes > 0 {
		gates.SizeReduction = 1 - float64(input.candidateSizeBytes)/float64(controlSizeBytes)
	} else {
		reasons = append(reasons, fmt.Sprintf("control %s size median unavailable; size reduction rejected", input.sizeScope))
	}
	gates.MeetsSizeGate = gates.SizeReduction >= layoutSizeGateMinimum
	if !gates.MeetsSizeGate {
		reasons = append(reasons, fmt.Sprintf("size reduction %.1f%% is below the 25%% gate on %s primary bytes",
			gates.SizeReduction*100, input.sizeScope))
	}
	gates.MeetsPerfGates = true
	for _, ratio := range input.perfRatios {
		if ratio <= 0 || ratio > layoutPerfRatioMaximum {
			gates.MeetsPerfGates = false
		}
	}
	if input.provenanceRejected {
		gates.MeetsPerfGates = false
	} else if !gates.MeetsPerfGates {
		reasons = append(reasons, "a performance ratio exceeds the 1.20x budget or was unavailable")
	}
	gates.MeetsCorrectness = input.correctnessValid
	if !gates.MeetsCorrectness {
		reasons = append(reasons, "control equivalence failed")
	}
	gates.MeetsPlanGate = input.plansValid
	if !gates.MeetsPlanGate {
		reasons = append(reasons, "query plan validation failed")
	}
	gates.Valid = gates.MeetsSizeGate && gates.MeetsPerfGates && gates.MeetsCorrectness && gates.MeetsPlanGate
	if len(reasons) > 0 {
		gates.Reasons = reasons
	}
	return gates
}

func setLayoutRatio(metric *LayoutMetric, control int64) {
	if control > 0 {
		metric.RatioToControl = float64(metric.Median) / float64(control)
	}
}

func finalizeLayoutMetric(metric *LayoutMetric) {
	if len(metric.Raw) == 0 {
		return
	}
	metric.Median = medianInt64(metric.Raw)
}

func medianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(first, second int) bool { return sorted[first] < sorted[second] })
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func countFixtureRows(fixture layoutbench.Fixture) int {
	rows := 0
	for _, file := range fixture.Files {
		rows += len(file.Parsed.Nodes) + len(file.Parsed.Facts)
	}
	return rows
}

// requirePinnedCleanWorktree fails closed before any sample runs when the
// corpus repository is missing, not a worktree, dirty, or without a pinned
// HEAD: uncommitted corpus changes would invalidate every sample.
func requirePinnedCleanWorktree(ctx context.Context, repository string) (benchmark.Corpus, error) {
	absolute, err := filepath.Abs(repository)
	if err != nil {
		return benchmark.Corpus{}, fmt.Errorf("resolve corpus path: %w", err)
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return benchmark.Corpus{}, fmt.Errorf("corpus repository %s must name an existing directory", repository)
	}
	rootOutput, err := layoutGitOutput(ctx, absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return benchmark.Corpus{}, fmt.Errorf("corpus repository must be a Git worktree: %w", err)
	}
	root := strings.TrimSpace(rootOutput)
	commitOutput, err := layoutGitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return benchmark.Corpus{}, fmt.Errorf("read corpus commit: %w", err)
	}
	commit := strings.TrimSpace(commitOutput)
	if commit == "" {
		return benchmark.Corpus{}, fmt.Errorf("corpus worktree %s has no pinned HEAD", root)
	}
	branch := "detached-" + commit[:min(12, len(commit))]
	if branchOutput, err := layoutGitOutput(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		branch = strings.TrimSpace(branchOutput)
	}
	status, err := layoutGitOutput(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return benchmark.Corpus{}, fmt.Errorf("read corpus status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return benchmark.Corpus{}, fmt.Errorf("corpus worktree %s at %s is dirty; pin or stash every change before benchmarking",
			root, commit)
	}
	return benchmark.Corpus{Path: root, Commit: commit, Branch: branch}, nil
}

func layoutGitOutput(ctx context.Context, directory string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, exit.Stderr)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
	}
	return string(output), nil
}

// layoutProvenance reports the grafo revision the runner itself was built
// from, preferring embedded VCS settings and falling back to Git in the
// source tree for go run invocations.
func layoutProvenance(ctx context.Context) (string, bool) {
	if info, ok := debug.ReadBuildInfo(); ok {
		commit, dirty := "", false
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				commit = setting.Value
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
		if commit != "" {
			return commit, dirty
		}
	}
	_, sourceFile, _, callerOK := runtime.Caller(0)
	if !callerOK {
		return "unknown", false
	}
	// internal/storagecompare -> internal -> repository root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(sourceFile)))
	commitOutput, err := layoutGitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "unknown", false
	}
	commit := strings.TrimSpace(commitOutput)
	if commit == "" {
		return "unknown", false
	}
	status, err := layoutGitOutput(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return commit, true
	}
	return commit, strings.TrimSpace(status) != ""
}

// inspectHostLimits reads the resource ceiling of the run: cgroup v2 quota
// and memory maximum when present, otherwise sysctl hw.ncpu and hw.memsize
// (darwin) or the runtime and /proc defaults, with the substitution recorded.
func inspectHostLimits(output string) HostLimits {
	limits := HostLimits{CPUCores: int64(runtime.NumCPU()), LimitSource: "runtime defaults"}
	if cores, err := sysctlInt64("hw.ncpu"); err == nil && cores > 0 {
		limits.CPUCores = cores
		limits.LimitSource = "sysctl"
	}
	if total, err := sysctlInt64("hw.memsize"); err == nil && total > 0 {
		limits.MemoryTotalBytes = total
		limits.LimitSource = "sysctl"
	}
	if content, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		line := strings.TrimSpace(string(content))
		limits.CgroupCPUMax = line
		if cores, ok := cgroupCPUCores(line); ok {
			limits.CPUCores = cores
			limits.LimitSource = "cgroup v2"
		}
	}
	if content, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		if maximum, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64); err == nil && maximum > 0 {
			limits.CgroupMemoryMaxBytes = maximum
			limits.MemoryTotalBytes = maximum
			limits.LimitSource = "cgroup v2"
		}
	}
	var usage syscall.Statfs_t
	if err := syscall.Statfs(output, &usage); err == nil {
		limits.DiskFreeBytes = int64(usage.Bavail) * int64(usage.Bsize)
	}
	switch {
	case runtime.GOOS == "darwin":
		limits.Reason = "cgroup v2 unavailable on darwin; cpu and memory limits substituted from sysctl hw.ncpu and hw.memsize"
	case limits.LimitSource != "cgroup v2":
		limits.Reason = "cgroup v2 controls not present; cpu and memory limits substituted from sysctl and runtime defaults"
	}
	return limits
}

func sysctlInt64(name string) (int64, error) {
	output, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
}

// cgroupCPUCores converts a cgroup v2 cpu.max line ("quota period", quota
// "max" meaning unlimited) to an effective core count.
func cgroupCPUCores(line string) (int64, bool) {
	quota, period, found := strings.Cut(line, " ")
	if !found || quota == "max" {
		return 0, false
	}
	quotaValue, err := strconv.ParseInt(quota, 10, 64)
	if err != nil || quotaValue <= 0 {
		return 0, false
	}
	periodValue, err := strconv.ParseInt(period, 10, 64)
	if err != nil || periodValue <= 0 {
		return 0, false
	}
	return (quotaValue + periodValue - 1) / periodValue, true
}
