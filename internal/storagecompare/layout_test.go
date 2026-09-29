package storagecompare

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/layoutbench"
)

// TestEvaluateLayoutGatesBoundary locks the gate thresholds at their exact
// boundaries: a 25.0% size reduction passes and 24.9% fails, a 1.20x
// performance ratio passes and 1.21x fails.
func TestEvaluateLayoutGatesBoundary(t *testing.T) {
	passing := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, candidateSizeBytes: 750, sizeScope: attributionScopeFixturePostCompact,
		perfRatios:       []float64{1.0, 1.20},
		correctnessValid: true, plansValid: true,
	})
	if !passing.MeetsSizeGate {
		t.Errorf("25.0%% size reduction rejected: %+v", passing)
	}
	if !passing.MeetsPerfGates {
		t.Errorf("1.20x performance ratio rejected: %+v", passing)
	}
	if !passing.Valid {
		t.Errorf("gate evaluation at both boundaries is not valid: %+v", passing)
	}
	if len(passing.Reasons) != 0 {
		t.Errorf("valid evaluation carries reasons: %v", passing.Reasons)
	}

	belowSize := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, candidateSizeBytes: 751, sizeScope: attributionScopeFixturePostCompact,
		perfRatios:       []float64{1.0},
		correctnessValid: true, plansValid: true,
	})
	if belowSize.MeetsSizeGate {
		t.Errorf("24.9%% size reduction passed the 25%% gate: %+v", belowSize)
	}
	if belowSize.Valid {
		t.Error("gate evaluation valid despite failing the size gate")
	}

	abovePerf := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, candidateSizeBytes: 750, sizeScope: attributionScopeFixturePostCompact,
		perfRatios:       []float64{1.0, 1.21},
		correctnessValid: true, plansValid: true,
	})
	if abovePerf.MeetsPerfGates {
		t.Errorf("1.21x performance ratio passed the 1.20x budget: %+v", abovePerf)
	}
	if abovePerf.Valid {
		t.Error("gate evaluation valid despite failing the perf gate")
	}
}

// TestEvaluateLayoutGatesFailsClosed proves the fail-closed rules: an
// unavailable ratio, a provenance mismatch, a correctness failure, and an
// invalid plan capture each invalidate the evaluation rather than being
// skipped.
func TestEvaluateLayoutGatesFailsClosed(t *testing.T) {
	missingRatio := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, candidateSizeBytes: 750, sizeScope: attributionScopeFixturePostCompact,
		perfRatios:       []float64{0},
		correctnessValid: true, plansValid: true,
	})
	if missingRatio.MeetsPerfGates {
		t.Error("a zero (unavailable) ratio passed the perf gate")
	}

	rejected := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, candidateSizeBytes: 750, sizeScope: attributionScopeFixturePostCompact,
		perfRatios:       []float64{1.0},
		correctnessValid: true, plansValid: true,
		provenanceRejected: true,
	})
	if rejected.MeetsPerfGates || rejected.MeetsSizeGate {
		t.Errorf("provenance mismatch did not reject the ratios: %+v", rejected)
	}

	noControlSize := evaluateGates(layoutGateInput{
		sizeScope:  attributionScopeFixturePostCompact,
		perfRatios: []float64{1.0}, correctnessValid: true, plansValid: true,
	})
	if noControlSize.MeetsSizeGate {
		t.Error("a missing control size median passed the size gate")
	}

	noCandidateSize := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, sizeScope: attributionScopeFixturePostCompact,
		perfRatios: []float64{1.0}, correctnessValid: true, plansValid: true,
	})
	if noCandidateSize.MeetsSizeGate || noCandidateSize.Valid {
		t.Errorf("a missing candidate size median scored a free pass: %+v", noCandidateSize)
	}
	if joined := strings.Join(noCandidateSize.Reasons, "; "); !strings.Contains(joined, "candidate fixture-final-post-compact size median unavailable") {
		t.Errorf("missing candidate size not reported as such: %v", noCandidateSize.Reasons)
	}

	brokenCorrectness := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, candidateSizeBytes: 750, sizeScope: attributionScopeFixturePostCompact,
		perfRatios: []float64{1.0}, plansValid: true,
	})
	if brokenCorrectness.MeetsCorrectness || brokenCorrectness.Valid {
		t.Error("a failed equivalence gate kept the evaluation valid")
	}

	brokenPlans := evaluateGates(layoutGateInput{
		controlSizeBytes: 1000, candidateSizeBytes: 750, sizeScope: attributionScopeFixturePostCompact,
		perfRatios: []float64{1.0}, correctnessValid: true,
	})
	if brokenPlans.MeetsPlanGate || brokenPlans.Valid {
		t.Error("a failed plan gate kept the evaluation valid")
	}
}

// TestLayoutReportRoundTrip proves the schema-v2 report serializes and
// deserializes without losing any field the artifact contract promises.
func TestLayoutReportRoundTrip(t *testing.T) {
	report := LayoutReport{
		SchemaVersion: LayoutReportSchemaVersion, GeneratedAt: "2026-01-01T00:00:00Z", Status: StatusPassed,
		Machine: Machine{Hostname: "host", OS: "darwin", Arch: "arm64", Go: "go1.26", CPU: "M3"},
		HostLimits: HostLimits{CPUCores: 10, MemoryTotalBytes: 34359738368, DiskFreeBytes: 1000,
			LimitSource: "sysctl", Reason: "cgroup v2 unavailable on darwin"},
		Samples: 2, FixtureRows: 42, GrafoCommit: "abc123", GrafoDirty: false,
		SemanticIndexVersion: "v1", GraphSchemaVersion: graph.SchemaVersion,
		ControlObservation: layoutControlObservation,
		Layouts: []LayoutResult{{
			Name: "control",
			Correctness: LayoutCorrectness{Valid: true, Equivalent: true, ControlDigest: "aa", VariantDigest: "aa",
				ControlProbes: 7, VariantProbes: 7},
			Fixture: FixtureResult{Counts: graph.Counts{Nodes: 10},
				ColdPersistenceNS:        LayoutMetric{Raw: []int64{5, 6}, Median: 6, RatioToControl: 1},
				ReconciliationNS:         LayoutMetric{Raw: []int64{7}, Median: 7, RatioToControl: 1},
				ColdTotalNS:              LayoutMetric{Raw: []int64{12}, Median: 12, RatioToControl: 1},
				DatabaseBytesPreCompact:  LayoutMetric{Raw: []int64{4096}, Median: 4096, RatioToControl: 1},
				DatabaseBytesPostCompact: LayoutMetric{Raw: []int64{3072}, Median: 3072, RatioToControl: 1},
				Writes:                   []graph.WriteStats{{}}},
			Attribution: []AttributionSample{{Scope: attributionScopeFixturePreCompact, Sample: 1,
				Capture: layoutbench.Attribution{DBStatSupported: true, PageSize: 4096, PageCount: 1,
					PrimaryBytes: 4096, Objects: []layoutbench.ObjectBytes{{Name: "nodes", Bytes: 2048, Pages: 1}}}}},
			Plans:      []layoutbench.PlanCapture{{Query: layoutbench.PlanQuery{Name: "GetNode"}, Valid: true, Steps: []layoutbench.PlanStep{{Detail: "SEARCH nodes"}}}},
			QuerySuite: []layoutbench.QueryMetric{{Pattern: "node-lookup-hub", Repetitions: 7, MedianNS: 100, RatioToControl: 1}},
			Gates:      GateEvaluation{SizeReduction: 0.3, MeetsSizeGate: true, MeetsPerfGates: true, MeetsCorrectness: true, MeetsPlanGate: true, Valid: true},
		}},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var decoded LayoutReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	decoded.GeneratedAt = ""
	report.GeneratedAt = ""
	if !reflect.DeepEqual(report, decoded) {
		t.Fatalf("report round-trip lost content:\nwant %+v\ngot  %+v", report, decoded)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal field map: %v", err)
	}
	for _, required := range []string{"schema_version", "coder_observation_disposition", "host_limits",
		"layouts", "fixture_rows"} {
		if _, present := fields[required]; !present {
			t.Errorf("report is missing the %s field", required)
		}
	}
}

// TestRunLayoutFixtureOnlyDryRun drives the whole orchestrator on the
// fixture-only path with one small sample: four layouts, control first,
// equivalence valid, plans and query suite captured, pre- and post-compaction
// attribution recorded, and a parseable schema-v2 artifact on disk.
func TestRunLayoutFixtureOnlyDryRun(t *testing.T) {
	if testing.Short() {
		t.Skip("layout dry run is too slow for short mode")
	}
	ctx := context.Background()
	report, err := RunLayout(ctx, LayoutOptions{Output: t.TempDir(), Samples: 1,
		FixtureSeed: 2, FixtureScale: 4, EquivalenceScale: 4, BatchInterruptionScale: 4, QueryRepetitions: 2})
	if err != nil {
		t.Fatalf("RunLayout: %v", err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("report status %s with error %q", report.Status, report.Error)
	}
	if report.SchemaVersion != LayoutReportSchemaVersion {
		t.Fatalf("schema version %d, want %d", report.SchemaVersion, LayoutReportSchemaVersion)
	}
	if report.ControlObservation != layoutControlObservation {
		t.Errorf("coder observation disposition not recorded verbatim: %q", report.ControlObservation)
	}
	if len(report.Layouts) != 4 {
		t.Fatalf("report has %d layouts, want 4", len(report.Layouts))
	}
	if report.Layouts[0].Name != layoutControlName {
		t.Fatalf("first layout is %s, want %s", report.Layouts[0].Name, layoutControlName)
	}
	scopes := map[string]bool{}
	for _, layout := range report.Layouts {
		if !layout.Correctness.Valid {
			t.Errorf("%s correctness invalid: %+v", layout.Name, layout.Correctness)
		}
		if len(layout.Plans) == 0 {
			t.Errorf("%s captured no query plans", layout.Name)
		}
		if len(layout.QuerySuite) < 8 {
			t.Errorf("%s query suite has %d patterns, want at least 8", layout.Name, len(layout.QuerySuite))
		}
		if layout.Fixture.ColdTotalNS.Median <= 0 || layout.Fixture.DatabaseBytesPostCompact.Median <= 0 {
			t.Errorf("%s fixture medians missing: %+v", layout.Name, layout.Fixture)
		}
		for _, sample := range layout.Attribution {
			scopes[sample.Scope] = true
		}
		if layout.Name == layoutControlName {
			if layout.Fixture.ColdTotalNS.RatioToControl != 1 {
				t.Errorf("control cold-total ratio is %f, want 1", layout.Fixture.ColdTotalNS.RatioToControl)
			}
			for _, metric := range layout.QuerySuite {
				if metric.RatioToControl != 1 {
					t.Errorf("control query pattern %s ratio is %f, want 1", metric.Pattern, metric.RatioToControl)
				}
			}
			if layout.Gates.Valid || layout.Gates.MeetsSizeGate {
				t.Errorf("control layout was gated instead of marked not applicable: %+v", layout.Gates)
			}
			if len(layout.Gates.Reasons) == 0 || !strings.Contains(layout.Gates.Reasons[0], "not applicable") {
				t.Errorf("control gate row lacks the not-applicable reason: %v", layout.Gates.Reasons)
			}
		}
		if len(layout.Limitations) == 0 || !strings.Contains(strings.Join(layout.Limitations, "; "), "peak RSS") {
			t.Errorf("%s does not record the peak RSS exclusion limitation: %v", layout.Name, layout.Limitations)
		}
	}
	for _, scope := range []string{attributionScopeFixturePreCompact, attributionScopeFixturePostCompact} {
		if !scopes[scope] {
			t.Errorf("no attribution captured at scope %s", scope)
		}
	}
	content, err := os.ReadFile(report.Artifact)
	if err != nil {
		t.Fatalf("read artifact %s: %v", report.Artifact, err)
	}
	if filepath.Base(report.Artifact) != LayoutReportFileName {
		t.Errorf("artifact name is %s, want %s", filepath.Base(report.Artifact), LayoutReportFileName)
	}
	var stored map[string]any
	if err := json.Unmarshal(content, &stored); err != nil {
		t.Fatalf("artifact is not valid JSON: %v", err)
	}
	if stored["schema_version"] != float64(LayoutReportSchemaVersion) {
		t.Errorf("artifact schema_version is %v, want %d", stored["schema_version"], LayoutReportSchemaVersion)
	}
	if _, present := stored["coder_observation_disposition"]; !present {
		t.Error("artifact is missing the coder observation disposition")
	}
}

// TestRequirePinnedCleanWorktree proves the corpus gate: a clean worktree at
// a pinned commit passes and returns its provenance, a dirty one fails before
// any sample could run.
func TestRequirePinnedCleanWorktree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	run := func(arguments ...string) {
		t.Helper()
		if out, err := layoutGitOutput(ctx, root, arguments...); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, out)
		}
	}
	run("init", "--initial-branch=main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("corpus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "corpus")

	corpus, err := requirePinnedCleanWorktree(ctx, root)
	if err != nil {
		t.Fatalf("clean worktree rejected: %v", err)
	}
	if corpus.Commit == "" || corpus.Path == "" {
		t.Fatalf("clean worktree provenance incomplete: %+v", corpus)
	}

	if err := os.WriteFile(filepath.Join(root, "dirt.txt"), []byte("dirt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := requirePinnedCleanWorktree(ctx, root); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty worktree not rejected: %v", err)
	}
	if _, err := requirePinnedCleanWorktree(ctx, filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing corpus directory accepted")
	}
}

// TestFinalizeLayoutReportGatesOnlyCandidates proves finalizeLayoutReport
// evaluates only the candidate rows: the control's gate row is marked not
// applicable, a candidate's inflated peak RSS ratio never enters the perf gate,
// and the corpus scenario ratios serialize under the honest ratio_to_control
// key rather than v1's ratio_to_sqlite.
func TestFinalizeLayoutReportGatesOnlyCandidates(t *testing.T) {
	results := []LayoutResult{
		{Name: layoutControlName, Correctness: LayoutCorrectness{Valid: true},
			Attribution: []AttributionSample{
				{Scope: attributionScopeFixturePostCompact, Capture: layoutbench.Attribution{PrimaryBytes: 1000}},
				{Scope: attributionScopeCorpusPostCompact, Capture: layoutbench.Attribution{PrimaryBytes: 1000}},
			},
			Plans:      []layoutbench.PlanCapture{{Valid: true}},
			QuerySuite: []layoutbench.QueryMetric{{Pattern: "node-lookup-hub", MedianNS: 5}},
			Fixture: FixtureResult{ColdTotalNS: LayoutMetric{Raw: []int64{10}, Median: 10},
				ColdPersistenceNS:        LayoutMetric{Raw: []int64{10}, Median: 10},
				ReconciliationNS:         LayoutMetric{Raw: []int64{10}, Median: 10},
				DatabaseBytesPostCompact: LayoutMetric{Raw: []int64{1000}, Median: 1000}},
			Corpus: LayoutCorpusResult{Scenarios: map[string]LayoutScenarioSamples{"cold": {
				TotalNS:      LayoutMetric{Raw: []int64{10}, Median: 10},
				PeakRSSBytes: LayoutMetric{Raw: []int64{10}, Median: 10}}}},
		},
		{Name: "candidate", Correctness: LayoutCorrectness{Valid: true},
			Attribution: []AttributionSample{
				{Scope: attributionScopeFixturePostCompact, Capture: layoutbench.Attribution{PrimaryBytes: 500}},
				{Scope: attributionScopeCorpusPostCompact, Capture: layoutbench.Attribution{PrimaryBytes: 500}},
			},
			Plans:      []layoutbench.PlanCapture{{Valid: true}},
			QuerySuite: []layoutbench.QueryMetric{{Pattern: "node-lookup-hub", MedianNS: 5}},
			Fixture: FixtureResult{ColdTotalNS: LayoutMetric{Raw: []int64{12}, Median: 12},
				ColdPersistenceNS:        LayoutMetric{Raw: []int64{12}, Median: 12},
				ReconciliationNS:         LayoutMetric{Raw: []int64{12}, Median: 12},
				DatabaseBytesPostCompact: LayoutMetric{Raw: []int64{500}, Median: 500}},
			Corpus: LayoutCorpusResult{Scenarios: map[string]LayoutScenarioSamples{"cold": {
				TotalNS:      LayoutMetric{Raw: []int64{12}, Median: 12},
				PeakRSSBytes: LayoutMetric{Raw: []int64{100}, Median: 100}}}},
		},
	}
	finalizeLayoutReport(results, nil, true)
	control, candidate := &results[0], &results[1]
	if control.Gates.Valid || !strings.Contains(strings.Join(control.Gates.Reasons, "; "), "not applicable") {
		t.Fatalf("control gate row is evaluated instead of not applicable: %+v", control.Gates)
	}
	if !candidate.Gates.Valid {
		t.Fatalf("candidate failed gates it meets: %+v", candidate.Gates)
	}
	if candidate.Corpus.Scenarios["cold"].PeakRSSBytes.RatioToControl != 10 {
		t.Fatalf("candidate peak RSS ratio is %f, want 10 (recorded for observation)",
			candidate.Corpus.Scenarios["cold"].PeakRSSBytes.RatioToControl)
	}
	if !candidate.Gates.MeetsPerfGates {
		t.Fatalf("peak RSS leaked into the perf gate: %+v", candidate.Gates)
	}
	encoded, err := json.Marshal(candidate.Corpus)
	if err != nil {
		t.Fatalf("marshal corpus result: %v", err)
	}
	if !strings.Contains(string(encoded), "\"ratio_to_control\"") || strings.Contains(string(encoded), "ratio_to_sqlite") {
		t.Fatalf("corpus ratios do not serialize under ratio_to_control: %s", encoded)
	}
	if len(candidate.Limitations) == 0 || !strings.Contains(strings.Join(candidate.Limitations, "; "), "peak RSS") {
		t.Fatalf("candidate lacks the peak RSS limitation: %v", candidate.Limitations)
	}
}
