package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexversion"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
	modernsqlite "modernc.org/sqlite"
)

// ReadRepository is the capability-safe SQLite adapter for existing indexes.
// It forwards only read methods from the shared adapter and therefore cannot
// satisfy graph.IndexRepository.
type ReadRepository struct {
	reader     *Repository
	statements *readStatementCache
	closeOnce  sync.Once
	closeErr   error
}

const maxReadStatements = 32

// readStatementCache lazily prepares only generated SQL that a read method
// actually executes. Generated sqlc queries remain the SQL owner, while this
// DBTX wrapper bounds both startup work and retained statement count.
type readStatementCache struct {
	db         *sql.DB
	mu         sync.Mutex
	statements map[string]*sql.Stmt
}

func newReadStatementCache(db *sql.DB) *readStatementCache {
	return &readStatementCache{db: db, statements: make(map[string]*sql.Stmt)}
}

func (c *readStatementCache) statement(ctx context.Context, query string) (*sql.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if statement := c.statements[query]; statement != nil {
		return statement, nil
	}
	if len(c.statements) >= maxReadStatements {
		return nil, nil
	}
	statement, err := c.db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.statements[query] = statement
	return statement, nil
}

func (c *readStatementCache) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("query-only SQLite handle does not execute writes")
}

func (c *readStatementCache) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	statement, err := c.statement(ctx, query)
	if statement == nil && err == nil {
		return c.db.PrepareContext(ctx, query)
	}
	return statement, err
}

func (c *readStatementCache) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	statement, err := c.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	if statement == nil {
		return c.db.QueryContext(ctx, query, args...)
	}
	return statement.QueryContext(ctx, args...)
}

func (c *readStatementCache) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	statement, err := c.statement(ctx, query)
	if err != nil || statement == nil {
		return c.db.QueryRowContext(ctx, query, args...)
	}
	return statement.QueryRowContext(ctx, args...)
}

func (c *readStatementCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var joined error
	for query, statement := range c.statements {
		if err := statement.Close(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("close read statement %q: %w", query, err))
		}
	}
	c.statements = nil
	return joined
}

var _ graph.ReadRepository = (*ReadRepository)(nil)
var _ graph.CatalogRepository = (*ReadRepository)(nil)
var _ graph.TopologyRepository = (*ReadRepository)(nil)
var _ graph.CanonicalMessageRepository = (*ReadRepository)(nil)
var _ graph.FileCatalog = (*ReadRepository)(nil)
var _ graph.ExternalEdgeRepository = (*ReadRepository)(nil)
var _ graph.ExternalNodeRepository = (*ReadRepository)(nil)
var _ semantic.CandidateRepository = (*ReadRepository)(nil)

// OpenReadOnly opens an existing, fully compatible index without creating,
// migrating, preparing writer statements, or mutating it.
func OpenReadOnly(ctx context.Context, path string) (*ReadRepository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve index path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, incompatibleIndex("index database does not exist")
		}
		return nil, fmt.Errorf("inspect index database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, incompatibleIndex("index path is not a regular database file")
	}

	dsn := readOnlyDSN(absolute)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open query-only graph: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := retrySQLiteBusy(ctx, func() error { return db.PingContext(ctx) }); err != nil {
		_ = db.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, incompatibleIndexCause("open database", err)
	}
	statements := newReadStatementCache(db)
	queries := sqlcgen.New(statements)
	shared := &Repository{db: db, queries: queries, path: absolute}
	if err := validateReadCompatibilityWithRetry(ctx, shared); err != nil {
		_ = statements.Close()
		_ = queries.Close()
		_ = db.Close()
		return nil, err
	}
	// This is connection-local lock policy, not a database write pragma. Set it
	// only after context-aware validation so a shorter open deadline wins.
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		_ = statements.Close()
		_ = queries.Close()
		_ = db.Close()
		return nil, fmt.Errorf("configure query-only busy timeout: %w", err)
	}
	return &ReadRepository{reader: shared, statements: statements}, nil
}

func readOnlyDSN(absolute string) string {
	return sqliteFileDSN(absolute, "ro")
}

func incompatibleIndex(reason string) error {
	return fmt.Errorf("incompatible index: %s; run 'grafo index <repository>'", reason)
}

func incompatibleIndexCause(reason string, cause error) error {
	return fmt.Errorf("incompatible index: %s: %w; run 'grafo index <repository>'", reason, cause)
}

func validateReadCompatibilityWithRetry(ctx context.Context, repository *Repository) error {
	return retrySQLiteBusy(ctx, func() error { return validateReadCompatibility(ctx, repository) })
}

func retrySQLiteBusy(ctx context.Context, operation func() error) error {
	for {
		err := operation()
		if err == nil || !sqliteBusy(err) {
			return err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func sqliteBusy(err error) bool {
	var sqliteErr *modernsqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5
}

func validateReadCompatibility(ctx context.Context, repository *Repository) error {
	if err := validateStorageCompatibility(ctx, repository.db); err != nil {
		return err
	}
	storedSchema, err := repository.Meta(ctx, "schema_version")
	if err != nil {
		return incompatibleIndexCause("read schema_version metadata", err)
	}
	if err := validateIntegerVersion("schema_version", storedSchema, int64(graph.SchemaVersion)); err != nil {
		return err
	}
	storedSemantic, err := repository.Meta(ctx, "semantic_index_version")
	if err != nil {
		return incompatibleIndexCause("read semantic_index_version metadata", err)
	}
	if strings.TrimSpace(storedSemantic) == "" {
		return incompatibleIndex("semantic_index_version metadata is missing")
	}
	if storedSemantic != indexversion.Semantic {
		if _, err := strconv.ParseUint(storedSemantic, 10, 64); err != nil {
			return incompatibleIndex(fmt.Sprintf("semantic_index_version %q is malformed", storedSemantic))
		}
		return incompatibleIndex(fmt.Sprintf("semantic_index_version is %q, want %q", storedSemantic, indexversion.Semantic))
	}
	return nil
}

func validateIntegerVersion(name, stored string, expected int64) error {
	if strings.TrimSpace(stored) == "" {
		return incompatibleIndex(name + " metadata is missing")
	}
	actual, err := strconv.ParseInt(stored, 10, 64)
	if err != nil || actual < 0 {
		return incompatibleIndex(fmt.Sprintf("%s %q is malformed", name, stored))
	}
	if actual != expected {
		return incompatibleIndex(fmt.Sprintf("%s is %d, want %d", name, actual, expected))
	}
	return nil
}

func validateStorageCompatibility(ctx context.Context, db *sql.DB) error {
	want, err := migrations.LatestVersion()
	if err != nil {
		return fmt.Errorf("resolve storage compatibility: %w", err)
	}
	var actual int64
	var applied bool
	if err := db.QueryRowContext(ctx,
		"SELECT version_id, is_applied FROM goose_db_version ORDER BY id DESC LIMIT 1",
	).Scan(&actual, &applied); err != nil {
		return incompatibleIndexCause("read migration version", err)
	}
	if actual != want || !applied {
		return incompatibleIndex(fmt.Sprintf("storage migration is %d (applied=%t), want %d", actual, applied, want))
	}
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table'")
	if err != nil {
		return incompatibleIndexCause("inspect database schema", err)
	}
	defer func() { _ = rows.Close() }()
	requiredNames := []string{"meta", "files", "paths", "nodes", "facts", "edges"}
	required := make(map[string]bool, len(requiredNames))
	for _, name := range requiredNames {
		required[name] = false
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return incompatibleIndexCause("inspect database schema", err)
		}
		if _, ok := required[name]; ok {
			required[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		return incompatibleIndexCause("inspect database schema", err)
	}
	for _, name := range requiredNames {
		if !required[name] {
			return incompatibleIndex(fmt.Sprintf("required table %q is missing", name))
		}
	}
	return nil
}

func (r *ReadRepository) Close() error {
	r.closeOnce.Do(func() { r.closeErr = errors.Join(r.statements.Close(), r.reader.Close()) })
	return r.closeErr
}

func (r *ReadRepository) Path() string { return r.reader.Path() }

func (r *ReadRepository) Meta(ctx context.Context, key string) (string, error) {
	return r.reader.Meta(ctx, key)
}
func (r *ReadRepository) Counts(ctx context.Context) (graph.Counts, error) {
	return r.reader.Counts(ctx)
}
func (r *ReadRepository) SearchNodes(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	return r.reader.SearchNodes(ctx, term, limit)
}
func (r *ReadRepository) MatchNodes(ctx context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
	return r.reader.MatchNodes(ctx, request)
}
func (r *ReadRepository) Node(ctx context.Context, id string) (graph.Node, error) {
	return r.reader.Node(ctx, id)
}
func (r *ReadRepository) EdgesFrom(ctx context.Context, id string) ([]graph.Edge, error) {
	return r.reader.EdgesFrom(ctx, id)
}
func (r *ReadRepository) EdgesTo(ctx context.Context, id string) ([]graph.Edge, error) {
	return r.reader.EdgesTo(ctx, id)
}
func (r *ReadRepository) ExternalRequestEdges(ctx context.Context, after string, limit int) (graph.ExternalRequestEdgePage, error) {
	return r.reader.ExternalRequestEdges(ctx, after, limit)
}
func (r *ReadRepository) Repositories(ctx context.Context) ([]string, error) {
	return r.reader.Repositories(ctx)
}
func (r *ReadRepository) ListNodesByKind(ctx context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	return r.reader.ListNodesByKind(ctx, request)
}
func (r *ReadRepository) CanonicalMessages(ctx context.Context, request graph.CanonicalMessageQuery) (graph.CanonicalMessagePage, error) {
	return r.reader.CanonicalMessages(ctx, request)
}
func (r *ReadRepository) RelationEdges(ctx context.Context, request graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	return r.reader.RelationEdges(ctx, request)
}
func (r *ReadRepository) ExternalNodesMatching(ctx context.Context, node graph.Node) ([]graph.Node, error) {
	return r.reader.ExternalNodesMatching(ctx, node)
}
func (r *ReadRepository) ExternalEdgesTo(ctx context.Context, node graph.Node) ([]graph.Edge, error) {
	return r.reader.ExternalEdgesTo(ctx, node)
}
func (r *ReadRepository) Files(ctx context.Context) (map[string]graph.FileRecord, error) {
	return r.reader.Files(ctx)
}
func (r *ReadRepository) CandidateNodes(ctx context.Context) ([]graph.Node, error) {
	return r.reader.CandidateNodes(ctx)
}
