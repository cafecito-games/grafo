# Compact SQLite physical layout spike

Status: complete. **Recommendation: retain the production schema.** This note
covers issue #112 and does not change Grafo's production storage schema or
default adapter. It follows the engine spike in
[storage-engine-spike.md](storage-engine-spike.md) (issue #47), which retained
SQLite itself; this spike asked whether a more compact physical layout of that
same SQLite database could meaningfully shrink the index.

## Decision threshold

A candidate is eligible for a migration proposal only if it satisfies all of
these predeclared gates:

1. exact control-equivalent graph counts, a deterministic query digest that
   matches the control's, and per-scenario equivalence against the same-sample
   control (the control layout's corresponding sample; the layouts run with
   identical seeds and scenario order);
2. at least a 25% median reduction in Uzir post-compact primary database
   bytes; and
3. no performance ratio above 1.20x, covering fixture cold
   total/persistence/reconciliation, every query-suite pattern, and every
   corpus scenario total. A missing ratio counts as a failure. Peak RSS is
   deliberately excluded from this gate (see Limitations).

The control layout runs the production adapter and defines the comparison
baseline, so the gates apply to candidate layouts only. Timing gates never
excuse a correctness failure.

## Candidate selection

All three candidates are SQLite schema variants exercised through the
benchmark-only package `internal/storage/layoutbench`. Each embeds Goose
migrations that mirror the production migration set's version numbers, so a
database seeded by production fails closed at statement preparation when its
schema is incompatible, and no variant can silently migrate a production
index:

| Layout | Decision | Reason |
| --- | --- | --- |
| Production schema | Control | Current migrations and adapter; the exact semantics and plans to preserve. |
| slim-edges | Prototype | Facts stay the canonical evidence; ordinary edge rows keep only identity and resolution columns, hydration joins the fact, and the one derived edge family keeps exact evidence in a compact `derived_edge_evidence` override table. It tests whether the wide evidence columns on `edges` are where the bytes are. |
| integer-keys | Prototype | Nodes gain an integer surrogate key; facts and edges store resolved surrogate references maintained by write-time triggers, and the adjacency indexes key on the surrogates. It tests whether 22-character string node ids dominate the index bytes. |
| index-reform | Prototype | Pure DDL: the small natural-key tables (`meta`, `files`, and the four `dirty_*` queues) re-created `WITHOUT ROWID` so their implicit autoindexes disappear, with every other index kept, widened, or reverted on measured plan evidence. It tests whether index/DDL reform alone moves the size. |

The NOCASE consolidation attempt inside index-reform (widening
`nodes_qualified_resolve` and `nodes_name_resolve` to `COLLATE NOCASE` and
dropping the separate match indexes) was reverted during the spike with live
plan evidence: a binary-collation equality cannot seek a NOCASE b-tree, so
`FindNodesExact` and the external matching joins degraded from covering seeks
to full index scans.

## Prototype boundaries and correctness

`internal/storage/layoutbench` implements each candidate as a `LayoutSpec`:
embedded migrations plus named statement overrides over the production
adapter, with the write and hydration hooks each layout needs. The production
CLI does not import it; `TestProductionBinariesExcludeLayoutbench` execs
`go list -deps` over `cmd/grafo` and `cmd/grafo-benchmark` and fails if either
binary reaches the package. Only `cmd/grafo-storage-layout-benchmark` imports
it, and the production schema and default adapter are unchanged by this spike.

Equivalence is measured, not assumed. Every layout produced the same fixture
counts (50,071 nodes, 60,003 facts, 60,004 edges, 65 external nodes) and the
same deterministic query digest
(`b47fc45e0c48f63854e1b780ca3d7fb6143eea279734366b5f7428912b9d54f8` across 99
probes for control and every variant), and every Uzir scenario matched the
same-sample control's counts. A plan gate additionally captured EXPLAIN QUERY
PLAN for every production statement against each variant schema; all
statements kept covered or seeking plans. For index-reform the meta/files
plan-evidence strings are hand-written and its migrations are drift-guarded
against the production text by `TestIndexReformMigrationsTrackProduction`, so
a production migration edit
that the reform set does not mirror fails a test rather than a benchmark.

## Reproducible measurements

Run:

```sh
GRAFO_BENCH_REPO=/path/to/uzir \
GRAFO_STORAGE_LAYOUT_OUTPUT=/path/outside/uzir/storage-layout \
GRAFO_STORAGE_LAYOUT_SAMPLES=3 \
GRAFO_STORAGE_LAYOUT_SCALE=5000 \
task bench:storage:layout
```

The command runs each layout sequentially in one process (control first),
three samples per layout, pins the corpus commit at the start of each isolated
run, exercises a generated fixture (seed 2, scale 5000: 505 files, 50,071
nodes, 60,003 facts, 60,004 edges — 110,009 fixture rows) and the Uzir corpus
scenarios, captures dbstat (SQLite's virtual table reporting per-btree page
usage) object bytes and payload statistics before and
after compaction for attribution, records a 13-pattern query suite (7
repetitions per pattern, median reported), and writes one atomic
`storage-layout.json` (schema 2). Timing ratios are candidate median divided
by control median; lower is better.

Corpus pin caveat: the accepted run pins Uzir at
`2decef60953660e10d6ab053e106cf864fd8d679` — the same commit as issue #47's
accepted run — not at Uzir's current HEAD. At HEAD (`7f51e418e`) current
Grafo leaves residual reconciliation work after a complete index (two batches
re-executed by every no-change run; the `resume_unchanged` one-pass
convergence check aborts), which would contaminate the unchanged/incremental
ratios. That behavior was verified as pre-existing at `main` (the builtin v1
engine fails identically; this branch touches no indexer or service code) and
does not reproduce at `2decef609`; it will be filed as a follow-up issue.

Committed report: [issue 112 storage layout](benchmarks/issue-112/storage-layout.json).

## Results and recommendation

**Retain the production schema.** No candidate approaches the 25% size gate —
the best reduces post-compact Uzir bytes by 8.15% — and two of the three also
fail the performance gate, one catastrophically. The attribution captures show
why: the physical layout of `edges` is not where the bytes are.

### At a glance

| Predeclared gate | slim-edges | integer-keys | index-reform |
| --- | --- | --- | --- |
| Exact counts, deterministic digest, per-scenario equivalence | Pass | Pass | Pass |
| Every statement keeps a covered/seeking plan | Pass | Pass | Pass |
| At least 25% median reduction in Uzir post-compact primary bytes | **Fail**: 8.15% | **Fail**: 5.46% | **Fail**: 0.03% |
| No performance ratio above 1.20x | **Fail**: external-edges-probe 1.90x, edges-to-hub 1.40x, edges-from-hub 1.31x | **Fail**: relation-edges-incoming 52.75x, relation-edges-outgoing 50.19x, scenario totals 1.37x–1.59x | Pass: worst gated ratio 1.04x |

### Median measurements

Generated fixture (scale 5000):

| Fixture workload | Production | slim-edges | integer-keys | index-reform |
| --- | ---: | ---: | ---: | ---: |
| Cold total | 8.737 s (1.00x) | 9.035 s (1.03x) | 9.695 s (1.11x) | 8.540 s (0.98x) |
| Cold persistence | **4.336 s** (1.00x) | 4.756 s (1.10x) | 5.861 s (1.35x) | 3.927 s (0.91x) |
| Reconciliation | 4.540 s (1.00x) | 4.273 s (0.94x) | **3.834 s** (0.84x) | 4.613 s (1.02x) |
| Post-compact primary bytes | 309,903,360 | 294,469,632 (−5.0%) | 256,389,120 (−17.3%) | 309,788,672 (−0.04%) |

Uzir corpus scenario totals, plus the persistence-ratio range across the
incremental scenarios:

| Scenario | Production | slim-edges | integer-keys | index-reform |
| --- | ---: | ---: | ---: | ---: |
| Cold total | 714.086 s (11m 54s, 1.00x) | 710.550 s (1.00x) | 660.255 s (0.92x) | 728.800 s (1.02x) |
| Restart/resume cold total | 708.620 s (1.00x) | 702.407 s (0.99x) | 654.418 s (0.92x) | 725.788 s (1.02x) |
| Unchanged total | 2.288 s (1.00x) | 2.370 s (1.04x) | 3.648 s (1.59x) | 2.338 s (1.02x) |
| Edit total | 2.655 s (1.00x) | 2.716 s (1.02x) | 4.226 s (1.59x) | 2.670 s (1.01x) |
| Delete total | 2.336 s (1.00x) | 2.306 s (0.99x) | 3.515 s (1.50x) | 2.314 s (0.99x) |
| Restore total | 2.449 s (1.00x) | 2.627 s (1.07x) | 3.557 s (1.45x) | 2.502 s (1.02x) |
| Branch switch total | 2.658 s (1.00x) | 2.603 s (0.98x) | 3.641 s (1.37x) | 2.597 s (0.98x) |
| Resume unchanged total | 2.287 s (1.00x) | 2.384 s (1.04x) | 3.493 s (1.53x) | 2.333 s (1.02x) |
| Incremental persistence (range, all 11 non-cold scenarios) | 1.00x | 0.96x–1.14x | 1.41x–2.09x (cold 0.95x) | 0.97x–1.02x |

Query suite (13 patterns, 7 repetitions each, median ratio to control):

| Pattern | slim-edges | integer-keys | index-reform |
| --- | ---: | ---: | ---: |
| external-edges-probe | **1.90x** | 1.01x | 0.98x |
| edges-to-hub | **1.40x** | 1.26x | 0.94x |
| edges-from-hub | **1.31x** | 1.29x | 0.91x |
| relation-edges-incoming | **1.13x** | **52.75x** | 0.98x |
| relation-edges-outgoing | 1.05x | **50.19x** | 0.95x |
| All other patterns | ≤ 1.00x | ≤ 1.04x | ≤ 1.04x |

Uzir database size:

| Measurement | Production | slim-edges | integer-keys | index-reform |
| --- | ---: | ---: | ---: | ---: |
| Cold-run primary bytes | 2,515,279,872 | 2,322,710,528 (−7.66%) | 2,378,723,328 (−5.43%) | 2,514,178,048 (−0.04%) |
| Post-compact primary bytes (gate input) | 2,363,404,288 | 2,170,785,792 (−8.15%) | 2,234,368,000 (−5.46%) | 2,362,585,088 (−0.03%) |
| Peak RSS (observation only) | 0.978 GiB | 1.052 GiB | 1.044 GiB | 1.101 GiB |

### Where the bytes are

The attribution captures (dbstat objects, Uzir post-compact, median sample)
explain every gate result. In the control database of 2,363,404,288 bytes:

| Cluster | Bytes | Share |
| --- | ---: | ---: |
| Payload tables: `facts` 559.3 MB, `edges` 446.7 MB, `nodes` 245.1 MB | 1,251,160,064 | 52.9% |
| `facts` indexes (owner, target, from_id, target_id, source, implicit unique) | 435,679,232 | 18.4% |
| `edges` indexes (from, to, fact, implicit unique) | 430,702,592 | 18.2% |
| `nodes` indexes (resolve, match, owner, kind, external, implicit unique) | 242,913,280 | 10.3% |
| Queue/config cluster (`files`, `meta`, `dirty_*`, `embeddings`, goose, autoindexes) | 2,949,120 | 0.12% |

slim-edges removes the wide evidence columns from `edges` and the payload
table shrinks 44.1% (446,672,896 to 249,847,808 bytes), but the four `edges`
indexes — which never carried those columns and together hold 430,702,592
bytes — are byte-identical to the control's, and the new
`derived_edge_evidence` table plus its autoindex add 4,206,592 bytes. The net
is −192,618,496 bytes, or 8.15%: slimming edge rows cannot reach 25% because
the evidence columns were never more than about a twelfth of the database.

integer-keys attacks the indexes instead: `edges_from` and `edges_to` shrink
62% each (143,302,656 to 54,194,176 and 143,065,088 to 54,214,656 bytes,
−177,958,912 combined). But the surrogate columns it must store grow
everything they touch — `edges` payload +19,132,416, `facts` payload
+14,782,464, `facts_from_id` +9,175,040, `facts_target_id` +5,099,520,
`nodes` payload +729,088 (the `node_key` column itself), and `files` +4,096 —
and the net is −129,036,288 bytes, or 5.46%. At fixture scale the same trade
looks far better (−17.3%: `edges_from`/`edges_to` collapse from ~28.2 MB each
to 1,277,952 bytes) because the fixture's fan concentrates adjacency in those
two indexes; at corpus scale the wide tables and the `facts` indexes dilute it.

index-reform saves exactly 819,200 bytes: the `files` autoindex (778,240), the
five queue autoindexes the WITHOUT ROWID re-creations drop (20,480), and the
`files` table's own shrink (20,480). That is 0.03% of the database — the
queue/config cluster it targets is 0.12% of the total, a rounding error next
to `nodes`/`facts`/`edges`.

`derived_edge_evidence` sizing comes from these dbstat captures, not from
WriteStats: at Uzir it holds 3,653,632 bytes plus a 552,960-byte autoindex
(about 0.19% of slim-edges' database); at fixture scale it is a single page,
because only the derived test-edge family whose evidence differs from its
fact writes an override row.

### Why each candidate fell short

**slim-edges** moves the evidence columns off the edge row and pays for every
read: adjacency hydration becomes a three-way join rendering
`COALESCE(de.x, f.x)` per column, so every pattern that emits many edges
regresses — external-edges-probe 1.90x, edges-to-hub 1.40x, edges-from-hub
1.31x, relation-edges-incoming 1.13x — while all other patterns stay at or
below parity. Corpus scenario totals stay within 1.08x and the fixture is
1.03x, so the query suite alone fails the 1.20x budget. With only 8.15% size
reduction on offer, the trade buys nothing.

**integer-keys** keys adjacency on integer surrogates resolved once per
lookup, but the relation-edge queries still order and join on the textual
counterpart ids, which the integer-key indexes do not serve: the captured
plans show `USE TEMP B-TREE FOR ORDER BY` over the whole match set (the
control needs only the last ORDER BY term) with a per-row join back to
`nodes` by textual id, and the patterns blow up to 52.75x and 50.19x. The
surrogates also cost write amplification: the migration's retire/heal
triggers fire four indexed `UPDATE`s (facts and edges, from and target) on
every node insert and delete, and every incremental scenario's persistence
regresses to between 1.41x and 2.09x (the cold run's bulk insert path is
unchanged at 0.95x, which is why the cold totals look fine at 0.92x while
everyday incremental work more than doubles). A 5.46% size reduction cannot
pay for that.

**index-reform** is the only candidate that passes the performance gate (no
gated ratio above 1.04x) — it changes nothing the hot paths touch — but it
also changes nothing the bytes care about: 0.03% at Uzir scale. Its value was
the plan evidence: it documented, with captured EXPLAIN QUERY PLAN, that the
production index set has no free consolidation (every index is either pinned
by `INDEXED BY`, the only b-tree on its column, or measured worse when
widened or collated), and that the two NOCASE consolidation attempts revert
on live regressions.

### Provenance

| Field | Recorded value |
| --- | --- |
| Machine | Apple M3 Pro, macOS/arm64, 12 cores, 36 GB (`Christians-MacBook-Pro-2.local` — identical to issue #47's machine) |
| Host limits | cgroup v2 unavailable on darwin; cpu and memory limits substituted from sysctl `hw.ncpu` and `hw.memsize` |
| Go | 1.26.2 |
| Grafo commit | `83354225ef2764831b07c76ac9241713c27b84e0` (clean; the commits after it on this branch are documentation only) |
| Uzir commit | `2decef60953660e10d6ab053e106cf864fd8d679` (detached; the same commit as issue #47's accepted run) |
| Indexed files at pin | 11,338 |
| Semantic index / graph schema | 29 / 11 |
| Samples | 3 per layout, sequential, one process, control first |
| Fixture | seed 2, scale 5000 — 110,009 rows |
| Report | schema 2, `status: passed` |

The machine-readable report preserves every raw sample, median, ratio, count,
write statistic, dbstat object, payload statistic, captured plan, and query
suite repetition. This note rounds values only for readability.

### Cold-total delta against issue #47

The control's cold median here is 714.086 s (restart/resume 708.620 s)
against #47's 507.304 s (restart/resume 512.271 s) on the same machine and
the same corpus commit. This is consistent with Grafo feature growth between
`d178f72f670170b929e08f07ebeeda642cff4927` (semantic index v16, graph schema
v4) and this branch (v29/v11), not a regression introduced by the spike: at
identical corpus content the graph carries 621,802 nodes versus #47's
619,200, 2,307,621 edges versus 2,288,574, and 2,289,616 facts versus
2,288,574 — the schema now records 19,680 test nodes, 14,055
`generated_from` edges, and `transport_operation` evidence nodes — and the
database grew from 2.14 GiB to 2.34 GiB. The control is the production
schema; the spike compares layouts within this run, never against #47's
absolute timings.

### What would move the needle

The attribution says the mass sits in the `nodes`/`facts`/`edges` payload
(52.9%) and the high-cardinality string-key indexes on those tables (46.9%),
with node ids averaging 22 characters and repeated across `facts.from_id`,
`facts.target_id`, `edges.from_id`, `edges.to_id`, and every adjacency index
entry. Only payload-level changes — shorter ids or normalized/deduplicated
columns — could plausibly reach a 25% reduction, and both change what
producers write and what every persisted id means. Those were out of scope
for this spike under its fail-closed rules around producer compatibility; a
future proposal would have to clear the same correctness, plan, and
performance gates with a migration and rebuild story for existing indexes.

### Limitations

- The owner-reported ~31-minute Coder observation was never reproduced. All
  measurements here are local on the M3 Pro by owner direction; the report's
  recorded disposition is "uncorroborated, no Coder run performed; owner
  directed local substitution". Every ratio compares same-machine runs only.
- The corpus is one pinned Uzir revision (`2decef609`, issue #47's commit),
  not current HEAD; see the corpus pin caveat above. The residual
  reconciliation behavior at HEAD is pre-existing and will be filed
  separately.
- Three samples per layout provide medians, not a statistical model. Timing
  and RSS come from one Apple M3 Pro; the large relative regressions, exact
  counts, plan captures, and byte attributions are the decision inputs.
- Peak RSS is captured per layout but excluded from the cross-layout
  performance gate: sequential in-process sampling is order-biased (the
  control runs first; later layouts inherit retained heap), so ordering alone
  could fail the gate. The raw per-layout medians remain in the report as
  observation only, and the report records this limitation per layout.
- WriteStats are not write-cost evidence across variants: the variant
  adapter's WriteStats cannot see evidence-table batch writes, so variant
  write byte counts must not be cited as write-cost comparisons (slim-edges'
  apparent edge-write halving at Uzir — 217,968,006 versus the control's
  434,581,488 bytes — is the invisible `derived_edge_evidence` batch writes,
  not a measured saving). Write cost is judged from the timed persistence
  ratios, which is why integer-keys' 1.41x–2.09x incremental persistence
  matters and its recorded write bytes do not.
- The index-reform meta/files plan-evidence strings are hand-written and its
  migrations are drift-guarded against production text by test; its other
  plan evidence is captured EXPLAIN QUERY PLAN output.
- `internal/storage/layoutbench` is benchmark-only and unreachable from
  `cmd/grafo` and `cmd/grafo-benchmark` (pinned by an isolation test that
  execs `go list -deps`); only `cmd/grafo-storage-layout-benchmark` imports
  it. This spike introduces no index migration or compatibility change.
