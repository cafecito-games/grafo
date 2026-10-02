package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
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
	variableLimit, err := activeVariableLimit(ctx, db)
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

// Compact performs a strict in-place VACUUM and proves compatibility, metadata,
// graph counts, integrity, and connection settings are unchanged.
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
	if _, err := r.db.ExecContext(ctx, "VACUUM"); err != nil {
		return result, fmt.Errorf("vacuum SQLite index: %w", err)
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
	result.Counts = countsAfter
	return result, nil
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
