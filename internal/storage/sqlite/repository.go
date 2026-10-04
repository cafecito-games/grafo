package sqlite

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
	"github.com/cafecito-games/grafo/internal/storage/sqlitedriver"
	"github.com/pressly/goose/v3"
)

// Repository is the SQLite adapter for graph.Repository.
type Repository struct {
	db      *sql.DB
	queries *sqlcgen.Queries
	path    string
	limits  batchLimits

	writeStatsMu sync.Mutex
	writeStats   graph.WriteStats
	afterBatch   func(string, graph.WriteBatchStats)

	// pathKeys caches interned path keys across transactions so a cold index
	// resolves each repository-relative path once instead of per fact.
	pathKeys pathInterner

	// deferredIndexes holds the secondary indexes a bulk load currently has
	// dropped. It is owned by the single writer, like every other durable
	// mutation, and the stored ledger is the authority a new process reads.
	deferredIndexes []deferredIndex
}

// reconciliationBatchSize is how many dirty facts one reconciliation transaction
// materializes into edges.
//
// Node, fact, and edge identities are content hashes, so index entries arrive in
// random page order and one b-tree page is touched by many rows. In WAL mode a
// commit spools every page it dirtied into the log, so a page touched across N
// commits is written N times and across one commit is written once. The batch
// size therefore sets write amplification, not just how often the commit
// overhead is paid, which is why raising it is worth far more than the commit
// count alone suggests.
//
// Measured on a cold index of cafecito-games/uzir at 63664458a (11,845 indexed
// files, 634,245 nodes, 2,439,451 facts, 2,521,694 edges) on a 12-core machine,
// moving this together with maxGroupedFiles and maxGroupedRows in
// internal/indexer/service.go by the same factor:
//
//	factor   batch size   runs    wall      sd      sys   peak RSS
//	     1       10,000       3   184.6s    3.2s   58.2s   2.42 GiB
//	     4       40,000       2   126.0s    2.3s   26.7s   2.61 GiB
//	     8       80,000       4   114.6s    2.6s   20.2s   2.89 GiB
//	    16      160,000       3   110.4s    3.2s   17.4s   3.02 GiB
//
// Eight is the knee rather than an arbitrary stop: the step from eight to
// sixteen is 4.2s against a 2.6-3.2s standard deviation, for a further 0.13 GiB.
// The system time falling with the batch size, by two thirds across the range,
// is the write amplification being removed: it tracks bytes handed to the
// kernel. Every one of these twelve runs produced an identical graph.
//
// The run-to-run spread above is also the honest noise floor for this workload.
// The 14.8s recorded on maxParseWorkers in internal/indexer/pipeline.go predates
// the parse pool and the persisted Go view cache, and treating it as current
// hides effects this size.
//
// Peak RSS rises 19% in exchange, which is the cost of holding a larger
// transaction's dirty pages and a larger group's parsed evidence. That is inside
// the 20% gate the storage spike predeclared but it pushes directly on #182, so
// a future bound on resident memory has to treat this as one of the knobs it
// trades against.
//
// It is a variable so a test can lower it and observe more than one batch
// without generating a production-sized fact set.
var reconciliationBatchSize int64 = 80_000

const (
	// reconciliationCheckpointBatches is how many batches share one WAL
	// checkpoint. Checkpointing after every batch copied the same hot index pages
	// into the database file again and again, and both that and never
	// checkpointing at all measured worse than this. The explicit checkpoint is
	// what actually bounds the log here: wal_autocheckpoint fires on commit but
	// cannot restart a log the next transaction has already started appending to,
	// so under continuous batch writes the live log reaches 65-180 MiB between
	// these calls. Each one copies every frame it finds and has never reported
	// itself blocked.
	reconciliationCheckpointBatches = 16
	resolutionCacheSize             = 50_000
)

var _ graph.Repository = (*Repository)(nil)
var _ graph.BulkIndexRepository = (*Repository)(nil)
var _ graph.CatalogRepository = (*Repository)(nil)
var _ graph.CanonicalMessageRepository = (*Repository)(nil)
var _ semantic.CandidateRepository = (*Repository)(nil)

func Open(ctx context.Context, path string) (*Repository, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}
	db, err := sql.Open(sqlitedriver.Name, path)
	if err != nil {
		return nil, fmt.Errorf("open graph: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := configureWritableConnection(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate graph database: %w", err)
	}
	if err := validateStorageCompatibility(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("validate migrated graph database: %w", err)
	}
	// An interrupted bulk load leaves secondary indexes dropped, and the schema
	// names several of them in INDEXED BY clauses, so the repair has to happen
	// before any statement is prepared against it.
	if err := repairDeferredIndexes(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	queries, err := sqlcgen.Prepare(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare graph queries: %w", err)
	}
	variableLimit, err := sqlitedriver.VariableLimit(ctx, db)
	if err != nil {
		_ = queries.Close()
		_ = db.Close()
		return nil, fmt.Errorf("read SQLite variable limit: %w", err)
	}
	repository := &Repository{db: db, queries: queries, path: path, limits: batchLimits{
		MaxRows: defaultBatchRows, MaxVariables: variableLimit, MaxBytes: defaultBatchBytes,
	}}
	if err := repository.SetMeta(ctx, "schema_version", fmt.Sprint(graph.SchemaVersion)); err != nil {
		_ = repository.Close()
		return nil, err
	}
	return repository, nil
}

// pageCacheKiB caps the page cache of a writable connection. Indexing inserts
// node, fact, and edge rows keyed by content hash, so every secondary index
// receives its entries in random order and each one touches a different b-tree
// page. Under SQLite's default cache of 2 MiB a repository whose index outgrows
// it re-reads nearly every page it writes. The cap is a ceiling rather than a
// reservation: SQLite allocates pages on demand, so an idle connection holds
// almost nothing and only a connection that is actively rewriting a large graph
// approaches the limit.
const pageCacheKiB = 512 * 1024

// TargetPageSize is the page size every index is expected to carry. Larger
// pages hold more index entries per read and turn the random b-tree traffic
// above into fewer, larger I/O operations. SQLite only honors the pragma while
// the database is empty and not yet in WAL mode, so a newly created index
// adopts it directly; one created by an earlier version carries its own page
// size until compaction rewrites it.
const TargetPageSize = 16384

// temporaryStoreMemory keeps SQLite's statement journals in memory. Every
// batched node and fact write is an upsert, and an upsert is a statement that
// may have to undo part of itself without rolling back its transaction, so
// SQLite journals the pages it is about to overwrite. On disk that journal is
// written as one four-byte page number plus one page at a time: a quarter of a
// cold run's CPU went into those writes, for data that is discarded the moment
// the statement succeeds.
const temporaryStoreMemory = "PRAGMA temp_store=MEMORY"

// retainedLogBytes caps the write-ahead log file a restarted log leaves behind.
// Reconciliation materializes its whole fact queue in a single transaction, and
// nothing can checkpoint while a transaction is open, so the log has to grow to
// hold it: on a 12k-file repository to just over half a gigabyte. Without a limit
// SQLite then reuses that allocation in place and the file keeps that peak for the
// rest of the run even though a checkpoint has copied every frame out of it.
//
// The cap is comfortably above the log the batch loop keeps live between
// checkpoints, measured at 150-175 MiB, because the limit is applied by
// truncating the file: a cap below the working set makes every restart truncate a
// log that is about to be grown straight back, which measured 7% slower on the
// reconciliation phase. It is only there to hand back what one oversized
// transaction forced.
//
// Raising reconciliationBatchSize eightfold did not move that working set, and
// the observation that suggested it had is worth recording so it is not read the
// same way twice. Sampling the log file every 250ms through a cold index of a
// 12.7k-file repository: persistence sawtooths to a live peak of 151.4 MiB, still
// inside the range above; at t=69s of a 121s run the queue transaction takes it to
// 529.1 MiB; the cap truncates it to 256 MiB once, and the file stays pinned there
// for the rest of the run. A file sitting at exactly this constant is therefore the
// retained allocation, not the live log reaching a ceiling -- and a capped file
// that never exceeds the cap is itself the evidence the live set stays under it.
//
// Raising the cap was measured and rejected: at 4 GiB the peak log is identical to
// the byte, because journal_size_limit bounds only what a restart leaves behind and
// never what an open transaction may grow, so the only effect is that the file holds
// 529 MiB instead of 256 MiB for the last 50s. Three interleaved pairs put wall
// clock and reconciliation inside a +-10s drift with no consistent sign. The
// truncate-and-regrow cost also needs many restarts to matter, and a cold run has
// one explicit checkpoint inside the batch loop at 31 batches and
// reconciliationCheckpointBatches of 16.
//
// The oversized transaction is the thing actually worth removing, which is #183.
const retainedLogBytes = 256 << 20

// writableConnectionPragmas configures a writable connection. busy_timeout is
// deliberately first: it is pure connection-local lock policy that cannot
// block, while journal_mode=WAL needs the database lock and therefore fails
// immediately with SQLITE_BUSY_RECOVERY when another writer holds it and the
// timeout is still SQLite's default of zero. page_size has to precede
// journal_mode because SQLite ignores it once a database is in WAL mode, and
// journal_size_limit has to follow it because the limit applies to the log of a
// database that is already in WAL mode.
var writableConnectionPragmas = []string{
	"PRAGMA busy_timeout=5000",
	fmt.Sprintf("PRAGMA page_size=%d", TargetPageSize),
	fmt.Sprintf("PRAGMA cache_size=-%d", pageCacheKiB),
	temporaryStoreMemory,
	"PRAGMA journal_mode=WAL",
	"PRAGMA synchronous=NORMAL",
	"PRAGMA wal_autocheckpoint=1000",
	fmt.Sprintf("PRAGMA journal_size_limit=%d", retainedLogBytes),
}

func configureWritableConnection(ctx context.Context, db *sql.DB) error {
	for _, pragma := range writableConnectionPragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure SQLite: %w", err)
		}
	}
	return nil
}

func (r *Repository) Close() error { return errors.Join(r.queries.Close(), r.db.Close()) }
func (r *Repository) Path() string { return r.path }

func (r *Repository) WriteStats() graph.WriteStats {
	r.writeStatsMu.Lock()
	defer r.writeStatsMu.Unlock()
	return r.writeStats
}

func (r *Repository) SetMeta(ctx context.Context, key, value string) error {
	return r.queries.SetMeta(ctx, sqlcgen.SetMetaParams{Key: key, Value: value})
}

func (r *Repository) Meta(ctx context.Context, key string) (string, error) {
	value, err := r.queries.GetMeta(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (r *Repository) CandidateNodes(ctx context.Context) ([]graph.Node, error) {
	rows, err := r.queries.ListSemanticCandidateNodes(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		result = append(result, nodeFromRow(row))
	}
	return result, nil
}

func (r *Repository) Files(ctx context.Context) (map[string]graph.FileRecord, error) {
	rows, err := r.queries.ListFiles(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]graph.FileRecord, len(rows))
	for _, row := range rows {
		result[row.Path] = graph.FileRecord{Path: row.Path, Hash: row.Hash, Language: row.Language,
			Size: row.Size, ModifiedNS: row.ModifiedNs, IndexedAt: row.IndexedAt,
			EvidenceDigest: row.EvidenceDigest}
	}
	return result, nil
}

func (r *Repository) ReplaceFile(ctx context.Context, file graph.FileRecord, parsed graph.ParseResult) error {
	if err := validateParseResult(parsed); err != nil {
		return err
	}
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		return replaceFile(ctx, q, writer, file, parsed)
	})
}

// ReplaceFiles applies several file replacements in one transaction. A cold run
// replaces every file in the repository, and one transaction per file meant one
// commit, one WAL header, and one freshly prepared statement set per file.
func (r *Repository) ReplaceFiles(ctx context.Context, replacements []graph.FileReplacement) error {
	for _, replacement := range replacements {
		if err := validateParseResult(replacement.Parsed); err != nil {
			return err
		}
	}
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		for _, replacement := range replacements {
			if err := replaceFile(ctx, q, writer, replacement.File, replacement.Parsed); err != nil {
				return err
			}
		}
		return nil
	})
}

func replaceFile(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter,
	file graph.FileRecord, parsed graph.ParseResult) error {
	if err := markOwnerDirty(ctx, q, file.Path); err != nil {
		return err
	}
	if err := q.DeleteEdgesByOwnerFacts(ctx, file.Path); err != nil {
		return err
	}
	if err := q.DeleteFactsByOwner(ctx, file.Path); err != nil {
		return err
	}
	if err := q.DeleteNodesByOwner(ctx, file.Path); err != nil {
		return err
	}
	if err := upsertFileRecord(ctx, q, file); err != nil {
		return err
	}
	return insertParseResult(ctx, q, writer, parsed)
}

func (r *Repository) ReplaceOwner(ctx context.Context, owner string, parsed graph.ParseResult) error {
	if err := validateParseResult(parsed); err != nil {
		return err
	}
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		if err := markOwnerDirty(ctx, q, owner); err != nil {
			return err
		}
		if err := q.DeleteEdgesByOwnerFacts(ctx, owner); err != nil {
			return err
		}
		if err := q.DeleteFactsByOwner(ctx, owner); err != nil {
			return err
		}
		if err := q.DeleteNodesByOwner(ctx, owner); err != nil {
			return err
		}
		return insertParseResult(ctx, q, writer, parsed)
	})
}

func validateParseResult(parsed graph.ParseResult) error {
	for index, fact := range parsed.Facts {
		if err := fact.ValidateSourceLocator(); err != nil {
			return fmt.Errorf("validate fact %d: %w", index, err)
		}
	}
	return nil
}

func (r *Repository) RemoveFiles(ctx context.Context, paths []string) error {
	if err := r.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		for _, path := range paths {
			if err := markOwnerDirty(ctx, q, path); err != nil {
				return err
			}
			if err := q.DeleteEdgesByOwnerFacts(ctx, path); err != nil {
				return err
			}
			if err := q.DeleteFactsByOwner(ctx, path); err != nil {
				return err
			}
			if err := q.DeleteNodesByOwner(ctx, path); err != nil {
				return err
			}
			if err := q.DeleteFile(ctx, path); err != nil {
				return err
			}
		}
		// A removed file leaves its interned path row referenced by nothing.
		return q.DeleteUnreferencedPaths(ctx)
	}); err != nil {
		return err
	}
	// Pruning can delete a row whose key the shared cache still remembers.
	r.pathKeys.discard()
	return nil
}

func upsertFileRecord(ctx context.Context, q *sqlcgen.Queries, file graph.FileRecord) error {
	return q.UpsertFile(ctx, sqlcgen.UpsertFileParams{Path: file.Path, Hash: file.Hash,
		Language: file.Language, Size: file.Size, ModifiedNs: file.ModifiedNS,
		IndexedAt: file.IndexedAt, EvidenceDigest: file.EvidenceDigest})
}

// UpdateFileRecord records a file's inputs without touching the evidence it
// already contributed. It exists for a file that was reparsed and produced the
// evidence the index already holds: the inputs that selected it did change and
// must be recorded, or the file is reselected and reparsed on every later run,
// while its rows and its reconciliation fan-out are left alone.
//
// It deliberately does not call markOwnerDirty. The caller has established that
// this file's evidence is unchanged, so there is nothing for the reconciler to
// resolve on its behalf.
func (r *Repository) UpdateFileRecord(ctx context.Context, file graph.FileRecord) error {
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		return upsertFileRecord(ctx, q, file)
	})
}

func insertParseResult(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, parsed graph.ParseResult) error {
	for _, node := range parsed.Nodes {
		external := int64(0)
		if node.External {
			external = 1
		}
		if err := writer.addNode(ctx, nodeParams(node, external)); err != nil {
			return fmt.Errorf("upsert node %s: %w", node.QualifiedName, err)
		}
		if !node.External {
			if err := markNodeDirty(ctx, writer, node); err != nil {
				return err
			}
		}
	}
	for _, fact := range parsed.Facts {
		params, err := factParams(ctx, q, writer, fact)
		if err != nil {
			return fmt.Errorf("upsert fact %s: %w", fact.ID, err)
		}
		if err := writer.addFact(ctx, params); err != nil {
			return fmt.Errorf("upsert fact %s: %w", fact.ID, err)
		}
	}
	return nil
}

func (r *Repository) Reconcile(ctx context.Context) error {
	_, err := r.ReconcileWithStats(ctx, nil)
	return err
}

// ReconciliationPending reports whether any durable resolver queue contains
// work. The indexer uses this adapter-owned proof to skip reconciliation; it
// never reaches into SQLite-specific tables itself.
func (r *Repository) ReconciliationPending(ctx context.Context) (bool, error) {
	return r.queries.ReconciliationPending(ctx)
}

func (r *Repository) ReconcileWithStats(ctx context.Context, observer graph.ReconciliationObserver) (graph.ReconciliationStats, error) {
	var stats graph.ReconciliationStats
	// Resolution reads nodes and facts and names several of their indexes in
	// INDEXED BY clauses, so those have to be back before the first batch. The
	// edge indexes deliberately stay deferred: this loop is what fills the edge
	// table, and they are the widest indexes in the schema.
	if err := r.rebuildDeferredIndexes(ctx, func(index deferredIndex) bool {
		return index.Table != edgeTable
	}); err != nil {
		return stats, err
	}
	if err := r.queueDirtyFacts(ctx); err != nil {
		return stats, fmt.Errorf("queue dirty facts: %w", err)
	}

	resolved := newResolutionCache(resolutionCacheSize)
	for {
		before := r.WriteStats()
		processed, err := r.reconcileBatch(ctx, resolved)
		if err != nil {
			return stats, fmt.Errorf("reconcile fact batch: %w", err)
		}
		if processed == 0 {
			break
		}
		stats.Batches++
		addWriteStats(&stats.Writes, subtractWriteStats(r.WriteStats(), before))
		if observer != nil {
			if err := observer(stats); err != nil {
				return stats, err
			}
		}
		if stats.Batches%reconciliationCheckpointBatches == 0 {
			if err := r.checkpoint(ctx, false); err != nil {
				return stats, err
			}
		}
	}
	// Every edge is written, so the edge indexes can be built in one sorted pass.
	// The cleanup below needs them: it looks for external nodes that no edge
	// mentions, which without them would scan the whole edge table per node.
	if err := r.EndBulkLoad(ctx); err != nil {
		return stats, err
	}
	cleanupPending, err := r.queries.ReconciliationCleanupPending(ctx)
	if err != nil {
		return stats, fmt.Errorf("check reconciliation cleanup: %w", err)
	}
	if !cleanupPending {
		return stats, nil
	}
	if err := r.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		if err := q.DeleteOrphanExternalNodes(ctx); err != nil {
			return err
		}
		// Replacing an owner can leave an interned path row referenced by
		// nothing. Sweeping once per converged pass is driven by the small
		// paths table, so it stays proportional to the indexed file set.
		if err := q.DeleteUnreferencedPaths(ctx); err != nil {
			return err
		}
		return q.ClearReconciliationCleanup(ctx)
	}); err != nil {
		return stats, fmt.Errorf("remove orphan external nodes: %w", err)
	}
	r.pathKeys.discard()
	return stats, r.checkpoint(ctx, true)
}

func (r *Repository) queueDirtyFacts(ctx context.Context) error {
	return r.inTransaction(ctx, func(q *sqlcgen.Queries, _ *batchWriter) error {
		if err := q.PruneDirtyFacts(ctx); err != nil {
			return err
		}
		if err := q.EnqueueDirtyFacts(ctx); err != nil {
			return err
		}
		if err := q.MarkReconciliationCleanup(ctx); err != nil {
			return err
		}
		if err := q.ClearDirtyOwners(ctx); err != nil {
			return err
		}
		if err := q.ClearDirtyNodes(ctx); err != nil {
			return err
		}
		return q.ClearDirtyTargets(ctx)
	})
}

func (r *Repository) reconcileBatch(ctx context.Context, resolved *resolutionCache) (int, error) {
	processed := 0
	err := r.inTransaction(ctx, func(q *sqlcgen.Queries, writer *batchWriter) error {
		facts, err := q.ListDirtyFactBatch(ctx, reconciliationBatchSize)
		if err != nil {
			return err
		}
		if len(facts) == 0 {
			return nil
		}
		if err := q.DeleteEdgesByDirtyFactBatch(ctx, reconciliationBatchSize); err != nil {
			return err
		}
		nodeCache := map[string]graph.Node{}
		loadNode := func(id string) (graph.Node, error) {
			if node, ok := nodeCache[id]; ok {
				return node, nil
			}
			row, err := q.GetNode(ctx, id)
			if err != nil {
				return graph.Node{}, err
			}
			node := nodeFromRow(row)
			nodeCache[id] = node
			return node, nil
		}
		for _, row := range facts {
			processed++
			fact, err := factFromDirtyRow(row)
			if err != nil {
				return err
			}
			sources, err := resolveSources(ctx, q, writer, fact, row.SourceExists != 0, resolved)
			if err != nil {
				return err
			}
			targetResult, err := resolveTargets(ctx, q, writer, fact, row.TargetExists != 0, resolved)
			if err != nil {
				return err
			}
			targets := targetResult.targets
			edgeProperties := fact.Properties
			if targetResult.properties != nil {
				edgeProperties = targetResult.properties
			}
			for _, source := range sources {
				for _, target := range targets {
					edge := graph.Edge{ID: graph.EdgeID(fact.ID, target), FactID: fact.ID,
						FromID: source, ToID: target, Kind: fact.Kind, Producer: fact.Producer, Location: fact.Location,
						Properties: edgeProperties}
					if err := writer.addEdge(ctx, edgeParams(edge)); err != nil {
						return err
					}
					if edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeReferences {
						continue
					}
					sourceNode, sourceErr := loadNode(source)
					if errors.Is(sourceErr, sql.ErrNoRows) {
						continue // a newly materialized unresolved source cannot be a local test
					}
					if sourceErr != nil {
						return sourceErr
					}
					if sourceNode.Kind != graph.KindTest {
						continue
					}
					targetNode, targetErr := loadNode(target)
					if errors.Is(targetErr, sql.ErrNoRows) {
						continue // a newly materialized unresolved target is never production evidence
					}
					if targetErr != nil {
						return targetErr
					}
					if testEdge, ok := graph.DirectTestEdge(sourceNode, targetNode, edge); ok {
						if err := writer.addEdge(ctx, edgeParams(testEdge)); err != nil {
							return err
						}
					}
				}
			}
		}
		if err := writer.flush(ctx); err != nil {
			return err
		}
		return q.DeleteDirtyFactBatch(ctx, reconciliationBatchSize)
	})
	return processed, err
}

func (r *Repository) checkpoint(ctx context.Context, truncate bool) error {
	statement := "PRAGMA wal_checkpoint(PASSIVE)"
	if truncate {
		statement = "PRAGMA wal_checkpoint(TRUNCATE)"
	}
	var busy, logFrames, checkpointedFrames int
	row := r.db.QueryRowContext(ctx, statement)
	if err := row.Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return fmt.Errorf("checkpoint SQLite WAL: %w", err)
	}
	return nil
}

func markOwnerDirty(ctx context.Context, q *sqlcgen.Queries, owner string) error {
	if err := q.MarkDirtyOwner(ctx, owner); err != nil {
		return err
	}
	if err := q.MarkOwnedNodesDirty(ctx, owner); err != nil {
		return err
	}
	return q.MarkOwnedNamesDirty(ctx, sqlcgen.MarkOwnedNamesDirtyParams{OwnerFile: owner, OwnerFile_2: owner})
}

func markNodeDirty(ctx context.Context, writer *batchWriter, node graph.Node) error {
	if err := writer.addDirtyNode(ctx, node.ID); err != nil {
		return err
	}
	for _, target := range []string{node.Name, node.QualifiedName} {
		if target == "" {
			continue
		}
		if err := writer.addDirtyTarget(ctx, target, string(node.Kind)); err != nil {
			return err
		}
	}
	return nil
}

func subtractWriteStats(after, before graph.WriteStats) graph.WriteStats {
	return graph.WriteStats{
		Nodes: subtractBatchStats(after.Nodes, before.Nodes),
		Facts: subtractBatchStats(after.Facts, before.Facts),
		Edges: subtractBatchStats(after.Edges, before.Edges),
	}
}

func subtractBatchStats(after, before graph.WriteBatchStats) graph.WriteBatchStats {
	return graph.WriteBatchStats{
		Batches: max(0, after.Batches-before.Batches),
		Rows:    max(0, after.Rows-before.Rows),
		Bytes:   max(0, after.Bytes-before.Bytes),
	}
}

type endpointDirection uint8

const (
	targetEndpoint endpointDirection = iota
	sourceEndpoint
)

type resolutionKey struct {
	direction  endpointDirection
	target     string
	targetKind graph.NodeKind
	edgeKind   graph.EdgeKind
	authority  httpmodel.AuthorityState
	method     string
	route      string
}

type resolutionCacheEntry struct {
	key     resolutionKey
	targets []string
}

type resolutionCache struct {
	capacity              int
	entries               map[resolutionKey]*list.Element
	recent                *list.List
	requests              map[resolutionKey]requestCacheState
	endpointNodes         []graph.Node
	endpointCatalog       httpmodel.CandidateCatalog
	endpointCatalogLoaded bool
}

type requestCacheState struct {
	resolution httpmodel.DestinationResolution
	evidence   httpmodel.DestinationEvidence
}

func newResolutionCache(capacity int) *resolutionCache {
	return &resolutionCache{capacity: capacity, entries: make(map[resolutionKey]*list.Element, capacity),
		recent: list.New(), requests: make(map[resolutionKey]requestCacheState, capacity)}
}

func (c *resolutionCache) get(key resolutionKey) ([]string, bool) {
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.recent.MoveToFront(element)
	return element.Value.(resolutionCacheEntry).targets, true
}

func (c *resolutionCache) set(key resolutionKey, targets []string) {
	if element, ok := c.entries[key]; ok {
		element.Value = resolutionCacheEntry{key: key, targets: targets}
		c.recent.MoveToFront(element)
		return
	}
	element := c.recent.PushFront(resolutionCacheEntry{key: key, targets: targets})
	c.entries[key] = element
	if c.recent.Len() <= c.capacity {
		return
	}
	oldest := c.recent.Back()
	oldestKey := oldest.Value.(resolutionCacheEntry).key
	delete(c.entries, oldestKey)
	delete(c.requests, oldestKey)
	c.recent.Remove(oldest)
}

func (c *resolutionCache) getRequest(key resolutionKey) ([]string, requestCacheState, bool) {
	targets, ok := c.get(key)
	if !ok {
		return nil, requestCacheState{}, false
	}
	state, ok := c.requests[key]
	return targets, state, ok
}

func (c *resolutionCache) setRequest(key resolutionKey, targets []string,
	resolution httpmodel.DestinationResolution, evidence httpmodel.DestinationEvidence,
) {
	c.set(key, targets)
	c.requests[key] = requestCacheState{resolution: resolution, evidence: evidence}
}

func (c *resolutionCache) loadEndpointCatalog(ctx context.Context, q *sqlcgen.Queries) error {
	if c.endpointCatalogLoaded {
		return nil
	}
	rows, err := q.ListNodesByKind(ctx, sqlcgen.ListNodesByKindParams{
		Kind: string(graph.KindEndpoint), MinExternal: 0, MaxExternal: 0,
		NameFragment: "", PathPrefixesJson: "[]", MaxResults: -1,
	})
	if err != nil {
		return err
	}
	nodes := make([]graph.Node, 0, len(rows))
	candidates := make([]httpmodel.EndpointCandidate, 0, len(rows))
	for _, row := range rows {
		node := nodeFromRow(row)
		candidateMethod, candidateRoute, ok := endpointNodeMethodRoute(node)
		if !ok {
			continue
		}
		nodes = append(nodes, node)
		candidates = append(candidates, httpmodel.EndpointCandidate{Method: candidateMethod,
			Authority: candidateRoute.Authority, Route: candidateRoute})
	}
	c.endpointNodes = nodes
	c.endpointCatalog = httpmodel.NewCandidateCatalog(candidates)
	c.endpointCatalogLoaded = true
	return nil
}

type resolutionCandidate struct {
	id            string
	kind          graph.NodeKind
	qualifiedName string
}

type endpointResult struct {
	targets    []string
	properties map[string]string
}

func resolveSources(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, fact graph.Fact, sourceExists bool, cache *resolutionCache) ([]string, error) {
	result, err := resolveEndpoint(ctx, q, writer, fact, sourceEndpoint, sourceExists, cache)
	return result.targets, err
}

func resolveTargets(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, fact graph.Fact, targetExists bool, cache *resolutionCache) (endpointResult, error) {
	return resolveEndpoint(ctx, q, writer, fact, targetEndpoint, targetExists, cache)
}

func resolveEndpoint(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, fact graph.Fact, direction endpointDirection, exactExists bool, cache *resolutionCache) (endpointResult, error) {
	if direction == targetEndpoint && fact.Kind == graph.EdgeRequests {
		return resolveRequestTarget(ctx, q, writer, fact, exactExists, cache)
	}
	exactID, name, kind := fact.TargetID, fact.Target, fact.TargetKind
	if direction == sourceEndpoint {
		exactID, name, kind = fact.FromID, fact.Source, fact.SourceKind
	}
	if exactID != "" {
		if exactExists {
			return endpointResult{targets: []string{exactID}}, nil
		}
		external := externalNode(exactID, "")
		if err := writer.addNode(ctx, nodeParams(external, 1)); err != nil {
			return endpointResult{}, err
		}
		return endpointResult{targets: []string{external.ID}}, nil
	}
	key := resolutionKey{direction: direction, target: name, targetKind: kind, edgeKind: fact.Kind}
	if targets, ok := cache.get(key); ok {
		return endpointResult{targets: targets}, nil
	}
	var targets []string
	var rows []resolutionCandidate
	if name != "" {
		if kind == "" {
			found, err := q.FindNodesExact(ctx, name)
			if err != nil {
				return endpointResult{}, err
			}
			rows = make([]resolutionCandidate, 0, len(found))
			for _, row := range found {
				rows = append(rows, resolutionCandidate{id: row.ID, kind: graph.NodeKind(row.Kind), qualifiedName: row.QualifiedName})
			}
		} else {
			found, err := q.FindNodesExactKind(ctx, sqlcgen.FindNodesExactKindParams{
				Target: name, Kind: string(kind)})
			if err != nil {
				return endpointResult{}, err
			}
			rows = make([]resolutionCandidate, 0, len(found))
			for _, row := range found {
				rows = append(rows, resolutionCandidate{id: row.ID, kind: graph.NodeKind(row.Kind), qualifiedName: row.QualifiedName})
			}
		}
		rows = filterCandidates(fact, direction, rows)
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].qualifiedName == rows[j].qualifiedName {
				return rows[i].id < rows[j].id
			}
			return rows[i].qualifiedName < rows[j].qualifiedName
		})
		// A name shared by multiple declarations is not enough evidence to
		// invent a fan-out edge. Preserve one explicit unresolved endpoint and
		// let a parser provide a qualified name when it can prove the binding.
		if len(rows) == 1 {
			targets = []string{rows[0].id}
		}
	}
	if len(targets) == 0 {
		external := externalNode(name, kind)
		if err := writer.addNode(ctx, nodeParams(external, 1)); err != nil {
			return endpointResult{}, err
		}
		targets = []string{external.ID}
	}
	cache.set(key, targets)
	return endpointResult{targets: targets}, nil
}

func resolveRequestTarget(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter, fact graph.Fact,
	exactExists bool, cache *resolutionCache,
) (endpointResult, error) {
	method, route, validRoute := requestFactMethodRoute(fact)
	contract, err := httpmodel.ParseDestinationContract(fact.Properties, route)
	if err != nil {
		return endpointResult{}, fmt.Errorf("request fact %s: %w", fact.ID, err)
	}
	if fact.TargetID != "" {
		if exactExists {
			row, nodeErr := q.GetNode(ctx, fact.TargetID)
			if nodeErr != nil {
				return endpointResult{}, nodeErr
			}
			if row.External != 0 {
				resolution, evidence := requestBoundaryResolution(contract)
				properties := httpmodel.WithDestinationEvidence(fact.Properties, resolution, evidence)
				marked, markErr := httpmodel.ParseDestinationContract(properties, route)
				if markErr != nil {
					return endpointResult{}, fmt.Errorf("request fact %s: %w", fact.ID, markErr)
				}
				if err := marked.ValidateTarget(true); err != nil {
					return endpointResult{}, fmt.Errorf("request fact %s: %w", fact.ID, err)
				}
				return endpointResult{targets: []string{fact.TargetID}, properties: properties}, nil
			}
			target := nodeFromRow(row)
			candidateMethod, candidateRoute, candidateValid := endpointNodeMethodRoute(target)
			compatible := validRoute && candidateValid && httpmodel.CandidateCompatible(method, route,
				httpmodel.EndpointCandidate{Method: candidateMethod, Authority: candidateRoute.Authority, Route: candidateRoute})
			if target.Kind != graph.KindEndpoint || contract.Authority == httpmodel.AuthorityExternal || !compatible {
				return materializeRequestBoundary(ctx, writer, fact, contract, route, requestBoundaryName(fact, method, route))
			}
			properties := httpmodel.WithDestinationEvidence(fact.Properties,
				httpmodel.DestinationResolved, httpmodel.EvidenceExactTarget)
			marked, markErr := httpmodel.ParseDestinationContract(properties, route)
			if markErr != nil {
				return endpointResult{}, fmt.Errorf("request fact %s: %w", fact.ID, markErr)
			}
			if err := marked.ValidateTarget(false); err != nil {
				return endpointResult{}, fmt.Errorf("request fact %s: %w", fact.ID, err)
			}
			return endpointResult{targets: []string{fact.TargetID}, properties: properties}, nil
		}
		return materializeRequestBoundary(ctx, writer, fact, contract, route, requestBoundaryName(fact, method, route))
	}

	key := resolutionKey{direction: targetEndpoint, target: fact.Target, targetKind: fact.TargetKind,
		edgeKind: fact.Kind, authority: contract.Authority, method: method, route: route.Canonical}
	if targets, state, ok := cache.getRequest(key); ok {
		return endpointResult{targets: targets,
			properties: httpmodel.WithDestinationEvidence(fact.Properties, state.resolution, state.evidence)}, nil
	}
	if contract.Authority != httpmodel.AuthorityLocal || !validRoute {
		result, boundaryErr := materializeRequestBoundary(ctx, writer, fact, contract, route, requestBoundaryName(fact, method, route))
		if boundaryErr == nil {
			marked, _ := httpmodel.ParseDestinationContract(result.properties, route)
			cache.setRequest(key, result.targets, marked.Resolution, marked.Evidence)
		}
		return result, boundaryErr
	}

	if err := cache.loadEndpointCatalog(ctx, q); err != nil {
		return endpointResult{}, err
	}
	indexes := cache.endpointCatalog.BestCandidateIndexes(method, route)
	if len(indexes) == 1 {
		result := endpointResult{targets: []string{cache.endpointNodes[indexes[0]].ID}, properties: httpmodel.WithDestinationEvidence(
			fact.Properties, httpmodel.DestinationResolved, httpmodel.EvidenceRoute)}
		cache.setRequest(key, result.targets, httpmodel.DestinationResolved, httpmodel.EvidenceRoute)
		return result, nil
	}
	resolution := httpmodel.DestinationUnresolved
	if len(indexes) > 1 {
		resolution = httpmodel.DestinationAmbiguous
	}
	external := externalNode(fact.Target, graph.KindEndpoint)
	if err := writer.addNode(ctx, nodeParams(external, 1)); err != nil {
		return endpointResult{}, err
	}
	result := endpointResult{targets: []string{external.ID}, properties: httpmodel.WithDestinationEvidence(
		fact.Properties, resolution, httpmodel.EvidenceRoute)}
	cache.setRequest(key, result.targets, resolution, httpmodel.EvidenceRoute)
	return result, nil
}

func requestBoundaryResolution(contract httpmodel.DestinationContract) (
	httpmodel.DestinationResolution, httpmodel.DestinationEvidence,
) {
	if contract.Authority == httpmodel.AuthorityExternal {
		return httpmodel.DestinationExternal, httpmodel.EvidenceExplicitAuthority
	}
	if contract.Authority == httpmodel.AuthorityUnknown {
		return httpmodel.DestinationUnresolved, httpmodel.EvidenceUnknownAuthority
	}
	return httpmodel.DestinationUnresolved, httpmodel.EvidenceRoute
}

func requestBoundaryName(fact graph.Fact, method string, route httpmodel.Route) string {
	if name := strings.TrimSpace(fact.Target); name != "" {
		return name
	}
	if method != "" && route.Canonical != "" {
		return method + " " + route.Canonical
	}
	return fact.TargetID
}

func materializeRequestBoundary(ctx context.Context, writer *batchWriter, fact graph.Fact,
	contract httpmodel.DestinationContract, route httpmodel.Route, name string,
) (endpointResult, error) {
	external := externalNode(name, graph.KindEndpoint)
	if err := writer.addNode(ctx, nodeParams(external, 1)); err != nil {
		return endpointResult{}, err
	}
	resolution, evidence := requestBoundaryResolution(contract)
	properties := httpmodel.WithDestinationEvidence(fact.Properties, resolution, evidence)
	marked, err := httpmodel.ParseDestinationContract(properties, route)
	if err != nil {
		return endpointResult{}, fmt.Errorf("request fact %s: %w", fact.ID, err)
	}
	if err := marked.ValidateTarget(true); err != nil {
		return endpointResult{}, fmt.Errorf("request fact %s: %w", fact.ID, err)
	}
	return endpointResult{targets: []string{external.ID}, properties: properties}, nil
}

func requestFactMethodRoute(fact graph.Fact) (string, httpmodel.Route, bool) {
	method := strings.TrimSpace(fact.Properties["http_method"])
	routeText := strings.TrimSpace(fact.Properties["http_route"])
	if targetMethod, targetRoute, ok := strings.Cut(strings.TrimSpace(fact.Target), " "); ok {
		if method == "" {
			method = targetMethod
		}
		if routeText == "" {
			routeText = targetRoute
		}
	}
	normalized, methodErr := httpmodel.PreserveMethod(method)
	route, routeErr := httpmodel.ParseRoute(routeText)
	return normalized, route, methodErr == nil && routeErr == nil && fact.Properties["http_invalid"] != "true"
}

func endpointNodeMethodRoute(node graph.Node) (string, httpmodel.Route, bool) {
	method := strings.TrimSpace(node.Properties["method"])
	routeText := strings.TrimSpace(node.Properties["route"])
	if targetMethod, targetRoute, ok := strings.Cut(strings.TrimSpace(node.Name), " "); ok {
		if method == "" {
			method = targetMethod
		}
		if routeText == "" {
			routeText = targetRoute
		}
	}
	normalized, methodErr := httpmodel.PreserveMethod(method)
	route, routeErr := httpmodel.ParseRoute(routeText)
	route.Authority = strings.TrimSpace(node.Properties["authority"])
	return normalized, route, methodErr == nil && routeErr == nil && node.Properties["http_invalid"] != "true"
}

func filterCandidates(fact graph.Fact, direction endpointDirection, rows []resolutionCandidate) []resolutionCandidate {
	kind := fact.TargetKind
	if direction == sourceEndpoint {
		kind = fact.SourceKind
	}
	if kind != "" || direction == sourceEndpoint {
		return rows
	}
	result := rows[:0]
	for _, row := range rows {
		if graph.AllowsResolutionKind(fact.Kind, row.kind) {
			result = append(result, row)
		}
	}
	return result
}

func externalNode(qualified string, kind graph.NodeKind) graph.Node {
	if kind == "" {
		kind = graph.KindExternal
	}
	return graph.Node{ID: graph.NodeID(kind, "external:"+qualified), Kind: kind,
		Name: graph.SimpleName(qualified), QualifiedName: qualified, OwnerFile: "__external__",
		External: true, Properties: map[string]string{"unresolved": "true"}}
}

func (r *Repository) Counts(ctx context.Context) (graph.Counts, error) {
	result := graph.Counts{ByKind: map[string]int{}, ByEdge: map[string]int{}}
	files, err := r.queries.CountFiles(ctx)
	if err != nil {
		return result, err
	}
	nodes, err := r.queries.CountNodes(ctx)
	if err != nil {
		return result, err
	}
	facts, err := r.queries.CountFacts(ctx)
	if err != nil {
		return result, err
	}
	edges, err := r.queries.CountEdges(ctx)
	if err != nil {
		return result, err
	}
	external, err := r.queries.CountExternalNodes(ctx)
	if err != nil {
		return result, err
	}
	result.Files, result.Nodes, result.Facts, result.Edges, result.External = int(files), int(nodes), int(facts), int(edges), int(external)
	nodeKinds, err := r.queries.CountNodesByKind(ctx)
	if err != nil {
		return result, err
	}
	for _, row := range nodeKinds {
		result.ByKind[row.Kind] = int(row.Count)
	}
	edgeKinds, err := r.queries.CountEdgesByKind(ctx)
	if err != nil {
		return result, err
	}
	for _, row := range edgeKinds {
		result.ByEdge[row.Kind] = int(row.Count)
	}
	return result, nil
}

func (r *Repository) SearchNodes(ctx context.Context, term string, limit int) ([]graph.Node, error) {
	rows, err := r.queries.SearchNodes(ctx, sqlcgen.SearchNodesParams{Term: foldName(term), MaxResults: int64(limit)})
	if err != nil {
		return nil, err
	}
	result := make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		result = append(result, nodeFromRow(row))
	}
	return result, nil
}

// Repositories reports the indexed repository this database stores, derived
// from the root recorded at index time.
func (r *Repository) Repositories(ctx context.Context) ([]string, error) {
	name, err := r.repositoryName(ctx)
	if err != nil || name == "" {
		return []string{}, err
	}
	return []string{name}, nil
}

// ListNodesByKind enumerates nodes of the requested kinds. It only enumerates:
// kind membership, name matching, and bounds are storage concerns, while usage
// direction and orphan classification stay in the query use cases.
func (r *Repository) ListNodesByKind(ctx context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	name, err := r.repositoryName(ctx)
	if err != nil {
		return nil, err
	}
	if request.Repository != "" && request.Repository != name {
		return []graph.ScopedNode{}, nil
	}
	minExternal, maxExternal := int64(0), int64(0)
	switch request.Visibility {
	case graph.ExternalNodes:
		minExternal, maxExternal = 1, 1
	case graph.AllNodes:
		minExternal, maxExternal = 0, 1
	case "", graph.LocalNodes:
	default:
		return nil, fmt.Errorf("unsupported node visibility %q", request.Visibility)
	}
	limit := int64(-1)
	if request.Limit > 0 {
		limit = int64(request.Limit)
	}
	// The fragment is matched literally, so a name containing % or _ narrows
	// the catalog instead of silently widening it.
	fragment := foldName(request.Name)
	prefixes := request.PathPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	prefixesJSON, err := json.Marshal(prefixes)
	if err != nil {
		return nil, fmt.Errorf("encode node path prefixes: %w", err)
	}
	result := []graph.ScopedNode{}
	for _, kind := range request.Kinds {
		rows, err := r.queries.ListNodesByKind(ctx, sqlcgen.ListNodesByKindParams{
			Kind: string(kind), MinExternal: minExternal, MaxExternal: maxExternal,
			NameFragment: fragment, PathPrefixesJson: string(prefixesJSON), MaxResults: limit,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			result = append(result, graph.ScopedNode{Repository: name, Node: nodeFromRow(row)})
		}
	}
	return result, nil
}

// CanonicalMessages enumerates authoritative protocol-message declarations.
// Filtering happens in SQLite before the deterministic order and bound, so
// unrelated type declarations cannot hide or truncate message coverage.
func (r *Repository) CanonicalMessages(ctx context.Context, request graph.CanonicalMessageQuery) (graph.CanonicalMessagePage, error) {
	if err := request.Validate(); err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	name, err := r.repositoryName(ctx)
	if err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	if request.Repository != "" && request.Repository != name {
		return graph.CanonicalMessagePage{Items: []graph.ScopedNode{}}, nil
	}
	if request.Limit == int(^uint(0)>>1) {
		return graph.CanonicalMessagePage{}, fmt.Errorf("canonical message limit is too large")
	}
	prefixes := request.PathPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	prefixesJSON, err := json.Marshal(prefixes)
	if err != nil {
		return graph.CanonicalMessagePage{}, fmt.Errorf("encode canonical message path prefixes: %w", err)
	}
	rows, err := r.queries.ListCanonicalMessages(ctx, sqlcgen.ListCanonicalMessagesParams{
		PackageName: strings.TrimSuffix(request.Package, "."), MessageName: request.Message,
		PathPrefixesJson: string(prefixesJSON), MaxResults: int64(request.Limit) + 1,
	})
	if err != nil {
		return graph.CanonicalMessagePage{}, err
	}
	page := graph.CanonicalMessagePage{Items: make([]graph.ScopedNode, 0, min(len(rows), request.Limit))}
	if len(rows) > request.Limit {
		page.Truncated = true
		rows = rows[:request.Limit]
	}
	for _, row := range rows {
		page.Items = append(page.Items, graph.ScopedNode{Repository: name, Node: nodeFromRow(row)})
	}
	return page, nil
}

func (r *Repository) repositoryName(ctx context.Context) (string, error) {
	root, err := r.Meta(ctx, "root")
	if err != nil || root == "" {
		return "", err
	}
	return filepath.Base(root), nil
}

// DefaultNodeMatchLimit bounds the candidate list MatchNodes returns when a
// caller does not ask for a different size. Totals are never bounded by it.
const DefaultNodeMatchLimit = 25

// matchScope pairs a match level with the node scope it is evaluated over.
// Scopes are ordered so a local declaration always outranks an external boundary
// node at the same level, and exact evidence about an external target is still
// exact rather than being pushed down into the substring level.
type matchScope struct {
	level    graph.NodeMatchLevel
	external bool
}

var matchScopes = []matchScope{
	{graph.MatchQualifiedName, false},
	{graph.MatchQualifiedName, true},
	{graph.MatchName, false},
	{graph.MatchName, true},
	{graph.MatchSubstring, false},
	{graph.MatchSubstring, true},
}

// MatchNodes resolves a selector against the narrow, index-backed match queries
// in nodes.sql. Scopes are tried strongest first and the first one holding a
// case-sensitive match wins, so a substring match never competes with an exact
// name match, an unresolved boundary node never competes with a real declaration,
// and a case-insensitive-only hit at a strong level never outranks a
// case-sensitive match at a weaker one. A scope with only case-insensitive matches
// is kept as the fallback for selectors that match nothing case-sensitively. The
// count query runs before the row query so the reported totals cover the whole
// graph even when the row list is truncated.
func (r *Repository) MatchNodes(ctx context.Context, request graph.NodeMatchQuery) (graph.NodeMatchGroup, error) {
	if request.Repository != "" {
		name, err := r.repositoryName(ctx)
		if err != nil {
			return graph.NodeMatchGroup{}, err
		}
		if request.Repository != name {
			return graph.NodeMatchGroup{}, nil
		}
	}
	selector := strings.TrimSpace(request.Selector)
	if selector == "" {
		return graph.NodeMatchGroup{}, nil
	}
	limit := request.Limit
	if limit <= 0 {
		limit = DefaultNodeMatchLimit
	}
	var fallback graph.NodeMatchGroup
	for _, scope := range matchScopes {
		group, err := r.matchNodesInScope(ctx, scope, selector, string(request.Kind), limit)
		if err != nil {
			return graph.NodeMatchGroup{}, err
		}
		if group.Strict > 0 {
			return group, nil
		}
		if group.StrongerThan(fallback) {
			fallback = group
		}
	}
	return fallback, nil
}

func (r *Repository) matchNodesInScope(ctx context.Context, scope matchScope,
	selector, kind string, limit int) (graph.NodeMatchGroup, error) {
	group := graph.NodeMatchGroup{Level: scope.level, External: scope.external}
	external := int64(0)
	if scope.external {
		external = 1
	}
	var rows []sqlcgen.Node
	var err error
	switch scope.level {
	case graph.MatchQualifiedName:
		counts, countErr := r.queries.CountNodeMatchesByQualifiedName(ctx,
			sqlcgen.CountNodeMatchesByQualifiedNameParams{Target: selector, External: external, Kind: kind})
		if countErr != nil {
			return graph.NodeMatchGroup{}, countErr
		}
		group.Total, group.Strict = int(counts.Total), int(counts.StrictMatches)
		if group.Total == 0 {
			return group, nil
		}
		rows, err = r.queries.MatchNodesByQualifiedName(ctx,
			sqlcgen.MatchNodesByQualifiedNameParams{Target: selector, External: external, Kind: kind,
				MaxResults: int64(limit)})
	case graph.MatchName:
		counts, countErr := r.queries.CountNodeMatchesByName(ctx,
			sqlcgen.CountNodeMatchesByNameParams{Target: selector, External: external, Kind: kind})
		if countErr != nil {
			return graph.NodeMatchGroup{}, countErr
		}
		group.Total, group.Strict = int(counts.Total), int(counts.StrictMatches)
		if group.Total == 0 {
			return group, nil
		}
		rows, err = r.queries.MatchNodesByName(ctx,
			sqlcgen.MatchNodesByNameParams{Target: selector, External: external, Kind: kind,
				MaxResults: int64(limit)})
	case graph.MatchSubstring:
		counts, countErr := r.queries.CountNodeMatchesBySubstring(ctx,
			sqlcgen.CountNodeMatchesBySubstringParams{Target: selector, External: external, Kind: kind})
		if countErr != nil {
			return graph.NodeMatchGroup{}, countErr
		}
		group.Total, group.Strict = int(counts.Total), int(counts.StrictMatches)
		if group.Total == 0 {
			return group, nil
		}
		rows, err = r.queries.MatchNodesBySubstring(ctx,
			sqlcgen.MatchNodesBySubstringParams{Target: selector, External: external, Kind: kind,
				MaxResults: int64(limit)})
	default:
		return graph.NodeMatchGroup{}, fmt.Errorf("unknown node match level %q", scope.level)
	}
	if err != nil {
		return graph.NodeMatchGroup{}, err
	}
	group.Nodes = make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		group.Nodes = append(group.Nodes, nodeFromRow(row))
	}
	return group, nil
}

func (r *Repository) Node(ctx context.Context, id string) (graph.Node, error) {
	row, err := r.queries.GetNode(ctx, id)
	if err != nil {
		return graph.Node{}, err
	}
	return nodeFromRow(row), nil
}

func (r *Repository) ExternalNodesMatching(ctx context.Context, node graph.Node) ([]graph.Node, error) {
	rows, err := r.queries.ListExternalNodesMatching(ctx, sqlcgen.ListExternalNodesMatchingParams{
		QualifiedName: node.QualifiedName, Name: node.Name,
	})
	if err != nil {
		return nil, err
	}
	result := make([]graph.Node, 0, len(rows))
	for _, row := range rows {
		result = append(result, nodeFromRow(row))
	}
	return result, nil
}

func (r *Repository) EdgesFrom(ctx context.Context, id string) ([]graph.Edge, error) {
	rows, err := r.queries.ListEdgesFrom(ctx, id)
	if err != nil {
		return nil, err
	}
	origins := make([]edgeOrigin, 0, len(rows))
	for _, row := range rows {
		origins = append(origins, edgeOrigin{factID: row.FactID, fromID: row.FromID,
			toID: row.ToID, kind: row.Kind, producer: row.Producer, path: row.Path, line: row.Line,
			columnNo: row.ColumnNo, endLine: row.EndLine, properties: row.Properties,
			originResolved: row.OriginResolved})
	}
	return edgesFromOrigins(origins)
}

func (r *Repository) EdgesTo(ctx context.Context, id string) ([]graph.Edge, error) {
	rows, err := r.queries.ListEdgesTo(ctx, id)
	if err != nil {
		return nil, err
	}
	origins := make([]edgeOrigin, 0, len(rows))
	for _, row := range rows {
		origins = append(origins, edgeOrigin{factID: row.FactID, fromID: row.FromID,
			toID: row.ToID, kind: row.Kind, producer: row.Producer, path: row.Path, line: row.Line,
			columnNo: row.ColumnNo, endLine: row.EndLine, properties: row.Properties,
			originResolved: row.OriginResolved})
	}
	return edgesFromOrigins(origins)
}

// ExternalRequestEdges returns unresolved HTTP request boundaries in stable,
// bounded pages. Both endpoint and source nodes are hydrated by the same query
// so federation can build its reverse projection without N+1 adjacency loads.
func (r *Repository) ExternalRequestEdges(ctx context.Context, after *graph.ExternalRequestEdgeCursor, limit int) (graph.ExternalRequestEdgePage, error) {
	if limit <= 0 {
		return graph.ExternalRequestEdgePage{}, fmt.Errorf("external request edge limit must be positive")
	}
	if limit == int(^uint(0)>>1) {
		return graph.ExternalRequestEdgePage{}, fmt.Errorf("external request edge limit is too large")
	}
	// The empty cursor is the first page: no identity sorts before three empty
	// strings, so a nil cursor needs no separate query.
	var cursor graph.ExternalRequestEdgeCursor
	if after != nil {
		cursor = *after
	}
	rows, err := r.queries.ListExternalRequestEdges(ctx, sqlcgen.ListExternalRequestEdgesParams{
		AfterFactID: cursor.FactID, AfterToID: cursor.ToID, AfterKind: string(cursor.Kind),
		MaxResults: int64(limit) + 1,
	})
	if err != nil {
		return graph.ExternalRequestEdgePage{}, err
	}
	page := graph.ExternalRequestEdgePage{Items: make([]graph.ExternalRequestEdge, 0, min(len(rows), limit))}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		page.Next = &graph.ExternalRequestEdgeCursor{
			FactID: last.EdgeFactID, ToID: last.EdgeToID, Kind: graph.EdgeKind(last.EdgeKind)}
	}
	for _, row := range rows {
		edgeID := graph.DerivedEdgeID(row.EdgeFactID, row.EdgeToID, graph.EdgeKind(row.EdgeKind))
		if row.EdgeOriginResolved == 0 {
			return graph.ExternalRequestEdgePage{}, corruptInternedPath("edge " + edgeID)
		}
		edgeProperties, err := decodeRelationProperties("edge "+edgeID, row.EdgeProperties)
		if err != nil {
			return graph.ExternalRequestEdgePage{}, err
		}
		sourceProperties, err := decodeRelationProperties("source node "+row.SourceID, row.SourceProperties)
		if err != nil {
			return graph.ExternalRequestEdgePage{}, err
		}
		targetProperties, err := decodeRelationProperties("target node "+row.TargetID, row.TargetProperties)
		if err != nil {
			return graph.ExternalRequestEdgePage{}, err
		}
		page.Items = append(page.Items, graph.ExternalRequestEdge{
			Edge: graph.Edge{ID: edgeID, FactID: row.EdgeFactID, FromID: row.EdgeFromID, ToID: row.EdgeToID,
				Kind: graph.EdgeKind(row.EdgeKind), Producer: row.EdgeProducer,
				Location:   graph.Location{Path: row.EdgePath, Line: int(row.EdgeLine), Column: int(row.EdgeColumnNo), EndLine: int(row.EdgeEndLine)},
				Properties: edgeProperties},
			Source: graph.Node{ID: row.SourceID, Kind: graph.NodeKind(row.SourceKind), Name: row.SourceName,
				QualifiedName: row.SourceQualifiedName, Language: row.SourceLanguage,
				Location:   graph.Location{Path: row.SourcePath, Line: int(row.SourceLine), Column: int(row.SourceColumnNo), EndLine: int(row.SourceEndLine)},
				Properties: sourceProperties, OwnerFile: row.SourceOwnerFile, External: row.SourceExternal != 0},
			Target: graph.Node{ID: row.TargetID, Kind: graph.NodeKind(row.TargetKind), Name: row.TargetName,
				QualifiedName: row.TargetQualifiedName, Language: row.TargetLanguage,
				Location:   graph.Location{Path: row.TargetPath, Line: int(row.TargetLine), Column: int(row.TargetColumnNo), EndLine: int(row.TargetEndLine)},
				Properties: targetProperties, OwnerFile: row.TargetOwnerFile, External: row.TargetExternal != 0},
		})
	}
	return page, nil
}

// RelationEdges loads at most Limit+1 rows for each exact relation and joins
// the node opposite the subject in the same query. Catalog callers therefore
// pay only for requested evidence and never perform per-edge node lookups.
func (r *Repository) RelationEdges(ctx context.Context, request graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	if err := request.Validate(); err != nil {
		return graph.RelationEdgePage{}, err
	}
	if request.Limit == int(^uint(0)>>1) {
		return graph.RelationEdgePage{}, fmt.Errorf("relation edge limit is too large")
	}
	relations := make([]graph.EdgeKind, 0, len(request.Relations))
	seenRelations := map[graph.EdgeKind]bool{}
	for _, relation := range request.Relations {
		if !seenRelations[relation] {
			seenRelations[relation] = true
			relations = append(relations, relation)
		}
	}
	sort.Slice(relations, func(i, j int) bool { return relations[i] < relations[j] })
	page := graph.RelationEdgePage{Items: []graph.HydratedRelationEdge{}}
	seenEdges := map[string]bool{}
	maxResults := int64(request.Limit) + 1
	for _, relation := range relations {
		items := []graph.HydratedRelationEdge{}
		switch request.Direction {
		case graph.IncomingRelations:
			rows, err := r.queries.ListIncomingRelationEdges(ctx, sqlcgen.ListIncomingRelationEdgesParams{
				SubjectID: request.SubjectID, Relation: string(relation), MaxResults: maxResults,
			})
			if err != nil {
				return graph.RelationEdgePage{}, err
			}
			for _, row := range rows {
				edgeID := graph.DerivedEdgeID(row.EdgeFactID, row.EdgeToID, graph.EdgeKind(row.EdgeKind))
				if row.EdgeOriginResolved == 0 {
					return graph.RelationEdgePage{}, corruptInternedPath("edge " + edgeID)
				}
				item, err := hydratedRelationEdge(edgeID, row.EdgeFactID, row.EdgeFromID, row.EdgeToID,
					row.EdgeKind, row.EdgeProducer, row.EdgePath, row.EdgeLine, row.EdgeColumnNo, row.EdgeEndLine, row.EdgeProperties,
					row.CounterpartID, row.CounterpartKind, row.CounterpartName, row.CounterpartQualifiedName,
					row.CounterpartLanguage, row.CounterpartPath, row.CounterpartLine, row.CounterpartColumnNo,
					row.CounterpartEndLine, row.CounterpartProperties, row.CounterpartOwnerFile, row.CounterpartExternal)
				if err != nil {
					return graph.RelationEdgePage{}, err
				}
				items = append(items, item)
			}
		case graph.OutgoingRelations:
			rows, err := r.queries.ListOutgoingRelationEdges(ctx, sqlcgen.ListOutgoingRelationEdgesParams{
				SubjectID: request.SubjectID, Relation: string(relation), MaxResults: maxResults,
			})
			if err != nil {
				return graph.RelationEdgePage{}, err
			}
			for _, row := range rows {
				edgeID := graph.DerivedEdgeID(row.EdgeFactID, row.EdgeToID, graph.EdgeKind(row.EdgeKind))
				if row.EdgeOriginResolved == 0 {
					return graph.RelationEdgePage{}, corruptInternedPath("edge " + edgeID)
				}
				item, err := hydratedRelationEdge(edgeID, row.EdgeFactID, row.EdgeFromID, row.EdgeToID,
					row.EdgeKind, row.EdgeProducer, row.EdgePath, row.EdgeLine, row.EdgeColumnNo, row.EdgeEndLine, row.EdgeProperties,
					row.CounterpartID, row.CounterpartKind, row.CounterpartName, row.CounterpartQualifiedName,
					row.CounterpartLanguage, row.CounterpartPath, row.CounterpartLine, row.CounterpartColumnNo,
					row.CounterpartEndLine, row.CounterpartProperties, row.CounterpartOwnerFile, row.CounterpartExternal)
				if err != nil {
					return graph.RelationEdgePage{}, err
				}
				items = append(items, item)
			}
		}
		if len(items) > request.Limit {
			page.Truncated = true
			items = items[:request.Limit]
		}
		for _, item := range items {
			if seenEdges[item.Edge.ID] {
				continue
			}
			seenEdges[item.Edge.ID] = true
			page.Items = append(page.Items, item)
		}
	}
	return page, nil
}

func (r *Repository) ExternalEdgesTo(ctx context.Context, node graph.Node) ([]graph.Edge, error) {
	rows, err := r.queries.ListExternalEdgesMatching(ctx, sqlcgen.ListExternalEdgesMatchingParams{
		QualifiedName: node.QualifiedName, Name: node.Name,
	})
	if err != nil {
		return nil, err
	}
	origins := make([]edgeOrigin, 0, len(rows))
	for _, row := range rows {
		origins = append(origins, edgeOrigin{factID: row.FactID, fromID: row.FromID,
			toID: row.ToID, kind: row.Kind, producer: row.Producer, path: row.Path, line: row.Line,
			columnNo: row.ColumnNo, endLine: row.EndLine, properties: row.Properties,
			originResolved: row.OriginResolved})
	}
	return edgesFromOrigins(origins)
}

func (r *Repository) inTransaction(ctx context.Context, fn func(*sqlcgen.Queries, *batchWriter) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	writer, err := newBatchWriter(tx, r.limits)
	if err != nil {
		return err
	}
	defer func() { _ = writer.close() }()
	writer.afterBatch = r.afterBatch
	writer.paths = r.pathKeys.begin()
	if err := fn(r.queries.WithTx(tx), writer); err != nil {
		return err
	}
	if err := writer.flush(ctx); err != nil {
		return err
	}
	if err := writer.close(); err != nil {
		return fmt.Errorf("close SQLite batch statements: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.pathKeys.publish(writer.paths.assigned)
	r.writeStatsMu.Lock()
	addWriteStats(&r.writeStats, writer.stats())
	r.writeStatsMu.Unlock()
	return nil
}

func nodeParams(n graph.Node, external int64) sqlcgen.UpsertNodeParams {
	return sqlcgen.UpsertNodeParams{ID: n.ID, Kind: string(n.Kind), Name: n.Name,
		QualifiedName: n.QualifiedName, Language: n.Language, Path: n.Location.Path,
		Line: int64(n.Location.Line), ColumnNo: int64(n.Location.Column), EndLine: int64(n.Location.EndLine),
		Properties: graph.MarshalProperties(n.Properties), OwnerFile: n.OwnerFile, External: external,
		NameFolded: foldName(n.Name), QualifiedNameFolded: foldName(n.QualifiedName)}
}

func foldName(name string) string {
	return strings.ToLower(name)
}

// factParams interns the fact's location path and owner key. A fact's location
// file and its owner file are interned separately because they are allowed to
// differ: the indexer's workspace facts are owned by a synthetic workspace key
// while their location points at the project configuration file.
func factParams(ctx context.Context, q *sqlcgen.Queries, writer *batchWriter,
	f graph.Fact,
) (sqlcgen.UpsertFactParams, error) {
	pathID, err := writer.pathKey(ctx, q, f.Location.Path)
	if err != nil {
		return sqlcgen.UpsertFactParams{}, err
	}
	ownerPathID := pathID
	if f.OwnerFile != f.Location.Path {
		ownerPathID, err = writer.pathKey(ctx, q, f.OwnerFile)
		if err != nil {
			return sqlcgen.UpsertFactParams{}, err
		}
	}
	return sqlcgen.UpsertFactParams{ID: f.ID, FromID: f.FromID, Source: f.Source,
		SourceKind: string(f.SourceKind), Kind: string(f.Kind), Producer: f.Producer, TargetID: f.TargetID,
		Target: f.Target, TargetKind: string(f.TargetKind), PathID: pathID, Line: int64(f.Location.Line),
		ColumnNo: int64(f.Location.Column), EndLine: int64(f.Location.EndLine),
		Properties: graph.MarshalProperties(f.Properties), OwnerPathID: ownerPathID}, nil
}

// edgeParams stores only what reconciliation can resolve differently from the
// originating fact. The edge's producer and location are the fact's and are
// joined back through fact_id instead of being stored a second time, and its
// identity is derived from fact_id, to_id and kind on read rather than stored,
// so graph.Edge.ID is deliberately not written here.
func edgeParams(e graph.Edge) sqlcgen.InsertEdgeParams {
	return sqlcgen.InsertEdgeParams{FactID: e.FactID, FromID: e.FromID, ToID: e.ToID,
		Kind: string(e.Kind), Properties: graph.MarshalProperties(e.Properties)}
}

func nodeFromRow(n sqlcgen.Node) graph.Node {
	return graph.Node{ID: n.ID, Kind: graph.NodeKind(n.Kind), Name: n.Name, QualifiedName: n.QualifiedName,
		Language: n.Language, Location: graph.Location{Path: n.Path, Line: int(n.Line), Column: int(n.ColumnNo), EndLine: int(n.EndLine)},
		Properties: graph.UnmarshalProperties(n.Properties), OwnerFile: n.OwnerFile, External: n.External != 0}
}

func factFromDirtyRow(f sqlcgen.ListDirtyFactBatchRow) (graph.Fact, error) {
	if f.PathsResolved == 0 {
		return graph.Fact{}, corruptInternedPath("fact " + f.ID)
	}
	return graph.Fact{ID: f.ID, FromID: f.FromID, Source: f.Source, SourceKind: graph.NodeKind(f.SourceKind),
		Kind: graph.EdgeKind(f.Kind), Producer: f.Producer, TargetID: f.TargetID,
		Target: f.Target, TargetKind: graph.NodeKind(f.TargetKind),
		Location:   graph.Location{Path: f.Path, Line: int(f.Line), Column: int(f.ColumnNo), EndLine: int(f.EndLine)},
		Properties: graph.UnmarshalProperties(f.Properties), OwnerFile: f.OwnerFile}, nil
}

// corruptInternedPath reports an index whose row references an interned path or
// originating fact that is missing. Reporting an empty location instead would
// hand a caller evidence that points nowhere.
func corruptInternedPath(subject string) error {
	return fmt.Errorf("%s references a missing originating fact or interned path; "+
		"the index is corrupt and must be rebuilt with 'grafo index --force'", subject)
}

// edgeOrigin is the producer and location an edge derives from its originating
// fact, projected by every edge read query.
type edgeOrigin struct {
	factID         string
	fromID         string
	toID           string
	kind           string
	producer       string
	path           string
	line           int64
	columnNo       int64
	endLine        int64
	properties     string
	originResolved int64
}

func edgeFromOrigin(row edgeOrigin) (graph.Edge, error) {
	// The identity is derived from the three columns that determine it rather than
	// read, which is the whole of migration 13: graph.DerivedEdgeID is the one
	// definition of that rule and every read path goes through it.
	id := graph.DerivedEdgeID(row.factID, row.toID, graph.EdgeKind(row.kind))
	if row.originResolved == 0 {
		return graph.Edge{}, corruptInternedPath("edge " + id)
	}
	return graph.Edge{ID: id, FactID: row.factID, FromID: row.fromID, ToID: row.toID,
		Kind: graph.EdgeKind(row.kind), Producer: row.producer,
		Location: graph.Location{Path: row.path, Line: int(row.line),
			Column: int(row.columnNo), EndLine: int(row.endLine)},
		Properties: graph.UnmarshalProperties(row.properties)}, nil
}

func edgesFromOrigins(rows []edgeOrigin) ([]graph.Edge, error) {
	result := make([]graph.Edge, 0, len(rows))
	for _, row := range rows {
		edge, err := edgeFromOrigin(row)
		if err != nil {
			return nil, err
		}
		result = append(result, edge)
	}
	return result, nil
}

func hydratedRelationEdge(edgeID, factID, fromID, toID, kind, producer, path string,
	line, column, endLine int64, properties, counterpartID, counterpartKind, counterpartName,
	counterpartQualifiedName, counterpartLanguage, counterpartPath string, counterpartLine,
	counterpartColumn, counterpartEndLine int64, counterpartProperties, counterpartOwnerFile string,
	counterpartExternal int64) (graph.HydratedRelationEdge, error) {
	if counterpartID == "" {
		return graph.HydratedRelationEdge{}, fmt.Errorf("edge %s has no counterpart node", edgeID)
	}
	edgeProperties, err := decodeRelationProperties("edge "+edgeID, properties)
	if err != nil {
		return graph.HydratedRelationEdge{}, err
	}
	nodeProperties, err := decodeRelationProperties("counterpart node "+counterpartID, counterpartProperties)
	if err != nil {
		return graph.HydratedRelationEdge{}, err
	}
	return graph.HydratedRelationEdge{
		Edge: graph.Edge{ID: edgeID, FactID: factID, FromID: fromID, ToID: toID, Kind: graph.EdgeKind(kind), Producer: producer,
			Location:   graph.Location{Path: path, Line: int(line), Column: int(column), EndLine: int(endLine)},
			Properties: edgeProperties},
		Counterpart: graph.Node{ID: counterpartID, Kind: graph.NodeKind(counterpartKind), Name: counterpartName,
			QualifiedName: counterpartQualifiedName, Language: counterpartLanguage,
			Location: graph.Location{Path: counterpartPath, Line: int(counterpartLine),
				Column: int(counterpartColumn), EndLine: int(counterpartEndLine)},
			Properties: nodeProperties, OwnerFile: counterpartOwnerFile,
			External: counterpartExternal != 0},
	}, nil
}

func decodeRelationProperties(label, raw string) (map[string]string, error) {
	if raw == "{}" {
		return nil, nil
	}
	var properties map[string]string
	if err := json.Unmarshal([]byte(raw), &properties); err != nil {
		return nil, fmt.Errorf("decode %s properties: %w", label, err)
	}
	if len(properties) == 0 {
		return nil, nil
	}
	return properties, nil
}
