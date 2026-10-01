package indexer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	protobufparser "github.com/cafecito-games/grafo/internal/parser/protobuf"
	pythonparser "github.com/cafecito-games/grafo/internal/parser/python"
	sqlparser "github.com/cafecito-games/grafo/internal/parser/sql"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// recordingRepository captures the exact durable call sequence an indexing run
// produces. It replaces SQLite in determinism tests so a comparison cannot be
// weakened by storage-side ordering or by per-run timestamps.
type recordingRepository struct {
	mu      sync.Mutex
	meta    map[string]string
	files   map[string]graph.FileRecord
	entries []repositoryCall
}

type repositoryCall struct {
	Call     string             `json:"call"`
	Path     string             `json:"path,omitempty"`
	Owner    string             `json:"owner,omitempty"`
	Hash     string             `json:"hash,omitempty"`
	Language string             `json:"language,omitempty"`
	Size     int64              `json:"size,omitempty"`
	Paths    []string           `json:"paths,omitempty"`
	Nodes    []string           `json:"nodes,omitempty"`
	Facts    []string           `json:"facts,omitempty"`
	Messages []graph.Diagnostic `json:"messages,omitempty"`
}

func newRecordingRepository() *recordingRepository {
	return &recordingRepository{meta: map[string]string{}, files: map[string]graph.FileRecord{}}
}

func (r *recordingRepository) Meta(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta[key], nil
}

func (r *recordingRepository) SetMeta(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta[key] = value
	return nil
}

func (r *recordingRepository) Files(context.Context) (map[string]graph.FileRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]graph.FileRecord, len(r.files))
	for path, record := range r.files {
		result[path] = record
	}
	return result, nil
}

func (r *recordingRepository) ReplaceFile(_ context.Context, record graph.FileRecord, parsed graph.ParseResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[record.Path] = record
	r.entries = append(r.entries, repositoryCall{Call: "replace_file", Path: record.Path, Hash: record.Hash,
		Language: record.Language, Size: record.Size, Nodes: nodeIdentities(parsed), Facts: factIdentities(parsed),
		Messages: parsed.Diagnostics})
	return nil
}

func (r *recordingRepository) ReplaceOwner(_ context.Context, owner string, parsed graph.ParseResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, repositoryCall{Call: "replace_owner", Owner: owner,
		Nodes: nodeIdentities(parsed), Facts: factIdentities(parsed), Messages: parsed.Diagnostics})
	return nil
}

func (r *recordingRepository) RemoveFiles(_ context.Context, paths []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, path := range paths {
		delete(r.files, path)
	}
	r.entries = append(r.entries, repositoryCall{Call: "remove_files", Paths: paths})
	return nil
}

func (r *recordingRepository) Reconcile(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, repositoryCall{Call: "reconcile"})
	return nil
}

func (r *recordingRepository) Counts(context.Context) (graph.Counts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return graph.Counts{Files: len(r.files)}, nil
}

func (r *recordingRepository) calls() []repositoryCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]repositoryCall(nil), r.entries...)
}

func nodeIdentities(parsed graph.ParseResult) []string {
	result := make([]string, 0, len(parsed.Nodes))
	for _, node := range parsed.Nodes {
		result = append(result, fmt.Sprintf("%s|%s|%s|%s:%d:%d", node.ID, node.Kind, node.QualifiedName,
			node.Location.Path, node.Location.Line, node.Location.Column))
	}
	return result
}

func factIdentities(parsed graph.ParseResult) []string {
	result := make([]string, 0, len(parsed.Facts))
	for _, fact := range parsed.Facts {
		result = append(result, fmt.Sprintf("%s|%s|%s|%s|%s|%s:%d:%d", fact.ID, fact.FromID, fact.Kind,
			fact.TargetID, fact.Producer, fact.Location.Path, fact.Location.Line, fact.Location.Column))
	}
	return result
}

// pipelineObservation is everything about an indexing session that the issue
// requires to stay identical across worker counts. Two runs are observed so the
// comparison covers a cold pass and the incremental pass that follows it, where
// every file resolves as unchanged.
type pipelineObservation struct {
	Runs  []pipelineRun    `json:"runs"`
	Calls []repositoryCall `json:"calls"`
}

type pipelineRun struct {
	Updated     []string           `json:"updated"`
	Unchanged   int                `json:"unchanged"`
	Removed     []string           `json:"removed"`
	Skipped     []string           `json:"skipped"`
	Checked     int                `json:"checked"`
	ScopedOut   int                `json:"scoped_out"`
	Diagnostics []graph.Diagnostic `json:"diagnostics"`
	Boundaries  []indexer.Boundary `json:"boundaries"`
	Progress    []string           `json:"progress"`
}

func writePipelineCorpus(t testing.TB, root string) {
	t.Helper()
	write(t, filepath.Join(root, "go.mod"), "module example.com/corpus\n\ngo 1.26\n")
	write(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() { Helper(); Other() }\n")
	write(t, filepath.Join(root, "helper.go"), "package main\n\n// Helper does work.\nfunc Helper() {}\n")
	write(t, filepath.Join(root, "other.go"), "package main\n\nfunc Other() { Helper() }\n")
	write(t, filepath.Join(root, "worker.py"), "def run():\n    return helper()\n\n\ndef helper():\n    return 1\n")
	write(t, filepath.Join(root, "queue.py"), "class Queue:\n    def push(self, value):\n        return value\n")
	write(t, filepath.Join(root, "client.ts"), "import { shape } from './shape'\nexport function call() { return shape() }\n")
	write(t, filepath.Join(root, "shape.ts"), "export function shape() { return 1 }\n")
	write(t, filepath.Join(root, "barrel.ts"), "export * from './shape'\n")
	write(t, filepath.Join(root, "project.godot"), "config_version=5\n\n[application]\n\nconfig/name=\"corpus\"\n")
	write(t, filepath.Join(root, "player.gd"), "class_name Player\n\nfunc run() -> void:\n\tstep()\n\nfunc step() -> void:\n\tpass\n")
	write(t, filepath.Join(root, "enemy.gd"), "class_name Enemy\n\nfunc tick() -> void:\n\tpass\n")
	write(t, filepath.Join(root, "world.tscn"), "[gd_scene format=3]\n\n[node name=\"World\" type=\"Node2D\"]\n")
	write(t, filepath.Join(root, "schema.sql"), "CREATE TABLE events (id bigint PRIMARY KEY, name text);\n")
	write(t, filepath.Join(root, "reports.sql"), "CREATE VIEW recent AS SELECT id FROM events;\n")
	write(t, filepath.Join(root, "service.proto"), "syntax = \"proto3\";\npackage corpus;\nmessage Ping { string id = 1; }\n")
	write(t, filepath.Join(root, "README.md"), "# Corpus\n\n## Usage\n\nSee `main.go`.\n")
	write(t, filepath.Join(root, ".env"), "API_URL=http://localhost:8080\n")
	// Exceeds pipelineMaxFileSize so the size screen is part of the comparison.
	write(t, filepath.Join(root, "huge.py"), "def wide():\n    return \""+strings.Repeat("x", 2*pipelineMaxFileSize)+"\"\n")
}

// pipelineMaxFileSize keeps the oversized fixture small while still exercising
// the size screen and its report slot.
const pipelineMaxFileSize = 32 << 10

func pipelineRegistry() *parserapi.Registry {
	return parserapi.NewRegistry(gdscriptparser.New(), godotparser.New(), golangparser.New(),
		pythonparser.New(), typescriptparser.New(), sqlparser.New(postgresparser.New()),
		protobufparser.New(), markdownparser.New(), configparser.New())
}

func runPipelineObservation(t *testing.T, workers int) pipelineObservation {
	t.Helper()
	ctx := context.Background()
	root := testtemp.Dir(t)
	writePipelineCorpus(t, root)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	// The temporary root differs per run, so neutralize the only identity that
	// leaks it into node and fact IDs.
	project.ID = "repo:pipeline"
	project.Name = "corpus"
	repository := newRecordingRepository()
	observation := pipelineObservation{}
	// Both passes share one repository and one parser registry, which is how the
	// CLI reuses them, so the incremental pass sees the first pass's hashes.
	service := indexer.NewService(repository, pipelineRegistry())
	for pass := range 2 {
		run := pipelineRun{}
		options := indexer.Options{
			ParseWorkers: workers,
			MaxFileSize:  pipelineMaxFileSize,
			Boundary: func(boundary indexer.Boundary) error {
				run.Boundaries = append(run.Boundaries, boundary)
				return nil
			},
			ProgressObserver: func(event indexer.ProgressEvent) error {
				if event.Phase == indexer.ProgressReadHash || event.Phase == indexer.ProgressParse ||
					event.Phase == indexer.ProgressPersistence {
					run.Progress = append(run.Progress,
						fmt.Sprintf("%s/%s/%d", event.Phase, event.State, event.Completed))
				}
				return nil
			},
		}
		report, err := service.Run(ctx, project, options)
		if err != nil {
			t.Fatalf("workers=%d pass=%d: %v", workers, pass, err)
		}
		run.Updated = report.Updated
		run.Unchanged = report.Unchanged
		run.Removed = report.Removed
		run.Skipped = report.Skipped
		run.Checked = report.Checked
		run.ScopedOut = report.ScopedOut
		run.Diagnostics = report.Diagnostics
		observation.Runs = append(observation.Runs, run)
	}
	observation.Calls = repository.calls()
	// A thin corpus or a pass that silently did nothing would let a reordering
	// slip through, so pin what each pass must have established.
	if len(observation.Runs[0].Updated) < 15 {
		t.Fatalf("workers=%d cold pass indexed only %v", workers, observation.Runs[0].Updated)
	}
	if !slices.Contains(observation.Runs[0].Skipped, "huge.py") {
		t.Fatalf("workers=%d never screened the oversized file: %v", workers, observation.Runs[0].Skipped)
	}
	if len(observation.Runs[1].Updated) != 0 || observation.Runs[1].Unchanged < 15 {
		t.Fatalf("workers=%d incremental pass was not a no-op: updated=%v unchanged=%d",
			workers, observation.Runs[1].Updated, observation.Runs[1].Unchanged)
	}
	// Unchanged also counts paths an incremental pass excludes before any file
	// access, so require the second pass to have read and hashed the corpus.
	// Otherwise a change that let it skip the stage would weaken this comparison
	// to a cold pass without failing it.
	if observation.Runs[1].Checked < 15 {
		t.Fatalf("workers=%d incremental pass skipped the read/hash stage: checked=%d",
			workers, observation.Runs[1].Checked)
	}
	return observation
}

// runSelectedPassObservation drives a git-managed corpus so the second pass
// takes the incremental path, where change detection excludes most paths before
// any filesystem access. That branch is applied on the writer and decides
// membership, so it has to keep its order across worker counts like every other
// outcome. The non-git harness above cannot reach it, because change detection
// needs a commit to compare against.
func runSelectedPassObservation(t *testing.T, workers int) pipelineObservation {
	t.Helper()
	ctx := context.Background()
	root := testtemp.Dir(t)
	runGit(t, root, "init", "-b", "main")
	writePipelineCorpus(t, root)
	runGit(t, root, "add", "-A")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid",
		"commit", "-m", "fixture")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	project.ID = "repo:pipeline"
	project.Name = "corpus"
	repository := newRecordingRepository()
	service := indexer.NewService(repository, pipelineRegistry())
	observation := pipelineObservation{}
	for pass := range 2 {
		if pass == 1 {
			write(t, filepath.Join(root, "helper.go"), "package main\n\n// Helper does more work.\nfunc Helper() { Other() }\n")
		}
		run := pipelineRun{}
		options := indexer.Options{ParseWorkers: workers, MaxFileSize: pipelineMaxFileSize,
			ProgressObserver: func(event indexer.ProgressEvent) error {
				if event.Phase == indexer.ProgressReadHash || event.Phase == indexer.ProgressParse ||
					event.Phase == indexer.ProgressPersistence {
					run.Progress = append(run.Progress,
						fmt.Sprintf("%s/%s/%d", event.Phase, event.State, event.Completed))
				}
				return nil
			}}
		report, err := service.Run(ctx, project, options)
		if err != nil {
			t.Fatalf("workers=%d pass=%d: %v", workers, pass, err)
		}
		run.Updated = report.Updated
		run.Unchanged = report.Unchanged
		run.Removed = report.Removed
		run.Skipped = report.Skipped
		run.Checked = report.Checked
		run.ScopedOut = report.ScopedOut
		run.Diagnostics = report.Diagnostics
		observation.Runs = append(observation.Runs, run)
	}
	observation.Calls = repository.calls()
	// The second pass must really have taken the incremental path: it persisted
	// only the edited file, and it excluded the rest before reading them, which
	// is what leaves Checked far below the corpus size.
	second := observation.Runs[1]
	// The edited file and its Go semantic dependents are reindexed; everything
	// else must have been excluded before being read.
	if !slices.Contains(second.Updated, "helper.go") {
		t.Fatalf("workers=%d selected pass did not reindex the edited file: %v", workers, second.Updated)
	}
	if len(second.Updated) > 6 {
		t.Fatalf("workers=%d selected pass reindexed too much to be incremental: %v", workers, second.Updated)
	}
	if second.Checked >= 15 {
		t.Fatalf("workers=%d selected pass read the whole corpus (checked=%d), so no path was excluded",
			workers, second.Checked)
	}
	if second.Unchanged < 10 {
		t.Fatalf("workers=%d selected pass excluded only %d paths", workers, second.Unchanged)
	}
	return observation
}

func TestServiceParseWorkerCountsProduceIdenticalSelectedPasses(t *testing.T) {
	var baseline []byte
	for _, workers := range []int{1, 2, runtime.NumCPU() + 2} {
		encoded, err := json.Marshal(runSelectedPassObservation(t, workers))
		if err != nil {
			t.Fatal(err)
		}
		if baseline == nil {
			baseline = encoded
			continue
		}
		if string(encoded) != string(baseline) {
			t.Fatalf("workers=%d diverged from the sequential incremental run:\nsequential=%s\nconcurrent=%s",
				workers, baseline, encoded)
		}
	}
}

// vanishingTransform removes the file it is handed after that file has been read
// and parsed, which leaves the writer's pre-persist existence check as the only
// thing that can keep it out of the graph. Deleting the path being transformed
// rather than a later one makes that true at every worker count.
type vanishingTransform struct {
	root string
	path string
}

func (vanishingTransform) SemanticKey() string { return "vanishing-transform-v1" }

func (v vanishingTransform) Transform(_ context.Context, input parserapi.Input, parsed graph.ParseResult) (graph.ParseResult, error) {
	if input.Path == v.path {
		if err := os.Remove(filepath.Join(v.root, filepath.FromSlash(v.path))); err != nil {
			return graph.ParseResult{}, err
		}
	}
	return parsed, nil
}

func TestServiceDropsFileThatVanishesAfterParsing(t *testing.T) {
	for _, workers := range []int{1, 4} {
		ctx := context.Background()
		root := testtemp.Dir(t)
		writePipelineCorpus(t, root)
		project, err := indexer.DiscoverProject(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		repository := newRecordingRepository()
		report, err := indexer.NewService(repository, pipelineRegistry()).Run(ctx, project,
			indexer.Options{ParseWorkers: workers, MaxFileSize: pipelineMaxFileSize,
				ResultTransform: vanishingTransform{root: root, path: "worker.py"}})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(report.Updated, "worker.py") {
			t.Fatalf("workers=%d: persisted a file that was deleted after parsing", workers)
		}
		for _, call := range repository.calls() {
			if call.Call == "replace_file" && call.Path == "worker.py" {
				t.Fatalf("workers=%d: the vanished file reached persistence", workers)
			}
		}
		reported := false
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.Path == "worker.py" && diagnostic.Level == "warning" {
				reported = true
			}
		}
		if !reported {
			t.Fatalf("workers=%d: the vanished file produced no diagnostic: %v", workers, report.Diagnostics)
		}
	}
}

func TestServiceParseWorkerCountsProduceIdenticalRuns(t *testing.T) {
	counts := []int{1, 2, runtime.NumCPU() + 2}
	var baseline []byte
	for _, workers := range counts {
		observation := runPipelineObservation(t, workers)
		encoded, err := json.Marshal(observation)
		if err != nil {
			t.Fatal(err)
		}
		if baseline == nil {
			baseline = encoded
			continue
		}
		if string(encoded) != string(baseline) {
			t.Fatalf("workers=%d diverged from the sequential run:\nsequential=%s\nconcurrent=%s",
				workers, baseline, encoded)
		}
	}
}

func TestServiceParseWorkerCountDefaultsToBoundedPool(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("the derived worker count is one on a single-core machine, so parses cannot overlap")
	}
	ctx := context.Background()
	root := testtemp.Dir(t)
	writePipelineCorpus(t, root)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	project.ID = "repo:pipeline"
	repository := newRecordingRepository()
	var observed int
	registry := parserapi.NewRegistry(newCountingParser(&observed), configparser.New())
	if _, err := indexer.NewService(repository, registry).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if observed < 2 {
		t.Fatalf("default worker count never overlapped parses: peak concurrency %d", observed)
	}
	if observed > runtime.NumCPU() {
		t.Fatalf("default worker count exceeded the available cores: peak concurrency %d", observed)
	}
}

// countingParser blocks until a second parse joins it, which proves that the
// default configuration actually overlaps parses, then records the peak.
type countingParser struct {
	mu          sync.Mutex
	active      int
	peak        *int
	release     chan struct{}
	releaseOnce sync.Once
}

func newCountingParser(peak *int) *countingParser {
	return &countingParser{peak: peak, release: make(chan struct{})}
}

func (*countingParser) Language() string { return "go" }
func (*countingParser) Supports(path string) bool {
	return strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".py") ||
		strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".gd") ||
		strings.HasSuffix(path, ".sql") || strings.HasSuffix(path, ".md")
}

func (p *countingParser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	p.mu.Lock()
	p.active++
	if p.active > *p.peak {
		*p.peak = p.active
	}
	reached := p.active >= 2
	p.mu.Unlock()
	if reached {
		p.releaseOnce.Do(func() { close(p.release) })
	} else {
		// The wait is bounded so a configuration that cannot overlap parses fails
		// the assertion instead of hanging until the package test timeout.
		select {
		case <-p.release:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return parserapi.NewBuilder(input, "go").Finish(), nil
}

// failingTransform fails one known path so the run's failure can be pinned to
// that path regardless of how far the read-ahead progressed.
type failingTransform struct{ path string }

func (failingTransform) SemanticKey() string { return "failing-transform-v1" }

func (f failingTransform) Transform(_ context.Context, input parserapi.Input, parsed graph.ParseResult) (graph.ParseResult, error) {
	if input.Path == f.path {
		return graph.ParseResult{}, errors.New("synthetic transform failure")
	}
	return parsed, nil
}

func TestServiceParseStagePropagatesTransformFailure(t *testing.T) {
	for _, workers := range []int{1, 4} {
		ctx := context.Background()
		root := testtemp.Dir(t)
		writePipelineCorpus(t, root)
		project, err := indexer.DiscoverProject(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		repository := newRecordingRepository()
		_, err = indexer.NewService(repository, pipelineRegistry()).Run(ctx, project,
			indexer.Options{ParseWorkers: workers, ResultTransform: failingTransform{path: "worker.py"}})
		if err == nil {
			t.Fatalf("workers=%d: expected the transform failure to fail the run", workers)
		}
		if !strings.Contains(err.Error(), "transform parsed evidence for worker.py") {
			t.Fatalf("workers=%d: unexpected error %v", workers, err)
		}
		for _, call := range repository.calls() {
			if call.Call == "replace_file" && call.Path > "worker.py" {
				t.Fatalf("workers=%d: persisted %s after the failing path", workers, call.Path)
			}
		}
	}
}

// cancelingParser cancels the run from inside the read/hash/parse stage, which
// is the only way to reach the stage's own abort path rather than a writer error.
type cancelingParser struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (*cancelingParser) Language() string { return "go" }
func (*cancelingParser) Supports(path string) bool {
	return strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".py") ||
		strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".gd") ||
		strings.HasSuffix(path, ".sql") || strings.HasSuffix(path, ".md")
}

func (p *cancelingParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return parserapi.NewBuilder(input, "go").Finish(), nil
}

// A repository that ignores context cancellation must not let a cancelled run
// report success, because the membership the stage collected is truncated and
// would drive removal and digest publication.
func TestServiceParseStageFailsCancelledRunWithoutRepositorySupport(t *testing.T) {
	for _, workers := range []int{1, 4} {
		ctx, cancel := context.WithCancel(context.Background())
		root := testtemp.Dir(t)
		writePipelineCorpus(t, root)
		project, err := indexer.DiscoverProject(ctx, root)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		repository := newRecordingRepository()
		registry := parserapi.NewRegistry(&cancelingParser{cancel: cancel}, configparser.New())
		_, err = indexer.NewService(repository, registry).Run(ctx, project,
			indexer.Options{ParseWorkers: workers})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("workers=%d: cancelled run reported %v", workers, err)
		}
		for _, call := range repository.calls() {
			if call.Call == "remove_files" {
				t.Fatalf("workers=%d: cancelled run removed files from truncated membership", workers)
			}
		}
	}
}

func TestServiceParseStageStopsOnCancellation(t *testing.T) {
	for _, workers := range []int{1, 4} {
		ctx, cancel := context.WithCancel(context.Background())
		root := testtemp.Dir(t)
		writePipelineCorpus(t, root)
		project, err := indexer.DiscoverProject(ctx, root)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		repository := newRecordingRepository()
		options := indexer.Options{ParseWorkers: workers, Boundary: func(indexer.Boundary) error {
			cancel()
			return context.Canceled
		}}
		_, err = indexer.NewService(repository, pipelineRegistry()).Run(ctx, project, options)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("workers=%d: expected cancellation, got %v", workers, err)
		}
		calls := repository.calls()
		persisted := 0
		for _, call := range calls {
			if call.Call == "replace_file" {
				persisted++
			}
		}
		if persisted != 1 {
			t.Fatalf("workers=%d: cancellation persisted %d files, want 1", workers, persisted)
		}
		// The pending-scope marker must survive so the next run rebuilds rather
		// than trusting a partially mutated catalog.
		pending, err := repository.Meta(ctx, "index_scope_pending")
		if err != nil {
			t.Fatal(err)
		}
		if pending == "" {
			t.Fatalf("workers=%d: cancellation left no pending scope marker", workers)
		}
	}
}

func TestServiceParseStageDropsFileRemovedBeforePersistence(t *testing.T) {
	for _, workers := range []int{1, 4} {
		ctx := context.Background()
		root := testtemp.Dir(t)
		if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, "app", "a.race"), "a")
		write(t, filepath.Join(root, "app", "b.race"), "b")
		project, err := indexer.DiscoverProject(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		repository := newRecordingRepository()
		report, err := indexer.NewService(repository, parserapi.NewRegistry(deletingParser{})).
			Run(ctx, project, indexer.Options{ParseWorkers: workers})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Updated) != 1 || report.Updated[0] != "app/a.race" {
			t.Fatalf("workers=%d: persisted a file lost before the writer reached it: %v", workers, report.Updated)
		}
	}
}
