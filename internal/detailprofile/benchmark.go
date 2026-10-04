package detailprofile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/cafecito-games/grafo/internal/projectconfig"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

const BenchmarkSchema = "grafo.detail-profile-benchmark/v1"

type BenchmarkOptions struct {
	Repository      string
	Output          string
	Profile         Profile
	FullDetailRoots []string
	Samples         int
	Baseline        string
	KeepDatabases   bool
	// AllowDirtyGrafo exists only so repository tests can exercise the harness
	// while their own uncommitted source is under test. The command never sets it.
	AllowDirtyGrafo bool
}

type BenchmarkReport struct {
	Schema       string                   `json:"schema"`
	GeneratedAt  string                   `json:"generated_at"`
	Grafo        GitProvenance            `json:"grafo"`
	Corpus       GitProvenance            `json:"corpus"`
	Environment  BenchmarkEnvironment     `json:"environment"`
	Profile      Profile                  `json:"profile"`
	ProfileKey   string                   `json:"profile_key"`
	Capabilities []Capability             `json:"capabilities"`
	Scope        ScopeEvidence            `json:"scope"`
	Matrix       EvidenceMatrix           `json:"matrix"`
	Samples      []DetailSample           `json:"samples"`
	Summary      DetailSummary            `json:"summary"`
	Equivalence  map[Capability]string    `json:"capability_fingerprints"`
	Comparison   *ProfileComparison       `json:"comparison,omitempty"`
	Artifacts    DetailBenchmarkArtifacts `json:"artifacts"`
}

type GitProvenance struct {
	Path   string `json:"path"`
	Commit string `json:"commit"`
	Branch string `json:"branch"`
	Remote string `json:"remote,omitempty"`
	Dirty  bool   `json:"dirty"`
}

type BenchmarkEnvironment struct {
	CPUQuota                 string `json:"cpu_quota"`
	EffectiveCPUs            int64  `json:"effective_cpus"`
	MemoryMaxBytes           int64  `json:"memory_max_bytes"` // -1 preserves the cgroup "max" sentinel.
	Filesystem               string `json:"filesystem"`
	FilesystemDevice         uint64 `json:"filesystem_device"`
	FilesystemAvailableBytes uint64 `json:"filesystem_available_bytes"`
	OutputPath               string `json:"output_path"`
	RSSMethod                string `json:"rss_method"`
}

type ScopeEvidence struct {
	SemanticKey string   `json:"semantic_key"`
	Include     []string `json:"include"`
	Exclude     []string `json:"exclude"`
	Accounting  string   `json:"accounting"`
}

type DetailSample struct {
	Number         int                   `json:"number"`
	Database       string                `json:"database,omitempty"`
	Cold           DetailRun             `json:"cold"`
	Incremental    DetailRun             `json:"incremental"`
	CompactedBytes int64                 `json:"compacted_primary_bytes"`
	FinalWALBytes  int64                 `json:"final_wal_bytes"`
	PeakRSSBytes   int64                 `json:"peak_rss_bytes"`
	Projection     ProjectionCounts      `json:"projection"`
	Fingerprints   map[Capability]string `json:"capability_fingerprints"`
}

type DetailRun struct {
	ElapsedNS int64          `json:"elapsed_ns"`
	Report    indexer.Report `json:"index"`
}

type ProjectionCounts struct {
	InputNodes            int            `json:"input_nodes"`
	OutputNodes           int            `json:"output_nodes"`
	InputFacts            int            `json:"input_facts"`
	OutputFacts           int            `json:"output_facts"`
	InputNodesByKind      map[string]int `json:"input_nodes_by_kind"`
	OutputNodesByKind     map[string]int `json:"output_nodes_by_kind"`
	InputFactsByKind      map[string]int `json:"input_facts_by_kind"`
	OutputFactsByKind     map[string]int `json:"output_facts_by_kind"`
	InputFactsByProducer  map[string]int `json:"input_facts_by_producer"`
	OutputFactsByProducer map[string]int `json:"output_facts_by_producer"`
}

type DetailSummary struct {
	Samples                 int   `json:"samples"`
	MedianColdNS            int64 `json:"median_cold_ns"`
	MedianIncrementalNS     int64 `json:"median_incremental_ns"`
	MedianCompactedBytes    int64 `json:"median_compacted_primary_bytes"`
	MedianPeakRSSBytes      int64 `json:"median_peak_rss_bytes"`
	OwnerObservationMinutes int   `json:"owner_observation_minutes"`
	CorroboratesObservation bool  `json:"corroborates_owner_observation"`
}

type ProfileComparison struct {
	BaselineProfile              Profile `json:"baseline_profile"`
	BaselineCommit               string  `json:"baseline_grafo_commit"`
	SizeSavingsPercent           float64 `json:"size_savings_percent"`
	ColdRegressionPercent        float64 `json:"cold_regression_percent"`
	IncrementalRegressionPercent float64 `json:"incremental_regression_percent"`
	RSSRegressionPercent         float64 `json:"rss_regression_percent"`
	ClaimedEquivalent            bool    `json:"claimed_capabilities_equivalent"`
	SizeGatePassed               bool    `json:"size_gate_passed"`
	ColdGatePassed               bool    `json:"cold_gate_passed"`
	IncrementalGatePassed        bool    `json:"incremental_gate_passed"`
	RSSGatePassed                bool    `json:"rss_gate_passed"`
	Recommendation               string  `json:"recommendation"`
}

type DetailBenchmarkArtifacts struct {
	Report string `json:"report"`
	RawDir string `json:"raw_directory"`
}

func RunBenchmark(ctx context.Context, options BenchmarkOptions) (BenchmarkReport, error) {
	matrix := Matrix()
	if err := matrix.Validate(); err != nil {
		return BenchmarkReport{}, err
	}
	if options.Samples <= 0 {
		options.Samples = 1
	}
	if options.Profile == "" {
		options.Profile = ProfileFull
	}
	if options.Profile != ProfileFull && options.Profile != ProfileStructural && options.Profile != ProfileScopedFull {
		return BenchmarkReport{}, fmt.Errorf("unknown profile %q", options.Profile)
	}
	corpus, err := inspectGit(ctx, options.Repository)
	if err != nil {
		return BenchmarkReport{}, fmt.Errorf("inspect corpus: %w", err)
	}
	if corpus.Dirty {
		return BenchmarkReport{}, fmt.Errorf("corpus %s is dirty; refusing incomparable measurement", corpus.Path)
	}
	output, err := filepath.Abs(options.Output)
	if err != nil || strings.TrimSpace(options.Output) == "" {
		return BenchmarkReport{}, fmt.Errorf("GRAFO_BENCH_OUTPUT is required")
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return BenchmarkReport{}, err
	}
	if insidePath(corpus.Path, output) {
		return BenchmarkReport{}, fmt.Errorf("benchmark output must be outside the corpus")
	}
	configuration, err := projectconfig.Load(corpus.Path)
	if err != nil {
		return BenchmarkReport{}, err
	}
	projector, err := NewProjector(Options{Profile: options.Profile, Membership: configuration.Index, FullDetailRoots: options.FullDetailRoots})
	if err != nil {
		return BenchmarkReport{}, err
	}
	grafo, err := inspectGit(ctx, ".")
	if err != nil {
		return BenchmarkReport{}, fmt.Errorf("inspect Grafo: %w", err)
	}
	if grafo.Dirty && !options.AllowDirtyGrafo {
		return BenchmarkReport{}, fmt.Errorf("grafo worktree %s is dirty; refusing incomparable measurement", grafo.Path)
	}
	environment, err := inspectEnvironment(output)
	if err != nil {
		return BenchmarkReport{}, err
	}
	rawDir := filepath.Join(output, "raw-"+string(options.Profile))
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return BenchmarkReport{}, err
	}
	report := BenchmarkReport{
		Schema: BenchmarkSchema, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Grafo: grafo, Corpus: corpus,
		Environment: environment, Profile: options.Profile, ProfileKey: projector.SemanticKey(),
		Capabilities: sortedCapabilities(ProfileCapabilities(options.Profile)), Matrix: matrix,
		Scope: ScopeEvidence{SemanticKey: configuration.Index.SemanticKey(), Include: configuration.Index.Include,
			Exclude: configuration.Index.Exclude, Accounting: "index scope is applied before detail projection; scoped_out is reported separately and excluded from fidelity ratios"},
		Artifacts: DetailBenchmarkArtifacts{Report: filepath.Join(output, string(options.Profile)+"-report.json"), RawDir: rawDir},
	}
	for sampleNumber := 1; sampleNumber <= options.Samples; sampleNumber++ {
		sample, runErr := runDetailSample(ctx, corpus.Path, rawDir, sampleNumber, projector, options.KeepDatabases)
		if runErr != nil {
			return report, runErr
		}
		report.Samples = append(report.Samples, sample)
	}
	report.Summary = summarizeSamples(report.Samples)
	if len(report.Samples) > 0 {
		report.Equivalence = report.Samples[0].Fingerprints
	}
	for _, sample := range report.Samples[1:] {
		if !equalFingerprints(report.Equivalence, sample.Fingerprints) {
			return report, fmt.Errorf("profile fingerprints changed across identical samples")
		}
	}
	if options.Baseline != "" {
		baseline, loadErr := loadBenchmarkReport(options.Baseline)
		if loadErr != nil {
			return report, loadErr
		}
		comparison, compareErr := compareBenchmark(baseline, report)
		if compareErr != nil {
			return report, compareErr
		}
		report.Comparison = &comparison
	}
	if err := writeBenchmarkReport(report.Artifacts.Report, report); err != nil {
		return report, err
	}
	return report, nil
}

func runDetailSample(ctx context.Context, root, rawDir string, number int, projector *Projector, keep bool) (sample DetailSample, returnErr error) {
	database := filepath.Join(rawDir, fmt.Sprintf("sample-%02d.sqlite", number))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(database + suffix)
	}
	if !keep {
		defer func() { returnErr = errors.Join(returnErr, removeSQLiteFiles(database)) }()
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		return DetailSample{}, err
	}
	project.IndexPath = database
	repository, err := sqlite.Open(ctx, database)
	if err != nil {
		return DetailSample{}, err
	}
	recorder := newRecordingTransform(projector)
	rss := newRSSSampler()
	rss.start()
	coldStarted := time.Now()
	cold, coldErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{ResultTransform: recorder})
	coldElapsed := time.Since(coldStarted).Nanoseconds()
	if coldErr != nil {
		_ = repository.Close()
		rss.stop()
		return DetailSample{}, coldErr
	}
	if err := recorder.validate(); err != nil {
		_ = repository.Close()
		rss.stop()
		return DetailSample{}, err
	}
	incrementalStarted := time.Now()
	incremental, incrementalErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{ResultTransform: recorder})
	incrementalElapsed := time.Since(incrementalStarted).Nanoseconds()
	closeErr := repository.Close()
	rss.stop()
	if err := errors.Join(incrementalErr, closeErr); err != nil {
		return DetailSample{}, err
	}
	if len(incremental.Updated) != 0 || len(incremental.Removed) != 0 {
		return DetailSample{}, fmt.Errorf("incremental sample did not converge: updated=%v removed=%v", incremental.Updated, incremental.Removed)
	}
	if err := compactSQLite(ctx, database); err != nil {
		return DetailSample{}, err
	}
	if err := validateDatabaseClosure(ctx, database); err != nil {
		return DetailSample{}, err
	}
	fingerprints, err := capabilityFingerprints(ctx, database)
	if err != nil {
		return DetailSample{}, err
	}
	primary, err := fileSize(database)
	if err != nil {
		return DetailSample{}, err
	}
	wal, err := optionalFileSize(database + "-wal")
	if err != nil {
		return DetailSample{}, err
	}
	sample = DetailSample{Number: number, Database: database,
		Cold: DetailRun{ElapsedNS: coldElapsed, Report: cold}, Incremental: DetailRun{ElapsedNS: incrementalElapsed, Report: incremental},
		CompactedBytes: primary, FinalWALBytes: wal, PeakRSSBytes: rss.peak(), Projection: recorder.counts(), Fingerprints: fingerprints}
	if !keep {
		sample.Database = ""
	}
	return sample, nil
}

func removeSQLiteFiles(database string) error {
	var result error
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(database + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

type recordingTransform struct {
	projector  *Projector
	mu         sync.Mutex
	value      ProjectionCounts
	resolution *AggregateResolutionValidator
}

func newRecordingTransform(projector *Projector) *recordingTransform {
	return &recordingTransform{projector: projector, value: ProjectionCounts{
		InputNodesByKind: map[string]int{}, OutputNodesByKind: map[string]int{}, InputFactsByKind: map[string]int{}, OutputFactsByKind: map[string]int{},
		InputFactsByProducer: map[string]int{}, OutputFactsByProducer: map[string]int{},
	}, resolution: NewAggregateResolutionValidator()}
}

func (r *recordingTransform) SemanticKey() string { return r.projector.SemanticKey() }

func (r *recordingTransform) Transform(ctx context.Context, input parserapi.Input, parsed graph.ParseResult) (graph.ParseResult, error) {
	projected, err := r.projector.Transform(ctx, input, parsed)
	if err != nil {
		return graph.ParseResult{}, err
	}
	producer := "unknown"
	for _, node := range parsed.Nodes {
		if node.Language != "" {
			producer = node.Language
			break
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolution.Observe(parsed, projected)
	r.value.InputNodes += len(parsed.Nodes)
	r.value.OutputNodes += len(projected.Nodes)
	r.value.InputFacts += len(parsed.Facts)
	r.value.OutputFacts += len(projected.Facts)
	for _, node := range parsed.Nodes {
		r.value.InputNodesByKind[string(node.Kind)]++
	}
	for _, node := range projected.Nodes {
		r.value.OutputNodesByKind[string(node.Kind)]++
	}
	for _, fact := range parsed.Facts {
		r.value.InputFactsByKind[string(fact.Kind)]++
		r.value.InputFactsByProducer[producerName(fact.Producer, producer)]++
	}
	for _, fact := range projected.Facts {
		r.value.OutputFactsByKind[string(fact.Kind)]++
		r.value.OutputFactsByProducer[producerName(fact.Producer, producer)]++
	}
	return projected, nil
}

func producerName(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
func (r *recordingTransform) counts() ProjectionCounts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value
}

func (r *recordingTransform) validate() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.resolution.Validate(); err != nil {
		return err
	}
	return r.projector.ValidateCoverage()
}

func capabilityFingerprints(ctx context.Context, path string) (map[Capability]string, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	matrix := Matrix()
	nodeCaps := map[string][]Capability{}
	edgeCaps := map[string][]Capability{}
	for _, item := range matrix.Nodes {
		nodeCaps[item.Name] = item.Capabilities
	}
	for _, item := range matrix.Edges {
		edgeCaps[item.Name] = item.Capabilities
	}
	writers := map[Capability]hash.Hash{}
	for _, capability := range capabilities {
		writers[capability] = sha256.New()
	}
	nodeKinds := map[string]string{}
	externalNodes := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line, properties, external FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, kind, name, qualified, language, sourcePath, properties string
		var line, column, endLine, external int64
		if err := rows.Scan(&id, &kind, &name, &qualified, &language, &sourcePath, &line, &column, &endLine, &properties, &external); err != nil {
			_ = rows.Close()
			return nil, err
		}
		nodeKinds[id] = kind
		if external != 0 {
			externalNodes[id] = strings.Join([]string{"X", id, kind, name, qualified, language, sourcePath, strconv.FormatInt(line, 10), strconv.FormatInt(column, 10), strconv.FormatInt(endLine, 10), properties, strconv.FormatInt(external, 10)}, "\x00") + "\n"
			continue
		}
		encoded := strings.Join([]string{"N", id, kind, name, qualified, language, sourcePath, strconv.FormatInt(line, 10), strconv.FormatInt(column, 10), strconv.FormatInt(endLine, 10), properties, strconv.FormatInt(external, 10)}, "\x00") + "\n"
		for _, capability := range nodeCaps[kind] {
			_, _ = writers[capability].Write([]byte(encoded))
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// An edge derives its producer and location from its originating fact, so
	// the join can only drop a row on an index whose fact or interned path is
	// missing. Fingerprinting a silently shortened edge set would compare two
	// profiles that never described the same graph.
	var joinedEdges, storedEdges int64
	if err := db.QueryRowContext(ctx, `SELECT
    (SELECT COUNT(*) FROM edges JOIN facts ON facts.id = edges.fact_id
         JOIN paths ON paths.id = facts.path_id),
    (SELECT COUNT(*) FROM edges)`).Scan(&joinedEdges, &storedEdges); err != nil {
		return nil, err
	}
	if joinedEdges != storedEdges {
		return nil, fmt.Errorf("%d of %d edges have no originating fact or interned path; "+
			"the index is corrupt and must be rebuilt", storedEdges-joinedEdges, storedEdges)
	}
	// The identity is derived from the three columns that determine it rather than
	// stored, so it is also what the rows are ordered by: it is the only ordering
	// available that is total.
	rows, err = db.QueryContext(ctx, `SELECT edges.fact_id, edges.from_id, edges.to_id, edges.kind, facts.producer,
	paths.path, facts.line, facts.column_no, facts.end_line, edges.properties
FROM edges
JOIN facts ON facts.id = edges.fact_id
JOIN paths ON paths.id = facts.path_id
ORDER BY edges.fact_id, edges.to_id, edges.kind`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var factID, fromID, toID, kind, producer, sourcePath, properties string
		var line, column, endLine int64
		if err := rows.Scan(&factID, &fromID, &toID, &kind, &producer, &sourcePath, &line, &column, &endLine, &properties); err != nil {
			_ = rows.Close()
			return nil, err
		}
		id := graph.DerivedEdgeID(factID, toID, graph.EdgeKind(kind))
		encoded := strings.Join([]string{"E", id, factID, fromID, toID, kind, producer, sourcePath, strconv.FormatInt(line, 10), strconv.FormatInt(column, 10), strconv.FormatInt(endLine, 10), properties}, "\x00") + "\n"
		for _, capability := range edgeCaps[kind] {
			if (kind == string(graph.EdgeDeclares) || kind == string(graph.EdgeContains)) && capability == CapabilityStructural &&
				(graph.NodeKind(nodeKinds[toID]) == graph.KindVariable || graph.NodeKind(nodeKinds[toID]) == graph.KindParameter) {
				continue
			}
			if (kind == string(graph.EdgeReads) || kind == string(graph.EdgeWrites)) && capability == CapabilityCatalogTopology && !graph.IsDataResourceKind(graph.NodeKind(nodeKinds[toID])) {
				continue
			}
			if (kind == string(graph.EdgeReads) || kind == string(graph.EdgeWrites)) && capability == CapabilityMessageFieldFlow && graph.NodeKind(nodeKinds[toID]) != graph.KindField {
				continue
			}
			_, _ = writers[capability].Write([]byte(encoded))
			if external := externalNodes[fromID]; external != "" {
				_, _ = writers[capability].Write([]byte(external))
			}
			if external := externalNodes[toID]; external != "" {
				_, _ = writers[capability].Write([]byte(external))
			}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := map[Capability]string{}
	for _, capability := range capabilities {
		result[capability] = hex.EncodeToString(writers[capability].Sum(nil))
	}
	return result, nil
}

func validateDatabaseClosure(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	checks := []struct {
		name  string
		query string
	}{
		{"source", `SELECT COUNT(*) FROM facts f LEFT JOIN nodes n ON n.id = f.from_id WHERE f.from_id <> '' AND n.id IS NULL`},
		{"target", `SELECT COUNT(*) FROM facts f LEFT JOIN nodes n ON n.id = f.target_id WHERE f.target_id <> '' AND n.id IS NULL`},
	}
	for _, check := range checks {
		var count int64
		if err := db.QueryRowContext(ctx, check.query).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("projection has %d dangling exact %s endpoints", count, check.name)
		}
	}
	return nil
}

func compactSQLite(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

func summarizeSamples(samples []DetailSample) DetailSummary {
	cold, incremental, size, rss := make([]int64, 0, len(samples)), make([]int64, 0, len(samples)), make([]int64, 0, len(samples)), make([]int64, 0, len(samples))
	for _, sample := range samples {
		cold = append(cold, sample.Cold.ElapsedNS)
		incremental = append(incremental, sample.Incremental.ElapsedNS)
		size = append(size, sample.CompactedBytes)
		rss = append(rss, sample.PeakRSSBytes)
	}
	medianCold := median(cold)
	return DetailSummary{Samples: len(samples), MedianColdNS: medianCold, MedianIncrementalNS: median(incremental), MedianCompactedBytes: median(size), MedianPeakRSSBytes: median(rss),
		OwnerObservationMinutes: 31, CorroboratesObservation: medianCold >= int64(25*time.Minute) && medianCold <= int64(37*time.Minute)}
}

func median(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	values = append([]int64(nil), values...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[len(values)/2]
}

func compareBenchmark(baseline, candidate BenchmarkReport) (ProfileComparison, error) {
	if baseline.Profile != ProfileFull {
		return ProfileComparison{}, fmt.Errorf("baseline profile is %q, want full", baseline.Profile)
	}
	if baseline.Grafo.Dirty || candidate.Grafo.Dirty || baseline.Corpus.Dirty || candidate.Corpus.Dirty {
		return ProfileComparison{}, fmt.Errorf("baseline or candidate has dirty Grafo/corpus provenance")
	}
	if baseline.Grafo.Commit != candidate.Grafo.Commit || baseline.Corpus.Commit != candidate.Corpus.Commit || baseline.Corpus.Path != candidate.Corpus.Path ||
		baseline.Scope.SemanticKey != candidate.Scope.SemanticKey || baseline.Environment.CPUQuota != candidate.Environment.CPUQuota ||
		baseline.Environment.MemoryMaxBytes != candidate.Environment.MemoryMaxBytes || baseline.Environment.Filesystem != candidate.Environment.Filesystem ||
		baseline.Environment.FilesystemDevice != candidate.Environment.FilesystemDevice || baseline.Environment.OutputPath != candidate.Environment.OutputPath {
		return ProfileComparison{}, fmt.Errorf("baseline Grafo/corpus, scope, cgroup, filesystem, or output provenance differs")
	}
	comparison := ProfileComparison{BaselineProfile: baseline.Profile, BaselineCommit: baseline.Grafo.Commit}
	comparison.SizeSavingsPercent = percentDecrease(baseline.Summary.MedianCompactedBytes, candidate.Summary.MedianCompactedBytes)
	comparison.ColdRegressionPercent = percentIncrease(baseline.Summary.MedianColdNS, candidate.Summary.MedianColdNS)
	comparison.IncrementalRegressionPercent = percentIncrease(baseline.Summary.MedianIncrementalNS, candidate.Summary.MedianIncrementalNS)
	comparison.RSSRegressionPercent = percentIncrease(baseline.Summary.MedianPeakRSSBytes, candidate.Summary.MedianPeakRSSBytes)
	comparison.ClaimedEquivalent = true
	for capability := range ProfileCapabilities(candidate.Profile) {
		if baseline.Equivalence[capability] != candidate.Equivalence[capability] {
			comparison.ClaimedEquivalent = false
			break
		}
	}
	comparison.SizeGatePassed = comparison.SizeSavingsPercent >= 30
	comparison.ColdGatePassed = comparison.ColdRegressionPercent <= 20
	comparison.IncrementalGatePassed = comparison.IncrementalRegressionPercent <= 20
	comparison.RSSGatePassed = comparison.RSSRegressionPercent <= 20
	if comparison.ClaimedEquivalent && comparison.SizeGatePassed && comparison.ColdGatePassed && comparison.IncrementalGatePassed && comparison.RSSGatePassed {
		comparison.Recommendation = "candidate clears the spike gates; file a bounded production implementation issue"
	} else {
		comparison.Recommendation = "retain full-only indexing; candidate does not clear every semantic and resource gate"
	}
	return comparison, nil
}

func percentDecrease(base, candidate int64) float64 {
	if base == 0 {
		return 0
	}
	return float64(base-candidate) * 100 / float64(base)
}
func percentIncrease(base, candidate int64) float64 {
	if base == 0 {
		return 0
	}
	return float64(candidate-base) * 100 / float64(base)
}

func equalFingerprints(first, second map[Capability]string) bool {
	for _, capability := range capabilities {
		if first[capability] != second[capability] {
			return false
		}
	}
	return true
}

func loadBenchmarkReport(path string) (BenchmarkReport, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return BenchmarkReport{}, err
	}
	var report BenchmarkReport
	if err := json.Unmarshal(content, &report); err != nil {
		return BenchmarkReport{}, err
	}
	if report.Schema != BenchmarkSchema {
		return BenchmarkReport{}, fmt.Errorf("baseline schema %q", report.Schema)
	}
	return report, nil
}
func writeBenchmarkReport(path string, report BenchmarkReport) error {
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".detail-profile-*.tmp")
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

func inspectGit(ctx context.Context, path string) (GitProvenance, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return GitProvenance{}, err
	}
	root, err := gitOutput(ctx, absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return GitProvenance{}, err
	}
	root = strings.TrimSpace(root)
	commit, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return GitProvenance{}, err
	}
	branch, err := gitOutput(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		branch = "detached"
	}
	status, err := gitOutput(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return GitProvenance{}, err
	}
	remote, _ := gitOutput(ctx, root, "remote", "get-url", "origin")
	return GitProvenance{Path: root, Commit: strings.TrimSpace(commit), Branch: strings.TrimSpace(branch), Remote: strings.TrimSpace(remote), Dirty: strings.TrimSpace(status) != ""}, nil
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func inspectEnvironment(output string) (BenchmarkEnvironment, error) {
	limits, err := inspectCgroupLimits("/sys/fs/cgroup")
	if err != nil {
		return BenchmarkEnvironment{}, err
	}
	filesystem, err := inspectFilesystem(output)
	if err != nil {
		return BenchmarkEnvironment{}, err
	}
	return BenchmarkEnvironment{CPUQuota: limits.cpuQuota, EffectiveCPUs: limits.effectiveCPUs, MemoryMaxBytes: limits.memoryMaxBytes,
		Filesystem: filesystem.description, FilesystemDevice: filesystem.device,
		FilesystemAvailableBytes: filesystem.availableBytes, OutputPath: output,
		RSSMethod: rssMethod}, nil
}

type cgroupLimits struct {
	cpuQuota       string
	effectiveCPUs  int64
	memoryMaxBytes int64
}

func inspectCgroupLimits(root string) (cgroupLimits, error) {
	cpuV2, cpuV2Err := os.ReadFile(filepath.Join(root, "cpu.max"))
	memoryV2, memoryV2Err := os.ReadFile(filepath.Join(root, "memory.max"))
	if cpuV2Err == nil && memoryV2Err == nil {
		return parseCgroupLimits(strings.TrimSpace(string(cpuV2)), strings.TrimSpace(string(memoryV2)))
	}
	if !errors.Is(cpuV2Err, os.ErrNotExist) || !errors.Is(memoryV2Err, os.ErrNotExist) {
		return cgroupLimits{}, fmt.Errorf("read cgroup v2 limits: cpu: %v; memory: %v", cpuV2Err, memoryV2Err)
	}

	quota, period, cpuV1Present, err := readCgroupV1CPU(filepath.Join(root, "cpu"))
	if err != nil {
		return cgroupLimits{}, err
	}
	if !cpuV1Present {
		quota, period, cpuV1Present, err = readCgroupV1CPU(filepath.Join(root, "cpu,cpuacct"))
		if err != nil {
			return cgroupLimits{}, err
		}
	}
	memory, memoryErr := os.ReadFile(filepath.Join(root, "memory", "memory.limit_in_bytes"))
	if !cpuV1Present && errors.Is(memoryErr, os.ErrNotExist) {
		return inspectHostLimits()
	}
	if !cpuV1Present || memoryErr != nil {
		return cgroupLimits{}, fmt.Errorf("read cgroup v1 limits: CPU controller present: %t; memory: %v", cpuV1Present, memoryErr)
	}
	return parseCgroupLimits("v1:"+strings.TrimSpace(string(quota))+" "+strings.TrimSpace(string(period)), strings.TrimSpace(string(memory)))
}

func readCgroupV1CPU(root string) ([]byte, []byte, bool, error) {
	quota, quotaErr := os.ReadFile(filepath.Join(root, "cpu.cfs_quota_us"))
	period, periodErr := os.ReadFile(filepath.Join(root, "cpu.cfs_period_us"))
	if quotaErr == nil && periodErr == nil {
		return quota, period, true, nil
	}
	if errors.Is(quotaErr, os.ErrNotExist) && errors.Is(periodErr, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	return nil, nil, false, fmt.Errorf("read cgroup v1 CPU limits under %s: quota: %v; period: %v", root, quotaErr, periodErr)
}

func inspectHostLimits() (cgroupLimits, error) {
	logicalCPUs := runtime.NumCPU()
	if logicalCPUs <= 0 {
		return cgroupLimits{}, fmt.Errorf("host logical CPU count is unavailable")
	}
	totalRAM, err := hostMemoryBytes()
	if err != nil {
		return cgroupLimits{}, err
	}
	return cgroupLimits{
		cpuQuota:       fmt.Sprintf("host:logical-cpus=%d", logicalCPUs),
		effectiveCPUs:  int64(logicalCPUs),
		memoryMaxBytes: int64(totalRAM),
	}, nil
}

func parseCgroupLimits(cpu, memory string) (cgroupLimits, error) {
	fields := strings.Fields(strings.TrimPrefix(cpu, "v1:"))
	if len(fields) != 2 {
		return cgroupLimits{}, fmt.Errorf("invalid cgroup CPU quota %q", cpu)
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return cgroupLimits{}, fmt.Errorf("invalid cgroup CPU period %q", cpu)
	}
	var effectiveCPUs int64
	if fields[0] != "max" && fields[0] != "-1" {
		quota, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || quota <= 0 {
			return cgroupLimits{}, fmt.Errorf("invalid cgroup CPU quota %q", cpu)
		}
		effectiveCPUs = quota / period
		if effectiveCPUs == 0 {
			effectiveCPUs = 1
		}
	}
	memoryMaxBytes := int64(-1)
	if memory != "max" {
		memoryMaxBytes, err = strconv.ParseInt(memory, 10, 64)
		if err != nil || memoryMaxBytes <= 0 {
			return cgroupLimits{}, fmt.Errorf("invalid cgroup memory limit %q", memory)
		}
	}
	return cgroupLimits{cpuQuota: cpu, effectiveCPUs: effectiveCPUs, memoryMaxBytes: memoryMaxBytes}, nil
}

func insidePath(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
func optionalFileSize(path string) (int64, error) {
	value, err := fileSize(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return value, err
}

type rssSampler struct {
	mu      sync.Mutex
	maximum int64
	cancel  context.CancelFunc
	done    chan struct{}
}

func newRSSSampler() *rssSampler { return &rssSampler{done: make(chan struct{})} }
func (s *rssSampler) start() {
	debug.FreeOSMemory()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			s.sample()
			select {
			case <-ctx.Done():
				s.sample()
				return
			case <-ticker.C:
			}
		}
	}()
}
func (s *rssSampler) stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
}
func (s *rssSampler) peak() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.maximum }
func (s *rssSampler) sample() {
	value := currentRSS()
	s.mu.Lock()
	if value > s.maximum {
		s.maximum = value
	}
	s.mu.Unlock()
}
