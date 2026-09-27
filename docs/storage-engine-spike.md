# Embedded storage engine spike

Status: benchmark implementation complete; measured recommendation recorded
below after the reproducible run. This note covers issue #47 and does not
change Grafo's production SQLite default.

## Decision threshold

A candidate is eligible for a migration proposal only if it satisfies all of
these predeclared gates:

1. exact SQLite-equivalent graph counts, deterministic query digest,
   incremental convergence, and restart/resume behavior;
2. at least a 2x generated-fixture persistence/reconciliation improvement and
   at least a 30% median reduction in Uzir cold total time;
3. no material regression (greater than 20%) in database size or peak RSS, and
   no systematic unchanged/incremental regression that would erase the cold
   benefit; and
4. an operational and maintenance advantage large enough to justify replacing
   SQL migrations, sqlc queries, SQLite inspection/recovery skills, and a
   rebuild-only index migration.

Machine-sensitive timing never gates correctness. A candidate that fails any
correctness check is excluded from performance ranking.

## Candidate selection

The selection was made against the issue's local-only, durability, indexed
lookup, Go 1.26, and production-credibility requirements before prototype
wiring:

| Engine | Decision | Reason |
| --- | --- | --- |
| SQLite (`modernc.org/sqlite` v1.57.0) | Control | Current production adapter; transactional relational indexes, mature recovery tooling, pure-Go build, and the exact semantics to preserve. |
| [bbolt v1.5.0](https://github.com/etcd-io/bbolt/releases/tag/v1.5.0) | Prototype | A maintained MIT-licensed pure-Go B+tree with serializable ACID transactions, deterministic ordered cursors, a single writer, and a compact API. It tests whether a purpose-built ordered KV layout removes SQL/driver overhead. |
| [Pebble v2.1.7](https://github.com/cockroachdb/pebble/releases/tag/v2.1.7) | Prototype | A maintained BSD-3-Clause pure-Go LSM used by CockroachDB, with indexed atomic batches, WAL sync, prefix iteration, checkpoints, and production-scale compaction behavior. It tests a write-optimized design distinct from both SQLite and bbolt. |
| [Badger v4.9.6](https://github.com/dgraph-io/badger/releases/tag/v4.9.6) | Assessed, not prototyped | Credible and Apache-2.0, but its separate value log, garbage collection, discard tuning, and backup/restore lifecycle add a third operational model without adding a new access-pattern category beyond the selected LSM candidate. |
| [DuckDB Go v2.10505.0](https://github.com/duckdb/duckdb-go/releases/tag/v2.10505.0) | Rejected | Strong analytical engine, but Grafo's workload is point lookup, ordered adjacency traversal, transactional file replacement, and small incremental writes. Its native/CGO distribution and analytical focus weaken the cross-platform and maintenance case without changing required query semantics. |

The bbolt module declares Go 1.25 and the Pebble v2 module builds under the
repository's Go 1.26 toolchain. The committed verification also runs the
prototype package with `CGO_ENABLED=0` so neither candidate introduces a
native build requirement.

## Prototype boundaries and correctness

`internal/storage/kvbench` implements the existing `graph.Repository` ports on
both engines through one benchmark-only KV graph layout. The production CLI
does not import it. Primary records and every secondary index are updated in
one engine transaction or synced atomic batch. Indexes cover owner replacement,
exact name and qualified-name resolution, fact target invalidation, and
incoming/outgoing/fact edge traversal.

Tests compare each candidate directly with SQLite for counts, lookup ordering,
traversal, ambiguous and unresolved targets, external-node deduplication,
later internal-declaration convergence, canceled file replacement, and
interruption after a committed reconciliation batch followed by reopen/resume.
The generated comparison alternates insertion order and hashes a semantic
probe result, while every corpus scenario compares exact counts with the
same-sample SQLite control.

The prototype intentionally exposes the maintenance cost of a KV migration:
Grafo must own record encoding, every secondary index, stale-index deletion,
resolution scans, format versioning, and recovery invariants that SQLite
currently supplies declaratively. A future candidate-specific optimized layout
would require another proof of semantic equivalence; these numbers cannot be
treated as proof that every possible layout performs the same.

## Reproducible measurements

Run:

```sh
GRAFO_BENCH_REPO=/path/to/uzir \
GRAFO_STORAGE_BENCH_OUTPUT=/path/outside/uzir/storage-spike \
GRAFO_STORAGE_BENCH_SAMPLES=3 \
GRAFO_STORAGE_BENCH_ROWS=5000 \
task bench:storage
```

The command pins the corpus commit at the start of each isolated run, uses the
same production parser registry and indexer, runs engines sequentially, removes
the per-sample database after metrics are captured, and writes one atomic
`storage-comparison.json`. It records raw values and medians for generated cold
persistence/reconciliation and every corpus scenario's total, persistence,
reconciliation, database/log size, RSS, write rows, and transaction batches.
Timing ratios are candidate median divided by SQLite median; lower is better.

Committed report: [issue 47 storage comparison](benchmarks/issue-47-storage-comparison.json).

## Operational and migration analysis

### SQLite control

- **Transactions and recovery:** WAL mode, `synchronous=NORMAL`, one writer,
  atomic file replacement and reconciliation transactions, bounded durable
  queue batches, and explicit WAL checkpoints.
- **Operations:** SQLite has broad integrity-check, dump, backup, inspection,
  and recovery tooling. Goose owns schema evolution and sqlc owns static
  queries. Downgrades must understand forward migrations, but the index can
  always be rebuilt from source.
- **Build and maintenance:** `modernc.org/sqlite` keeps the production build
  free of CGO. The adapter and its failure modes are already exercised in
  production-shaped tests.

### bbolt

- **Transactions and recovery:** one read-write transaction at a time, many
  readers, copy-on-write B+tree pages, and a two-phase fsync/meta-page commit.
  An incomplete commit rolls back on reopen. This matches Grafo's current
  single-writer model but large write transactions materialize dirty pages.
- **Operations:** a read transaction can create a consistent hot backup;
  `bbolt check` validates page reachability. The single file is simple to copy,
  but compaction requires rewriting it and long readers delay page reuse.
- **Evolution:** Grafo would own logical record/index versions. Page-format
  compatibility is library-controlled; downgrades need validation. Rebuilding
  from source is safer than translating an existing SQLite index.
- **Build and maintenance:** pure Go on macOS, Linux, and Windows; MIT license;
  v1.5.0 was released 2026-06-21 and the repository remains active. Memory
  mapping makes absolute RSS less directly comparable with buffered engines.

### Pebble

- **Transactions and recovery:** the prototype uses an indexed batch for
  read-your-own-writes and commits it with WAL sync. Batches are atomic, but
  Pebble deliberately does not provide general multi-transaction isolation;
  Grafo must continue enforcing one logical writer.
- **Operations:** checkpoints provide a consistent physical copy and the
  `pebble` tool supports inspection. WAL replay and background compaction are
  mature, but backup retention, compaction debt, corruption response, and file
  lifecycle are more involved than SQLite's one primary file plus WAL.
- **Evolution:** Pebble has explicit format-major-version ratcheting and
  downgrade constraints. Grafo would additionally own logical key/index
  versions. Safe rebuild from source is preferable to translating SQLite.
- **Build and maintenance:** pure Go on supported Grafo platforms;
  BSD-3-Clause; v2.1.7 was released 2026-08-24 and is the active major line.
  Its dependency and tuning surface is materially larger than bbolt or SQLite.

## Results and recommendation

The measured medians and final recommendation are filled from the committed
report after the three-sample Uzir run. Until that report exists, no candidate
is eligible for a performance recommendation.
