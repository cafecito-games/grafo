# Architecture

Grafo is split into domain, use-case, and adapter layers so language support and
storage can evolve independently.

```text
cmd/grafo
  └─ internal/cli          composition and presentation
       ├─ internal/indexer changed-file indexing use case
       └─ internal/query   deterministic traversal use case
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
3. A parser emits declaration nodes and unresolved relationship facts. It never
   talks to the database.
4. The repository transactionally replaces the changed file's nodes and facts.
5. Reconciliation deterministically resolves facts against declarations and
   materializes adjacency-indexed edges. When evidence is insufficient, the
   target remains an explicit `external` node rather than a guessed edge.
6. Queries walk ordered incoming or outgoing adjacency lists. Node IDs, edge
   IDs, ordering, and breadth-first tie-breaking are stable.

Reconciliation currently scans facts after changed files are parsed. This is
not a source re-index: unchanged files never re-enter a parser. A later storage
migration can add a dirty-target table to make edge reconciliation local too,
without changing parser or query contracts.

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
- Cross-repository federation should namespace stable IDs by repository and
  merge resolved protocol/config facts, not copy source text between indexes.
- Embeddings belong in a candidate-source port for natural-language discovery;
  they must not change structural edge traversal or ordering.
