# Grafo

Grafo turns a repository into a deterministic semantic graph that coding agents
can query from the command line. Source symbols, calls, imports, configuration,
HTTP routes, and event-like publish/subscribe operations become nodes and edges.

This repository is at the foundation stage. Go is parsed with the Go compiler
AST, Godot source formats use `gdparser`, Java, Python, Swift, and TypeScript/TSX
use Tree-sitter, PostgreSQL SQL uses PostgreSQL's own parser, and SQLite SQL uses
Meyer's SQLite grammar. The index is local, incremental, branch-aware, and
stored in SQLite.

[Architecture and extension points](docs/architecture.md)

## Quick start

```sh
go install github.com/cafecito-games/grafo/cmd/grafo@latest
grafo install
grafo index .
grafo status
grafo find "MyHandler"
grafo neighbors "MyHandler" --depth 2
grafo path "HandleCheckout" "Charge"
grafo impact "Charge"
grafo search "chargeRetryLimit"
grafo source "HandleCheckout"
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
grafo uninstall --all         # remove only Grafo's own registration
```

Config files are parsed structurally, written atomically, and unrelated servers
and settings are preserved. `--list` and `--dry-run` cannot write. Naming a
client that is not installed is an error; automatic and `--all` mode skip
missing clients and say so. All of these accept `--json`.

The generated MCP configuration uses the absolute path of the installed Grafo
binary, so agents do not depend on their launch environment's `PATH`. Re-run
the command after moving the binary. Each repository still needs an initial
`grafo index .`; subsequent MCP queries refresh its active branch index
incrementally.

Every query supports `--json` for agent-friendly output. Run `grafo help` for
the complete command surface.

`grafo mcp` starts a standards-compatible MCP server over stdio with tools for
symbol discovery, node lookup, traversal, shortest paths, callers, callees,
change impact, graph-addressed source retrieval, bounded source search,
reusable-code discovery, and index status.

Symbol, node, source, caller, callee, path, and impact tools accept a batch of
inputs and return one result or error per input in the caller's order, so one
bad selector never erases unrelated results. The scalar input fields remain
supported.

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
  imports, calls, basic assignment/argument/return flow, embedding,
  `os.Getenv`/`LookupEnv`, `net/http` routes, and common publish/subscribe calls.
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
- Godot text scenes and resources (`.tscn`, `.tres`, and `.escn`), including
  scene nodes, subresources, properties, external resources, node paths, and
  declarative signal connections; ConfigFile documents (`project.godot`,
  `.cfg`, `.gdextension`, `.import`, and `.remap`) and `.uid` sidecars; and
  shader/include modules, uniforms, structs, functions, parameters, locals,
  calls, global references, and `#include` relationships.
- `.env`, YAML, JSON, and Java `.properties` keys and value references.
- Markdown documents (`.md` and `.markdown`) as structural graphs: headings are
  bounded `document_section` nodes, relative links connect sections to files or
  other document sections, and explicit code keywords connect prose to symbols.
- Outbound Go `net/http`, Python Requests/HTTPX, and TypeScript `fetch`/Axios
  calls, linked to matching endpoint declarations locally or across repository
  boundaries.
- Deterministic symbol lookup, neighborhood traversal, shortest paths, callers,
  callees, blast-radius traversal, multi-repository federation, and MCP access.

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
the configured embedding endpoint; the default endpoint is local Ollama.

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

## Development

```sh
task check
task eval:resolution
```

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

### End-to-end corpus benchmark

The opt-in benchmark exercises the production parser registry, indexer, and
SQLite repository across cold, interrupted/resumed, unchanged, edit,
delete/restore, and branch-switch scenarios:

```sh
GRAFO_BENCH_REPO=~/CafecitoGames/uzir task bench:corpus
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

After changing a migration or query, run `task generate` and commit the updated
sqlc output.

Licensed under Apache-2.0.
