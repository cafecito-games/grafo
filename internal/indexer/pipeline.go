package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

// maxParseWorkers caps the automatically derived worker count. Reading, hashing,
// and parsing stop scaling well before the core count of a large machine, and
// every additional worker also raises the number of parsed results held in
// memory ahead of the single writer. The cap matters most in service mode, where
// several repositories index at once and each one brings its own pool.
const maxParseWorkers = 8

// resolveParseWorkers selects how many goroutines read, hash, and parse files
// ahead of the ordered writer. A resolved count of one runs the stage inline on
// the writer goroutine, which is byte-for-byte the historical sequential path.
func resolveParseWorkers(requested int) int {
	if requested > 0 {
		return requested
	}
	workers := runtime.NumCPU()
	if workers > maxParseWorkers {
		workers = maxParseWorkers
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

// fileOutcomeKind classifies what the read/hash/parse stage established about
// one discovered path. The writer applies outcomes strictly in the original
// path order, which is what keeps report slices, progress counters, and
// boundary callbacks identical to the sequential pipeline.
type fileOutcomeKind int

const (
	// The zero value is reserved and deliberately unnamed. A closed hand-off slot
	// yields it, and the writer never acts on one because run's receive protocol
	// stops on the closed slot rather than passing its value on; the kind itself
	// carries no protection.
	_ fileOutcomeKind = iota
	// outcomeUnselected means an incremental pass excluded the path before any
	// filesystem access.
	outcomeUnselected
	// outcomeUnreadable means stat or read failed and the path contributes a
	// warning diagnostic.
	outcomeUnreadable
	// outcomeTooLarge means the file exceeds the configured maximum size.
	outcomeTooLarge
	// outcomeUnsupported means the file was read but no parser claims it.
	outcomeUnsupported
	// outcomeUnchanged means the content hash matches the indexed hash.
	outcomeUnchanged
	// outcomeParsed means the file parsed and is ready to persist.
	outcomeParsed
	// outcomeFailed means this path failed the run as a whole.
	outcomeFailed
)

// fileOutcome carries everything the writer needs for one path. Only the fields
// meaningful for its kind are populated.
type fileOutcome struct {
	path       string
	kind       fileOutcomeKind
	diagnostic graph.Diagnostic
	err        error
	readHashNS int64
	parseNS    int64
	record     graph.FileRecord
	parsed     graph.ParseResult
}

// fileStage holds the immutable run inputs that the read/hash/parse stage
// consults. Every field is read-only for the lifetime of the stage so workers
// can share it without synchronization; the writer owns all mutable run state.
type fileStage struct {
	project               Project
	options               Options
	parsers               *parserapi.Registry
	paths                 []string
	selected              map[string]bool
	known                 map[string]graph.FileRecord
	workspaceSemanticKeys map[string]string
}

// prepare performs every step of the per-file pipeline that has no durable side
// effect: membership selection, stat, size screening, read, cache-key hashing,
// parsing, the optional result transform, and producer stamping. It never
// touches the repository, the report, or the progress emitter.
func (stage *fileStage) prepare(ctx context.Context, path string) fileOutcome {
	if stage.selected != nil && !stage.selected[path] {
		return fileOutcome{path: path, kind: outcomeUnselected}
	}
	absolute := filepath.Join(stage.project.Root, filepath.FromSlash(path))
	readStarted := time.Now()
	info, err := os.Stat(absolute)
	if err != nil {
		return fileOutcome{path: path, kind: outcomeUnreadable,
			diagnostic: graph.Diagnostic{Path: path, Level: "warning", Message: err.Error()}}
	}
	if info.Size() > stage.options.MaxFileSize {
		return fileOutcome{path: path, kind: outcomeTooLarge, readHashNS: time.Since(readStarted).Nanoseconds()}
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return fileOutcome{path: path, kind: outcomeUnreadable,
			diagnostic: graph.Diagnostic{Path: path, Level: "warning", Message: err.Error()}}
	}
	languageParser, ok := stage.parsers.For(path)
	if !ok {
		return fileOutcome{path: path, kind: outcomeUnsupported}
	}
	input := parserapi.Input{Root: stage.project.Root, Path: path, Content: content,
		SourcePaths: stage.paths, Repository: stage.project.Name, RepoID: stage.project.ID, GoModule: stage.project.GoModule}
	digest := sha256.New()
	_, _ = digest.Write(content)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(SemanticIndexVersion))
	if stage.options.ResultTransform != nil {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(stage.options.ResultTransform.SemanticKey()))
	}
	if _, ok := languageParser.(parserapi.WorkspaceSemanticKeyer); ok {
		semanticKey := stage.workspaceSemanticKeys[languageParser.Language()]
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(semanticKey))
		input.SemanticKey = semanticKey
	}
	// A workspace keyer may also refine its shared key per file. The
	// refinement enters the content hash only: Input.SemanticKey stays the
	// repository-wide key so parser-side workspace caches keep one entry.
	if keyer, ok := languageParser.(parserapi.SemanticKeyer); ok {
		semanticKey, keyErr := keyer.SemanticKey(ctx, input)
		if keyErr != nil {
			return fileOutcome{path: path, kind: outcomeFailed,
				err: fmt.Errorf("load parser configuration for %s: %w", path, keyErr)}
		}
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(semanticKey))
		if input.SemanticKey == "" {
			input.SemanticKey = semanticKey
		}
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	readHashNS := time.Since(readStarted).Nanoseconds()
	if previous, exists := stage.known[path]; exists && previous.Hash == hash && !stage.options.Force {
		return fileOutcome{path: path, kind: outcomeUnchanged, readHashNS: readHashNS}
	}
	parseStarted := time.Now()
	parsed, parseErr := languageParser.Parse(ctx, input)
	parseNS := time.Since(parseStarted).Nanoseconds()
	if parseErr != nil {
		parsed.Diagnostics = append(parsed.Diagnostics, graph.Diagnostic{Path: path, Level: "error", Message: parseErr.Error()})
	}
	if stage.options.ResultTransform != nil {
		transformStarted := time.Now()
		transformed, transformErr := stage.options.ResultTransform.Transform(ctx, input, parsed)
		parseNS += time.Since(transformStarted).Nanoseconds()
		if transformErr != nil {
			return fileOutcome{path: path, kind: outcomeFailed, readHashNS: readHashNS, parseNS: parseNS,
				err: fmt.Errorf("transform parsed evidence for %s: %w", path, transformErr)}
		}
		parsed = transformed
	}
	// The selected parser is the producer authority. Parser-returned facts are
	// otherwise untrusted and cannot claim another extractor's identity.
	for index := range parsed.Facts {
		parsed.Facts[index].Producer = languageParser.Language()
	}
	fileID := graph.NodeID(graph.KindFile, stage.project.ID+":"+path)
	parsed.Facts = append(parsed.Facts, graph.Fact{
		ID:     graph.FactID(path, stage.project.ID, graph.EdgeContains, fileID, 1, 0),
		FromID: stage.project.ID, Kind: graph.EdgeContains, Producer: graph.ProducerIndexer, TargetID: fileID,
		Location: graph.Location{Path: path, Line: 1, Column: 1}, OwnerFile: path,
	})
	return fileOutcome{path: path, kind: outcomeParsed, readHashNS: readHashNS, parseNS: parseNS,
		record: graph.FileRecord{Path: path, Hash: hash, Language: languageParser.Language(),
			Size: info.Size(), ModifiedNS: info.ModTime().UnixNano()},
		parsed: parsed,
	}
}

// run drives the stage over every path and hands each outcome to apply in the
// original path order. With more than one worker the preparation runs on a fixed
// pool while a dispatcher reserves one ordered hand-off slot per path; the slot
// channel's capacity is what bounds how many parsed results exist at once. The
// first error from apply stops dispatching and is returned unchanged, so the run
// fails at the same path it would have failed at sequentially.
func (stage *fileStage) run(ctx context.Context, workers int, apply func(fileOutcome) error) error {
	if workers <= 1 {
		for _, path := range stage.paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := apply(stage.prepare(ctx, path)); err != nil {
				return err
			}
		}
		return nil
	}
	stageCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type preparation struct {
		path string
		slot chan fileOutcome
	}
	jobs := make(chan preparation)
	ordered := make(chan chan fileOutcome, workers)
	var running sync.WaitGroup
	for range workers {
		running.Add(1)
		go func() {
			defer running.Done()
			for job := range jobs {
				// The slot is buffered, so a worker never waits on the writer.
				job.slot <- stage.prepare(stageCtx, job.path)
			}
		}()
	}
	go func() {
		defer close(jobs)
		defer close(ordered)
		for _, path := range stage.paths {
			slot := make(chan fileOutcome, 1)
			select {
			case <-stageCtx.Done():
				return
			case ordered <- slot:
			}
			select {
			case <-stageCtx.Done():
				// Closing the reserved slot tells the writer that no worker
				// will ever fill it, instead of leaving it blocked forever.
				close(slot)
				return
			case jobs <- preparation{path: path, slot: slot}:
			}
		}
	}()
	var applyErr error
	handed := 0
	for slot := range ordered {
		outcome, ok := <-slot
		if !ok {
			break
		}
		handed++
		if applyErr != nil {
			continue
		}
		if err := apply(outcome); err != nil {
			applyErr = err
			cancel()
		}
	}
	cancel()
	for slot := range ordered {
		<-slot
	}
	running.Wait()
	if applyErr != nil {
		return applyErr
	}
	if handed != len(stage.paths) {
		// The dispatcher stopped early, which happens only when the caller's
		// context was cancelled. Reporting it fails the run instead of letting
		// truncated membership reach removal and digest publication.
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	return nil
}
