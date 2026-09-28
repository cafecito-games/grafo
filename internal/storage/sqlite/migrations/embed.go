package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// Files contains every ordered Goose migration shipped with Grafo.
//
//go:embed *.sql
var Files embed.FS

// LatestVersion derives the authoritative storage version from the embedded
// Goose migration names. Both writable migration and query-only validation use
// this source, so adding a migration cannot silently leave a stale reader
// constant behind.
func LatestVersion() (int64, error) {
	entries, err := fs.ReadDir(Files, ".")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var latest int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return 0, fmt.Errorf("migration %q has no numeric prefix", entry.Name())
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil || version <= 0 {
			return 0, fmt.Errorf("migration %q has invalid version", entry.Name())
		}
		latest = max(latest, version)
	}
	if latest == 0 {
		return 0, fmt.Errorf("no embedded migrations")
	}
	return latest, nil
}
