// Package cache owns the user-level, content-addressed embedding cache.
package cache

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/semantic"
	_ "modernc.org/sqlite"
)

const (
	EnvPath       = "GRAFO_EMBED_CACHE"
	SchemaVersion = 1
	readBatchSize = 300
	busyTimeoutMS = 5000
	touchInterval = time.Hour
)

type Options struct {
	Now           func() time.Time
	TouchInterval time.Duration
}

type Cache struct {
	db            *sql.DB
	path          string
	now           func() time.Time
	touchInterval time.Duration
}

type ModelStatus struct {
	Model        string `json:"model"`
	Rows         int64  `json:"rows"`
	Dimensions   []int  `json:"dimensions"`
	BlobBytes    int64  `json:"blob_bytes"`
	OldestUsedAt string `json:"oldest_used_at,omitempty"`
	NewestUsedAt string `json:"newest_used_at,omitempty"`
}

type Status struct {
	Path             string        `json:"path"`
	SchemaVersion    int           `json:"schema_version"`
	Models           []ModelStatus `json:"models"`
	Rows             int64         `json:"rows"`
	BlobBytes        int64         `json:"blob_bytes"`
	DatabaseBytes    int64         `json:"database_bytes"`
	WALBytes         int64         `json:"wal_bytes"`
	SHMBytes         int64         `json:"shm_bytes"`
	ReclaimableBytes int64         `json:"reclaimable_bytes"`
	OldestUsedAt     string        `json:"oldest_used_at,omitempty"`
	NewestUsedAt     string        `json:"newest_used_at,omitempty"`
}

type PruneOptions struct {
	Model     string
	OlderThan time.Duration
	MaxBytes  *int64
	DryRun    bool
}

type PruneReport struct {
	DryRun             bool  `json:"dry_run"`
	DeletedRows        int64 `json:"deleted_rows"`
	FreedBlobBytes     int64 `json:"freed_blob_bytes"`
	RemainingRows      int64 `json:"remaining_rows"`
	RemainingBlobBytes int64 `json:"remaining_blob_bytes"`
	DatabaseBytes      int64 `json:"database_bytes"`
	WALBytes           int64 `json:"wal_bytes"`
	SHMBytes           int64 `json:"shm_bytes"`
	ReclaimableBytes   int64 `json:"reclaimable_bytes"`
}

func ResolvePath() (string, error) {
	if value, present := os.LookupEnv(EnvPath); present {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is set but empty", EnvPath)
		}
		return filepath.Abs(value)
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory (set %s to override): %w", EnvPath, err)
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("resolve user cache directory: empty path (set %s to override)", EnvPath)
	}
	return filepath.Join(root, "grafo", "embeddings.sqlite"), nil
}

func Open(ctx context.Context, path string) (*Cache, error) {
	return OpenWithOptions(ctx, path, Options{})
}

func OpenDefault(ctx context.Context) (*Cache, error) {
	path, err := ResolvePath()
	if err != nil {
		return nil, err
	}
	return Open(ctx, path)
}

// OpenReadOnly opens an existing compatible cache without creating files,
// changing permissions, checkpointing, or updating access timestamps.
func OpenReadOnly(ctx context.Context, path string) (*Cache, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("embedding cache path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve embedding cache path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect embedding cache path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("embedding cache path is a symlink: %s", absolute)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("embedding cache path is not a regular file: %s", absolute)
	}
	if err := rejectSymlinkParents(filepath.Dir(absolute)); err != nil {
		return nil, fmt.Errorf("inspect embedding cache parent: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open embedding cache read-only: %w", err)
	}
	database.SetMaxOpenConns(1)
	if _, err := database.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS)); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("configure embedding cache read-only: %w", err)
	}
	if err := validateSchema(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &Cache{db: database, path: absolute, now: time.Now, touchInterval: touchInterval}, nil
}

func OpenWithOptions(ctx context.Context, path string, options Options) (*Cache, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("embedding cache path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve embedding cache path: %w", err)
	}
	created, err := preparePath(absolute)
	if err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", absolute)
	if err != nil {
		return nil, fmt.Errorf("open embedding cache: %w", err)
	}
	database.SetMaxOpenConns(1)
	closeWith := func(cause error) (*Cache, error) {
		_ = database.Close()
		if created {
			_ = os.Remove(absolute)
			_ = os.Remove(absolute + "-wal")
			_ = os.Remove(absolute + "-shm")
		}
		return nil, cause
	}
	if created {
		if err := initialize(ctx, database); err != nil {
			return closeWith(err)
		}
	} else if err := validateSchema(ctx, database); err != nil {
		return closeWith(err)
	}
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS), "PRAGMA wal_autocheckpoint=1000"} {
		if _, err := database.ExecContext(ctx, pragma); err != nil {
			return closeWith(fmt.Errorf("configure embedding cache: %w", err))
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(absolute, 0o600); err != nil {
			return closeWith(fmt.Errorf("secure embedding cache file: %w", err))
		}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	interval := options.TouchInterval
	if interval == 0 {
		interval = touchInterval
	}
	if interval < 0 {
		return closeWith(errors.New("embedding cache touch interval cannot be negative"))
	}
	return &Cache{db: database, path: absolute, now: now, touchInterval: interval}, nil
}

func preparePath(path string) (bool, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("embedding cache path is a symlink: %s", path)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("embedding cache path is not a regular file: %s", path)
		}
		if err := rejectSymlinkParents(filepath.Dir(path)); err != nil {
			return false, fmt.Errorf("inspect embedding cache parent: %w", err)
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect embedding cache path: %w", err)
	}
	parent := filepath.Dir(path)
	_, parentErr := os.Lstat(parent)
	parentExisted := parentErr == nil
	if parentErr != nil && !errors.Is(parentErr, os.ErrNotExist) {
		return false, fmt.Errorf("inspect embedding cache parent: %w", parentErr)
	}
	if err := secureMkdirAll(parent); err != nil {
		return false, fmt.Errorf("create embedding cache directory: %w", err)
	}
	if !parentExisted && runtime.GOOS != "windows" {
		if err := os.Chmod(parent, 0o700); err != nil {
			return false, fmt.Errorf("secure embedding cache directory: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create embedding cache file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("close embedding cache file: %w", err)
	}
	return true, nil
}

func secureMkdirAll(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	current := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(remainder, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent component is a symlink: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent component is not a directory: %s", current)
		}
	}
	return nil
}

func rejectSymlinkParents(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	current := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(remainder, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent component is a symlink: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent component is not a directory: %s", current)
		}
	}
	return nil
}

func initialize(ctx context.Context, database *sql.DB) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("initialize embedding cache: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE embeddings (
		model TEXT NOT NULL,
		document_version TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		dimensions INTEGER NOT NULL CHECK (dimensions > 0),
		vector BLOB NOT NULL,
		created_at TEXT NOT NULL,
		last_used_at TEXT NOT NULL,
		PRIMARY KEY (model, document_version, content_hash)
	) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("create embedding cache schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "CREATE INDEX embeddings_last_used ON embeddings(last_used_at, model, document_version, content_hash)"); err != nil {
		return fmt.Errorf("create embedding cache index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("set embedding cache schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit embedding cache schema: %w", err)
	}
	return nil
}

func validateSchema(ctx context.Context, database *sql.DB) error {
	var version int
	if err := database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read embedding cache schema version: %w", err)
	}
	if version != SchemaVersion {
		return fmt.Errorf("incompatible embedding cache schema version %d, want %d", version, SchemaVersion)
	}
	var table string
	if err := database.QueryRowContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' AND name = 'embeddings'").Scan(&table); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("incompatible embedding cache: required table \"embeddings\" is missing")
		}
		return fmt.Errorf("validate embedding cache schema: %w", err)
	}
	type columnSpec struct {
		name     string
		typeName string
		notNull  int
		primary  int
	}
	want := []columnSpec{
		{name: "model", typeName: "TEXT", notNull: 1, primary: 1},
		{name: "document_version", typeName: "TEXT", notNull: 1, primary: 2},
		{name: "content_hash", typeName: "TEXT", notNull: 1, primary: 3},
		{name: "dimensions", typeName: "INTEGER", notNull: 1},
		{name: "vector", typeName: "BLOB", notNull: 1},
		{name: "created_at", typeName: "TEXT", notNull: 1},
		{name: "last_used_at", typeName: "TEXT", notNull: 1},
	}
	rows, err := database.QueryContext(ctx, "PRAGMA table_info(embeddings)")
	if err != nil {
		return fmt.Errorf("validate embedding cache columns: %w", err)
	}
	var got []columnSpec
	for rows.Next() {
		var cid int
		var column columnSpec
		var defaultValue any
		if err := rows.Scan(&cid, &column.name, &column.typeName, &column.notNull, &defaultValue, &column.primary); err != nil {
			_ = rows.Close()
			return fmt.Errorf("validate embedding cache columns: %w", err)
		}
		column.typeName = strings.ToUpper(column.typeName)
		got = append(got, column)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("validate embedding cache columns: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("validate embedding cache columns: %w", err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("incompatible embedding cache schema: embeddings has %d columns, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			return fmt.Errorf("incompatible embedding cache schema: column %d is %#v, want %#v", index, got[index], want[index])
		}
	}
	var indexCount int
	if err := database.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'embeddings_last_used'").Scan(&indexCount); err != nil {
		return fmt.Errorf("validate embedding cache index: %w", err)
	}
	if indexCount != 1 {
		return errors.New("incompatible embedding cache schema: required index \"embeddings_last_used\" is missing")
	}
	return nil
}

func (c *Cache) Close() error { return c.db.Close() }
func (c *Cache) Path() string { return c.path }

func (c *Cache) Load(ctx context.Context, keys []semantic.CacheKey) (map[semantic.CacheKey][]float32, error) {
	unique, err := normalizedKeys(keys)
	if err != nil {
		return nil, err
	}
	result := make(map[semantic.CacheKey][]float32, len(unique))
	var touch []semantic.CacheKey
	cutoff := c.now().UTC().Add(-c.touchInterval)
	for start := 0; start < len(unique); start += readBatchSize {
		end := min(start+readBatchSize, len(unique))
		query, arguments := loadQuery(unique[start:end])
		rows, err := c.db.QueryContext(ctx, query, arguments...)
		if err != nil {
			return nil, fmt.Errorf("load embedding cache: %w", err)
		}
		for rows.Next() {
			var key semantic.CacheKey
			var dimensions int64
			var blob []byte
			var lastUsed string
			if err := rows.Scan(&key.Model, &key.DocumentVersion, &key.ContentHash, &dimensions, &blob, &lastUsed); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("load embedding cache: %w", err)
			}
			vector, err := decodeVector(key, dimensions, blob)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			result[key] = vector
			usedAt, err := time.Parse(time.RFC3339Nano, lastUsed)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("embedding cache key %s has invalid last_used_at: %w", formatKey(key), err)
			}
			if c.touchInterval == 0 || usedAt.Before(cutoff) {
				touch = append(touch, key)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("load embedding cache: %w", err)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("load embedding cache: %w", err)
		}
	}
	if len(touch) > 0 {
		if err := c.touch(ctx, touch); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func normalizedKeys(keys []semantic.CacheKey) ([]semantic.CacheKey, error) {
	set := make(map[semantic.CacheKey]struct{}, len(keys))
	for _, key := range keys {
		if err := validateKey(key); err != nil {
			return nil, err
		}
		set[key] = struct{}{}
	}
	result := make([]semantic.CacheKey, 0, len(set))
	for key := range set {
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool { return lessKey(result[i], result[j]) })
	return result, nil
}

func loadQuery(keys []semantic.CacheKey) (string, []any) {
	clauses := make([]string, 0, len(keys))
	arguments := make([]any, 0, len(keys)*3)
	for _, key := range keys {
		clauses = append(clauses, "(model = ? AND document_version = ? AND content_hash = ?)")
		arguments = append(arguments, key.Model, key.DocumentVersion, key.ContentHash)
	}
	return `SELECT model, document_version, content_hash, dimensions, vector, last_used_at
		FROM embeddings WHERE ` + strings.Join(clauses, " OR ") +
		" ORDER BY model, document_version, content_hash", arguments
}

func (c *Cache) touch(ctx context.Context, keys []semantic.CacheKey) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("touch embedding cache: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stamp := c.now().UTC().Format(time.RFC3339Nano)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `UPDATE embeddings SET last_used_at = ?
			WHERE model = ? AND document_version = ? AND content_hash = ?`, stamp, key.Model, key.DocumentVersion, key.ContentHash); err != nil {
			return fmt.Errorf("touch embedding cache key %s: %w", formatKey(key), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit embedding cache access times: %w", err)
	}
	return nil
}

func (c *Cache) Store(ctx context.Context, entries []semantic.CacheEntry) error {
	unique := make(map[semantic.CacheKey][]float32, len(entries))
	for _, entry := range entries {
		if err := validateKey(entry.Key); err != nil {
			return err
		}
		if _, err := encodeVector(entry.Key, entry.Vector); err != nil {
			return err
		}
		if prior, exists := unique[entry.Key]; exists && !equalVectors(prior, entry.Vector) {
			return fmt.Errorf("embedding cache batch contains conflicting vectors for key %s", formatKey(entry.Key))
		}
		unique[entry.Key] = entry.Vector
	}
	keys := make([]semantic.CacheKey, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })
	if len(keys) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store embedding cache: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stamp := c.now().UTC().Format(time.RFC3339Nano)
	for _, key := range keys {
		vector := unique[key]
		blob, _ := encodeVector(key, vector)
		if _, err := tx.ExecContext(ctx, `INSERT INTO embeddings
			(model, document_version, content_hash, dimensions, vector, created_at, last_used_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(model, document_version, content_hash) DO UPDATE SET
				dimensions = excluded.dimensions,
				vector = excluded.vector,
				last_used_at = excluded.last_used_at`, key.Model, key.DocumentVersion, key.ContentHash,
			len(vector), blob, stamp, stamp); err != nil {
			return fmt.Errorf("store embedding cache key %s: %w", formatKey(key), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit embedding cache batch: %w", err)
	}
	return nil
}

func validateKey(key semantic.CacheKey) error {
	if strings.TrimSpace(key.Model) == "" {
		return errors.New("embedding cache model is required")
	}
	if strings.TrimSpace(key.DocumentVersion) == "" {
		return errors.New("embedding cache document version is required")
	}
	if len(key.ContentHash) != 64 {
		return fmt.Errorf("embedding cache content hash %q is not a SHA-256 hex digest", key.ContentHash)
	}
	for _, character := range key.ContentHash {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return fmt.Errorf("embedding cache content hash %q is not a lowercase SHA-256 hex digest", key.ContentHash)
		}
	}
	return nil
}

func encodeVector(key semantic.CacheKey, vector []float32) ([]byte, error) {
	if len(vector) == 0 {
		return nil, fmt.Errorf("embedding cache key %s has an empty vector", formatKey(key))
	}
	if uint64(len(vector)) > uint64(^uint(0)>>1)/4 {
		return nil, fmt.Errorf("embedding cache key %s dimensions overflow binary length", formatKey(key))
	}
	result := make([]byte, len(vector)*4)
	for index, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("embedding cache key %s contains a non-finite value", formatKey(key))
		}
		binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
	}
	return result, nil
}

func decodeVector(key semantic.CacheKey, dimensions int64, blob []byte) ([]float32, error) {
	if dimensions <= 0 || dimensions > int64(^uint(0)>>1)/4 || int64(len(blob)) != dimensions*4 {
		return nil, fmt.Errorf("embedding cache key %s has malformed vector length %d for %d dimensions", formatKey(key), len(blob), dimensions)
	}
	result := make([]float32, int(dimensions))
	for index := range result {
		value := math.Float32frombits(binary.LittleEndian.Uint32(blob[index*4:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("embedding cache key %s contains a non-finite value", formatKey(key))
		}
		result[index] = value
	}
	return result, nil
}

func (c *Cache) Status(ctx context.Context) (Status, error) {
	status := Status{Path: c.path, SchemaVersion: SchemaVersion, Models: []ModelStatus{}}
	rows, err := c.db.QueryContext(ctx, `SELECT model, dimensions, count(*), coalesce(sum(length(vector)), 0), min(last_used_at), max(last_used_at)
		FROM embeddings GROUP BY model, dimensions ORDER BY model, dimensions`)
	if err != nil {
		return Status{}, fmt.Errorf("read embedding cache status: %w", err)
	}
	byModel := map[string]int{}
	for rows.Next() {
		var model, oldest, newest string
		var dimensions int
		var count, bytes int64
		if err := rows.Scan(&model, &dimensions, &count, &bytes, &oldest, &newest); err != nil {
			_ = rows.Close()
			return Status{}, fmt.Errorf("read embedding cache status: %w", err)
		}
		index, exists := byModel[model]
		if !exists {
			index = len(status.Models)
			byModel[model] = index
			status.Models = append(status.Models, ModelStatus{Model: model})
		}
		item := &status.Models[index]
		item.Dimensions = append(item.Dimensions, dimensions)
		item.Rows += count
		item.BlobBytes += bytes
		if item.OldestUsedAt == "" || oldest < item.OldestUsedAt {
			item.OldestUsedAt = oldest
		}
		if newest > item.NewestUsedAt {
			item.NewestUsedAt = newest
		}
		status.Rows += count
		status.BlobBytes += bytes
		if status.OldestUsedAt == "" || oldest < status.OldestUsedAt {
			status.OldestUsedAt = oldest
		}
		if newest > status.NewestUsedAt {
			status.NewestUsedAt = newest
		}
	}
	if err := rows.Close(); err != nil {
		return Status{}, fmt.Errorf("read embedding cache status: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Status{}, fmt.Errorf("read embedding cache status: %w", err)
	}
	status.DatabaseBytes = fileSize(c.path)
	status.WALBytes = fileSize(c.path + "-wal")
	status.SHMBytes = fileSize(c.path + "-shm")
	var pageSize, freePages int64
	if err := c.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return Status{}, fmt.Errorf("read embedding cache page size: %w", err)
	}
	if err := c.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
		return Status{}, fmt.Errorf("read embedding cache free pages: %w", err)
	}
	status.ReclaimableBytes = pageSize * freePages
	return status, nil
}

type pruneRow struct {
	key   semantic.CacheKey
	bytes int64
}

func (c *Cache) Prune(ctx context.Context, options PruneOptions) (PruneReport, error) {
	if options.OlderThan < 0 {
		return PruneReport{}, errors.New("embedding cache prune age cannot be negative")
	}
	if options.MaxBytes != nil && *options.MaxBytes < 0 {
		return PruneReport{}, errors.New("embedding cache maximum bytes cannot be negative")
	}
	selected, freed, status, err := c.prunePlan(ctx, options)
	if err != nil {
		return PruneReport{}, err
	}
	report := PruneReport{DryRun: options.DryRun, DeletedRows: int64(len(selected)), FreedBlobBytes: freed,
		RemainingRows: status.Rows - int64(len(selected)), RemainingBlobBytes: status.BlobBytes - freed,
		DatabaseBytes: status.DatabaseBytes, WALBytes: status.WALBytes, SHMBytes: status.SHMBytes,
		ReclaimableBytes: status.ReclaimableBytes}
	if options.DryRun || len(selected) == 0 {
		return report, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return PruneReport{}, fmt.Errorf("prune embedding cache: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range selected {
		if _, err := tx.ExecContext(ctx, `DELETE FROM embeddings WHERE model = ? AND document_version = ? AND content_hash = ?`,
			row.key.Model, row.key.DocumentVersion, row.key.ContentHash); err != nil {
			return PruneReport{}, fmt.Errorf("prune embedding cache key %s: %w", formatKey(row.key), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PruneReport{}, fmt.Errorf("commit embedding cache prune: %w", err)
	}
	if _, err := c.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return PruneReport{}, fmt.Errorf("checkpoint embedding cache after prune: %w", err)
	}
	after, err := c.Status(ctx)
	if err != nil {
		return PruneReport{}, err
	}
	report.RemainingRows = after.Rows
	report.RemainingBlobBytes = after.BlobBytes
	report.DatabaseBytes = after.DatabaseBytes
	report.WALBytes = after.WALBytes
	report.SHMBytes = after.SHMBytes
	report.ReclaimableBytes = after.ReclaimableBytes
	return report, nil
}

func (c *Cache) prunePlan(ctx context.Context, options PruneOptions) ([]pruneRow, int64, Status, error) {
	status, err := c.Status(ctx)
	if err != nil {
		return nil, 0, Status{}, err
	}
	clauses := []string{"1 = 1"}
	arguments := []any{}
	if options.Model != "" {
		clauses = append(clauses, "model = ?")
		arguments = append(arguments, options.Model)
	}
	if options.OlderThan > 0 {
		clauses = append(clauses, "last_used_at < ?")
		arguments = append(arguments, c.now().UTC().Add(-options.OlderThan).Format(time.RFC3339Nano))
	}
	rows, err := c.db.QueryContext(ctx, `SELECT model, document_version, content_hash, length(vector)
		FROM embeddings WHERE `+strings.Join(clauses, " AND ")+` ORDER BY last_used_at, model, document_version, content_hash`, arguments...)
	if err != nil {
		return nil, 0, Status{}, fmt.Errorf("plan embedding cache prune: %w", err)
	}
	eligible := []pruneRow{}
	for rows.Next() {
		var row pruneRow
		if err := rows.Scan(&row.key.Model, &row.key.DocumentVersion, &row.key.ContentHash, &row.bytes); err != nil {
			_ = rows.Close()
			return nil, 0, Status{}, fmt.Errorf("plan embedding cache prune: %w", err)
		}
		eligible = append(eligible, row)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, Status{}, fmt.Errorf("plan embedding cache prune: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, Status{}, fmt.Errorf("plan embedding cache prune: %w", err)
	}
	needed := int64(math.MaxInt64)
	if options.MaxBytes != nil {
		needed = max(0, status.BlobBytes-*options.MaxBytes)
	}
	selected := make([]pruneRow, 0, len(eligible))
	var freed int64
	for _, row := range eligible {
		if freed >= needed {
			break
		}
		selected = append(selected, row)
		freed += row.bytes
	}
	return selected, freed, status, nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func lessKey(left, right semantic.CacheKey) bool {
	if left.Model != right.Model {
		return left.Model < right.Model
	}
	if left.DocumentVersion != right.DocumentVersion {
		return left.DocumentVersion < right.DocumentVersion
	}
	return left.ContentHash < right.ContentHash
}

func equalVectors(left, right []float32) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if math.Float32bits(left[index]) != math.Float32bits(right[index]) {
			return false
		}
	}
	return true
}

func formatKey(key semantic.CacheKey) string {
	return fmt.Sprintf("(%q,%q,%s)", key.Model, key.DocumentVersion, key.ContentHash)
}
