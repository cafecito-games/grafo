package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cafecito-games/grafo/internal/mcpreap"
)

// processReaper is the process scan `grafo reap` and `grafo mcp` share, as an
// interface so a test drives the report without signalling anything real.
type processReaper interface {
	Reap(dryRun bool) ([]mcpreap.Result, error)
}

// newReapScanner builds the scanner for this machine. It is a variable for the
// same reason: the command is tested over fabricated results.
var newReapScanner = func() processReaper { return mcpreap.NewScanner() }

// reap asks every stdio MCP server that outlived its client to stop.
//
// With --hook it is a client's session-teardown hook and must fail open: a scan
// that cannot run is never allowed to fail a session's teardown, so the command
// reports nothing and exits 0. Invoked by hand it reports what it did and fails
// loudly, because a caller who asked for the scan is owed its outcome.
func (a *App) reap(_ context.Context, args parsedArguments) error {
	if len(args.positionals) != 0 {
		return fmt.Errorf("usage: grafo reap [--dry-run]")
	}
	if err := rejectUnsupportedOptions(args, "reap",
		map[string]bool{"dry-run": true}, map[string]bool{"hook": true}); err != nil {
		return err
	}
	hook := strings.TrimSpace(args.values["hook"]) != ""
	dryRun := args.flags["dry-run"]
	results, err := newReapScanner().Reap(dryRun)
	if err != nil {
		switch {
		case hook:
			return nil
		case errors.Is(err, mcpreap.ErrUnsupported):
			// Not a failure: the platform offers no process table, and saying so
			// keeps an empty report from reading as proof that nothing was left
			// behind.
			a.printf("%v; nothing was scanned\n", err)
			return nil
		default:
			return err
		}
	}
	if hook {
		a.reportHookReap(results)
		return nil
	}
	a.reportReap(results, dryRun)
	return nil
}

// reportReap renders one line per selected server plus a summary, so a caller
// can see which process was signalled and what proved it had no client.
func (a *App) reportReap(results []mcpreap.Result, dryRun bool) {
	if len(results) == 0 {
		a.println("no orphaned grafo mcp servers")
		return
	}
	signalled := 0
	for _, result := range results {
		switch {
		case result.Err != nil:
			a.printf("%d\t%s\tnot signalled: %v\n", result.PID, result.Reason, result.Err)
		case dryRun:
			a.printf("%d\t%s\twould signal\n", result.PID, result.Reason)
		case result.Signalled:
			signalled++
			a.printf("%d\t%s\tsignalled\n", result.PID, result.Reason)
		}
	}
	if dryRun {
		a.printf("%d orphaned · nothing signalled (dry run)\n", len(results))
		return
	}
	a.printf("%d signalled · %d orphaned\n", signalled, len(results))
}

// reportHookReap stays silent unless the hook actually stopped something, so a
// client's teardown transcript records only the cleanup that happened.
func (a *App) reportHookReap(results []mcpreap.Result) {
	for _, result := range results {
		if result.Signalled {
			a.printf("Grafo stopped orphaned mcp server %d (%s).\n", result.PID, result.Reason)
		}
	}
}

// reapOrphanedServers cleans up servers left by earlier clients before this one
// starts serving. It is advisory: a scan that fails is reported on stderr and
// never keeps this server from answering.
func (a *App) reapOrphanedServers() {
	results, err := newReapScanner().Reap(false)
	if err != nil {
		if !errors.Is(err, mcpreap.ErrUnsupported) {
			a.errorf("grafo mcp: could not look for orphaned servers: %v\n", err)
		}
		return
	}
	for _, result := range results {
		if result.Signalled {
			a.errorf("grafo mcp: asked orphaned server %d to stop (%s)\n", result.PID, result.Reason)
		}
	}
}
