// Package mcpreap keeps a stdio MCP server from outliving the client that
// spawned it.
//
// A stdio server normally ends when its client does: the client closes the
// write end of the server's stdin pipe, the transport reads EOF, and the server
// exits. That write end closes only once every holder of the descriptor closes
// it, so a client that forked a helper which inherited the descriptor and was
// then killed leaves the server blocked on a read that can never return EOF.
// Nothing can write a request to such a server again, yet it keeps an index
// database open for a tree that may no longer exist.
//
// Guard watches for that condition from inside the server. Scanner cleans up
// the servers that were already left behind.
package mcpreap

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// defaultInterval is how often a guard looks for a vanished client. Each check
// reads one process id and stats the configured roots, so the cost is
// negligible; a server that lingers at most this long past its client holds
// nothing long enough to matter.
const defaultInterval = 15 * time.Second

// GuardOptions configures one guard. Every probe is injected so the watchdog is
// testable without real processes, real clocks, or a real filesystem.
type GuardOptions struct {
	// Roots are the repository roots this server was asked to serve. An empty
	// slice disables the root check, leaving only the parent check: a server
	// without roots has nothing whose removal could speak for its client.
	Roots []string
	// Parent reports the current parent process id. Defaults to os.Getppid.
	Parent func() int
	// RootExists reports whether one root is still present on disk. Defaults to
	// a stat.
	RootExists func(string) bool
	// Ticks drives the checks. Defaults to a ticker of Interval.
	Ticks <-chan time.Time
	// Interval overrides defaultInterval for the default ticker.
	Interval time.Duration
	// Report receives one line naming why the server is shutting down. The
	// server is about to exit, so that line is the only record of the reason.
	Report func(string)

	// checked and stopped are test seams that observe the watchdog's progress
	// without a sleep. They are unexported because no caller needs them.
	checked func()
	stopped func()
}

// Guard returns a context cancelled once the client that spawned this server is
// gone, together with a function that stops the watchdog.
//
// Cancellation rather than exit is deliberate: the server's own shutdown path
// closes the freshness coordinator and the index it holds, and that path only
// runs when the context it was given ends.
//
// Two conditions speak for a dead client. The first is reparenting: a process
// is reparented only when its original parent dies, so a parent id that differs
// from the one captured here proves the client is gone. The comparison is
// against the captured id rather than against 1, because under a subreaper the
// new parent is the subreaper and not init. The second is the disappearance of
// every configured root, which is the worktree case: a client that deleted the
// tree out from under the server cannot want answers about it. Both are facts
// about this server's own situation; neither infers anything from how long it
// has been running or how many servers exist.
func Guard(ctx context.Context, options GuardOptions) (context.Context, func()) {
	parent := options.Parent
	if parent == nil {
		parent = os.Getppid
	}
	rootExists := options.RootExists
	if rootExists == nil {
		rootExists = func(root string) bool {
			_, err := os.Stat(root)
			return err == nil
		}
	}
	guarded, cancel := context.WithCancel(ctx)
	ticks := options.Ticks
	stopTicker := func() {}
	if ticks == nil {
		interval := options.Interval
		if interval <= 0 {
			interval = defaultInterval
		}
		ticker := time.NewTicker(interval)
		ticks, stopTicker = ticker.C, ticker.Stop
	}
	quit := make(chan struct{})
	original := parent()
	go func() {
		if options.stopped != nil {
			defer options.stopped()
		}
		for {
			select {
			case <-guarded.Done():
				return
			case <-quit:
				return
			case <-ticks:
				if reason := clientGone(original, parent, rootExists, options.Roots); reason != "" {
					report(options.Report, reason)
					cancel()
					return
				}
				if options.checked != nil {
					options.checked()
				}
			}
		}
	}()
	// A caller that stops the guard explicitly and also defers a stop must not
	// close the quit channel twice, so the shutdown runs once.
	var once sync.Once
	return guarded, func() {
		once.Do(func() {
			stopTicker()
			close(quit)
		})
		cancel()
	}
}

// clientGone names the condition that proves this server has no client left, or
// returns the empty string while it still has one.
func clientGone(original int, parent func() int, rootExists func(string) bool, roots []string) string {
	if current := parent(); current != original {
		return fmt.Sprintf("client process %d exited (now parented to %d)", original, current)
	}
	if len(roots) == 0 {
		return ""
	}
	for _, root := range roots {
		if rootExists(root) {
			return ""
		}
	}
	return fmt.Sprintf("every repository root was removed (%s)", strings.Join(roots, ", "))
}

// report delivers one shutdown reason when the caller asked for them.
func report(sink func(string), reason string) {
	if sink != nil {
		sink(reason)
	}
}
