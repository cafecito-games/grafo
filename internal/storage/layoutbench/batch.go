package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

const (
	defaultBatchRows  = 32
	defaultBatchBytes = 1 << 20
)

// Statement names that own a fixed buffer, in flush order. Production flushes
// nodes, dirty nodes, dirty targets, facts, then edges; any other statement a
// layout writes through the batch writer flushes after these, sorted by name.
const (
	nodeStatement        = "UpsertNode"
	dirtyNodeStatement   = "MarkDirtyNode"
	dirtyTargetStatement = "MarkDirtyTarget"
	factStatement        = "UpsertFact"
	edgeStatement        = "InsertEdge"
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

// batchSpec turns one named single-row INSERT statement into a bounded
// multi-row batch. The prefix holds the statement up to and including the
// VALUES keyword; the row template is the values tuple with a bare ? at every
// bind point (a named or numbered placeholder inside the tuple, including one
// inside a subselect, becomes one bind per emitted row); the suffix keeps the
// conflict actions.
type batchSpec struct {
	name         string
	label        string
	prefix       string
	rowTemplate  string
	suffix       string
	bindsPerRow  int
	statsSection string
}

func parseBatchSpec(name, statement string) (batchSpec, error) {
	valuesIndex, found := findValuesKeyword(statement)
	if !found {
		return batchSpec{}, fmt.Errorf("statement %s has no VALUES tuple to batch", name)
	}
	groupStart := valuesIndex
	for groupStart < len(statement) && (statement[groupStart] == ' ' || statement[groupStart] == '\t' ||
		statement[groupStart] == '\n' || statement[groupStart] == '\r') {
		groupStart++
	}
	if groupStart >= len(statement) || statement[groupStart] != '(' {
		return batchSpec{}, fmt.Errorf("statement %s has no values tuple after VALUES", name)
	}
	groupEnd, err := scanBalancedGroup(statement, groupStart)
	if err != nil {
		return batchSpec{}, fmt.Errorf("statement %s: %w", name, err)
	}
	template, binds := normalizeTuple(statement[groupStart:groupEnd])
	if binds == 0 {
		return batchSpec{}, fmt.Errorf("statement %s values tuple binds no values", name)
	}
	// A placeholder outside the values tuple cannot be re-bound per emitted
	// row, so a layout carrying one would silently mis-bind at flush time.
	if placeholder, position := findFirstPlaceholder(statement[:groupStart]); placeholder != "" {
		return batchSpec{}, fmt.Errorf("statement %s has placeholder %s before the values tuple at offset %d",
			name, placeholder, position)
	}
	if placeholder, position := findFirstPlaceholder(statement[groupEnd:]); placeholder != "" {
		return batchSpec{}, fmt.Errorf("statement %s has placeholder %s after the values tuple at offset %d",
			name, placeholder, position)
	}
	return batchSpec{name: name, prefix: statement[:groupStart], rowTemplate: template,
		suffix: statement[groupEnd:], bindsPerRow: binds,
		label: labelFor(name), statsSection: statsSectionFor(name)}, nil
}

// findFirstPlaceholder reports the first ? or @name bind point in text,
// skipping string literals and comments, so conflict actions that embed a
// placeholder are rejected instead of silently dropped from the batch binds.
// It returns the placeholder text ("" when none) and its byte offset.
func findFirstPlaceholder(text string) (string, int) {
	index := 0
	for index < len(text) {
		switch {
		case text[index] == '\'':
			index = skipStringLiteral(text, index)
		case strings.HasPrefix(text[index:], "--"):
			for index < len(text) && text[index] != '\n' {
				index++
			}
		case strings.HasPrefix(text[index:], "/*"):
			index += 2
			for index < len(text) && !strings.HasPrefix(text[index:], "*/") {
				index++
			}
			index = min(index+2, len(text))
		case text[index] == '?':
			end := index + 1
			for end < len(text) && text[end] >= '0' && text[end] <= '9' {
				end++
			}
			return text[index:end], index
		case text[index] == '@':
			end := index + 1
			for end < len(text) && isIdentifierByte(text[end]) {
				end++
			}
			if end > index+1 {
				return text[index:end], index
			}
			index++
		default:
			index++
		}
	}
	return "", -1
}

// labelFor maps a statement name to the label the production batch writer
// reports through its afterBatch hook.
func labelFor(statementName string) string {
	switch statementName {
	case nodeStatement:
		return "nodes"
	case dirtyNodeStatement:
		return "dirty nodes"
	case dirtyTargetStatement:
		return "dirty targets"
	case factStatement:
		return "facts"
	case edgeStatement:
		return "edges"
	default:
		return statementName
	}
}

func statsSectionFor(statementName string) string {
	switch statementName {
	case nodeStatement:
		return "nodes"
	case factStatement:
		return "facts"
	case edgeStatement:
		return "edges"
	default:
		return ""
	}
}

// findValuesKeyword returns the index just after the top-level VALUES keyword,
// skipping string literals and comments.
func findValuesKeyword(statement string) (int, bool) {
	index := 0
	for index < len(statement) {
		switch {
		case statement[index] == '\'':
			index = skipStringLiteral(statement, index)
		case strings.HasPrefix(statement[index:], "--"):
			for index < len(statement) && statement[index] != '\n' {
				index++
			}
		case strings.HasPrefix(statement[index:], "/*"):
			index += 2
			for index < len(statement) && !strings.HasPrefix(statement[index:], "*/") {
				index++
			}
			index = min(index+2, len(statement))
		case isIdentifierByte(statement[index]):
			end := index
			for end < len(statement) && isIdentifierByte(statement[end]) {
				end++
			}
			if strings.EqualFold(statement[index:end], "VALUES") {
				return end, true
			}
			index = end
		default:
			index++
		}
	}
	return 0, false
}

func skipStringLiteral(statement string, index int) int {
	index++ // opening quote
	for index < len(statement) {
		if statement[index] == '\'' {
			if index+1 < len(statement) && statement[index+1] == '\'' {
				index += 2 // doubled quote escapes within the literal
				continue
			}
			index++ // closing quote
			return index
		}
		index++
	}
	return index
}

// scanBalancedGroup returns the index just past the parenthesis group that
// starts at groupStart, respecting string literals and nested groups.
func scanBalancedGroup(statement string, groupStart int) (int, error) {
	depth := 0
	index := groupStart
	for index < len(statement) {
		switch {
		case statement[index] == '\'':
			index = skipStringLiteral(statement, index)
		case statement[index] == '(':
			depth++
			index++
		case statement[index] == ')':
			depth--
			index++
			if depth == 0 {
				return index, nil
			}
		default:
			index++
		}
	}
	return 0, fmt.Errorf("unbalanced values tuple")
}

// normalizeTuple rewrites every placeholder of one values tuple to a bare ?
// and returns the tuple with its bind count. Placeholders inside string
// literals stay literal.
func normalizeTuple(tuple string) (string, int) {
	var rewritten strings.Builder
	binds := 0
	index := 0
	for index < len(tuple) {
		switch {
		case tuple[index] == '\'':
			end := skipStringLiteral(tuple, index)
			rewritten.WriteString(tuple[index:end])
			index = end
		case tuple[index] == '?':
			index++
			for index < len(tuple) && tuple[index] >= '0' && tuple[index] <= '9' {
				index++
			}
			rewritten.WriteByte('?')
			binds++
		case tuple[index] == '@':
			end := index + 1
			for end < len(tuple) && isIdentifierByte(tuple[end]) {
				end++
			}
			if end == index+1 {
				rewritten.WriteByte(tuple[index])
				index++
				continue
			}
			rewritten.WriteByte('?')
			binds++
			index = end
		default:
			rewritten.WriteByte(tuple[index])
			index++
		}
	}
	return rewritten.String(), binds
}

type batchBuffer struct {
	spec  batchSpec
	rows  [][]any
	bytes int64
}

type batchWriter struct {
	execer        sqlExecer
	limits        batchLimits
	afterBatch    func(string, graph.WriteBatchStats)
	statementText map[string]string
	buffers       map[string]*batchBuffer
	writes        graph.WriteStats
	prepared      map[string]*sql.Stmt
}

func newBatchWriter(execer sqlExecer, limits batchLimits, statementText map[string]string) (*batchWriter, error) {
	if execer == nil {
		return nil, newBatchLimitsError("executor is nil")
	}
	if limits.MaxRows <= 0 {
		return nil, newBatchLimitsError("row limit must be positive")
	}
	if limits.MaxVariables <= 0 {
		return nil, newBatchLimitsError("variable limit must be positive")
	}
	if limits.MaxBytes <= 0 {
		return nil, newBatchLimitsError("byte limit must be positive")
	}
	writer := &batchWriter{execer: execer, limits: limits, statementText: statementText,
		buffers: make(map[string]*batchBuffer), prepared: make(map[string]*sql.Stmt)}
	for _, name := range []string{nodeStatement, dirtyNodeStatement, dirtyTargetStatement, factStatement, edgeStatement} {
		spec, err := parseBatchSpec(name, statementText[name])
		if err != nil {
			return nil, err
		}
		writer.buffers[name] = &batchBuffer{spec: spec}
	}
	return writer, nil
}

func newBatchLimitsError(message string) error {
	return fmt.Errorf("configure variant batch writer: %s", message)
}

func (writer *batchWriter) addNode(ctx context.Context, values []any) error {
	return writer.add(ctx, nodeStatement, values)
}

func (writer *batchWriter) addFact(ctx context.Context, values []any) error {
	return writer.add(ctx, factStatement, values)
}

func (writer *batchWriter) addDirtyNode(ctx context.Context, identifier string) error {
	return writer.add(ctx, dirtyNodeStatement, []any{identifier})
}

func (writer *batchWriter) addDirtyTarget(ctx context.Context, target, kind string) error {
	return writer.add(ctx, dirtyTargetStatement, []any{target, kind})
}

func (writer *batchWriter) addEdge(ctx context.Context, values []any) error {
	return writer.add(ctx, edgeStatement, values)
}

// AddStatement buffers one row of any named single-row statement, creating
// the buffer on first use; it is the writer surface a layout hook receives.
func (writer *batchWriter) AddStatement(ctx context.Context, statementName string, values ...any) error {
	return writer.add(ctx, statementName, values)
}

func (writer *batchWriter) add(ctx context.Context, statementName string, row []any) error {
	buffer, found := writer.buffers[statementName]
	if !found {
		spec, err := parseBatchSpec(statementName, writer.statementText[statementName])
		if err != nil {
			return err
		}
		buffer = &batchBuffer{spec: spec}
		writer.buffers[statementName] = buffer
	}
	if len(row) != buffer.spec.bindsPerRow {
		return fmt.Errorf("buffer %s row: got %d values, want %d",
			buffer.spec.name, len(row), buffer.spec.bindsPerRow)
	}
	if buffer.spec.bindsPerRow > writer.limits.MaxVariables {
		return fmt.Errorf("buffer %s row: %d values exceed SQLite variable limit %d",
			buffer.spec.name, buffer.spec.bindsPerRow, writer.limits.MaxVariables)
	}
	rowBytes := encodedValuesBytes(row)
	wouldOverflow := len(buffer.rows) > 0 &&
		(len(buffer.rows) >= writer.limits.MaxRows ||
			(len(buffer.rows)+1)*buffer.spec.bindsPerRow > writer.limits.MaxVariables ||
			buffer.bytes+rowBytes > writer.limits.MaxBytes)
	if wouldOverflow {
		if err := writer.flushBuffer(ctx, buffer); err != nil {
			return err
		}
	}
	buffer.rows = append(buffer.rows, row)
	buffer.bytes += rowBytes
	if rowBytes > writer.limits.MaxBytes {
		return writer.flushBuffer(ctx, buffer)
	}
	return nil
}

func (writer *batchWriter) flush(ctx context.Context) error {
	for _, name := range writer.flushOrder() {
		if err := writer.flushBuffer(ctx, writer.buffers[name]); err != nil {
			return err
		}
	}
	return nil
}

// flushOrder keeps the production buffer order first and appends any
// layout-defined buffers sorted by statement name, so flushing is
// deterministic for a given set of writes.
func (writer *batchWriter) flushOrder() []string {
	order := make([]string, 0, len(writer.buffers))
	fixed := []string{nodeStatement, dirtyNodeStatement, dirtyTargetStatement, factStatement, edgeStatement}
	inFixed := make(map[string]bool, len(fixed))
	for _, name := range fixed {
		if _, found := writer.buffers[name]; found {
			order = append(order, name)
			inFixed[name] = true
		}
	}
	remaining := make([]string, 0, len(writer.buffers))
	for name := range writer.buffers {
		if !inFixed[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(order, remaining...)
}

func (writer *batchWriter) flushBuffer(ctx context.Context, buffer *batchBuffer) error {
	if len(buffer.rows) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var statement strings.Builder
	statement.Grow(len(buffer.spec.prefix) + len(buffer.spec.suffix) + len(buffer.rows)*len(buffer.spec.rowTemplate))
	statement.WriteString(buffer.spec.prefix)
	arguments := make([]any, 0, len(buffer.rows)*buffer.spec.bindsPerRow)
	for rowIndex := range buffer.rows {
		if rowIndex > 0 {
			statement.WriteByte(',')
		}
		statement.WriteString(buffer.spec.rowTemplate)
		arguments = append(arguments, buffer.rows[rowIndex]...)
	}
	statement.WriteString(buffer.spec.suffix)
	if err := writer.exec(ctx, statement.String(), arguments); err != nil {
		return fmt.Errorf("write variant %s batch (%d rows, %d bytes): %w",
			buffer.spec.name, len(buffer.rows), buffer.bytes, err)
	}
	written := graph.WriteBatchStats{Batches: 1, Rows: int64(len(buffer.rows)), Bytes: buffer.bytes}
	switch buffer.spec.statsSection {
	case "nodes":
		addBatchStats(&writer.writes.Nodes, written)
	case "facts":
		addBatchStats(&writer.writes.Facts, written)
	case "edges":
		addBatchStats(&writer.writes.Edges, written)
	}
	if writer.afterBatch != nil {
		writer.afterBatch(buffer.spec.label, written)
	}
	buffer.rows = buffer.rows[:0]
	buffer.bytes = 0
	return nil
}

func (writer *batchWriter) exec(ctx context.Context, statement string, arguments []any) error {
	preparer, ok := writer.execer.(sqlPreparer)
	if !ok {
		_, err := writer.execer.ExecContext(ctx, statement, arguments...)
		return err
	}
	prepared := writer.prepared[statement]
	if prepared == nil {
		var err error
		prepared, err = preparer.PrepareContext(ctx, statement)
		if err != nil {
			return err
		}
		writer.prepared[statement] = prepared
	}
	_, err := prepared.ExecContext(ctx, arguments...)
	return err
}

func (writer *batchWriter) close() error {
	names := make([]string, 0, len(writer.prepared))
	for statement := range writer.prepared {
		names = append(names, statement)
	}
	sort.Strings(names)
	var first error
	for _, statement := range names {
		if err := writer.prepared[statement].Close(); err != nil && first == nil {
			first = err
		}
		delete(writer.prepared, statement)
	}
	return first
}

func (writer *batchWriter) stats() graph.WriteStats { return writer.writes }

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
