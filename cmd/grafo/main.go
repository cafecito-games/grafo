package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/cafecito-games/grafo/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(cli.New(os.Stdout, os.Stderr).Run(ctx, os.Args[1:]))
}
