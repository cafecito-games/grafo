package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestInspectIndexReportsMetadataAndCompatibilityWithoutMutation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "branch.sqlite")
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	metadata := IndexMetadata{
		Root: "/workspace/project", RepositoryID: "repo-1", Branch: "feature",
		Commit: "abc123", IndexedAt: "2026-09-28T12:00:00Z",
	}
	for key, value := range map[string]string{
		"root": metadata.Root, "repository_id": metadata.RepositoryID,
		"branch": metadata.Branch, "commit": metadata.Commit,
		"indexed_at": metadata.IndexedAt, "semantic_index_version": indexer.SemanticIndexVersion,
	} {
		if err := repository.SetMeta(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Compatibility != CompatibilityCompatible || inspection.Metadata != metadata || inspection.Diagnostic != "" {
		t.Fatalf("inspection = %#v", inspection)
	}
	if inspection.Metrics == nil || inspection.Metrics.PageSize <= 0 || inspection.Metrics.PageCount <= 0 {
		t.Fatalf("inspection metrics = %#v", inspection.Metrics)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read-only inspection changed the primary database")
	}
}

func TestInspectIndexReadsCommittedWALMetadata(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "live.sqlite")
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	for key, value := range map[string]string{
		"root": "/workspace/project", "repository_id": "repo-1", "branch": "live",
		"commit": "new", "indexed_at": "2026-09-28T12:00:00Z",
		"semantic_index_version": indexer.SemanticIndexVersion,
	} {
		if err := repository.SetMeta(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture has no committed WAL evidence: %v", err)
	}
	inspection, err := InspectIndex(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Compatibility != CompatibilityCompatible || inspection.Metadata.Commit != "new" {
		t.Fatalf("live WAL inspection = %#v", inspection)
	}
}

func TestInspectIndexRetainsValidatedIdentityWhenMetricsFail(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "metrics-failure.sqlite")
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	metadata := IndexMetadata{
		Root: "/workspace/project", RepositoryID: "repo-1", Branch: "feature",
		Commit: "abc123", IndexedAt: "2026-09-28T12:00:00Z",
	}
	for key, value := range map[string]string{
		"root": metadata.Root, "repository_id": metadata.RepositoryID,
		"branch": metadata.Branch, "commit": metadata.Commit,
		"indexed_at": metadata.IndexedAt, "semantic_index_version": indexer.SemanticIndexVersion,
	} {
		if err := repository.SetMeta(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	inspection, err := inspectIndex(ctx, path, func(context.Context, *sql.DB) (StorageMetrics, error) {
		return StorageMetrics{}, errors.New("injected page metric failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Metadata != metadata || inspection.Metrics != nil || inspection.Compatibility != CompatibilityUnverified {
		t.Fatalf("metrics failure inspection = %#v", inspection)
	}
	if !strings.Contains(inspection.Diagnostic, "injected page metric failure") {
		t.Fatalf("metrics failure diagnostic = %q", inspection.Diagnostic)
	}
}

func TestInspectIndexClassifiesIncompatibleAndCorruptDatabases(t *testing.T) {
	ctx := context.Background()
	incompatiblePath := filepath.Join(testtemp.Dir(t), "incompatible.sqlite")
	repository, err := Open(ctx, incompatiblePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", "0"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectIndex(ctx, incompatiblePath)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Compatibility != CompatibilityIncompatible || inspection.Diagnostic == "" {
		t.Fatalf("incompatible inspection = %#v", inspection)
	}

	corruptPath := filepath.Join(testtemp.Dir(t), "corrupt.sqlite")
	if err := os.WriteFile(corruptPath, []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err = InspectIndex(ctx, corruptPath)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Compatibility != CompatibilityCorrupt || inspection.Diagnostic == "" || len(inspection.Diagnostic) > maxInspectionDiagnostic {
		t.Fatalf("corrupt inspection = %#v", inspection)
	}
}

func TestCheckpointIndexRequiresSameVerifiedMetadataAndNeverMigrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "branch.sqlite")
	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	metadata := IndexMetadata{Root: "/workspace/project", RepositoryID: "repo-1", Branch: "old", Commit: "abc", IndexedAt: "2026-09-27T12:00:00Z"}
	for key, value := range map[string]string{
		"root": metadata.Root, "repository_id": metadata.RepositoryID, "branch": metadata.Branch,
		"commit": metadata.Commit, "indexed_at": metadata.IndexedAt,
		"semantic_index_version": indexer.SemanticIndexVersion,
	} {
		if err := repository.SetMeta(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckpointIndex(ctx, path, metadata); err != nil {
		t.Fatal(err)
	}
	mismatch := metadata
	mismatch.Branch = "other"
	beforeMismatch := inspectionFileSnapshot(t, path)
	if err := CheckpointIndex(ctx, path, mismatch); err == nil || !strings.Contains(err.Error(), "metadata changed") {
		t.Fatalf("mismatched checkpoint error = %v", err)
	}
	if afterMismatch := inspectionFileSnapshot(t, path); !reflect.DeepEqual(beforeMismatch, afterMismatch) {
		t.Fatalf("failed checkpoint changed index files: before=%#v after=%#v", beforeMismatch, afterMismatch)
	}

	bare := filepath.Join(testtemp.Dir(t), "bare.sqlite")
	if err := os.WriteFile(bare, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(bare)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckpointIndex(ctx, bare, IndexMetadata{}); err == nil {
		t.Fatal("unverified database was checkpointed")
	}
	after, err := os.Stat(bare)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("checkpoint migrated bare database: before=%d after=%d", before.Size(), after.Size())
	}
}

func inspectionFileSnapshot(t *testing.T, primary string) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, path := range []string{primary, primary + "-wal", primary + "-shm"} {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result[filepath.Base(path)] = content
	}
	return result
}
