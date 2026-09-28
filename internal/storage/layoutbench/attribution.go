// Package layoutbench captures SQLite storage attribution for the layout
// comparison spike. It is benchmark-only and must not be imported by
// production code paths.
package layoutbench

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// Attribution reports per-object and whole-file SQLite storage usage for one
// database at a point in its lifecycle.
type Attribution struct {
	DBStatSupported bool           `json:"dbstat_supported"`
	Objects         []ObjectBytes  `json:"objects,omitempty"`
	PageSize        int64          `json:"page_size"`
	PageCount       int64          `json:"page_count"`
	FreelistPages   int64          `json:"freelist_pages"`
	PrimaryBytes    int64          `json:"primary_bytes"` // os.Stat of the main DB file; object bytes reconcile with it only after a checkpoint flushes the WAL
	WALBytes        int64          `json:"wal_bytes"`
	SHMBytes        int64          `json:"shm_bytes"`
	PayloadStats    []TablePayload `json:"payload_stats,omitempty"`
	DBStatReason    string         `json:"dbstat_reason,omitempty"`
}

// ObjectBytes names a table, explicit index, or sqlite_autoindex_* with the
// bytes and pages dbstat attributes to it.
type ObjectBytes struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Pages int64  `json:"pages"`
}

// TablePayload summarizes representative payload lengths for one graph table.
type TablePayload struct {
	Table          string  `json:"table"`
	Rows           int64   `json:"rows"`
	AvgIDLength    float64 `json:"avg_id_length"`
	MaxIDLength    int64   `json:"max_id_length"`
	AvgOwnerLength float64 `json:"avg_owner_length,omitempty"`
	AvgProperties  float64 `json:"avg_properties_length,omitempty"`
}

// queryObjectBytes reads per-object byte and page totals from the dbstat
// virtual table. It is a variable so tests can simulate a build without
// dbstat support; it is not safe for concurrent mutation.
var queryObjectBytes = func(ctx context.Context, db *sql.DB) ([]ObjectBytes, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, SUM(pgsize) AS bytes, COUNT(*) AS pages FROM dbstat GROUP BY name ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("query dbstat: %w", err)
	}
	defer rows.Close()
	var objects []ObjectBytes
	for rows.Next() {
		var object ObjectBytes
		if err := rows.Scan(&object.Name, &object.Bytes, &object.Pages); err != nil {
			return nil, fmt.Errorf("scan dbstat row: %w", err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dbstat rows: %w", err)
	}
	return objects, nil
}

// payloadTables lists the graph tables whose payload lengths are worth
// reporting; edges has no owner_file column.
var payloadTables = []struct {
	name        string
	hasOwnerCol bool
}{
	{name: "nodes", hasOwnerCol: true},
	{name: "facts", hasOwnerCol: true},
	{name: "edges", hasOwnerCol: false},
}

// CaptureAttribution measures db. Whole-file metrics come from the SQLite
// pragmas and os.Stat of path, path+"-wal", and path+"-shm"; per-object
// metrics come from dbstat and degrade to a typed limitation when dbstat is
// unavailable rather than being guessed. Run Checkpoint first: while the WAL
// holds frames, dbstat reflects the merged logical view, so object byte
// totals diverge from PrimaryBytes (the main file size) by many pages.
// Payload stats assume the graph column contract (id, owner_file except on
// edges, properties) for any nodes/facts/edges tables present.
func CaptureAttribution(ctx context.Context, db *sql.DB, path string) (Attribution, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Attribution{}, fmt.Errorf("stat database file: %w", err)
	}
	attribution := Attribution{PrimaryBytes: info.Size()}
	if attribution.WALBytes, err = fileSize(path + "-wal"); err != nil {
		return Attribution{}, err
	}
	if attribution.SHMBytes, err = fileSize(path + "-shm"); err != nil {
		return Attribution{}, err
	}
	for _, pragma := range []struct {
		name        string
		destination *int64
	}{
		{"page_size", &attribution.PageSize},
		{"page_count", &attribution.PageCount},
		{"freelist_count", &attribution.FreelistPages},
	} {
		if err := db.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(pragma.destination); err != nil {
			return Attribution{}, fmt.Errorf("read %s: %w", pragma.name, err)
		}
	}
	objects, dbstatErr := queryObjectBytes(ctx, db)
	if dbstatErr != nil {
		attribution.DBStatReason = dbstatErr.Error()
	} else {
		attribution.DBStatSupported = true
		attribution.Objects = objects
	}
	payloadStats, err := capturePayloadStats(ctx, db)
	if err != nil {
		return Attribution{}, err
	}
	attribution.PayloadStats = payloadStats
	return attribution, nil
}

// Checkpoint flushes the WAL back into the main database. SQLite reports an
// incomplete checkpoint through the busy result column rather than an error:
// in PASSIVE mode a busy database may leave frames unflushed, which is
// expected and stays non-fatal; when truncate is true an incomplete
// checkpoint (busy != 0) means the WAL was not truncated, so it is an error.
func Checkpoint(ctx context.Context, db *sql.DB, truncate bool) error {
	statement := "PRAGMA wal_checkpoint(PASSIVE)"
	if truncate {
		statement = "PRAGMA wal_checkpoint(TRUNCATE)"
	}
	var busy, logFrames, checkpointedFrames int
	row := db.QueryRowContext(ctx, statement)
	if err := row.Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return fmt.Errorf("checkpoint SQLite WAL: %w", err)
	}
	if truncate && busy != 0 {
		return fmt.Errorf("checkpoint SQLite WAL: %s left the database busy", statement)
	}
	return nil
}

// Compact rebuilds the database file, releasing freelist pages. Callers
// capture attribution before and after to measure the reclaimed space.
func Compact(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("vacuum SQLite database: %w", err)
	}
	return nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil // a WAL or SHM file that was never created is size 0
	}
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return info.Size(), nil
}

func capturePayloadStats(ctx context.Context, db *sql.DB) ([]TablePayload, error) {
	var stats []TablePayload
	for _, table := range payloadTables {
		var present int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?",
			table.name,
		).Scan(&present); err != nil {
			return nil, fmt.Errorf("check %s existence: %w", table.name, err)
		}
		if present == 0 {
			continue
		}
		// The owner column is a literal 0 for edges so one statement shape and
		// one scan path serve every table.
		ownerAverage := "0"
		if table.hasOwnerCol {
			ownerAverage = "COALESCE(AVG(length(owner_file)), 0)"
		}
		statement := fmt.Sprintf(
			"SELECT COUNT(*), COALESCE(AVG(length(id)), 0), COALESCE(MAX(length(id)), 0), %s, COALESCE(AVG(length(properties)), 0) FROM %s",
			ownerAverage, table.name,
		)
		payload := TablePayload{Table: table.name}
		if err := db.QueryRowContext(ctx, statement).Scan(
			&payload.Rows, &payload.AvgIDLength, &payload.MaxIDLength,
			&payload.AvgOwnerLength, &payload.AvgProperties,
		); err != nil {
			return nil, fmt.Errorf("capture %s payload stats: %w", table.name, err)
		}
		stats = append(stats, payload)
	}
	return stats, nil
}
