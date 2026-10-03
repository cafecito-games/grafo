package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embeddingcache "github.com/cafecito-games/grafo/internal/embedding/cache"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestEmbedCacheStatusAndPruneJSON(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	t.Setenv(embeddingcache.EnvPath, path)
	store, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	key := semantic.CacheKey{Model: "fixture", DocumentVersion: semantic.DocumentVersion, ContentHash: strings.Repeat("a", 64)}
	if err := store.Store(ctx, []semantic.CacheEntry{{Key: key, Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := New(&stdout, &stderr).Run(ctx, []string{"embed-cache", "status", "--json"}); code != 0 {
		t.Fatalf("status code=%d stderr=%q", code, stderr.String())
	}
	var status embeddingcache.Status
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Path != path || status.Rows != 1 || status.BlobBytes != 8 {
		t.Fatalf("status = %#v", status)
	}
	stdout.Reset()
	stderr.Reset()
	if code := New(&stdout, &stderr).Run(ctx, []string{"embed-cache", "status"}); code != 0 {
		t.Fatalf("text status code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 rows · 8 vector bytes") || !strings.Contains(stdout.String(), "fixture · 1 rows · dimensions [2] · 8 vector bytes") {
		t.Fatalf("text status = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := New(&stdout, &stderr).Run(ctx, []string{"embed-cache", "prune", "--model", "fixture", "--dry-run", "--json"}); code != 0 {
		t.Fatalf("dry-run code=%d stderr=%q", code, stderr.String())
	}
	var dry embeddingcache.PruneReport
	if err := json.Unmarshal(stdout.Bytes(), &dry); err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.DeletedRows != 1 || dry.FreedBlobBytes != 8 {
		t.Fatalf("dry-run = %#v", dry)
	}

	stdout.Reset()
	stderr.Reset()
	if code := New(&stdout, &stderr).Run(ctx, []string{"embed-cache", "prune", "--model", "fixture"}); code == 0 || !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("unconfirmed prune code=%d stderr=%q", code, stderr.String())
	}
	verify, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	remaining, _ := verify.Status(ctx)
	_ = verify.Close()
	if remaining.Rows != 1 {
		t.Fatalf("unconfirmed prune removed rows: %#v", remaining)
	}

	stdout.Reset()
	stderr.Reset()
	if code := New(&stdout, &stderr).Run(ctx, []string{"embed-cache", "prune", "--model", "fixture", "--yes", "--json"}); code != 0 {
		t.Fatalf("prune code=%d stderr=%q", code, stderr.String())
	}
	var actual embeddingcache.PruneReport
	if err := json.Unmarshal(stdout.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.DryRun || actual.DeletedRows != 1 || actual.RemainingRows != 0 {
		t.Fatalf("prune = %#v", actual)
	}
}

func TestEmbedCachePruneValidatesBeforeOpeningWritableCache(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "missing", "embeddings.sqlite")
	t.Setenv(embeddingcache.EnvPath, path)
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), []string{"embed-cache", "prune", "--older-than", "yesterday", "--yes"})
	if code == 0 || !strings.Contains(stderr.String(), "--older-than") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid prune opened cache: %v", err)
	}
}

func TestEmbedCacheRejectsUnsupportedOptionsBeforePruning(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "embeddings.sqlite")
	t.Setenv(embeddingcache.EnvPath, path)
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(ctx, []string{"embed-cache", "status", "--keep", "1"})
	if code == 0 || !strings.Contains(stderr.String(), "--keep is not supported by grafo embed-cache status") {
		t.Fatalf("status code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unsupported status option opened cache: %v", err)
	}

	store, err := embeddingcache.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	key := semantic.CacheKey{Model: "fixture", DocumentVersion: semantic.DocumentVersion, ContentHash: strings.Repeat("b", 64)}
	if err := store.Store(ctx, []semantic.CacheEntry{{Key: key, Vector: []float32{1}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code = New(&stdout, &stderr).Run(ctx, []string{"embed-cache", "prune", "--keep", "1", "--yes"})
	if code == 0 || !strings.Contains(stderr.String(), "--keep is not supported by grafo embed-cache prune") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	verify, err := embeddingcache.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = verify.Close() }()
	status, err := verify.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Rows != 1 {
		t.Fatalf("unsupported option pruned cache: %#v", status)
	}
}

func TestEmbedCacheStatusDoesNotCreateMissingCache(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "typo", "embeddings.sqlite")
	t.Setenv(embeddingcache.EnvPath, path)
	var stdout, stderr bytes.Buffer
	code := New(&stdout, &stderr).Run(context.Background(), []string{"embed-cache", "status"})
	if code == 0 || !strings.Contains(stderr.String(), "inspect embedding cache path") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("status created missing cache parent: %v", err)
	}
}

func TestParseEmbedCacheOptions(t *testing.T) {
	t.Parallel()
	parsed, err := parseArguments([]string{"embed-cache", "prune", "--older-than", "24h", "--max-bytes", "1024", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.values["older-than"] != "24h" || parsed.values["max-bytes"] != "1024" || !parsed.flags["yes"] {
		t.Fatalf("parsed = %#v", parsed)
	}
	if duration, err := time.ParseDuration(parsed.values["older-than"]); err != nil || duration != 24*time.Hour {
		t.Fatalf("duration=%s err=%v", duration, err)
	}
}
