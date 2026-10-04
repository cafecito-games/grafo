package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"github.com/pressly/goose/v3"
)

// rowidQueueSchemaVersion is the last storage version that kept the
// reconciliation queue in a rowid table, where fact_id cost one b-tree as the
// table's key and a second as sqlite_autoindex_dirty_facts_1.
const rowidQueueSchemaVersion = 11

func openIndexAtVersion(t *testing.T, path string, version int64, statements string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(context.Background(), version); err != nil {
		t.Fatal(err)
	}
	if statements != "" {
		if _, err := database.ExecContext(context.Background(), statements); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func scanString(t *testing.T, path, query string) string {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var value string
	if err := database.QueryRowContext(context.Background(), query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// TestMigrationPreservesPendingReconciliationQueue covers the one way rebuilding
// this table can lose work. Reconciliation is resumable, so an index can be
// migrated while the queue still holds the facts an interrupted run owed, and
// ReconciliationPending reads exactly those rows to decide whether work is due.
// A migration that recreated the table empty would report nothing pending and
// leave edges unreconciled against facts that no longer match.
func TestMigrationPreservesPendingReconciliationQueue(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "pending-queue.sqlite")
	openIndexAtVersion(t, path, rowidQueueSchemaVersion, `
INSERT INTO paths(id, path) VALUES (1, 'owner.go'), (2, 'other.go');
INSERT INTO nodes(id, kind, name, qualified_name, path, line, owner_file) VALUES
    ('caller', 'function', 'Caller', 'legacy.Caller', 'owner.go', 2, 'owner.go');
INSERT INTO facts(id, from_id, kind, producer, target_id, path_id, line, column_no, end_line, properties, owner_path_id)
    VALUES
    ('fact-a', 'caller', 'calls', 'go', '', 1, 5, 7, 5, '{}', 1),
    ('fact-b', 'caller', 'calls', 'go', '', 2, 6, 7, 6, '{}', 2);
INSERT INTO dirty_facts(fact_id, owner_file) VALUES
    ('fact-a', 'owner.go'),
    ('fact-b', 'other.go');`)

	// Opening the repository runs the remaining migrations, which is where the
	// queue is rebuilt WITHOUT ROWID.
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	if queued := countRows(t, path, "SELECT COUNT(*) FROM dirty_facts"); queued != 2 {
		t.Fatalf("the migrated queue holds %d facts, want the 2 it was carrying", queued)
	}
	// The owner is what batch ordering and the resolution caches' locality depend
	// on, so the payload column has to survive the rebuild, not just the key.
	if owner := scanString(t, path,
		"SELECT owner_file FROM dirty_facts WHERE fact_id = grafo_identity_blob('fact-b')"); owner != "other.go" {
		t.Errorf("fact-b's owner is %q after the rebuild, want %q", owner, "other.go")
	}
}

func TestDirtyFactsQueueIsStoredWithoutRowid(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "queue-shape.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	definition := scanString(t, path, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'dirty_facts'")
	if !strings.Contains(strings.ToUpper(definition), "WITHOUT ROWID") {
		t.Errorf("dirty_facts is a rowid table, so fact_id is stored twice per row:\n%s", definition)
	}

	// The implicit index is the thing WITHOUT ROWID removes, and its absence is a
	// sharper assertion than the DDL: a rowid table with a TEXT PRIMARY KEY always
	// has one, so a future migration cannot quietly reintroduce the second b-tree
	// without failing here.
	autoIndexes := countRows(t, path,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'dirty_facts' AND name LIKE 'sqlite_autoindex%'")
	if autoIndexes != 0 {
		t.Errorf("dirty_facts carries %d implicit key indexes, want none", autoIndexes)
	}

	// dirty_facts_order is named in INDEXED BY clauses, and SQLite refuses to
	// prepare a statement whose named index is missing rather than planning around
	// it, so losing it in the rebuild would break the queue outright.
	ordering := countRows(t, path,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'dirty_facts_order'")
	if ordering != 1 {
		t.Errorf("dirty_facts_order is missing after the rebuild, which the queue's queries name explicitly")
	}
}
