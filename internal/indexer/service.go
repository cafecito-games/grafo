package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

const workspaceOwner = "__workspace__"
const workspaceSemanticKeysMeta = "parser_workspace_semantic_keys"
const SemanticIndexVersion = "29"

type Options struct {
	Force       bool
	MaxFileSize int64
	Boundary    BoundaryHook
}

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
	DiscoveryNS      int64 `json:"discovery_ns"`
	ReadHashNS       int64 `json:"read_hash_ns"`
	ParseNS          int64 `json:"parse_ns"`
	PersistenceNS    int64 `json:"persistence_ns"`
	ReconciliationNS int64 `json:"reconciliation_ns"`
	TotalNS          int64 `json:"total_ns"`
}

type Report struct {
	Project               Project            `json:"project"`
	Updated               []string           `json:"updated"`
	Unchanged             int                `json:"unchanged"`
	Removed               []string           `json:"removed"`
	Skipped               []string           `json:"skipped,omitempty"`
	Checked               int                `json:"content_checked"`
	Diagnostics           []graph.Diagnostic `json:"diagnostics,omitempty"`
	Counts                graph.Counts       `json:"counts"`
	Phases                PhaseDurations     `json:"phases"`
	Writes                graph.WriteStats   `json:"writes"`
	ReconciliationBatches int                `json:"reconciliation_batches"`
	ElapsedMS             int64              `json:"elapsed_ms"`
	ReconcileMS           int64              `json:"reconciliation_ms"`
	Rebuild               string             `json:"rebuild_reason,omitempty"`
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
	writeStart := writeStats(s.repository)
	if options.MaxFileSize <= 0 {
		options.MaxFileSize = 5 << 20
	}
	report = Report{Project: project, Updated: []string{}, Removed: []string{}}
	defer func() {
		report.Writes = writeStatsDelta(writeStats(s.repository), writeStart)
		report.Phases.TotalNS = time.Since(started).Nanoseconds()
		report.ElapsedMS = time.Since(started).Milliseconds()
	}()
	configuration, err := projectconfig.Load(project.Root)
	if err != nil {
		return report, fmt.Errorf("load project configuration: %w", err)
	}
	discoveryStarted := time.Now()
	discovered, err := discoverFiles(ctx, project, s.parsers)
	report.Phases.DiscoveryNS += time.Since(discoveryStarted).Nanoseconds()
	if err != nil {
		return report, fmt.Errorf("discover source files: %w", err)
	}
	paths := discovered.paths
	report.Skipped = append(report.Skipped, discovered.skipped...)
	indexedVersion, err := s.repository.Meta(ctx, "semantic_index_version")
	if err != nil {
		return report, fmt.Errorf("load semantic index version: %w", err)
	}
	schemaChanged := indexedVersion != SemanticIndexVersion
	if schemaChanged {
		report.Rebuild = "semantic schema changed"
	}
	indexedCommit, err := s.repository.Meta(ctx, "commit")
	if err != nil {
		return report, fmt.Errorf("load indexed commit: %w", err)
	}
	previousDirtyRaw, err := s.repository.Meta(ctx, gitDirtyPathsMeta)
	if err != nil {
		return report, fmt.Errorf("load dirty paths: %w", err)
	}
	var previousDirty []string
	previousDirtyValid := true
	if previousDirtyRaw != "" {
		previousDirtyValid = json.Unmarshal([]byte(previousDirtyRaw), &previousDirty) == nil
	}
	persistenceStarted := time.Now()
	known, err := s.repository.Files(ctx)
	report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	if err != nil {
		return report, fmt.Errorf("load indexed files: %w", err)
	}
	workspaceSemanticKeys, err := s.parsers.WorkspaceSemanticKeys(ctx, parserapi.Input{
		Root: project.Root, Repository: project.Name, RepoID: project.ID, GoModule: project.GoModule,
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
	var dirtyPaths []string
	dirtyPathsValid := false
	if project.GitManaged && project.Commit != "" {
		baseCommit := indexedCommit
		if baseCommit == "" {
			baseCommit = project.Commit
		}
		changes, changeErr := detectGitChanges(ctx, project.Root, baseCommit)
		if changeErr == nil {
			dirtyPaths, dirtyPathsValid = changes.dirty, true
			if !options.Force && !schemaChanged && indexedCommit != "" && previousDirtyValid {
				selected = selectChangedPaths(paths, known, changes.changed, previousDirty, s.parsers)
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
		current[path] = true
		languageParser, ok := s.parsers.For(path)
		if !ok {
			continue
		}
		input := parserapi.Input{Root: project.Root, Path: path, Content: content,
			Repository: project.Name, RepoID: project.ID, GoModule: project.GoModule}
		digest := sha256.New()
		_, _ = digest.Write(content)
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(SemanticIndexVersion))
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
		if err := s.repository.ReplaceFile(ctx, record, parsed); err != nil {
			return report, fmt.Errorf("store %s: %w", path, err)
		}
		report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
		report.Updated = append(report.Updated, path)
		report.Diagnostics = append(report.Diagnostics, parsed.Diagnostics...)
		if options.Boundary != nil {
			if err := options.Boundary(Boundary{Kind: BoundaryFilePersisted, Path: path, Completed: len(report.Updated)}); err != nil {
				return report, fmt.Errorf("file persistence boundary %s: %w", path, err)
			}
		}
	}
	membershipPaths := make([]string, 0, len(current))
	for path := range current {
		membershipPaths = append(membershipPaths, path)
	}
	sort.Strings(membershipPaths)
	workspace, workspaceDiagnostics := componentWorkspace(project, configuration.Components, membershipPaths)
	report.Diagnostics = append(report.Diagnostics, workspaceDiagnostics...)
	persistenceStarted = time.Now()
	if err := s.repository.ReplaceOwner(ctx, workspaceOwner, workspace); err != nil {
		return report, fmt.Errorf("store workspace: %w", err)
	}
	if options.Boundary != nil {
		if err := options.Boundary(Boundary{Kind: BoundaryWorkspacePersisted, Completed: 1}); err != nil {
			return report, fmt.Errorf("workspace persistence boundary: %w", err)
		}
	}
	report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	for path := range known {
		if !current[path] {
			report.Removed = append(report.Removed, path)
		}
	}
	sort.Strings(report.Removed)
	persistenceStarted = time.Now()
	if err := s.repository.RemoveFiles(ctx, report.Removed); err != nil {
		return report, fmt.Errorf("remove deleted files: %w", err)
	}
	if options.Boundary != nil && len(report.Removed) > 0 {
		if err := options.Boundary(Boundary{Kind: BoundaryFilesRemoved, Completed: len(report.Removed)}); err != nil {
			return report, fmt.Errorf("file removal boundary: %w", err)
		}
	}
	report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	reconcileStarted := time.Now()
	if instrumented, ok := s.repository.(graph.InstrumentedIndexRepository); ok {
		stats, err := instrumented.ReconcileWithStats(ctx, func(stats graph.ReconciliationStats) error {
			if options.Boundary == nil {
				return nil
			}
			return options.Boundary(Boundary{Kind: BoundaryReconciliationBatch, Completed: stats.Batches})
		})
		report.ReconciliationBatches = stats.Batches
		report.ReconcileMS = time.Since(reconcileStarted).Milliseconds()
		report.Phases.ReconciliationNS = time.Since(reconcileStarted).Nanoseconds()
		if err != nil {
			return report, fmt.Errorf("resolve graph edges: %w", err)
		}
	} else if err := s.repository.Reconcile(ctx); err != nil {
		return report, fmt.Errorf("resolve graph edges: %w", err)
	}
	report.ReconcileMS = time.Since(reconcileStarted).Milliseconds()
	report.Phases.ReconciliationNS = time.Since(reconcileStarted).Nanoseconds()
	persistenceStarted = time.Now()
	if err := s.repository.SetMeta(ctx, "repository_id", project.ID); err != nil {
		return report, err
	}
	for key, value := range map[string]string{
		"root": project.Root, "branch": project.Branch, "commit": project.Commit,
		"indexed_at": graph.NowUTC(), "schema_version": fmt.Sprint(graph.SchemaVersion),
		"semantic_index_version": SemanticIndexVersion,
	} {
		if err := s.repository.SetMeta(ctx, key, value); err != nil {
			return report, err
		}
	}
	if err := s.repository.SetMeta(ctx, workspaceSemanticKeysMeta, string(workspaceSemanticKeysJSON)); err != nil {
		return report, err
	}
	if dirtyPathsValid {
		encoded, marshalErr := json.Marshal(dirtyPaths)
		if marshalErr != nil {
			return report, marshalErr
		}
		if err := s.repository.SetMeta(ctx, gitDirtyPathsMeta, string(encoded)); err != nil {
			return report, err
		}
	}
	if options.Boundary != nil {
		if err := options.Boundary(Boundary{Kind: BoundaryMetadataPersisted, Completed: 1}); err != nil {
			return report, fmt.Errorf("metadata persistence boundary: %w", err)
		}
	}
	report.Counts, err = s.repository.Counts(ctx)
	report.Phases.PersistenceNS += time.Since(persistenceStarted).Nanoseconds()
	if err != nil {
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
