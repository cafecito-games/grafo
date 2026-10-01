package cache_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	embeddingcache "github.com/cafecito-games/grafo/internal/embedding/cache"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestBinaryRoundTripStatusAndPrivacy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "private", "embeddings.sqlite")
	store, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	key := semantic.CacheKey{Model: "model-A", DocumentVersion: "1", ContentHash: strings.Repeat("a", 64)}
	want := []float32{1.25, -2.5, 0.125}
	if err := store.Store(ctx, []semantic.CacheEntry{{Key: key, Vector: want}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, []semantic.CacheKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[key], want) {
		t.Fatalf("round trip = %#v, want %#v", got[key], want)
	}

	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.SchemaVersion != embeddingcache.SchemaVersion || status.Rows != 1 || status.BlobBytes != 12 ||
		len(status.Models) != 1 || !reflect.DeepEqual(status.Models[0].Dimensions, []int{3}) {
		t.Fatalf("status = %#v", status)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("cache mode = %o, want owner-only", info.Mode().Perm())
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm()&0o077 != 0 {
		t.Fatalf("cache directory mode = %o, want owner-only", parent.Mode().Perm())
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var dimensions, bytes int
	var blob []byte
	if err := database.QueryRowContext(ctx, "SELECT dimensions, length(vector), vector FROM embeddings").Scan(&dimensions, &bytes, &blob); err != nil {
		t.Fatal(err)
	}
	if dimensions != 3 || bytes != 12 {
		t.Fatalf("stored dimensions=%d bytes=%d", dimensions, bytes)
	}
	for _, forbidden := range []string{"payments.ChargeCard", "/workspace/private", "http://localhost:11434"} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("binary vector contains private text %q", forbidden)
		}
	}
}

func TestMalformedAndNonFiniteVectorsFailClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	store, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("b", 64)}
	for _, vector := range [][]float32{{}, {float32(math.NaN())}, {float32(math.Inf(1))}} {
		if err := store.Store(ctx, []semantic.CacheEntry{{Key: key, Vector: vector}}); err == nil {
			t.Fatalf("Store(%v) succeeded", vector)
		}
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO embeddings
		(model, document_version, content_hash, dimensions, vector, created_at, last_used_at)
		VALUES (?, ?, ?, 2, ?, ?, ?)`, key.Model, key.DocumentVersion, key.ContentHash, []byte{0, 0, 0, 0},
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, []semantic.CacheKey{key}); err == nil || !strings.Contains(err.Error(), key.ContentHash) {
		t.Fatalf("malformed load error = %v", err)
	}
	if _, err := database.ExecContext(ctx, "DELETE FROM embeddings"); err != nil {
		t.Fatal(err)
	}
	overflowKey := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("0", 64)}
	if _, err := database.ExecContext(ctx, `INSERT INTO embeddings
		(model, document_version, content_hash, dimensions, vector, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, overflowKey.Model, overflowKey.DocumentVersion, overflowKey.ContentHash,
		int64(^uint64(0)>>1), []byte{0, 0, 0, 0}, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, []semantic.CacheKey{overflowKey}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("overflow load error = %v", err)
	}
	if _, err := database.ExecContext(ctx, "DELETE FROM embeddings"); err != nil {
		t.Fatal(err)
	}
	nonFiniteKey := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("9", 64)}
	blob := make([]byte, 4)
	binary.LittleEndian.PutUint32(blob, math.Float32bits(float32(math.NaN())))
	if _, err := database.ExecContext(ctx, `INSERT INTO embeddings
		(model, document_version, content_hash, dimensions, vector, created_at, last_used_at)
		VALUES (?, ?, ?, 1, ?, ?, ?)`, nonFiniteKey.Model, nonFiniteKey.DocumentVersion, nonFiniteKey.ContentHash,
		blob, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, []semantic.CacheKey{nonFiniteKey}); err == nil || !strings.Contains(err.Error(), "non-finite") {
		t.Fatalf("non-finite load error = %v", err)
	}
	_ = database.Close()
}

func TestConcurrentIdenticalUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	first, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	entry := semantic.CacheEntry{Key: semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("c", 64)}, Vector: []float32{1, 0}}

	var wait sync.WaitGroup
	errorsByWriter := make(chan error, 16)
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			writer := first
			if index%2 == 1 {
				writer = second
			}
			errorsByWriter <- writer.Store(ctx, []semantic.CacheEntry{entry})
		}(index)
	}
	wait.Wait()
	close(errorsByWriter)
	for err := range errorsByWriter {
		if err != nil {
			t.Error(err)
		}
	}
	status, err := first.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Rows != 1 {
		t.Fatalf("rows = %d, want 1", status.Rows)
	}
}

func TestModelAndDocumentVersionAreExactCacheNamespaces(t *testing.T) {
	ctx := context.Background()
	store, err := embeddingcache.Open(ctx, filepath.Join(testtemp.Dir(t), "embeddings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	hash := strings.Repeat("e", 64)
	entries := []semantic.CacheEntry{
		{Key: semantic.CacheKey{Model: "Model", DocumentVersion: "1", ContentHash: hash}, Vector: []float32{1}},
		{Key: semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: hash}, Vector: []float32{2}},
		{Key: semantic.CacheKey{Model: "Model", DocumentVersion: "2", ContentHash: hash}, Vector: []float32{3}},
	}
	if err := store.Store(ctx, entries); err != nil {
		t.Fatal(err)
	}
	keys := make([]semantic.CacheKey, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Key)
	}
	loaded, err := store.Load(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !reflect.DeepEqual(loaded[entry.Key], entry.Vector) {
			t.Fatalf("key %#v = %#v, want %#v", entry.Key, loaded[entry.Key], entry.Vector)
		}
	}
}

func TestLoadBatchesBeyondSQLiteVariableLimit(t *testing.T) {
	ctx := context.Background()
	store, err := embeddingcache.Open(ctx, filepath.Join(testtemp.Dir(t), "embeddings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	entries := make([]semantic.CacheEntry, 0, 701)
	keys := make([]semantic.CacheKey, 0, 701)
	for index := 0; index < 701; index++ {
		key := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: hashForIndex(index)}
		entries = append(entries, semantic.CacheEntry{Key: key, Vector: []float32{float32(index)}})
		keys = append(keys, key)
	}
	if err := store.Store(ctx, entries); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(keys) {
		t.Fatalf("loaded %d keys, want %d", len(loaded), len(keys))
	}
}

func TestAccessTimesAreCoalesced(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	store, err := embeddingcache.OpenWithOptions(ctx, path, embeddingcache.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	key := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("f", 64)}
	if err := store.Store(ctx, []semantic.CacheEntry{{Key: key, Vector: []float32{1}}}); err != nil {
		t.Fatal(err)
	}
	readTimestamp := func() string {
		t.Helper()
		database, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = database.Close() }()
		var stamp string
		if err := database.QueryRowContext(ctx, "SELECT last_used_at FROM embeddings").Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		return stamp
	}
	original := readTimestamp()
	now = now.Add(30 * time.Minute)
	if _, err := store.Load(ctx, []semantic.CacheKey{key}); err != nil {
		t.Fatal(err)
	}
	if got := readTimestamp(); got != original {
		t.Fatalf("access time advanced inside coalescing interval: %q -> %q", original, got)
	}
	now = now.Add(31 * time.Minute)
	if _, err := store.Load(ctx, []semantic.CacheKey{key}); err != nil {
		t.Fatal(err)
	}
	if got := readTimestamp(); got == original {
		t.Fatalf("access time did not advance after coalescing interval: %q", got)
	}
}

func TestResolvePathRejectsUnsafeTargets(t *testing.T) {
	t.Run("blank override", func(t *testing.T) {
		t.Setenv(embeddingcache.EnvPath, "   ")
		if _, err := embeddingcache.ResolvePath(); err == nil {
			t.Fatal("blank override succeeded")
		}
	})
	t.Run("directory", func(t *testing.T) {
		path := testtemp.Dir(t)
		if _, err := embeddingcache.Open(context.Background(), path); err == nil {
			t.Fatal("directory cache path succeeded")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := testtemp.Dir(t)
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := embeddingcache.Open(context.Background(), link); err == nil {
			t.Fatal("symlink cache path succeeded")
		}
	})
	t.Run("symlinked parent component", func(t *testing.T) {
		root := testtemp.Dir(t)
		realParent := filepath.Join(root, "real")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(realParent, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := embeddingcache.Open(context.Background(), filepath.Join(link, "nested", "embeddings.sqlite")); err == nil {
			t.Fatal("symlinked parent component succeeded")
		}
	})
}

func TestOpenRejectsCorruptAndIncompatibleCachesWithoutReplacingThem(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
		want := []byte("not a sqlite database")
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := embeddingcache.Open(context.Background(), path); err == nil {
			t.Fatal("corrupt cache opened")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("corrupt cache was replaced: %q", got)
		}
	})
	t.Run("incompatible schema", func(t *testing.T) {
		path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
		database, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec("CREATE TABLE embeddings(model TEXT); PRAGMA user_version = 1"); err != nil {
			t.Fatal(err)
		}
		_ = database.Close()
		if _, err := embeddingcache.Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "incompatible") {
			t.Fatalf("incompatible cache error = %v", err)
		}
	})
	t.Run("future version", func(t *testing.T) {
		path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
		database, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec("PRAGMA user_version = 99"); err != nil {
			t.Fatal(err)
		}
		_ = database.Close()
		if _, err := embeddingcache.Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "version 99") {
			t.Fatalf("future cache error = %v", err)
		}
	})
}

func TestPruneComposesFiltersUsesOldestFirstAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	store, err := embeddingcache.OpenWithOptions(ctx, path, embeddingcache.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	entries := []semantic.CacheEntry{
		{Key: semantic.CacheKey{Model: "keep", DocumentVersion: "1", ContentHash: strings.Repeat("1", 64)}, Vector: []float32{1, 0}},
		{Key: semantic.CacheKey{Model: "trim", DocumentVersion: "1", ContentHash: strings.Repeat("2", 64)}, Vector: []float32{1, 0}},
		{Key: semantic.CacheKey{Model: "trim", DocumentVersion: "1", ContentHash: strings.Repeat("3", 64)}, Vector: []float32{0, 1}},
	}
	if err := store.Store(ctx, entries); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for index, entry := range entries {
		stamp := now.Add(time.Duration(index-3) * time.Hour).Format(time.RFC3339Nano)
		if _, err := database.ExecContext(ctx, "UPDATE embeddings SET last_used_at = ? WHERE model = ? AND content_hash = ?", stamp, entry.Key.Model, entry.Key.ContentHash); err != nil {
			t.Fatal(err)
		}
	}
	_ = database.Close()

	filter := embeddingcache.PruneOptions{Model: "trim", OlderThan: 30 * time.Minute, MaxBytes: pointer(int64(16)), DryRun: true}
	dry, err := store.Prune(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if dry.DeletedRows != 1 || dry.FreedBlobBytes != 8 || !dry.DryRun {
		t.Fatalf("dry-run = %#v", dry)
	}
	status, _ := store.Status(ctx)
	if status.Rows != 3 {
		t.Fatalf("dry-run mutated rows: %d", status.Rows)
	}
	filter.DryRun = false
	actual, err := store.Prune(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if actual.DeletedRows != dry.DeletedRows || actual.FreedBlobBytes != dry.FreedBlobBytes {
		t.Fatalf("actual=%#v dry=%#v", actual, dry)
	}
	remaining, err := store.Load(ctx, []semantic.CacheKey{entries[1].Key, entries[2].Key})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := remaining[entries[1].Key]; exists {
		t.Fatal("prune retained the oldest eligible row")
	}
	if _, exists := remaining[entries[2].Key]; !exists {
		t.Fatal("prune removed a newer row after meeting the size target")
	}
	replay, err := store.Prune(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if replay.DeletedRows != 0 || replay.FreedBlobBytes != 0 {
		t.Fatalf("replay = %#v", replay)
	}
}

func TestRFC3339NanoTimestampsUseChronologicalBoundsAndAge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 2, 0, time.UTC)
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	store, err := embeddingcache.OpenWithOptions(ctx, path, embeddingcache.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	whole := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("4", 64)}
	fractional := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("5", 64)}
	if err := store.Store(ctx, []semantic.CacheEntry{{Key: whole, Vector: []float32{1}}, {Key: fractional, Vector: []float32{2}}}); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "UPDATE embeddings SET last_used_at = ? WHERE content_hash = ?", "2026-09-28T10:00:00Z", whole.ContentHash); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "UPDATE embeddings SET last_used_at = ? WHERE content_hash = ?", "2026-09-28T10:00:00.5Z", fractional.ContentHash); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.OldestUsedAt != "2026-09-28T10:00:00Z" || status.NewestUsedAt != "2026-09-28T10:00:00.5Z" {
		t.Fatalf("chronological bounds = %q..%q", status.OldestUsedAt, status.NewestUsedAt)
	}
	report, err := store.Prune(ctx, embeddingcache.PruneOptions{OlderThan: 1750 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeletedRows != 1 {
		t.Fatalf("age prune = %#v", report)
	}
	remaining, err := store.Load(ctx, []semantic.CacheKey{whole, fractional})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := remaining[whole]; exists {
		t.Fatal("whole-second older row was not pruned")
	}
	if _, exists := remaining[fractional]; !exists {
		t.Fatal("newer fractional row was pruned")
	}
}

func TestCanceledPruneRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	store, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	key := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("d", 64)}
	if err := store.Store(ctx, []semantic.CacheEntry{{Key: key, Vector: []float32{1}}}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Prune(canceled, embeddingcache.PruneOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("prune error = %v, want context.Canceled", err)
	}
	status, _ := store.Status(ctx)
	if status.Rows != 1 {
		t.Fatalf("canceled prune left %d rows", status.Rows)
	}
}

func TestPruneFailureRollsBackSelectedBatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	store, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	first := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("1", 64)}
	second := semantic.CacheKey{Model: "model", DocumentVersion: "1", ContentHash: strings.Repeat("2", 64)}
	if err := store.Store(ctx, []semantic.CacheEntry{{Key: first, Vector: []float32{1}}, {Key: second, Vector: []float32{2}}}); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER reject_second BEFORE DELETE ON embeddings
		WHEN OLD.content_hash = '`+second.ContentHash+`' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	if _, err := store.Prune(ctx, embeddingcache.PruneOptions{}); err == nil {
		t.Fatal("prune with failing second delete succeeded")
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Rows != 2 || status.BlobBytes != 8 {
		t.Fatalf("failed prune partially committed: %#v", status)
	}
}

func pointer[T any](value T) *T { return &value }

func hashForIndex(index int) string {
	return strings.Repeat("0", 60) + fmt.Sprintf("%04x", index)
}
