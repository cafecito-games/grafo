// Package sqlitedriver owns this project's SQLite driver choice and the few
// operations that cannot be expressed through database/sql.
//
// The driver is the C library through cgo rather than a pure-Go translation of
// it. Nothing is given up by that: the tree-sitter grammars already make cgo and
// a C toolchain mandatory on every release target, so a pure-Go SQLite bought no
// portability, while its emulated libc cost a measurable share of every indexing
// run. On the same index rebuild the C library used 15.4s of CPU against 27.9s.
package sqlitedriver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/identity"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// Name is the driver name every caller opens a graph database with. It is
// deliberately unchanged by the move to cgo so that no call site has to know
// which implementation is registered under it.
const Name = "sqlite"

func init() {
	sql.Register(Name, &sqlite3.SQLiteDriver{ConnectHook: registerFunctions})
}

// stableIDFunction is the SQL name of graph.StableID, and the identity functions
// are the SQL names of the storage encoding in
// internal/storage/sqlite/identity.
const (
	stableIDFunction     = "grafo_stable_id"
	identityBlobFunction = "grafo_identity_blob"
	identityTextFunction = "grafo_identity_text"
)

// registerFunctions exposes graph.StableID to SQL as grafo_stable_id(prefix,
// part...). A migration that has to produce an identity needs the exact bytes
// StableID hashes -- each part followed by a zero byte, SHA-256, the first 20 hex
// characters -- and SQLite offers no hash of its own to rebuild that from. Calling
// the Go function is what keeps the two from drifting: reimplementing the framing
// in SQL would put a second definition of identity in the schema, where nothing
// would catch it diverging from the first.
//
// It is registered on every connection because a migration runs on whichever one
// the pool hands out, and declared deterministic so SQLite may use it in an index
// or a partial-index predicate if a later migration needs that.
func registerFunctions(conn *sqlite3.SQLiteConn) error {
	for name, implementation := range map[string]any{
		stableIDFunction:     stableID,
		identityBlobFunction: identity.Encode,
		identityTextFunction: identity.Decode,
	} {
		if err := conn.RegisterFunc(name, implementation, true); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
	}
	return nil
}

// stableID adapts graph.StableID to SQLite's calling convention. A NULL or
// non-text part is an error rather than an empty string, because an identity
// quietly built from the wrong parts is worse than a failed migration.
func stableID(prefix string, parts ...any) (string, error) {
	text := make([]string, 0, len(parts))
	for index, part := range parts {
		switch value := part.(type) {
		case string:
			text = append(text, value)
		case []byte:
			text = append(text, string(value))
		default:
			return "", fmt.Errorf("%s: part %d is %T, want text", stableIDFunction, index+1, part)
		}
	}
	return graph.StableID(prefix, text...), nil
}

// Busy reports whether err is SQLite refusing an operation because another
// connection holds the lock it needs, which callers retry rather than surface.
func Busy(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrBusy
}

// Corrupt reports whether err is SQLite rejecting a file as damaged or as not a
// database at all, which is a different answer to a caller than a file it simply
// could not read.
func Corrupt(err error) bool {
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code == sqlite3.ErrCorrupt || sqliteErr.Code == sqlite3.ErrNotADB
}

// VariableLimit reports how many bound parameters one statement may carry on
// this connection. Batched writes are sized against it, and it is a property of
// the linked library rather than of the schema, so it is read rather than
// assumed.
func VariableLimit(ctx context.Context, db *sql.DB) (int, error) {
	connection, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = connection.Close() }()
	limit := 0
	if err := connection.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("connection is %T, not a SQLite connection", driverConn)
		}
		limit = sqliteConn.GetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER)
		return nil
	}); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, fmt.Errorf("driver reported invalid limit %d", limit)
	}
	return limit, nil
}
