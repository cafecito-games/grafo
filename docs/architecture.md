# Architecture

Grafo is split into domain, use-case, and adapter layers so language support and
storage can evolve independently.

```text
  cmd/grafo
  └─ internal/cli          composition and presentation
       ├─ internal/indexer changed-file indexing use case
       ├─ internal/query   deterministic traversal use case
       ├─ internal/mcpserver agent-facing stdio tools
       └─ internal/federation read-only multi-index graph
            │
      internal/graph       nodes, edges, facts, narrow repository ports
            ▲
            │
  internal/parser/*        Go AST, TypeScript Tree-sitter, config mappers
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

## Federation

Every declaration ID includes its repository identity. A federation opens the
current branch index for each requested worktree and presents them through the
same read-only repository port used by normal traversal. When an explicit
external fact exactly matches a compatible declaration in a peer index, the
adapter synthesizes a deterministic edge marked `federated=true`. It neither
copies databases nor guesses from similarity. Both one-shot CLI queries and
long-running MCP tool calls incrementally refresh their indexes first.

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
- Add storage by implementing the small `IndexRepository`, `QueryRepository`,
  and `StatusRepository` ports.
- Add new relationships as facts first; keep name/type resolution in the
  reconciliation stage so parsers remain syntactic.
- Embeddings belong in a candidate-source port for natural-language discovery;
  they must not change structural edge traversal or ordering.
