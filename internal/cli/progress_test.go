package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestStatusProgressModesKeepStdoutMachineReadable(t *testing.T) {
	root := indexedRepository(t)
	tests := []struct {
		name         string
		mode         string
		terminal     bool
		delay        time.Duration
		wantProgress bool
		jsonProgress bool
	}{
		{name: "auto pipe stays quiet", mode: "auto", terminal: false, delay: 0},
		{name: "fast auto terminal stays quiet", mode: "auto", terminal: true, delay: time.Hour},
		{name: "slow auto terminal reports", mode: "auto", terminal: true, delay: 0, wantProgress: true},
		{name: "human reports", mode: "human", wantProgress: true},
		{name: "json reports ndjson", mode: "json", wantProgress: true, jsonProgress: true},
		{name: "off stays quiet", mode: "off"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := New(&stdout, &stderr)
			app.stderrIsTerminal = func(io.Writer) bool { return test.terminal }
			app.progressDelay = test.delay
			code := app.Run(context.Background(), []string{"status", "--progress", test.mode, "--json", root})
			if code != 0 {
				t.Fatalf("status exited %d: %s", code, stderr.String())
			}
			var output statusOutput
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout.String())
			}
			if output.Refresh == nil || len(output.Refresh) != 1 {
				t.Fatalf("refresh summary = %#v", output.Refresh)
			}
			if got := stderr.Len() > 0; got != test.wantProgress {
				t.Fatalf("progress=%v want %v: %q", got, test.wantProgress, stderr.String())
			}
			if test.jsonProgress {
				for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
					var event indexer.ProgressEvent
					if err := json.Unmarshal([]byte(line), &event); err != nil || event.Schema != indexer.ProgressSchemaV1 {
						t.Fatalf("invalid NDJSON progress %q: %#v %v", line, event, err)
					}
				}
			}
		})
	}
}

func TestAutoProgressPublishesCachedPhaseAndStopsBeforeReturn(t *testing.T) {
	var stderr bytes.Buffer
	renderer := newProgressRenderer(&stderr, progressAuto, true, 10*time.Millisecond)
	event := indexer.ProgressEvent{Schema: indexer.ProgressSchemaV1, RepositoryName: "sample", Phase: indexer.ProgressParse, State: indexer.ProgressStarted}
	if err := renderer.Observe(event); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := renderer.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "sample parse: started") {
		t.Fatalf("cached active phase was not rendered: %q", stderr.String())
	}
	stable := stderr.String()
	time.Sleep(20 * time.Millisecond)
	if stderr.String() != stable {
		t.Fatalf("late timer write after close: before=%q after=%q", stable, stderr.String())
	}
}

type failWriter struct{ err error }

func (writer failWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestStatusProgressWriteFailurePreventsSuccessAndRetryConverges(t *testing.T) {
	root := indexedRepository(t)
	sentinel := errors.New("stderr failed")
	var stdout bytes.Buffer
	app := New(&stdout, failWriter{err: sentinel})
	code := app.Run(context.Background(), []string{"status", "--progress=human", root})
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("write failure: code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	var stderr bytes.Buffer
	app = New(&stdout, &stderr)
	if code := app.Run(context.Background(), []string{"status", "--progress=off", "--json", root}); code != 0 {
		t.Fatalf("retry exited %d: %s", code, stderr.String())
	}
	var output statusOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || len(output.Refresh) != 1 {
		t.Fatalf("retry output=%q decoded=%#v err=%v", stdout.String(), output, err)
	}
}

func TestStatusProgressOptionIsPositionIndependentAndValidatedBeforeOpen(t *testing.T) {
	for _, arguments := range [][]string{
		{"status", "--progress=json", "/missing"},
		{"--progress", "human", "counts", "/missing"},
	} {
		args, err := parseArguments(arguments)
		if err != nil || args.values["progress"] == "" {
			t.Fatalf("parse %#v = %#v, %v", arguments, args, err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), []string{"status", "--progress", "wat", t.TempDir()})
	if code != 2 || !strings.Contains(stderr.String(), "--progress must be one of") {
		t.Fatalf("invalid progress: code=%d stderr=%q", code, stderr.String())
	}
	if _, err := parseArguments([]string{"find", "thing", "--progress", "off"}); err == nil {
		t.Fatal("non-status command accepted --progress")
	}
}

func TestStatusAndCountsAcceptGlobalJSONWithSameProgressContract(t *testing.T) {
	root := indexedRepository(t)
	for _, command := range []string{"status", "counts"} {
		var stdout, stderr bytes.Buffer
		code := New(&stdout, &stderr).Run(context.Background(), []string{"--json", "--progress=off", command, root})
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("%s: code=%d stderr=%q", command, code, stderr.String())
		}
		var output statusOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || output.Refresh == nil || len(output.Refresh) != 1 {
			t.Fatalf("%s output=%q decoded=%#v err=%v", command, stdout.String(), output, err)
		}
	}
}

func TestStatusReportsSemanticRebuildInHumanAndJSONCompletion(t *testing.T) {
	root := indexedRepository(t)
	project, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(context.Background(), project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(context.Background(), "semantic_index_version", "stale"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), []string{"status", "--json", "--progress=human", root})
	if code != 0 || !strings.Contains(stderr.String(), "refreshed ") || !strings.Contains(stderr.String(), "rebuild semantic schema changed") {
		t.Fatalf("rebuild status: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var output statusOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || len(output.Refresh) != 1 || output.Refresh[0].RebuildReason != "semantic schema changed" {
		t.Fatalf("rebuild summary=%#v err=%v", output.Refresh, err)
	}
}

func TestFederatedStatusRefreshSummariesFollowCanonicalProjectsAndDeduplicatePaths(t *testing.T) {
	left, right := indexedRepository(t), indexedRepository(t)
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), []string{
		"status", "--json", "--progress=off", "--repos", strings.Join([]string{right, left, right}, ","),
	})
	if code != 0 {
		t.Fatalf("federated status exited %d: %s", code, stderr.String())
	}
	var output statusOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Projects) != 2 || len(output.Refresh) != 2 {
		t.Fatalf("projects=%#v refresh=%#v", output.Projects, output.Refresh)
	}
	for index := range output.Projects {
		if output.Refresh[index].RepositoryID != output.Projects[index].ID {
			t.Fatalf("summary order differs from canonical projects: %#v %#v", output.Projects, output.Refresh)
		}
	}
}

func TestStatusJSONProgressOwnsTerminalError(t *testing.T) {
	root := indexedRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(ctx, []string{"status", "--progress=json", root})
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("canceled status: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "grafo:") {
		t.Fatalf("JSON progress mixed with plain error: %q", stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var terminal indexer.ProgressEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &terminal); err != nil || terminal.State != indexer.ProgressCanceled {
		t.Fatalf("terminal progress = %#v, %v; stderr=%q", terminal, err, stderr.String())
	}
}

func TestStatusProgressErrorsArePathFreeAndHumanErrorsAreNotDuplicated(t *testing.T) {
	missing := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), []string{"status", "--progress=json", missing})
	var missingEvent indexer.ProgressEvent
	decodeErr := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &missingEvent)
	if code != 1 || stdout.Len() != 0 || decodeErr != nil || strings.Contains(stderr.String(), missing) || strings.Contains(stderr.String(), "grafo:") ||
		!strings.Contains(missingEvent.Error, "has no index") || !strings.Contains(missingEvent.Error, "run 'grafo index <repository>'") {
		t.Fatalf("missing-index JSON: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	root := indexedRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stdout.Reset()
	stderr.Reset()
	code = New(&stdout, &stderr).Run(ctx, []string{"status", "--progress=human", root})
	if code != 1 || stdout.Len() != 0 || strings.Count(strings.TrimSpace(stderr.String()), "\n") != 0 || strings.Contains(stderr.String(), "grafo:") {
		t.Fatalf("human cancellation duplicated terminal error: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// indexableRepository writes an unindexed fixture under a fixed basename so
// two runs produce reports that differ only in their measured durations.
func indexableRepository(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sample")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module sample\n\ngo 1.26\n")
	write("charge.go", "package sample\n\n// Charge settles a payment.\nfunc Charge() error { return nil }\n")
	write("checkout.go", "package sample\n\nfunc Checkout() error {\n\treturn Charge()\n}\n")
	return root
}

func TestIndexProgressModesKeepStdoutMachineReadable(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		terminal     bool
		delay        time.Duration
		wantProgress bool
		jsonProgress bool
	}{
		{name: "auto pipe stays quiet", mode: "auto", terminal: false, delay: 0},
		{name: "fast auto terminal stays quiet", mode: "auto", terminal: true, delay: time.Hour},
		{name: "slow auto terminal reports", mode: "auto", terminal: true, delay: 0, wantProgress: true},
		{name: "human reports", mode: "human", wantProgress: true},
		{name: "json reports ndjson", mode: "json", wantProgress: true, jsonProgress: true},
		{name: "off stays quiet", mode: "off"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := indexableRepository(t)
			var stdout, stderr bytes.Buffer
			app := New(&stdout, &stderr)
			app.stderrIsTerminal = func(io.Writer) bool { return test.terminal }
			app.progressDelay = test.delay
			code := app.Run(context.Background(), []string{"index", "--progress", test.mode, "--json", root})
			if code != 0 {
				t.Fatalf("index exited %d: %s", code, stderr.String())
			}
			var report indexer.Report
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout.String())
			}
			if len(report.Updated) == 0 {
				t.Fatalf("report indexed nothing: %#v", report)
			}
			if got := stderr.Len() > 0; got != test.wantProgress {
				t.Fatalf("progress=%v want %v: %q", got, test.wantProgress, stderr.String())
			}
			if test.wantProgress && !test.jsonProgress &&
				!strings.Contains(stderr.String(), string(indexer.ProgressParse)) {
				t.Fatalf("human progress never named the parse phase: %q", stderr.String())
			}
			if test.jsonProgress {
				phases := map[indexer.ProgressPhase]bool{}
				for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
					var event indexer.ProgressEvent
					if err := json.Unmarshal([]byte(line), &event); err != nil || event.Schema != indexer.ProgressSchemaV1 {
						t.Fatalf("invalid NDJSON progress %q: %#v %v", line, event, err)
					}
					phases[event.Phase] = true
				}
				for _, phase := range []indexer.ProgressPhase{indexer.ProgressParse, indexer.ProgressPersistence, indexer.ProgressComplete} {
					if !phases[phase] {
						t.Fatalf("progress never reported %s: %q", phase, stderr.String())
					}
				}
			}
		})
	}
}

func TestIndexProgressOffMatchesTheUnflaggedRun(t *testing.T) {
	var flagged, flaggedErr bytes.Buffer
	app := New(&flagged, &flaggedErr)
	app.stderrIsTerminal = func(io.Writer) bool { return false }
	if code := app.Run(context.Background(), []string{"index", "--progress=off", indexableRepository(t)}); code != 0 {
		t.Fatalf("flagged index exited: %s", flaggedErr.String())
	}
	var plain, plainErr bytes.Buffer
	app = New(&plain, &plainErr)
	app.stderrIsTerminal = func(io.Writer) bool { return false }
	if code := app.Run(context.Background(), []string{"index", indexableRepository(t)}); code != 0 {
		t.Fatalf("plain index exited: %s", plainErr.String())
	}
	if flaggedErr.Len() != 0 || plainErr.Len() != 0 {
		t.Fatalf("progress leaked: flagged=%q plain=%q", flaggedErr.String(), plainErr.String())
	}
	// Durations are the one part of the report that legitimately differs
	// between two runs of the same fixture.
	measured := regexp.MustCompile(`[0-9]+ms`)
	normalize := func(text string) string { return measured.ReplaceAllString(text, "Nms") }
	if normalize(flagged.String()) != normalize(plain.String()) {
		t.Fatalf("--progress=off changed output:\n%q\n%q", flagged.String(), plain.String())
	}
}

func TestIndexProgressWriteFailureStillCompletesTheIndex(t *testing.T) {
	root := indexableRepository(t)
	var stdout bytes.Buffer
	app := New(&stdout, failWriter{err: errors.New("stderr failed")})
	if code := app.Run(context.Background(), []string{"index", "--progress=human", "--json", root}); code != 0 {
		t.Fatalf("renderer failure failed the index: code=%d stdout=%q", code, stdout.String())
	}
	var report indexer.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Updated) == 0 {
		t.Fatalf("report=%#v err=%v stdout=%q", report, err, stdout.String())
	}
}

// cancelingWriter interrupts the run as soon as progress proves the marked
// phase is under way, so an interruption test measures real phase durations
// instead of cancelling before any work has been done.
type cancelingWriter struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
	marker string
	cancel context.CancelFunc
	fired  bool
}

func (writer *cancelingWriter) Write(data []byte) (int, error) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	written, err := writer.buffer.Write(data)
	if !writer.fired && strings.Contains(string(data), writer.marker) {
		writer.fired = true
		writer.cancel()
	}
	return written, err
}

func (writer *cancelingWriter) String() string {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.buffer.String()
}

func (writer *cancelingWriter) interrupted() bool {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.fired
}

func TestCanceledIndexReportsAccumulatedPhasesAndFails(t *testing.T) {
	root := indexableRepository(t)
	runInterrupted := func(t *testing.T, arguments ...string) (string, *cancelingWriter) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stderr := &cancelingWriter{marker: "parse: progress", cancel: cancel}
		var stdout bytes.Buffer
		app := New(&stdout, stderr)
		app.stderrIsTerminal = func(io.Writer) bool { return false }
		code := app.Run(ctx, append([]string{"index", "--progress=human", "--force", "--counts"}, arguments...))
		if !stderr.interrupted() {
			t.Fatalf("progress never reached the parse phase: %q", stderr.String())
		}
		if code != 1 {
			t.Fatalf("interrupted index exited %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		return stdout.String(), stderr
	}

	stdout, stderr := runInterrupted(t, "--json", root)
	var report indexer.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("interrupted run did not emit one JSON report: %v\n%q", err, stdout)
	}
	if report.Phases.ReadHashNS <= 0 || report.Phases.ParseNS <= 0 || report.Phases.TotalNS <= 0 {
		t.Fatalf("interrupted report lost the phase timings it had measured: %#v", report.Phases)
	}
	// --counts is requested, so a completed run would report collected counts;
	// an interrupted one never reaches the counts query.
	if report.CountsCollected {
		t.Fatalf("interrupted report claims complete counts: %#v", report)
	}
	if strings.Contains(stderr.String(), "grafo:") {
		t.Fatalf("human progress was duplicated by a plain error: %q", stderr.String())
	}

	stdout, _ = runInterrupted(t, root)
	for _, want := range []string{"interrupted index of sample", "read+hash", "parse", "persistence"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("human phase block is missing %q: %q", want, stdout)
		}
	}
}

func TestIndexAcceptsProgressOptionAndRejectsUnknownMode(t *testing.T) {
	args, err := parseArguments([]string{"index", "--progress=json", "/repo"})
	if err != nil || args.values["progress"] != "json" {
		t.Fatalf("parse index --progress = %#v, %v", args, err)
	}
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), []string{"index", "--progress", "wat", t.TempDir()})
	if code != 2 || !strings.Contains(stderr.String(), "--progress must be one of") {
		t.Fatalf("invalid progress: code=%d stderr=%q", code, stderr.String())
	}
}
