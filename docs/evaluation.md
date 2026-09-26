# Deterministic resolution corpus

The corpus under `internal/eval/testdata` records Grafo's parser-to-query
contract against small, traceable repositories. It uses the same parser
registry as the CLI, persists through SQLite reconciliation, and uses the
production query and federation services. The cases cover a local language
matrix, cross-language paths within a monorepo, and an HTTP path across two
federated repositories.

Run the gate with:

```sh
task eval:resolution
```

Every case is copied to an isolated temporary Git repository. The runner
indexes it once, performs an unchanged incremental refresh, then repeats the
work against a fresh database. Canonical nodes, edges, unresolved targets, and
ordered shortest paths must match byte-for-byte at each stage. Parser warnings
or errors fail the case.

## Manifest contract

Each case has a schema-versioned `manifest.json` and one or more repositories
under `repos/`. Repository directories must exactly match the manifest. The
loader rejects malformed JSON, duplicate keys, unknown fields, unsupported
schema versions, unknown node/edge kinds, duplicate case IDs, and paths that
escape the case before indexing begins.

Node selectors use repository ID, kind, qualified name, and external status;
they never record opaque graph IDs. Edge expectations also retain the source
path, line, column, and structural properties. The complete sorted `nodes` and
`edges` arrays are the golden graph. Query definitions record their selectors,
direction, and relation filter alongside the exact ordered nodes and edges
returned by the production shortest-path service.

Negative contracts belong in `ambiguities`, `forbidden_edges`, and
`forbidden_paths`. Forbidden-edge endpoints must each resolve exactly once.
Forbidden paths only pass on a real no-path result; a missing or ambiguous
endpoint fails so a negative expectation cannot silently disappear.

## Changing fixtures and expectations

Edit fixture source and negative/query definitions by hand. Normal tests never
rewrite expectations. After reviewing the intended structural change, update
goldens explicitly:

```sh
task eval:update
git diff -- internal/eval/testdata
task eval:resolution
task check
```

Update mode validates and evaluates every case completely before replacing any
manifest. Each replacement is written to a temporary file, flushed, and
renamed atomically. A malformed manifest, parser diagnostic, failed assertion,
or partial evaluation leaves the committed expectations untouched.

When adding a parser or supported dialect, add a compact positive fixture and
at least one production edge that proves its output survives persistence.
Prefer a negative assertion for syntax that might tempt name-based linking.
Keep files small enough that every changed edge can be traced to its recorded
source location.
