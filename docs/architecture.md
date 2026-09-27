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

## Source retrieval

Source retrieval begins with normal deterministic node resolution; it never
searches file text. The node's repository identity and indexed location select
one active worktree and one bounded line span. Canonical path validation rejects
absolute paths, traversal, and symlinks that resolve outside the repository.

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
  and `StatusRepository` ports.
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
