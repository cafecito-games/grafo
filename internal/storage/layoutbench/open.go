package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/pressly/goose/v3"
	modernsqlite "modernc.org/sqlite"
)

// sqlHandle is the database/sql surface the statement layer runs against.
// Both *sql.DB and *sql.Tx satisfy it.
type sqlHandle interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	PrepareContext(context.Context, string) (*sql.Stmt, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// statementSet holds one prepared statement per named entry of a layout's
// statement text, mirroring the generated sqlc Prepare of the production
// adapter. Named @parameters are rewritten to numbered placeholders at prepare
// time so every call site binds positionally.
type statementSet struct {
	handle     sqlHandle
	text       map[string]string
	statements map[string]*sql.Stmt
}

func prepareStatementSet(ctx context.Context, handle sqlHandle, text map[string]string) (*statementSet, error) {
	set := &statementSet{handle: handle, text: text, statements: make(map[string]*sql.Stmt, len(text))}
	names := make([]string, 0, len(text))
	for name := range text {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rewritten, _ := rewriteNamedParameters(text[name])
		statement, err := handle.PrepareContext(ctx, rewritten)
		if err != nil {
			_ = set.Close()
			return nil, fmt.Errorf("prepare statement %s: %w", name, err)
		}
		set.statements[name] = statement
	}
	return set, nil
}

func (set *statementSet) Close() error {
	names := make([]string, 0, len(set.statements))
	for name := range set.statements {
		names = append(names, name)
	}
	sort.Strings(names)
	var first error
	for _, name := range names {
		if err := set.statements[name].Close(); err != nil && first == nil {
			first = fmt.Errorf("close statement %s: %w", name, err)
		}
	}
	return first
}

func (set *statementSet) exec(ctx context.Context, name string, values ...any) (sql.Result, error) {
	statement, err := set.statement(name)
	if err != nil {
		return nil, err
	}
	return statement.ExecContext(ctx, values...)
}

func (set *statementSet) query(ctx context.Context, name string, values ...any) (*sql.Rows, error) {
	statement, err := set.statement(name)
	if err != nil {
		return nil, err
	}
	return statement.QueryContext(ctx, values...)
}

func (set *statementSet) queryRow(ctx context.Context, name string, values ...any) (*sql.Row, error) {
	statement, err := set.statement(name)
	if err != nil {
		return nil, err
	}
	return statement.QueryRowContext(ctx, values...), nil
}

func (set *statementSet) statement(name string) (*sql.Stmt, error) {
	statement, found := set.statements[name]
	if !found {
		return nil, fmt.Errorf("layout statement %s is not defined", name)
	}
	return statement, nil
}

// transactionStatements runs the shared statements inside one transaction the
// way the production adapter does: each database-level prepared statement is
// borrowed by the transaction once and reused for the transaction's lifetime,
// so per-lookup work matches the sqlc WithTx reference.
type transactionStatements struct {
	transaction *sql.Tx
	shared      *statementSet
	borrowed    map[string]*sql.Stmt
}

func newTransactionStatements(transaction *sql.Tx, shared *statementSet) *transactionStatements {
	return &transactionStatements{transaction: transaction, shared: shared,
		borrowed: make(map[string]*sql.Stmt)}
}

func (bound *transactionStatements) exec(ctx context.Context, name string, values ...any) (sql.Result, error) {
	statement, err := bound.borrow(ctx, name)
	if err != nil {
		return nil, err
	}
	return statement.ExecContext(ctx, values...)
}

func (bound *transactionStatements) query(ctx context.Context, name string, values ...any) (*sql.Rows, error) {
	statement, err := bound.borrow(ctx, name)
	if err != nil {
		return nil, err
	}
	return statement.QueryContext(ctx, values...)
}

func (bound *transactionStatements) queryRow(ctx context.Context, name string, values ...any) (*sql.Row, error) {
	statement, err := bound.borrow(ctx, name)
	if err != nil {
		return nil, err
	}
	return statement.QueryRowContext(ctx, values...), nil
}

// ExecStatement runs one named statement on the transaction; it is the
// executor surface a PostEdgeDelete hook receives.
func (bound *transactionStatements) ExecStatement(ctx context.Context, statementName string, values ...any) error {
	statement, err := bound.borrow(ctx, statementName)
	if err != nil {
		return err
	}
	_, err = statement.ExecContext(ctx, values...)
	return err
}

func (bound *transactionStatements) borrow(ctx context.Context, name string) (*sql.Stmt, error) {
	if statement, found := bound.borrowed[name]; found {
		return statement, nil
	}
	shared, err := bound.shared.statement(name)
	if err != nil {
		return nil, err
	}
	statement := bound.transaction.StmtContext(ctx, shared)
	bound.borrowed[name] = statement
	return statement, nil
}

// OpenVariant opens (creating if needed) the SQLite database at path with the
// layout's Goose migrations and returns the shared variant adapter bound to
// the layout's statement text. It mirrors the production Open: same pragmas,
// WAL mode, prepared statements, batch limits, and schema metadata.
func OpenVariant(ctx context.Context, spec LayoutSpec, path string) (*VariantRepository, error) {
	text, err := spec.statementText()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open variant graph: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA busy_timeout=5000", "PRAGMA wal_autocheckpoint=1000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure SQLite: %w", err)
		}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, spec.Migrations)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate variant graph database: %w", err)
	}
	statements, err := prepareStatementSet(ctx, db, text)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare variant statements: %w", err)
	}
	variableLimit, err := activeVariableLimit(ctx, db)
	if err != nil {
		_ = statements.Close()
		_ = db.Close()
		return nil, fmt.Errorf("read SQLite variable limit: %w", err)
	}
	repository := &VariantRepository{db: db, statements: statements, statementText: text,
		spec: spec, path: path, limits: batchLimits{
			MaxRows: defaultBatchRows, MaxVariables: variableLimit, MaxBytes: defaultBatchBytes,
		}}
	if err := repository.SetMeta(ctx, "schema_version", fmt.Sprint(graph.SchemaVersion)); err != nil {
		_ = repository.Close()
		return nil, err
	}
	return repository, nil
}

func activeVariableLimit(ctx context.Context, db *sql.DB) (int, error) {
	connection, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = connection.Close() }()
	limit, err := modernsqlite.Limit(connection, sqliteLimitVariables, -1)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, fmt.Errorf("driver reported invalid limit %d", limit)
	}
	return limit, nil
}
