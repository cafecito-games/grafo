package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexversion"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

const workspaceOwner = "__workspace__"
const workspaceSemanticKeysMeta = "parser_workspace_semantic_keys"
const workspaceStateDigestMeta = "workspace_state_digest"

// GitUntrackedPathsMeta names the stored ledger of untracked paths the index
// last covered. See GitDirtyPathsMeta for why both are cleared rather than
// carried when an index is adopted from another worktree.
const GitUntrackedPathsMeta = "git_untracked_paths"
const indexScopeDigestMeta = "index_scope_digest"
const indexScopePendingMeta = "index_scope_pending"
const indexScopeScopedOutMeta = "index_scope_scoped_out"
const SemanticIndexVersion = indexversion.Semantic

// maxGroupedFiles and maxGroupedRows bound one grouped persistence commit. A
// repository that can replace several files at once lets a cold run amortize the
// commit, the WAL header, and the prepared statement set over many files instead
// of paying for each one per file. The row bound keeps the memory held ahead of
// the commit proportional to evidence rather than to file count, so a handful of
// very large files flushes early.
const (
	maxGroupedFiles = 64
	maxGroupedRows  = 25_000
)

type Options struct {
	Force            bool
	MaxFileSize      int64
	Boundary         BoundaryHook
	ProgressObserver ProgressObserver
	ReportDetail     ReportDetail
	// ParseWorkers bounds the goroutines that read, hash, and parse files ahead
	// of the single ordered writer. Zero selects a CPU-derived default under an
	// explicit cap; one runs the stage inline on the writer goroutine and
	// reproduces the sequential pipeline exactly. A positive value is honored as
	// given, so a caller exposing this must bound its own input: in-flight parsed
	// results scale with the worker count. Persistence is single-writer and
	// path-ordered at every worker count, so the indexed graph never depends on
	// this value.
	ParseWorkers int
	// Seed records that this index was adopted from another worktree immediately
	// before the run, so the report can explain where its starting facts came
	// from. It is provenance only: the run's behaviour never depends on it,
	// because an adopted index proves itself through the same content hashing as
	// any other, and the adopting caller clears the dirty ledgers so no file is
	// reused without being read.
	Seed *SeedProvenance
	// ResultTransform is a benchmark/test-only interception point after parsing
	// and before any durable mutation. Production CLI, MCP, watch, and service
	// composition leave it nil. Its semantic key participates in each file hash
	// so switching a prototype profile reparses instead of reusing stale detail.
	ResultTransform ParseResultTransform
}

// SeedProvenance describes the index a run started from when that index was
// copied out of a sibling worktree rather than built here. It is reported so an
// unexpectedly fast first index is explainable, and so the donor can be named
// when its facts turn out to be wrong.
type SeedProvenance struct {
	// DonorRoot is the worktree whose index was copied.
	DonorRoot string `json:"donor_root"`
	// DonorBranch is the branch that index described.
	DonorBranch string `json:"donor_branch,omitempty"`
	// DonorCommit is the commit that index was last written at.
	DonorCommit string `json:"donor_commit,omitempty"`
	// ChangedPaths counts the tracked files differing between the donor's commit
	// and this worktree's, which is how the donor was chosen. It is not the work
	// the run will do: the adopting pass re-reads every file regardless.
	ChangedPaths int `json:"changed_paths"`
}

// ParseResultTransform supports isolated fidelity experiments without adding a
// production detail setting or coupling the indexer to a candidate profile.
type ParseResultTransform interface {
	SemanticKey() string
	Transform(context.Context, parserapi.Input, graph.ParseResult) (graph.ParseResult, error)
}

// ReportDetail selects optional work whose result is used only for reporting.
// The zero value preserves the historical complete index report.
type ReportDetail string

const (
	ReportComplete      ReportDetail = ""
	ReportWithoutCounts ReportDetail = "without_counts"
)

type BoundaryKind string

const (
	BoundaryFilePersisted       BoundaryKind = "file_persisted"
	BoundaryWorkspacePersisted  BoundaryKind = "workspace_persisted"
	BoundaryFilesRemoved        BoundaryKind = "files_removed"
	BoundaryReconciliationBatch BoundaryKind = "reconciliation_batch"
	BoundaryMetadataPersisted   BoundaryKind = "metadata_persisted"
)

// Boundary identifies a durable indexing boundary suitable for observation or
// deterministic cancellation.
type Boundary struct {
	Kind      BoundaryKind `json:"kind"`
	Path      string       `json:"path,omitempty"`
	Completed int          `json:"completed"`
}

type BoundaryHook func(Boundary) error

// PhaseDurations reports time attributed to each indexing phase. Every field is
// the sum of the per-unit durations measured inside that phase, not a wall-clock
// span of the run. ReadHashNS and ParseNS therefore aggregate work performed on
// several goroutines and can exceed TotalNS when more than one parse worker is
// active; TotalNS alone is wall clock.
type PhaseDurations struct {
	GitProbeNS       int64 `json:"git_probe_ns"`
	MembershipNS     int64 `json:"membership_ns"`
	ChangeProbeNS    int64 `json:"change_probe_ns"`
	DiscoveryNS      int64 `json:"discovery_ns"`
	ReadHashNS       int64 `json:"read_hash_ns"`
	ParseNS          int64 `json:"parse_ns"`
	PersistenceNS    int64 `json:"persistence_ns"`
	ReconciliationNS int64 `json:"reconciliation_ns"`
	TotalNS          int64 `json:"total_ns"`
}

type Report struct {
	Project   Project  `json:"project"`
	Updated   []string `json:"updated"`
	Unchanged int      `json:"unchanged"`
	Removed   []string `json:"removed"`
	Skipped   []string `json:"skipped,omitempty"`
	// ScopedOut counts supported candidate paths rejected by configuration.
	// Scope is intentionally evaluated before stat/symlink/read work, so a
	// rejected candidate is classified here rather than as an unsafe/read skip.
	ScopedOut                    int                `json:"scoped_out"`
	Checked                      int                `json:"content_checked"`
	Diagnostics                  []graph.Diagnostic `json:"diagnostics,omitempty"`
	Counts                       graph.Counts       `json:"-"`
	CountsCollected              bool               `json:"counts_collected"`
	Phases                       PhaseDurations     `json:"phases"`
	Writes                       graph.WriteStats   `json:"writes"`
	ReconciliationBatches        int                `json:"reconciliation_batches"`
	ReconciliationPendingAtStart bool               `json:"reconciliation_pending_at_start"`
	GitCommands                  int                `json:"git_commands"`
	ElapsedMS                    int64              `json:"elapsed_ms"`
	ReconcileMS                  int64              `json:"reconciliation_ms"`
	Rebuild                      string             `json:"rebuild_reason,omitempty"`
	Seed                         *SeedProvenance    `json:"seed,omitempty"`
}

func (r Report) MarshalJSON() ([]byte, error) {
	type reportAlias Report
	encoded := struct {
		reportAlias
		Counts *graph.Counts `json:"counts,omitempty"`
	}{reportAlias: reportAlias(r)}
	if r.CountsCollected {
		encoded.Counts = &r.Counts
	}
	return json.Marshal(encoded)
}

type Service struct {
	repository graph.IndexRepository
	parsers    *parserapi.Registry
}

func NewService(repository graph.IndexRepository, parsers *parserapi.Registry) *Service {
	return &Service{repository: repository, parsers: parsers}
}

func (s *Service) Run(ctx context.Context, project Project, options Options) (report Report, runErr error) {
	started := time.Now()
	report = Report{Project: project, Updated: []string{}, Removed: []string{}, Seed: options.Seed}
	writeStart := writeStats(s.repository)
	progress := newProgressEmitter(project, options.ProgressObserver, started)
	defer func() {
		report.Writes = writeStatsDelta(writeStats(s.repository), writeStart)
		report.Phases.TotalNS = time.Since(started).Nanoseconds()
		report.ElapsedMS = time.Since(started).Milliseconds()
		progress.rebuild = report.Rebuild
		if err := progress.terminal(ctx, runErr); err != nil {
			if runErr == nil {
				runErr = err
			} else {
				runErr = errors.Join(runErr, err)
			}
		}
	}()
	if err := progress.emit(ProgressGitProbe, ProgressStarted, "", 0, 0, ""); err != nil {
		return report, err
	}
	if options.MaxFileSize <= 0 {
		options.MaxFileSize = 5 << 20
	}
	if project.gitSnapshot != nil {
		if project.gitSnapshot.used.Swap(true) {
			runner := project.gitSnapshot.runner
			if runner == nil {
				runner = execGitRunner{}
			}
			refreshed, err := refreshGitSnapshot(ctx, project.gitSnapshot, runner)
			if err != nil {
				return Report{Project: project, Updated: []string{}, Removed: []string{}, Seed: options.Seed}, err
			}
			branch := refreshed.Branch
			if branch == "(detached)" {
				branch = refreshed.DetachedBranch
			}
			if branch != project.Branch {
				return Report{Project: project, Updated: []string{}, Removed: []string{}, Seed: options.Seed}, fmt.Errorf("git branch changed from %q to %q; rediscover the project before indexing", project.Branch, branch)
			}
			project.gitSnapshot = refreshed
			project.Commit = refreshed.Head
			if project.Commit == "(initial)" {
				project.Commit = ""
			}
		}
	}
	report.Project = project
	if project.gitSnapshot != nil {
		report.GitCommands = project.gitSnapshot.Commands
		report.Phases.GitProbeNS = project.gitSnapshot.ProbeNS
	}
	if err := progress.emit(ProgressGitProbe, ProgressCompleted, "", 0, 0, ""); err != nil {
		return report, err
	}
	if status, ok := s.repository.(graph.ReconciliationStatusRepository); ok {
		pending, err := status.ReconciliationPending(ctx)
		if err != nil {
			return report, fmt.Errorf("check pending reconciliation at start: %w", err)
		}
		report.ReconciliationPendingAtStart = pending
	}
	configuration, err := projectconfig.Load(project.Root)
	if err != nil {
		return report, fmt.Errorf("load project configuration: %w", err)
	}
	storedRepositoryID, err := s.repository.Meta(ctx, "repository_id")
	if err != nil {
		return report, fmt.Errorf("load repository identity: %w", err)
	}
	identityChanged := storedRepositoryID != "" && storedRepositoryID != project.ID
	if identityChanged && !options.Force {
		return report, fmt.Errorf("repository identity changed for %s: existing index belongs to %s, discovered %s; run grafo index --force %s to rebuild this branch index", project.Root, storedRepositoryID, project.ID, project.Root)
	}
	if identityChanged {
		report.Rebuild = "repository identity changed"
	}
	indexedVersion, err := s.repository.Meta(ctx, "semantic_index_version")
	if err != nil {
		return report, fmt.Errorf("load semantic index version: %w", err)
	}
	schemaChanged := indexedVersion != SemanticIndexVersion
	if schemaChanged {
		if report.Rebuild == "" {
			report.Rebuild = "semantic schema changed"
		}
	}
	progress.rebuild = report.Rebuild
	indexedCommit, err := s.repository.Meta(ctx, "commit")
	if err != nil {
		return report, fmt.Errorf("load indexed commit: %w", err)
	}
	previousDirtyRaw, err := s.repository.Meta(ctx, GitDirtyPathsMeta)
	if err != nil {
		return report, fmt.Errorf("load dirty paths: %w", err)
	}
	previousDirty, previousDirtyValid := decodeStoredPaths(previousDirtyRaw)
	if indexedCommit != "" && previousDirtyRaw == "" {
		previousDirtyValid = false
	}
	previousUntrackedRaw, err := s.repository.Meta(ctx, GitUntrackedPathsMeta)
	if err != nil {
		return report, fmt.Errorf("load untracked paths: %w", err)
	}
	previousUntracked, previousUntrackedValid := decodeStoredPaths(previousUntrackedRaw)
	if indexedCommit != "" && previousUntrackedRaw == "" {
		previousUntrackedValid = false
	}
	indexedScopeDigest, err := s.repository.Meta(ctx, indexScopeDigestMeta)
	if err != nil {
		return report, fmt.Errorf("load index scope digest: %w", err)
	}
	scopeDigest := configuration.Index.SemanticKey()
	scopeChanged := indexedScopeDigest != scopeDigest
	indexedScopePending, err := s.repository.Meta(ctx, indexScopePendingMeta)
	if err != nil {
		return report, fmt.Errorf("load pending index scope: %w", err)
	}
	indexedScopedOutRaw, err := s.repository.Meta(ctx, indexScopeScopedOutMeta)
	if err != nil {
		return report, fmt.Errorf("load index scope excluded count: %w", err)
	}
	indexedScopedOut, scopedOutErr := strconv.Atoi(indexedScopedOutRaw)
	indexedScopedOutValid := scopedOutErr == nil && indexedScopedOut >= 0
	if err := progress.emit(ProgressMembership, ProgressStarted, "files", 0, 0, ""); err != nil {
		return report, err
	}
	persistenceStarted := time.Now()
	known, err := s.repository.Files(ctx)
	report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	if err != nil {
		return report, fmt.Errorf("load indexed files: %w", err)
	}
	if err := progress.emit(ProgressMembership, ProgressCompleted, "files", len(known), len(known), ""); err != nil {
		return report, err
	}
	reuseMembership := project.gitSnapshot != nil && indexedCommit != "" &&
		project.gitSnapshot.Head == indexedCommit && project.gitSnapshot.MembershipStable &&
		previousUntrackedValid && equalPaths(previousUntracked, project.gitSnapshot.Untracked) &&
		!scopeChanged && indexedScopePending == "" && indexedScopedOutValid && !schemaChanged &&
		previousDirtyValid && !options.Force && options.Boundary == nil
	var detectedChanges gitChanges
	changesValid := false
	var dirtyPaths []string
	var untrackedPaths []string
	dirtyPathsValid := false
	if err := progress.emit(ProgressChangeProbe, ProgressStarted, "files", 0, 0, ""); err != nil {
		return report, err
	}
	if project.GitManaged && project.Commit != "" {
		baseCommit := indexedCommit
		if baseCommit == "" {
			baseCommit = project.Commit
		}
		changeStarted := time.Now()
		var changeErr error
		detectedChanges, changeErr = detectGitChanges(ctx, project.Root, baseCommit, project.gitSnapshot)
		report.Phases.ChangeProbeNS += time.Since(changeStarted).Nanoseconds()
		report.GitCommands += detectedChanges.gitCommands
		changesValid = changeErr == nil
	}
	if err := progress.emit(ProgressChangeProbe, ProgressCompleted, "files", 0, 0, ""); err != nil {
		return report, err
	}
	if err := progress.emit(ProgressDiscovery, ProgressStarted, "files", 0, 0, ""); err != nil {
		return report, err
	}
	discoveryStarted := time.Now()
	membershipStarted := time.Now()
	discovered, err := discoverFilesWithCatalog(ctx, project, s.parsers, known, reuseMembership, configuration.Index)
	report.Phases.MembershipNS += time.Since(membershipStarted).Nanoseconds()
	report.Phases.DiscoveryNS += time.Since(discoveryStarted).Nanoseconds()
	report.GitCommands += discovered.gitCommands
	if err != nil {
		return report, fmt.Errorf("discover source files: %w", err)
	}
	if reuseMembership {
		discovered.scopedOut = indexedScopedOut
	}
	paths := discovered.paths
	if err := progress.emit(ProgressDiscovery, ProgressCompleted, "files", len(paths), len(paths), ""); err != nil {
		return report, err
	}
	report.Skipped = append(report.Skipped, discovered.skipped...)
	report.Diagnostics = append(report.Diagnostics, discovered.diagnostics...)
	report.ScopedOut = discovered.scopedOut
	workspaceSemanticKeys, err := s.parsers.WorkspaceSemanticKeys(ctx, parserapi.Input{
		Root: project.Root, SourcePaths: paths, Repository: project.Name, RepoID: project.ID, GoModule: project.GoModule,
	})
	if err != nil {
		return report, err
	}
	workspaceSemanticKeysJSON, err := json.Marshal(workspaceSemanticKeys)
	if err != nil {
		return report, err
	}
	indexedWorkspaceSemanticKeys, err := s.repository.Meta(ctx, workspaceSemanticKeysMeta)
	if err != nil {
		return report, fmt.Errorf("load parser workspace semantic keys: %w", err)
	}
	changedSemanticLanguages := map[string]bool{}
	if indexedWorkspaceSemanticKeys != "" && indexedWorkspaceSemanticKeys != string(workspaceSemanticKeysJSON) {
		var previous map[string]string
		if json.Unmarshal([]byte(indexedWorkspaceSemanticKeys), &previous) != nil {
			for language := range workspaceSemanticKeys {
				changedSemanticLanguages[language] = true
			}
		} else {
			for language, key := range workspaceSemanticKeys {
				if previous[language] != key {
					changedSemanticLanguages[language] = true
				}
			}
		}
	}
	var selected map[string]bool
	if changesValid {
		dirtyPaths, dirtyPathsValid = detectedChanges.dirty, true
		untrackedPaths = detectedChanges.untracked
		if !options.Force && !schemaChanged && indexedCommit != "" && previousDirtyValid && previousUntrackedValid {
			selected = selectChangedPaths(project.Root, paths, known, detectedChanges.changed, previousDirty, s.parsers)
			// grafo.yaml may intentionally be ignored by Git while remaining the
			// authoritative control-plane input. Hash it on every selected pass so
			// an ignore rule cannot make its indexed evidence stale.
			for _, path := range paths {
				if path == projectconfig.FileName {
					selected[path] = true
					break
				}
			}
		}
	}
	if selected != nil && len(changedSemanticLanguages) > 0 {
		for _, path := range paths {
			if languageParser, ok := s.parsers.For(path); ok && changedSemanticLanguages[languageParser.Language()] {
				selected[path] = true
			}
		}
	}
	current := make(map[string]bool, len(paths))
	graphDirtied := false
	scopeMutationStarted := false
	markScopeMutation := func() error {
		if scopeMutationStarted {
			return nil
		}
		// Publish a fail-reuse marker before the first durable graph mutation.
		// A crash or boundary failure can then never pair an older committed
		// scope digest with the partially-mutated file catalog.
		if err := setMetaIfChanged(ctx, s.repository, indexScopePendingMeta, scopeDigest); err != nil {
			return err
		}
		scopeMutationStarted = true
		return nil
	}
	if err := progress.emit(ProgressReadHash, ProgressStarted, "files", 0, 0, ""); err != nil {
		return report, err
	}
	if err := progress.emit(ProgressParse, ProgressStarted, "files", 0, 0, ""); err != nil {
		return report, err
	}
	if err := progress.emit(ProgressPersistence, ProgressStarted, "files", 0, 0, ""); err != nil {
		return report, err
	}
	stage := &fileStage{project: project, options: options, parsers: s.parsers, paths: paths,
		selected: selected, known: known, workspaceSemanticKeys: workspaceSemanticKeys}
	// A boundary observer is promised one durable boundary per persisted file, so
	// a run that installs one keeps its transaction per file.
	grouping, groupable := s.repository.(graph.BulkIndexRepository)
	if options.Boundary != nil {
		groupable = false
	}
	var grouped []fileOutcome
	groupedRows := 0
	parsedFiles := 0
	// persistGrouped commits the files held back so far and only then records
	// them in the report, which keeps every report slice in path order.
	persistGrouped := func() error {
		if len(grouped) == 0 {
			return nil
		}
		replacements := make([]graph.FileReplacement, 0, len(grouped))
		for _, outcome := range grouped {
			replacements = append(replacements, graph.FileReplacement{File: outcome.record, Parsed: outcome.parsed})
		}
		persistenceStarted := time.Now()
		if err := grouping.ReplaceFiles(ctx, replacements); err != nil {
			return fmt.Errorf("store %d files from %s: %w", len(grouped), grouped[0].path, err)
		}
		graphDirtied = true
		report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
		for _, outcome := range grouped {
			report.Updated = append(report.Updated, outcome.path)
			if err := progress.emit(ProgressPersistence, ProgressProgress, "files", len(report.Updated), 0, ""); err != nil {
				return err
			}
			report.Diagnostics = append(report.Diagnostics, outcome.parsed.Diagnostics...)
		}
		grouped, groupedRows = grouped[:0], 0
		return nil
	}
	// The writer owns every durable mutation and every piece of run state, so
	// report slices, progress counters, and boundary callbacks stay in the
	// original path order regardless of how many workers prepared the files.
	applyOutcome := func(outcome fileOutcome) error {
		// Discovery, reading, and parsing all precede persistence, so a file can
		// disappear in between: a parser side effect, or an external edit during
		// the run. Re-checking existence on the writer, immediately before the
		// only mutation point, keeps a vanished file out of the graph and out of
		// membership at every worker count rather than leaving the outcome to how
		// far the read-ahead had progressed.
		if outcome.kind == outcomeParsed {
			_, statErr := os.Stat(filepath.Join(project.Root, filepath.FromSlash(outcome.path)))
			// Only a path that no longer resolves may discard parsed evidence. A
			// permission or I/O failure on a file that still exists is not proof
			// that it left the repository, so it persists as it did before.
			// ENOTDIR is load-bearing on Unix, where an ancestor replaced by a
			// regular file does not satisfy fs.ErrNotExist; Windows maps it into
			// fs.ErrNotExist already, so it only looks redundant there.
			if errors.Is(statErr, fs.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR) {
				outcome = fileOutcome{path: outcome.path, kind: outcomeUnreadable,
					diagnostic: graph.Diagnostic{Path: outcome.path, Level: "warning", Message: statErr.Error()}}
			}
		}
		switch outcome.kind {
		case outcomeUnselected:
			current[outcome.path] = true
			report.Unchanged++
			return nil
		case outcomeUnreadable:
			if err := persistGrouped(); err != nil {
				return err
			}
			report.Diagnostics = append(report.Diagnostics, outcome.diagnostic)
			return nil
		case outcomeTooLarge:
			if err := persistGrouped(); err != nil {
				return err
			}
			report.Phases.ReadHashNS += outcome.readHashNS
			report.Skipped = append(report.Skipped, outcome.path)
			return nil
		}
		// Every remaining kind read the file, which is exactly what the
		// read/hash counter reports, so a path that later failed the run still
		// lands in the same report slots it would have landed in sequentially.
		report.Checked++
		if err := progress.emit(ProgressReadHash, ProgressProgress, "files", report.Checked, 0, ""); err != nil {
			return err
		}
		current[outcome.path] = true
		switch outcome.kind {
		case outcomeFailed:
			// The sequential pipeline had already attributed whatever work it
			// completed before the failing step, so an interrupted run's phase
			// report keeps the same totals.
			report.Phases.ReadHashNS += outcome.readHashNS
			report.Phases.ParseNS += outcome.parseNS
			return outcome.err
		case outcomeUnsupported:
			return nil
		}
		report.Phases.ReadHashNS += outcome.readHashNS
		if outcome.kind == outcomeUnchanged {
			report.Unchanged++
			return nil
		}
		report.Phases.ParseNS += outcome.parseNS
		parsedFiles++
		if err := progress.emit(ProgressParse, ProgressProgress, "files", parsedFiles, 0, ""); err != nil {
			return err
		}
		outcome.record.IndexedAt = graph.NowUTC()
		if err := markScopeMutation(); err != nil {
			return err
		}
		if groupable {
			grouped = append(grouped, outcome)
			groupedRows += len(outcome.parsed.Nodes) + len(outcome.parsed.Facts)
			if len(grouped) >= maxGroupedFiles || groupedRows >= maxGroupedRows {
				return persistGrouped()
			}
			return nil
		}
		record := outcome.record
		persistenceStarted := time.Now()
		if err := s.repository.ReplaceFile(ctx, record, outcome.parsed); err != nil {
			return fmt.Errorf("store %s: %w", outcome.path, err)
		}
		graphDirtied = true
		report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
		report.Updated = append(report.Updated, outcome.path)
		if err := progress.emit(ProgressPersistence, ProgressProgress, "files", len(report.Updated), 0, ""); err != nil {
			return err
		}
		report.Diagnostics = append(report.Diagnostics, outcome.parsed.Diagnostics...)
		if options.Boundary != nil {
			if err := options.Boundary(Boundary{Kind: BoundaryFilePersisted, Path: outcome.path, Completed: len(report.Updated)}); err != nil {
				return fmt.Errorf("file persistence boundary %s: %w", outcome.path, err)
			}
		}
		return nil
	}
	if err := stage.run(ctx, resolveParseWorkers(options.ParseWorkers), applyOutcome); err != nil {
		return report, err
	}
	if err := persistGrouped(); err != nil {
		return report, err
	}
	if err := progress.emit(ProgressReadHash, ProgressCompleted, "files", report.Checked, 0, ""); err != nil {
		return report, err
	}
	if err := progress.emit(ProgressParse, ProgressCompleted, "files", len(report.Updated), 0, ""); err != nil {
		return report, err
	}
	membershipPaths := make([]string, 0, len(current))
	for path := range current {
		membershipPaths = append(membershipPaths, path)
	}
	sort.Strings(membershipPaths)
	workspace, workspaceDiagnostics := componentWorkspace(project, configuration.Components, membershipPaths)
	report.Diagnostics = append(report.Diagnostics, workspaceDiagnostics...)
	workspaceDigest, err := digestWorkspaceState(workspace)
	if err != nil {
		return report, fmt.Errorf("digest workspace state: %w", err)
	}
	indexedWorkspaceDigest, err := s.repository.Meta(ctx, workspaceStateDigestMeta)
	if err != nil {
		return report, fmt.Errorf("load workspace state digest: %w", err)
	}
	// A boundary hook observes durable work; it never creates any. Rewriting an
	// unchanged workspace just to emit its boundary marked the synthetic
	// workspace owner dirty, which re-enqueued every fact that owner holds — one
	// `contains` fact per file per declared component — so an unchanged refresh
	// could never converge in a single pass. The workspace is replaced only when
	// its own digest proves it changed, exactly as on a hookless run, and the
	// boundary reports the replacement that actually happened.
	replaceWorkspace := options.Force || schemaChanged ||
		!validDigest(indexedWorkspaceDigest) || indexedWorkspaceDigest != workspaceDigest
	if replaceWorkspace {
		persistenceStarted = time.Now()
		if err := markScopeMutation(); err != nil {
			return report, err
		}
		if err := s.repository.ReplaceOwner(ctx, workspaceOwner, workspace); err != nil {
			return report, fmt.Errorf("store workspace: %w", err)
		}
		graphDirtied = true
		if options.Boundary != nil {
			if err := options.Boundary(Boundary{Kind: BoundaryWorkspacePersisted, Completed: 1}); err != nil {
				return report, fmt.Errorf("workspace persistence boundary: %w", err)
			}
		}
		report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	}
	for path := range known {
		if !current[path] {
			report.Removed = append(report.Removed, path)
		}
	}
	sort.Strings(report.Removed)
	if len(report.Removed) > 0 {
		persistenceStarted = time.Now()
		if err := markScopeMutation(); err != nil {
			return report, err
		}
		if err := s.repository.RemoveFiles(ctx, report.Removed); err != nil {
			return report, fmt.Errorf("remove deleted files: %w", err)
		}
		graphDirtied = true
		if options.Boundary != nil {
			if err := options.Boundary(Boundary{Kind: BoundaryFilesRemoved, Completed: len(report.Removed)}); err != nil {
				return report, fmt.Errorf("file removal boundary: %w", err)
			}
		}
		report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	}
	shouldReconcile := graphDirtied || !previousDirtyValid || !previousUntrackedValid || report.ReconciliationPendingAtStart
	if !shouldReconcile {
		if status, ok := s.repository.(graph.ReconciliationStatusRepository); ok {
			shouldReconcile, err = status.ReconciliationPending(ctx)
			if err != nil {
				return report, fmt.Errorf("check pending reconciliation: %w", err)
			}
		} else {
			shouldReconcile = true
		}
	}
	if err := progress.emit(ProgressReconciliation, ProgressStarted, "batches", 0, 0, ""); err != nil {
		return report, err
	}
	if shouldReconcile {
		reconcileStarted := time.Now()
		if instrumented, ok := s.repository.(graph.InstrumentedIndexRepository); ok {
			stats, reconcileErr := instrumented.ReconcileWithStats(ctx, func(stats graph.ReconciliationStats) error {
				if err := progress.emit(ProgressReconciliation, ProgressProgress, "batches", stats.Batches, 0, ""); err != nil {
					return err
				}
				if options.Boundary != nil {
					return options.Boundary(Boundary{Kind: BoundaryReconciliationBatch, Completed: stats.Batches})
				}
				return nil
			})
			report.ReconciliationBatches = stats.Batches
			if reconcileErr != nil {
				return report, fmt.Errorf("resolve graph edges: %w", reconcileErr)
			}
		} else if err := s.repository.Reconcile(ctx); err != nil {
			return report, fmt.Errorf("resolve graph edges: %w", err)
		}
		report.ReconcileMS = time.Since(reconcileStarted).Milliseconds()
		report.Phases.ReconciliationNS = time.Since(reconcileStarted).Nanoseconds()
	}
	if err := progress.emit(ProgressReconciliation, ProgressCompleted, "batches", report.ReconciliationBatches, report.ReconciliationBatches, ""); err != nil {
		return report, err
	}
	persistenceStarted = time.Now()
	if err := setMetaIfChanged(ctx, s.repository, "repository_id", project.ID); err != nil {
		return report, err
	}
	for key, value := range map[string]string{
		"root": project.Root, "branch": project.Branch, "commit": project.Commit,
		"schema_version": fmt.Sprint(graph.SchemaVersion), "semantic_index_version": SemanticIndexVersion,
	} {
		if err := setMetaIfChanged(ctx, s.repository, key, value); err != nil {
			return report, err
		}
	}
	if err := setMetaIfChanged(ctx, s.repository, workspaceSemanticKeysMeta, string(workspaceSemanticKeysJSON)); err != nil {
		return report, err
	}
	if err := setMetaIfChanged(ctx, s.repository, workspaceStateDigestMeta, workspaceDigest); err != nil {
		return report, err
	}
	if dirtyPathsValid {
		encoded, marshalErr := json.Marshal(dirtyPaths)
		if marshalErr != nil {
			return report, marshalErr
		}
		if err := setMetaIfChanged(ctx, s.repository, GitDirtyPathsMeta, string(encoded)); err != nil {
			return report, err
		}
		untrackedEncoded, marshalErr := json.Marshal(untrackedPaths)
		if marshalErr != nil {
			return report, marshalErr
		}
		if err := setMetaIfChanged(ctx, s.repository, GitUntrackedPathsMeta, string(untrackedEncoded)); err != nil {
			return report, err
		}
	}
	// indexed_at remains the time of the most recent successful freshness pass,
	// so it intentionally advances even when every graph and other metadata
	// value is unchanged.
	if err := s.repository.SetMeta(ctx, "indexed_at", graph.NowUTC()); err != nil {
		return report, err
	}
	if err := setMetaIfChanged(ctx, s.repository, indexScopeScopedOutMeta, strconv.Itoa(discovered.scopedOut)); err != nil {
		return report, err
	}
	// Publish the digest last: it is the reuse authority for both membership
	// and the associated scoped-out count, after every other index metadata
	// value for this run has been stored successfully.
	if err := setMetaIfChanged(ctx, s.repository, indexScopeDigestMeta, scopeDigest); err != nil {
		return report, err
	}
	// Clear the fail-reuse marker only after the committed digest and its
	// associated catalog/count metadata are durable.
	if err := setMetaIfChanged(ctx, s.repository, indexScopePendingMeta, ""); err != nil {
		return report, err
	}
	if options.Boundary != nil {
		if err := options.Boundary(Boundary{Kind: BoundaryMetadataPersisted, Completed: 1}); err != nil {
			return report, fmt.Errorf("metadata persistence boundary: %w", err)
		}
	}
	if options.ReportDetail != ReportWithoutCounts {
		// The graph is already durable, so a failed count query degrades the
		// optional summary instead of failing the run. Partial totals are
		// discarded so a caller never reads them as real values, and the cause
		// is reported so a storage failure is never mistaken for a summary the
		// caller simply did not request.
		if counts, countsErr := s.repository.Counts(ctx); countsErr == nil {
			report.Counts, report.CountsCollected = counts, true
		} else {
			// Diagnostics carry repository-relative paths, and this condition is
			// graph-wide rather than owned by one file, so it is attributed to the
			// repository root itself.
			report.Diagnostics = append(report.Diagnostics, graph.Diagnostic{
				Path: ".", Level: "warning", Message: "collect graph counts: " + countsErr.Error()})
		}
	}
	report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	if err := progress.emit(ProgressPersistence, ProgressCompleted, "files", len(report.Updated), 0, ""); err != nil {
		return report, err
	}
	return report, nil
}

func writeStats(repository graph.IndexRepository) graph.WriteStats {
	if instrumented, ok := repository.(graph.InstrumentedWriteRepository); ok {
		return instrumented.WriteStats()
	}
	return graph.WriteStats{}
}

func decodeStoredPaths(raw string) ([]string, bool) {
	if raw == "" {
		return nil, true
	}
	var paths []string
	if err := json.Unmarshal([]byte(raw), &paths); err != nil || paths == nil {
		return nil, false
	}
	normalized := uniquePaths(paths)
	if len(normalized) != len(paths) {
		return nil, false
	}
	for index, path := range paths {
		candidate, err := normalizedGitPath(path)
		if err != nil || candidate != path || normalized[index] != path {
			return nil, false
		}
	}
	return paths, true
}

func digestWorkspaceState(workspace graph.ParseResult) (string, error) {
	encoded, err := json.Marshal(workspace)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func equalPaths(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func setMetaIfChanged(ctx context.Context, repository graph.IndexRepository, key, value string) error {
	current, err := repository.Meta(ctx, key)
	if err != nil {
		return fmt.Errorf("load metadata %s: %w", key, err)
	}
	if current == value {
		return nil
	}
	if err := repository.SetMeta(ctx, key, value); err != nil {
		return fmt.Errorf("store metadata %s: %w", key, err)
	}
	return nil
}

func writeStatsDelta(after, before graph.WriteStats) graph.WriteStats {
	subtract := func(after, before graph.WriteBatchStats) graph.WriteBatchStats {
		return graph.WriteBatchStats{
			Batches: max(0, after.Batches-before.Batches),
			Rows:    max(0, after.Rows-before.Rows),
			Bytes:   max(0, after.Bytes-before.Bytes),
		}
	}
	return graph.WriteStats{
		Nodes: subtract(after.Nodes, before.Nodes),
		Facts: subtract(after.Facts, before.Facts),
		Edges: subtract(after.Edges, before.Edges),
	}
}

func selectChangedPaths(root string, paths []string, known map[string]graph.FileRecord, changed, previousDirty []string, parsers *parserapi.Registry) map[string]bool {
	selected := make(map[string]bool, len(changed)+len(previousDirty))
	for _, path := range changed {
		selected[path] = true
	}
	for _, path := range previousDirty {
		selected[path] = true
	}
	for _, path := range paths {
		if _, exists := known[path]; !exists {
			selected[path] = true
		}
	}
	changedPaths := make([]string, 0, len(selected))
	for path := range selected {
		changedPaths = append(changedPaths, path)
	}
	for _, path := range parsers.SemanticAffectedPaths(root, paths, changedPaths) {
		selected[path] = true
	}
	for _, path := range paths {
		languageParser, ok := parsers.For(path)
		if !ok {
			continue
		}
		provider, ok := languageParser.(parserapi.SemanticDependencyProvider)
		if !ok {
			continue
		}
		for _, dependency := range provider.SemanticDependencies() {
			dependency = filepath.ToSlash(strings.TrimPrefix(dependency, "./"))
			if selected[dependency] {
				selected[path] = true
				break
			}
		}
	}
	return selected
}
