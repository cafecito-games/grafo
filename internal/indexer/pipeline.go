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

// maxParseWorkers caps the automatically derived worker count.
//
// A cold index of cafecito-games/uzir at 638159ead (11,696 indexed files) on a
// 12-core machine, one run per worker count:
//
//	workers   wall    aggregate parse   peak RSS
//	      4   394.7s            169.3s   5.34 GiB
//	      8   314.5s            202.4s   6.52 GiB
//	     12   320.7s            249.8s   6.47 GiB
//	     16   330.5s            359.2s   6.57 GiB
//
// Read those against the run-to-run spread, which is large: three runs at eight
// workers on one build gave 308.9s, 326.7s, and 338.2s, a standard deviation of
// 14.8s, their aggregate parse figures a standard deviation of 18.1s. Only gaps
// well outside that band carry information. Four workers are genuinely too few.
// Eight, twelve, and sixteen are indistinguishable in wall clock, and the
// aggregate parse growth by sixteen is the one effect clearly outside the
// noise.
//
// So eight stays as the conservative choice rather than as a demonstrated
// optimum: nothing here shows that raising it buys wall clock, while the CPU
// summed across workers and the peak memory both rise. Why they rise is not
// settled. Lock contention would do it, and so would the extra workers merely
// queueing deeper ahead of the single ordered writer; these numbers cannot
// separate the two, and a worker count chosen on more than one run per point
// would need to.
//
// Every additional worker also raises the number of parsed results held in
// memory ahead of that writer, on top of the grouped commits in service.go,
// which is why worker count and group size belong together.
//
// The cap matters most in service mode, where several repositories index at
// once and each one brings its own pool: this bounds one pool, not their sum.
// Their sum is bounded by processParseBudget below, which is this same number
// applied once per process, so raising this constant raises both bounds and the
// rationale recorded there is part of this one.
const maxParseWorkers = 8

// ResolveParseWorkers selects how many goroutines read, hash, and parse files
// ahead of the ordered writer. A resolved count of one runs the stage inline on
// the writer goroutine, which produces byte-for-byte what the historical
// sequential path produced; it is metered by the process-wide ParseBudget like
// any other pool, which changes when its preparations run and never what they
// yield. It is exported so a caller that reports the count it ran under derives
// it here rather than reimplementing the derivation.
func ResolveParseWorkers(requested int) int {
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

// ParseBudget bounds how many files are being read, hashed, and parsed at the
// same time, summed across every parse pool sharing the budget. One budget per
// process is what composes the pools of concurrently indexing roots into a
// single ceiling.
type ParseBudget struct {
	// A nil channel admits every caller, so the unbounded budget needs no
	// special case at the acquisition site.
	permits chan struct{}
}

// UnboundedParseBudget applies no bound at all. A caller measuring one pool in
// isolation, such as the benchmark harness that produced the table above, uses
// it so a process-wide ceiling cannot silently cap the pool size it asked for.
var UnboundedParseBudget = &ParseBudget{}

// NewParseBudget returns a budget admitting limit preparations at once. A limit
// below one has no bound to enforce and yields UnboundedParseBudget.
func NewParseBudget(limit int) *ParseBudget {
	if limit < 1 {
		return UnboundedParseBudget
	}
	return &ParseBudget{permits: make(chan struct{}, limit)}
}

// Limit reports how many preparations the budget admits at once. Zero means
// unbounded.
func (budget *ParseBudget) Limit() int {
	if budget == nil {
		return 0
	}
	return cap(budget.permits)
}

// processParseBudget is the budget every run shares unless its Options name
// another one.
//
// Why it is exactly ResolveParseWorkers(0), the same number as a single pool's
// derived size: the per-pool cap above was measured as the point where a cold
// index of one repository stops gaining wall clock while its summed CPU and its
// peak memory keep climbing. That measurement is a statement about the machine,
// not about one repository, so the same number is the right ceiling for the
// machine as a whole. Applying it once per process instead of once per pool is
// the whole of this policy: one root indexing alone still resolves a pool of
// min(NumCPU, maxParseWorkers) workers and holds that many permits, so it is
// bounded by exactly what bounded it before; N roots indexing at once share the
// same permits instead of each claiming a full pool's worth.
//
// The two bounds therefore compose by construction rather than by coincidence,
// and raising maxParseWorkers raises both. They are deliberately the same
// constant: a process-wide number smaller than the per-pool cap would slow a
// lone root, and a larger one would admit the oversubscription this exists to
// prevent.
//
// Permits are held only across one file's preparation, never across the ordered
// writer, so the budget throttles work rather than resizing pools. That is what
// keeps a lone root unaffected: with as many permits as workers, no worker ever
// waits. It is also the limit of what this bounds. Each pool still runs its own
// goroutines and still reserves one hand-off slot per worker, so the parsed
// results held in memory ahead of the writers remain proportional to root count
// rather than to the permits. Bounding that, together with the per-writer page
// cache and temp_store, is issue #182; this budget bounds concurrent parse work
// and nothing else.
//
// The ceiling is also per process, because that is where the oversubscription
// it was reported for lives: service mode indexes every root from one
// supervisor. Two separate `grafo index` invocations still each get their own,
// which is the same bound a user running two builds at once would get, and
// bounding across processes would need shared state no indexing run has.
//
// Measured on a 12-core machine (6 performance cores) with two roots of 7,625
// and 7,546 indexed files indexed cold and concurrently by one `grafo service
// run --once` pass, three runs per configuration, interleaved so drift could
// not land on one of them:
//
//	                      parse workers   root A   root B   peak RSS
//	one pool per root                16   334.5s   331.2s   6.10 GiB
//	one process-wide budget           8   336.2s   336.9s   6.16 GiB
//
// Read those against the run-to-run spread, as with the per-pool table above.
// Per-root standard deviation was 8.5s and 10.2s with a pool each, 23.9s and
// 22.1s under the budget, and peak RSS varied by 0.20 GiB and 0.39 GiB. Halving
// the parse workers that actually run at once therefore costs nothing
// measurable: the ~2s of wall clock and 0.06 GiB separating the two
// configurations sit an order of magnitude inside that spread.
//
// It also buys nothing measurable at two roots, which is the honest reading:
// the oversubscription is not what made these two roots slow, and this is a
// bound rather than a demonstrated speedup. What the bound is worth grows with
// root count, which is exactly what was not measured here. The supervisor's
// default concurrency of four roots would have run 4 x 8 = 32 parse workers on
// this machine and now runs eight, and four roots were not measured.
//
// A lone root was measured the same way, two runs per configuration, for the
// criterion that the budget must not slow it down:
//
//	                      parse workers     wall   peak RSS
//	one pool per root                 8   259.7s   5.05 GiB
//	one process-wide budget           8   258.6s   4.99 GiB
//
// Standard deviations 0.6s and 9.3s. There is nothing for a lone root to wait
// on, because it resolves the pool it always resolved and the budget holds a
// permit for every worker in it;
// TestParseBudgetLeavesALoneRootAtItsFullPoolSize asserts that directly rather
// than leaving it to two runs of a noisy measurement.
var processParseBudget = NewParseBudget(ResolveParseWorkers(0))

// ProcessParseBudget returns the process-wide budget that a run uses when its
// Options do not name another. It is exported so a caller that reports the
// ceiling it ran under reads it here rather than rederiving it.
func ProcessParseBudget() *ParseBudget { return processParseBudget }

// resolveParseBudget picks the budget a run is metered by. Nil Options select
// the process-wide one, which is what every production caller does; naming a
// budget is for measurement harnesses and tests.
func resolveParseBudget(requested *ParseBudget) *ParseBudget {
	if requested != nil {
		return requested
	}
	return processParseBudget
}

// acquire blocks until the budget admits one file preparation and returns the
// release for it. A cancelled context stops the wait and returns an unmetered
// release: the stage still owes an outcome to every hand-off slot it reserved,
// so abandoning the protocol to honor the ceiling would hang the writer, and
// the preparations it still performs all observe the cancelled context anyway.
func (budget *ParseBudget) acquire(ctx context.Context) func() {
	if budget == nil || budget.permits == nil {
		return func() {}
	}
	select {
	case budget.permits <- struct{}{}:
		return func() { <-budget.permits }
	case <-ctx.Done():
		return func() {}
	}
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
	// outcomeEvidenceUnchanged means the file was selected and reparsed because
	// its inputs moved, and produced the evidence the index already holds. Its
	// file record still has to be written, or the stale input hash reselects it
	// on every later run, but its rows and its reconciliation fan-out are left
	// as they are.
	outcomeEvidenceUnchanged
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

// prepareWithin prepares one path while holding a permit from budget, so the
// preparation work of every pool in the process is metered by one ceiling.
func (stage *fileStage) prepareWithin(ctx context.Context, budget *ParseBudget, path string) fileOutcome {
	release := budget.acquire(ctx)
	defer release()
	return stage.prepare(ctx, path)
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
	// Deriving the scope-local part of the key can cost real filesystem work,
	// and the parser needs the same value again when it decides whether its
	// cached semantic view is still valid. Derive it once here and carry it, so
	// the refinement below and the parse that follows both read one value.
	if scopeKeyer, ok := languageParser.(parserapi.ScopeKeyer); ok {
		scopeKey, scopeErr := scopeKeyer.ScopeKey(ctx, input)
		if scopeErr != nil {
			return fileOutcome{path: path, kind: outcomeFailed,
				err: fmt.Errorf("derive parser scope key for %s: %w", path, scopeErr)}
		}
		input.ScopeKey = scopeKey
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
	// The digest is computed on the worker rather than on the writer: it is the
	// one piece of per-file work the elision decision needs, and the writer is
	// the stage's serial point.
	//
	// SemanticIndexVersion is part of the stored value, not just of the content
	// hash. A version bump exists to change how evidence is extracted or
	// resolved, and a file whose extraction is unaffected would otherwise digest
	// equal, elide its write, skip its dirty marks, and keep edges resolved
	// under the previous version's rules while the run stamped the new version
	// and reported the rebuild as done. Carrying the version here makes every
	// stored digest from another version a mismatch, so a bump rewrites
	// everything exactly as it did before elision existed.
	evidenceDigest := SemanticIndexVersion + "|" + graph.EvidenceDigest(parsed)
	kind := outcomeParsed
	// An empty recorded digest is what an index written before this existed
	// carries, and no digest can equal it, so such a file is written once more
	// and records its digest as it goes.
	if previous, exists := stage.known[path]; exists && previous.EvidenceDigest != "" &&
		previous.EvidenceDigest == evidenceDigest && !stage.options.Force {
		kind = outcomeEvidenceUnchanged
	}
	return fileOutcome{path: path, kind: kind, readHashNS: readHashNS, parseNS: parseNS,
		record: graph.FileRecord{Path: path, Hash: hash, Language: languageParser.Language(),
			Size: info.Size(), ModifiedNS: info.ModTime().UnixNano(), EvidenceDigest: evidenceDigest},
		parsed: parsed,
	}
}

// run drives the stage over every path and hands each outcome to apply in the
// original path order. With more than one worker the preparation runs on a fixed
// pool while a dispatcher reserves one ordered hand-off slot per path; the slot
// channel's capacity is what bounds how many parsed results exist at once. Every
// preparation, pooled or inline, holds a permit from the run's ParseBudget for
// its duration, which is what bounds the parse work of concurrently indexing
// roots by the machine rather than by their number. The first error from apply
// stops dispatching and is returned unchanged, so the run fails at the same path
// it would have failed at sequentially.
func (stage *fileStage) run(ctx context.Context, workers int, apply func(fileOutcome) error) error {
	budget := resolveParseBudget(stage.options.ParseBudget)
	if workers <= 1 {
		for _, path := range stage.paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := apply(stage.prepareWithin(ctx, budget, path)); err != nil {
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
				job.slot <- stage.prepareWithin(stageCtx, budget, job.path)
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
