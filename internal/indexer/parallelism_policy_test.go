package indexer_test

// Test parallelism policy for this package.
//
// Every test here builds a fixture repository and drives a full indexing
// pipeline over it, so the package's runtime is the sum of its cases unless
// they overlap. They can: each one owns a testtemp.Dir of its own, discovers
// its project from that root, and writes its index under it, so no two tests
// share a repository root, a SQLite file, or a persisted Go semantic view
// cache (that cache lives under the fixture's own .grafo directory). Nothing
// in the package keeps mutable package-level state.
//
// So every test calls t.Parallel() as its first statement, with exactly two
// exceptions:
//
//   - TestServiceReindexesGoDependentsWhenTypeEvidenceChanges calls
//     t.Setenv("GOOS", ...) to prove a build-context change invalidates Go
//     semantic evidence. The environment is process-wide and the Go package
//     loader reads GOOS when it derives its workspace key, so a concurrent
//     sibling indexing a Go module would observe the mutation. It stays
//     serial: Go runs every top-level serial test to completion before it
//     resumes any parallel one, which is what keeps the mutation invisible to
//     the 117 parallel ones. A parallel sibling needing a different GOOS would
//     have to take it through indexer.Options or an injected build context
//     instead of the process environment.
//
//   - TestColdIndexDerivesEachScopeKeyOncePerFile reads a process-wide counter
//     (golang.ScopeWorkCounts) before and after one cold index and asserts the
//     difference. Any concurrent test that indexes Go files inflates that
//     difference, so the measurement is only meaningful while nothing else
//     runs. It stays serial for the same reason the test above does.
//
// Parse workers inside one Service are bounded by indexer.Options, and
// internal/parser/golang gates workspace loads process-wide, so the parallel
// binary does not multiply those budgets by the number of running tests.
