// Package indexes inventories and prunes branch-specific Grafo indexes.
package indexes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
	lifeservice "github.com/cafecito-games/grafo/internal/service"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type Compatibility string

const (
	CompatibilityCompatible   Compatibility = "compatible"
	CompatibilityIncompatible Compatibility = "incompatible"
	CompatibilityCorrupt      Compatibility = "corrupt"
	CompatibilityUnverified   Compatibility = "unverified"

	StatusDeleted     Status = "deleted"
	StatusWouldDelete Status = "would-delete"
	StatusProtected   Status = "protected"
	StatusIneligible  Status = "ineligible"
	StatusLocked      Status = "locked"
	StatusFailed      Status = "failed"

	inspectionTimeout = 2 * time.Second
	maxDiagnostic     = 240
)

// Sizes reports the physical bytes used by an index database and its SQLite
// sidecars.
type Sizes struct {
	Database int64 `json:"database"`
	WAL      int64 `json:"wal"`
	SHM      int64 `json:"shm"`
	Total    int64 `json:"total"`
}

// Add returns the component-wise sum of two physical size reports.
func (s Sizes) Add(other Sizes) Sizes {
	return Sizes{
		Database: s.Database + other.Database,
		WAL:      s.WAL + other.WAL,
		SHM:      s.SHM + other.SHM,
		Total:    s.Total + other.Total,
	}
}

// Index describes one branch index discovered in the repository's index
// directory.
type Index struct {
	Filename      string        `json:"filename"`
	Path          string        `json:"path"`
	Root          string        `json:"root,omitempty"`
	RepositoryID  string        `json:"repository_id,omitempty"`
	Branch        string        `json:"branch,omitempty"`
	Commit        string        `json:"commit,omitempty"`
	IndexedAt     string        `json:"indexed_at,omitempty"`
	Sizes         Sizes         `json:"sizes"`
	Current       bool          `json:"current"`
	Compatibility Compatibility `json:"compatibility"`
	Verified      bool          `json:"verified"`
	Diagnostic    string        `json:"diagnostic,omitempty"`

	indexedTime time.Time
}

// Inventory is a deterministic snapshot of the repository's branch indexes.
type Inventory struct {
	Root           string  `json:"root"`
	RepositoryID   string  `json:"repository_id"`
	CurrentIndex   string  `json:"current_index"`
	IndexDirectory string  `json:"index_directory"`
	Indexes        []Index `json:"indexes"`
	Totals         Sizes   `json:"totals"`
}

// Policy selects stale indexes for pruning. When both selectors are present,
// an index must satisfy both.
type Policy struct {
	OlderThan *time.Duration
	Keep      *int
	DryRun    bool
	Confirm   bool
	Now       time.Time
}

// Status is the terminal outcome for one prune candidate.
type Status string

// Result reports the prune decision and reclaimed bytes for one index.
type Result struct {
	Index     Index  `json:"index"`
	Status    Status `json:"status"`
	Reason    string `json:"reason"`
	Selected  bool   `json:"selected"`
	Reclaimed Sizes  `json:"reclaimed"`
}

// PruneReport reports every candidate independently, including partial
// failures and successfully reclaimed bytes.
type PruneReport struct {
	Inventory Inventory `json:"inventory"`
	DryRun    bool      `json:"dry_run"`
	Results   []Result  `json:"results"`
	Reclaimed Sizes     `json:"reclaimed"`
}

type manager struct {
	discover   func(context.Context, string) (indexer.Project, error)
	inspect    func(context.Context, string) (sqlite.IndexInspection, error)
	tryLock    func(string) (lifeservice.Unlock, bool, error)
	checkpoint func(context.Context, string, sqlite.IndexMetadata) error
	readDir    func(string) ([]fs.DirEntry, error)
	lstat      func(string) (fs.FileInfo, error)
	remove     func(string) error
}

func newManager() *manager {
	return &manager{
		discover: indexer.DiscoverProject, inspect: sqlite.InspectIndex,
		tryLock: lifeservice.TryIndexLock, checkpoint: sqlite.CheckpointIndex,
		readDir: os.ReadDir, lstat: os.Lstat, remove: os.Remove,
	}
}

// List inventories direct branch index files without creating or migrating any
// database.
func List(ctx context.Context, root string) (Inventory, error) {
	return newManager().list(ctx, root)
}

// Prune removes verified stale branch indexes under a fail-closed policy.
func Prune(ctx context.Context, root string, policy Policy) (PruneReport, error) {
	return newManager().prune(ctx, root, policy)
}

func (m *manager) list(ctx context.Context, root string) (Inventory, error) {
	project, err := m.discover(ctx, root)
	if err != nil {
		return Inventory{}, err
	}
	current, err := filepath.Abs(project.IndexPath)
	if err != nil {
		return Inventory{}, fmt.Errorf("resolve current index path: %w", err)
	}
	directory := filepath.Dir(current)
	inventory := Inventory{
		Root: project.Root, RepositoryID: project.ID, CurrentIndex: current,
		IndexDirectory: directory, Indexes: []Index{},
	}
	directoryInfo, err := m.lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return inventory, nil
	}
	if err != nil {
		return Inventory{}, fmt.Errorf("inspect branch index directory: %w", err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return Inventory{}, fmt.Errorf("branch index directory is not a non-symlink directory")
	}
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return Inventory{}, fmt.Errorf("resolve branch index directory: %w", err)
	}
	if !samePath(directory, resolvedDirectory) {
		return Inventory{}, fmt.Errorf("branch index directory resolves outside the discovered repository")
	}
	entries, err := m.readDir(directory)
	if err != nil {
		return Inventory{}, fmt.Errorf("list branch indexes: %w", err)
	}
	for _, directoryEntry := range entries {
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		name := directoryEntry.Name()
		if filepath.Ext(name) != ".sqlite" || filepath.Base(name) != name {
			continue
		}
		path := filepath.Join(directory, name)
		info, statErr := m.lstat(path)
		if statErr != nil {
			continue
		}
		if info.IsDir() || (!info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0) {
			continue
		}
		candidate := Index{Filename: name, Path: path, Current: samePath(path, current)}
		if info.Mode()&os.ModeSymlink != 0 {
			candidate.Compatibility = CompatibilityUnverified
			candidate.Diagnostic = "candidate is a symlink"
			inventory.Indexes = append(inventory.Indexes, candidate)
			continue
		}
		candidate.Sizes.Database = info.Size()
		candidate.Sizes.Total = info.Size()
		if size, safe, reason := m.sidecarSize(directory, path+"-wal"); !safe {
			candidate.Diagnostic = appendDiagnostic(candidate.Diagnostic, reason)
		} else {
			candidate.Sizes.WAL = size
			candidate.Sizes.Total += size
		}
		if size, safe, reason := m.sidecarSize(directory, path+"-shm"); !safe {
			candidate.Diagnostic = appendDiagnostic(candidate.Diagnostic, reason)
		} else {
			candidate.Sizes.SHM = size
			candidate.Sizes.Total += size
		}

		inspectionCtx, cancel := context.WithTimeout(ctx, inspectionTimeout)
		inspection, inspectErr := m.inspect(inspectionCtx, path)
		cancel()
		if inspectErr != nil {
			if ctx.Err() != nil {
				return Inventory{}, ctx.Err()
			}
			candidate.Compatibility = CompatibilityUnverified
			candidate.Diagnostic = appendDiagnostic(candidate.Diagnostic, inspectErr.Error())
		} else {
			candidate.Root = inspection.Metadata.Root
			candidate.RepositoryID = inspection.Metadata.RepositoryID
			candidate.Branch = inspection.Metadata.Branch
			candidate.Commit = inspection.Metadata.Commit
			candidate.IndexedAt = inspection.Metadata.IndexedAt
			candidate.Compatibility = compatibility(inspection.Compatibility)
			candidate.Diagnostic = appendDiagnostic(candidate.Diagnostic, inspection.Diagnostic)
		}
		candidate.indexedTime, candidate.Verified, candidate.Diagnostic = verifyCandidate(candidate, project)
		inventory.Indexes = append(inventory.Indexes, candidate)
	}
	sortIndexes(inventory.Indexes)
	for _, candidate := range inventory.Indexes {
		inventory.Totals = inventory.Totals.Add(candidate.Sizes)
	}
	return inventory, nil
}

func (m *manager) sidecarSize(directory, path string) (int64, bool, string) {
	if !containedPath(directory, path) {
		return 0, false, "sidecar path escapes index directory"
	}
	info, err := m.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, true, ""
	}
	if err != nil {
		return 0, false, fmt.Sprintf("inspect sidecar %s: %v", filepath.Base(path), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, false, fmt.Sprintf("sidecar %s is not a regular non-symlink file", filepath.Base(path))
	}
	return info.Size(), true, ""
}

func (m *manager) prune(ctx context.Context, root string, policy Policy) (PruneReport, error) {
	if err := validatePolicy(policy); err != nil {
		return PruneReport{}, err
	}
	inventory, err := m.list(ctx, root)
	if err != nil {
		return PruneReport{}, err
	}
	report := PruneReport{Inventory: inventory, DryRun: policy.DryRun, Results: []Result{}}
	now := policy.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	keep := retainedIndexes(inventory.Indexes, policy.Keep)
	var operationErr error
	for _, candidate := range inventory.Indexes {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(operationErr, err)
		}
		result := Result{Index: candidate}
		switch {
		case candidate.Current:
			result.Status, result.Reason = StatusProtected, "current branch index"
		case !candidate.Verified:
			result.Status, result.Reason = StatusIneligible, diagnosticOr(candidate.Diagnostic, "index is not verified")
		case candidate.indexedTime.After(now):
			result.Status, result.Reason = StatusProtected, "indexed_at is in the future"
		case policy.OlderThan != nil && now.Sub(candidate.indexedTime) <= *policy.OlderThan:
			result.Status, result.Reason = StatusProtected, "inside --older-than retention window"
		case policy.Keep != nil && keep[candidate.Path]:
			result.Status, result.Reason = StatusProtected, "inside --keep retention set"
		default:
			result.Selected = true
			if policy.DryRun {
				result.Status, result.Reason = StatusWouldDelete, "dry run; would delete"
			} else {
				result = m.deleteCandidate(ctx, root, result)
				if result.Status == StatusFailed {
					operationErr = errors.Join(operationErr, errors.New(result.Reason))
				}
			}
		}
		report.Reclaimed = report.Reclaimed.Add(result.Reclaimed)
		report.Results = append(report.Results, result)
	}
	return report, operationErr
}

func (m *manager) deleteCandidate(ctx context.Context, root string, initial Result) (result Result) {
	result = initial
	candidate := result.Index
	unlock, acquired, err := m.tryLock(candidate.Path)
	if err != nil {
		result.Status, result.Reason = StatusFailed, boundedDiagnostic(err.Error())
		return result
	}
	if !acquired {
		result.Status, result.Reason = StatusLocked, "branch index is locked"
		return result
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil && result.Status != StatusFailed {
			result.Status, result.Reason = StatusFailed, boundedDiagnostic("unlock branch index: "+unlockErr.Error())
		}
	}()
	currentProject, err := m.discover(ctx, root)
	if err != nil {
		result.Status, result.Reason = StatusFailed, boundedDiagnostic("revalidate current branch index: "+err.Error())
		return result
	}
	currentPath, err := filepath.Abs(currentProject.IndexPath)
	if err != nil {
		result.Status, result.Reason = StatusFailed, boundedDiagnostic("resolve revalidated current index path: "+err.Error())
		return result
	}
	if samePath(candidate.Path, currentPath) {
		result.Selected = false
		result.Status, result.Reason = StatusProtected, "became the current branch index before deletion"
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Status, result.Reason = StatusFailed, err.Error()
		return result
	}
	expected := sqlite.IndexMetadata{
		Root: candidate.Root, RepositoryID: candidate.RepositoryID, Branch: candidate.Branch,
		Commit: candidate.Commit, IndexedAt: candidate.IndexedAt,
	}
	if err := m.checkpoint(ctx, candidate.Path, expected); err != nil {
		result.Status, result.Reason = StatusFailed, boundedDiagnostic(err.Error())
		return result
	}
	targets, err := m.removalTargets(candidate.Path)
	if err != nil {
		result.Status, result.Reason = StatusFailed, boundedDiagnostic(err.Error())
		return result
	}
	for index, target := range targets {
		if err := ctx.Err(); err != nil {
			result.Status, result.Reason = StatusFailed, err.Error()
			return result
		}
		if err := m.remove(target.path); err != nil {
			if index > 0 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			result.Status, result.Reason = StatusFailed, boundedDiagnostic(fmt.Sprintf("remove %s: %v", filepath.Base(target.path), err))
			return result
		}
		result.Reclaimed = result.Reclaimed.Add(target.sizes)
	}
	result.Status, result.Reason = StatusDeleted, "deleted verified stale branch index"
	return result
}

type removalTarget struct {
	path  string
	sizes Sizes
}

func (m *manager) removalTargets(primary string) ([]removalTarget, error) {
	directory := filepath.Dir(primary)
	paths := []string{primary, primary + "-wal", primary + "-shm"}
	targets := make([]removalTarget, 0, len(paths))
	for index, path := range paths {
		if !containedPath(directory, path) {
			return nil, fmt.Errorf("deletion target escapes index directory")
		}
		info, err := m.lstat(path)
		if errors.Is(err, os.ErrNotExist) && index > 0 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect deletion target %s: %w", filepath.Base(path), err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("deletion target %s is not a regular non-symlink file", filepath.Base(path))
		}
		sizes := Sizes{Total: info.Size()}
		switch index {
		case 0:
			sizes.Database = info.Size()
		case 1:
			sizes.WAL = info.Size()
		case 2:
			sizes.SHM = info.Size()
		}
		targets = append(targets, removalTarget{path: path, sizes: sizes})
	}
	return targets, nil
}

func validatePolicy(policy Policy) error {
	if policy.OlderThan == nil && policy.Keep == nil {
		return fmt.Errorf("at least one of --older-than or --keep is required")
	}
	if policy.OlderThan != nil && *policy.OlderThan < 0 {
		return fmt.Errorf("--older-than must not be negative")
	}
	if policy.Keep != nil && *policy.Keep < 0 {
		return fmt.Errorf("--keep must not be negative")
	}
	if !policy.DryRun && !policy.Confirm {
		return fmt.Errorf("pruning requires --yes (or use --dry-run)")
	}
	return nil
}

func retainedIndexes(indexes []Index, count *int) map[string]bool {
	retained := map[string]bool{}
	if count == nil || *count == 0 {
		return retained
	}
	verified := make([]Index, 0, len(indexes))
	remaining := *count
	for _, candidate := range indexes {
		if candidate.Verified && candidate.Current && remaining > 0 {
			retained[candidate.Path] = true
			remaining--
			continue
		}
		if candidate.Verified && !candidate.Current {
			verified = append(verified, candidate)
		}
	}
	sort.SliceStable(verified, func(left, right int) bool {
		if !verified[left].indexedTime.Equal(verified[right].indexedTime) {
			return verified[left].indexedTime.After(verified[right].indexedTime)
		}
		if verified[left].Branch != verified[right].Branch {
			return verified[left].Branch < verified[right].Branch
		}
		return verified[left].Filename < verified[right].Filename
	})
	for index := 0; index < len(verified) && index < remaining; index++ {
		retained[verified[index].Path] = true
	}
	return retained
}

func verifyCandidate(candidate Index, project indexer.Project) (time.Time, bool, string) {
	diagnostic := candidate.Diagnostic
	if candidate.Compatibility != CompatibilityCompatible {
		return time.Time{}, false, diagnostic
	}
	if diagnostic != "" {
		return time.Time{}, false, diagnostic
	}
	if filepath.Clean(candidate.Root) != filepath.Clean(project.Root) {
		return time.Time{}, false, appendDiagnostic(diagnostic, "stored root does not match discovered project")
	}
	if candidate.RepositoryID != project.ID {
		return time.Time{}, false, appendDiagnostic(diagnostic, "stored repository_id does not match discovered project")
	}
	if strings.TrimSpace(candidate.Branch) == "" {
		return time.Time{}, false, appendDiagnostic(diagnostic, "stored branch is missing")
	}
	indexedAt, err := time.Parse(time.RFC3339Nano, candidate.IndexedAt)
	if err != nil || !strings.HasSuffix(candidate.IndexedAt, "Z") {
		return time.Time{}, false, appendDiagnostic(diagnostic, "stored indexed_at is not valid RFC3339 UTC")
	}
	return indexedAt.UTC(), true, diagnostic
}

func sortIndexes(indexes []Index) {
	sort.SliceStable(indexes, func(left, right int) bool {
		if indexes[left].Current != indexes[right].Current {
			return indexes[left].Current
		}
		if !indexes[left].indexedTime.Equal(indexes[right].indexedTime) {
			return indexes[left].indexedTime.After(indexes[right].indexedTime)
		}
		if indexes[left].Branch != indexes[right].Branch {
			return indexes[left].Branch < indexes[right].Branch
		}
		return indexes[left].Filename < indexes[right].Filename
	})
}

func compatibility(value sqlite.IndexCompatibility) Compatibility {
	switch value {
	case sqlite.CompatibilityCompatible:
		return CompatibilityCompatible
	case sqlite.CompatibilityIncompatible:
		return CompatibilityIncompatible
	case sqlite.CompatibilityCorrupt:
		return CompatibilityCorrupt
	default:
		return CompatibilityUnverified
	}
}

func containedPath(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func samePath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func appendDiagnostic(existing, added string) string {
	if added == "" {
		return existing
	}
	if existing != "" {
		added = existing + "; " + added
	}
	return boundedDiagnostic(added)
}

func boundedDiagnostic(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) <= maxDiagnostic {
		return message
	}
	return message[:maxDiagnostic-3] + "..."
}

func diagnosticOr(diagnostic, fallback string) string {
	if diagnostic != "" {
		return diagnostic
	}
	return fallback
}
