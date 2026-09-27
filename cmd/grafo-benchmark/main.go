package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/cafecito-games/grafo/internal/benchmark"
)

func main() {
	os.Exit(run())
}

func run() int {
	options := benchmark.Options{}
	flags := flag.NewFlagSet("grafo-benchmark", flag.ContinueOnError)
	flags.StringVar(&options.Repository, "repo", os.Getenv("GRAFO_BENCH_REPO"), "Git corpus worktree (or GRAFO_BENCH_REPO)")
	flags.StringVar(&options.Output, "output", os.Getenv("GRAFO_BENCH_OUTPUT"), "artifact directory (or GRAFO_BENCH_OUTPUT)")
	flags.StringVar(&options.Baseline, "baseline", os.Getenv("GRAFO_BENCH_BASELINE"), "compatible report to validate (or GRAFO_BENCH_BASELINE)")
	flags.StringVar(&options.Engine, "engine", os.Getenv("GRAFO_BENCH_ENGINE"), "storage engine: sqlite, bbolt, or pebble (or GRAFO_BENCH_ENGINE)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	var err error
	if options.MaxWALBytes, err = environmentInt64("GRAFO_BENCH_MAX_WAL_BYTES"); err != nil {
		fmt.Fprintln(os.Stderr, "grafo benchmark:", err)
		return 2
	}
	if options.MaxRSSBytes, err = environmentInt64("GRAFO_BENCH_MAX_RSS_BYTES"); err != nil {
		fmt.Fprintln(os.Stderr, "grafo benchmark:", err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := benchmark.Run(ctx, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grafo benchmark:", err)
		if report.Artifacts.Report != "" {
			fmt.Fprintln(os.Stderr, "retained report:", report.Artifacts.Report)
		}
		return 1
	}
	fmt.Fprintln(os.Stdout, report.Artifacts.Report)
	return 0
}

func environmentInt64(name string) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive byte count", name)
	}
	return parsed, nil
}
