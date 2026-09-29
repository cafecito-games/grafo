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
	flags := flag.NewFlagSet("grafo-storage-layout-benchmark", flag.ContinueOnError)
	flags.StringVar(&options.Repository, "repo", os.Getenv("GRAFO_BENCH_REPO"), "Git corpus worktree (or GRAFO_BENCH_REPO)")
	flags.StringVar(&options.Output, "output", os.Getenv("GRAFO_STORAGE_LAYOUT_OUTPUT"), "layout artifact directory")
	flags.IntVar(&options.Samples, "samples", environmentInt("GRAFO_STORAGE_LAYOUT_SAMPLES", 1), "samples per layout")
	flags.IntVar(&options.FixtureSeed, "fixture-seed", environmentInt("GRAFO_STORAGE_LAYOUT_SEED", 2), "first fixture seed; seed parity varies insertion order")
	flags.IntVar(&options.FixtureScale, "fixture-scale", environmentInt("GRAFO_STORAGE_LAYOUT_SCALE", 150), "fixture scale (roughly 10x nodes)")
	flags.IntVar(&options.EquivalenceScale, "equivalence-scale", environmentInt("GRAFO_STORAGE_LAYOUT_EQUIVALENCE_SCALE", 60), "fixture scale of the equivalence corpus")
	flags.IntVar(&options.BatchInterruptionScale, "batch-interruption-scale", environmentInt("GRAFO_STORAGE_LAYOUT_BATCH_SCALE", 900), "fixture scale of the batch interruption corpus")
	flags.IntVar(&options.QueryRepetitions, "query-repetitions", environmentInt("GRAFO_STORAGE_LAYOUT_REPETITIONS", 7), "timed repetitions per query pattern")
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

func environmentInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return parsed
}
