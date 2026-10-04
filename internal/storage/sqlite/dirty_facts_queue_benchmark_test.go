package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// BenchmarkDirtyFactsQueueWrite isolates the single widest write in an indexing
// run: enqueueing every fact a reconciliation pass has to resolve.
//
// It exists because a full cold index cannot answer the question this change
// raises. WITHOUT ROWID removes one b-tree, which is why it writes less, but it
// also moves owner_file into the tree keyed by the content hash, and inserts into
// that tree are in random page order. A rowid table paid a sequential append for
// the row and a narrow random insert for the key; a WITHOUT ROWID table pays one
// wider random insert. Whether removing a tree or widening the random one
// dominates is a b-tree question, and a 2-minute index measures it behind parse,
// persistence and resolution on a machine other work is also using.
//
// The rows are shaped like the real queue in the two ways that decide the answer:
// fact ids are content hashes, so they arrive in random page order, and they are
// inserted grouped by owner, which is the order EnqueueDirtyFacts produces.
func BenchmarkDirtyFactsQueueWrite(b *testing.B) {
	const (
		rowCount      = 200_000
		factsPerOwner = 190 // 2.4M facts over 12.7k files on the reference corpus
	)
	type row struct{ factID, ownerFile string }
	rows := make([]row, rowCount)
	for index := range rowCount {
		// A content hash, like graph.StableID: the identity carries no locality, so
		// the insert lands on an unpredictable page.
		sum := sha256.Sum256(fmt.Appendf(nil, "fact-%d", index))
		rows[index] = row{
			factID:    "f:" + hex.EncodeToString(sum[:])[:20],
			ownerFile: fmt.Sprintf("internal/generated/package%04d/file%02d.go", index/factsPerOwner, index%7),
		}
	}

	schemas := map[string]string{
		"rowid": `CREATE TABLE dirty_facts (
    fact_id TEXT PRIMARY KEY,
    owner_file TEXT NOT NULL
);`,
		"without_rowid": `CREATE TABLE dirty_facts (
    fact_id TEXT PRIMARY KEY,
    owner_file TEXT NOT NULL
) WITHOUT ROWID;`,
	}

	for _, variant := range []string{"rowid", "without_rowid"} {
		b.Run(variant, func(b *testing.B) {
			ctx := context.Background()
			var logBytes, databaseBytes int64
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				path := filepath.Join(testtemp.Dir(b), "queue.sqlite")
				database := openQueueBenchmarkDatabase(b, path, schemas[variant])
				b.StartTimer()

				// One transaction, which is what the real enqueue is: nothing can
				// checkpoint while it is open, so every page it dirties is held in the
				// write-ahead log until it commits.
				transaction, err := database.BeginTx(ctx, nil)
				if err != nil {
					b.Fatal(err)
				}
				statement, err := transaction.PrepareContext(ctx,
					"INSERT OR IGNORE INTO dirty_facts(fact_id, owner_file) VALUES (?, ?)")
				if err != nil {
					b.Fatal(err)
				}
				for _, item := range rows {
					if _, err := statement.ExecContext(ctx, item.factID, item.ownerFile); err != nil {
						b.Fatal(err)
					}
				}
				if err := statement.Close(); err != nil {
					b.Fatal(err)
				}
				if err := transaction.Commit(); err != nil {
					b.Fatal(err)
				}

				b.StopTimer()
				// The log is measured before the connection closes, because closing
				// checkpoints and truncates it.
				if info, err := os.Stat(path + "-wal"); err == nil {
					logBytes = info.Size()
				}
				if info, err := os.Stat(path); err == nil {
					databaseBytes = info.Size()
				}
				if err := database.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.StopTimer()
			b.ReportMetric(float64(logBytes)/(1<<20), "wal_MiB")
			b.ReportMetric(float64(databaseBytes)/(1<<20), "db_MiB")
			b.ReportMetric(rowCount, "rows/op")
		})
	}
}

// openQueueBenchmarkDatabase builds a database carrying one queue schema under the
// pragmas a writable index uses, since page size and journal mode are what make
// the measurement about the same b-tree behavior the indexer sees.
func openQueueBenchmarkDatabase(b *testing.B, path, schema string) *sql.DB {
	b.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	// One connection, so the pragmas below apply to the connection the benchmark
	// writes through rather than to whichever one the pool hands out.
	database.SetMaxOpenConns(1)
	for _, pragma := range []string{
		fmt.Sprintf("PRAGMA page_size=%d", TargetPageSize),
		fmt.Sprintf("PRAGMA cache_size=-%d", pageCacheKiB),
		temporaryStoreMemory,
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := database.ExecContext(context.Background(), pragma); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := database.ExecContext(context.Background(), schema); err != nil {
		b.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(),
		"CREATE INDEX dirty_facts_order ON dirty_facts(owner_file, fact_id)"); err != nil {
		b.Fatal(err)
	}
	return database
}
