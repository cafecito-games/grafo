# Storage Layout Spike (Issue #112) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers-extended-cc:subagent-driven-development (recommended) or superpowers-extended-cc:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce a reproducible, correctness-gated comparison of compact SQLite graph layouts (slim edges, integer internal keys, index reform) against the production schema, with a machine-readable report and an evidence-backed retain/proceed recommendation.

**Architecture:** A new benchmark-only package `internal/storage/layoutbench` holds page-attribution capture, query-plan capture, deterministic high-cardinality fixtures, and variant schema adapters. `internal/storagecompare` gains a layout mode that reuses `internal/benchmark` scenario runs and emits a schema-v2 `storage-layout.json`. Candidate 3 (index reform) is pure DDL: variant goose migrations pre-seed the database and the untouched production adapter opens it (verified: goose records version ids only, no checksum rejection, pinned driver v1.57.0 has `dbstat`). Candidates 1 and 2 share one `database/sql` adapter parameterized by a `LayoutSpec` (DDL + SQL deltas + behavioral flags). Nothing here changes production schema, queries, or the default adapter; `cmd/grafo` never imports the prototype code (compile-time-checked).

**Tech Stack:** Go 1.26, modernc.org/sqlite v1.57.0 (WAL, synchronous=NORMAL), goose v3, existing Taskfile targets. Runs execute on the local Apple M3 Pro (same machine class as the accepted #47 Uzir baseline, 8m27s median cold — hostname `Christians-MacBook-Pro-2.local`), per the owner's instruction to substitute this machine for the Coder workspace. The report must state that the ~31-minute Coder observation remains **uncorroborated** because no Coder run was performed.

**Key facts established during exploration (do not re-derive):**

- Production schema: `internal/storage/sqlite/migrations/00001..00007`. Tables `meta, files, nodes, facts, edges, dirty_owners, dirty_nodes, dirty_targets, dirty_facts, reconciliation_cleanup, embeddings`. Index inventory: `nodes_name, nodes_qualified, nodes_owner, nodes_kind` (NOcase/plain), `edges_from, edges_to, facts_owner, facts_target, facts_target_id, dirty_facts_order, edges_fact, nodes_external, nodes_name_resolve, nodes_qualified_resolve, facts_from_id, facts_source, embeddings_model`.
- Adapter: `internal/storage/sqlite/repository.go` (1167 lines) implements `graph.Repository`, `graph.CatalogRepository`, `graph.InstrumentedIndexRepository`, `graph.InstrumentedWriteRepository`, `graph.ReconciliationStatusRepository`, `semantic.Repository`. Bounded batch writer: `internal/storage/sqlite/batch_writer.go`.
- Reconciliation derives ordinary edges 1:1 from facts; derived `tests` edges come from `graph.DirectTestEdge` (`internal/graph/testcoverage.go:19-43`) and differ in kind/location/properties — they are the one edge family whose evidence is not the fact row.
- #47 harness: `internal/storagecompare` (engine comparison, report schema v1, artifact `storage-comparison.json`) and `internal/benchmark` (Uzir corpus scenarios: cold/unchanged/interrupted/resumed/edit/delete/restore/branch_switch/branch_restore/branch_unchanged, phase durations, WAL/RSS sampling). Both are benchmark-only code.
- `dbstat` virtual table IS available in the pinned driver (probed). `EXPLAIN QUERY PLAN` returns `(selectid, order, from, detail)` rows; full scans show `SCAN table`.
- Variant pre-seed trick works (probed): open DB → goose `Up` with variant migrations carrying the production version numbers → close → `sqlite.Open(ctx, path)` succeeds without re-migrating.
- Gates: ≥25% median Uzir primary-DB reduction; ≤20% regression in Uzir cold total, unchanged refresh, any incremental scenario, query suite, peak RSS; zero correctness/query-plan failures; no new operational dependency. Fail-closed rules are in the issue and reproduced in the runner.

**File structure (created unless marked Modify):**

```
internal/storage/layoutbench/            benchmark-only package (new)
  attribution.go        dbstat/page/freelist/WAL/payload capture + compaction
  plans.go              EXPLAIN QUERY PLAN capture + full-scan validation
  fixture.go            deterministic high-cardinality generated fixture
  workload.go           representative query-suite runner (timed, deterministic)
  equivalence.go        exact-equivalence driver control↔variant (fixture scenarios)
  spec.go               LayoutSpec type + registry of layouts
  repository.go         shared database/sql variant adapter (candidates 1 & 2)
  batch.go              bounded batch writer for the variant adapter
  open.go               open variant DB (pre-seed goose + adapter or sqlite.Open)
  variants/slimedges/00001..00007 equivalent migrations (candidate 1 DDL)
  variants/integerkeys/00001..00007 equivalent migrations (candidate 2 DDL)
  variants/indexreform/00001..00007 equivalent migrations (candidate 3 DDL)
  *_test.go             unit + equivalence + isolation tests
internal/storagecompare/layout.go        layout comparison runner + schema-v2 report (Modify package)
internal/benchmark/runner.go             route "layout:<name>" engines to layoutbench (Modify, small)
cmd/grafo-storage-layout-benchmark/      new command (new)
Taskfile.yml                             add bench:storage:layout (Modify)
docs/storage-layout-spike.md             architecture note / recommendation (new)
docs/benchmarks/issue-112/storage-layout.json   committed final report (new)
```

---

### Task 1: Page attribution and size capture

**Goal:** Capture per-object SQLite bytes/pages, page/freelist counts, WAL size, and representative payload-length stats for any SQLite database, with equal-lifecycle checkpoint/compaction points.

**Files:**
- Create: `internal/storage/layoutbench/attribution.go`
- Create: `internal/storage/layoutbench/attribution_test.go`

**Acceptance Criteria:**
- [ ] `CaptureAttribution` returns per-object (table, explicit index, autoindex) byte/page totals from `dbstat`, plus `page_size`, `page_count`, `freelist_count`, WAL and SHM file sizes, and payload-length stats (count, avg, max of `length(id)`, `length(owner_file)`, `length(properties)` for nodes/facts/edges).
- [ ] `dbstat` unavailability is reported as a typed limitation (`dbstat_supported=false`) — never guessed per-object bytes; total file size remains mandatory.
- [ ] `Checkpoint(ctx, db, truncate)` and `Compact(ctx, db)` (VACUUM) helpers exist; attribution is captured pre- and post-compaction separately.

**Verify:** `go test ./internal/storage/layoutbench -run TestAttribution -count=1` → PASS (includes a synthetic DB asserting the autoindex of a TEXT PRIMARY KEY table appears as `sqlite_autoindex_*` and that VACUUM after churn reduces freelist to 0).

**Steps:**

- [ ] **Step 1: Write the failing test** — build a temp DB with churn (insert, delete half, checkpoint, vacuum), assert: attribution totals sum within one page of file size; `freelist_count == 0` post-VACUUM; payload stats report expected counts.
- [ ] **Step 2: Implement** `attribution.go`:

```go
package layoutbench

// Attribution records where the bytes of one SQLite database live.
type Attribution struct {
    DBStatSupported bool              `json:"dbstat_supported"`
    Objects         []ObjectBytes     `json:"objects,omitempty"` // sorted by name
    PageSize        int64             `json:"page_size"`
    PageCount       int64             `json:"page_count"`
    FreelistPages   int64             `json:"freelist_pages"`
    PrimaryBytes    int64             `json:"primary_bytes"` // os.Stat after checkpoint
    WALBytes        int64             `json:"wal_bytes"`
    SHMBytes        int64             `json:"shm_bytes"`
    PayloadStats    []TablePayload    `json:"payload_stats,omitempty"`
    DbstatReason    string            `json:"dbstat_reason,omitempty"`
}

type ObjectBytes struct {
    Name  string `json:"name"`  // table, index, or sqlite_autoindex_*
    Bytes int64  `json:"bytes"`
    Pages int64  `json:"pages"`
}

type TablePayload struct {
    Table          string  `json:"table"`
    Rows           int64   `json:"rows"`
    AvgIDLength    float64 `json:"avg_id_length"`
    MaxIDLength    int64   `json:"max_id_length"`
    AvgOwnerLength float64 `json:"avg_owner_length,omitempty"`
    AvgProperties  float64 `json:"avg_properties_length,omitempty"`
}
```

Queries: `SELECT name, SUM(pgsize), SUM(pagedup_count) ... ` — use `SELECT name, SUM(pgsize) AS bytes, COUNT(*) AS pages FROM dbstat GROUP BY name ORDER BY name` (each dbstat row ≈ one page); `PRAGMA page_size/page_count/freelist_count`; payload stats via `SELECT COUNT(*), AVG(length(id)), MAX(length(id)), AVG(length(owner_file)), AVG(length(properties)) FROM <table>` for nodes/facts/edges (guarded by table existence).
- [ ] **Step 3: Test passes; commit** `feat: add SQLite page attribution capture for layout spike`.

---

### Task 2: Query-plan capture and validation

**Goal:** Capture `EXPLAIN QUERY PLAN` for every production and variant query at representative cardinality, and flag unexplained full scans on high-cardinality paths.

**Files:**
- Create: `internal/storage/layoutbench/plans.go`
- Create: `internal/storage/layoutbench/plans_test.go`

**Acceptance Criteria:**
- [ ] A query inventory (`PlanQuery{Name, SQL, Params []any, HighCardinality bool}`) covers **every** named query in `internal/storage/sqlite/queries/*.sql` plus every variant-adapter override (populated from `LayoutSpec`).
- [ ] `CapturePlans(ctx, db, queries)` returns the plan rows per query; `ValidatePlans` marks a plan invalid when `detail` contains `SCAN <table>` (excluding index-covered scans, i.e. `SCAN table USING INDEX ...` is valid) for high-cardinality queries, and when an intended index (`INDEXED BY` in the SQL) is absent in the variant schema.
- [ ] Plans are captured against a populated high-cardinality fixture database (Task 3), not an empty one.

**Verify:** `go test ./internal/storage/layoutbench -run TestPlan -count=1` → PASS, including a negative test (drop `edges_from` in a scratch DB → the incoming-adjacency plan is flagged invalid).

**Steps:**

- [ ] **Step 1: Failing test** — scratch DB with the production schema + fixture rows; capture plans for `ListEdgesFrom`, `ListIncomingRelationEdges`, `MatchNodesByQualifiedName`, `SearchNodes`, `ListDirtyFactBatch`, `EnqueueDirtyFacts`; assert all valid; then drop `edges_from`, re-capture, assert flagged.
- [ ] **Step 2: Implement** with `EXPLAIN QUERY PLAN <sql>` executed with params; store rows verbatim in the report. Keep the production inventory as a hand-maintained slice mirroring `queries/*.sql` names (a comment in `plans.go` lists the file each entry mirrors; a unit test fails when a `-- name:` in `queries/*.sql` has no inventory entry, parsed with `os.DirFS` relative to the package via `../../storage/sqlite/queries`).
- [ ] **Step 3: Test passes; commit** `feat: capture and validate SQLite query plans for layout candidates`.

---

### Task 3: Deterministic high-cardinality fixture

**Goal:** A generated fixture that separately stresses textual ID length, repeated owner/path strings, ambiguous names, high-degree adjacency, external resolution, and derived test edges — same logical graph for control and every candidate, with insertion-order variants.

**Files:**
- Create: `internal/storage/layoutbench/fixture.go`
- Create: `internal/storage/layoutbench/fixture_test.go`

**Acceptance Criteria:**
- [ ] `GenerateFixture(seed, scale) Fixture` is pure and deterministic; `scale` bounds rows (default profile ≈ 50k nodes / 60k facts); odd/even sample numbers reverse insertion order (matching #47's pattern in `storagecompare/runner.go:224-227`).
- [ ] Fixture content by construction includes: nodes with 200+ character stable IDs (`n:pkg/very/deep/path/...:symbol`), ≥30 files sharing repeated long owner paths, ambiguous plain names declared in ≥2 files, one hub node with ≥5,000 incoming and ≥5,000 outgoing calls edges, facts with textual targets resolving externally, and a test-source function calling production symbols so `DirectTestEdge` fires (mirroring `internal/graph/testcoverage.go` semantics).
- [ ] The fixture renders as `[]graph.FileRecord` + per-file `graph.ParseResult` so it flows through the real `ReplaceFile`/`Reconcile` path of any adapter.

**Verify:** `go test ./internal/storage/layoutbench -run TestFixture -count=1` → PASS (determinism: two calls produce identical JSON digests; reversal variant produces identical post-reconcile counts).

**Steps:**

- [ ] **Step 1: Failing test** for determinism and required shape assertions (counts by kind, presence of ambiguity, hub degree, ≥1 derived test edge after reconcile against the production adapter).
- [ ] **Step 2: Implement** `fixture.go`; no randomness beyond the seed (use a small splitmix64; do not add dependencies).
- [ ] **Step 3: Test passes; commit** `feat: add deterministic high-cardinality fixture for storage layout spike`.

---

### Task 4: Shared variant adapter core (speaks the production layout first)

**Goal:** One `database/sql` adapter implementing the full repository surface, parameterized by `LayoutSpec`. It is first run with a spec whose SQL is exactly the production schema/queries and proven exactly equivalent to the production adapter; candidates 1–2 then become small SQL/DDL deltas.

**Files:**
- Create: `internal/storage/layoutbench/spec.go`
- Create: `internal/storage/layoutbench/repository.go`
- Create: `internal/storage/layoutbench/batch.go`
- Create: `internal/storage/layoutbench/open.go`
- Create: `internal/storage/layoutbench/equivalence.go`
- Create: `internal/storage/layoutbench/equivalence_test.go`

**Acceptance Criteria:**
- [ ] `LayoutSpec{Name, Migrations fs.FS, SQL map[string]string, EdgeInsert func, Hydrate func, PostEdgeDelete func, DerivedEvidence bool, IntegerKeys bool}` — unspecified SQL entries fall back to the production text mirrored from `internal/storage/sqlite/queries/*.sql`.
- [ ] The adapter implements: `graph.Repository`, `graph.CatalogRepository`, `graph.RelationEdgeRepository`, `graph.ExternalEdgeRepository`, `graph.ExternalNodeRepository`, `graph.InstrumentedIndexRepository`, `graph.InstrumentedWriteRepository`, `graph.ReconciliationStatusRepository` — everything the indexer and query paths touch (compile-time `var _` assertions).
- [ ] The adapter body is an adaptation of `internal/storage/sqlite/repository.go` (reconciliation loop `ReconcileWithStats` incl. batch size 10_000 and resolution cache 50_000, resolution/fan-out/ambiguity rules `repository.go:561-660`, external node materialization, orphan cleanup, checkpoint cadence) and `batch_writer.go` (bounded 32-row/1MiB batches), with sqlc replaced by prepared statements from `LayoutSpec.SQL`. Copy — do not refactor production to share; the production package stays untouched.
- [ ] **Equivalence driver** (`equivalence.go`): given two opener funcs, runs the fixture through both and compares exactly — graph counts, per-kind counts, stable IDs, ordered outputs of every access pattern (SearchNodes, MatchNodes ×3 scopes, Node, EdgesFrom/To, RelationEdges both directions, ListNodesByKind ×3 visibilities, ExternalEdgesTo, ExternalNodesMatching, Embeddings API surface), ambiguity outcomes, later-declaration convergence, orphan external cleanup, node folding (`foldName` parity: Turkish İ, ASCII case, CJK), interruption at every `indexer.Boundary` kind + resume convergence, cancellation mid-`ReplaceFile` (transactional rollback), incremental edit/delete/restore/branch-switch behavior, and a deterministic digest over the union. Any difference → `Equivalent=false` with the first differing probe named.
- [ ] `equivalence_test.go`: production adapter vs variant-adapter-with-production-spec on the Task 3 fixture = the red/green harness proving the adapter core before any layout delta lands.

**Verify:** `go test ./internal/storage/layoutbench -count=1` → PASS.

**Steps:**

- [ ] **Step 1:** Write `equivalence_test.go` first — `TestVariantAdapterMatchesProduction` (fails: adapter doesn't exist).
- [ ] **Step 2:** Implement `spec.go` with the production SQL text transcribed from `queries/*.sql` (verbatim; the plan-inventory test from Task 2 keeps them aligned).
- [ ] **Step 3:** Implement `repository.go`/`batch.go`/`open.go` by adapting the named production files; keep method order and logic traceable (side-by-side review is part of Task 10's review round).
- [ ] **Step 4:** Equivalence green; commit `feat: add layout spike variant adapter equivalent to production`.

---

### Task 5: Candidate 1 — slim resolved adjacency

**Goal:** Facts remain the canonical evidence; ordinary edge rows keep only `id, fact_id, from_id, to_id, kind`; hydration joins the fact; derived `tests` edges keep exact evidence via a compact override table.

**Files:**
- Create: `internal/storage/layoutbench/variants/slimedges/00001_initial.sql` … `00007_fact_edge_producers.sql` (same version numbers as production; content: production DDL with the `edges` table replaced and `derived_edge_evidence` added)
- Modify: `internal/storage/layoutbench/spec.go` (register `slim-edges`)

**Schema deltas (exact):**

```sql
-- edges (slim): identity/resolution/adjacency only
CREATE TABLE edges (
    id TEXT PRIMARY KEY,
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL
);
CREATE INDEX edges_from ON edges(from_id, kind, to_id);
CREATE INDEX edges_to ON edges(to_id, kind, from_id);

-- exact evidence for the one derived edge family (rare rows)
CREATE TABLE derived_edge_evidence (
    edge_id TEXT PRIMARY KEY,
    producer TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    line INTEGER NOT NULL DEFAULT 0,
    column_no INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    properties TEXT NOT NULL DEFAULT '{}'
);
```

Remaining migrations mirror production 00002–00007 minus dropped edge columns (`edges_fact` index unchanged — it keys on `fact_id` only).

**SQL deltas (exact, in `LayoutSpec.SQL`):**

```sql
-- InsertEdge becomes two statements (second only when the edge is derived):
INSERT INTO edges(id, fact_id, from_id, to_id, kind) VALUES (?, ?, ?, ?, ?);
INSERT OR REPLACE INTO derived_edge_evidence(edge_id, producer, path, line, column_no, end_line, properties)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- ListEdgesFrom / ListEdgesTo (ordering preserved):
SELECT e.id, e.fact_id, e.from_id, e.to_id, e.kind,
       COALESCE(de.producer, f.producer) AS producer,
       COALESCE(de.path, f.path) AS path,
       COALESCE(de.line, f.line) AS line,
       COALESCE(de.column_no, f.column_no) AS column_no,
       COALESCE(de.end_line, f.end_line) AS end_line,
       COALESCE(de.properties, f.properties) AS properties
FROM edges e
JOIN facts f ON f.id = e.fact_id
LEFT JOIN derived_edge_evidence de ON de.edge_id = e.id
WHERE e.from_id = ? ORDER BY e.kind, e.to_id, e.id;
```

`ListIncomingRelationEdges`/`ListOutgoingRelationEdges`/`ListExternalEdgesMatching` gain the same join (counterpart node join unchanged). `DeleteEdgesByDirtyFactBatch`/`DeleteEdgesByOwnerFacts` are each preceded by `DELETE FROM derived_edge_evidence WHERE edge_id IN (SELECT id FROM edges WHERE <same predicate>)` via `PostEdgeDelete`.

**Acceptance Criteria:**
- [ ] Equivalence driver passes production ↔ slim-edges on the fixture, including a dedicated derived-test-edge probe (test source calls production symbol; hydrated `tests` edge evidence — kind, producer, location, properties — is byte-identical to production output).
- [ ] `EnqueueDirtyFacts`'s edges-existence subquery (`repository.go:411-416` region) still resolves via `edges(fact_id, from_id)`-covered columns — plan-captured and valid.
- [ ] dbstat attribution shows the edges table + its indexes shrink; hydration query plans show no scan.

**Verify:** `go test ./internal/storage/layoutbench -run 'TestEquivalence.*Slim|TestSlim' -count=1` → PASS.

**Steps:** failing equivalence test → migrations + spec registration → green → commit `feat: add slim-edges layout candidate`.

---

### Task 6: Candidate 2 — compact integer internal keys

**Goal:** Stable public IDs unchanged at every repository boundary; adjacency and high-volume secondary indexes switch to stable integer surrogate keys; measure lookup translation and incremental replacement cost.

**Files:**
- Create: `internal/storage/layoutbench/variants/integerkeys/00001_initial.sql` … `00007_fact_edge_producers.sql`
- Modify: `internal/storage/layoutbench/spec.go` (register `integer-keys`)

**Schema deltas (exact):**

```sql
-- nodes gain a VACUUM-stable integer surrogate (INTEGER PRIMARY KEY never
-- renumbers; the textual id becomes a UNIQUE index):
CREATE TABLE nodes (
    node_key INTEGER PRIMARY KEY,
    id TEXT NOT NULL UNIQUE,
    ... same columns as production ...
);

-- edges keep textual identity columns (ordering semantics) but key
-- adjacency on integers:
CREATE TABLE edges (
    id TEXT PRIMARY KEY,
    fact_id TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    producer TEXT NOT NULL DEFAULT '',
    ... evidence columns as production ...,
    from_key INTEGER NOT NULL,
    to_key INTEGER NOT NULL
);
CREATE INDEX edges_from ON edges(from_key, kind, to_key);
CREATE INDEX edges_to ON edges(to_key, kind, from_key);
CREATE INDEX edges_fact ON edges(fact_id);

-- facts secondary lookups keyed on node surrogates where the reference is a
-- node id (textual name lookups keep their indexes):
CREATE TABLE facts ( ... production columns ...,
    from_key INTEGER NOT NULL DEFAULT 0,
    target_key INTEGER NOT NULL DEFAULT 0 );
CREATE INDEX facts_from_id ON facts(from_key);
CREATE INDEX facts_target_id ON facts(target_key);
```

`facts_owner`, `facts_target`, `facts_source`, dirty queues, and node match indexes stay textual (they key on paths/names, not node ids — the isolated measurement keeps this attributable).

**SQL/behavior deltas:**
- `UpsertFact` resolves `from_key`/`target_key` via `(SELECT node_key FROM nodes WHERE id = ?)` subselects (0 when unresolvable at write time; reconciliation is the authority and re-resolves).
- `InsertEdge` resolves `from_key`/`to_key` the same way.
- `EnqueueDirtyFacts`'s `facts.from_id = dirty_nodes.node_id` arms become `facts.from_key = dirty_nodes.node_key` (dirty_nodes gains `node_key INTEGER NOT NULL` written alongside `node_id`).
- `ListEdgesFrom`/`ListEdgesTo`/relation queries translate the public id → `node_key` once, then seek integer indexes; hydration returns textual ids from the row (unchanged output).
- Key stability invariant: any node delete+reinsert changes `node_key`; correctness depends on the existing dirty-propagation guarantee that every edge referencing a changed node is re-derived (dirty_nodes → facts → edges). The equivalence suite's edit/delete/restore/branch scenarios are the proof; if any scenario dangles a key, the candidate is marked invalid per the fail-closed contract.

**Acceptance Criteria:**
- [ ] Equivalence driver passes production ↔ integer-keys including incremental scenarios and interruption/resume.
- [ ] No integer surrogate is observable through any `graph.Repository` method (digest proves it: outputs identical).
- [ ] Attribution isolates the index-size delta; the report records insert-time translation cost (persistence phase) separately from lookup gain (query suite).

**Verify:** `go test ./internal/storage/layoutbench -run 'TestEquivalence.*Integer|TestInteger' -count=1` → PASS.

**Steps:** failing equivalence test → migrations + spec + adapter hooks (`IntegerKeys` flag paths) → green → commit `feat: add integer-keys layout candidate`.

---

### Task 7: Candidate 3 — B-tree/index reform (pure DDL)

**Goal:** Justified `WITHOUT ROWID`, composite/covering index, collation-consolidated lookup indexes, and redundancy removal — driven by measured dbstat attribution and verified plans, with zero adapter changes.

**Files:**
- Create: `internal/storage/layoutbench/variants/indexreform/00001_initial.sql` … `00007_fact_edge_producers.sql`
- Modify: `internal/storage/layoutbench/spec.go` (register `index-reform`), `open.go`

**Approach:** the variant migrations reproduce the production schema with the same goose version numbers; `open.go` pre-seeds via goose then calls `sqlite.Open` (the probed trick) so the **production adapter itself** runs the candidate — the cleanest possible production-equivalence. Because `nodes_name`/`nodes_qualified` are `COLLATE NOCASE` and serve the `COLLATE NOCASE` equality in the MatchNodes queries while `nodes_*_resolve` are binary-collation, they are NOT redundant; consolidation must add NOCASE to the composite instead of dropping.

**Reform set (each measured; drop any element whose plan degrades):**

```sql
-- (a) consolidate node lookup indexes: replace nodes_qualified + nodes_qualified_resolve
--     with one covering composite:
CREATE INDEX nodes_qualified_resolve ON nodes(qualified_name COLLATE NOCASE, external, kind, id);
-- ...and nodes_name + nodes_name_resolve likewise. MatchNodes/FindNodesExact
-- queries then seek one index per level; captured plans must show it.
-- (b) WITHOUT ROWID for the queue/config tables (tiny rows, natural keys):
--     meta, files, dirty_owners, dirty_targets(PK already composite), dirty_facts
--     re-created WITHOUT ROWID with their PRIMARY KEYs.
-- (c) trim redundant single-column indexes where a composite covers them:
--     evaluate nodes_kind (GROUP BY covering), facts_owner vs a future
--     composite, edges_fact vs edges covering needs — keep only what plans prove.
-- (d) nodes_external partial index retention check (used by orphan cleanup).
```

**Acceptance Criteria:**
- [ ] Equivalence passes (production adapter on variant DDL vs production adapter on production DDL — same code both sides).
- [ ] Every reformed index is justified in the report by a captured plan showing it serves its queries; every removed index has plan evidence the remaining composite covers the access path.
- [ ] If a reform regresses a plan to a scan, the element is reverted and the reversion recorded — the fail-closed table's "invalid until corrected" rule.

**Verify:** `go test ./internal/storage/layoutbench -run 'TestEquivalence.*IndexReform|TestIndexReform' -count=1` → PASS; `go test ./internal/storage/layoutbench -run TestPlan -count=1` → PASS against the index-reform DB.

**Steps:** baseline attribution analysis of the control fixture DB (Task 1 tooling) to rank indexes by bytes → migrations for the justified set → equivalence + plan tests → commit `feat: add index-reform layout candidate`.

---

### Task 8: Layout comparison runner, schema-v2 report, gates, and query workload

**Goal:** The orchestrating runner: control + candidates × N samples on generated fixture and Uzir, sequential, isolated empty databases, checkpointed+compacted size capture, plan capture, query-suite timing, provenance, and gate evaluation — emitting `storage-layout.json`.

**Files:**
- Create: `internal/storage/layoutbench/workload.go`
- Create: `internal/storagecompare/layout.go` (+ `layout_test.go`)
- Modify: `internal/benchmark/runner.go` (route engine names `layout:<name>` and `control` to `layoutbench.Open` inside its private `openRepository`; no other change)
- Create: `cmd/grafo-storage-layout-benchmark/main.go`
- Modify: `Taskfile.yml` (add `bench:storage:layout`)

**Report shape (schema_version 2, artifact `storage-layout.json`):**

```go
type LayoutReport struct {
    SchemaVersion int                `json:"schema_version"` // 2
    GeneratedAt   string             `json:"generated_at"`
    Status        string             `json:"status"`
    Error         string             `json:"error,omitempty"`
    Machine       Machine            `json:"machine"`   // reuse storagecompare.Machine + limits below
    HostLimits    HostLimits         `json:"host_limits"` // cpu cores, mem bytes, disk free at output dir; cgroup fields empty on darwin with reason
    Samples       int                `json:"samples"`
    FixtureRows   int                `json:"fixture_rows"`
    GrafoCommit   string             `json:"grafo_commit"`
    GrafoDirty    bool               `json:"grafo_dirty"`
    Corpus        benchmark.Corpus   `json:"corpus"`
    SemanticIndexVersion string      `json:"semantic_index_version"`
    GraphSchemaVersion   int         `json:"graph_schema_version"`
    ControlObservation string        `json:"coder_observation_disposition"` // fixed text: uncorroborated, no Coder run performed; owner directed local substitution
    Layouts       []LayoutResult     `json:"layouts"`
}

type LayoutResult struct {
    Name         string                `json:"name"` // control | slim-edges | integer-keys | index-reform
    Correctness  LayoutCorrectness     `json:"correctness"` // equivalence digest/counts/validity + reasons
    Fixture      FixtureResult         `json:"fixture"`     // timings, db bytes pre/post compaction, attribution, writes
    Corpus       CorpusResult          `json:"corpus"`      // reuse storagecompare.CorpusResult (scenario medians incl. phase timings)
    Attribution  []Attribution         `json:"attribution"` // per sample point: fixture-final + uzir-cold-final, pre/post compaction
    Plans        []PlanCapture         `json:"plans"`
    QuerySuite   []QueryMetric         `json:"query_suite"` // per access pattern: median ns, ratio to control
    Gates        GateEvaluation        `json:"gates"`
}

type GateEvaluation struct {
    SizeReduction   float64  `json:"size_reduction_ratio"` // median uzir primary bytes vs control
    MeetsSizeGate   bool     `json:"meets_size_gate"`      // >= 25% reduction
    MeetsPerfGates  bool     `json:"meets_perf_gates"`     // every ratio <= 1.20
    MeetsCorrectness bool   `json:"meets_correctness_gate"`
    MeetsPlanGate   bool     `json:"meets_plan_gate"`
    Valid           bool     `json:"valid"`
    Reasons         []string `json:"reasons,omitempty"`
}
```

**Runner rules (fail-closed, from the issue):** corpus must be a clean pinned worktree (reuse `benchmark.Run`'s isolation clone); abort before samples otherwise. Every sample starts from an isolated empty database; no reuse. Compact before final size capture; report pre/post separately; require zero pending reconciliation before compaction. Sample failure/OOM marks the sample invalid — never median partial sets. Baseline provenance mismatch → reject ratios. `dbstat` unavailable → record limitation, totals mandatory. Machine limits from cgroup v2 when present, else sysctl (`hw.ncpu`, `hw.memsize`) with the substitution recorded.

**Query workload (`workload.go`):** deterministic suite over the final compacted DB — for each production access pattern (the Task 2 inventory): fixed parameter sets derived from the fixture/corpus (e.g. hub-node adjacency, ambiguous-name MatchNodes, external-node probes), k repetitions, median recorded; identical for control and candidates.

**Acceptance Criteria:**
- [ ] `task bench:storage:layout` runs end-to-end on the fixture-only path (no corpus) in CI-safe time; corpus path gated behind `GRAFO_BENCH_REPO`.
- [ ] `go test ./internal/storagecompare -count=1` passes including a unit test of gate evaluation (boundary: 25.0% passes, 24.9% fails; 1.20x passes, 1.21x fails) and report round-trip.
- [ ] Isolation test: `go list -deps ./cmd/grafo ./cmd/grafo-benchmark` output contains no `layoutbench` import path (run as a Go test in `layoutbench` via `exec` of `go list`), and `storagecompare` engine mode (v1 report) is unchanged.
- [ ] `task check` passes.

**Steps:** workload tests → runner tests (fixture-only dry run with 1 sample) → cmd + Taskfile → commit `feat: add storage layout comparison runner and schema-v2 report`.

---

### Task 9: Execute the comparison locally

**Goal:** The actual measured evidence: control + candidates, generated fixture + Uzir, 3 samples each, sequential, on this machine.

**Files:** no source changes expected; fixes to candidates allowed only with re-run of all affected samples (fail-closed).

**Procedure:**

- [ ] Confirm `../uzir` is a clean worktree at the pinned revision recorded by the harness; never modify it.
- [ ] Command (from the worktree root, output under `$HOME` per issue, adapted from the issue's verification block):

```sh
GRAFO_BENCH_REPO=$HOME/CafecitoGames/uzir \
GRAFO_STORAGE_LAYOUT_OUTPUT=$HOME/grafo-storage-layout \
GRAFO_STORAGE_LAYOUT_SAMPLES=3 \
task bench:storage:layout
```

- [ ] Budget: Uzir cold ≈ 8.5 min on this machine class; each engine sample (cold + 12 scenarios) ≈ 15–20 min ⇒ 4 layouts × 3 samples ≈ 3–4 h wall clock; run in background, sequential (no cgroup on darwin; no parallel candidates).
- [ ] After the run: verify every layout's correctness gate; extract medians, ratios, attribution tables, plans; sanity-check that control Uzir cold total is in the same range as the #47 accepted run (8m27s) — record actual.
- [ ] Any candidate failing correctness or plan gates: fix or mark invalid (per fail-closed contract) and re-run that candidate's full sample set.

**Verify:** `storage-layout.json` exists with status `passed`, 4 layouts, all raw samples present; `python3 -m json.tool` parses it.

---

### Task 10: Architecture note and recommendation

**Goal:** `docs/storage-layout-spike.md` mirroring `docs/storage-engine-spike.md`'s rigor: results at a glance, page-attribution findings, per-candidate isolated results, gate table, and the retain/proceed decision with exact follow-up boundary if a candidate wins.

**Files:**
- Create: `docs/storage-layout-spike.md`
- Create: `docs/benchmarks/issue-112/storage-layout.json` (committed copy of the final report)

**Content requirements (from the issue):**
- [ ] Byte/page attribution for every table, autoindex, and explicit index on generated and Uzir databases.
- [ ] Control phase timings; explicit statement that the ~31-minute Coder observation is uncorroborated (local substitution per owner instruction) and that candidate ratios use same-machine runs only.
- [ ] All three candidate families with isolated measurements; combinations only if justified, retaining attributable results.
- [ ] Gates applied exactly (25% size, 20% regression) with operational/migration complexity stated.
- [ ] If a winner exists: exact production tables/indexes/queries/migration boundary, derived-test-edge handling, compatibility versioning, rebuild strategy — enough to file a bounded implementation issue. If none: explicit retention of the current layout, no manufactured proposal.
- [ ] Prose tables derive from the machine-readable report only.

**Verify:** `task check` → PASS; commit `docs: add storage layout spike results and recommendation`.

---

### Task 11: Verification, review chain, PR

**Goal:** Full verification, independent GLM review (configured project reviewer), convergence, PR, merge.

**Steps:**
- [ ] `task check` and `go test -race ./internal/storage/sqlite ./internal/storagecompare ./internal/benchmark ./internal/storage/layoutbench -count=1` all green.
- [ ] Confirm zero production diff beyond: `internal/benchmark/runner.go` engine routing and Taskfile target (review these extra carefully as the only production-adjacent touches).
- [ ] Independent review via the configured `glm-reviewer` with the worktree, immutable base `origin/main`, issue scope, and the incremental review-chain protocol from the work-issue skill. Triage and disposition every finding; fix validated in-scope defects and re-verify; contiguous review chain through final HEAD.
- [ ] PR: scope, `Closes #112`, verification commands/results, review rounds + dispositions, artifacts. Squash-merge per repo policy after checks.

---

## Self-review notes

- Spec coverage: every issue deliverable maps to a task — attribution (T1), plans (T2), fixtures (T3), candidates 1/2/3 (T5/6/7), correctness/workload coverage (T4 equivalence + T8 workload), Coder-observation handling (T8 report field + T10 note, adapted per owner instruction to local machine), gates (T8), report/artifact/note (T8/T10), production isolation (T8), fail-closed contract (T8 runner rules + per-candidate invalid marking).
- Out of scope honored: no production migration, no engine replacement, embeddings untouched beyond reading presence in attribution, no VACUUM-as-savings without equal compaction of control (both sides compacted).
- Known risk flagged in-plan: integer-keys' key-stability dependency on dirty propagation is exactly what the incremental equivalence scenarios exist to prove.
