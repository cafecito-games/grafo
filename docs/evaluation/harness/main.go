package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	"github.com/cafecito-games/grafo/internal/search"
	"github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Request struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 || strings.TrimSpace(os.Args[1]) == "" {
		return errors.New("usage: evaluation-harness /path/to/indexed/repository")
	}
	ctx := context.Background()
	p, e := indexer.DiscoverProject(ctx, os.Args[1])
	if e != nil {
		return fmt.Errorf("discover project: %w", e)
	}
	if override := os.Getenv("GRAFO_EVALUATION_INDEX"); override != "" {
		p.IndexPath = override
	}
	if os.Getenv("GRAFO_EVALUATION_PREPARE") == "1" {
		if os.Getenv("GRAFO_EVALUATION_INDEX") == "" {
			return errors.New("prepare requires GRAFO_EVALUATION_INDEX pointing to a disposable cloned index")
		}
		info, err := os.Stat(p.IndexPath)
		if err != nil {
			return fmt.Errorf("prepare requires an existing cloned index: %w", err)
		}
		if !info.Mode().IsRegular() {
			return errors.New("prepare index must be a regular file")
		}
		w, err := sqlite.Open(ctx, p.IndexPath)
		if err != nil {
			return fmt.Errorf("prepare cloned index: %w", err)
		}
		if err = w.Close(); err != nil {
			return fmt.Errorf("close prepared index: %w", err)
		}
		return nil
	}
	r, e := sqlite.OpenReadOnly(ctx, p.IndexPath)
	if e != nil {
		return fmt.Errorf("open read-only index: %w", e)
	}
	defer r.Close()
	s := source.NewService(r, source.NewSingleProjectLocator(r, p))
	service := mcpserver.NewFederated(r, []indexer.Project{p}).
		WithSource(s.ReadKind).
		WithSearch(search.NewService([]search.Source{{Project: p, Catalog: r}})).
		WithInvestigationSource(source.NewScopedService)
	st, ct := mcp.NewInMemoryTransports()
	ss, e := service.Server("evaluation").Connect(ctx, st, nil)
	if e != nil {
		return fmt.Errorf("connect server: %w", e)
	}
	defer ss.Close()
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "evaluation", Version: "1"}, nil).Connect(ctx, ct, nil)
	if e != nil {
		return fmt.Errorf("connect client: %w", e)
	}
	defer cs.Close()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var a Request
		if err := json.Unmarshal(scanner.Bytes(), &a); err != nil || strings.TrimSpace(a.Name) == "" {
			if err == nil {
				err = errors.New("request requires nonempty name and optional arguments object")
			}
			if err := encoder.Encode(map[string]any{"response": nil, "milliseconds": 0, "error": err.Error(), "wrapper_error": true}); err != nil {
				return fmt.Errorf("write response: %w", err)
			}
			continue
		}
		start := time.Now()
		v, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: a.Name, Arguments: a.Arguments})
		out := map[string]any{"response": v, "milliseconds": float64(time.Since(start).Microseconds()) / 1000}
		if err != nil {
			out["error"] = err.Error()
		}
		if err := encoder.Encode(out); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return scanner.Err()
}
