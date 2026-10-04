package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

type recordingExecer struct {
	calls []recordedExec
	err   error
}

type recordedExec struct {
	query string
	args  []any
}

func (e *recordingExecer) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	e.calls = append(e.calls, recordedExec{query: query, args: append([]any(nil), args...)})
	return recordingResult(0), e.err
}

type recordingResult int64

func (r recordingResult) LastInsertId() (int64, error) { return 0, nil }
func (r recordingResult) RowsAffected() (int64, error) { return int64(r), nil }

func TestBatchWriterRespectsRowAndVariableLimits(t *testing.T) {
	execer := &recordingExecer{}
	writer, err := newBatchWriter(execer, batchLimits{MaxRows: 3, MaxVariables: 28, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for index := range 5 {
		params := sqlcgen.UpsertNodeParams{ID: strings.Repeat("n", index+1), Kind: "function", Name: "node"}
		if err := writer.addNode(context.Background(), params); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(execer.calls) != 3 {
		t.Fatalf("executions = %d, want 3", len(execer.calls))
	}
	for _, call := range execer.calls {
		if len(call.args) > 28 {
			t.Fatalf("execution used %d variables, want at most 28", len(call.args))
		}
	}
	stats := writer.stats()
	if stats.Nodes.Batches != 3 || stats.Nodes.Rows != 5 {
		t.Fatalf("node stats = %#v, want 3 batches and 5 rows", stats.Nodes)
	}
}

func TestBatchWriterRespectsByteLimitAndExecutesOversizedRowAlone(t *testing.T) {
	execer := &recordingExecer{}
	writer, err := newBatchWriter(execer, batchLimits{MaxRows: 100, MaxVariables: 10_000, MaxBytes: 20})
	if err != nil {
		t.Fatal(err)
	}
	rows := []sqlcgen.InsertEdgeParams{
		{FactID: "f", FromID: "a", ToID: "small-1", Kind: "calls"},
		{FactID: "f", FromID: "a", ToID: "oversized", Kind: "calls", Properties: strings.Repeat("x", 100)},
		{FactID: "f", FromID: "a", ToID: "small-2", Kind: "calls"},
	}
	for _, row := range rows {
		if err := writer.addEdge(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(execer.calls) != 3 {
		t.Fatalf("executions = %d, want each row isolated by byte limit", len(execer.calls))
	}
	if got := writer.stats().Edges; got.Batches != 3 || got.Rows != 3 || got.Bytes <= 100 {
		t.Fatalf("edge stats = %#v", got)
	}
}

func TestBatchWriterRejectsInvalidLimits(t *testing.T) {
	for name, limits := range map[string]batchLimits{
		"rows":      {MaxVariables: 100, MaxBytes: 100},
		"variables": {MaxRows: 1, MaxBytes: 100},
		"bytes":     {MaxRows: 1, MaxVariables: 100},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newBatchWriter(&recordingExecer{}, limits); err == nil {
				t.Fatal("expected invalid limits to fail")
			}
		})
	}
}
