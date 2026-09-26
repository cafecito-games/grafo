package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	evaluation "github.com/cafecito-games/grafo/internal/eval"
)

func main() {
	update := flag.Bool("update", false, "atomically replace committed structural expectations")
	flag.Parse()
	if !*update || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/eval/cmd/grafo-eval -update <corpus-root>")
		os.Exit(2)
	}
	root, err := filepath.Abs(flag.Arg(0))
	if err == nil {
		err = evaluation.RunCorpus(context.Background(), root, true)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "grafo-eval:", err)
		os.Exit(1)
	}
}
