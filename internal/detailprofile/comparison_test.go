package detailprofile

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestCompareBenchmarkRequiresCleanFrozenSameMachineProvenance(t *testing.T) {
	base := comparableReport()
	candidate := comparableReport()
	candidate.Profile = ProfileStructural
	if _, err := compareBenchmark(base, candidate); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*BenchmarkReport)
	}{
		{"Grafo commit", func(report *BenchmarkReport) { report.Grafo.Commit = "other" }},
		{"dirty Grafo", func(report *BenchmarkReport) { report.Grafo.Dirty = true }},
		{"dirty corpus", func(report *BenchmarkReport) { report.Corpus.Dirty = true }},
		{"corpus path", func(report *BenchmarkReport) { report.Corpus.Path = "/other" }},
		{"CPU quota", func(report *BenchmarkReport) { report.Environment.CPUQuota = "400000 100000" }},
		{"filesystem", func(report *BenchmarkReport) { report.Environment.FilesystemDevice++ }},
		{"output", func(report *BenchmarkReport) { report.Environment.OutputPath = "/other-output" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := candidate
			test.mutate(&changed)
			if _, err := compareBenchmark(base, changed); err == nil || !strings.Contains(err.Error(), "provenance") {
				t.Fatalf("comparison error = %v", err)
			}
		})
	}
}

func TestCapabilityFingerprintsAttributeTypedExternalNodesToProducingEdges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	parsed := graph.ParseResult{Nodes: []graph.Node{{ID: "n:caller", Kind: graph.KindFunction, Name: "Caller", QualifiedName: "app.Caller"}}, Facts: []graph.Fact{{
		ID: "f:call", FromID: "n:caller", Kind: graph.EdgeCalls, Target: "missing", TargetKind: graph.KindVariable, Producer: "go",
	}}}
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "app.go", Hash: "hash", Language: "go", IndexedAt: graph.NowUTC()}, parsed); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := capabilityFingerprints(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE nodes SET properties = '{"unresolved":"changed"}' WHERE external = 1`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := capabilityFingerprints(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if before[CapabilityStructural] == after[CapabilityStructural] {
		t.Fatal("typed external-node drift was absent from its producing edge capability")
	}
}

func comparableReport() BenchmarkReport {
	fingerprints := map[Capability]string{}
	for _, capability := range capabilities {
		fingerprints[capability] = string(capability)
	}
	return BenchmarkReport{
		Profile: ProfileFull,
		Grafo:   GitProvenance{Commit: "grafo"}, Corpus: GitProvenance{Path: "/corpus", Commit: "corpus"},
		Environment: BenchmarkEnvironment{CPUQuota: "800000 100000", EffectiveCPUs: 8, MemoryMaxBytes: 20 << 30,
			Filesystem: "type=xfs block_size=4096", FilesystemDevice: 7, OutputPath: "/output"},
		Scope: ScopeEvidence{SemanticKey: "scope"}, Equivalence: fingerprints,
		Summary: DetailSummary{MedianColdNS: 100, MedianIncrementalNS: 100, MedianCompactedBytes: 100, MedianPeakRSSBytes: 100},
	}
}
