package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/mcpreap"
)

// stubReaper stands in for the process scanner so the command's report is
// tested without signalling anything on the machine running the tests.
type stubReaper struct {
	results []mcpreap.Result
	err     error
	dryRuns []bool
}

func (s *stubReaper) Reap(dryRun bool) ([]mcpreap.Result, error) {
	s.dryRuns = append(s.dryRuns, dryRun)
	return s.results, s.err
}

// withReaper installs a stub scanner for one test and restores the real one.
func withReaper(t *testing.T, stub *stubReaper) {
	t.Helper()
	previous := newReapScanner
	newReapScanner = func() processReaper { return stub }
	t.Cleanup(func() { newReapScanner = previous })
}

// runReap runs the command the way a caller would, returning its exit status
// and both streams.
func runReap(t *testing.T, arguments ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := New(&stdout, &stderr).Run(context.Background(), arguments)
	return status, stdout.String(), stderr.String()
}

func TestReapReportsEveryServerItSignalled(t *testing.T) {
	stub := &stubReaper{results: []mcpreap.Result{
		{PID: 100, Reason: mcpreap.ReasonClientExited, Signalled: true},
		{PID: 200, Reason: mcpreap.ReasonWorkingDirectoryDeleted, Signalled: true},
	}}
	withReaper(t, stub)

	status, stdout, stderr := runReap(t, "reap")

	if status != 0 {
		t.Fatalf("status = %d, stderr = %q", status, stderr)
	}
	for _, want := range []string{"100", "200", string(mcpreap.ReasonClientExited), "2 signalled"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("report %q is missing %q", stdout, want)
		}
	}
	if len(stub.dryRuns) != 1 || stub.dryRuns[0] {
		t.Fatalf("expected one live run, got %v", stub.dryRuns)
	}
}

func TestReapDryRunAsksTheScannerNotToSignal(t *testing.T) {
	stub := &stubReaper{results: []mcpreap.Result{
		{PID: 100, Reason: mcpreap.ReasonClientExited},
	}}
	withReaper(t, stub)

	status, stdout, stderr := runReap(t, "reap", "--dry-run")

	if status != 0 {
		t.Fatalf("status = %d, stderr = %q", status, stderr)
	}
	if len(stub.dryRuns) != 1 || !stub.dryRuns[0] {
		t.Fatalf("expected one dry run, got %v", stub.dryRuns)
	}
	if !strings.Contains(stdout, "100") || !strings.Contains(stdout, "would signal") {
		t.Fatalf("dry-run report %q does not say what it would do", stdout)
	}
	if strings.Contains(stdout, "1 signalled") {
		t.Fatalf("dry-run report %q claims it signalled something", stdout)
	}
}

func TestReapReportsWhenThereIsNothingToReap(t *testing.T) {
	withReaper(t, &stubReaper{})

	status, stdout, stderr := runReap(t, "reap")

	if status != 0 {
		t.Fatalf("status = %d, stderr = %q", status, stderr)
	}
	if !strings.Contains(stdout, "no orphaned") {
		t.Fatalf("report %q does not say the machine is clean", stdout)
	}
}

// A server that could not be signalled is reported as such rather than counted
// as reaped.
func TestReapReportsAServerItCouldNotSignal(t *testing.T) {
	withReaper(t, &stubReaper{results: []mcpreap.Result{
		{PID: 100, Reason: mcpreap.ReasonClientExited, Err: errors.New("operation not permitted")},
	}})

	status, stdout, _ := runReap(t, "reap")

	if status != 0 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(stdout, "operation not permitted") || strings.Contains(stdout, "1 signalled") {
		t.Fatalf("report %q misreports a failed signal", stdout)
	}
}

// A platform without procfs reports that it looked at nothing, and still exits
// cleanly so a hook on such a machine is harmless.
func TestReapOnAPlatformWithoutProcessScanning(t *testing.T) {
	withReaper(t, &stubReaper{err: mcpreap.ErrUnsupported})

	status, stdout, stderr := runReap(t, "reap")

	if status != 0 {
		t.Fatalf("status = %d, stderr = %q", status, stderr)
	}
	if !strings.Contains(stdout, "unavailable") {
		t.Fatalf("report %q does not explain that nothing was scanned", stdout)
	}
}

// The hook must fail open: a scan that breaks can never be allowed to fail a
// client's session teardown.
func TestReapHookExitsCleanlyWhenTheScanFails(t *testing.T) {
	withReaper(t, &stubReaper{err: errors.New("procfs is not mounted")})

	status, _, _ := runReap(t, "reap", "--hook", "session-end")

	if status != 0 {
		t.Fatalf("hook status = %d, want 0", status)
	}
}

func TestReapWithoutTheHookReportsAFailedScan(t *testing.T) {
	withReaper(t, &stubReaper{err: errors.New("procfs is not mounted")})

	status, _, stderr := runReap(t, "reap")

	if status == 0 {
		t.Fatal("a failed scan reported success")
	}
	if !strings.Contains(stderr, "procfs is not mounted") {
		t.Fatalf("stderr %q does not name the failure", stderr)
	}
}

func TestReapRejectsPositionalsAndUnsupportedOptions(t *testing.T) {
	withReaper(t, &stubReaper{})
	for _, arguments := range [][]string{
		{"reap", "/some/repo"},
		{"reap", "--repo", "/some/repo"},
		{"reap", "--json"},
	} {
		status, _, stderr := runReap(t, arguments...)
		if status == 0 {
			t.Fatalf("%v was accepted", arguments)
		}
		if stderr == "" {
			t.Fatalf("%v failed without explanation", arguments)
		}
	}
}

// The startup cleanup in `grafo mcp` reports on stderr, because stdout carries
// the MCP protocol and nothing else may be written there.
func TestMCPStartupReapReportsWhatItStoppedOnStderr(t *testing.T) {
	withReaper(t, &stubReaper{results: []mcpreap.Result{
		{PID: 100, Reason: mcpreap.ReasonClientExited, Signalled: true},
		{PID: 200, Reason: mcpreap.ReasonClientExited, Err: errors.New("operation not permitted")},
	}})
	var stdout, stderr bytes.Buffer

	New(&stdout, &stderr).reapOrphanedServers()

	if stdout.Len() != 0 {
		t.Fatalf("the startup cleanup wrote to the protocol stream: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "100") {
		t.Fatalf("stderr %q does not report the server it stopped", stderr.String())
	}
	if strings.Contains(stderr.String(), "200") {
		t.Fatalf("stderr %q claims a server it could not signal was stopped", stderr.String())
	}
}

// A scan that fails must not keep this server from serving, and a platform
// without process scanning is not a failure worth a line at all.
func TestMCPStartupReapIsAdvisory(t *testing.T) {
	for name, stub := range map[string]*stubReaper{
		"failed scan":      {err: errors.New("procfs is not mounted")},
		"unsupported":      {err: mcpreap.ErrUnsupported},
		"nothing to clean": {},
	} {
		t.Run(name, func(t *testing.T) {
			withReaper(t, stub)
			var stdout, stderr bytes.Buffer

			New(&stdout, &stderr).reapOrphanedServers()

			if stdout.Len() != 0 {
				t.Fatalf("the startup cleanup wrote to the protocol stream: %q", stdout.String())
			}
			if name == "unsupported" && stderr.Len() != 0 {
				t.Fatalf("an unsupported platform was reported as a problem: %q", stderr.String())
			}
			if name == "failed scan" && !strings.Contains(stderr.String(), "procfs is not mounted") {
				t.Fatalf("a failed scan went unreported: %q", stderr.String())
			}
		})
	}
}

// A shutdown the guard decided on is not a failure: its reason is already on
// stderr, and a client still alive must not read it as its server crashing.
// Every other outcome is reported exactly as it came back.
func TestMCPShutdownErrorOnlySwallowsAGuardedCancellation(t *testing.T) {
	broken := errors.New("the index could not be opened")
	cases := []struct {
		name         string
		err          error
		guardStopped bool
		want         error
	}{
		{name: "guard stopped the server", err: context.Canceled, guardStopped: true},
		{name: "guard stopped the server, wrapped", err: fmt.Errorf("serving: %w", context.Canceled), guardStopped: true},
		{name: "signalled run", err: context.Canceled, want: context.Canceled},
		{name: "clean end of input", err: nil},
		{name: "a real failure after the guard fired", err: broken, guardStopped: true, want: broken},
		{name: "a real failure", err: broken, want: broken},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := mcpShutdownError(testCase.err, testCase.guardStopped)
			if testCase.want == nil && got != nil {
				t.Fatalf("mcpShutdownError = %v, want nil", got)
			}
			if testCase.want != nil && !errors.Is(got, testCase.want) {
				t.Fatalf("mcpShutdownError = %v, want %v", got, testCase.want)
			}
		})
	}
}
