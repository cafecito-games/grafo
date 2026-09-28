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
code-generation configuration, SQL files, and repository-owned application
wrappers. Use Grafo's graph when the baseline index supports the question and
native search for build files, documentation, unsupported code, wrapper bodies,
and other content outside the graph.

Classify findings as follows:

- **Components:** declare deployable or runtime boundaries in a monorepo, not
  every package or source directory. Require disjoint repository-relative roots.
- **SQL:** configure a default or path mapping only when `.sql` dialect selection
  is ambiguous or the repository has explicit PostgreSQL/SQLite ownership. An
  ambiguity proves that a choice is needed, not which dialect to choose; require
  driver, migration, deployment, or project documentation evidence for that.
- **Semantic application wrappers:** look for high-fanout helpers around
  supported event or HTTP primitives. Graph callers and call counts can identify
  candidates, but prove every adapter from the exact helper declaration, body,
  and representative callsites. A suggestive name, comment, or popularity is
  never proof. Built-in supported APIs remain automatic.
- **Tests:** declare additional GDScript test base classes only when project
  dependencies and test source prove the exact class name. `GutTest` is built in.
- **Transport and serialization:** Protobuf bindings/message use and supported
  ENet APIs are automatic. Inspect `grafo message-coverage` and `grafo
  message-flow`; do not add transport or serialization keys to `grafo.yaml`.
  Report unsupported libraries or dynamic evidence as coverage gaps.

Do not create `grafo.yaml` when the repository needs no supported setting.

### Audit call-effect adapters

Before editing configuration, record a baseline successful index and the
relevant `grafo events`, `grafo orphaned-events`, `grafo godot interactions`,
`grafo outbound-requests`, or `grafo service-topology` result. Inspect existing
root `grafo.yaml` adapters and current diagnostics as well as source. Existing
HTTP coverage may use the legacy `http.request_apis` compatibility alias;
prefer `adapters` for new declarations, preserve a proven legacy declaration,
and never declare the same symbol through both forms because configuration
validation rejects the duplicate.

The installed V1 adapter matrix is deliberately closed: only `gdscript` with
`event.publish`, `event.subscribe`, `event.unsubscribe`,
`event.connection_test`, or `http.request` is supported. `event.publish` takes
the `event` role; subscribe, unsubscribe, and connection-test effects take
`event` and `handler`; HTTP requests take `method` and `url`. Match an exact qualified symbol
and map each required role to a proven zero-based argument.
Unsupported languages, effects, roles, dynamic or reflective dispatch, and
conditional or conflicting semantics are limitations to report—not fields to
invent. When multiple declared effects are independently proven, each may be
listed; otherwise leave the helper unconfigured.

An adapter is repository-owner evidence, not a wildcard plugin mechanism and
not permission to emit arbitrary graph relations. If an existing adapter no
longer agrees with its helper, report it and remove or correct it only when the
user authorized repository setup or configuration changes.

## 3. Edit only the supported schema

Use only the sections that the evidence requires:

```yaml
components:
  - name: client
    roots: [apps/client]
  - name: api
    roots: [apps/api]

adapters:
  - match:
      language: gdscript
      symbol: Signals.wire
    effects:
      - kind: event.subscribe
        roles:
          event: {argument: 0}
          handler: {argument: 1}
  - match:
      language: gdscript
      symbol: API.fetch
    effects:
      - kind: http.request
        roles:
          method: {argument: 1}
          url: {argument: 0}

sql:
  default_dialect: postgres
  paths:
    "desktop/**/*.sql": sqlite
    "migrations/**/*.sql": postgres

tests:
  gdscript_bases: [SpecBase, addons.gut.CustomBase]
```

The standalone adapter fragment above is also validated by Grafo's own tests:

<!-- BEGIN adapter-example -->
```yaml
adapters:
  - match:
      language: gdscript
      symbol: Signals.wire
    effects:
      - kind: event.subscribe
        roles:
          event: {argument: 0}
          handler: {argument: 1}
  - match:
      language: gdscript
      symbol: API.fetch
    effects:
      - kind: http.request
        roles:
          method: {argument: 1}
          url: {argument: 0}
```
<!-- END adapter-example -->

Component names must be unique and use letters, digits, `.`, `_`, or `-`.
Roots must use `/`, stay within the repository, contain no globs, and never
overlap. SQL dialects are `postgres` and `sqlite`; path mappings use slash-based
glob patterns. Adapter argument positions are zero-based, distinct, and
non-negative. Adapter symbols are exact; do not use wildcards or approximate a
similar helper. Test bases must be exact GDScript class names; do not list
frameworks, paths, or ordinary test classes.

If the requested integration cannot be represented by this schema, leave the
configuration valid and state the limitation with source evidence.

## 4. Validate and report

1. Run `grafo index .` again. A successful reindex is the configuration
   validator; do not claim setup succeeded when it reports diagnostics or
   ambiguity. Restore or fix only the attempted in-scope edit on failure.
2. Run `grafo status .` and the relevant coverage queries: `grafo
   service-topology`, `grafo endpoints`, `grafo outbound-requests`, `grafo
   message-coverage`, or a focused `grafo message-flow <message>`.
3. Compare the relevant baseline and post-index query. An adapter that leaves
   expected evidence unresolved or truncated has not proved complete coverage.
4. Summarize the settings added, corrected, removed, or preserved; the source
   evidence for each; before and after coverage when measurable; and every
   unsupported or unresolved gap. Re-running this workflow on an already valid,
   source-proven adapter should make no repository change.
