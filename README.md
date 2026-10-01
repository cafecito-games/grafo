# Grafo

Grafo turns a repository into a deterministic semantic graph that coding agents
can query from the command line. Source symbols, calls, imports, configuration,
HTTP routes, and event-like publish/subscribe operations become nodes and edges.

This repository is at the foundation stage. Go is parsed with the Go compiler
AST, Godot source formats use `gdparser`, Protobuf uses Buf's `protocompile`,
Java, Python, Swift, and TypeScript/TSX use Tree-sitter, PostgreSQL SQL uses
PostgreSQL's own parser, and SQLite SQL uses Meyer's SQLite grammar. The index
is local, incremental, branch-aware, and stored in SQLite.

[Architecture and extension points](docs/architecture.md)

## Install

Homebrew is the supported way to get a prebuilt binary on macOS and Linux:

```sh
brew install cafecito-games/tap/grafo
```

Building from source needs a C toolchain, because the tree-sitter grammars are
C libraries:

```sh
go install github.com/cafecito-games/grafo/cmd/grafo@latest
```

Prebuilt archives for macOS, Linux, and Windows are also attached to every
[release](https://github.com/cafecito-games/grafo/releases).

## Quick start

```sh
grafo install
grafo index .
grafo status
grafo indexes list
grafo find "MyHandler"
grafo neighbors "MyHandler" --depth 2
grafo path "HandleCheckout" "Charge"
grafo impact "Charge"
grafo search "chargeRetryLimit"
grafo source "HandleCheckout"
grafo data-resources
grafo data-usage "orders"
grafo config-keys
grafo events
grafo orphaned-events
grafo endpoints
grafo outbound-requests
grafo find-handler --route /orders
grafo service-topology --mermaid
grafo watch
grafo mcp
```

### Installing into MCP clients

`grafo install` detects every supported MCP client and registers Grafo with
each one as a user-level stdio server. Supported clients are Claude Code,
Claude Desktop, Cline, Codex, Cursor, Gemini CLI, OpenCode, VS Code, and
Windsurf. Clients with an official CLI are configured through it; the rest are
configured by editing their documented user-level config file.

```sh
grafo install --list          # detect only; never writes
grafo install --all --dry-run # report every file and command a real run touches
grafo install claude codex    # or: grafo install --client cursor,vscode
grafo install --mcp-only      # register the server without installing guidance
grafo install --refresh       # update only artifacts that already exist
grafo install --hooks         # also install advisory, fail-open hooks
grafo uninstall --all         # remove only Grafo-owned artifacts
```

Config files are parsed structurally, written atomically, and unrelated servers
and settings are preserved. `--list` and `--dry-run` cannot write. Naming a
client that is not installed is an error; automatic and `--all` mode skip
missing clients and say so. All of these accept `--json`.

### Installed agent guidance

Registration alone does not teach an agent when to use the graph, so `grafo
install` also installs a canonical, embedded guidance playbook: prefer graph
structure over content search for symbol, call, endpoint, event, data, and impact
questions; check `get_index_status` before trusting the graph; search for reusable
code before adding code; run bidirectional `get_blast_radius` before a
behaviour-changing edit; and fall back to native tools deliberately when the
content is not code or the branch has no index.

Claude Code and Codex also receive a separate `grafo-setup` skill. Invoke it
when onboarding or tuning a repository for Grafo (`/grafo-setup` in Claude Code
or “use `$grafo-setup`” in Codex). It inspects monorepo boundaries,
SQL ownership, source-proven GDScript event and HTTP application wrappers,
custom test bases, Protobuf serialization, and supported transport evidence. It
merges only supported `grafo.yaml` settings, indexes the repository, and verifies
topology and message coverage. Built-in transport and serialization support is
detected from source and code-generation evidence, so the skill reports
unsupported integrations instead of inventing configuration keys. Adapter audits
require an exact helper body, qualified symbol, supported effect, and
argument-role mapping, followed by a reindex and before/after graph comparison.

Guidance is installed only through documented, user-scoped surfaces: isolated
Grafo-owned skill files for Claude Code and Codex, and one delimited managed
block (`<!-- BEGIN grafo-guidance -->` … `<!-- END grafo-guidance -->`) for
Gemini CLI, OpenCode, and Windsurf. Everything outside the markers is preserved
byte-for-byte, a file with no Grafo ownership marker is never overwritten, and
duplicated or half-present markers are reported instead of repaired.
Repository-local instruction files are never edited, no permission is granted,
and no edit is ever blocked.

After upgrading Grafo, run `grafo install --refresh` to update existing
Grafo-owned structural and setup skills to the current canonical versions. It
does not create a missing guidance artifact and never adopts a foreign file.

`--hooks` opts in to advisory `PreToolUse` hooks in Claude Code's documented
personal settings. They only inject context: `grafo guidance --hook pre-search`
and `--hook pre-edit` print one advisory line and always exit 0, so a missing or
broken Grafo can never block a tool call. `grafo guidance` prints the same
canonical text plus whether the current repository and branch actually have an
index.

Every installed artifact is recorded in a receipt under the Grafo configuration
directory (`$XDG_CONFIG_HOME/grafo/installed-artifacts.json`) with its target,
digest, guidance version, and ownership marker, so an upgrade replaces exactly
the previous Grafo-owned content and `grafo uninstall` removes only what it can
prove Grafo wrote. Anything it cannot prove is left in place and reported with
the manual step. Re-running install is idempotent, `--dry-run` writes nothing,
and a real run prints every target and action before mutating anything.

The generated MCP configuration uses the absolute path of the installed Grafo
binary, so agents do not depend on their launch environment's `PATH`. Re-run
the command after moving the binary. Each repository still needs an initial
`grafo index .`. An MCP session then performs one synchronous startup refresh
and serves unchanged calls through a persistent query-only generation. Before
each tool call it compares an opaque source-freshness token; only changed
projects reopen a short-lived writer and refresh before a new generation is
published. Non-Git projects conservatively retain a full refresh per call.

### Index reporting cost

`grafo index` reports what the run changed: updated, unchanged, removed, and
skipped files plus reconciliation timing. The full-graph counts summary is a
separate scan of every node, fact, and edge whose cost grows with total graph
size rather than with what changed, so it is opt-in:

```sh
grafo index .                    # change report only
grafo index . --counts           # also collect the full-graph counts summary
grafo watch . --counts           # same opt-in for the watch report
grafo status                     # always reports the full counts summary
```

Without `--counts` the report states that counts were not collected, and the
JSON report sets `counts_collected` to false and omits `counts` entirely rather
than reporting zero totals as real values. If counts were requested but a count
query failed, the run still succeeds, the report says the summary is unavailable
rather than not requested, and the cause is reported as a run diagnostic. The
background service never requests counts, because it never reports them.

### Background indexing and diagnostics

`grafo watch` keeps one repository current while a terminal stays open. For
several repositories at once, register them and install a local background
service instead:

```sh
grafo service add .              # register this repository root
grafo service list               # show the user-level registry of watched roots
grafo service install            # macOS launchd agent or Linux user systemd unit
grafo service status             # platform state plus per-root last pass
grafo service logs --lines 50    # bounded, rotating, source-free log
grafo service uninstall          # remove only the definition Grafo owns
grafo service run --once         # one reconciliation pass in the foreground
```

The service is entirely local. Exactly one supervisor runs at a time, every
indexing run holds its branch index's lock so a foreground `grafo index` can
never race it, and a killed supervisor leaves committed indexes intact: the next
pass reconciles from them. Branch switches and worktrees stay isolated, a
temporarily unavailable root is paused rather than pruned, and one failing root
never stops the others.

```sh
grafo doctor                     # read-only report; nothing is mutated
grafo doctor --json
grafo doctor --repair            # only the four documented repairs
```

`grafo doctor` reports the binary, the registry, every root's branch and index,
the service definition, the supervisor, and the agent registrations. `--repair`
unregisters a definitively missing, unshared root, refreshes Grafo-owned agent
artifacts, recreates a service definition Grafo installed, and restarts a stale
service. It never deletes repository indexes, never guesses where a moved
repository went, and never rewrites content Grafo cannot prove it wrote.

Ownership is bound to one resolved path, so a receipt for one definition never
authorizes writing another, and a unit Grafo cannot prove it wrote is never even
stopped. Service definitions must name a durable binary: installing or repairing
from a `go run` build is refused, because the unit would outlive the build
directory it points into.

Every query supports `--json` for agent-friendly output. Run `grafo help` for
the complete command surface.

`grafo mcp` starts a standards-compatible MCP server over stdio with tools for
symbol discovery, node lookup, traversal, shortest paths, callers, callees,
change impact, failure flow, Godot composition, graph-addressed source
retrieval, bounded source search, reusable-code discovery, index status, and the data,
configuration, and event catalogs.

Symbol, node, source, caller, callee, path, and impact tools accept a batch of
inputs and return one result or error per input in the caller's order, so one
bad selector never erases unrelated results. The scalar input fields remain
supported.

### Selector resolution

Every command and tool that takes a symbol resolves it through one code path, and
that path either returns a single node on evidence or reports the complete set of
equally good matches. It never returns one of several matches as though it were
unique.

Only the strongest kind of evidence a selector produced is considered: an exact
qualified name, else an exact name, else a substring, and within each of those a
local declaration outranks an external boundary node. Case-sensitive evidence
outranks all of that, so a case-insensitive-only hit at a strong level never beats
a case-sensitive match at a weaker one — the selector `Path` is decided by the
nodes named exactly `Path`, not by a module whose qualified name is `path`. Within
one kind of evidence a case-sensitive match decides on its own, so `impact`
resolves to `App.impact` rather than tying with `Service.Impact`;
case-insensitive-only matches are still listed when the selector is ambiguous,
because they are usually what has to be told apart. A symbol's own parameters, local variables, and fields do not make its
selector ambiguous, but a nested qualified name alone never suppresses a
candidate: `type Charge struct{}` and `func (Charge) Charge()` are two
declarations sharing one name, so `Charge` stays ambiguous between them.

An ambiguity error reports the total number of matches in the graph and says when
the listed candidates are only part of it. Commands and tools that take a selector
also accept an optional node kind (`--kind` on the CLI, `kind` in MCP input), so a
caller can say it means the function rather than a parameter of the same name.

### Change impact

`grafo impact` (also `grafo blast-radius`, and the MCP `get_blast_radius` tool)
returns one bidirectional report instead of an incoming-only traversal: what
depends on the symbol, what the symbol depends on, the impacted files, any
cross-repository hops, and the related configuration, data, and event facts.

```sh
grafo impact "Charge" --upstream-depth 3 --downstream-depth 2
grafo impact "Charge" --source --max-lines 40 --json
```

Upstream and downstream depth and node limits are bounded independently and
each section reports its own truncation. Source excerpts are opt-in and read
through the same bounded reader `grafo source` uses.

### Failure flow

`grafo failure-flow` (MCP `get_failure_flow`) reports a callable's typed error
result positions separately from failures that actually escape by direct return,
wrapping, or joining. It also groups consuming handlers, panic and recovery
sites, and deferred cleanup without claiming branch reachability.

```sh
grafo failure-flow "example.com/app.Process"
grafo failure-flow "ErrNotFound" --direction incoming --json
```

Go object and type identity is authoritative for error-returning calls, sentinel
variables, named error types, `fmt.Errorf` `%w`, `errors.Join`, `errors.Is`, and
`errors.As`. Comparisons and common `if err != nil` or switch handlers are
reported only when their branch consumes control flow; logging by itself is not
handling. Dynamic panic payloads and facts from incomplete packages remain
explicit unresolved or conditional evidence.

### Godot composition

`grafo godot composition` (MCP `get_godot_composition`) answers Godot runtime
composition questions directly from the graph: which scenes a scene
instantiates, which scenes instantiate it, which scripts are attached to which
scene nodes, scenes, and resources, and which autoload singletons expose a
script or scene globally.

```sh
grafo godot composition "scenes/main"
grafo godot composition "godot:autoload:client/project.godot:GameSession" --json
```

Scenes, resources, scene nodes, and autoloads are first-class node kinds
(`godot_scene`, `godot_resource`, `godot_scene_node`, `godot_autoload`) linked
by `instantiates`, `attaches_script`, and `autoloads` edges. Every edge keeps
its original evidence - resource path, UID alias, `ExtResource` id, scene node
path, and instance-placeholder marker. Scene inheritance is an `instantiates`
edge, never language `extends`.

`res://` references resolve against the nearest ancestor `project.godot`, so a
Godot project in a monorepo subdirectory resolves correctly and a reference that
traverses out of its own project is diagnosed rather than resolved into a sibling
project. Autoload identity is scoped to that file
(`godot:autoload:<project.godot path>:<Name>`) so sibling projects that share an
autoload name stay distinct. A reference whose UID is declared by a different
resource, whose `ExtResource` id is declared more than once, or whose autoload
name is declared more than once is reported as a diagnostic and stays unresolved
instead of resolving to a guess, and an autoload declared without Godot's `*`
singleton marker never satisfies a global identifier in a script. Evidence that
could not be read is never treated as agreement: a UID resolves only when Grafo
can prove it is declared exactly once, so while any candidate file is unreadable
the reference stays unresolved and the diagnostic names the file that blocked the
proof.

### Godot gameplay interactions

`grafo godot interactions` (MCP `get_godot_interactions`) answers how a Godot
project is wired at runtime: which input actions a script reads, which node
groups a scene node or script joins, leaves, inspects, and dispatches to, and
which signal routes a symbol takes part in - whether a scene declared the route
or a script established it.

```sh
grafo godot interactions "scenes/arena"
grafo godot interactions "godot:node_group:client/project.godot:enemies" --direction incoming
grafo godot interactions "scripts/player.poll" --filter action,group --json
```

Input actions and node groups are first-class node kinds
(`godot_input_action`, `godot_node_group`) linked by `uses_input_action`,
`in_group`, and `uses_group` edges; signals stay `event` nodes with the
`publishes`, `subscribes`, and `handled_by` edges every other producer uses, so
generic event and configuration queries keep working. The operation behind an
edge is a `form` property (`declared`, `add`, `remove`, `membership_test`,
`lookup`, `call`, `notify`, `query`, `press`, `release`, `configure`, `emit`,
`connect`) rather than another edge kind. `is_in_group` is a lookup and never
membership: asking whether a node is in a group is not evidence that it is.
Every fact and reconciled edge also exposes an explicit `producer`; this is
extraction provenance, not endpoint language. Godot interaction reports accept
only `gdscript` and `godot` producers, and still require a known signal form.
Missing, unknown, or non-Godot producers fail closed even if they reuse a
Godot-looking relation or form.

Identity is scoped to the declaring project
(`godot:input_action:<project.godot path>:<name>`,
`godot:node_group:<project.godot path>:<name>`), so a scene's
`groups=["enemies"]`, a script's `add_to_group("enemies")`, and the project's
`[global_group]` declaration converge on one node. Godot's declaration sites are
narrow and Grafo does not widen them: an action comes from an `[input]` entry and
a group from a `[global_group]` entry, so a group nothing declares stays an
unresolved node and the report counts it - missing wiring is visible rather than
absent. Only non-secret declaration metadata is stored; device bindings are not
modelled.

Script resolution is literal-only. A computed action or group name produces no
edge at all, a group dispatch keeps its method name as evidence without ever
resolving a handler, and a signal name that several declarations could own stays
unresolved rather than fanning out to all of them. `disconnect` and
`is_connected` are recorded as routing evidence, not as subscriptions.

### Bounded source search

`grafo search` (MCP `search_source`) answers content questions the graph does
not model. It searches only files that belong to a refreshed Grafo index,
reading them from the matching worktree, and never copies source into SQLite.

```sh
grafo search "chargeRetryLimit" --context-lines 2
grafo search "func \(s \*Service\) [A-Z]" --regex --language go --path-prefix internal
```

Patterns are literal by default, or Go RE2 with `--regex`. Binary and oversized
files are skipped, every cap is reported rather than silently applied, and
ordering is by repository, path, line, and column so results never depend on
filesystem enumeration. Prefer the graph tools when the question is structural.

`grafo source` resolves a graph node first, then reads its exact bounded source
span from the active worktree. In a federation, the node ID selects the correct
repository even when several repos contain the same relative path. Reads are
confined to the repository root and capped by line and byte limits.

### Semantic candidate discovery

Embeddings are optional and only select candidates for natural-language reuse
queries. Grafo then resolves each candidate's relationships from the graph and
returns its deterministic one-hop structural context.

With a local [Ollama](https://docs.ollama.com/capabilities/embeddings) instance:

```sh
ollama pull embeddinggemma
grafo embed .
grafo reusable "validate and normalize an incoming payment request"
```

`grafo embed` is incremental: only new or semantically changed symbol metadata
is sent to the embedding provider. `grafo reusable` and the MCP
`find_reusable_code` tool perform that sync automatically. Configure another
Ollama-compatible location or model with `--ollama-url`, `--model`,
`GRAFO_OLLAMA_URL`, and `GRAFO_EMBED_MODEL`. The defaults are
`http://localhost:11434` and `embeddinggemma`. Use `grafo embed --force` after
replacing a model under the same model name.

Normalized vectors are shared across branches and repositories through a
content-addressed SQLite cache at `<os.UserCacheDir()>/grafo/embeddings.sqlite`.
Set `GRAFO_EMBED_CACHE` to an explicit local file path to override it. Cache
keys contain only the exact model name, semantic document version, and SHA-256
content hash; vectors are stored losslessly as little-endian float32 binary
data. The cache contains no source documents, symbol names, repository paths,
provider credentials, or endpoint URLs. Equal semantic documents therefore use
one cached vector while their graph nodes remain separate search candidates.
On POSIX platforms, Grafo creates the cache directory with owner-only `0700`
permissions and the cache file with owner-only `0600` permissions.

Inspect or evict this rebuildable data without contacting the provider:

```sh
grafo embed-cache status
grafo embed-cache status --json
grafo embed-cache prune --older-than 720h --dry-run
grafo embed-cache prune --model embeddinggemma --max-bytes 1073741824 --yes
```

Prune filters compose, and size-bounded eviction removes the oldest eligible
vectors first. A real prune always requires `--yes`; `--dry-run` opens the cache
read-only. Pruning checkpoints the cache WAL but deliberately does not run
`VACUUM`, so reported logical vector bytes can fall before the cache file
shrinks.

Upgrading from a branch-local embedding layout drops those rebuildable vectors,
so the first `grafo embed`, `grafo reusable`, or MCP reusable-code query may need
one provider-backed sync. Dropped pages remain reusable inside each branch
database. To return them to the filesystem immediately, run the separate
`grafo indexes compact . --yes` command; embedding-cache pruning never compacts
or mutates a structural graph index.

To query several repositories as one graph, index each once and pass their
paths to any query command or to the MCP server:

```sh
grafo index ../checkout-api
grafo index ../payments
grafo path "Checkout" "POST /charge" --repos ../checkout-api,../payments
grafo mcp --repos ../checkout-api,../payments
```

Grafo refreshes each active branch index incrementally before a query. Explicit
HTTP targets, imports, configuration references, and other unresolved facts are
resolved against exact declarations in the other indexes; synthesized edges
are marked `federated` and retain their original evidence.

## What the first slice understands

- Go packages, functions, methods, types, fields, parameters, local variables,
  imports, calls, basic assignment/argument/return flow, typed error returns,
  propagation, wrapping, joining, handling, panic/recovery, deferred cleanup,
  embedding,
  `os.Getenv`/`LookupEnv`, `net/http` routes, and common publish/subscribe calls.
  Go `Test*`, `Benchmark*`, `Fuzz*`, and `Example*` declarations in `_test.go`
  files are first-class test nodes when their standard-library signatures are
  valid; internal and external test-package identity is retained.
- TypeScript and TSX modules, functions, classes, interfaces, methods,
  parameters, local variables, basic assignment/argument/return flow, imports,
  calls, inheritance, `process.env`, Express-style routes, and common
  publish/subscribe calls.
- Java packages, imports, classes, interfaces, records, enums, annotations,
  methods, constructors, fields, parameters, local variables, inheritance and
  interface implementation, basic assignment/argument/return flow,
  `System.getenv`/`getProperty`, Spring and JAX-RS routes, common outbound HTTP
  client calls, and publish/subscribe calls.
- PostgreSQL tables, views, columns, indexes, functions, procedures, and the
  relations read or written by DDL and DML statements in `.sql`, `.pgsql`, and
  `.psql` files.
- SQLite tables, virtual tables, views, columns, indexes, triggers, foreign-key
  references, and the relations read or written by DDL and DML statements in
  `.sql` files.
- Python modules, functions, classes, methods, fields, parameters, local
  variables, type annotations, basic assignment/argument/return flow, imports,
  calls, inheritance, environment reads, framework route decorators, outbound
  HTTP requests, and common publish/subscribe calls.
- Protobuf packages, imports, messages, nested messages, enums and values,
  fields, maps, oneofs, extensions, services, RPC methods, streaming direction,
  and message/enum type references in `.proto` files. Buf v2 generation
  configuration can project supported `protoc-gen-go` v1 and gdproto v0.6
  names back onto those canonical declarations, including bindings whose
  generated GDScript files are ignored. Go application use of those projections
  records canonical message encoding/decoding and exact field/oneof reads and
  writes when `go/types` and the binding registry provide matching evidence.
- Swift imports, functions, classes, actors, structs, enums, protocols,
  extensions, methods, properties, parameters, local variables, basic
  assignment/argument/return flow, inheritance and protocol conformance,
  `ProcessInfo` environment reads, common server routes, outbound HTTP
  requests, and publish/subscribe calls.
- Go modules, npm packages, and Python requirements from `go.mod`,
  `package.json`, and `requirements*.txt`, including version, scope, indirect,
  optional, and replacement metadata. Dependency edges resolve across repos.
- Godot 4 GDScript script and inner classes, methods, fields, parameters, local
  variables, enums, inheritance, resource loads, calls, basic
  assignment/argument/return flow, environment and project-setting reads, and
  signal declarations, emissions, and connections. `$Node/Path`, `%UniqueName`,
  and literal `get_node`-family lookups reference matching scene nodes when the
  name is unambiguous.
  GUT-style `test_*` methods are first-class tests only when their class
  structurally extends `GutTest` or a validated configured base; lifecycle and
  helper methods remain explicit support nodes.
- Godot text scenes and resources (`.tscn`, `.tres`, and `.escn`) as first-class
  scenes, resources, and scene nodes, including subresources, properties,
  external resources, node paths, declarative signal connections, scene
  instances and inheritance, and script attachments; `project.godot` autoload
  singletons and their script or scene targets, resolved in GDScript uses of the
  autoload name; ConfigFile documents (`project.godot`,
  `.cfg`, `.gdextension`, `.import`, and `.remap`) and `.uid` sidecars; and
  shader/include modules, uniforms, structs, functions, parameters, locals,
  calls, global references, and `#include` relationships.
- `.env`, YAML, JSON, TOML, and Java `.properties` keys and value references.
- Markdown documents (`.md` and `.markdown`) as structural graphs: headings are
  bounded `document_section` nodes, relative links connect sections to files or
  other document sections, and explicit code keywords connect prose to symbols.
- Outbound Go `net/http`, Python Requests/HTTPX, and TypeScript `fetch`/Axios
  calls, linked to matching endpoint declarations locally or across repository
  boundaries. Go extraction follows typed `net/http` request constructors,
  convenience calls, `Client.Do`, request fields, and bounded package-local
  wrappers; unescaped dynamic paths and unknown receiver authorities stay
  unresolved rather than becoming path-only links.
- Deterministic symbol lookup, neighborhood traversal, shortest paths, callers,
  callees, failure-flow and blast-radius reports, multi-repository federation,
  and MCP access.

## Structural test relationships

Grafo persists a `tests` relationship only when a first-class test has a unique
local `calls` or `references` edge to a production declaration. Bounded query
expansion may cross methods or functions explicitly classified as test helpers
or lifecycle hooks. External, ambiguous, and unsupported targets fail closed;
cycles, depth exhaustion, and size exhaustion are reported as truncated.

```sh
grafo find-tests example.com/shop.Charge --json
grafo test-coverage example.com/shop.TestCharge --json
```

The same operations are available through MCP as `find_tests` and
`get_test_coverage`. They report structural source evidence, not runtime
execution coverage.

Godot projects can add exact test base names without weakening the built-in
structural requirement:

```yaml
tests:
  gdscript_bases: [SpecBase, addons.gut.CustomBase]
```

Invalid `tests` configuration emits a warning and uses only the built-in
`GutTest` convention. Editing, fixing, or removing the section invalidates
otherwise unchanged GDScript files so stale classifications are reconciled.

Grafo combines syntactic extraction with Go toolchain type evidence for exact
Go function and method dispatch, promotions, generic instantiations, and
repository-local interface method sets. Other languages remain syntactic and
name-based, and Grafo does not perform whole-program pointer analysis or full
SSA data flow.
Structural retrieval remains graph-based; vector similarity is confined to
optional candidate discovery.

## Documentation graph

Markdown headings become addressable nodes such as
`docs/architecture.md#request-flow`. Their `contains` edges preserve the heading
hierarchy, and their source spans cover the section through the next heading at
the same or higher level. Relative inline and reference-style links produce
directional `documents` edges to indexed files or Markdown anchors. External
URLs and image links are ignored.

Documentation can point directly to code with a structural keyword followed by
an inline-code name:

```md
The function `HandleCheckout` uses class `ChargeService`.
Requests enter through endpoint `POST /checkout` and method `Run`.
```

Supported keywords are `function`, `method`, `class`, `interface`, `type`, and
`endpoint`. Resolution uses exact qualified names or an unambiguous simple name.
If several declarations match, Grafo retains an explicit unresolved node rather
than guessing an edge.

## Data, configuration, and event catalogs

The catalogs answer inventory and usage questions directly instead of leaving
them to manual traversal:

```sh
grafo data-resources --kind table,view --name order --json
grafo data-usage "orders" --json
grafo config-keys --name DATABASE_ --json
grafo events --json
grafo orphaned-events --json
```

The same results are available as the `list_data_resources`,
`get_data_resource_usage`, `list_config_keys`, `list_events`, and
`find_orphaned_events` MCP tools. Every catalog accepts a repository filter and
an explicit bound, reports truncation, and orders results deterministically. The
list catalogs also accept a name filter, and the data-resource catalog a kind
filter; usage names its resource with a selector instead. A name filter matches literally, so it narrows a catalog and
never widens it. The bound applies to each catalog section and, separately, to
the evidence sites of each relation.

`data-usage` partitions the edges reaching a table or view into readers,
writers, and references, each with the source site that proves it. An ambiguous
name returns its candidates and asks for a qualified name or node ID rather than
choosing one. An unsupported kind is rejected instead of answered with an empty
catalog that would imply absence, and a name filter is rejected here rather than
accepted and ignored, because the selector already names the resource. A name
filter is trimmed before use, so a blank one narrows nothing at every surface
rather than narrowing to nothing at some of them.

`config-keys` reports where a key is defined and read. Stored values are never
returned: only properties classified as non-secret configuration metadata appear,
and the names of anything withheld are listed in `withheld_properties`.

`orphaned-events` separates three categories — published without a consumer,
consumed without a producer, and declared with neither — and separates a
confirmed orphan from an uncertain one. When an unresolved target could be the
missing counterpart, the finding's status is `unknown` and the response carries
the counterpart counts and their evidence. Evidence a bound cut off is treated
the same way, so a truncated event is never a confirmed orphan. Each relation
carries its own bound, so a busy reader or publisher list can never make another
relation look empty. Event names that no declaration
resolves are always `unknown`, because nothing in the index bounds where they
are published or consumed. Federation applies the same contract: a producer in
one repository and a consumer in another clear the orphan.

## Repository components

A monorepo can declare explicit component ownership in its repository-root
`grafo.yaml`:

```yaml
components:
  - name: client
    roots: [client, web/ui]
  - name: server
    roots:
      - apps/server
```

Component names are case-sensitive, unique, and use letters, digits, `.`, `_`,
or `-`. Roots are normalized repository-relative paths with `/` separators;
they cannot contain globs, escape through `..`, duplicate one another, or
overlap another root. `.` assigns every eligible indexed file to one component.

Indexing stores a component node, a repository `contains` component edge, and a
component `contains` file edge for every proven match. Root matching respects
path segments, so `client` owns `client/main.gd` but not
`client-old/main.gd`. Ignored, unsupported, symlinked, and oversized files are
never assigned. Files that match no component remain repository-scoped, and a
component with no eligible files remains in the graph with a warning.

The declarations are the configuration authority; persisted component-to-file
edges are the authority for queries. Grafo rejects malformed, duplicate,
overlapping, or ambiguous component configuration before changing the durable
index. Component edits rebuild only workspace ownership evidence, so unchanged
language source files are not reparsed.

## Repository source scope

Repository owners can explicitly narrow indexed source membership in the root
`grafo.yaml`:

```yaml
index:
  include: ["cmd/**", "internal/**"]
  exclude: ["internal/eval/testdata/**"]
```

Patterns are slash-based repository-relative globs with literal segments, `*`,
`?`, and recursive `**` segments. An empty `include` means every otherwise
eligible source, an empty `exclude` excludes nothing, and exclusion wins. The
root `grafo.yaml` remains indexed as the control plane even when its own path
does not match. Includes cannot re-enable built-in ignored paths or symlinks.
Invalid, absolute, traversing, or backslash paths fail before index mutation.

Changing the normalized scope forces full candidate discovery at the next
index pass: narrowing removes stale file-owned graph evidence and broadening
discovers files absent from the prior catalog. Repeating an unchanged scope
retains normal incremental membership reuse. Index reports expose only the
count of parser-supported candidates scoped out by configuration, not the
excluded path list. Because scope is enforced before filesystem inspection,
an excluded candidate is classified as scoped out rather than subsequently as
a symlink, non-regular file, size skip, or read failure.

## Endpoint and service topology

Godot 4 `HTTPRequest.request` calls are indexed only when the receiver is
typed or inferred as `HTTPRequest`. Projects can describe an exact GDScript
callable with one or more typed effects in repository-root `grafo.yaml`:

```yaml
adapters:
  - match: {language: gdscript, symbol: Signals.wire}
    effects:
      - kind: event.subscribe
        roles:
          event: {argument: 0}
          handler: {argument: 1}
  - match: {language: gdscript, symbol: AuthAPI.request_json}
    effects:
      - kind: http.request
        roles:
          method: {argument: 0}
          url: {argument: 1}
```

The V1 effect vocabulary is `event.publish`, `event.subscribe`,
`event.unsubscribe`, `event.connection_test`, and `http.request`. Argument
positions are zero-based. Symbols are exact qualified identities: wildcards,
simple-name fallback, shadowed or unresolved receivers, unknown roles, and
unsupported languages fail closed. Invalid declarations fail indexing before
durable mutation. Emitted facts carry `adapter_symbol` and
`adapter_source=grafo.yaml:<line>` while retaining the ordinary `calls` fact.
Editing or removing adapters reparses GDScript and reconciles stale effects.

The legacy `http.request_apis` section remains a compatibility alias and is
normalized into the same `http.request` registry; `route_argument` remains an
alias of `url_argument`. Declaring the same language/symbol through both forms
is rejected. New configuration should use `adapters`; removal of the legacy
form requires a separate compatibility decision.

The extractor accepts Godot's symbolic `HTTPClient.METHOD_*` constants and
bounded, fully known string literals, constants, assignments, concatenations,
and `%` formatting. Raw numeric methods and dynamic routes produce no request
edge. Query strings and fragments remain evidence while the shared HTTP model
owns canonical endpoint identity.

Endpoint queries turn HTTP and event wiring into task-shaped results while
keeping the graph evidence authoritative:

```sh
grafo endpoints --method GET --route /orders --json
grafo outbound-requests --repo-name checkout --json
grafo find-handler --event order.placed --json
grafo service-topology --repo-name checkout --direction outgoing --json
grafo service-topology --component client --direction outgoing --json
grafo service-topology --mermaid
```

The MCP equivalents are `list_endpoints`, `list_outbound_requests`,
`find_handler`, and `get_service_topology`. Every query supports explicit
bounds and repository filtering. Service topology additionally accepts an exact
component name, either alone across selected repositories or together with a
repository filter. HTTP methods are normalized and
matched exactly; route filters use the same canonical template compatibility
as indexed evidence. Handler and topology queries can also filter by event.
Service topology supports incoming, outgoing, or both directions relative to
every service matching the repository/component scope. An unknown component
returns an empty result rather than falling back to its repository.

An explicitly owned component is one stable service identity; files with no
component evidence retain the indexed repository's stable fallback service.
Component service IDs derive from repository identity and the indexed component
node ID, so equal component names in federated repositories never collide.
Labels render as `repository/component`, while structured resources and service
nodes expose repository, component name, and component node ID separately.
Endpoint and event nodes, participating code resources, edge IDs, fact IDs,
locations, and federation markers remain in the structured response beneath
those service boundaries. HTTP route
identity excludes queries, fragments, and trailing slashes, and template
parameter names are canonicalized while regex constraints and catchalls remain
distinct. Resolution ranks exact literals ahead of compatible single-segment
templates, then compatible catchalls. Several declarations at the best rank
remain ambiguous and create no confirmed service link. Unknown values do not
prove regex matches, and absolute URLs with an authority stay external rather
than resolving from their path alone. Their scheme and authority remain part of
the external service identity. Handler results use only `handled_by`
evidence and likewise distinguish resolved, unresolved, ambiguous, and missing
handlers. Go/Chi route composition uses typed `Route`, `Mount`, `Group`, `Use`,
`With`, verb, `Method`, and `Handle` calls to retain the complete mounted path.
Endpoint results expose the effective outer-to-inner middleware chain through
bounded `uses_middleware` evidence; unresolved middleware stays explicit and
each entry retains its form, order, and source call site.

`service-topology --mermaid` is an escaped, deterministic rendering of the
structured result; it never replaces the node and edge evidence. Explicit
federation refreshes all member indexes before answering, so a failed refresh
returns no mixed-freshness topology.

Inventory commands accept repeatable or comma-separated `--path-prefix`; their
MCP inputs use `path_prefixes`. The option is available on `data-resources`,
`config-keys`, `events`, `orphaned-events`, `endpoints`, `outbound-requests`,
`service-topology`, and `message-coverage`. Prefixes are validated
repository-relative segment prefixes: `internal/app` includes descendants but
not `internal/application`. They select canonical top-level subjects before
limits while retaining complete bounded counterpart evidence. A topology link
is selected when either non-external boundary is in scope. Federation applies
the same relative prefixes independently to every member. Scalar selector
commands and commands outside this list intentionally reject this filter before
opening a repository; they never accept and silently ignore it. Source search
keeps its pre-existing `--path-prefix` support.

## Protobuf message flow and coverage

Message-flow queries join canonical Protobuf declarations to generated
bindings, exact field and oneof-arm reads/writes, codecs, ENet operations, and
bounded caller evidence without reconstructing relationships from names:

```sh
grafo message-flow acme.v1.Envelope --json
grafo message-coverage --package acme.v1 --status missing_evidence --json
grafo message-coverage --message Envelope --oneof payload --component client --json
```

The MCP equivalents are `get_message_flow` (scalar or ordered batch selectors)
and `list_message_coverage`. Coverage reports `resolved`, `missing_evidence`,
or `unknown`; dynamic transport, absent supported binding evidence, unresolved
projections, and bounded truncation are uncertainty rather than proof of a
missing runtime stage. Every aggregate retains canonical node IDs, edge and
fact IDs, source locations, repository identity, and component identity when
available. Oneof arms are evaluated independently. Channel mismatches are
reported only for proven send and receive operations carrying the same
canonical message.

## SQL dialects

One SQL router owns `.sql` files and delegates them to installed dialects.
PostgreSQL also owns the unambiguous `.pgsql` and `.psql` extensions. SQLite
does not claim `.sqlite`, `.sqlite3`, or `.db`, because those normally contain
binary databases rather than SQL source. Plain `.sql` remains
configuration-free when exactly one installed dialect accepts its syntax.
When multiple dialects accept the same file, select one explicitly in a
repository-root `grafo.yaml` that is safe to commit:

```yaml
sql:
  default_dialect: postgres
  paths:
    "desktop/**/*.sql": sqlite
    "migrations/**/*.sql": postgres
```

Selection is deterministic: a matching path mapping wins, followed by a
dialect-specific extension, the repository default, and finally syntax
probing. Path patterns use `/` separators and support `*`, `?`, character
classes, and `**` as a whole path segment. If patterns overlap, the one with
more literal characters wins, then the one with fewer wildcards, then lexical
order. A configured dialect must be installed; Grafo reports ambiguous syntax
or an unavailable dialect instead of depending on parser registration order.

The distribution installs PostgreSQL and SQLite dialects. Files containing
portable SQL that both parsers accept therefore need a default or path mapping;
dialect-specific syntax can still be selected by probing.

## Design principles

1. Structural answers come from ordered graph traversal, never top-k guessing.
2. Stable IDs and explicit edge evidence make every hop inspectable.
3. Changed files are the unit of parsing; unchanged files are not reparsed.
4. Each Git branch/worktree has its own index and is never silently substituted.
5. Unsupported syntax produces diagnostics instead of invented relationships.

Index files live under `.grafo/indexes/` and should not be committed.
Configuration values are used transiently to discover references such as
`${DATABASE_HOST}` but are not persisted, so indexing an `.env` file does not
copy its secrets into the graph. Normal indexing is entirely local. Semantic
sync sends only generated symbol metadata and signatures—not source files—to
the configured embedding endpoint; the default endpoint is local Ollama. The
user-level embedding cache is local to the OS user and stores only numeric
vectors plus source-free content hashes. It is never graph evidence, and stale
cache rows cannot make a symbol appear in results.

On Git worktrees, Grafo also avoids rereading every file to rediscover changes.
It combines the indexed commit, Git's current tracked/untracked changes, and a
persisted dirty-path set. The persisted set is what makes restoring a previously
indexed dirty file detectable. Non-Git directories retain the content-hash
full-scan fallback.

Edge reconciliation is restartable and bounded. A durable SQLite queue is
resolved in committed batches, with WAL checkpoints between batches; an
interrupted initial index resumes completed file hashes and pending facts.
Ambiguous symbolic names remain explicit unresolved nodes instead of producing
speculative edges to every declaration with the same name.

### Branch index inventory and retention

Every Git branch/worktree has its own SQLite database, so disk usage grows with
both graph size and the number of branches that have been indexed. Inspect the
complete physical footprint for one repository with:

```sh
grafo indexes list .
grafo indexes list . --json
```

The inventory reports each primary database and its exact WAL/SHM sidecar
bytes, stored branch/commit/repository metadata, compatibility, current-index
status, SQLite page/freelist estimates, and deterministic totals. A compaction
recommendation appears only when estimated reclaimable freelist space is both
at least 20% of the primary database and at least 256 MiB. WAL/SHM bytes are
separate and are not counted as freelist space. It examines only direct regular
`.grafo/indexes/*.sqlite` entries and never follows symlinks.

Pruning always requires at least one explicit retention selector. Preview first;
a dry run does not lock, checkpoint, rename, or delete any index file:

```sh
grafo indexes prune . --older-than 720h --keep 3 --dry-run
grafo indexes prune . --older-than 720h --keep 3 --yes
```

When both selectors are present, a branch must be older than the duration and
outside the keep set to be selected. `--keep 0` keeps no historical index, but
the current branch index is still always protected. Grafo also refuses to
automatically prune locked, corrupt, incompatible, metadata-free,
identity-mismatched, future-dated, or symlinked candidates. Each deleted index
is rebuildable from source with `grafo index`, but the next switch to that
branch pays the full rebuild cost. Advisory `.lock` anchors remain in place.

Compaction addresses free pages inside the current branch database; it does not
remove live graph data or stale branch databases. Preview the current estimate,
then compact explicitly:

```sh
grafo indexes compact . --dry-run
grafo indexes compact . --yes
```

Compaction takes the same exclusive index lock as indexing, checkpoints the
WAL, and runs SQLite `VACUUM`. It can require temporary disk space comparable to
the live database and future queries wait while it runs. An empty freelist is a
successful zero-byte no-op. Use source include/exclude settings to reduce live
graph scope, and `indexes prune` to remove whole non-current branch databases.

## Development

```sh
task hooks:install
task check
task eval:resolution
```

Grafo uses [prek](https://prek.j178.dev/) for commit-time formatting,
dependency tidiness, generated-code freshness, and lint checks. Install `prek`
using one of its supported installation methods, then run `task hooks:install`
once per clone. Use `task hooks:run` to check the whole repository manually.
The golangci-lint hook is pinned in `.pre-commit-config.yaml`, and CI runs that
same hook so local and pull-request lint results use the same version.

The resolution corpus is the parser-to-storage correctness gate for structural
changes. See [Deterministic resolution corpus](docs/evaluation.md) before
changing fixtures or committed expectations.

To exercise GDScript extraction against a representative Godot project, run:

```sh
GRAFO_GDSCRIPT_CORPUS=/path/to/project go test ./internal/parser/gdscript -run TestCorpus -count=1
```

The non-GDScript Godot formats can be checked against the same project with:

```sh
GRAFO_GODOT_CORPUS=/path/to/project go test ./internal/parser/godot -run TestCorpus -count=1
```

### Releasing

Releases are cut from the `Release` GitHub Actions workflow. Running it manually
with a `patch`, `minor`, or `major` increment tags the current default-branch
commit and publishes that tag; pushing a `v*` tag publishes it directly.
GoReleaser attaches the archives and checksums to the GitHub release and updates
the `grafo` cask in [cafecito-games/homebrew-tap](https://github.com/cafecito-games/homebrew-tap).

Every target links the tree-sitter C grammars, so the release job runs on macOS:
the system clang covers both Darwin architectures and [Zig](https://ziglang.org/)
cross-compiles the Linux and Windows targets. To reproduce the full artifact set
locally, install Zig and run:

```sh
task release:snapshot
```

### End-to-end corpus benchmark

The opt-in benchmark exercises the production parser registry, indexer, and
storage repository across cold, interrupted/resumed, unchanged, edit,
delete/restore, and branch-switch scenarios. SQLite remains the default:

```sh
GRAFO_BENCH_REPO=~/CafecitoGames/uzir task bench:corpus
GRAFO_BENCH_REPO=~/CafecitoGames/uzir GRAFO_BENCH_ENGINE=pebble task bench:corpus
```

Set `GRAFO_BENCH_OUTPUT=/path/outside/the/corpus` to retain artifacts at a
specific location. Otherwise the command creates and prints a temporary output
directory. `GRAFO_BENCH_BASELINE` accepts a prior report and rejects incompatible
report, graph-schema, or semantic-index versions; it records provenance but does
not impose an automatic timing comparison. Optional absolute resource gates use
`GRAFO_BENCH_MAX_WAL_BYTES` and `GRAFO_BENCH_MAX_RSS_BYTES`. Peak WAL is the
largest value observed at 100 ms intervals and durable indexer boundaries. Peak
RSS is the absolute process RSS observed during each scenario, including heap
retained from earlier scenarios. Timing measurements remain reported baselines
rather than committed machine-sensitive budgets.

The harness validates the input before creating an index, checks out only the
tracked files from the selected commit in an isolated clone, and stores every
database and report outside the supplied repository. It never reads the
corpus's untracked or ignored files, creates `.grafo` in the corpus, uploads
artifacts, or changes the source checkout's branch, HEAD, or worktree.
Unsupported tracked paths and platform metrics are explicit in the versioned
JSON report. A failed run keeps its isolated checkout and current database for
diagnosis; a successful run removes the checkout and duplicate control database
while retaining the converged database and atomic `report.json`.

The storage-spike harness runs SQLite, bbolt, and Pebble sequentially against
the same generated fixture and corpus revision, verifies candidate counts and
query semantics against SQLite, and emits raw samples, medians, relative
results, database/log size, RSS, and write statistics:

```sh
GRAFO_BENCH_REPO=~/CafecitoGames/uzir \
GRAFO_STORAGE_BENCH_OUTPUT=/tmp/grafo-storage \
GRAFO_STORAGE_BENCH_SAMPLES=3 \
task bench:storage
```

The bbolt and Pebble adapters are intentionally benchmark-only and are not
available to the production CLI. See the
[embedded storage spike](docs/storage-engine-spike.md) for the selection,
operational analysis, decision threshold, and recommendation.

After changing a migration or query, run `task generate` and commit the updated
sqlc output.

Licensed under Apache-2.0.
