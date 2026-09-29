# Capability-safe reduced-detail index profiles

Status: benchmark/design spike, schema `grafo.detail-profile-matrix/v1`. This
document does not add a production setting and does not change full fidelity as
the permanent default.

## Decision

The production decision is mechanical, not subjective. A candidate may proceed
only when same-machine samples show all of the following:

- at least 30% median compacted-primary-size reduction;
- no more than 20% median cold-time, incremental-time, or peak-RSS regression;
- exact fingerprints for every capability the candidate claims; and
- deterministic closure, resolution, cancellation/retry, and profile-switch
  convergence.

If any condition fails, Grafo retains full-only indexing. The versioned raw
report under `docs/benchmarks/issue-113/` owns the measured recommendation;
this note describes the contract used to reach it.

## Prototype boundary

`internal/detailprofile` is benchmark/test-only. A dependency test proves that
the production CLI, MCP server, supervisor, and service do not import it. The
only shared seam is `indexer.Options.ResultTransform`, a nil-by-default hook
whose semantic key participates in file hashes. It runs after a parser returns
and before the first durable mutation for that file. No production composition
sets it.

This is the correct spike boundary because the candidate rules need the
parser's typed nodes, exact/named locators, properties, and producer identity.
A SQL delete pass would be too late: reconciliation may already have converted
omitted declarations into external nodes or changed ambiguity. Parser-level
emission controls remain a possible optimization for a future implementation,
but the `ParseResult` prototype first proves semantic closure.

## Closed capabilities

The matrix embedded in every benchmark report classifies every current
`graph.NodeKind`, every current `graph.EdgeKind`, relevant property/form groups,
and every production parser/indexer producer. Tests compare it with
`graph.NodeKinds()`, `graph.EdgeKinds()`, and the production parser registry, so
a vocabulary or producer addition fails until reviewed.

| Capability | Operations requiring it |
| --- | --- |
| `structural_traversal` | find, show, neighbors, callers, callees, path |
| `source_lookup` | source, indexed search |
| `impact_dataflow` | impact / blast radius |
| `failure_flow` | failure-flow |
| `message_field_flow` + `transport_flow` | message-flow, message-coverage |
| `transport_flow` | send/receive/carry and codec transport evidence |
| `test_coverage` | find-tests, test-coverage |
| `godot_composition` | Godot composition |
| `godot_interactions` | Godot interactions |
| `catalogs_topology` | data/config/event/endpoint catalogs, handler lookup, outbound requests, service topology |
| `semantic_reuse` | semantic reusable-code candidates |

CLI and MCP names map to the same operation record. Options that only bound or
filter an operation do not weaken its minimum capability. Status may report an
index with any valid capability set and indexing itself requires none of these
query capabilities.

An unsupported operation must stop before selector resolution, enumeration, or
traversal with a structured error equivalent to:

```json
{
  "operation": "impact",
  "missing_capabilities": ["impact_dataflow"],
  "rebuild_profile": "full",
  "message": "index lacks capability impact_dataflow for impact; rebuild with profile full"
}
```

There is no partial impact result. Partial output is allowed only for an
existing query whose output model can identify the exact missing evidence
category without presenting it as absence. No current reduced candidate relies
on that exception.

## Profile semantics

### `full`

`full` returns each `ParseResult` unchanged. It claims every capability and
remains the default. Its graph semantics, ordering, stable IDs, diagnostics,
and producer identity are identical to indexing without the spike hook.

### `structural-v1`

The structural/topology candidate retains:

- repository, component, file, package/module, callable, type, field, test,
  data/config, endpoint/event, documentation, Godot, and transport-operation
  declarations;
- imports/exports, calls, direct tests, type relationships, config/catalog and
  endpoint/event relations, middleware/handlers, requests/dependencies,
  documentation, generated bindings, transport/codec facts, Godot composition,
  and Godot interactions;
- SQL/data-resource `reads` and `writes`, identified by the target's semantic
  kind rather than by relation name alone; and
- any otherwise-local declaration that a retained exact or named fact needs to
  preserve endpoint closure and candidate ambiguity.

It omits local variable/parameter declarations when no retained fact needs
them, `assigns`, `passes`, `returns`, failure relations, and non-data-resource
`reads`/`writes`. Consequently it does **not** claim `impact_dataflow`,
`failure_flow`, or `message_field_flow`. Relation kind alone is deliberately
insufficient: `reads`/`writes` against SQL resources are catalog evidence,
while the same kinds against protocol fields are message-flow evidence.

Transport is claimed because its retained proof is the callable-to-operation
`sends`/`receives` fact, operation-to-message `carries` fact, and codec
`encodes`/`decodes` fact. The candidate is invalid if closure forces removal of
any of those facts or if their full-profile fingerprint changes.

### `scoped-full-v1`

This candidate first applies #108 `index.include`/`index.exclude` membership.
Excluded paths produce no input and their savings are counted only as scope
savings. Among included paths, explicitly configured application roots receive
`full`; other included paths receive `structural-v1`. A path cannot be brought
back by a detail root after #108 excluded it.

The spike accepts full-detail roots as a benchmark argument rather than adding
configuration. If a candidate wins, configuration belongs under the existing
`index` owner; there must not be a second config parser or scope grammar:

```yaml
index:
  include: ["apps/**"]       # existing #108 membership
  exclude: ["apps/gen/**"]  # existing #108 membership
  detail:                    # proposed only after a winning result
    profile: structural-v1
    full_include: ["apps/api/**", "apps/server/**"]
```

The effective capability set for a scoped index is the intersection needed to
answer across all relevant included files. In practice, repository-wide impact,
failure, and message-field queries remain unsupported when any relevant region
is structural. A future narrowly path-scoped operation could claim more only if
its existing uncertainty model proves that every traversable member is full.

## Closure and fail-closed validation

Projection is deterministic and preserves input order. The prototype rejects:

1. a retained fact whose exact source or target ID is absent from the aggregate
   projected corpus;
2. any retained named locator whose exact local candidate set differs from the
   full parse result;
3. a candidate that creates a new external/unresolved node through omitted
   declarations;
4. a claimed capability whose sorted semantic fingerprint differs from full;
5. changing outputs across identical fresh samples; or
6. an unclassified vocabulary, property/form, producer, or operation.

Exact closure is validated after reconciliation as well as in deterministic
fixtures. External identities are fingerprinted with the capability of the
edge that produced them, not globally by `external` kind; otherwise omitted
dataflow externals could incorrectly invalidate a structural-only claim.

Cancellation before a transformed file is persisted leaves the preceding
valid graph intact. A cancellation after durable file boundaries may leave a
recoverable incremental index, exactly like current indexing; retry must
converge to a clean build. A production profile switch needs the stronger
atomic publication design below.

## Compatibility, metadata, and convergence design

If measurement justifies production work, add these values to semantic index
compatibility and status:

- `detail_profile_schema = 1`;
- `detail_profile = full|structural-v1|scoped-full-v1`;
- normalized profile options and #108 scope digest;
- sorted capability vocabulary plus a digest of the reviewed matrix version;
- per-federation-member profile/capabilities.

The metadata is a pure function of validated configuration and implementation
version. It is written only with the graph it describes. Any change forces
reparse/reconciliation; hashes must include the profile semantic key. Switching
either direction must build into a branch-local staging database and publish it
only after closure, reconciliation, query equivalence, and metadata checks pass.
Invalid config, cancellation, or migration failure leaves the last published
index untouched. Branch indexes keep independent metadata, so switching a Git
branch never borrows another branch's capability claim.

Federation computes capability availability per operation across the members
actually needed. It must not advertise the union. If `api` is full and `web` is
structural, message-flow returns an attributable unsupported state naming
`web`; missing evidence is never converted into an empty federated result.

## Measurement contract

`task bench:detail-profiles` runs the production parser registry, indexer, and
SQLite adapter against one clean pinned Git worktree. Each raw sample records:

- corpus and Grafo commits, branches, remotes, and dirty states;
- cgroup CPU quota and memory maximum, output filesystem/path, and RSS method;
- #108 scope digest/include/exclude and `scoped_out` count;
- discovery, membership, read/hash, parse/projection, persistence,
  reconciliation, and total timings from the indexer;
- nodes/facts/edges by kind and parse-result facts by producer;
- compacted primary and final WAL bytes;
- cold and zero-change incremental times and sampled peak RSS; and
- semantic fingerprints for every closed capability.

Compaction runs `wal_checkpoint(TRUNCATE)`, `VACUUM`, and a final checkpoint.
Only the primary bytes after that sequence count toward the 30% gate. Scope
savings remain separate. Candidate comparisons require the same corpus commit,
scope digest, CPU quota, and memory cap. The approximately 31-minute owner
observation is context only; the report marks corroboration when the
provenance-complete full median falls within 25–37 minutes and never imports
the older M3 #47 timing as a sample.

Peak RSS is sampled every 50 ms from `/proc/self/status`. Reports explicitly
record that it is an absolute process measure and that Go may retain runtime
memory between samples. For a release decision, run each profile in a fresh
process and use at least three samples; a single sample remains raw exploratory
evidence, not a statistically strong gate.

## Future implementation boundary

Only after a candidate clears every gate:

1. move the reviewed capability/matrix vocabulary into a production-neutral
   owner under `internal/graph`;
2. extend the existing `projectconfig.Config.Index` parser and semantic key;
3. add parser emission/projection rules with aggregate closure validation;
4. persist/version capability metadata and implement staged atomic rebuilds;
5. add one preflight service shared by query, CLI, MCP, source, semantic, and
   federation adapters;
6. expose profile/capabilities in status and federation member diagnostics;
7. run fresh/incremental edit-delete-restore, cancellation/retry, branch-switch,
   mixed-language, mixed-profile federation, and every query equivalence suite;
8. retain `full` as the default with an explicit rebuild command for any
   unsupported operation.

No production issue should be filed from cardinality alone. If the measured
candidate misses size, semantic, cold, incremental, or RSS gates, the decision
is full-only and the evidence that prevented removal remains documented in the
report.
