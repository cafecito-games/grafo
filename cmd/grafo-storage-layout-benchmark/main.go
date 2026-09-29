package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/cafecito-games/grafo/internal/storagecompare"
)

func main() { os.Exit(run()) }

func run() int {
	options := storagecompare.LayoutOptions{}
	defaults := map[string]int{}
	for _, entry := range []struct {
		name     string
		fallback int
	}{
		{name: "GRAFO_STORAGE_LAYOUT_SAMPLES", fallback: 1},
		{name: "GRAFO_STORAGE_LAYOUT_SEED", fallback: 2},
		{name: "GRAFO_STORAGE_LAYOUT_SCALE", fallback: 150},
		{name: "GRAFO_STORAGE_LAYOUT_EQUIVALENCE_SCALE", fallback: 60},
		{name: "GRAFO_STORAGE_LAYOUT_BATCH_SCALE", fallback: 900},
		{name: "GRAFO_STORAGE_LAYOUT_REPETITIONS", fallback: 7},
	} {
		value, err := environmentInt(entry.name, entry.fallback)
		if err != nil {
			fmt.Fprintln(os.Stderr, "grafo storage layout benchmark:", err)
			return 2
		}
		defaults[entry.name] = value
	}
	flags := flag.NewFlagSet("grafo-storage-layout-benchmark", flag.ContinueOnError)
	flags.StringVar(&options.Repository, "repo", os.Getenv("GRAFO_BENCH_REPO"), "Git corpus worktree (or GRAFO_BENCH_REPO)")
	flags.StringVar(&options.Output, "output", os.Getenv("GRAFO_STORAGE_LAYOUT_OUTPUT"), "layout artifact directory")
	flags.IntVar(&options.Samples, "samples", defaults["GRAFO_STORAGE_LAYOUT_SAMPLES"], "samples per layout")
	flags.IntVar(&options.FixtureSeed, "fixture-seed", defaults["GRAFO_STORAGE_LAYOUT_SEED"], "first fixture seed; seed parity varies insertion order")
	flags.IntVar(&options.FixtureScale, "fixture-scale", defaults["GRAFO_STORAGE_LAYOUT_SCALE"], "fixture scale (roughly 10x nodes)")
	flags.IntVar(&options.EquivalenceScale, "equivalence-scale", defaults["GRAFO_STORAGE_LAYOUT_EQUIVALENCE_SCALE"], "fixture scale of the equivalence corpus")
	flags.IntVar(&options.BatchInterruptionScale, "batch-interruption-scale", defaults["GRAFO_STORAGE_LAYOUT_BATCH_SCALE"], "fixture scale of the batch interruption corpus")
	flags.IntVar(&options.QueryRepetitions, "query-repetitions", defaults["GRAFO_STORAGE_LAYOUT_REPETITIONS"], "timed repetitions per query pattern")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if options.Samples <= 0 || options.FixtureScale <= 0 || options.QueryRepetitions <= 0 {
		fmt.Fprintln(os.Stderr, "grafo storage layout benchmark: samples, fixture-scale, and query-repetitions must be positive")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := storagecompare.RunLayout(ctx, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grafo storage layout benchmark:", err)
		if report.Artifact != "" {
			fmt.Fprintln(os.Stderr, "retained report:", report.Artifact)
		}
		return 1
	}
	_, _ = fmt.Fprintln(os.Stdout, report.Artifact)
	return 0
}

func environmentInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, value)
	}
	return parsed, nil
}
