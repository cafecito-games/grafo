# Repository Guidelines

## Project Structure & Module Organization

`cmd/grafo/` contains the CLI entry point. Most code lives under `internal/`: `parser/` holds language and configuration extractors, `graph/` defines the shared model and repository interfaces, `indexer/` coordinates incremental indexing, and `query/`, `federation/`, `semantic/`, and `mcpserver/` expose graph capabilities. SQLite persistence lives in `internal/storage/sqlite/`; edit SQL in `queries/` and Goose migrations in `migrations/`, while `sqlcgen/` is generated. Architecture notes belong in `docs/`. Tests are colocated with packages as `*_test.go`.

## Build, Test, and Development Commands

Use Taskfile targets; do not add a Makefile.

- `task build` builds `./cmd/grafo`.
- `task test` runs `go test ./...`.
- `task vet` runs Go static analysis.
- `task generate` regenerates the SQLite repository with sqlc.
- `task check` generates, tests, vets, and builds; run it before submitting.
- `go run ./cmd/grafo index /path/to/repo` runs the CLI locally.

After editing migrations or queries, run `task generate` and commit the resulting `sqlcgen` changes. Optional GDScript corpus coverage uses `GRAFO_GDSCRIPT_CORPUS=/path/to/project go test ./internal/parser/gdscript -run TestCorpus -count=1`.

## Coding Style & Naming Conventions

Target Go 1.26 and format Go files with `gofmt`. Keep packages lowercase and focused; exported identifiers use Go-style `CamelCase`, while filenames remain descriptive lowercase. Depend on interfaces in `internal/graph` instead of concrete storage adapters. Preserve deterministic ordering and stable graph IDs. Unsupported or ambiguous syntax must produce diagnostics or unresolved nodes, never guessed edges.

## Testing Guidelines

Use Go's standard `testing` package and name tests `TestBehavior`. Add table-driven cases for parsers and resolution rules, including malformed input and ambiguity. Storage changes must test transactions, restart/idempotency, and incremental reconciliation. Run focused package tests during development and `task check` before opening a PR; use `go test -race ./internal/...` for concurrency-sensitive changes.

## Commit & Pull Request Guidelines

History follows Conventional Commit-style subjects such as `feat: add Python parser support` and `perf: bound large graph reconciliation`. Keep commits scoped and imperative. PRs should explain the problem, implementation, verification commands, compatibility or migration impact, and link the issue. Include before/after measurements for performance work; screenshots are needed only for user-visible UI changes.

## Security & Generated Data

Do not commit `.grafo/indexes/`, corpus data, secrets, or local `.env` values. Indexing must remain local unless a feature explicitly documents external metadata transfer. Never hand-edit generated `internal/storage/sqlite/sqlcgen/*.go` files.
