package sqlite

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

func TestPathInternerAssignsOneKeyPerDistinctPath(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)

	var first, repeated, other int64
	if err := repository.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		var err error
		if first, err = writer.pathKey(ctx, q, "a.go"); err != nil {
			return err
		}
		if repeated, err = writer.pathKey(ctx, q, "a.go"); err != nil {
			return err
		}
		other, err = writer.pathKey(ctx, q, "b.go")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if first != repeated || first == other {
		t.Fatalf("interned keys = %d, %d, %d", first, repeated, other)
	}

	// A later transaction reuses the committed keys without re-interning.
	var cached int64
	if err := repository.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		var err error
		cached, err = writer.pathKey(ctx, q, "a.go")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cached != first {
		t.Fatalf("cached key = %d, want %d", cached, first)
	}
	if rows := tableRows(t, repository, "paths"); len(rows) != 2 {
		t.Fatalf("interned path rows = %v", rows)
	}
}

func TestRolledBackTransactionDoesNotPublishInternedPathKeys(t *testing.T) {
	ctx := context.Background()
	repository := openTestRepository(t)

	duplicate := sqlcgen.InsertEdgeParams{ID: "duplicate", FactID: "fact", FromID: "a", ToID: "b", Kind: "calls"}
	var rolledBack int64
	if err := repository.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		key, err := writer.pathKey(ctx, q, "rolled-back.go")
		if err != nil {
			return err
		}
		rolledBack = key
		if err := writer.addEdge(ctx, duplicate); err != nil {
			return err
		}
		return writer.addEdge(ctx, duplicate)
	}); err == nil {
		t.Fatal("duplicate edge batch unexpectedly committed")
	}
	if rolledBack == 0 {
		t.Fatal("path was never interned inside the rolled-back transaction")
	}
	if rows := tableRows(t, repository, "paths"); len(rows) != 0 {
		t.Fatalf("rolled-back paths = %v", rows)
	}
	if _, cached := repository.pathKeys.lookup("rolled-back.go"); cached {
		t.Fatal("rolled-back path key was published to the shared cache")
	}

	// Re-interning after the rollback must write the row the facts reference.
	if err := repository.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		_, err := writer.pathKey(ctx, q, "rolled-back.go")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows := tableRows(t, repository, "paths"); len(rows) != 1 {
		t.Fatalf("re-interned paths = %v", rows)
	}
}
