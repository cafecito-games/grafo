package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

const (
	// defaultBatchRows bounds how many rows one INSERT carries. Each flush is a
	// statement SQLite has to prepare, plan, and journal, so a cold run that
	// writes millions of rows pays the per-statement overhead millions of times
	// at a small row count. The encoded-byte and variable limits below still cap
	// the statement, so a batch of wide rows flushes before reaching this count.
	defaultBatchRows  = 256
	defaultBatchBytes = 1 << 20
)

type batchLimits struct {
	MaxRows      int
	MaxVariables int
	MaxBytes     int64
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type sqlPreparer interface {
	PrepareContext(context.Context, string) (*sql.Stmt, error)
}

type batchSpec struct {
	name    string
	columns int
	prefix  string
	suffix  string
}

var (
	// The pinned SQLite driver applies VALUES rows in order, including repeated
	// IDs in one upsert. Exact-equivalence tests lock that last-row-wins behavior
	// to the sqlc single-row reference path.
	nodeBatchSpec = batchSpec{
		name:    "nodes",
		columns: 14,
		prefix: `INSERT INTO nodes(
    id, kind, name, qualified_name, language, path, line, column_no, end_line,
    properties, owner_file, external, name_folded, qualified_name_folded
) VALUES `,
		suffix: ` ON CONFLICT(id) DO UPDATE SET
    kind = excluded.kind,
    name = excluded.name,
    qualified_name = excluded.qualified_name,
    language = excluded.language,
    path = excluded.path,
    line = excluded.line,
    column_no = excluded.column_no,
    end_line = excluded.end_line,
    properties = excluded.properties,
    owner_file = excluded.owner_file,
    external = excluded.external,
    name_folded = excluded.name_folded,
    qualified_name_folded = excluded.qualified_name_folded`,
	}
	factBatchSpec = batchSpec{
		name:    "facts",
		columns: 15,
		prefix: `INSERT INTO facts(
    id, from_id, source, source_kind, kind, producer, target_id, target, target_kind,
    path_id, line, column_no, end_line, properties, owner_path_id
) VALUES `,
		suffix: ` ON CONFLICT(id) DO UPDATE SET
    from_id = excluded.from_id,
    source = excluded.source,
    source_kind = excluded.source_kind,
    kind = excluded.kind,
    producer = excluded.producer,
    target_id = excluded.target_id,
    target = excluded.target,
    target_kind = excluded.target_kind,
    path_id = excluded.path_id,
    line = excluded.line,
    column_no = excluded.column_no,
    end_line = excluded.end_line,
    properties = excluded.properties,
    owner_path_id = excluded.owner_path_id`,
	}
	edgeBatchSpec = batchSpec{
		name:    "edges",
		columns: 6,
		prefix: `INSERT INTO edges(
    id, fact_id, from_id, to_id, kind, properties
) VALUES `,
	}
	dirtyNodeBatchSpec = batchSpec{
		name: "dirty nodes", columns: 1,
		prefix: `INSERT OR IGNORE INTO dirty_nodes(node_id) VALUES `,
	}
	dirtyTargetBatchSpec = batchSpec{
		name: "dirty targets", columns: 2,
		prefix: `INSERT OR IGNORE INTO dirty_targets(target, target_kind) VALUES `,
	}
)

type batchBuffer struct {
	spec  batchSpec
	rows  [][]any
	bytes int64
}

type batchWriter struct {
	execer       sqlExecer
	limits       batchLimits
	afterBatch   func(string, graph.WriteBatchStats)
	nodes        batchBuffer
	dirtyNodes   batchBuffer
	dirtyTargets batchBuffer
	facts        batchBuffer
	edges        batchBuffer
	paths        *pathTransaction
	writes       graph.WriteStats
	prepared     map[string]*sql.Stmt
}

func newBatchWriter(execer sqlExecer, limits batchLimits) (*batchWriter, error) {
	if execer == nil {
		return nil, errorsNewBatchLimits("executor is nil")
	}
	if limits.MaxRows <= 0 {
		return nil, errorsNewBatchLimits("row limit must be positive")
	}
	if limits.MaxVariables <= 0 {
		return nil, errorsNewBatchLimits("variable limit must be positive")
	}
	if limits.MaxBytes <= 0 {
		return nil, errorsNewBatchLimits("byte limit must be positive")
	}
	return &batchWriter{
		execer:       execer,
		limits:       limits,
		nodes:        batchBuffer{spec: nodeBatchSpec},
		dirtyNodes:   batchBuffer{spec: dirtyNodeBatchSpec},
		dirtyTargets: batchBuffer{spec: dirtyTargetBatchSpec},
		facts:        batchBuffer{spec: factBatchSpec},
		edges:        batchBuffer{spec: edgeBatchSpec},
		prepared:     make(map[string]*sql.Stmt),
	}, nil
}

func errorsNewBatchLimits(message string) error {
	return fmt.Errorf("configure SQLite batch writer: %s", message)
}

func (w *batchWriter) addNode(ctx context.Context, row sqlcgen.UpsertNodeParams) error {
	return w.add(ctx, &w.nodes, []any{row.ID, row.Kind, row.Name, row.QualifiedName, row.Language,
		row.Path, row.Line, row.ColumnNo, row.EndLine, row.Properties, row.OwnerFile, row.External,
		row.NameFolded, row.QualifiedNameFolded})
}

func (w *batchWriter) addFact(ctx context.Context, row sqlcgen.UpsertFactParams) error {
	return w.add(ctx, &w.facts, []any{row.ID, row.FromID, row.Source, row.SourceKind, row.Kind,
		row.Producer, row.TargetID, row.Target, row.TargetKind, row.PathID, row.Line, row.ColumnNo, row.EndLine,
		row.Properties, row.OwnerPathID})
}

func (w *batchWriter) addDirtyNode(ctx context.Context, id string) error {
	return w.add(ctx, &w.dirtyNodes, []any{id})
}

func (w *batchWriter) addDirtyTarget(ctx context.Context, target, kind string) error {
	return w.add(ctx, &w.dirtyTargets, []any{target, kind})
}

func (w *batchWriter) addEdge(ctx context.Context, row sqlcgen.InsertEdgeParams) error {
	return w.add(ctx, &w.edges, []any{row.ID, row.FactID, row.FromID, row.ToID, row.Kind,
		row.Properties})
}

// pathKey resolves the interned key for one owner key inside this transaction.
// Resolution runs outside the buffered inserts so the referenced paths row is
// always written before the fact that references it.
func (w *batchWriter) pathKey(ctx context.Context, q *sqlcgen.Queries, path string) (int64, error) {
	if w.paths == nil {
		return 0, fmt.Errorf("resolve interned path %q: batch writer has no path transaction", path)
	}
	return w.paths.key(ctx, q, path)
}

func (w *batchWriter) add(ctx context.Context, buffer *batchBuffer, row []any) error {
	if len(row) != buffer.spec.columns {
		return fmt.Errorf("buffer %s row: got %d values, want %d", buffer.spec.name, len(row), buffer.spec.columns)
	}
	if buffer.spec.columns > w.limits.MaxVariables {
		return fmt.Errorf("buffer %s row: %d values exceed SQLite variable limit %d",
			buffer.spec.name, buffer.spec.columns, w.limits.MaxVariables)
	}
	rowBytes := encodedValuesBytes(row)
	wouldOverflow := len(buffer.rows) > 0 &&
		(len(buffer.rows) >= w.limits.MaxRows ||
			(len(buffer.rows)+1)*buffer.spec.columns > w.limits.MaxVariables ||
			buffer.bytes+rowBytes > w.limits.MaxBytes)
	if wouldOverflow {
		if err := w.flushBuffer(ctx, buffer); err != nil {
			return err
		}
	}
	buffer.rows = append(buffer.rows, row)
	buffer.bytes += rowBytes
	if rowBytes > w.limits.MaxBytes {
		return w.flushBuffer(ctx, buffer)
	}
	return nil
}

func (w *batchWriter) flush(ctx context.Context) error {
	for _, buffer := range []*batchBuffer{&w.nodes, &w.dirtyNodes, &w.dirtyTargets, &w.facts, &w.edges} {
		if err := w.flushBuffer(ctx, buffer); err != nil {
			return err
		}
	}
	return nil
}

func (w *batchWriter) flushBuffer(ctx context.Context, buffer *batchBuffer) error {
	if len(buffer.rows) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var statement strings.Builder
	statement.Grow(len(buffer.spec.prefix) + len(buffer.spec.suffix) + len(buffer.rows)*(buffer.spec.columns*2+2))
	statement.WriteString(buffer.spec.prefix)
	arguments := make([]any, 0, len(buffer.rows)*buffer.spec.columns)
	for rowIndex, row := range buffer.rows {
		if rowIndex > 0 {
			statement.WriteByte(',')
		}
		statement.WriteByte('(')
		for column := range buffer.spec.columns {
			if column > 0 {
				statement.WriteByte(',')
			}
			statement.WriteByte('?')
		}
		statement.WriteByte(')')
		arguments = append(arguments, row...)
	}
	statement.WriteString(buffer.spec.suffix)
	if err := w.exec(ctx, statement.String(), arguments); err != nil {
		return fmt.Errorf("write SQLite %s batch (%d rows, %d bytes): %w",
			buffer.spec.name, len(buffer.rows), buffer.bytes, err)
	}
	written := graph.WriteBatchStats{Batches: 1, Rows: int64(len(buffer.rows)), Bytes: buffer.bytes}
	switch buffer.spec.name {
	case nodeBatchSpec.name:
		addBatchStats(&w.writes.Nodes, written)
	case factBatchSpec.name:
		addBatchStats(&w.writes.Facts, written)
	case edgeBatchSpec.name:
		addBatchStats(&w.writes.Edges, written)
	}
	if w.afterBatch != nil {
		w.afterBatch(buffer.spec.name, written)
	}
	buffer.rows = buffer.rows[:0]
	buffer.bytes = 0
	return nil
}

func (w *batchWriter) exec(ctx context.Context, statement string, arguments []any) error {
	preparer, ok := w.execer.(sqlPreparer)
	if !ok {
		_, err := w.execer.ExecContext(ctx, statement, arguments...)
		return err
	}
	prepared := w.prepared[statement]
	if prepared == nil {
		var err error
		prepared, err = preparer.PrepareContext(ctx, statement)
		if err != nil {
			return err
		}
		w.prepared[statement] = prepared
	}
	_, err := prepared.ExecContext(ctx, arguments...)
	return err
}

func (w *batchWriter) close() error {
	var first error
	for statement, prepared := range w.prepared {
		if err := prepared.Close(); err != nil && first == nil {
			first = err
		}
		delete(w.prepared, statement)
	}
	return first
}

func (w *batchWriter) stats() graph.WriteStats { return w.writes }

func addBatchStats(total *graph.WriteBatchStats, increment graph.WriteBatchStats) {
	total.Batches += increment.Batches
	total.Rows += increment.Rows
	total.Bytes += increment.Bytes
}

func addWriteStats(total *graph.WriteStats, increment graph.WriteStats) {
	addBatchStats(&total.Nodes, increment.Nodes)
	addBatchStats(&total.Facts, increment.Facts)
	addBatchStats(&total.Edges, increment.Edges)
}

func encodedValuesBytes(values []any) int64 {
	var total int64
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			total += int64(len(typed))
		case []byte:
			total += int64(len(typed))
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			total += 8
		case bool:
			total++
		case nil:
		default:
			total += int64(len(fmt.Sprint(typed)))
		}
	}
	return total
}
