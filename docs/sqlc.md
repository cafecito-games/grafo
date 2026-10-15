# sqlc indexing

Grafo indexes supported sqlc templates as handwritten SQL, not as generated
Go. A named declaration such as `-- name: GetUser :one` becomes one
`sql_query` node whose canonical source and location remain in the `.sql` file.
Table and proven column `reads` and `writes` originate at that query node.
Generated Go methods can add a `generated_from` edge to it, so callers remain
traversable without making generated output a second source of SQL truth.

## Supported surface

| Surface | Supported |
|---|---|
| SQL engines | SQLite and PostgreSQL |
| Configuration | Repository-owned `sqlc.yaml`, `sqlc.yml`, and `sqlc.json`, version 2 |
| SQL entries | Nested configs, multiple entries, scalar or list schema/query paths, and file or directory inputs |
| Generation | Built-in `gen.go` package/output settings, prepared queries, and exported query constants |
| Templates | `sqlc.arg`, `sqlc.narg`, `sqlc.slice`, `sqlc.embed`, and `@name` shorthand |
| Named commands | `:one`, `:many`, `:exec`, `:execrows`, `:execlastid`, `:execresult`, `:copyfrom`, and `:batchexec`, `:batchone`, `:batchmany` |

Macro recognition is lexical. Text in comments, string literals, quoted
identifiers, and PostgreSQL dollar-quoted strings is left alone. Native
placeholders and operators remain native SQL. Grafo parses a same-length
dialect-specific representation internally so all reported byte spans, lines,
and columns still refer to the original source.

Indexing never runs sqlc, opens a database, executes SQL, loads a sqlc plugin,
or fetches a remote schema. Config paths must be local repository members;
absolute paths, traversal, and escaping symlinks are rejected.

## Dialect routing

The normal Grafo SQL selection remains authoritative. Precedence is:

1. the most-specific `grafo.yaml` `sql.paths` match;
2. a dialect-specific extension;
3. `sql.default_dialect`;
4. the engine of the unique owning sqlc entry;
5. grammar probing.

If explicit Grafo routing disagrees with the owning sqlc entry, Grafo reports
the mismatch and disables that entry's generated binding. It does not silently
reinterpret the SQL. A SQLite sqlc query therefore needs no PostgreSQL routing
workaround: remove the workaround and let the entry's `engine: sqlite` select
SQLite, or route it explicitly to SQLite.

## Binding proof

A generated Go method binds only when all of the following agree inside one
valid sqlc entry:

- configuration ownership of the query, schema, output directory, and package;
- the exact sqlc generated-file header and source header;
- the named query and generated method identity;
- the query constant actually referenced by that method; and
- the current handwritten SQL after dialect-aware placeholder normalization.

Missing, forged, stale, ambiguous, wrong-package, or unused evidence produces
no verified `generated_from` edge. Ordinary Go declarations and calls remain
available even when sqlc binding proof fails.

## Column evidence and uncertainty

Grafo emits table access whenever the dialect parser proves it. It emits a
column access only when the statement and configured schema identify one
candidate: qualified aliases, unique unqualified names, and explicit
insert/update targets are supported. `sqlc.embed` expands to columns only when
its table or alias has one complete configured schema declaration.

Ambiguous joins, duplicate schema declarations, incomplete wildcard evidence,
and CTE or view lineage remain explicit diagnostics or unresolved boundaries.
Grafo does not guess a column or claim that an unresolved query has no usage.
Data-usage queries, the CLI, and MCP all consume these same `reads` and
`writes` facts.

Generated-method source lookup presents the canonical handwritten SQL first
and includes the generated symbol as provenance. Looking up an explicit
generated node ID still returns the generated Go source.

## Deliberate limits

MySQL, version-1 configuration, custom config filenames, plugins, non-Go
generation, remote schema discovery, runtime SQL, and arbitrary view/CTE
lineage are not supported. Generator transformations beyond documented
placeholder, formatting, comment, and `sqlc.embed` expansion behavior fail
closed rather than producing a speculative binding.
