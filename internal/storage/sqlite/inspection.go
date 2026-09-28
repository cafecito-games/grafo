package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
	modernsqlite "modernc.org/sqlite"
)

// IndexCompatibility is the bounded compatibility state used by branch-index
// inventory. Inspection never migrates or otherwise repairs a database.
type IndexCompatibility string

const (
	CompatibilityCompatible   IndexCompatibility = "compatible"
	CompatibilityIncompatible IndexCompatibility = "incompatible"
	CompatibilityCorrupt      IndexCompatibility = "corrupt"
	CompatibilityUnverified   IndexCompatibility = "unverified"

	maxInspectionDiagnostic = 240
)

// IndexMetadata is the identity and freshness evidence persisted only after a
// successful indexing pass.
type IndexMetadata struct {
	Root         string `json:"root,omitempty"`
	RepositoryID string `json:"repository_id,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Commit       string `json:"commit,omitempty"`
	IndexedAt    string `json:"indexed_at,omitempty"`
}

// IndexInspection reports metadata and compatibility without granting graph
// query or write capabilities.
type IndexInspection struct {
	Metadata      IndexMetadata      `json:"metadata"`
	Compatibility IndexCompatibility `json:"compatibility"`
	Diagnostic    string             `json:"diagnostic,omitempty"`
}

// InspectIndex opens an existing regular database in SQLite read-only mode and
// reports bounded evidence. Compatibility failures are data in the report;
// cancellation remains an operation error.
func InspectIndex(ctx context.Context, path string) (IndexInspection, error) {
	mode := "ro"
	if _, err := os.Lstat(path + "-wal"); errors.Is(err, os.ErrNotExist) {
		// Without a WAL there is no newer committed state to discover. Immutable
		// mode prevents SQLite from creating empty WAL/SHM sidecars during a
		// list or dry-run inspection.
		mode = "ro-immutable"
	}
	db, err := openExistingIndex(ctx, path, mode)
	if err != nil {
		if ctx.Err() != nil {
			return IndexInspection{}, ctx.Err()
		}
		return inspectionFailure(err), nil
	}
	defer func() { _ = db.Close() }()

	metadata, metadataErr := readIndexMetadata(ctx, db)
	repository := &Repository{db: db, queries: sqlcgen.New(db), path: path}
	compatibilityErr := validateReadCompatibility(ctx, repository)
	if err := errors.Join(metadataErr, compatibilityErr); err != nil {
		if ctx.Err() != nil {
			return IndexInspection{}, ctx.Err()
		}
		failure := inspectionFailure(err)
		failure.Metadata = metadata
		return failure, nil
	}
	return IndexInspection{Metadata: metadata, Compatibility: CompatibilityCompatible}, nil
}

// CheckpointIndex verifies that an existing compatible database still carries
// the expected identity evidence, then truncates its WAL. It never creates or
// migrates an index.
func CheckpointIndex(ctx context.Context, path string, expected IndexMetadata) (resultErr error) {
	db, err := openExistingIndex(ctx, path, "rw")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()

	repository := &Repository{db: db, queries: sqlcgen.New(db), path: path}
	if err := validateReadCompatibility(ctx, repository); err != nil {
		return err
	}
	actual, err := readIndexMetadata(ctx, db)
	if err != nil {
		return fmt.Errorf("read index metadata before checkpoint: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("index metadata changed before checkpoint")
	}
	var busy, logFrames, checkpointedFrames int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return fmt.Errorf("checkpoint SQLite WAL: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint SQLite WAL: database remained busy")
	}
	return nil
}

func openExistingIndex(ctx context.Context, path, mode string) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve index path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect index database: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("index path is not a regular non-symlink database file")
	}
	db, err := sql.Open("sqlite", sqliteFileDSN(absolute, mode))
	if err != nil {
		return nil, fmt.Errorf("open index database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open index database: %w", err)
	}
	return db, nil
}

func sqliteFileDSN(absolute, mode string) string {
	uriPath := filepath.ToSlash(absolute)
	if len(uriPath) >= 3 && uriPath[1] == ':' && uriPath[2] == '/' && uriPath[0] != '/' {
		uriPath = "/" + uriPath
	}
	query := url.Values{}
	if mode == "ro-immutable" {
		mode = "ro"
		query.Set("immutable", "1")
	}
	query.Set("mode", mode)
	return (&url.URL{Scheme: "file", Path: uriPath, RawQuery: query.Encode()}).String()
}

func readIndexMetadata(ctx context.Context, db *sql.DB) (IndexMetadata, error) {
	values := map[string]*string{}
	metadata := IndexMetadata{}
	values["root"] = &metadata.Root
	values["repository_id"] = &metadata.RepositoryID
	values["branch"] = &metadata.Branch
	values["commit"] = &metadata.Commit
	values["indexed_at"] = &metadata.IndexedAt
	for _, key := range []string{"root", "repository_id", "branch", "commit", "indexed_at"} {
		err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(values[key])
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return metadata, fmt.Errorf("read %s metadata: %w", key, err)
		}
	}
	return metadata, nil
}

func inspectionFailure(err error) IndexInspection {
	compatibility := CompatibilityUnverified
	var sqliteErr *modernsqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case 11, 26: // SQLITE_CORRUPT, SQLITE_NOTADB
			compatibility = CompatibilityCorrupt
		}
	}
	message := strings.ToLower(err.Error())
	if compatibility != CompatibilityCorrupt && (strings.Contains(message, "malformed") || strings.Contains(message, "not a database")) {
		compatibility = CompatibilityCorrupt
	}
	if compatibility == CompatibilityUnverified && strings.Contains(message, "incompatible index:") {
		compatibility = CompatibilityIncompatible
	}
	return IndexInspection{Compatibility: compatibility, Diagnostic: boundedInspectionDiagnostic(err.Error())}
}

func boundedInspectionDiagnostic(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) <= maxInspectionDiagnostic {
		return message
	}
	return message[:maxInspectionDiagnostic-3] + "..."
}
