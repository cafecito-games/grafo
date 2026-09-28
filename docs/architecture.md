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
       │   └─ godotid/*    Godot resource identity, project scope, and UID aliases
       ├─ java/*           Tree-sitter Java AST adapter
       ├─ protobuf/*       protocompile Protobuf AST adapter
       ├─ protobufbinding/* deterministic Buf-generated API projections
       ├─ swift/*          Tree-sitter Swift AST adapter
       └─ sql/*            dialect adapters such as PostgreSQL
  internal/projectconfig   shared grafo.yaml loader and owned-section validation
  internal/storage/sqlite  Goose + sqlc adapter
```

## Semantic pipeline

1. Discovery asks Git for tracked and non-ignored files, falling back to a
   filesystem walk outside Git repositories.
2. SHA-256 content hashes select changed files. Only those files are parsed.
   A semantic-index version forces a one-time rebuild when parser behavior or
   graph meaning changes, so an upgraded binary never serves an old schema as
   if it were current.
   Before the first durable write, the shared project-configuration loader
   validates Grafo-owned `grafo.yaml` sections and discovery identifies the
   eligible file set. Invalid component ownership therefore preserves the last
   valid index.
3. A parser emits declaration nodes and relationship facts without talking to
   the database. Most adapters use syntax evidence; the Go adapter augments it
   with compact `go/packages`/`go/types` object evidence when available.
   Every fact carries an explicit extraction `producer`. The parser builder
   stamps direct consumers, and the indexer overwrites that value with the
   selected parser's `Language()` before persistence so parser output cannot
   forge another producer. Indexer-owned containment/component facts use the
   stable `indexer` identity.
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

Facts identify each source either by an exact `from_id` or by a symbolic
`source` plus an optional `source_kind`; conflicting or missing source locators
are rejected before an owner is replaced. Both endpoints are resolved before an
edge is written. An absent exact source becomes an explicit external node rather
than a dangling edge, and symbolic resolution creates a declaration edge only
when exactly one candidate matches. Target resolution additionally applies the
edge-kind compatibility rules; kindless source resolution requires uniqueness
across every node kind. Ambiguous names remain explicit external nodes rather
than becoming speculative fan-out.

Node-name substring search uses shadow values written with Go's Unicode-aware
lowercase rule. Both the stored `name` and `qualified_name` values and every
search fragment pass through that same rule, so catalog filters and symbol
search agree for ASCII and non-ASCII names without locale-sensitive or
accent-insensitive collation. Migration 00005 preserves the old ASCII behavior
for existing rows; semantic-index version 18 required a one-time reindex to
populate Unicode-correct shadow values for every node. Migration 00006 adds
named fact sources without changing legacy exact-source rows, and semantic-index
version 19 reparses producers so they can emit the expanded contract.
Migration 00007 adds non-null producer columns to facts and edges with an empty
legacy default. Graph schema version 10 and semantic-index version 28 force a
complete reparse, replacing those legacy unknown values before current query
results are served. Reconciliation copies fact provenance to every replacement
edge without making it part of fact or edge identity.

Graph schema version 11 and semantic-index version 29 add first-class Go and
Godot test declarations. Reconciliation derives a `tests` edge only when a
local test's exact `calls` or `references` evidence resolves uniquely to a
local production declaration. Query-time helper expansion follows only nodes
explicitly marked as test helpers or lifecycle hooks, is bounded by depth and
work limits, and reports cycles or exhausted bounds as truncated. These reports
are structural evidence and never claim runtime execution coverage.

For Git worktrees, the indexer narrows content hashing to files changed since
the indexed commit, current untracked files, and paths that were dirty during
the previous run. Remembering the previous dirty set closes the restore case:
if a dirty file was indexed and then restored to `HEAD`, it is still checked
once before leaving that set. Parser-level semantic dependencies propagate
configuration changes (for example `grafo.yaml`) to otherwise unchanged source
files. Any Git detection failure safely falls back to hashing every supported
file; non-Git directories always use that fallback.

Workspace evidence is replaced independently on every run. It contains the
repository node plus explicit component nodes and `contains` facts from the
repository to each component and from each component to its eligible file
nodes. Component IDs derive from repository identity and exact component name;
fact IDs derive from their endpoints, so YAML declaration order cannot change
identity. Node locations point to component names, and membership facts point
to the matching root declaration. Removing or renaming configuration therefore
removes stale ownership without making unchanged source files enter a parser.
Unmatched files retain their existing repository `contains` evidence.

The Go semantic loader runs with module downloads and toolchain switching
disabled. One bounded workspace load converts `types.Info` calls, selections,
instances, and method sets into per-file evidence, then releases the toolchain
syntax/type graphs. Its cache key includes source and module/workspace digests
plus GOOS, GOARCH, CGO, tags/flags, workspace selection, and toolchain version.
Type errors remain diagnostics while proven facts augment AST output; excluded
build-tag files record the active context without emitting declarations.
`implements` comparisons are bounded to interfaces declared in loaded workspace
packages; dependency and standard-library interfaces remain external facts.

## Protobuf binding projections

Canonical `.proto` declarations own schema identity. The shared
`protobufbinding` registry reads those declarations plus repository-owned Buf
v2 module and generation configuration, then applies versioned naming adapters
for `protoc-gen-go` v1 and gdproto v0.6. It emits language-facing message,
enum, field, accessor, and oneof-wrapper nodes with a `generated_from` edge to
the exact canonical node ID. Go and GDScript parsers consume that one registry;
they do not carry their own generator naming rules.

The registry never runs a generator, downloads a plugin, or reads ignored
output as an authority. This allows configured GDScript bindings to exist in
the graph even when their output directory is ignored. A tracked generated file
is reduced to file/provenance evidence only when its standard header, source
annotation, configured output path, and supported generator all agree; the
schema/config projections retain useful API names while runtime helpers and
descriptor initialization stay out of normal catalogs. Other generated Go code
keeps the ordinary Go indexing path.

Schema, Buf configuration, adapter version, and corroborating generated-file
content participate in incremental hashes. Missing or malformed configuration,
unsupported plugins or versions, ambiguous schema names, and ambiguous header
source basenames fail closed with diagnostics or unresolved evidence instead of
inventing a projection.

Go application usage follows the same authority boundary. The semantic loader
reduces exact `go/types` identities for `proto.Marshal`, `proto.Unmarshal`,
generated getters and fields, keyed literals, oneof wrappers, and type-switch
cases into a compact per-file view. The Go parser emits `encodes` and `decodes`
to canonical messages and `reads` or `writes` to canonical fields only after a
unique registry projection confirms the generated binding. Each fact retains
its source location, static type, API, operation form, and binding-node identity.
Ill-typed packages, ordinary same-name APIs, unkeyed field positions, default
switch cases, and missing or ambiguous projections produce no guessed protocol
relationship. Generated implementations remain type-checking input but are not
application producers or consumers.

## Failure flow

`internal/graph` owns the versioned failure vocabulary: `returns_error` for a
callable's typed result contract, `propagates_error` and `wraps_error` for
actual exits, `handles_error` for consuming conditional actions, and `panics`,
`recovers`, and `defers` for abrupt exits and cleanup. Recognition details such
as return, join, comparison, switch, and recovery are edge properties, so the
vocabulary remains language-neutral while Go is its first producer.

The Go semantic loader reduces `go/types` objects and result tuples into compact
per-file failure evidence before releasing the package graph. One extractor owns
signature result positions, sentinel and named error identity, call origins,
standard-library wrapping/checking forms, and conservative handler recognition.
Syntax fallback emits only structural defer statements and unshadowed builtins;
unknown identities and dynamic panic payloads remain source-scoped external
boundaries. Every fact keeps location, recognition form, type/object evidence,
and conditional or unresolved state.

`internal/query.FailureFlow` groups those stored facts without reading source:
typed error declarations, escaping failures, handlers, panics, recoveries, and
deferred cleanup remain separate. It reads both edge directions, so selecting a
callable explains its behavior while selecting an error identity or callee shows
which functions return, propagate, or handle it. The CLI `failure-flow` command
and MCP `get_failure_flow` tool are adapters over that same report.

## Federation

Every declaration ID includes its repository identity. A federation opens the
current branch index for each requested worktree and presents them through the
same read-only repository port used by normal traversal. When an explicit
external fact exactly matches a compatible declaration in a peer index, the
adapter synthesizes a deterministic edge marked `federated=true`. It neither
copies databases nor guesses from similarity. Both one-shot CLI queries and
long-running MCP tool calls prove source freshness first. MCP publishes one
atomic query-only generation after all changed members refresh successfully;
unchanged members perform no writable open or metadata write. A tool holds one
generation lease for its complete scalar or batched response, so a later
refresh cannot expose a partially updated federation or close a handle in use.

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

`internal/parser/godot/godotid` is the single Godot resource identity resolver,
shared by the text-resource, UID sidecar, configuration, shader, and GDScript
producers so one resource has one qualified name whatever evidence named it.
Canonical identity is the repository-relative path without its extension, but a
`res://` reference is *project*-relative: it resolves against the directory of
the nearest ancestor `project.godot`, so a Godot project in a monorepo
subdirectory targets identities under that subdirectory instead of identities no
file owns. Identity and reference resolution are therefore separate entry points
(`Identity` and `Resolve`); nothing canonicalizes a reference without knowing
which project owns it. The project directory is also a boundary, not just an
origin: a reference that traverses out of its own project resolves to nothing and
is diagnosed, because `res://` names a location inside one project by definition
and in a monorepo whatever it lands on usually belongs to a different project.

UIDs are aliases, never identity: the resource that owns a UID declares it and
references carry it as evidence. Because a path that contradicts its UID would
otherwise produce a confident edge to the wrong resource, the package also owns a
repository-wide alias table of UID declarations, built from text-resource
headers, `.uid` sidecars, and `.import` sidecars with bounded header reads. A
reference whose UID is declared by a different resource, or by several, produces
a diagnostic and no resolved edge; a UID that is not declared anywhere cannot
contradict anything, so the path stands and an absent target remains an explicit
external node. The check runs in that direction only, because canonical identity
drops the extension: a scene and its script share one identity while declaring
two UIDs of their own, so "this path declares some other UID" is not evidence of
disagreement. That table plus the set of `project.godot` locations is the Godot
parser's workspace semantic key, so a UID or project move reparses the files
whose resolution it changes even when their own content is untouched.

Autoload identity is scoped to the declaring `project.godot`
(`godot:autoload:<project.godot path>:<Name>`), which keeps two projects in one
repository that both declare `Game` independently addressable. A GDScript use of
an autoload name resolves only against an exact, unshadowed, singly declared
autoload that Godot actually exposes as a global singleton: a declaration
without the leading `*` marker keeps its node and its `autoloads` edge, because
the declaration is real, but satisfies no global identifier reference.

Scene inheritance is an `instantiates` edge from both the inheriting scene and
its root node, never language `extends`, so scene composition is never confused
with class inheritance. `internal/query` assembles inbound and outbound
instances, script attachments, and autoload availability from those edges only;
it introduces no vocabulary of its own and reports an unresolved target as an
external node rather than omitting it.

## Godot gameplay interactions

Composition answers what a scene is built from; interactions answer how it is
wired at runtime. `internal/graph` owns that vocabulary too: `godot_input_action`
and `godot_node_group` nodes joined by `uses_input_action`, `in_group`, and
`uses_group` edges, alongside the `event` `publishes`, `subscribes`, and
`handled_by` edges signals already used. Signals stay events on purpose - a
Godot-only signal kind would split every generic event query in two.

The operation behind an edge is a `form` property rather than another edge kind,
because direction and meaning are shared inside each relation: `add` and `remove`
both name a membership between a node and a group, and `lookup`, `call`, and
`notify` all read a group's members. One consequence is deliberate:
`is_in_group` is a `uses_group` lookup with form `membership_test` and never an
`in_group` edge, because asking whether a node is in a group is not evidence
that it is.

Operation form is not parser provenance. `Fact.Producer` is the extraction
authority and `Edge.Producer` is its reconciled read model. Godot interaction
classification first requires producer `gdscript` or `godot` for every action,
group, signal, reference, and definition; shared signal relations additionally
require one of the known forms. Missing, unknown, or non-Godot producers are
excluded even when their endpoint kinds and `form` values look Godot-specific.
Federation retargets a copied edge and therefore preserves the original
producer unchanged.

Actions and groups are project-scoped exactly as autoloads are
(`godot:input_action:<project.godot path>:<name>`,
`godot:node_group:<project.godot path>:<name>`), and `godotid` owns those names
so the scene, script, and configuration producers converge on one node. Godot's
declaration sites are narrow and Grafo does not widen them: an action is declared
by an `[input]` entry and a group by a `[global_group]` entry, so a group that a
scene or a script merely uses stays an external node until the project declares
it. That is what keeps missing wiring visible instead of letting the first use
define the vocabulary. Only non-secret declaration metadata is stored - an
action's deadzone as written and how many events it lists - because device
bindings are out of scope.

Script-side resolution is literal-only. A recognized action or group API with a
literal name argument produces one edge; a computed name produces none, because
the name is not knowable and matching it by partial text would attach the use to
whichever declaration shared a substring. The API tables are exact method names,
not patterns, so a project's own `is_action_bar_visible()` is not read as an
action query, and a bare call that resolves to a method the script declares is
that method rather than an engine entry point sharing its name. A group dispatch
keeps its method name as evidence and never resolves a handler: which object's
method runs is decided by the group's runtime membership. `Input` and `InputMap`
action reads keep their generic `reads_config` fact to `input/<action>` as well,
so configuration catalogs see the same evidence they saw before actions became
their own kind - explicit compatibility rather than a name-prefix heuristic.

Signal routing is unified across producers. A scene `[connection]` and a script
`connect` both subscribe with form `connect`, and the scene-declared one is
marked `declared`. Only `connect` subscribes: `disconnect` and `is_connected`
are `references` carrying form `signal_disconnect` and `signal_connection_test`,
because treating either as a subscription would make a signal look consumed by a
script that removes or inspects its own wiring. A handler is named only when a
literal `connect` passes a bare identifier that resolves to a method the script
declares.

`internal/query.GodotInteractions` is a sibling of the composition report rather
than an extension of it, reads only those edges, filters by category and
direction, and counts the interactions whose far side is unresolved so a caller
can tell a wired report from one that merely looks wired.

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

Agent guidance uses additional artifact kinds on the same seam.
`internal/agentguide` owns the structural graph-usage playbook and the separate
repository-setup playbook, with independent format versions and ownership
markers so either skill can evolve without making the other stale. The graph
playbook renders either an isolated skill file or a delimited managed block; the
setup workflow is an isolated `grafo-setup` skill. Claude Code and Codex receive
both personal skills; Codex upgrades also remove the retired managed block from
`~/.codex/AGENTS.md` when that direct target is safe to mutate.
`internal/agentinstall` declares, per client, which documented user-scoped
surfaces exist, refuses targets that are symlinks, non-regular, world-writable,
or outside the user configuration roots, and treats a conflicting or unowned
file as a reported conflict rather than something to repair. Advisory hooks are
opt-in, capability-gated to a client whose hook API is documented, and fail open
because the hook command always exits 0. Every mutation is followed by a receipt
under the Grafo configuration directory recording target, digest, artifact
version, and marker, so uninstall and upgrade prove ownership from receipts plus
exact markers instead of substring matching.

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

## Endpoint and service topology

`internal/query.Topology` is the single owner of HTTP endpoint, handler,
outbound-request, and service-link interpretation. It consumes the same narrow
`graph.NodeListRepository` contract as the catalogs; storage and federation
enumerate exact endpoint/event nodes but do not infer service semantics.

Persisted component-to-file membership defines a service boundary when it can
be proven. The query builds ownership from `component contains file` edges and
attributes parser nodes through their owner-file evidence; it never reopens
`grafo.yaml` or infers ownership from a path prefix. Files without membership
retain repository identity as a fallback service, while conflicting component
owners fail the whole query without a partial topology. Component service IDs
derive from repository identity plus component node ID, and presentation labels
use `repository/component`. Structured service nodes and resources keep the
repository, component name, and component node ID separate. The participating
code resources stay nested under their service and retain qualified names,
kinds, languages, and source locations. Links carry the endpoint or event node
IDs plus every edge and fact ID used to construct them. Mermaid rendering is a
deterministic, escaped view of this structure rather than a second source of
truth.

Repository filters match every component and fallback service in that
repository. Component filters match that exact name across selected
repositories, combined filters require both, and direction is evaluated against
every service in the resulting scope. Unknown components return an empty result
and never select the repository fallback. Equal component names in federated
repositories remain distinct because their service IDs include repository and
component-node identity.

Outbound HTTP facts are grouped by fact identity so the unresolved edge and
any federated replacement cannot become duplicate calls. `internal/httpmodel`
is the single owner of method normalization, route parsing and joining,
canonical template identity, and directional compatibility. Queries and
fragments remain raw evidence but do not participate in endpoint identity;
parameter names are interchangeable, while regex constraints and catchalls are
preserved. Candidate resolution ranks exact literals, compatible templates,
then catchalls, and equal best candidates remain an ambiguous boundary with no
confirmed service link. An authority on an absolute request URL prevents a
path-only local match, while scheme plus authority distinguish its external
service identity. Asynchronous links pair publisher and subscriber
evidence for the same event without persisting a derived relationship.
Federation retains its edge marker, and cross-repository evidence also marks
template-compatible HTTP links as federated.

Go outbound HTTP extraction is resolved once in the compact package semantic
view using go/types API identity. It composes `net/http` constructors,
convenience functions, `Client.Do`, request/URL fields, and bounded
package-local helpers while retaining the highest application callsite and the
underlying sink chain. Literal concatenation and `fmt.Sprintf` are accepted
only when dynamic path segments are proven safe through `url.PathEscape`;
query strings remain evidence outside route identity. Cycles, unsafe dynamic
segments, and unknown receiver authorities fail closed, and an unknown
authority can never produce a path-only local match.

Go/Chi composition is resolved once in the compact package semantic view using
go/types API identity. The analyzer expands package-local router helpers and
constructors with a bounded recursion guard, applies `Use`, `With`, `Group`,
`Route`, and `Mount` scope rules, and emits only the final mounted endpoint at
the leaf registration location. Dynamic prefixes and ambiguous or cyclic
helpers remain diagnosed boundaries rather than root-relative guesses.
Persisted `uses_middleware` edges are the sole query authority for the ordered
effective middleware chain; topology queries do not reconstruct router syntax.

## Protocol message flow

`internal/query.MessageFlowService` owns the task-shaped interpretation of
canonical Protobuf flow. It resolves canonical messages through the shared
selector service, then uses only bounded `RelationEdgeRepository` pages for
`generated_from`, field reads/writes, codecs, transport carries, sends,
receives, and direct caller evidence. Storage and federation hydrate each
counterpart atomically; the query layer alone calculates member gaps, pipeline
gaps, channel conflicts, and `resolved`, `missing_evidence`, or `unknown`.

Component and repository attribution is built from bounded node enumeration and
bounded `component contains file` evidence. Dynamic or symbolic transport,
unresolved or unsupported bindings, failed refreshes, and truncation never
become negative claims. CLI and MCP adapters render the same typed result, and
evaluation schema version 2 stores compact task-query goldens keyed by stable
node and edge identities across initial, unchanged-incremental, and fresh
database runs.

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
generic YAML source parser. One shared loader validates Grafo-owned top-level
sections while ignoring sections owned elsewhere. The SQL router consumes its
validated `sql` section, and its semantic cache key includes only that section:
changing a path mapping or default reparses unchanged SQL sources that may now
select another dialect, while a component-only edit does not. Parser-contract
changes also bump the semantic-index version.

The same loader owns `http.request_apis`. GDScript includes only that validated
HTTP subtree in its semantic key and declares repository-root `grafo.yaml` as a
semantic dependency. A configured adapter names one exact qualified callable
and its method/URL argument positions; parser scope and receiver evidence still
decide whether a source call resolves to that identity. Built-in Godot
`HTTPRequest.request` uses its fixed Godot 4 signature. Both paths feed the
shared HTTP route model, so canonical request identity and topology matching do
not acquire a GDScript-specific variant.
