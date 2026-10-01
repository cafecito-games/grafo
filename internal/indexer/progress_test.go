package indexer_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

type progressParser struct{}

func (progressParser) Language() string          { return "progress" }
func (progressParser) Supports(path string) bool { return filepath.Ext(path) == ".progress" }
func (progressParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	builder := parserapi.NewBuilder(input, "progress")
	builder.AddNode(graph.Node{Kind: graph.KindFunction, Name: "Work", QualifiedName: "fixture.Work",
		Location: graph.Location{Path: input.Path, Line: 1, Column: 1}})
	return builder.Finish(), nil
}

func TestServiceProgressObserverIsOrderedAndObservational(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "sample.progress"), []byte("safe source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	registry := parserapi.NewRegistry(progressParser{})
	open := func(name string) *sqlite.Repository {
		repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = repository.Close() })
		return repository
	}
	without := open("without")
	with := open("with")
	plain, err := indexer.NewService(without, registry).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var events []indexer.ProgressEvent
	observed, err := indexer.NewService(with, registry).Run(ctx, project, indexer.Options{
		ProgressObserver: func(event indexer.ProgressEvent) error {
			events = append(events, event)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain.Counts, observed.Counts) || plain.Checked != observed.Checked ||
		!reflect.DeepEqual(plain.Updated, observed.Updated) || !reflect.DeepEqual(plain.Removed, observed.Removed) ||
		plain.Rebuild != observed.Rebuild || plain.Writes != observed.Writes {
		t.Fatalf("observer changed report\nplain=%#v\nobserved=%#v", plain, observed)
	}
	if len(events) < 2 || events[0].Phase != indexer.ProgressGitProbe || events[0].State != indexer.ProgressStarted {
		t.Fatalf("first event = %#v", events)
	}
	last := events[len(events)-1]
	if last.Phase != indexer.ProgressComplete || last.State != indexer.ProgressCompleted || last.Schema != indexer.ProgressSchemaV1 {
		t.Fatalf("last event = %#v", last)
	}
	if last.RebuildReason != observed.Rebuild || last.RebuildReason == "" {
		t.Fatalf("terminal rebuild reason=%q report=%q", last.RebuildReason, observed.Rebuild)
	}
	started := map[indexer.ProgressPhase]int{}
	completed := map[indexer.ProgressPhase]int{}
	firstStart := map[indexer.ProgressPhase]int{}
	for _, event := range events {
		if event.RepositoryID != project.ID || event.RepositoryName != project.Name || event.Branch != project.Branch {
			t.Fatalf("event lost project identity: %#v", event)
		}
		if event.State == indexer.ProgressStarted {
			started[event.Phase]++
			if _, exists := firstStart[event.Phase]; !exists {
				firstStart[event.Phase] = len(firstStart)
			}
		}
		if event.State == indexer.ProgressCompleted {
			completed[event.Phase]++
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "sample.progress") || strings.Contains(string(encoded), "safe source bytes") {
			t.Fatalf("progress leaked source detail: %s", encoded)
		}
	}
	ordered := []indexer.ProgressPhase{indexer.ProgressGitProbe, indexer.ProgressMembership, indexer.ProgressChangeProbe,
		indexer.ProgressDiscovery, indexer.ProgressReadHash, indexer.ProgressParse, indexer.ProgressPersistence,
		indexer.ProgressReconciliation}
	for index := 1; index < len(ordered); index++ {
		if firstStart[ordered[index-1]] >= firstStart[ordered[index]] {
			t.Fatalf("phase starts are not deterministic: %#v", events)
		}
	}
	for _, phase := range []indexer.ProgressPhase{indexer.ProgressGitProbe, indexer.ProgressMembership,
		indexer.ProgressDiscovery, indexer.ProgressChangeProbe, indexer.ProgressReadHash,
		indexer.ProgressParse, indexer.ProgressPersistence, indexer.ProgressReconciliation} {
		if started[phase] != 1 || completed[phase] != 1 {
			t.Fatalf("phase %s starts=%d completes=%d events=%#v", phase, started[phase], completed[phase], events)
		}
	}
	plainWarm, err := indexer.NewService(without, registry).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	observedWarm, err := indexer.NewService(with, registry).Run(ctx, project, indexer.Options{ProgressObserver: func(indexer.ProgressEvent) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if plainWarm.Checked != observedWarm.Checked || plainWarm.Unchanged != observedWarm.Unchanged ||
		!reflect.DeepEqual(plainWarm.Updated, observedWarm.Updated) || plainWarm.Writes != observedWarm.Writes {
		t.Fatalf("observer changed warm reuse/write decisions\nplain=%#v\nobserved=%#v", plainWarm, observedWarm)
	}
}

func TestServiceProgressObserverFailureIsTerminalAndFailClosed(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "sample.progress"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	sentinel := errors.New("progress writer failed")
	var terminal []indexer.ProgressEvent
	_, err = indexer.NewService(repository, parserapi.NewRegistry(progressParser{})).Run(ctx, project, indexer.Options{
		ProgressObserver: func(event indexer.ProgressEvent) error {
			if event.Phase == indexer.ProgressParse && event.State == indexer.ProgressProgress {
				return sentinel
			}
			if event.State == indexer.ProgressError || event.State == indexer.ProgressCanceled {
				terminal = append(terminal, event)
			}
			return nil
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want observer failure", err)
	}
	if len(terminal) != 1 || terminal[0].State != indexer.ProgressError {
		t.Fatalf("terminal events = %#v", terminal)
	}
}

func TestServiceProgressCancellationIsTerminalAndRetryable(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "sample.progress"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(progressParser{})
	var terminal []indexer.ProgressEvent
	_, err = indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{ProgressObserver: func(event indexer.ProgressEvent) error {
		if event.Phase == indexer.ProgressParse && event.State == indexer.ProgressProgress {
			return context.Canceled
		}
		if event.State == indexer.ProgressCanceled {
			terminal = append(terminal, event)
		}
		return nil
	}})
	if !errors.Is(err, context.Canceled) || len(terminal) != 1 {
		t.Fatalf("cancellation err=%v terminal=%#v", err, terminal)
	}
	report, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{})
	if err != nil || len(report.Updated) != 1 {
		t.Fatalf("retry report=%#v err=%v", report, err)
	}
}

func TestServiceTerminalProgressRedactsRepositoryPaths(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	path := filepath.Join(root, "sample.progress")
	if err := os.WriteFile(path, []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	var terminal indexer.ProgressEvent
	_, err = indexer.NewService(repository, parserapi.NewRegistry(progressParser{})).Run(ctx, project, indexer.Options{
		Boundary: func(boundary indexer.Boundary) error {
			if boundary.Kind == indexer.BoundaryFilePersisted {
				return errors.New("failed near " + path)
			}
			return nil
		},
		ProgressObserver: func(event indexer.ProgressEvent) error {
			if event.State == indexer.ProgressError {
				terminal = event
			}
			return nil
		},
	})
	if err == nil || terminal.State != indexer.ProgressError {
		t.Fatalf("error=%v terminal=%#v", err, terminal)
	}
	if strings.Contains(terminal.Error, root) || strings.Contains(terminal.Error, path) {
		t.Fatalf("terminal event leaked repository path: %#v", terminal)
	}
}

func TestProgressErrorMessageIsActionableBoundedAndPathFree(t *testing.T) {
	root := testtemp.Dir(t)
	nested := filepath.Join(root, "nested", "file.go")
	message := indexer.ProgressErrorMessage(errors.New("open "+nested+": permission denied\n"+strings.Repeat("界", 600)), root)
	if strings.Contains(message, root) || strings.ContainsAny(message, "\r\n") {
		t.Fatalf("message leaked a path or line break: %q", message)
	}
	if !strings.Contains(message, "<repository>/nested/file.go: permission denied") || len([]rune(message)) > 512 || !strings.HasSuffix(message, "…") {
		t.Fatalf("message lost remediation or bound: %q", message)
	}
}

func TestProgressErrorMessageDoesNotReplaceShortRelativeProseOrItsPlaceholder(t *testing.T) {
	for _, relative := range []string{"e", "it"} {
		absolute, err := filepath.Abs(relative)
		if err != nil {
			t.Fatal(err)
		}
		message := indexer.ProgressErrorMessage(errors.New("resolve "+relative+" failed at "+absolute), relative)
		want := "resolve " + relative + " failed at <repository>"
		if message != want {
			t.Fatalf("relative %q: message=%q want=%q", relative, message, want)
		}
	}
}

func TestServiceReportCapturesPendingReconciliationAtStart(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "sample.progress"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(progressParser{})
	stop := errors.New("stop after durable file")
	_, err = indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{
		Boundary: func(boundary indexer.Boundary) error {
			if boundary.Kind == indexer.BoundaryFilePersisted {
				return stop
			}
			return nil
		},
	})
	if !errors.Is(err, stop) {
		t.Fatalf("boundary error = %v", err)
	}
	report, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReconciliationPendingAtStart {
		t.Fatalf("report did not retain pending reconciliation provenance: %#v", report)
	}
}
