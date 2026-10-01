package sqlite

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
