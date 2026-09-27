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
	options := storagecompare.Options{}
	flags := flag.NewFlagSet("grafo-storage-benchmark", flag.ContinueOnError)
	flags.StringVar(&options.Repository, "repo", os.Getenv("GRAFO_BENCH_REPO"), "Git corpus worktree (or GRAFO_BENCH_REPO)")
	flags.StringVar(&options.Output, "output", os.Getenv("GRAFO_STORAGE_BENCH_OUTPUT"), "comparison artifact directory")
	flags.IntVar(&options.Samples, "samples", environmentInt("GRAFO_STORAGE_BENCH_SAMPLES", 3), "samples per engine")
	flags.IntVar(&options.GeneratedRows, "generated-rows", environmentInt("GRAFO_STORAGE_BENCH_ROWS", 5_000), "generated nodes and facts per sample")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if options.Samples <= 0 || options.GeneratedRows <= 0 {
		fmt.Fprintln(os.Stderr, "grafo storage benchmark: samples and generated-rows must be positive")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := storagecompare.Run(ctx, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grafo storage benchmark:", err)
		if report.Artifact != "" {
			fmt.Fprintln(os.Stderr, "retained report:", report.Artifact)
		}
		return 1
	}
	fmt.Fprintln(os.Stdout, report.Artifact)
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
