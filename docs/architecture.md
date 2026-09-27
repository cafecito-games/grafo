# Architecture

Grafo is split into domain, use-case, and adapter layers so language support and
storage can evolve independently.

```text
  cmd/grafo
  └─ internal/cli          composition and presentation
       ├─ internal/indexer changed-file indexing use case
       ├─ internal/query   deterministic traversal use case
       ├─ internal/semantic optional candidate-ranking use case
       ├─ internal/source   bounded graph-addressed source retrieval
       ├─ internal/embedding/ollama provider adapter
       ├─ internal/mcpserver agent-facing stdio tools
       └─ internal/federation read-only multi-index graph
            │
      internal/graph       nodes, edges, facts, narrow repository ports
            ▲
            │
  internal/parser/*        language AST adapters, manifests, SQL router, config mappers
       ├─ defaults/*       production registry shared by CLI and evaluation
       ├─ gdscript/*       gdparser GDScript AST adapter
       ├─ godot/*          gdparser scene, resource, config, UID, and shader adapters
       ├─ java/*           Tree-sitter Java AST adapter
       ├─ swift/*          Tree-sitter Swift AST adapter
       └─ sql/*            dialect adapters such as PostgreSQL
  internal/storage/sqlite  Goose + sqlc adapter
```

## Semantic pipeline

1. Discovery asks Git for tracked and non-ignored files, falling back to a
   filesystem walk outside Git repositories.
2. SHA-256 content hashes select changed files. Only those files are parsed.
   A semantic-index version forces a one-time rebuild when parser behavior or
   graph meaning changes, so an upgraded binary never serves an old schema as
   if it were current.
3. A parser emits declaration nodes and relationship facts without talking to
   the database. Most adapters use syntax evidence; the Go adapter augments it
   with compact `go/packages`/`go/types` object evidence when available.
4. The repository transactionally replaces the changed file's nodes and facts.
5. Reconciliation deterministically resolves only dirty facts against
   declarations and materializes adjacency-indexed edges. When evidence is
   insufficient, the target remains an explicit `external` node rather than a
   guessed edge.
6. Queries walk ordered incoming or outgoing adjacency lists. Node IDs, edge
   IDs, ordering, and breadth-first tie-breaking are stable.

Dirty-target tracking limits edge reconciliation to facts affected by changed
declarations or owners. Unchanged files normally do not re-enter a parser. A Go
source, module, workspace, vendoring, or build-context change invalidates Go
semantic views so dependent method sets and call targets converge with a clean
rebuild.

Affected facts are first materialized in a durable SQLite queue, then resolved
in bounded transactions. Each committed batch checkpoints the WAL, and the
queue entry is removed in the same transaction as its replacement edges. An
interrupted index therefore resumes pending facts without holding one
repository-sized transaction or retaining a repository-sized fact slice in
memory. The semantic schema version is part of every file hash, so a restarted
schema rebuild also skips files already converted to that version. Indexes on
`edges.fact_id` and dirty lookup keys keep replacement work proportional to the
affected graph.

Symbolic resolution creates a declaration edge only when exactly one candidate
matches the parser's target and edge-kind constraints. Ambiguous names remain
explicit external nodes until a parser can supply a qualified target; Grafo
does not turn uncertainty into speculative fan-out.

For Git worktrees, the indexer narrows content hashing to files changed since
the indexed commit, current untracked files, and paths that were dirty during
the previous run. Remembering the previous dirty set closes the restore case:
if a dirty file was indexed and then restored to `HEAD`, it is still checked
once before leaving that set. Parser-level semantic dependencies propagate
configuration changes (for example `grafo.yaml`) to otherwise unchanged source
files. Any Git detection failure safely falls back to hashing every supported
file; non-Git directories always use that fallback.

The Go semantic loader runs with module downloads and toolchain switching
disabled. One bounded workspace load converts `types.Info` calls, selections,
instances, and method sets into per-file evidence, then releases the toolchain
syntax/type graphs. Its cache key includes source and module/workspace digests
plus GOOS, GOARCH, CGO, tags/flags, workspace selection, and toolchain version.
Type errors remain diagnostics while proven facts augment AST output; excluded
build-tag files record the active context without emitting declarations.
`implements` comparisons are bounded to interfaces declared in loaded workspace
packages; dependency and standard-library interfaces remain external facts.

## Federation

Every declaration ID includes its repository identity. A federation opens the
current branch index for each requested worktree and presents them through the
same read-only repository port used by normal traversal. When an explicit
external fact exactly matches a compatible declaration in a peer index, the
adapter synthesizes a deterministic edge marked `federated=true`. It neither
copies databases nor guesses from similarity. Both one-shot CLI queries and
long-running MCP tool calls incrementally refresh their indexes first.

## Semantic discovery

`semantic.Embedder` isolates vector generation from the candidate-selection use
case, and `semantic.Repository` isolates vector persistence. SQLite stores one
normalized vector per node and model. A SHA-256 hash of the stable semantic
document means unchanged candidates are never embedded again; deleted nodes are
pruned on the next sync.

Cosine similarity may rank candidates for `find_reusable_code`, but it never
creates an edge or determines a path. Each selected node is resolved by stable
ID and returned with a one-hop traversal from the graph repository. Similarity
search can later move to a specialized vector index without changing graph
parsers, traversal, or the MCP contract.

## Change impact

`internal/query` owns the bidirectional impact use case. Upstream is an incoming
traversal and downstream an outgoing one over the same relation set, so one
definition of "what participates in impact" serves the CLI and the MCP server
instead of each keeping its own list. The report separates the two directions,
each with its own depth, node limit, and truncation flag, and adds impacted
files, cross-repository hops, and configuration, data, and event relations
derived from the edges already traversed. No new node or edge vocabulary is
introduced, and an unresolved or external target is never reported as a
confirmed dependency.

Impacted files are keyed by repository identity plus canonical relative path and
retain every node and edge that caused inclusion, so a diagram or summary is
never the only evidence. Optional source excerpts arrive through a narrow
`SourceReader` port; `internal/source` adapts to it, which keeps the dependency
pointing from source retrieval to query and not back.

## Godot composition

`internal/graph` owns the Godot composition vocabulary: `godot_scene`,
`godot_resource`, `godot_scene_node`, and `godot_autoload` nodes joined by
`instantiates`, `attaches_script`, and `autoloads` edges. Promoting these from
generic modules and variables is a semantic schema change, so the graph schema
and semantic index versions move together and an older index rebuilds instead of
serving both vocabularies at once.

`internal/parser/godot/godotid` is the single Godot resource identity resolver.
Canonical identity is the repository-relative path without its extension, shared
by the text-resource, UID sidecar, configuration, and GDScript producers, so one
resource has one qualified name regardless of whether a `res://` path, a
repository path, or a `uid://` alias named it. UIDs are aliases: the resource
that owns a UID declares it, references carry it as evidence, and a UID that
maps to more than one resource produces a diagnostic and stays unresolved. The
same package owns the `[autoload]` vocabulary of the nearest `project.godot`,
which is why a GDScript use of an autoload name resolves only against an exact,
unshadowed, singly declared autoload.

Scene inheritance is an `instantiates` edge from both the inheriting scene and
its root node, never language `extends`, so scene composition is never confused
with class inheritance. `internal/query` assembles inbound and outbound
instances, script attachments, and autoload availability from those edges only;
it introduces no vocabulary of its own and reports an unresolved target as an
external node rather than omitting it.

## Source retrieval

Source retrieval begins with normal deterministic node resolution; it never
searches file text. The node's repository identity and indexed location select
one active worktree and one bounded line span. Canonical path validation rejects
absolute paths, traversal, and symlinks that resolve outside the repository.
`source.SafePath` is the single implementation of that validation and is shared
by every bounded reader.

## Source search

`internal/search` answers content questions the graph does not model. Index
membership, not the filesystem, decides what is searchable: the service reads a
`graph.FileCatalog` per repository and opens only those paths in the matching
worktree, through `source.SafePath`. Files are streamed one at a time and
abandoned as soon as a bound is reached, so no repository corpus is held in
memory and source text is never persisted. Every cap that clips a result is
named in the response rather than applied silently, and ordering is by
repository, path, line, and column so results never depend on directory
enumeration order.

## Agent installation

`internal/agentinstall` models each MCP client as an adapter declaring identity,
detection, scope, and either an official CLI or a documented user-level config
file. Detection and mutation are separated at the type level: detection and
planning take a read-only `Reader`, and only a real run reaches `Writer`, which
makes `--list` and `--dry-run` unable to write by construction. Platform config
paths resolve through the injected environment rather than `runtime.GOOS`, so
macOS, Linux, and Windows layouts are all testable anywhere. File-backed
adapters parse structurally, preserve unknown keys and ordering, and replace
files atomically; uninstall removes only a registration Grafo owns.

Agent guidance is a second artifact kind on the same seam. `internal/agentguide`
owns one embedded playbook, one format version, and the exact begin/end markers
that make an installed copy provably Grafo-owned; it renders either an isolated
skill file or a delimited managed block and never touches bytes outside its
markers. `internal/agentinstall` declares, per client, which documented
user-scoped surfaces exist, refuses targets that are symlinks, non-regular,
world-writable, or outside the user configuration roots, and treats a conflicting
or unowned file as a reported conflict rather than something to repair. Advisory
hooks are opt-in, capability-gated to a client whose hook API is documented, and
fail open because the hook command always exits 0. Every mutation is followed by
a receipt under the Grafo configuration directory recording target, digest,
guidance version, and marker, so uninstall and upgrade prove ownership from
receipts plus exact markers instead of substring matching.

## Background service and diagnostics

`internal/service` orchestrates indexing across repositories; it never implements
a second indexer. Three authorities are consumed rather than duplicated: a
user-level registry (`$XDG_CONFIG_HOME/grafo/watched-roots.json`, written
atomically at mode 0600 under an exclusive lock) owns which roots are watched,
`indexer.DiscoverProject` owns branch and index identity, and
`internal/indexer` owns indexing semantics. A malformed or unreadable registry
blocks every mutation and reports how to recover.

The concurrency model has three layers. Exactly one supervisor process per user
is admitted by an exclusive kernel-backed lock on `service.lock` in the state
directory. Inside it, at most one goroutine works on a root at a time and at most
`--concurrency` roots index at once. Every indexing run - background or
foreground `grafo index`/`grafo watch` - holds the per-index lock beside the
branch database, so the two can never write one index concurrently. A killed
supervisor therefore needs no cleanup: the kernel drops its locks, SQLite rolls
back the uncommitted transaction, and the next pass rediscovers and reconciles
from the committed index, which makes restarts idempotent.

Change hints are debounced and advisory; periodic reconciliation runs regardless,
so a lossy or missing platform watcher only affects latency. A watcher overflow
hint schedules one bounded full reconciliation of that root. A root that is
temporarily unreadable is paused for the pass with its registration and indexes
preserved; a definitively deleted root is reported and never pruned implicitly. If
branch identity cannot be proven, the root is not indexed at all, so one branch is
never written into another branch's database. One failing root never stops
another: failures are recorded per root and exposed in the status file. The
bounded, rotating log records paths, counts, timings, and errors, with every field
collapsed to a single line; there is no API that accepts file content, so source
text cannot reach it.

Platform adapters own the generated launchd plist and systemd user unit, each
carrying an exact version marker and pointing at the absolute installed binary
and a stable `--state-dir`. Installation is idempotent; a definition that differs
from the generated content is replaced only when a receipt in the shared
`agentinstall` ledger proves Grafo wrote the bytes on disk, and is otherwise
reported as a conflict. Ownership and path containment reuse `agentinstall`'s
receipt ledger and user-configuration-root check rather than adding a second
mechanism.

Four rules keep that boundary honest:

- Ownership is bound to one **resolved path**, not to the artifact kind
  (`agentinstall.OwnedFileAt`). A receipt written for one definition location can
  never authorize creating, replacing, or activating a definition somewhere else.
  This is the contract PR #53 established for hook receipts.
- Ownership is decided **before any action**. Stopping or disabling a service is
  itself a mutation, so a unit at Grafo's path that Grafo cannot prove it wrote is
  never stopped, disabled, or removed - only reported.
- Generated content is **escaped for its own syntax**. `ExecStart` is a systemd
  command line, so each path is quoted with systemd's escapes and `%` is doubled
  so no specifier expands; a path carrying a control character cannot be
  represented and is refused with a diagnostic rather than emitted broken. plist
  arguments are separate XML elements and are XML-escaped.
- A definition may only name a **durable executable**. `InstallableBinary` refuses
  a path inside the temporary directory (`$TMPDIR`/`$GOTMPDIR`), inside the Go
  build cache (`$GOCACHE` or the per-user cache directory's `go-build` subtree),
  or inside a `go-build<digits>` build directory, because a unit that outlives the
  command must not point into a directory the toolchain deletes. The first two are
  decided by containment in directories the environment reports. The third is the
  only name rule, applies when those signals are absent, and matches just the
  shape the toolchain creates: refusing a durable prefix such as
  `/opt/go-builder/bin` would be as much a defect as accepting an ephemeral path,
  so `go-builder`, `go-build-tools` and a plain `go-build` directory outside every
  cache root all remain installable.

Whether an installed definition still starts the running binary is decided by
parsing the platform's documented command field and comparing whole resolved
paths, never by searching the file for the binary path: a substring test reports
`/opt/grafo-next/grafo` as correct while `/opt/grafo` is running.

`grafo doctor` reads the binary, registry, per-root branch and index state, the
platform service, the supervisor status file, and the agent registrations from the
same client registry the installer uses; without `--repair` it mutates nothing.
`--repair` performs only four enumerated repairs: unregister a definitively
missing root that overlaps no other registration, refresh Grafo-owned agent
artifacts that a receipt proves Grafo installed, recreate a service definition
Grafo installed, and restart a stale service. Everything else is reported with an
actionable manual step.

## Catalogs and orphan detection

`internal/graph` owns the resource kind and relationship vocabulary; a single
catalog use case in `internal/query` owns usage direction and orphan
classification. Storage adapters implement one narrow listing port,
`graph.NodeListRepository`, which enumerates nodes by exact kind and attributes
each one to an indexed repository. Adapters never interpret semantics, so a new
catalog needs no new SQL and a new resource kind needs no presentation change.

Node visibility is explicit rather than implicit: an enumeration asks for local
declarations, unresolved external targets, or both, so a catalog never silently
mixes a declaration with an unresolved reference. Each catalog reports the two
classes in separate sections.

Orphan status is a query result, never a persisted edge or diagnostic. An event
with both a producer and a consumer is not reported. A one-sided event is
classified as published-without-consumer, consumed-without-producer, or
declared-with-neither, and its status is downgraded from orphaned to unknown
whenever an unresolved target could be the missing counterpart or a bound cut
off part of the evidence. Each relation is bounded independently, because a
budget shared in edge order would let one relation starve another into looking
empty and turn a bound into a false absence claim. Because a
federated edge keeps the fact identity of the unresolved edge it replaced,
resolved cross-repository evidence is never double-counted as uncertainty. An
event name that no declaration resolves is always unknown: the index does not
bound where such a name is published or consumed.

Configuration output is value-free by construction. Only properties on an
explicit non-secret metadata list are returned, and the names of withheld
properties are reported so a future parser that records a value cannot leak it
through a catalog.

## Persistence

Each branch has a separate SQLite file under `.grafo/indexes`. The database is
in WAL mode. Goose owns schema history under
`internal/storage/sqlite/migrations`; sqlc owns the generated database access
code under `internal/storage/sqlite/sqlcgen`. Handwritten code only adapts
between generated rows and domain models.

Run `task generate` after changing SQL. Generated code is committed so building
Grafo does not require sqlc.

## Extension seams

- Add a language by implementing `parser.Parser` and registering it in the
  shared production registry under `internal/parser/defaults`.
- Add a SQL dialect by implementing `sql.Dialect` under `internal/parser/sql`
  and passing it to the single SQL router in the CLI composition root. A
  dialect declares only its unambiguous extensions; `.sql` always belongs to
  the router. Dialects emit the shared table/view/column/index graph vocabulary,
  while the router records the selected dialect in node metadata.
- Add storage by implementing the small `IndexRepository`, `QueryRepository`,
  `StatusRepository`, `FileCatalog`, and `NodeListRepository` ports.
- Add a catalog by reusing `NodeListRepository` with another node kind; extend
  `graph.DataResourceKinds` to widen the data catalog.
- Add an MCP client by adding one adapter to the `internal/agentinstall`
  registry. Declare a client only when both install and uninstall use a
  documented surface covered by fixtures. Add a guidance surface for it only when
  the client documents a user-scoped instruction or hook API that install and
  uninstall can both drive safely.
- Add new relationships as facts first; keep graph-candidate reconciliation in
  the repository. A parser may provide an exact qualified target when its
  language's authoritative semantic model proves object identity.
- Dependency manifests emit shared module nodes and `depends_on` facts, keeping
  ecosystem-specific versions and scopes in edge properties. Federation
  resolves exact module declarations without copying manifest contents.
- Replace the current portable vector scan with a specialized vector index at
  very large candidate counts; keep the `semantic.Repository` port stable.

## SQL routing

SQL selection follows one fixed precedence: a `grafo.yaml` path mapping, a
dialect-specific extension, `sql.default_dialect`, then syntax probes sorted by
dialect name. Zero successful probes produce a diagnostic; multiple successful
probes require explicit configuration. Path mappings are ranked by specificity,
not declaration order, so neither router registration nor YAML map order changes
the result.

`grafo.yaml` is repository-level application configuration, separate from the
generic YAML source parser. The SQL router's semantic cache key includes the
configuration file, so changing a path mapping or default reparses unchanged
SQL sources that may now select another dialect. Parser-contract changes also
bump the semantic-index version.
