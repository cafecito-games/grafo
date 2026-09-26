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
       ├─ gdscript/*       gdparser typed AST adapter
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
3. A parser emits declaration nodes and unresolved relationship facts. It never
   talks to the database.
4. The repository transactionally replaces the changed file's nodes and facts.
5. Reconciliation deterministically resolves only dirty facts against
   declarations and materializes adjacency-indexed edges. When evidence is
   insufficient, the target remains an explicit `external` node rather than a
   guessed edge.
6. Queries walk ordered incoming or outgoing adjacency lists. Node IDs, edge
   IDs, ordering, and breadth-first tie-breaking are stable.

Dirty-target tracking limits edge reconciliation to facts affected by changed
declarations or owners. Unchanged files do not re-enter a parser.

For Git worktrees, the indexer narrows content hashing to files changed since
the indexed commit, current untracked files, and paths that were dirty during
the previous run. Remembering the previous dirty set closes the restore case:
if a dirty file was indexed and then restored to `HEAD`, it is still checked
once before leaving that set. Parser-level semantic dependencies propagate
configuration changes (for example `grafo.yaml`) to otherwise unchanged source
files. Any Git detection failure safely falls back to hashing every supported
file; non-Git directories always use that fallback.

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

- Add a language by implementing `parser.Parser` and registering it in the CLI
  composition root.
- Add a SQL dialect by implementing `sql.Dialect` under `internal/parser/sql`
  and passing it to the single SQL router in the CLI composition root. A
  dialect declares only its unambiguous extensions; `.sql` always belongs to
  the router. Dialects emit the shared table/view/column/index graph vocabulary,
  while the router records the selected dialect in node metadata.
- Add storage by implementing the small `IndexRepository`, `QueryRepository`,
  and `StatusRepository` ports.
- Add new relationships as facts first; keep name/type resolution in the
  reconciliation stage so parsers remain syntactic.
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
