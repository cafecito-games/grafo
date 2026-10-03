package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
	"github.com/cafecito-games/grafo/internal/storage/sqlitedriver"
)

const maxMetadataEntries = 10_000

// StorageMetrics reports SQLite page allocation. ReclaimableBytes is an
// estimate based on freelist pages; WAL and SHM bytes are intentionally absent.
type StorageMetrics struct {
	PageSize           int64   `json:"page_size"`
	PageCount          int64   `json:"page_count"`
	FreelistCount      int64   `json:"freelist_count"`
	LiveAllocatedBytes int64   `json:"live_allocated_bytes"`
	ReclaimableBytes   int64   `json:"reclaimable_bytes"`
	ReclaimablePercent float64 `json:"reclaimable_percent"`
}

// MetadataEntry is one deterministic opaque metadata key/value pair.
type MetadataEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// CompactionResult is the adapter-owned logical preservation evidence for one
// successful in-place VACUUM.
type CompactionResult struct {
	Before StorageMetrics `json:"before"`
	After  StorageMetrics `json:"after"`
	Counts graph.Counts   `json:"counts"`
}

// MaintenanceRepository is the narrow writable capability required by current
// index lifecycle orchestration.
type MaintenanceRepository interface {
	Compact(context.Context) (CompactionResult, error)
	Close() error
}

// OpenMaintenance opens an existing compatible index for bounded maintenance.
// It never creates the file, runs migrations, or changes compatibility metadata.
func OpenMaintenance(ctx context.Context, path string) (MaintenanceRepository, error) {
	repository, err := openMaintenanceRepository(ctx, path)
	if err != nil {
		return nil, err
	}
	return repository, nil
}

func openMaintenanceRepository(ctx context.Context, path string) (*Repository, error) {
	db, err := openExistingIndex(ctx, path, "rw")
	if err != nil {
		return nil, err
	}
	reader := &Repository{db: db, queries: sqlcgen.New(db), path: path}
	if err := validateReadCompatibility(ctx, reader); err != nil {
		_ = db.Close()
		return nil, err
	}
	settings, err := readMaintenanceSettings(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := validateMaintenanceJournalMode(settings.JournalMode); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := configureWritableConnection(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Maintenance prepares the same statements as an indexing open, so an
	// interrupted bulk load has to be repaired before any of them.
	if err := repairDeferredIndexes(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	queries, err := sqlcgen.Prepare(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare graph maintenance queries: %w", err)
	}
	variableLimit, err := sqlitedriver.VariableLimit(ctx, db)
	if err != nil {
		_ = queries.Close()
		_ = db.Close()
		return nil, fmt.Errorf("read SQLite variable limit: %w", err)
	}
	return &Repository{db: db, queries: queries, path: path, limits: batchLimits{
		MaxRows: defaultBatchRows, MaxVariables: variableLimit, MaxBytes: defaultBatchBytes,
	}}, nil
}

// Metrics returns checked SQLite page and freelist metrics.
func (r *Repository) Metrics(ctx context.Context) (StorageMetrics, error) {
	return readStorageMetrics(ctx, r.db)
}

// Metadata lists every metadata key deterministically with a defensive bound.
func (r *Repository) Metadata(ctx context.Context) ([]MetadataEntry, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT key, value FROM meta ORDER BY key LIMIT ?", maxMetadataEntries+1)
	if err != nil {
		return nil, fmt.Errorf("list index metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]MetadataEntry, 0)
	for rows.Next() {
		var entry MetadataEntry
		if err := rows.Scan(&entry.Key, &entry.Value); err != nil {
			return nil, fmt.Errorf("list index metadata: %w", err)
		}
		entries = append(entries, entry)
		if len(entries) > maxMetadataEntries {
			return nil, fmt.Errorf("index metadata exceeds %d entries", maxMetadataEntries)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list index metadata: %w", err)
	}
	return entries, nil
}

// Compact rewrites the index at TargetPageSize and proves compatibility,
// metadata, graph counts, integrity, and connection settings are unchanged. An
// index already at that page size is compacted by a strict in-place VACUUM.
func (r *Repository) Compact(ctx context.Context) (CompactionResult, error) {
	var result CompactionResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	pending, err := r.ReconciliationPending(ctx)
	if err != nil {
		return result, fmt.Errorf("read reconciliation status before compaction: %w", err)
	}
	if pending {
		return result, fmt.Errorf("index reconciliation is pending; run 'grafo index' before compaction")
	}
	metadataBefore, err := r.Metadata(ctx)
	if err != nil {
		return result, err
	}
	countsBefore, err := r.Counts(ctx)
	if err != nil {
		return result, fmt.Errorf("count graph before compaction: %w", err)
	}
	settingsBefore, err := readMaintenanceSettings(ctx, r.db)
	if err != nil {
		return result, err
	}
	result.Before, err = readStorageMetrics(ctx, r.db)
	if err != nil {
		return result, err
	}
	if err := strictCheckpoint(ctx, r.db); err != nil {
		return result, err
	}
	if result.Before.PageSize == TargetPageSize {
		if _, err := r.db.ExecContext(ctx, "VACUUM"); err != nil {
			return result, fmt.Errorf("vacuum SQLite index: %w", err)
		}
	} else if err := r.upgradePageSize(ctx); err != nil {
		return result, err
	}
	if err := strictCheckpoint(ctx, r.db); err != nil {
		return result, fmt.Errorf("checkpoint compacted index: %w", err)
	}
	if err := integrityCheck(ctx, r.db); err != nil {
		return result, err
	}
	if err := validateReadCompatibility(ctx, r); err != nil {
		return result, fmt.Errorf("validate compacted index compatibility: %w", err)
	}
	metadataAfter, err := r.Metadata(ctx)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(metadataBefore, metadataAfter) {
		return result, fmt.Errorf("compaction changed index metadata")
	}
	countsAfter, err := r.Counts(ctx)
	if err != nil {
		return result, fmt.Errorf("count graph after compaction: %w", err)
	}
	if !reflect.DeepEqual(countsBefore, countsAfter) {
		return result, fmt.Errorf("compaction changed graph counts: before=%v after=%v", countsBefore, countsAfter)
	}
	settingsAfter, err := readMaintenanceSettings(ctx, r.db)
	if err != nil {
		return result, err
	}
	if settingsBefore != settingsAfter {
		return result, fmt.Errorf("compaction changed SQLite settings: before=%v after=%v", settingsBefore, settingsAfter)
	}
	result.After, err = readStorageMetrics(ctx, r.db)
	if err != nil {
		return result, err
	}
	if result.After.PageSize != TargetPageSize {
		return result, fmt.Errorf("compacted index page size is %d, want %d", result.After.PageSize, TargetPageSize)
	}
	result.Counts = countsAfter
	return result, nil
}

// upgradePageSize rebuilds the index at TargetPageSize. SQLite honors
// PRAGMA page_size only for an empty database or across a VACUUM, and ignores
// it entirely for an in-place VACUUM of a database in WAL mode, so the rewrite
// goes to a sibling file that VACUUM INTO creates at the requested page size
// and that then replaces the original.
//
// The sibling is proved intact and switched to WAL before the rename, and the
// rename itself is atomic, so an interruption at any point leaves a readable
// WAL index in place: either the untouched original or the finished rewrite.
func (r *Repository) upgradePageSize(ctx context.Context) error {
	replacement, err := r.writeReplacementAtTargetPageSize(ctx)
	if err != nil {
		return err
	}
	// The original has to be closed before it is replaced, and its checkpointed
	// sidecars have to go with it: a stale -wal or -shm left beside the
	// replacement would describe the file the rename is about to unlink.
	if err := errors.Join(r.queries.Close(), r.db.Close()); err != nil {
		_ = removeIndexFiles(replacement)
		return fmt.Errorf("close the index being upgraded: %w", err)
	}
	if err := removeIndexSidecars(r.path); err != nil {
		_ = removeIndexFiles(replacement)
		return err
	}
	if err := os.Rename(replacement, r.path); err != nil {
		_ = removeIndexFiles(replacement)
		return fmt.Errorf("replace the index with its rewrite: %w", err)
	}
	upgraded, err := openMaintenanceRepository(ctx, r.path)
	if err != nil {
		return fmt.Errorf("reopen the upgraded index: %w", err)
	}
	r.db, r.queries, r.limits = upgraded.db, upgraded.queries, upgraded.limits
	return nil
}

// writeReplacementAtTargetPageSize copies the open index into a sibling file at
// TargetPageSize and returns that file, already verified and in WAL mode. The
// original is only read, so a failure here leaves nothing to undo but the
// sibling itself.
func (r *Repository) writeReplacementAtTargetPageSize(ctx context.Context) (string, error) {
	replacement := ReplacementIndexPath(r.path)
	// VACUUM INTO refuses to write a file that already exists, so an abandoned
	// earlier attempt has to go first.
	if err := removeIndexFiles(replacement); err != nil {
		return "", err
	}
	if _, err := r.db.ExecContext(ctx, fmt.Sprintf("PRAGMA page_size=%d", TargetPageSize)); err != nil {
		return "", fmt.Errorf("request the target index page size: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, "VACUUM INTO ?", replacement); err != nil {
		_ = removeIndexFiles(replacement)
		return "", fmt.Errorf("rewrite index at the target page size: %w", err)
	}
	if err := prepareReplacementIndex(ctx, replacement); err != nil {
		_ = removeIndexFiles(replacement)
		return "", err
	}
	// Closing the replacement drops the sidecars its WAL switch created, so this
	// only proves the rename has nothing left to carry across.
	if err := removeIndexSidecars(replacement); err != nil {
		_ = removeIndexFiles(replacement)
		return "", err
	}
	return replacement, nil
}

// prepareReplacementIndex proves a rewritten index carries the target page size
// and an intact, compatible graph, then persists WAL journal mode in its header
// and closes it cleanly so it has no sidecars of its own to carry across the
// rename.
func prepareReplacementIndex(ctx context.Context, path string) (resultErr error) {
	db, err := openExistingIndex(ctx, path, "rw")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	var pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return fmt.Errorf("read rewritten index page size: %w", err)
	}
	if pageSize != TargetPageSize {
		return fmt.Errorf("rewritten index page size is %d, want %d", pageSize, TargetPageSize)
	}
	if err := integrityCheck(ctx, db); err != nil {
		return err
	}
	if err := validateReadCompatibility(ctx, &Repository{db: db, queries: sqlcgen.New(db), path: path}); err != nil {
		return fmt.Errorf("validate rewritten index compatibility: %w", err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("enable WAL on the rewritten index: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("rewritten index journal mode is %q, want WAL", mode)
	}
	return nil
}

// ReplacementIndexPath names the sibling a page size upgrade rewrites an index
// into. An interrupted upgrade can leave the file behind, so index lifecycle
// orchestration has to know about it as well.
func ReplacementIndexPath(path string) string { return path + ".rewrite" }

func removeIndexFiles(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", filepath.Base(path), err)
	}
	return removeIndexSidecars(path)
}

func removeIndexSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", filepath.Base(sidecar), err)
		}
	}
	return nil
}

func readStorageMetrics(ctx context.Context, db *sql.DB) (StorageMetrics, error) {
	var pageSize, pageCount, freelistCount int64
	for _, query := range []struct {
		name   string
		value  *int64
		pragma string
	}{
		{name: "page_size", value: &pageSize, pragma: "PRAGMA page_size"},
		{name: "page_count", value: &pageCount, pragma: "PRAGMA page_count"},
		{name: "freelist_count", value: &freelistCount, pragma: "PRAGMA freelist_count"},
	} {
		if err := db.QueryRowContext(ctx, query.pragma).Scan(query.value); err != nil {
			return StorageMetrics{}, fmt.Errorf("read SQLite %s: %w", query.name, err)
		}
	}
	return calculateStorageMetrics(pageSize, pageCount, freelistCount)
}

func calculateStorageMetrics(pageSize, pageCount, freelistCount int64) (StorageMetrics, error) {
	if pageSize <= 0 || pageCount < 0 || freelistCount < 0 || freelistCount > pageCount {
		return StorageMetrics{}, fmt.Errorf("invalid SQLite page metrics: size=%d pages=%d freelist=%d", pageSize, pageCount, freelistCount)
	}
	if pageCount > math.MaxInt64/pageSize || freelistCount > math.MaxInt64/pageSize {
		return StorageMetrics{}, fmt.Errorf("SQLite page metrics overflow")
	}
	reclaimable := freelistCount * pageSize
	live := (pageCount - freelistCount) * pageSize
	percent := float64(0)
	if pageCount > 0 {
		percent = float64(freelistCount) * 100 / float64(pageCount)
	}
	return StorageMetrics{PageSize: pageSize, PageCount: pageCount, FreelistCount: freelistCount,
		LiveAllocatedBytes: live, ReclaimableBytes: reclaimable, ReclaimablePercent: percent}, nil
}

type maintenanceSettings struct {
	JournalMode string
	Synchronous int
}

func validateMaintenanceJournalMode(mode string) error {
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("current index journal mode is %q, want WAL; run 'grafo index' before compaction", mode)
	}
	return nil
}

func readMaintenanceSettings(ctx context.Context, db *sql.DB) (maintenanceSettings, error) {
	var settings maintenanceSettings
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&settings.JournalMode); err != nil {
		return settings, fmt.Errorf("read SQLite journal mode: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&settings.Synchronous); err != nil {
		return settings, fmt.Errorf("read SQLite synchronous setting: %w", err)
	}
	return settings, nil
}

func strictCheckpoint(ctx context.Context, db *sql.DB) error {
	var busy, logFrames, checkpointedFrames int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return fmt.Errorf("checkpoint SQLite WAL before compaction: %w", err)
	}
	if busy != 0 || checkpointedFrames < logFrames {
		return fmt.Errorf("checkpoint SQLite WAL before compaction was incomplete: busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointedFrames)
	}
	return nil
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("check compacted index integrity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	results := make([]string, 0, 1)
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("check compacted index integrity: %w", err)
		}
		results = append(results, result)
		if len(results) > 100 {
			return fmt.Errorf("check compacted index integrity returned too many errors")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check compacted index integrity: %w", err)
	}
	if len(results) != 1 || strings.ToLower(strings.TrimSpace(results[0])) != "ok" {
		return fmt.Errorf("compacted index failed integrity check: %s", strings.Join(results, "; "))
	}
	return nil
}
