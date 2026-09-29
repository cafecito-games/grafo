package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/cafecito-games/grafo/internal/detailprofile"
)

func main() { os.Exit(run()) }

func run() int {
	options := detailprofile.BenchmarkOptions{}
	var profile, roots string
	flags := flag.NewFlagSet("grafo-detail-benchmark", flag.ContinueOnError)
	flags.StringVar(&options.Repository, "repo", os.Getenv("GRAFO_BENCH_REPO"), "clean Git corpus worktree")
	flags.StringVar(&options.Output, "output", os.Getenv("GRAFO_BENCH_OUTPUT"), "artifact directory outside the corpus")
	flags.StringVar(&profile, "profile", environment("GRAFO_DETAIL_PROFILE", "full"), "full, structural-v1, or scoped-full-v1")
	flags.StringVar(&roots, "full-roots", os.Getenv("GRAFO_DETAIL_FULL_ROOTS"), "comma-separated full-detail roots for scoped-full-v1")
	flags.StringVar(&options.Baseline, "baseline", os.Getenv("GRAFO_DETAIL_BASELINE"), "full-profile report for exact comparison")
	flags.BoolVar(&options.KeepDatabases, "keep-databases", false, "retain compacted sample databases")
	flags.IntVar(&options.Samples, "samples", environmentInt("GRAFO_DETAIL_SAMPLES", 1), "number of raw samples")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	options.Profile = detailprofile.Profile(profile)
	for _, root := range strings.Split(roots, ",") {
		if root = strings.TrimSpace(root); root != "" {
			options.FullDetailRoots = append(options.FullDetailRoots, root)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := detailprofile.RunBenchmark(ctx, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grafo detail benchmark:", err)
		return 1
	}
	if _, err := fmt.Fprintln(os.Stdout, report.Artifacts.Report); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "grafo detail benchmark:", err)
		return 1
	}
	return 0
}

func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func environmentInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
