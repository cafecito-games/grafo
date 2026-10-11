package mcpreap

import (
	"context"
	"slices"
	"testing"
	"time"
)

// waitForCancel reports whether a context was cancelled before the test's
// patience ran out. The guard cancels from its own goroutine, so a test cannot
// assert on the context synchronously after sending a tick.
func waitForCancel(t *testing.T, ctx context.Context) bool {
	t.Helper()
	select {
	case <-ctx.Done():
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// requireStillRunning asserts a context is not cancelled once the guard has
// certainly observed a tick. The guard acknowledges every tick it consumes, so
// the wait is bounded rather than a sleep.
func requireStillRunning(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("guard cancelled the server while its client and roots were alive")
	default:
	}
}

func TestGuardCancelsWhenTheParentProcessChanges(t *testing.T) {
	ticks := make(chan time.Time)
	parent := 4242
	var reported []string
	guarded, stop := Guard(context.Background(), GuardOptions{
		Parent:     func() int { return parent },
		Ticks:      ticks,
		Report:     func(line string) { reported = append(reported, line) },
		RootExists: func(string) bool { return true },
	})
	defer stop()

	parent = 1
	ticks <- time.Now()

	if !waitForCancel(t, guarded) {
		t.Fatal("guard did not cancel the server after its client exited")
	}
	if len(reported) != 1 {
		t.Fatalf("expected one reported reason, got %v", reported)
	}
}

func TestGuardKeepsServingWhileTheParentAndRootsLive(t *testing.T) {
	ticks := make(chan time.Time)
	acknowledged := make(chan struct{}, 1)
	guarded, stop := Guard(context.Background(), GuardOptions{
		Roots:      []string{"/repo"},
		Parent:     func() int { return 4242 },
		RootExists: func(string) bool { return true },
		Ticks:      ticks,
		checked:    func() { acknowledged <- struct{}{} },
	})
	defer stop()

	for range 3 {
		ticks <- time.Now()
		<-acknowledged
	}
	requireStillRunning(t, guarded)
}

func TestGuardCancelsWhenEveryRootIsRemoved(t *testing.T) {
	ticks := make(chan time.Time)
	var reported []string
	guarded, stop := Guard(context.Background(), GuardOptions{
		Roots:      []string{"/gone/one", "/gone/two"},
		Parent:     func() int { return 4242 },
		RootExists: func(string) bool { return false },
		Ticks:      ticks,
		Report:     func(line string) { reported = append(reported, line) },
	})
	defer stop()

	ticks <- time.Now()

	if !waitForCancel(t, guarded) {
		t.Fatal("guard did not cancel the server after every root was removed")
	}
	if len(reported) != 1 {
		t.Fatalf("expected one reported reason, got %v", reported)
	}
}

// A federated server outlives one unavailable root: a removed worktree among
// several is a reason to report nothing and keep answering for the rest.
func TestGuardKeepsServingWhenOneOfSeveralRootsIsRemoved(t *testing.T) {
	ticks := make(chan time.Time)
	acknowledged := make(chan struct{}, 1)
	present := []string{"/live"}
	guarded, stop := Guard(context.Background(), GuardOptions{
		Roots:      []string{"/live", "/gone"},
		Parent:     func() int { return 4242 },
		RootExists: func(root string) bool { return slices.Contains(present, root) },
		Ticks:      ticks,
		checked:    func() { acknowledged <- struct{}{} },
	})
	defer stop()

	ticks <- time.Now()
	<-acknowledged
	requireStillRunning(t, guarded)
}

// A server with no configured roots still watches its client: the root check is
// disabled rather than vacuously true, which would cancel on the first tick.
func TestGuardWithoutRootsWatchesOnlyTheParent(t *testing.T) {
	ticks := make(chan time.Time)
	acknowledged := make(chan struct{}, 1)
	guarded, stop := Guard(context.Background(), GuardOptions{
		Parent:     func() int { return 4242 },
		RootExists: func(string) bool { return false },
		Ticks:      ticks,
		checked:    func() { acknowledged <- struct{}{} },
	})
	defer stop()

	ticks <- time.Now()
	<-acknowledged
	requireStillRunning(t, guarded)
}

// Cancelling the parent context must still cancel the guarded one, so the
// ordinary EOF and signal paths shut the server down exactly as before.
func TestGuardPropagatesCancellationFromItsParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	guarded, stop := Guard(ctx, GuardOptions{
		Parent: func() int { return 4242 },
		Ticks:  make(chan time.Time),
	})
	defer stop()

	cancel()

	if !waitForCancel(t, guarded) {
		t.Fatal("guarded context outlived its parent context")
	}
}

func TestGuardStopEndsTheWatchdog(t *testing.T) {
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	_, stop := Guard(context.Background(), GuardOptions{
		Parent:  func() int { return 4242 },
		Ticks:   ticks,
		stopped: func() { close(stopped) },
	})

	stop()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog goroutine outlived the stop function")
	}
	// Stop is idempotent: the deferred stop in a caller must be harmless after
	// an explicit one.
	stop()
}
