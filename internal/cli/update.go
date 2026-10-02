package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/cafecito-games/grafo/internal/updatecheck"
)

// version renders the running build, and on --check reports whether a newer
// release exists. The explicit form bypasses the cached advisory check entirely
// and, unlike it, reports its failures: the caller asked.
func (a *App) version(ctx context.Context, args parsedArguments) int {
	checker := a.updateChecker()
	current := checker.CurrentVersion
	if !args.flags["check"] {
		a.println("grafo " + current)
		return 0
	}
	latest, err := checker.LatestRelease(ctx)
	if err != nil {
		a.println("grafo " + current)
		a.fail(fmt.Errorf("check for a newer release: %w", err))
		return 1
	}
	switch {
	case !updatecheck.IsRelease(current):
		// A development build has no release it can be compared against, so the
		// newest release is reported without an upgrade command that would move
		// the caller off the build they deliberately made.
		a.printf("grafo %s (development build; latest release is %s)\n",
			updatecheck.Display(current), updatecheck.Display(latest))
	case !updatecheck.Newer(current, latest):
		a.printf("grafo %s (up to date)\n", updatecheck.Display(current))
	default:
		a.printf("grafo %s (%s available)\n",
			updatecheck.Display(current), updatecheck.Display(latest))
		a.printf("update with: %s\n", checker.UpgradeHint(latest))
	}
	return 0
}

// updateChecker returns the release checker, building the production one on
// first use. Tests install their own so no test reaches the network or the real
// cache directory.
func (a *App) updateChecker() *updatecheck.Checker {
	if a.checker == nil {
		a.checker = updatecheck.NewChecker(Version)
	}
	return a.checker
}

// startUpdateCheck begins the advisory release check, or returns nil when this
// run cannot carry its output. A nil lookup reports no notice, so every caller
// is the same shape.
//
// The check is withheld from `grafo mcp`, which serves a protocol rather than a
// person, from --json runs whose consumer parses what it is given, and from a
// non-terminal stderr, which is a log or a pipe nobody upgrades from.
func (a *App) startUpdateCheck(ctx context.Context, args parsedArguments) *updatecheck.Lookup {
	if args.command == "mcp" || args.flags["json"] {
		return nil
	}
	if updatecheck.Disabled(os.LookupEnv) {
		return nil
	}
	if !a.stderrIsTerminal(a.stderr) {
		return nil
	}
	checker := a.updateChecker()
	if !updatecheck.IsRelease(checker.CurrentVersion) {
		return nil
	}
	return checker.Start(ctx)
}

// reportUpdate prints the advisory after the command has rendered its own
// output. An interrupted run prints nothing: the caller asked to stop, and
// waiting on an in-flight lookup would delay the exit they asked for.
func (a *App) reportUpdate(ctx context.Context, lookup *updatecheck.Lookup) {
	if ctx.Err() != nil {
		return
	}
	if notice := lookup.Notice(); notice != "" {
		a.errorf("%s", notice)
	}
}
