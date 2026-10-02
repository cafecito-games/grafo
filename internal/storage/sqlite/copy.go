package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
)

// CopyIndex writes a transactionally consistent copy of an existing index
// database to destination.
//
// SQLite's VACUUM INTO is used rather than a filesystem copy because the source
// may be an index another Grafo process is writing: VACUUM INTO reads one
// snapshot under a read transaction, so the destination can never contain a torn
// page or a partially applied batch. It also rewrites the file without free
// pages, which matters because a long-lived branch index accumulates enough of
// them to carry hundreds of megabytes of reclaimable space.
//
// destination must not already exist; SQLite refuses to overwrite it, and that
// refusal is deliberately not worked around so a publish can never clobber an
// index another worktree is using.
func CopyIndex(ctx context.Context, source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("copy index: destination %s already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("copy index: inspect destination %s: %w", destination, err)
	}
	db, err := openExistingIndex(ctx, source, readOnlyMode(source))
	if err != nil {
		return fmt.Errorf("copy index: open source: %w", err)
	}
	defer func() { _ = db.Close() }()
	// The destination is bound rather than interpolated: an index path is
	// attacker-irrelevant but user-controlled, and a bound argument removes the
	// question of quoting a path containing an apostrophe.
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		return fmt.Errorf("copy index: vacuum %s into %s: %w", source, destination, err)
	}
	return nil
}

// SetIndexMeta writes meta values into an existing index database without
// migrating it or granting graph write capabilities. Keys are applied in sorted
// order so a failure part way through leaves a reproducible state.
//
// This is the narrow write counterpart to InspectIndex: it exists for callers
// that must correct an index's own description of itself, and it deliberately
// cannot reach any graph table.
func SetIndexMeta(ctx context.Context, path string, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	db, err := openExistingIndex(ctx, path, "rw")
	if err != nil {
		return fmt.Errorf("write index metadata: open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	for _, key := range keys {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			key, values[key]); err != nil {
			return fmt.Errorf("write index metadata %q in %s: %w", key, path, err)
		}
	}
	return nil
}

// readOnlyMode selects the strictest read-only mode that can still observe every
// committed transaction. Immutable mode avoids creating sidecars during a read,
// but it is only correct when no WAL holds committed frames the main database
// file does not.
func readOnlyMode(path string) string {
	if _, err := os.Lstat(path + "-wal"); errors.Is(err, os.ErrNotExist) {
		return "ro-immutable"
	}
	return "ro"
}
