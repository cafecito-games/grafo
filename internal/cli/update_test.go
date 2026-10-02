package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
	"github.com/cafecito-games/grafo/internal/updatecheck"
)

// recordingFeed answers from memory and counts its calls so a test can assert
// that a suppressed check reached the network not at all.
type recordingFeed struct {
	tag   string
	err   error
	calls atomic.Int32
}

func (f *recordingFeed) LatestRelease(context.Context) (string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", f.err
	}
	return f.tag, nil
}

// updateApp builds an app whose release check is fully in memory: a stub feed, a
// temporary cache and a terminal stderr.
func updateApp(t *testing.T, feed updatecheck.Feed, current string) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr)
	app.stderrIsTerminal = func(io.Writer) bool { return true }
	app.checker = &updatecheck.Checker{
		CurrentVersion: current,
		ExecutablePath: "/opt/homebrew/bin/grafo",
		Method:         updatecheck.MethodHomebrew,
		Feed:           feed,
		CachePath:      filepath.Join(testtemp.Dir(t), "update-check.json"),
	}
	t.Setenv(updatecheck.EnvDisable, "0")
	t.Setenv("CI", "false")
	return app, stdout, stderr
}

// TestRunReportsANewerReleaseOnStderr covers the advisory on an ordinary
// command. It belongs on stderr so it never contaminates command output, and it
// has to name the command for the way this binary was installed.
func TestRunReportsANewerReleaseOnStderr(t *testing.T) {
	app, stdout, stderr := updateApp(t, &recordingFeed{tag: "v0.4.2"}, "0.4.1")

	app.Run(context.Background(), []string{"nonsense-command"})

	if !strings.Contains(stderr.String(), "0.4.2 is available (you have 0.4.1)") {
		t.Fatalf("stderr = %q, want the release advisory", stderr.String())
	}
	if !strings.Contains(stderr.String(), "brew upgrade --cask grafo") {
		t.Fatalf("stderr = %q, want the homebrew upgrade command", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want the advisory kept off it", stdout.String())
	}
}

// TestRunReportsTheNoticeAfterTheCommandOutput keeps the advisory from being
// mistaken for part of what the command had to say.
func TestRunReportsTheNoticeAfterTheCommandOutput(t *testing.T) {
	app, _, stderr := updateApp(t, &recordingFeed{tag: "v0.4.2"}, "0.4.1")

	app.Run(context.Background(), []string{"nonsense-command"})

	rendered := stderr.String()
	failure := strings.Index(rendered, "unknown command")
	advisory := strings.Index(rendered, "is available")
	if failure < 0 || advisory < 0 {
		t.Fatalf("stderr = %q, want both the failure and the advisory", rendered)
	}
	if advisory < failure {
		t.Fatalf("stderr = %q, want the advisory after the command's own output", rendered)
	}
}

func TestRunWithholdsTheNotice(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		arguments   []string
		current     string
		environment map[string]string
		terminal    bool
	}{
		{
			name:      "the mcp server speaks a protocol rather than to a person",
			arguments: []string{"mcp", "--repo", "/nonexistent"},
			current:   "0.4.1",
			terminal:  true,
		},
		{
			name:      "a json consumer parses what it is given",
			arguments: []string{"status", "--json", "--repo", "/nonexistent"},
			current:   "0.4.1",
			terminal:  true,
		},
		{
			name:      "a pipe or log is not something anyone upgrades from",
			arguments: []string{"nonsense-command"},
			current:   "0.4.1",
			terminal:  false,
		},
		{
			name:      "a development build has no release to compare against",
			arguments: []string{"nonsense-command"},
			current:   "0.1.0-dev",
			terminal:  true,
		},
		{
			name:        "the caller opted out",
			arguments:   []string{"nonsense-command"},
			current:     "0.4.1",
			environment: map[string]string{updatecheck.EnvDisable: "1"},
			terminal:    true,
		},
		{
			name:        "continuous integration",
			arguments:   []string{"nonsense-command"},
			current:     "0.4.1",
			environment: map[string]string{"CI": "true"},
			terminal:    true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			feed := &recordingFeed{tag: "v0.4.2"}
			app, _, stderr := updateApp(t, feed, testCase.current)
			app.stderrIsTerminal = func(io.Writer) bool { return testCase.terminal }
			for name, value := range testCase.environment {
				t.Setenv(name, value)
			}

			app.Run(context.Background(), testCase.arguments)

			if strings.Contains(stderr.String(), "is available") {
				t.Fatalf("stderr = %q, want no advisory", stderr.String())
			}
			if calls := feed.calls.Load(); calls != 0 {
				t.Fatalf("feed called %d times, want the check skipped entirely", calls)
			}
		})
	}
}

// TestRunWithholdsTheNoticeFromAnInterruptedCommand keeps the advisory from
// delaying an exit the caller asked for.
func TestRunWithholdsTheNoticeFromAnInterruptedCommand(t *testing.T) {
	app, _, stderr := updateApp(t, &recordingFeed{tag: "v0.4.2"}, "0.4.1")
	// An interrupted run returns without waiting for its lookup, so the lookup
	// is given nothing to write rather than a file that outlives the test.
	app.checker.CachePath = ""
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	app.Run(ctx, []string{"nonsense-command"})

	if strings.Contains(stderr.String(), "is available") {
		t.Fatalf("stderr = %q, want no advisory after an interruption", stderr.String())
	}
}

// TestRunKeepsAFailedAdvisoryCheckSilent is the guarantee the whole feature
// rests on: an unreachable feed changes neither the exit code nor the output.
func TestRunKeepsAFailedAdvisoryCheckSilent(t *testing.T) {
	app, _, stderr := updateApp(t, &recordingFeed{err: errors.New("no network")}, "0.4.1")

	if code := app.Run(context.Background(), []string{"nonsense-command"}); code != 1 {
		t.Fatalf("exit code = %d, want the command's own 1", code)
	}
	rendered := stderr.String()
	if strings.Contains(rendered, "no network") || strings.Contains(rendered, "is available") {
		t.Fatalf("stderr = %q, want only the command's own failure", rendered)
	}
}

func TestVersionPrintsTheRunningBuild(t *testing.T) {
	feed := &recordingFeed{tag: "v0.4.2"}
	app, stdout, _ := updateApp(t, feed, "0.4.1")

	if code := app.Run(context.Background(), []string{"version"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout.String() != "grafo 0.4.1\n" {
		t.Fatalf("stdout = %q, want the running build alone", stdout.String())
	}
	if calls := feed.calls.Load(); calls != 0 {
		t.Fatalf("feed called %d times, want no lookup without --check", calls)
	}
}

func TestVersionCheck(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		current  string
		tag      string
		want     []string
		unwanted []string
	}{
		{
			name:    "a newer release names the upgrade command",
			current: "0.4.1",
			tag:     "v0.4.2",
			want:    []string{"grafo 0.4.1 (0.4.2 available)", "update with: brew upgrade --cask grafo"},
		},
		{
			name:     "the newest release says so",
			current:  "0.4.2",
			tag:      "v0.4.2",
			want:     []string{"grafo 0.4.2 (up to date)"},
			unwanted: []string{"update with"},
		},
		{
			name:     "a build ahead of the newest release is not pushed backwards",
			current:  "0.5.0",
			tag:      "v0.4.2",
			want:     []string{"grafo 0.5.0 (up to date)"},
			unwanted: []string{"update with"},
		},
		{
			name:     "a development build is told the release without being moved off it",
			current:  "0.1.0-dev",
			tag:      "v0.4.2",
			want:     []string{"grafo 0.1.0-dev (development build; latest release is 0.4.2)"},
			unwanted: []string{"update with"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app, stdout, _ := updateApp(t, &recordingFeed{tag: testCase.tag}, testCase.current)

			if code := app.Run(context.Background(), []string{"version", "--check"}); code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			for _, fragment := range testCase.want {
				if !strings.Contains(stdout.String(), fragment) {
					t.Errorf("stdout = %q, want it to mention %q", stdout.String(), fragment)
				}
			}
			for _, fragment := range testCase.unwanted {
				if strings.Contains(stdout.String(), fragment) {
					t.Errorf("stdout = %q, want it not to mention %q", stdout.String(), fragment)
				}
			}
		})
	}
}

// TestVersionCheckReportsItsFailure is the one path that surfaces a lookup
// error, because the caller asked for the lookup.
func TestVersionCheckReportsItsFailure(t *testing.T) {
	app, stdout, stderr := updateApp(t, &recordingFeed{err: errors.New("no network")}, "0.4.1")

	if code := app.Run(context.Background(), []string{"version", "--check"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "grafo 0.4.1") {
		t.Fatalf("stdout = %q, want the running build reported regardless", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no network") {
		t.Fatalf("stderr = %q, want the failure reported", stderr.String())
	}
}

// TestVersionCheckRunsWithoutATerminal covers an explicit check from a script:
// the suppression rules govern the unsolicited advisory, not this.
func TestVersionCheckRunsWithoutATerminal(t *testing.T) {
	app, stdout, _ := updateApp(t, &recordingFeed{tag: "v0.4.2"}, "0.4.1")
	app.stderrIsTerminal = func(io.Writer) bool { return false }
	t.Setenv(updatecheck.EnvDisable, "1")

	if code := app.Run(context.Background(), []string{"version", "--check"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "0.4.2 available") {
		t.Fatalf("stdout = %q, want the requested answer", stdout.String())
	}
}

func TestCheckFlagIsRejectedElsewhere(t *testing.T) {
	app, _, stderr := updateApp(t, &recordingFeed{tag: "v0.4.2"}, "0.4.1")

	if code := app.Run(context.Background(), []string{"status", "--check"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--check is not supported by status") {
		t.Fatalf("stderr = %q, want the flag rejected by name", stderr.String())
	}
}
