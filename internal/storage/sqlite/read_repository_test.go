package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"github.com/pressly/goose/v3"
)

func TestOpenReadOnlyRequiresExistingCompatibleDatabaseAndEscapesPath(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(testtemp.Dir(t), "missing", "graph.sqlite")
	if _, err := OpenReadOnly(ctx, missing); err == nil || !strings.Contains(err.Error(), "run 'grafo index") {
		t.Fatalf("missing index error = %v, want actionable index instruction", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created parent directory: %v", err)
	}

	directory := testtemp.Dir(t)
	seedPath := filepath.Join(directory, "seed.sqlite")
	path := filepath.Join(directory, "graph ?#%.sqlite")
	writable, err := Open(ctx, seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(seedPath, path); err != nil {
		t.Fatal(err)
	}

	read, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	if read.Path() != path {
		t.Fatalf("path = %q, want %q", read.Path(), path)
	}
	if version, err := read.Meta(ctx, "semantic_index_version"); err != nil || version != indexer.SemanticIndexVersion {
		t.Fatalf("semantic version = %q, %v", version, err)
	}
}

func TestReadOnlyDSNEncodesWindowsDrivePathWithoutURIAuthority(t *testing.T) {
	got := readOnlyDSN(`C:/Users/Grafo Data/index ?#%.sqlite`)
	want := "file:///C:/Users/Grafo%20Data/index%20%3F%23%25.sqlite?mode=ro"
	if got != want {
		t.Fatalf("read-only Windows DSN = %q, want %q", got, want)
	}
}

func TestReadRepositoryCapabilitiesExcludeWrites(t *testing.T) {
	var repository any = (*ReadRepository)(nil)
	for name, supported := range map[string]bool{
		"read":                implements[graph.ReadRepository](repository),
		"catalog":             implements[graph.CatalogRepository](repository),
		"topology":            implements[graph.TopologyRepository](repository),
		"file catalog":        implements[graph.FileCatalog](repository),
		"semantic candidates": implements[semantic.CandidateRepository](repository),
	} {
		if !supported {
			t.Errorf("read repository does not implement %s capability", name)
		}
	}
	if implements[graph.IndexRepository](repository) || implements[graph.Repository](repository) {
		t.Fatal("read repository exposes a write capability")
	}
}

func TestOpenReadOnlyRejectsIncompatibleMetadataWithoutMutation(t *testing.T) {
	semanticVersion, err := strconv.Atoi(indexer.SemanticIndexVersion)
	if err != nil {
		t.Fatalf("semantic index version %q is not numeric: %v", indexer.SemanticIndexVersion, err)
	}
	olderSemantic := strconv.Itoa(semanticVersion - 1)
	newerSemantic := strconv.Itoa(semanticVersion + 1)
	latestMigration, err := migrations.LatestVersion()
	if err != nil {
		t.Fatalf("resolve latest migration: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(context.Context, *sql.DB) error
		want   string
	}{
		{name: "missing schema", mutate: deleteMeta("schema_version"), want: "schema_version metadata is missing"},
		{name: "malformed schema", mutate: setMetaRaw("schema_version", "ten"), want: `schema_version "ten" is malformed`},
		{name: "older schema", mutate: setMetaRaw("schema_version", fmt.Sprint(graph.SchemaVersion-1)), want: fmt.Sprintf("schema_version is %d, want %d", graph.SchemaVersion-1, graph.SchemaVersion)},
		{name: "newer schema", mutate: setMetaRaw("schema_version", fmt.Sprint(graph.SchemaVersion+1)), want: fmt.Sprintf("schema_version is %d, want %d", graph.SchemaVersion+1, graph.SchemaVersion)},
		{name: "missing semantic", mutate: deleteMeta("semantic_index_version"), want: "semantic_index_version metadata is missing"},
		{name: "malformed semantic", mutate: setMetaRaw("semantic_index_version", "next"), want: `semantic_index_version "next" is malformed`},
		{name: "older semantic", mutate: setMetaRaw("semantic_index_version", olderSemantic), want: fmt.Sprintf("semantic_index_version is %q, want %q", olderSemantic, indexer.SemanticIndexVersion)},
		{name: "newer semantic", mutate: setMetaRaw("semantic_index_version", newerSemantic), want: fmt.Sprintf("semantic_index_version is %q, want %q", newerSemantic, indexer.SemanticIndexVersion)},
		{name: "older migration", mutate: setLatestMigration(latestMigration - 1),
			want: fmt.Sprintf("storage migration is %d", latestMigration-1)},
		{name: "newer migration", mutate: setLatestMigration(latestMigration + 1),
			want: fmt.Sprintf("storage migration is %d", latestMigration+1)},
		{name: "rolled back migration", mutate: execRaw("UPDATE goose_db_version SET is_applied = 0 WHERE id = (SELECT MAX(id) FROM goose_db_version)"), want: "applied=false"},
		{name: "missing required table", mutate: execRaw("DROP TABLE edges"), want: `required table "edges" is missing`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
			seedCompatibleDatabase(t, path)
			database, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.mutate(context.Background(), database); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			before := fileSnapshot(t, path)
			if repository, err := OpenReadOnly(context.Background(), path); err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "run 'grafo index") {
				if repository != nil {
					_ = repository.Close()
				}
				t.Fatalf("error = %v, want %q and index instruction", err, test.want)
			}
			if after := fileSnapshot(t, path); before != after {
				t.Fatalf("compatibility failure mutated database: before=%q after=%q", before, after)
			}
		})
	}
}

func TestOpenReadOnlyRejectsCorruptDatabaseAndHonorsCancellation(t *testing.T) {
	corrupt := filepath.Join(testtemp.Dir(t), "corrupt.sqlite")
	if err := os.WriteFile(corrupt, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if repository, err := OpenReadOnly(context.Background(), corrupt); err == nil || !strings.Contains(err.Error(), "incompatible index") {
		if repository != nil {
			_ = repository.Close()
		}
		t.Fatalf("corrupt database error = %v", err)
	}

	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	seedCompatibleDatabase(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if repository, err := OpenReadOnly(ctx, path); !errors.Is(err, context.Canceled) {
		if repository != nil {
			_ = repository.Close()
		}
		t.Fatalf("canceled open error = %v, want context.Canceled", err)
	}
}

func TestOpenReadOnlyIsIdempotentAndWorksOnReadOnlyFilesystem(t *testing.T) {
	directory := testtemp.Dir(t)
	path := filepath.Join(directory, "graph.sqlite")
	seedCompatibleDatabase(t, path)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(path, 0o644)
	})
	repository, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Counts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestOpenReadOnlyObservesCommittedWALUpdates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	writable, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writable.Close() })
	if err := writable.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		t.Fatal(err)
	}
	if err := writable.SetMeta(ctx, "wal_probe", "before"); err != nil {
		t.Fatal(err)
	}
	read, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	if value, err := read.Meta(ctx, "wal_probe"); err != nil || value != "before" {
		t.Fatalf("initial WAL value = %q, %v", value, err)
	}
	if err := writable.SetMeta(ctx, "wal_probe", "after"); err != nil {
		t.Fatal(err)
	}
	if value, err := read.Meta(ctx, "wal_probe"); err != nil || value != "after" {
		t.Fatalf("updated WAL value = %q, %v", value, err)
	}
}

func TestReadOnlyAndWritableQueriesAreByteEquivalent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	writable, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writable.Close() })
	if err := writable.SetMeta(ctx, "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		t.Fatal(err)
	}
	if err := writable.SetMeta(ctx, "root", "/workspace/sample"); err != nil {
		t.Fatal(err)
	}
	caller := graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "Caller", QualifiedName: "sample.Caller", OwnerFile: "sample.go"}
	target := graph.Node{ID: "target", Kind: graph.KindFunction, Name: "Target", QualifiedName: "sample.Target", OwnerFile: "sample.go"}
	fact := graph.Fact{ID: "call", FromID: caller.ID, Kind: graph.EdgeCalls, TargetID: target.ID, OwnerFile: "sample.go"}
	if err := writable.ReplaceFile(ctx, graph.FileRecord{Path: "sample.go", Hash: "hash", Language: "go", IndexedAt: graph.NowUTC()},
		graph.ParseResult{Nodes: []graph.Node{caller, target}, Facts: []graph.Fact{fact}}); err != nil {
		t.Fatal(err)
	}
	if err := writable.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	read, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	writableJSON := querySnapshot(t, writable, caller.ID)
	readJSON := querySnapshot(t, read, caller.ID)
	if !reflect.DeepEqual(writableJSON, readJSON) {
		t.Fatalf("query snapshots differ\nwritable: %s\nread-only: %s", writableJSON, readJSON)
	}
	if count := len(read.statements.statements); count == 0 || count > maxReadStatements {
		t.Fatalf("lazy statement count = %d, want 1..%d", count, maxReadStatements)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := read.Node(canceled, caller.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query error = %v, want context.Canceled", err)
	}
}

func TestOpenReadOnlyConcurrentOpenQueryCloseDoesNotLeak(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	seedCompatibleDatabase(t, path)
	const workers = 24
	var wait sync.WaitGroup
	errorsByWorker := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			repository, err := OpenReadOnly(context.Background(), path)
			if err != nil {
				errorsByWorker <- err
				return
			}
			if _, err := repository.Counts(context.Background()); err != nil {
				errorsByWorker <- err
			}
			if err := repository.Close(); err != nil {
				errorsByWorker <- err
			}
			if err := repository.Close(); err != nil {
				errorsByWorker <- err
			}
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Error(err)
	}
}

func TestOpenReadOnlyBusyValidationHonorsContext(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	seedCompatibleDatabase(t, path)
	locker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	connection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := connection.ExecContext(context.Background(), "PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = connection.ExecContext(context.Background(), "ROLLBACK") })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	repository, err := OpenReadOnly(ctx, path)
	if repository != nil {
		_ = repository.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy open error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("busy open ignored cancellation for %s", elapsed)
	}
}

type readSurface interface {
	graph.ReadRepository
	graph.CatalogRepository
	graph.FileCatalog
	semantic.CandidateRepository
}

func querySnapshot(t *testing.T, repository readSurface, callerID string) []byte {
	t.Helper()
	ctx := context.Background()
	node, err := repository.Node(ctx, callerID)
	if err != nil {
		t.Fatal(err)
	}
	search, err := repository.SearchNodes(ctx, "Caller", 10)
	if err != nil {
		t.Fatal(err)
	}
	match, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "sample.Caller", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, callerID)
	if err != nil {
		t.Fatal(err)
	}
	relations, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: callerID,
		Direction: graph.OutgoingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindFunction}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	files, err := repository.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := repository.CandidateNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		Node       graph.Node
		Search     []graph.Node
		Match      graph.NodeMatchGroup
		Edges      []graph.Edge
		Relations  graph.RelationEdgePage
		Listed     []graph.ScopedNode
		Files      map[string]graph.FileRecord
		Counts     graph.Counts
		Candidates []graph.Node
	}{node, search, match, edges, relations, listed, files, counts, candidates})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestWritableOpenDropsLegacyEmbeddingsWithoutChangingStructuralCounts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "graph.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO nodes
		(id, kind, name, qualified_name, language, path, line, column_no, end_line, properties, owner_file, external,
		 name_folded, qualified_name_folded)
		VALUES ('node', 'function', 'Node', 'sample.Node', 'go', 'sample.go', 1, 1, 1, '{}', 'sample.go', 0, 'node', 'sample.node')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO embeddings(node_id, model, content_hash, dimensions, vector_json, updated_at)
		VALUES ('node', 'legacy', 'hash', 2, '[1,0]', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Nodes != 1 {
		t.Fatalf("node count after migration = %d, want 1", counts.Nodes)
	}
	var tableCount int
	if err := repository.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'embeddings'").Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatal("legacy embeddings table still exists")
	}
}

func seedCompatibleDatabase(t *testing.T, path string) {
	t.Helper()
	repository, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(context.Background(), "semantic_index_version", indexer.SemanticIndexVersion); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
}

func setMetaRaw(key, value string) func(context.Context, *sql.DB) error {
	return func(ctx context.Context, database *sql.DB) error {
		_, err := database.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
		return err
	}
}

func deleteMeta(key string) func(context.Context, *sql.DB) error {
	return func(ctx context.Context, database *sql.DB) error {
		_, err := database.ExecContext(ctx, "DELETE FROM meta WHERE key = ?", key)
		return err
	}
}

func setLatestMigration(version int64) func(context.Context, *sql.DB) error {
	return func(ctx context.Context, database *sql.DB) error {
		_, err := database.ExecContext(ctx, "UPDATE goose_db_version SET version_id = ? WHERE id = (SELECT MAX(id) FROM goose_db_version)", version)
		return err
	}
}

func execRaw(statement string) func(context.Context, *sql.DB) error {
	return func(ctx context.Context, database *sql.DB) error {
		_, err := database.ExecContext(ctx, statement)
		return err
	}
}

func fileSnapshot(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d:%o", info.Size(), info.ModTime().UnixNano(), info.Mode().Perm())
}

func implements[T any](value any) bool {
	_, ok := value.(T)
	return ok
}
