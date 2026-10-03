package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestCalculateStorageMetricsChecksArithmeticAndPercent(t *testing.T) {
	metrics, err := calculateStorageMetrics(4096, 100, 25)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.LiveAllocatedBytes != 307200 || metrics.ReclaimableBytes != 102400 || metrics.ReclaimablePercent != 25 {
		t.Fatalf("metrics = %#v", metrics)
	}
	for _, input := range [][3]int64{{0, 1, 0}, {4096, 1, 2}, {2, math.MaxInt64, 1}} {
		if _, err := calculateStorageMetrics(input[0], input[1], input[2]); err == nil {
			t.Fatalf("calculateStorageMetrics%v accepted invalid/overflow input", input)
		}
	}
}

func TestCompactShrinksFreelistAndPreservesMetadataCountsAndSettings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "compact.sqlite")
	repository := openMaintenanceFixture(t, path)
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, "opaque_future_key", "opaque-value"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.ExecContext(ctx, "CREATE TABLE maintenance_fixture(id INTEGER PRIMARY KEY, payload BLOB)"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x < 1024) INSERT INTO maintenance_fixture(payload) SELECT zeroblob(16384) FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.ExecContext(ctx, "DELETE FROM maintenance_fixture"); err != nil {
		t.Fatal(err)
	}
	beforeMetadata, err := repository.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeCounts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeSettings, err := readMaintenanceSettings(ctx, repository.db)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	result, err := repository.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterMetadata, _ := repository.Metadata(ctx)
	afterCounts, _ := repository.Counts(ctx)
	afterSettings, _ := readMaintenanceSettings(ctx, repository.db)
	afterStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Before.ReclaimableBytes < 8<<20 || result.After.ReclaimableBytes >= result.Before.ReclaimableBytes {
		t.Fatalf("compaction metrics = %#v", result)
	}
	if afterStat.Size() >= beforeStat.Size()/2 {
		t.Fatalf("primary did not shrink materially: before=%d after=%d", beforeStat.Size(), afterStat.Size())
	}
	if !reflect.DeepEqual(beforeMetadata, afterMetadata) || !reflect.DeepEqual(beforeCounts, afterCounts) || !reflect.DeepEqual(beforeSettings, afterSettings) {
		t.Fatalf("preservation mismatch: metadata=%t counts=%t settings=%t",
			reflect.DeepEqual(beforeMetadata, afterMetadata), reflect.DeepEqual(beforeCounts, afterCounts), reflect.DeepEqual(beforeSettings, afterSettings))
	}
	if result.Counts.Files != beforeCounts.Files || result.Counts.Nodes != beforeCounts.Nodes {
		t.Fatalf("reported counts = %#v, want %#v", result.Counts, beforeCounts)
	}
	if value, err := repository.Meta(ctx, "opaque_future_key"); err != nil || value != "opaque-value" {
		t.Fatalf("opaque metadata = %q, %v", value, err)
	}
}

func TestCompactRefusesPendingReconciliationAndCancellation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "pending.sqlite")
	repository := openMaintenanceFixture(t, path)
	defer func() { _ = repository.Close() }()
	if _, err := repository.db.ExecContext(ctx, "INSERT INTO dirty_owners(owner_file) VALUES ('pending.go')"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Compact(ctx); err == nil || !strings.Contains(err.Error(), "reconciliation is pending") {
		t.Fatalf("pending compaction error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.Compact(canceled); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("canceled compaction error = %v", err)
	}
}

func TestStrictCheckpointRejectsAnIncompleteBusyCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "busy.sqlite")
	repository := openMaintenanceFixture(t, path)
	defer func() { _ = repository.Close() }()
	reader, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	transaction, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transaction.Rollback() }()
	var value string
	if err := transaction.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema_version'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "checkpoint_busy_fixture", "new WAL evidence"); err != nil {
		t.Fatal(err)
	}
	if err := strictCheckpoint(ctx, repository.db); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("busy checkpoint error = %v", err)
	}
}

func TestCompactCheckpointFailurePreservesIntegrityAndEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "busy-compact.sqlite")
	repository := openMaintenanceFixture(t, path)
	if err := repository.SetMeta(ctx, "opaque_future_key", "preserved"); err != nil {
		t.Fatal(err)
	}
	countsBefore, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := transaction.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema_version'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "checkpoint_busy_fixture", "new WAL evidence"); err != nil {
		t.Fatal(err)
	}
	metadataBefore, err := repository.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Compact(ctx); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("busy compaction error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenMaintenance(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	maintained := reopened.(*Repository)
	defer func() { _ = maintained.Close() }()
	metadataAfter, err := maintained.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	countsAfter, err := maintained.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(metadataBefore, metadataAfter) || !reflect.DeepEqual(countsBefore, countsAfter) {
		t.Fatalf("failed compaction changed evidence: metadata=%t counts=%t",
			reflect.DeepEqual(metadataBefore, metadataAfter), reflect.DeepEqual(countsBefore, countsAfter))
	}
	if err := integrityCheck(ctx, maintained.db); err != nil {
		t.Fatalf("failed compaction damaged index: %v", err)
	}
}

func TestOpenMaintenanceDoesNotCreateOrMigrate(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(testtemp.Dir(t), "missing.sqlite")
	if _, err := OpenMaintenance(ctx, missing); err == nil {
		t.Fatal("maintenance opener created a missing index")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing index was created: %v", err)
	}
	bare := filepath.Join(testtemp.Dir(t), "bare.sqlite")
	if err := os.WriteFile(bare, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenMaintenance(ctx, bare); err == nil {
		t.Fatal("maintenance opener migrated a bare database")
	}
	if info, err := os.Stat(bare); err != nil || info.Size() != 0 {
		t.Fatalf("bare database changed: size=%v err=%v", info, err)
	}
}

func TestOpenMaintenanceUsesNormalWritableSettings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "settings.sqlite")
	repository := openMaintenanceFixture(t, path)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenMaintenance(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	opened := maintenance.(*Repository)
	defer func() { _ = opened.Close() }()
	settings, err := readMaintenanceSettings(ctx, opened.db)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(settings.JournalMode) != "wal" || settings.Synchronous != 1 {
		t.Fatalf("maintenance settings = %#v, want WAL/NORMAL", settings)
	}
}

func TestOpenMaintenanceRefusesNonWALWithoutChangingJournalMode(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "delete-mode.sqlite")
	repository := openMaintenanceFixture(t, path)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(mode) != "delete" {
		t.Fatalf("fixture journal mode = %q, want delete", mode)
	}

	if _, err := OpenMaintenance(ctx, path); err == nil || !strings.Contains(strings.ToLower(err.Error()), "journal mode") {
		t.Fatalf("non-WAL maintenance error = %v", err)
	}
	database, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(mode) != "delete" {
		t.Fatalf("refused maintenance changed journal mode to %q", mode)
	}
}

func openMaintenanceFixture(t *testing.T, path string) *Repository {
	t.Helper()
	ctx := context.Background()
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	return repository
}

func TestCompactUpgradesALegacyPageSizeAndPreservesEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "legacy.sqlite")
	openLegacyPageSizeFixture(t, path)
	maintenance, err := OpenMaintenance(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repository := maintenance.(*Repository)
	defer func() { _ = repository.Close() }()
	metadataBefore, err := repository.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	countsBefore, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	result, err := repository.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Before.PageSize != legacyPageSize || result.After.PageSize != TargetPageSize {
		t.Fatalf("page size went from %d to %d, want %d to %d",
			result.Before.PageSize, result.After.PageSize, legacyPageSize, TargetPageSize)
	}
	metadataAfter, err := repository.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	countsAfter, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(metadataBefore, metadataAfter) || !reflect.DeepEqual(countsBefore, countsAfter) {
		t.Fatalf("upgrade changed evidence: metadata=%t counts=%t",
			reflect.DeepEqual(metadataBefore, metadataAfter), reflect.DeepEqual(countsBefore, countsAfter))
	}
	if err := integrityCheck(ctx, repository.db); err != nil {
		t.Fatalf("upgraded index failed its integrity check: %v", err)
	}
	settings, err := readMaintenanceSettings(ctx, repository.db)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(settings.JournalMode) != "wal" {
		t.Fatalf("upgraded index journal mode = %q, want WAL", settings.JournalMode)
	}
	if value, err := repository.Meta(ctx, "opaque_future_key"); err != nil || value != "opaque-value" {
		t.Fatalf("opaque metadata = %q, %v", value, err)
	}
	assertNoReplacementLeftBehind(t, path)
}

func TestCompactLeavesAnIndexAlreadyAtTheTargetPageSizeInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "current.sqlite")
	repository := openMaintenanceFixture(t, path)
	defer func() { _ = repository.Close() }()
	if err := repository.SetMeta(ctx, "opaque_future_key", "opaque-value"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	result, err := repository.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Before.PageSize != TargetPageSize || result.After.PageSize != TargetPageSize {
		t.Fatalf("page size went from %d to %d, want %d throughout",
			result.Before.PageSize, result.After.PageSize, TargetPageSize)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("compaction replaced an index that was already at the target page size")
	}
	assertNoReplacementLeftBehind(t, path)
}

func TestPageSizeUpgradeIsReadableInWALModeOnEitherSideOfTheReplacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "interrupted.sqlite")
	openLegacyPageSizeFixture(t, path)
	maintenance, err := OpenMaintenance(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repository := maintenance.(*Repository)
	defer func() { _ = repository.Close() }()
	countsBefore, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	replacement, err := repository.writeReplacementAtTargetPageSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	// An interruption before the replacement is renamed into place leaves the
	// original exactly as maintenance found it.
	assertReadableWALIndex(t, path, legacyPageSize, countsBefore)
	// An interruption after it is renamed into place leaves that file, which is
	// already a WAL index at the target page size.
	assertReadableWALIndex(t, replacement, TargetPageSize, countsBefore)

	// A later compaction clears the abandoned replacement instead of failing on it.
	reopened, err := OpenMaintenance(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	retried := reopened.(*Repository)
	defer func() { _ = retried.Close() }()
	result, err := retried.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.After.PageSize != TargetPageSize {
		t.Fatalf("retried upgrade page size = %d, want %d", result.After.PageSize, TargetPageSize)
	}
	if !reflect.DeepEqual(result.Counts, countsBefore) {
		t.Fatalf("retried upgrade counts = %#v, want %#v", result.Counts, countsBefore)
	}
	assertNoReplacementLeftBehind(t, path)
}

const legacyPageSize = 4096

// openLegacyPageSizeFixture writes an index the way a version before the 16 KiB
// default would have left it: populated, in WAL mode, and at SQLite's old
// default page size.
func openLegacyPageSizeFixture(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	repository := openMaintenanceFixture(t, path)
	if err := repository.SetMeta(ctx, "opaque_future_key", "opaque-value"); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if _, err := repository.db.ExecContext(ctx, "CREATE TABLE legacy_fixture(id INTEGER PRIMARY KEY, payload BLOB)"); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if _, err := repository.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x < 512) INSERT INTO legacy_fixture(payload) SELECT zeroblob(4096) FROM n`); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	var mode string
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, fmt.Sprintf("PRAGMA page_size=%d", legacyPageSize)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "VACUUM"); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	var pageSize int64
	if err := database.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if pageSize != legacyPageSize || strings.ToLower(mode) != "wal" {
		t.Fatalf("legacy fixture is page_size=%d journal_mode=%q", pageSize, mode)
	}
}

func assertReadableWALIndex(t *testing.T, path string, wantPageSize int64, wantCounts graph.Counts) {
	t.Helper()
	ctx := context.Background()
	walMode, err := hasWALJournalHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if !walMode {
		t.Fatalf("%s does not carry a WAL journal header", filepath.Base(path))
	}
	maintenance, err := OpenMaintenance(ctx, path)
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	repository := maintenance.(*Repository)
	defer func() { _ = repository.Close() }()
	var pageSize int64
	if err := repository.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if pageSize != wantPageSize {
		t.Fatalf("%s page size = %d, want %d", filepath.Base(path), pageSize, wantPageSize)
	}
	if err := integrityCheck(ctx, repository.db); err != nil {
		t.Fatalf("%s failed its integrity check: %v", filepath.Base(path), err)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("%s counts = %#v, want %#v", filepath.Base(path), counts, wantCounts)
	}
}

func assertNoReplacementLeftBehind(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		leftover := ReplacementIndexPath(path) + suffix
		if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s was left behind: %v", filepath.Base(leftover), err)
		}
	}
}
