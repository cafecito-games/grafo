# Grafo

Grafo turns a repository into a deterministic semantic graph that coding agents
can query from the command line. Source symbols, calls, imports, configuration,
HTTP routes, and event-like publish/subscribe operations become nodes and edges.

This repository is at the foundation stage. Go is parsed with the Go compiler
AST, TypeScript/TSX with Tree-sitter, and PostgreSQL SQL with PostgreSQL's own
parser. The index is local, incremental, branch-aware, and stored in SQLite.

[Architecture and extension points](docs/architecture.md)

## Quick start

```sh
go install ./cmd/grafo
grafo index .
grafo status
grafo find "MyHandler"
grafo neighbors "MyHandler" --depth 2
grafo path "HandleCheckout" "Charge"
grafo watch
grafo mcp
```

Every query supports `--json` for agent-friendly output. Run `grafo help` for
the complete command surface.

`grafo mcp` starts a standards-compatible MCP server over stdio with tools for
symbol discovery, node lookup, traversal, shortest paths, callers, callees,
blast radius, and index status.

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
- `.env`, YAML, JSON, and Java `.properties` keys and value references.
- Outbound Go `net/http` and TypeScript `fetch`/Axios calls, linked to matching
  endpoint declarations locally or across repository boundaries.
- Deterministic symbol lookup, neighborhood traversal, shortest paths, callers,
  callees, blast-radius traversal, multi-repository federation, and MCP access.

Grafo currently performs syntactic and name-based linking. It does not yet do
type-checker-grade dispatch, full SSA data flow, or embedding-based natural
language candidate discovery. Structural retrieval remains graph-based.

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

Index files live under `.grafo/indexes/` and should not be committed. Source
never leaves the machine. Configuration values are used transiently to discover
references such as `${DATABASE_HOST}` but are not persisted, so indexing an
`.env` file does not copy its secrets into the graph.

## Development

```sh
task check
```

After changing a migration or query, run `task generate` and commit the updated
sqlc output.

Licensed under Apache-2.0.
