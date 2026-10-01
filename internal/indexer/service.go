package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexversion"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

const workspaceOwner = "__workspace__"
const workspaceSemanticKeysMeta = "parser_workspace_semantic_keys"
const workspaceStateDigestMeta = "workspace_state_digest"
const gitUntrackedPathsMeta = "git_untracked_paths"
const indexScopeDigestMeta = "index_scope_digest"
const indexScopePendingMeta = "index_scope_pending"
const indexScopeScopedOutMeta = "index_scope_scoped_out"
const SemanticIndexVersion = indexversion.Semantic

type Options struct {
	Force            bool
	MaxFileSize      int64
	Boundary         BoundaryHook
	ProgressObserver ProgressObserver
	ReportDetail     ReportDetail
	// ResultTransform is a benchmark/test-only interception point after parsing
	// and before any durable mutation. Production CLI, MCP, watch, and service
	// composition leave it nil. Its semantic key participates in each file hash
	// so switching a prototype profile reparses instead of reusing stale detail.
	ResultTransform ParseResultTransform
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
	report = Report{Project: project, Updated: []string{}, Removed: []string{}}
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
				return Report{Project: project, Updated: []string{}, Removed: []string{}}, err
			}
			branch := refreshed.Branch
			if branch == "(detached)" {
				branch = refreshed.DetachedBranch
			}
			if branch != project.Branch {
				return Report{Project: project, Updated: []string{}, Removed: []string{}}, fmt.Errorf("git branch changed from %q to %q; rediscover the project before indexing", project.Branch, branch)
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
	previousDirtyRaw, err := s.repository.Meta(ctx, gitDirtyPathsMeta)
	if err != nil {
		return report, fmt.Errorf("load dirty paths: %w", err)
	}
	previousDirty, previousDirtyValid := decodeStoredPaths(previousDirtyRaw)
	if indexedCommit != "" && previousDirtyRaw == "" {
		previousDirtyValid = false
	}
	previousUntrackedRaw, err := s.repository.Meta(ctx, gitUntrackedPathsMeta)
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
			selected = selectChangedPaths(paths, known, detectedChanges.changed, previousDirty, s.parsers)
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
	for _, path := range paths {
		if selected != nil && !selected[path] {
			current[path] = true
			report.Unchanged++
			continue
		}
		absolute := filepath.Join(project.Root, filepath.FromSlash(path))
		readStarted := time.Now()
		info, err := os.Stat(absolute)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, graph.Diagnostic{Path: path, Level: "warning", Message: err.Error()})
			continue
		}
		if info.Size() > options.MaxFileSize {
			report.Phases.ReadHashNS += time.Since(readStarted).Nanoseconds()
			report.Skipped = append(report.Skipped, path)
			continue
		}
		content, err := os.ReadFile(absolute)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, graph.Diagnostic{Path: path, Level: "warning", Message: err.Error()})
			continue
		}
		report.Checked++
		if err := progress.emit(ProgressReadHash, ProgressProgress, "files", report.Checked, 0, ""); err != nil {
			return report, err
		}
		current[path] = true
		languageParser, ok := s.parsers.For(path)
		if !ok {
			continue
		}
		input := parserapi.Input{Root: project.Root, Path: path, Content: content,
			SourcePaths: paths, Repository: project.Name, RepoID: project.ID, GoModule: project.GoModule}
		digest := sha256.New()
		_, _ = digest.Write(content)
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(SemanticIndexVersion))
		if options.ResultTransform != nil {
			_, _ = digest.Write([]byte{0})
			_, _ = digest.Write([]byte(options.ResultTransform.SemanticKey()))
		}
		if _, ok := languageParser.(parserapi.WorkspaceSemanticKeyer); ok {
			semanticKey := workspaceSemanticKeys[languageParser.Language()]
			_, _ = digest.Write([]byte{0})
			_, _ = digest.Write([]byte(semanticKey))
			input.SemanticKey = semanticKey
		} else if keyer, ok := languageParser.(parserapi.SemanticKeyer); ok {
			semanticKey, keyErr := keyer.SemanticKey(ctx, input)
			if keyErr != nil {
				return report, fmt.Errorf("load parser configuration for %s: %w", path, keyErr)
			}
			_, _ = digest.Write([]byte{0})
			_, _ = digest.Write([]byte(semanticKey))
			input.SemanticKey = semanticKey
		}
		hash := hex.EncodeToString(digest.Sum(nil))
		report.Phases.ReadHashNS += time.Since(readStarted).Nanoseconds()
		previous, exists := known[path]
		if exists && previous.Hash == hash && !options.Force {
			report.Unchanged++
			continue
		}
		parseStarted := time.Now()
		parsed, parseErr := languageParser.Parse(ctx, input)
		report.Phases.ParseNS += time.Since(parseStarted).Nanoseconds()
		if parseErr != nil {
			parsed.Diagnostics = append(parsed.Diagnostics, graph.Diagnostic{Path: path, Level: "error", Message: parseErr.Error()})
		}
		if options.ResultTransform != nil {
			transformStarted := time.Now()
			parsed, err = options.ResultTransform.Transform(ctx, input, parsed)
			report.Phases.ParseNS += time.Since(transformStarted).Nanoseconds()
			if err != nil {
				return report, fmt.Errorf("transform parsed evidence for %s: %w", path, err)
			}
		}
		if err := progress.emit(ProgressParse, ProgressProgress, "files", len(report.Updated)+1, 0, ""); err != nil {
			return report, err
		}
		// The selected parser is the producer authority. Parser-returned facts are
		// otherwise untrusted and cannot claim another extractor's identity.
		for index := range parsed.Facts {
			parsed.Facts[index].Producer = languageParser.Language()
		}
		fileID := graph.NodeID(graph.KindFile, project.ID+":"+path)
		parsed.Facts = append(parsed.Facts, graph.Fact{
			ID:     graph.FactID(path, project.ID, graph.EdgeContains, fileID, 1, 0),
			FromID: project.ID, Kind: graph.EdgeContains, Producer: graph.ProducerIndexer, TargetID: fileID,
			Location: graph.Location{Path: path, Line: 1, Column: 1}, OwnerFile: path,
		})
		record := graph.FileRecord{Path: path, Hash: hash, Language: languageParser.Language(),
			Size: info.Size(), ModifiedNS: info.ModTime().UnixNano(), IndexedAt: graph.NowUTC()}
		persistenceStarted := time.Now()
		if err := markScopeMutation(); err != nil {
			return report, err
		}
		if err := s.repository.ReplaceFile(ctx, record, parsed); err != nil {
			return report, fmt.Errorf("store %s: %w", path, err)
		}
		graphDirtied = true
		report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
		report.Updated = append(report.Updated, path)
		if err := progress.emit(ProgressPersistence, ProgressProgress, "files", len(report.Updated), 0, ""); err != nil {
			return report, err
		}
		report.Diagnostics = append(report.Diagnostics, parsed.Diagnostics...)
		if options.Boundary != nil {
			if err := options.Boundary(Boundary{Kind: BoundaryFilePersisted, Path: path, Completed: len(report.Updated)}); err != nil {
				return report, fmt.Errorf("file persistence boundary %s: %w", path, err)
			}
		}
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
	replaceWorkspace := options.Force || options.Boundary != nil || schemaChanged ||
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
		if err := setMetaIfChanged(ctx, s.repository, gitDirtyPathsMeta, string(encoded)); err != nil {
			return report, err
		}
		untrackedEncoded, marshalErr := json.Marshal(untrackedPaths)
		if marshalErr != nil {
			return report, marshalErr
		}
		if err := setMetaIfChanged(ctx, s.repository, gitUntrackedPathsMeta, string(untrackedEncoded)); err != nil {
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

func selectChangedPaths(paths []string, known map[string]graph.FileRecord, changed, previousDirty []string, parsers *parserapi.Registry) map[string]bool {
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
	for _, path := range parsers.SemanticAffectedPaths(paths, changedPaths) {
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
