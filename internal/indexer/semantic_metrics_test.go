package indexer_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// semanticMetricsCorpus writes a two-package Go module. The importing package
// is what makes a real workspace load happen: its view only exists if
// packages.Load produced type information for the package it imports.
func semanticMetricsCorpus(t testing.TB) string {
	t.Helper()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/metrics\n\ngo 1.26\n")
	write(t, filepath.Join(root, "store", "store.go"), `package store

type Reader interface{ Get(id string) string }

type Memory struct{}

func (Memory) Get(id string) string { return id }
`)
	write(t, filepath.Join(root, "app", "app.go"), `package app

import "example.com/metrics/store"

func Run(reader store.Reader) string { return reader.Get("a") }
`)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid",
		"commit", "-m", "initial")
	return root
}

func indexCorpus(t testing.TB, root string) indexer.Report {
	t.Helper()
	return indexCorpusWith(t, root, parserdefaults.NewRegistry())
}

func indexCorpusWith(t testing.TB, root string, registry *parserapi.Registry) indexer.Report {
	t.Helper()
	ctx := context.Background()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	report, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// TestReportCarriesGoSemanticLoadMetrics is the instrumentation contract. The
// whole-workspace load and the view derivation both happen inside one Parse
// call, so without these counters a refresh cannot be attributed between fixed
// semantic cost and per-file parse cost.
func TestReportCarriesGoSemanticLoadMetrics(t *testing.T) {
	report := indexCorpus(t, semanticMetricsCorpus(t))

	metrics, reported := report.Semantic["go"]
	if !reported {
		t.Fatalf("no Go semantic metrics in report; languages present: %v", keysOf(report.Semantic))
	}
	if metrics.Loads == 0 {
		t.Fatal("a cold index of a Go module reported no workspace load")
	}
	if metrics.LoadNS <= 0 {
		t.Fatalf("workspace load time = %d, want a positive duration", metrics.LoadNS)
	}
	if metrics.DerivationNS <= 0 {
		t.Fatalf("view derivation time = %d, want a positive duration", metrics.DerivationNS)
	}
	if metrics.DerivationNS > metrics.LoadNS {
		t.Fatalf("derivation %d exceeds the load %d it is a part of", metrics.DerivationNS, metrics.LoadNS)
	}
}

// TestSemanticPhasesAreASubsetOfParse covers the relationship the phase
// comment claims. Reporting the semantic figures is only meaningful if they are
// the part of ParseNS the reader is meant to be able to subtract.
func TestSemanticPhasesAreASubsetOfParse(t *testing.T) {
	report := indexCorpus(t, semanticMetricsCorpus(t))

	if report.Phases.SemanticNS <= 0 {
		t.Fatalf("semantic phase = %d, want a positive duration", report.Phases.SemanticNS)
	}
	if report.Phases.SemanticNS > report.Phases.ParseNS {
		t.Fatalf("semantic phase %d exceeds parse %d, so it is not a subset of it",
			report.Phases.SemanticNS, report.Phases.ParseNS)
	}
	if report.Phases.SemanticDerivationNS > report.Phases.SemanticNS {
		t.Fatalf("derivation phase %d exceeds the semantic phase %d it is a part of",
			report.Phases.SemanticDerivationNS, report.Phases.SemanticNS)
	}
}

// TestSemanticMetricsReachTheJSONReport covers the operator-facing half: the
// profile this instrumentation exists for is read from `grafo index --json`,
// not from Go structs.
func TestSemanticMetricsReachTheJSONReport(t *testing.T) {
	report := indexCorpus(t, semanticMetricsCorpus(t))

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Phases struct {
			SemanticNS           int64 `json:"semantic_ns"`
			SemanticDerivationNS int64 `json:"semantic_derivation_ns"`
		} `json:"phases"`
		Semantic map[string]struct {
			Loads  int64 `json:"loads"`
			LoadNS int64 `json:"load_ns"`
		} `json:"semantic"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Phases.SemanticNS <= 0 {
		t.Fatalf("semantic_ns absent or zero in the JSON report: %s", encoded)
	}
	if decoded.Semantic["go"].Loads == 0 {
		t.Fatalf("go semantic metrics absent from the JSON report: %s", encoded)
	}
}

// TestNonGoRepositoryReportsIdleSemanticMetrics is the fail-closed half. The Go
// parser always keeps counters, so a repository with no Go sources reports a
// zeroed entry rather than no entry: that is what distinguishes "its loader did
// nothing" from "this language has no semantic loader", which is absence.
// Indexing must not depend on any of it.
func TestNonGoRepositoryReportsIdleSemanticMetrics(t *testing.T) {
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "README.md"), "# No Go here\n\nJust prose.\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid",
		"commit", "-m", "initial")

	report := indexCorpus(t, root)

	metrics, reported := report.Semantic["go"]
	if !reported {
		t.Fatalf("the Go parser keeps counters, so its entry must be present; languages: %v", keysOf(report.Semantic))
	}
	if metrics.Loads != 0 || metrics.LoadNS != 0 {
		t.Fatalf("a repository with no Go sources reported %d loads over %dns", metrics.Loads, metrics.LoadNS)
	}
	if report.Phases.SemanticNS != 0 {
		t.Fatalf("semantic phase = %d on a repository with no Go sources, want 0", report.Phases.SemanticNS)
	}
}

func keysOf(metrics map[string]parserapi.SemanticLoadMetrics) []string {
	result := make([]string, 0, len(metrics))
	for language := range metrics {
		result = append(result, language)
	}
	return result
}

// TestReusedRegistryReportsOnlyItsOwnRunsWork is the regression guard for
// lifetime counters leaking between runs. Federation indexes every member
// through one registry (internal/federation/repository.go), and the MCP
// freshness coordinator keeps one for the session, so a report built from raw
// counters attributes an earlier run's semantic time to a later one — and makes
// SemanticNS exceed the ParseNS it is documented as a subset of.
func TestReusedRegistryReportsOnlyItsOwnRunsWork(t *testing.T) {
	registry := parserdefaults.NewRegistry()
	first := indexCorpusWith(t, semanticMetricsCorpus(t), registry)
	if first.Phases.SemanticNS <= 0 {
		t.Fatalf("first run semantic phase = %d, want a positive duration", first.Phases.SemanticNS)
	}

	// A second, separate repository indexed through the same registry.
	second := indexCorpusWith(t, semanticMetricsCorpus(t), registry)

	if second.Phases.SemanticNS > second.Phases.ParseNS {
		t.Fatalf("second run reported semantic %d above its own parse %d, so it inherited the first run's work",
			second.Phases.SemanticNS, second.Phases.ParseNS)
	}
	// The two corpora are equivalent single-module repositories, so each run
	// performs the same load work and must report the same count. Lifetime
	// counters make the second report the first run's loads as well.
	if second.Semantic["go"].Loads != first.Semantic["go"].Loads {
		t.Fatalf("second run reported %d loads against the first run's %d over an equivalent corpus",
			second.Semantic["go"].Loads, first.Semantic["go"].Loads)
	}
}
