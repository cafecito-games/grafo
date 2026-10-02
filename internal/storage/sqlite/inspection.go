package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
	"github.com/cafecito-games/grafo/internal/storage/sqlitedriver"
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
	Metadata             IndexMetadata      `json:"metadata"`
	Metrics              *StorageMetrics    `json:"metrics,omitempty"`
	Compatibility        IndexCompatibility `json:"compatibility"`
	Diagnostic           string             `json:"diagnostic,omitempty"`
	CompactionDiagnostic string             `json:"compaction_diagnostic,omitempty"`
}

// InspectIndex opens an existing regular database in SQLite read-only mode and
// reports bounded evidence. Compatibility failures are data in the report;
// cancellation remains an operation error.
func InspectIndex(ctx context.Context, path string) (IndexInspection, error) {
	return inspectIndex(ctx, path, readStorageMetrics)
}

func inspectIndex(ctx context.Context, path string, metricsReader func(context.Context, *sql.DB) (StorageMetrics, error)) (IndexInspection, error) {
	// Without a WAL there is no newer committed state to discover, and immutable
	// mode prevents SQLite from creating empty WAL/SHM sidecars during a list or
	// dry-run inspection.
	db, err := openExistingIndex(ctx, path, readOnlyMode(path))
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
	metrics, err := metricsReader(ctx, db)
	if err != nil {
		if ctx.Err() != nil {
			return IndexInspection{}, ctx.Err()
		}
		return IndexInspection{
			Metadata:      metadata,
			Compatibility: CompatibilityUnverified,
			Diagnostic:    boundedInspectionDiagnostic(fmt.Sprintf("read SQLite storage metrics: %v", err)),
		}, nil
	}
	walMode, err := hasWALJournalHeader(path)
	if err != nil {
		if ctx.Err() != nil {
			return IndexInspection{}, ctx.Err()
		}
		return IndexInspection{
			Metadata:      metadata,
			Compatibility: CompatibilityUnverified,
			Diagnostic:    boundedInspectionDiagnostic(fmt.Sprintf("read compaction eligibility: %v", err)),
		}, nil
	}
	inspection := IndexInspection{Metadata: metadata, Metrics: &metrics, Compatibility: CompatibilityCompatible}
	if !walMode {
		inspection.CompactionDiagnostic = "current index does not use WAL journal mode; run 'grafo index' before compaction"
	}
	return inspection, nil
}

// hasWALJournalHeader reads SQLite's persistent write/read-version bytes.
// Using the file header keeps dry-run inspection strictly read-only: querying
// journal_mode through an immutable connection reports DELETE even for a WAL
// database, while a regular read-only connection may create WAL sidecars.
func hasWALJournalHeader(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open SQLite header: %w", err)
	}
	defer func() { _ = file.Close() }()
	var versions [2]byte
	if _, err := file.ReadAt(versions[:], 18); err != nil {
		if errors.Is(err, io.EOF) {
			return false, fmt.Errorf("read SQLite journal header: database header is truncated")
		}
		return false, fmt.Errorf("read SQLite journal header: %w", err)
	}
	return versions[0] == 2 && versions[1] == 2, nil
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
	db, err := sql.Open(sqlitedriver.Name, sqliteFileDSN(absolute, mode))
	if err != nil {
		return nil, fmt.Errorf("open index database: %w", err)
	}
	db.SetMaxOpenConns(1)
	// Lock policy before anything that can block. A writable inspection opens
	// against an index another grafo process may be refreshing, and the ping
	// below is the first statement that can meet that writer's lock.
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure index database busy timeout: %w", err)
	}
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
	// The driver otherwise applies a five-second busy timeout of its own, which
	// blocks inside the C library and so cannot be interrupted. Query-only paths
	// wait out contention through retrySQLiteBusy instead, which honors the
	// caller's context; the ones that want a timeout set the pragma themselves.
	query.Set("_busy_timeout", "0")
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
	if sqlitedriver.Corrupt(err) {
		compatibility = CompatibilityCorrupt
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
