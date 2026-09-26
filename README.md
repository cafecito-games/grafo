# Grafo

Grafo turns a repository into a deterministic semantic graph that coding agents
can query from the command line. Source symbols, calls, imports, configuration,
HTTP routes, and event-like publish/subscribe operations become nodes and edges.

This repository is at the foundation stage. Go is parsed with the Go compiler
AST and TypeScript/TSX with Tree-sitter. The index is local, incremental,
branch-aware, and stored in SQLite.

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

## What the first slice understands

- Go packages, functions, methods, types, fields, parameters, local variables,
  imports, calls, basic assignment/argument/return flow, embedding,
  `os.Getenv`/`LookupEnv`, `net/http` routes, and common publish/subscribe calls.
- TypeScript and TSX modules, functions, classes, interfaces, methods,
  parameters, local variables, basic assignment/argument/return flow, imports,
  calls, inheritance, `process.env`, Express-style routes, and common
  publish/subscribe calls.
- `.env`, YAML, JSON, and Java `.properties` keys and value references.
- Deterministic symbol lookup, neighborhood traversal, shortest paths, callers,
  callees, and blast-radius traversal.

Grafo currently performs syntactic and name-based linking. It does not yet do
type-checker-grade dispatch, SSA data flow, embeddings, cross-repository graph
federation, or expose an MCP server. Those belong in later layers; the storage,
parser, fact-resolution, and traversal boundaries are designed for them.

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
