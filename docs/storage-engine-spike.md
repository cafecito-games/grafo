# Embedded storage engine spike

Status: complete. **Recommendation: retain SQLite.** This note covers issue
#47 and does not change Grafo's production storage default.

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

Committed report: [issue 47 storage comparison](benchmarks/issue-47/storage-comparison.json).

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

**Retain SQLite.** Both prototypes passed the final correctness gate, but
neither met the predeclared performance or resource thresholds. The
alternatives also require Grafo to own substantially more indexing, recovery,
and format-evolution code, so there is no operational benefit that offsets the
measured regressions.

### At a glance

All ratios below are candidate median divided by SQLite median. Lower is
better; `1.00x` is SQLite parity.

```text
Median Uzir cold index time
SQLite  ██████████        1.00x   8m 27s
bbolt   ███████████████   1.46x  12m 21s
Pebble  █████████████████ 1.68x  14m 11s
```

| Predeclared gate | bbolt | Pebble |
| --- | --- | --- |
| Exact counts, query semantics, incremental convergence, and restart/resume | Pass | Pass |
| At least 2x faster generated persistence/reconciliation and at least 30% faster Uzir cold index | **Fail**: 5.39x generated and 1.46x Uzir cold | **Fail**: 1.64x generated and 1.68x Uzir cold |
| No greater than 20% size/RSS regression and no systematic unchanged/incremental regression | **Fail**: 2.48x database, 29.18x RSS, 3.20x unchanged | **Fail**: database is 39% smaller, but RSS is 13.58x and unchanged is 4.29x |
| Operational advantage sufficient to justify migration | **Fail**: manual indexes/encoding and compaction add work | **Fail**: manual indexes/encoding plus WAL/compaction tuning add work |

### Correctness result

The final report marks every engine valid. Each produced the same generated
fixture counts and semantic digest, and every Uzir scenario matched SQLite's
file, node, fact, edge, external-node, node-kind, and edge-kind counts. The
candidate tests also cover lookup ordering, selector match evidence,
traversal, ambiguity, unresolved targets, external-node deduplication, later
internal-declaration convergence, canceled replacement rollback, and durable
restart/resume.

The first full diagnostic run was deliberately excluded. It exposed a defect
in the shared benchmark-only KV resolver: an explicitly supplied target kind
was incorrectly passed through SQLite's fallback edge-kind filter. That left
561 valid `module` and `variable` targets external on the Uzir corpus. A
regression test now pins SQLite equivalence, the resolver was fixed, and the
entire matrix was rerun from a clean commit. This is evidence that the
cross-engine gate detects semantic drift rather than allowing a fast but
different graph into the ranking.

### Median measurements

| Workload | SQLite | bbolt | Pebble |
| --- | ---: | ---: | ---: |
| Generated fixture total | 0.185 s (1.00x) | 0.997 s (5.39x) | 0.302 s (1.64x) |
| Generated persistence | 0.102 s | 0.825 s | **0.049 s** |
| Generated reconciliation | 0.083 s | 0.172 s | 0.254 s |
| Uzir cold total | **507.304 s** (1.00x) | 741.412 s (1.46x) | 850.945 s (1.68x) |
| Uzir cold persistence | **208.273 s** | 436.668 s | 217.945 s |
| Uzir cold reconciliation | **276.282 s** | 280.603 s | 600.730 s |
| Unchanged refresh | **3.063 s** (1.00x) | 9.791 s (3.20x) | 13.148 s (4.29x) |
| Restart/resume cold total | **512.271 s** (1.00x) | 731.416 s (1.43x) | 838.700 s (1.64x) |

Pebble is the only candidate with an isolated win: generated-fixture node/fact
persistence is about 2.07x faster than SQLite. Its reconciliation is 3.05x
slower, however, so generated total time still regresses by 64%. On the real
corpus its cold persistence is also 5% slower and reconciliation is 2.17x
slower. The isolated microbenchmark win therefore does not survive the full
graph lifecycle.

| Incremental scenario | SQLite | bbolt | Pebble |
| --- | ---: | ---: | ---: |
| Edit | **3.087 s** | 10.303 s (3.34x) | 15.911 s (5.15x) |
| Delete | **2.618 s** | 10.151 s (3.88x) | 13.603 s (5.20x) |
| Restore | **2.399 s** | 9.745 s (4.06x) | 17.303 s (7.21x) |
| Branch switch | **2.244 s** | 9.636 s (4.29x) | 14.144 s (6.30x) |

| Cold resource | SQLite | bbolt | Pebble |
| --- | ---: | ---: | ---: |
| Primary database | **2.140 GiB** | 5.314 GiB (2.48x) | **1.305 GiB** (0.61x) |
| Auxiliary log after cold run | 0 MiB checkpointed WAL | Not applicable | 542 MiB |
| Peak RSS | **0.201 GiB** | 5.855 GiB (29.18x) | 2.725 GiB (13.58x) |

bbolt's memory-mapped file makes its RSS less comparable to buffered engines,
but that caveat does not change the decision: it is also slower and 2.48x
larger on disk. Pebble's 39% smaller primary database is real, but its
auxiliary log, much larger RSS, and slower cold and incremental behavior fail
the resource/performance gate.

### Provenance and reproducibility

The accepted run used three raw samples per engine, executed sequentially on
the same machine:

| Field | Recorded value |
| --- | --- |
| Machine | Apple M3 Pro, macOS/arm64 |
| Go | 1.26.2 |
| Grafo commit | `d178f72f670170b929e08f07ebeeda642cff4927` (clean) |
| Uzir commit | `2decef60953660e10d6ab053e106cf864fd8d679` (`main`) |
| Corpus coverage | 12,762 tracked; 11,268 routed/indexable files; 95,798,429 bytes indexed |
| Semantic index / graph schema | 16 / 4 |
| Total matrix duration | Approximately 3h 43m |

The machine-readable report preserves every raw sample, median, ratio, count,
write statistic, engine/library version, durability setting, scenario resource
measurement, and input-coverage field. The architecture note rounds values
only for readability.

### Decision boundary

No additional evidence is needed to choose among these implementations: both
candidates miss the migration threshold by wide margins on the representative
corpus. Revisit storage only if a materially different design is proposed—for
example, an engine-specific bulk-build/index layout rather than this common KV
layout—and require it to clear the same correctness gates plus the existing 2x
generated and 30% Uzir cold-time thresholds. Rebuild-from-source should remain
the migration path for any future index format.

### Limitations

- Timing and RSS came from one Apple M3 Pro; absolute values will vary across
  machines. The large relative regressions, exact-count gates, and operational
  analysis are the decision inputs.
- The corpus is one pinned Uzir revision and the generated fixture has 5,000
  nodes/facts. Three samples provide medians, not a full statistical model.
- bbolt and Pebble share one correctness-first KV layout. The result rejects
  migrating to that demonstrated design; it does not prove every possible
  engine-specific layout must perform identically.
- The adapters remain benchmark/test-only and are not wired into the production
  CLI, so this spike introduces no index migration or compatibility change.
