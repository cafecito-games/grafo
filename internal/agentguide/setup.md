# Set up a repository for Grafo

Configure the current repository for the strongest graph coverage Grafo can
prove. Treat source and build configuration as evidence; do not guess runtime
boundaries or invent `grafo.yaml` fields.

## 1. Establish scope and preserve user work

1. Resolve the repository root and read its agent instructions.
2. Inspect `git status` and the existing root `grafo.yaml`, if any. Preserve
   unrelated edits, comments, ordering, and unknown top-level sections.
3. Run `grafo version`, then `grafo index .` to establish a baseline. If
   indexing fails, diagnose that failure before changing configuration.

## 2. Discover configuration evidence

Inspect manifests, workspace files, deployment entry points, source roots,
code-generation configuration, SQL files, and application wrappers. Use Grafo's
graph when the baseline index supports the question and native search for build
files, documentation, unsupported code, and other content outside the graph.

Classify findings as follows:

- **Components:** declare deployable or runtime boundaries in a monorepo, not
  every package or source directory. Require disjoint repository-relative roots.
- **SQL:** configure a default or path mapping only when `.sql` dialect selection
  is ambiguous or the repository has explicit PostgreSQL/SQLite ownership. An
  ambiguity proves that a choice is needed, not which dialect to choose; require
  driver, migration, deployment, or project documentation evidence for that.
- **HTTP wrappers:** declare only exact GDScript wrapper callables whose method
  and URL arguments can be proven. Other supported HTTP clients are automatic.
- **Tests:** declare additional GDScript test base classes only when project
  dependencies and test source prove the exact class name. `GutTest` is built in.
- **Transport and serialization:** Protobuf bindings/message use and supported
  ENet APIs are automatic. Inspect `grafo message-coverage` and `grafo
  message-flow`; do not add transport or serialization keys to `grafo.yaml`.
  Report unsupported libraries or dynamic evidence as coverage gaps.

Do not create `grafo.yaml` when the repository needs no supported setting.

## 3. Edit only the supported schema

Use only the sections that the evidence requires:

```yaml
components:
  - name: client
    roots: [apps/client]
  - name: api
    roots: [apps/api]

http:
  request_apis:
    - language: gdscript
      symbol: AuthAPI.request_json
      method_argument: 0
      url_argument: 1

sql:
  default_dialect: postgres
  paths:
    "desktop/**/*.sql": sqlite
    "migrations/**/*.sql": postgres

tests:
  gdscript_bases: [SpecBase, addons.gut.CustomBase]
```

Component names must be unique and use letters, digits, `.`, `_`, or `-`.
Roots must use `/`, stay within the repository, contain no globs, and never
overlap. SQL dialects are `postgres` and `sqlite`; path mappings use slash-based
glob patterns. HTTP argument positions are zero-based, distinct, non-negative,
and `route_argument` is an alias for `url_argument`. Test bases must be exact
GDScript class names; do not list frameworks, paths, or ordinary test classes.

If the requested integration cannot be represented by this schema, leave the
configuration valid and state the limitation with source evidence.

## 4. Validate and report

1. Run `grafo index .` again. A successful run is the configuration validator;
   do not claim setup succeeded when it reports diagnostics or ambiguity.
2. Run `grafo status .` and the relevant coverage queries: `grafo
   service-topology`, `grafo endpoints`, `grafo outbound-requests`, `grafo
   message-coverage`, or a focused `grafo message-flow <message>`.
3. Summarize the settings added or preserved, the evidence for each, before and
   after coverage when measurable, and every unsupported or unresolved gap.
