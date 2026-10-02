package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestWritableConnectionSetsBusyTimeoutBeforeBlockingPragmas pins the ordering
// itself. busy_timeout is connection-local policy that cannot block, while
// journal_mode=WAL needs the database lock, so any pragma that can block has
// to come after the timeout is in force.
func TestWritableConnectionSetsBusyTimeoutBeforeBlockingPragmas(t *testing.T) {
	if len(writableConnectionPragmas) == 0 {
		t.Fatal("writable connection configures no pragmas")
	}
	if !strings.Contains(writableConnectionPragmas[0], "busy_timeout") {
		t.Fatalf("first writable pragma = %q, want busy_timeout", writableConnectionPragmas[0])
	}
	for _, pragma := range writableConnectionPragmas[1:] {
		if strings.Contains(pragma, "busy_timeout") {
			t.Fatalf("busy_timeout is applied twice: %#v", writableConnectionPragmas)
		}
	}
}

// TestConfigureWritableConnectionWaitsOutContendedJournalMode covers the
// behavior the ordering buys: switching an existing rollback-journal database
// to WAL needs the exclusive database lock, and while busy_timeout was still
// at SQLite's default of zero the pragma failed immediately with "database is
// locked" instead of waiting for the competing connection to let go.
func TestConfigureWritableConnectionWaitsOutContendedJournalMode(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.db")

	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open competing connection: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	blocker.SetMaxOpenConns(1)
	if _, err := blocker.ExecContext(ctx, "CREATE TABLE contended(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("seed rollback-journal database: %v", err)
	}
	transaction, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin competing transaction: %v", err)
	}
	var rows int
	if err := transaction.QueryRowContext(ctx, "SELECT count(*) FROM contended").Scan(&rows); err != nil {
		t.Fatalf("take the competing database lock: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		released <- transaction.Commit()
	}()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writable connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := configureWritableConnection(ctx, db); err != nil {
		t.Fatalf("configure writable connection under contention: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatalf("competing write commit: %v", err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal mode = %q, want wal", mode)
	}
}

// TestWritableConnectionSetsPageSizeBeforeJournalMode pins the second ordering
// constraint: SQLite ignores page_size once a database is in WAL mode, so a
// fresh index would silently keep the default page size if the pragmas were
// applied the other way round.
func TestWritableConnectionSetsPageSizeBeforeJournalMode(t *testing.T) {
	pageSizeIndex, journalModeIndex := -1, -1
	for index, pragma := range writableConnectionPragmas {
		switch {
		case strings.Contains(pragma, "page_size"):
			pageSizeIndex = index
		case strings.Contains(pragma, "journal_mode"):
			journalModeIndex = index
		}
	}
	if pageSizeIndex < 0 {
		t.Fatalf("writable pragmas set no page size: %#v", writableConnectionPragmas)
	}
	if journalModeIndex < 0 {
		t.Fatalf("writable pragmas set no journal mode: %#v", writableConnectionPragmas)
	}
	if pageSizeIndex > journalModeIndex {
		t.Fatalf("page_size is applied after journal_mode: %#v", writableConnectionPragmas)
	}
}

// TestOpenAppliesPageSizeAndPageCache covers the effect rather than the order:
// a newly created index has to come out with the larger page size and the
// raised cache ceiling, because random secondary-index writes are what the two
// settings exist to absorb.
func TestOpenAppliesPageSizeAndPageCache(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.db"))
	if err != nil {
		t.Fatalf("open graph: %v", err)
	}
	defer func() { _ = repository.Close() }()

	var pageSize int
	if err := repository.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatalf("read page size: %v", err)
	}
	if pageSize != indexPageSize {
		t.Fatalf("page size = %d, want %d", pageSize, indexPageSize)
	}
	var cacheSize int
	if err := repository.db.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cacheSize); err != nil {
		t.Fatalf("read cache size: %v", err)
	}
	if cacheSize != -pageCacheKiB {
		t.Fatalf("cache size = %d, want %d", cacheSize, -pageCacheKiB)
	}
}

// TestOpenKeepsStatementJournalsInMemory pins temp_store. SQLite journals the
// pages an upsert may have to undo, and every batched node and fact write is an
// upsert, so letting that journal reach the filesystem turns each one into a
// four-byte write plus a page write that is discarded immediately afterwards.
func TestOpenKeepsStatementJournalsInMemory(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.db"))
	if err != nil {
		t.Fatalf("open graph: %v", err)
	}
	defer func() { _ = repository.Close() }()

	// 2 is SQLite's encoding of temp_store=MEMORY.
	var temporaryStore int
	if err := repository.db.QueryRowContext(ctx, "PRAGMA temp_store").Scan(&temporaryStore); err != nil {
		t.Fatalf("read temp store: %v", err)
	}
	if temporaryStore != 2 {
		t.Fatalf("temp_store = %d, want 2 (memory)", temporaryStore)
	}
}

// TestWritableConnectionLimitsTheRetainedLogAfterJournalMode pins the third
// ordering constraint: journal_size_limit describes the write-ahead log, so it
// has no log to apply to until the database is in WAL mode.
func TestWritableConnectionLimitsTheRetainedLogAfterJournalMode(t *testing.T) {
	journalModeIndex, limitIndex := -1, -1
	for index, pragma := range writableConnectionPragmas {
		switch {
		case strings.Contains(pragma, "journal_size_limit"):
			limitIndex = index
		case strings.Contains(pragma, "journal_mode"):
			journalModeIndex = index
		}
	}
	if limitIndex < 0 {
		t.Fatalf("writable pragmas set no journal size limit: %#v", writableConnectionPragmas)
	}
	if limitIndex < journalModeIndex {
		t.Fatalf("journal_size_limit is applied before journal_mode: %#v", writableConnectionPragmas)
	}
}

// TestCheckpointHandsBackTheLogAnOversizedTransactionGrew covers the behavior the
// limit exists for. Nothing can checkpoint while a transaction is open, so one
// large transaction forces the log to hold all of its pages; without a limit
// SQLite then reuses that allocation in place and the file keeps its peak for the
// rest of the process even though every frame has been copied out of it. The
// space comes back when the log restarts, which is the first write after a
// checkpoint has copied everything, so the write below stands for the next
// reconciliation batch.
func TestCheckpointHandsBackTheLogAnOversizedTransactionGrew(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.db")
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open graph: %v", err)
	}
	defer func() { _ = repository.Close() }()

	var configured int64
	if err := repository.db.QueryRowContext(ctx, "PRAGMA journal_size_limit").Scan(&configured); err != nil {
		t.Fatalf("read journal size limit: %v", err)
	}
	if want := int64(retainedLogBytes); configured != want {
		t.Fatalf("journal size limit = %d, want %d", configured, want)
	}
	// The production cap is larger than a transaction this test can afford to
	// write, so the mechanism is exercised at a size it can: the limit is the
	// same pragma either way.
	const limit = 16 << 20
	if _, err := repository.db.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_size_limit=%d", limit)); err != nil {
		t.Fatalf("lower the journal size limit: %v", err)
	}

	// One transaction wide enough to push the log well past the limit.
	transaction, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.ExecContext(ctx, "CREATE TABLE wide(id TEXT PRIMARY KEY, filler TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("x", 1024)
	for row := range 50_000 {
		if _, err := transaction.ExecContext(ctx, "INSERT INTO wide(id, filler) VALUES(?, ?)",
			fmt.Sprintf("%064d", row), filler); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	peak := logSize(t, path)
	if peak <= limit {
		t.Fatalf("log peaked at %d bytes, which the %d byte limit already covers: the fixture proves nothing", peak, limit)
	}
	if err := repository.checkpoint(ctx, false); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := repository.db.ExecContext(ctx, "INSERT INTO wide(id, filler) VALUES('next', 'x')"); err != nil {
		t.Fatalf("restart the log: %v", err)
	}
	if retained := logSize(t, path); retained > limit {
		t.Fatalf("log retained %d bytes after a checkpoint, want at most %d (peak was %d)", retained, limit, peak)
	}
}

func logSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat write-ahead log: %v", err)
	}
	return info.Size()
}
