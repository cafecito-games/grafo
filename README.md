# Grafo

Grafo turns a repository into a deterministic semantic graph that coding agents
can query from the command line. Source symbols, calls, imports, configuration,
HTTP routes, and event-like publish/subscribe operations become nodes and edges.

This repository is at the foundation stage. Go is parsed with the Go compiler
AST, Godot source formats use `gdparser`, Python and TypeScript/TSX use
Tree-sitter, and PostgreSQL SQL uses PostgreSQL's own parser. The index is
local, incremental, branch-aware, and stored in SQLite.

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
grafo source "HandleCheckout"
grafo watch
grafo mcp
```

`grafo install` detects Claude Code, Codex, and OpenCode on `PATH` and adds
Grafo to each one as a user-level MCP server. To configure only selected
agents, name them explicitly:

```sh
grafo install claude codex
```

The generated MCP configuration uses the absolute path of the installed Grafo
binary, so agents do not depend on their launch environment's `PATH`. Re-run
the command after moving the binary. Each repository still needs an initial
`grafo index .`; subsequent MCP queries refresh its active branch index
incrementally.

Every query supports `--json` for agent-friendly output. Run `grafo help` for
the complete command surface.

`grafo mcp` starts a standards-compatible MCP server over stdio with tools for
symbol discovery, node lookup, traversal, shortest paths, callers, callees,
blast radius, graph-addressed source retrieval, reusable-code discovery, and
index status.

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
- PostgreSQL tables, views, columns, indexes, functions, procedures, and the
  relations read or written by DDL and DML statements in `.sql`, `.pgsql`, and
  `.psql` files.
- Python modules, functions, classes, methods, fields, parameters, local
  variables, type annotations, basic assignment/argument/return flow, imports,
  calls, inheritance, environment reads, framework route decorators, outbound
  HTTP requests, and common publish/subscribe calls.
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
- Outbound Go `net/http`, Python Requests/HTTPX, and TypeScript `fetch`/Axios
  calls, linked to matching endpoint declarations locally or across repository
  boundaries.
- Deterministic symbol lookup, neighborhood traversal, shortest paths, callers,
  callees, blast-radius traversal, multi-repository federation, and MCP access.

Grafo currently performs syntactic and name-based linking. It does not yet do
type-checker-grade dispatch or full SSA data flow. Structural retrieval remains
graph-based; vector similarity is confined to optional candidate discovery.

## SQL dialects

One SQL router owns `.sql` files and delegates them to installed dialects.
PostgreSQL also owns the unambiguous `.pgsql` and `.psql` extensions. Plain
`.sql` remains configuration-free when exactly one installed dialect accepts
its syntax. When multiple dialects accept the same file, select one explicitly
in a repository-root `grafo.yaml` that is safe to commit:

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

The current distribution installs only the PostgreSQL dialect. The config
example's `sqlite` mapping becomes valid when a SQLite dialect is installed.

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

After changing a migration or query, run `task generate` and commit the updated
sqlc output.

Licensed under Apache-2.0.
