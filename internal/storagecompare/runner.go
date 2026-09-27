// Package storagecompare runs the benchmark-only embedded storage spike.
package storagecompare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/cafecito-games/grafo/internal/benchmark"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/kvbench"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

const (
	ReportSchemaVersion = 1
	ReportFileName      = "storage-comparison.json"
	StatusPassed        = "passed"
	StatusFailed        = "failed"
)

type Options struct {
	Repository    string
	Output        string
	Samples       int
	GeneratedRows int
}

type Report struct {
	SchemaVersion        int                     `json:"schema_version"`
	GeneratedAt          string                  `json:"generated_at"`
	Status               string                  `json:"status"`
	Error                string                  `json:"error,omitempty"`
	Machine              Machine                 `json:"machine"`
	Samples              int                     `json:"samples"`
	GeneratedRows        int                     `json:"generated_rows"`
	GrafoCommit          string                  `json:"grafo_commit"`
	GrafoDirty           bool                    `json:"grafo_dirty"`
	SemanticIndexVersion string                  `json:"semantic_index_version"`
	GraphSchemaVersion   int                     `json:"graph_schema_version"`
	Corpus               benchmark.Corpus        `json:"corpus"`
	Inputs               benchmark.InputCoverage `json:"inputs"`
	Engines              []EngineResult          `json:"engines"`
	Artifact             string                  `json:"-"`
}

type Machine struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"architecture"`
	Go       string `json:"go_version"`
	CPU      string `json:"cpu,omitempty"`
}

type EngineResult struct {
	Storage     benchmark.Storage `json:"storage"`
	Correctness Correctness       `json:"correctness"`
	Generated   GeneratedResult   `json:"generated_fixture"`
	Corpus      CorpusResult      `json:"corpus"`
}

type Correctness struct {
	Valid                    bool   `json:"valid"`
	GraphCountsEquivalent    bool   `json:"graph_counts_equivalent"`
	QuerySemanticsEquivalent bool   `json:"query_semantics_equivalent"`
	RestartResumePassed      bool   `json:"restart_resume_passed"`
	Reason                   string `json:"reason,omitempty"`
}

type MetricSamples struct {
	Raw           []int64 `json:"raw"`
	Median        int64   `json:"median"`
	RatioToSQLite float64 `json:"ratio_to_sqlite"`
}

type GeneratedResult struct {
	Counts            graph.Counts       `json:"counts"`
	QueryDigest       string             `json:"query_digest"`
	ColdPersistenceNS MetricSamples      `json:"cold_persistence_ns"`
	ReconciliationNS  MetricSamples      `json:"reconciliation_ns"`
	ColdTotalNS       MetricSamples      `json:"cold_total_ns"`
	DatabaseBytes     MetricSamples      `json:"database_bytes"`
	Writes            []graph.WriteStats `json:"writes"`
}

type CorpusResult struct {
	Scenarios map[string]ScenarioSamples `json:"scenarios"`
}

type ScenarioSamples struct {
	Counts            graph.Counts       `json:"counts"`
	TotalNS           MetricSamples      `json:"total_ns"`
	PersistenceNS     MetricSamples      `json:"persistence_ns"`
	ReconciliationNS  MetricSamples      `json:"reconciliation_ns"`
	DatabaseBytes     MetricSamples      `json:"database_bytes"`
	AuxiliaryLogBytes MetricSamples      `json:"auxiliary_log_bytes"`
	PeakRSSBytes      MetricSamples      `json:"peak_rss_bytes"`
	Writes            []graph.WriteStats `json:"writes"`
}

type generatedSample struct {
	counts         graph.Counts
	digest         string
	persistence    int64
	reconciliation int64
	total          int64
	database       int64
	writes         graph.WriteStats
}

func Run(ctx context.Context, options Options) (report Report, resultErr error) {
	if options.Samples <= 0 {
		options.Samples = 3
	}
	if options.GeneratedRows <= 0 {
		options.GeneratedRows = 5_000
	}
	output, err := prepareOutput(options.Output)
	if err != nil {
		return report, err
	}
	report = Report{SchemaVersion: ReportSchemaVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Status: StatusFailed, Machine: inspectMachine(), Samples: options.Samples, GeneratedRows: options.GeneratedRows,
		Engines: []EngineResult{}, Artifact: filepath.Join(output, ReportFileName)}
	defer func() {
		if resultErr != nil {
			report.Status = StatusFailed
			report.Error = resultErr.Error()
		}
		if report.Artifact != "" {
			if err := writeReport(report.Artifact, report); resultErr == nil && err != nil {
				resultErr = err
			}
		}
	}()

	engines := []string{"sqlite", string(kvbench.EngineBolt), string(kvbench.EnginePebble)}
	allCorpusReports := make(map[string][]benchmark.Report, len(engines))
	allGenerated := make(map[string][]generatedSample, len(engines))
	for _, engine := range engines {
		for sample := 0; sample < options.Samples; sample++ {
			generated, err := runGeneratedSample(ctx, engine, options.GeneratedRows, sample, filepath.Join(output, "generated", engine, fmt.Sprintf("sample-%03d", sample+1)))
			if err != nil {
				return report, fmt.Errorf("%s generated sample %d: %w", engine, sample+1, err)
			}
			allGenerated[engine] = append(allGenerated[engine], generated)

			runOutput := filepath.Join(output, "corpus", engine, fmt.Sprintf("sample-%03d", sample+1))
			corpusReport, err := benchmark.Run(ctx, benchmark.Options{Repository: options.Repository, Output: runOutput,
				Engine: engine, CleanupDatabase: true})
			if err != nil {
				return report, fmt.Errorf("%s corpus sample %d: %w", engine, sample+1, err)
			}
			allCorpusReports[engine] = append(allCorpusReports[engine], corpusReport)
			if report.Corpus.Commit == "" {
				report.Corpus = corpusReport.Corpus
				report.Inputs = corpusReport.Inputs
				report.GrafoCommit = corpusReport.GrafoCommit
				report.GrafoDirty = corpusReport.GrafoDirty
				report.SemanticIndexVersion = corpusReport.SemanticIndexVersion
				report.GraphSchemaVersion = corpusReport.GraphSchemaVersion
			} else if report.Corpus.Commit != corpusReport.Corpus.Commit || report.Corpus.Branch != corpusReport.Corpus.Branch {
				return report, errors.New("corpus revision changed between samples")
			}
			if err := os.RemoveAll(runOutput); err != nil {
				return report, fmt.Errorf("remove summarized corpus sample: %w", err)
			}
		}
	}

	controlGenerated := allGenerated["sqlite"]
	controlCorpus := allCorpusReports["sqlite"]
	for _, engine := range engines {
		result := summarizeEngine(allGenerated[engine], allCorpusReports[engine])
		result.Correctness = validateEngine(allGenerated[engine], allCorpusReports[engine], controlGenerated, controlCorpus)
		report.Engines = append(report.Engines, result)
	}
	applyRatios(report.Engines)
	_ = os.RemoveAll(filepath.Join(output, "generated"))
	_ = os.RemoveAll(filepath.Join(output, "corpus"))
	for _, engine := range report.Engines {
		if !engine.Correctness.Valid {
			return report, fmt.Errorf("%s failed correctness validation: %s", engine.Storage.Engine, engine.Correctness.Reason)
		}
	}
	report.Status = StatusPassed
	return report, nil
}

func runGeneratedSample(ctx context.Context, engine string, rows, sample int, path string) (generatedSample, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return generatedSample{}, err
	}
	repository, err := openRepository(ctx, engine, path)
	if err != nil {
		return generatedSample{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = repository.Close()
		}
	}()
	nodes := make([]graph.Node, rows)
	facts := make([]graph.Fact, rows)
	for index := range rows {
		id := fmt.Sprintf("n:generated-%06d", index)
		factID := fmt.Sprintf("f:generated-%06d", index)
		nodes[index] = graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Function%06d", index),
			QualifiedName: fmt.Sprintf("generated.Function%06d", index), Language: "go", OwnerFile: "generated.go",
			Location: graph.Location{Path: "generated.go", Line: index + 1}, Properties: map[string]string{"fixture": "deterministic"}}
		facts[index] = graph.Fact{ID: factID, FromID: id, Kind: graph.EdgeCalls, TargetID: id, OwnerFile: "generated.go",
			Location: graph.Location{Path: "generated.go", Line: index + 1}, Properties: map[string]string{"fixture": "deterministic"}}
	}
	if sample%2 == 1 {
		reverseNodes(nodes)
		reverseFacts(facts)
	}
	file := graph.FileRecord{Path: "generated.go", Hash: "deterministic", Language: "go", Size: int64(rows), ModifiedNS: 1, IndexedAt: "2026-01-01T00:00:00Z"}
	started := time.Now()
	persistenceStarted := time.Now()
	if err := repository.ReplaceFile(ctx, file, graph.ParseResult{Nodes: nodes, Facts: facts}); err != nil {
		return generatedSample{}, err
	}
	persistence := time.Since(persistenceStarted).Nanoseconds()
	reconciliationStarted := time.Now()
	if err := repository.Reconcile(ctx); err != nil {
		return generatedSample{}, err
	}
	reconciliation := time.Since(reconciliationStarted).Nanoseconds()
	total := time.Since(started).Nanoseconds()
	counts, err := repository.Counts(ctx)
	if err != nil {
		return generatedSample{}, err
	}
	writes := graph.WriteStats{}
	if instrumented, ok := repository.(graph.InstrumentedWriteRepository); ok {
		writes = instrumented.WriteStats()
	}
	if err := repository.Close(); err != nil {
		return generatedSample{}, err
	}
	closed = true
	database := pathSize(path)

	repository, err = openRepository(ctx, engine, path)
	if err != nil {
		return generatedSample{}, err
	}
	closed = false
	digest, err := semanticProbe(ctx, repository)
	if err != nil {
		return generatedSample{}, err
	}
	if err := repository.Close(); err != nil {
		return generatedSample{}, err
	}
	closed = true
	if err := removeDatabase(engine, path); err != nil {
		return generatedSample{}, err
	}
	return generatedSample{counts: counts, digest: digest, persistence: persistence, reconciliation: reconciliation,
		total: total, database: database, writes: writes}, nil
}

func semanticProbe(ctx context.Context, repository graph.Repository) (string, error) {
	source := graph.Node{ID: "n:probe-source", Kind: graph.KindFunction, Name: "Source", QualifiedName: "probe.Source", OwnerFile: "probe.go"}
	duplicates := []graph.Node{
		{ID: "n:probe-duplicate-a", Kind: graph.KindFunction, Name: "Duplicate", QualifiedName: "probe.Duplicate", OwnerFile: "duplicates.go"},
		{ID: "n:probe-duplicate-b", Kind: graph.KindFunction, Name: "Duplicate", QualifiedName: "other.Duplicate", OwnerFile: "duplicates.go"},
	}
	facts := []graph.Fact{
		{ID: "f:probe-ambiguous", FromID: source.ID, Kind: graph.EdgeCalls, Target: "Duplicate", TargetKind: graph.KindFunction, OwnerFile: "probe.go"},
		{ID: "f:probe-missing", FromID: source.ID, Kind: graph.EdgeCalls, Target: "probe.Missing", TargetKind: graph.KindFunction, OwnerFile: "probe.go"},
	}
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "probe.go", Hash: "probe"}, graph.ParseResult{Nodes: []graph.Node{source}, Facts: facts}); err != nil {
		return "", err
	}
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "duplicates.go", Hash: "duplicates"}, graph.ParseResult{Nodes: duplicates}); err != nil {
		return "", err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return "", err
	}
	before, err := repository.EdgesFrom(ctx, source.ID)
	if err != nil {
		return "", err
	}
	missing := graph.Node{ID: "n:probe-missing", Kind: graph.KindFunction, Name: "Missing", QualifiedName: "probe.Missing", OwnerFile: "missing.go"}
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "missing.go", Hash: "missing"}, graph.ParseResult{Nodes: []graph.Node{missing}}); err != nil {
		return "", err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return "", err
	}
	after, err := repository.EdgesFrom(ctx, source.ID)
	if err != nil {
		return "", err
	}
	search, err := repository.SearchNodes(ctx, "Duplicate", 100)
	if err != nil {
		return "", err
	}
	matches := make([]graph.NodeMatchGroup, 0, 3)
	for _, request := range []graph.NodeMatchQuery{
		{Selector: "probe.Source"},
		{Selector: "Duplicate", Kind: graph.KindFunction},
		{Selector: "missing", Limit: 1},
	} {
		match, err := repository.MatchNodes(ctx, request)
		if err != nil {
			return "", err
		}
		matches = append(matches, match)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Before, After []graph.Edge
		Search        []graph.Node
		Matches       []graph.NodeMatchGroup
		Counts        graph.Counts
	}{before, after, search, matches, counts})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func summarizeEngine(generated []generatedSample, corpus []benchmark.Report) EngineResult {
	result := EngineResult{Storage: corpus[0].Storage, Corpus: CorpusResult{Scenarios: map[string]ScenarioSamples{}}}
	for _, sample := range generated {
		result.Generated.Counts = sample.counts
		result.Generated.QueryDigest = sample.digest
		result.Generated.ColdPersistenceNS.Raw = append(result.Generated.ColdPersistenceNS.Raw, sample.persistence)
		result.Generated.ReconciliationNS.Raw = append(result.Generated.ReconciliationNS.Raw, sample.reconciliation)
		result.Generated.ColdTotalNS.Raw = append(result.Generated.ColdTotalNS.Raw, sample.total)
		result.Generated.DatabaseBytes.Raw = append(result.Generated.DatabaseBytes.Raw, sample.database)
		result.Generated.Writes = append(result.Generated.Writes, sample.writes)
	}
	finalizeGenerated(&result.Generated)
	for _, report := range corpus {
		for _, scenario := range report.Scenarios {
			summary := result.Corpus.Scenarios[scenario.Name]
			summary.Counts = scenario.Index.Counts
			summary.TotalNS.Raw = append(summary.TotalNS.Raw, scenario.Index.Phases.TotalNS)
			summary.PersistenceNS.Raw = append(summary.PersistenceNS.Raw, scenario.Index.Phases.PersistenceNS)
			summary.ReconciliationNS.Raw = append(summary.ReconciliationNS.Raw, scenario.Index.Phases.ReconciliationNS)
			summary.DatabaseBytes.Raw = appendOptional(summary.DatabaseBytes.Raw, scenario.Resources.DatabaseBytes)
			summary.AuxiliaryLogBytes.Raw = appendOptional(summary.AuxiliaryLogBytes.Raw, scenario.Resources.FinalWALBytes)
			summary.PeakRSSBytes.Raw = appendOptional(summary.PeakRSSBytes.Raw, scenario.Resources.PeakRSSBytes)
			summary.Writes = append(summary.Writes, scenario.Index.Writes)
			result.Corpus.Scenarios[scenario.Name] = summary
		}
	}
	for name, scenario := range result.Corpus.Scenarios {
		finalizeScenario(&scenario)
		result.Corpus.Scenarios[name] = scenario
	}
	return result
}

func validateEngine(generated []generatedSample, corpus []benchmark.Report, controlGenerated []generatedSample, controlCorpus []benchmark.Report) Correctness {
	result := Correctness{GraphCountsEquivalent: true, QuerySemanticsEquivalent: true, RestartResumePassed: true}
	if len(generated) != len(controlGenerated) || len(corpus) != len(controlCorpus) {
		result.Reason = "sample count differs from control"
		return result
	}
	for index := range generated {
		if !countsEqual(generated[index].counts, controlGenerated[index].counts) {
			result.GraphCountsEquivalent = false
		}
		if generated[index].digest != controlGenerated[index].digest {
			result.QuerySemanticsEquivalent = false
		}
		candidateScenarios := scenariosByName(corpus[index])
		controlScenarios := scenariosByName(controlCorpus[index])
		for name, control := range controlScenarios {
			candidate, ok := candidateScenarios[name]
			if !ok || !countsEqual(candidate.Index.Counts, control.Index.Counts) {
				result.GraphCountsEquivalent = false
			}
		}
		if resumed, ok := candidateScenarios["resumed"]; !ok || !countsEqual(resumed.Index.Counts, candidateScenarios["cold"].Index.Counts) {
			result.RestartResumePassed = false
		}
	}
	result.Valid = result.GraphCountsEquivalent && result.QuerySemanticsEquivalent && result.RestartResumePassed
	if !result.Valid {
		result.Reason = "graph counts, query digest, or restart convergence differed from SQLite"
	}
	return result
}

func applyRatios(engines []EngineResult) {
	if len(engines) == 0 {
		return
	}
	control := &engines[0]
	for index := range engines {
		candidate := &engines[index]
		setRatio(&candidate.Generated.ColdPersistenceNS, control.Generated.ColdPersistenceNS.Median)
		setRatio(&candidate.Generated.ReconciliationNS, control.Generated.ReconciliationNS.Median)
		setRatio(&candidate.Generated.ColdTotalNS, control.Generated.ColdTotalNS.Median)
		setRatio(&candidate.Generated.DatabaseBytes, control.Generated.DatabaseBytes.Median)
		for name, scenario := range candidate.Corpus.Scenarios {
			controlScenario := control.Corpus.Scenarios[name]
			setRatio(&scenario.TotalNS, controlScenario.TotalNS.Median)
			setRatio(&scenario.PersistenceNS, controlScenario.PersistenceNS.Median)
			setRatio(&scenario.ReconciliationNS, controlScenario.ReconciliationNS.Median)
			setRatio(&scenario.DatabaseBytes, controlScenario.DatabaseBytes.Median)
			setRatio(&scenario.AuxiliaryLogBytes, controlScenario.AuxiliaryLogBytes.Median)
			setRatio(&scenario.PeakRSSBytes, controlScenario.PeakRSSBytes.Median)
			candidate.Corpus.Scenarios[name] = scenario
		}
	}
}

func finalizeGenerated(result *GeneratedResult) {
	finalize(&result.ColdPersistenceNS)
	finalize(&result.ReconciliationNS)
	finalize(&result.ColdTotalNS)
	finalize(&result.DatabaseBytes)
}
func finalizeScenario(result *ScenarioSamples) {
	finalize(&result.TotalNS)
	finalize(&result.PersistenceNS)
	finalize(&result.ReconciliationNS)
	finalize(&result.DatabaseBytes)
	finalize(&result.AuxiliaryLogBytes)
	finalize(&result.PeakRSSBytes)
}
func finalize(metric *MetricSamples) {
	if len(metric.Raw) == 0 {
		return
	}
	values := append([]int64(nil), metric.Raw...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	middle := len(values) / 2
	metric.Median = values[middle]
	if len(values)%2 == 0 {
		metric.Median = (values[middle-1] + values[middle]) / 2
	}
}
func setRatio(metric *MetricSamples, control int64) {
	if control > 0 {
		metric.RatioToSQLite = float64(metric.Median) / float64(control)
	}
}
func appendOptional(values []int64, metric benchmark.OptionalBytes) []int64 {
	if metric.Supported && metric.Value != nil {
		return append(values, *metric.Value)
	}
	return values
}
func scenariosByName(report benchmark.Report) map[string]benchmark.ScenarioReport {
	result := map[string]benchmark.ScenarioReport{}
	for _, scenario := range report.Scenarios {
		result[scenario.Name] = scenario
	}
	return result
}
func countsEqual(left, right graph.Counts) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}
func reverseNodes(values []graph.Node) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}
func reverseFacts(values []graph.Fact) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func openRepository(ctx context.Context, engine, path string) (graph.Repository, error) {
	if engine == "sqlite" {
		return sqlite.Open(ctx, path)
	}
	return kvbench.Open(ctx, kvbench.Engine(engine), path, kvbench.Options{ReconciliationBatchSize: 1_000})
}
func removeDatabase(engine, path string) error {
	if engine == string(kvbench.EnginePebble) {
		return os.RemoveAll(path)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func pathSize(path string) int64 {
	var size int64
	_ = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			size += info.Size()
		}
		return nil
	})
	if size == 0 {
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
	}
	return size
}

func prepareOutput(requested string) (string, error) {
	if requested == "" {
		return os.MkdirTemp("", "grafo-storage-comparison-")
	}
	absolute, err := filepath.Abs(requested)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return "", err
	}
	return absolute, nil
}
func inspectMachine() Machine {
	hostname, _ := os.Hostname()
	cpu := ""
	if output, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		cpu = string(output)
		for len(cpu) > 0 && (cpu[len(cpu)-1] == '\n' || cpu[len(cpu)-1] == '\r') {
			cpu = cpu[:len(cpu)-1]
		}
	}
	return Machine{Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), CPU: cpu}
}
func writeReport(path string, report Report) error {
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".storage-comparison-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
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
	return os.Rename(temporaryPath, path)
}
