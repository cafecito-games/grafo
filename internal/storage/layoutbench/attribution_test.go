package layoutbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openAttributionDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layout.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := db.Exec(pragma); err != nil {
			t.Fatalf("configure %s: %v", pragma, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// churnDatabase creates table t and leaves behind allocated plus reusable pages
// so compaction and freelist assertions have real state to observe.
func churnDatabase(t *testing.T, db *sql.DB, rows, deletedRows, refillRows, payloadLength int) {
	t.Helper()
	if _, err := db.Exec("CREATE TABLE t(a TEXT PRIMARY KEY, b TEXT)"); err != nil {
		t.Fatalf("create table t: %v", err)
	}
	insertRows := func(start, count int) {
		t.Helper()
		for i := start; i < start+count; i++ {
			statement := fmt.Sprintf("INSERT INTO t(a, b) VALUES('%06d', '%s')", i, strings.Repeat("x", payloadLength))
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("insert row %d: %v", i, err)
			}
		}
	}
	insertRows(0, rows)
	if _, err := db.Exec(fmt.Sprintf("DELETE FROM t WHERE a <= '%06d'", deletedRows-1)); err != nil {
		t.Fatalf("delete rows: %v", err)
	}
	insertRows(rows, refillRows)
	if err := Checkpoint(context.Background(), db, true); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

func objectNames(attribution Attribution) []string {
	names := make([]string, 0, len(attribution.Objects))
	for _, object := range attribution.Objects {
		names = append(names, object.Name)
	}
	return names
}

func TestAttributionCaptureRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, path := openAttributionDatabase(t)
	churnDatabase(t, db, 100, 50, 50, 32)

	attribution, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("CaptureAttribution: %v", err)
	}

	if !attribution.DBStatSupported {
		t.Fatalf("dbstat unsupported: %s", attribution.DBStatReason)
	}
	if attribution.PageSize <= 0 || attribution.PageCount <= 0 {
		t.Fatalf("unexpected page metrics: page_size=%d page_count=%d", attribution.PageSize, attribution.PageCount)
	}
	if attribution.PrimaryBytes <= 0 {
		t.Fatalf("primary bytes not captured: %d", attribution.PrimaryBytes)
	}
	names := objectNames(attribution)
	hasTable, hasAutoindex := false, false
	for _, name := range names {
		if name == "t" {
			hasTable = true
		}
		if strings.HasPrefix(name, "sqlite_autoindex_") {
			hasAutoindex = true
		}
	}
	if !hasTable {
		t.Errorf("objects missing table t: %v", names)
	}
	if !hasAutoindex {
		t.Errorf("objects missing sqlite_autoindex entry: %v", names)
	}
	var objectBytes int64
	for _, object := range attribution.Objects {
		if object.Bytes <= 0 || object.Pages <= 0 {
			t.Errorf("object %s has non-positive metrics: bytes=%d pages=%d", object.Name, object.Bytes, object.Pages)
		}
		objectBytes += object.Bytes
	}
	if difference := objectBytes - attribution.PrimaryBytes; difference < -attribution.PageSize || difference > attribution.PageSize {
		t.Errorf("object bytes %d differ from primary bytes %d by more than page size %d", objectBytes, attribution.PrimaryBytes, attribution.PageSize)
	}
	if len(attribution.PayloadStats) != 0 {
		t.Errorf("payload stats captured for absent tables: %+v", attribution.PayloadStats)
	}
}

func TestAttributionCompactionReducesFreelist(t *testing.T) {
	ctx := context.Background()
	db, path := openAttributionDatabase(t)
	churnDatabase(t, db, 200, 150, 20, 2048)

	before, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("capture before compaction: %v", err)
	}
	if before.FreelistPages <= 0 {
		t.Fatalf("churn left no freelist pages: %d", before.FreelistPages)
	}
	if err := Compact(ctx, db); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("capture after compaction: %v", err)
	}
	if after.FreelistPages != 0 {
		t.Errorf("freelist pages after VACUUM: %d", after.FreelistPages)
	}
	if after.PageCount > before.PageCount {
		t.Errorf("page count grew after VACUUM: before=%d after=%d", before.PageCount, after.PageCount)
	}
}

func TestAttributionCapturesPayloadStatsForGraphTables(t *testing.T) {
	ctx := context.Background()
	db, path := openAttributionDatabase(t)
	statements := []string{
		"CREATE TABLE nodes(id TEXT PRIMARY KEY, owner_file TEXT, properties TEXT)",
		"CREATE TABLE facts(id TEXT PRIMARY KEY, owner_file TEXT, properties TEXT)",
		"CREATE TABLE edges(id TEXT PRIMARY KEY, properties TEXT)",
		"INSERT INTO nodes(id, owner_file, properties) VALUES('node:1', 'a.go', '{\"k\":1}')",
		"INSERT INTO nodes(id, owner_file, properties) VALUES('node:22', 'b.go', '{}')",
		"INSERT INTO facts(id, owner_file, properties) VALUES('fact:1', 'a.go', '{}')",
		"INSERT INTO edges(id, properties) VALUES('edge:1', '{\"edge\":true}')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}

	attribution, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("CaptureAttribution: %v", err)
	}
	statsByTable := make(map[string]TablePayload)
	for _, stat := range attribution.PayloadStats {
		statsByTable[stat.Table] = stat
	}
	if len(statsByTable) != 3 {
		t.Fatalf("payload stats tables: %+v", attribution.PayloadStats)
	}
	nodes := statsByTable["nodes"]
	if nodes.Rows != 2 || nodes.MaxIDLength != int64(len("node:22")) {
		t.Errorf("nodes row metrics wrong: %+v", nodes)
	}
	if nodes.AvgOwnerLength != float64(len("a.go")+len("b.go"))/2 {
		t.Errorf("nodes average owner length wrong: %+v", nodes)
	}
	if nodes.AvgProperties == 0 {
		t.Errorf("nodes average properties length missing: %+v", nodes)
	}
	edges := statsByTable["edges"]
	if edges.Rows != 1 || edges.AvgOwnerLength != 0 {
		t.Errorf("edges stats wrong: %+v", edges)
	}
}

func TestAttributionCheckpointDrainsWAL(t *testing.T) {
	ctx := context.Background()
	db, path := openAttributionDatabase(t)
	if _, err := db.Exec("CREATE TABLE t(a TEXT PRIMARY KEY, b TEXT)"); err != nil {
		t.Fatalf("create table t: %v", err)
	}
	for i := 0; i < 20; i++ {
		statement := fmt.Sprintf("INSERT INTO t(a, b) VALUES('%06d', '%s')", i, strings.Repeat("x", 512))
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	before, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("capture before checkpoint: %v", err)
	}
	if before.WALBytes == 0 {
		t.Fatal("WAL bytes zero before checkpoint")
	}
	if err := Checkpoint(ctx, db, true); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	after, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("capture after checkpoint: %v", err)
	}
	if after.WALBytes != 0 {
		t.Errorf("WAL bytes %d after truncate checkpoint", after.WALBytes)
	}
}

func TestAttributionReportsDBStatUnavailability(t *testing.T) {
	ctx := context.Background()
	db, path := openAttributionDatabase(t)
	if _, err := db.Exec("CREATE TABLE t(a TEXT PRIMARY KEY, b TEXT)"); err != nil {
		t.Fatalf("create table t: %v", err)
	}

	originalQuery := queryObjectBytes
	queryObjectBytes = func(context.Context, *sql.DB) ([]ObjectBytes, error) {
		return nil, errors.New("no such table: dbstat")
	}
	t.Cleanup(func() { queryObjectBytes = originalQuery })

	attribution, err := CaptureAttribution(ctx, db, path)
	if err != nil {
		t.Fatalf("CaptureAttribution with dbstat unavailable: %v", err)
	}
	if attribution.DBStatSupported {
		t.Fatal("DBStatSupported = true with dbstat unavailable")
	}
	if attribution.DBStatReason == "" {
		t.Error("DBStatReason empty with dbstat unavailable")
	}
	if attribution.Objects != nil {
		t.Errorf("objects guessed without dbstat: %+v", attribution.Objects)
	}
	if attribution.PrimaryBytes <= 0 {
		t.Errorf("primary bytes not captured: %d", attribution.PrimaryBytes)
	}
}
