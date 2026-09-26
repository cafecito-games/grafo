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
)

const workspaceOwner = "__workspace__"
const SemanticIndexVersion = "9"

type Options struct {
	Force       bool
	MaxFileSize int64
}

type Report struct {
	Project     Project            `json:"project"`
	Updated     []string           `json:"updated"`
	Unchanged   int                `json:"unchanged"`
	Removed     []string           `json:"removed"`
	Skipped     []string           `json:"skipped,omitempty"`
	Checked     int                `json:"content_checked"`
	Diagnostics []graph.Diagnostic `json:"diagnostics,omitempty"`
	Counts      graph.Counts       `json:"counts"`
	ElapsedMS   int64              `json:"elapsed_ms"`
	ReconcileMS int64              `json:"reconciliation_ms"`
	Rebuild     string             `json:"rebuild_reason,omitempty"`
}

type Service struct {
	repository graph.IndexRepository
	parsers    *parserapi.Registry
}

func NewService(repository graph.IndexRepository, parsers *parserapi.Registry) *Service {
	return &Service{repository: repository, parsers: parsers}
}

func (s *Service) Run(ctx context.Context, project Project, options Options) (Report, error) {
	started := time.Now()
	if options.MaxFileSize <= 0 {
		options.MaxFileSize = 5 << 20
	}
	report := Report{Project: project, Updated: []string{}, Removed: []string{}}
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
	workspace := graph.ParseResult{Nodes: []graph.Node{{
		ID: project.ID, Kind: graph.KindRepository, Name: project.Name,
		QualifiedName: project.Name, OwnerFile: workspaceOwner,
		Properties: map[string]string{"root": project.Root, "branch": project.Branch},
	}}}
	if err := s.repository.ReplaceOwner(ctx, workspaceOwner, workspace); err != nil {
		return report, fmt.Errorf("store workspace: %w", err)
	}
	known, err := s.repository.Files(ctx)
	if err != nil {
		return report, fmt.Errorf("load indexed files: %w", err)
	}
	paths, err := discoverFiles(ctx, project, s.parsers)
	if err != nil {
		return report, fmt.Errorf("discover source files: %w", err)
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
	current := make(map[string]bool, len(paths))
	for _, path := range paths {
		if selected != nil && !selected[path] {
			current[path] = true
			report.Unchanged++
			continue
		}
		absolute := filepath.Join(project.Root, filepath.FromSlash(path))
		info, err := os.Stat(absolute)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, graph.Diagnostic{Path: path, Level: "warning", Message: err.Error()})
			continue
		}
		if info.Size() > options.MaxFileSize {
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
		if keyer, ok := languageParser.(parserapi.SemanticKeyer); ok {
			semanticKey, keyErr := keyer.SemanticKey(ctx, input)
			if keyErr != nil {
				return report, fmt.Errorf("load parser configuration for %s: %w", path, keyErr)
			}
			_, _ = digest.Write([]byte{0})
			_, _ = digest.Write([]byte(semanticKey))
		}
		hash := hex.EncodeToString(digest.Sum(nil))
		previous, exists := known[path]
		if exists && previous.Hash == hash && !options.Force {
			report.Unchanged++
			continue
		}
		parsed, parseErr := languageParser.Parse(ctx, input)
		if parseErr != nil {
			parsed.Diagnostics = append(parsed.Diagnostics, graph.Diagnostic{Path: path, Level: "error", Message: parseErr.Error()})
		}
		fileID := graph.NodeID(graph.KindFile, project.ID+":"+path)
		parsed.Facts = append(parsed.Facts, graph.Fact{
			ID:     graph.FactID(path, project.ID, graph.EdgeContains, fileID, 1, 0),
			FromID: project.ID, Kind: graph.EdgeContains, TargetID: fileID,
			Location: graph.Location{Path: path, Line: 1, Column: 1}, OwnerFile: path,
		})
		record := graph.FileRecord{Path: path, Hash: hash, Language: languageParser.Language(),
			Size: info.Size(), ModifiedNS: info.ModTime().UnixNano(), IndexedAt: graph.NowUTC()}
		if err := s.repository.ReplaceFile(ctx, record, parsed); err != nil {
			return report, fmt.Errorf("store %s: %w", path, err)
		}
		report.Updated = append(report.Updated, path)
		report.Diagnostics = append(report.Diagnostics, parsed.Diagnostics...)
	}
	for path := range known {
		if !current[path] {
			report.Removed = append(report.Removed, path)
		}
	}
	sort.Strings(report.Removed)
	if err := s.repository.RemoveFiles(ctx, report.Removed); err != nil {
		return report, fmt.Errorf("remove deleted files: %w", err)
	}
	reconcileStarted := time.Now()
	if err := s.repository.Reconcile(ctx); err != nil {
		return report, fmt.Errorf("resolve graph edges: %w", err)
	}
	report.ReconcileMS = time.Since(reconcileStarted).Milliseconds()
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
	if dirtyPathsValid {
		encoded, marshalErr := json.Marshal(dirtyPaths)
		if marshalErr != nil {
			return report, marshalErr
		}
		if err := s.repository.SetMeta(ctx, gitDirtyPathsMeta, string(encoded)); err != nil {
			return report, err
		}
	}
	report.Counts, err = s.repository.Counts(ctx)
	if err != nil {
		return report, err
	}
	report.ElapsedMS = time.Since(started).Milliseconds()
	return report, nil
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
