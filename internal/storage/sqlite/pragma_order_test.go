package sqlite

import (
	"context"
	"database/sql"
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
