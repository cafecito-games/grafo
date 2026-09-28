# Catalog evidence benchmark

Issue #43 adds `BenchmarkCatalogEvidence`, a SQLite-backed catalog fixture with
10,000 unrelated incoming edges and one matching `reads` edge. Fixture creation
and reconciliation happen before the timer starts. The catalog request uses a
per-relation limit of one.

Measurements below used the same checkout, fixture, Go toolchain, CPU, and
command before and after the bounded hydrated relation-edge port:

```sh
GOMAXPROCS=8 go test ./internal/query ./internal/storage/sqlite \
  -run '^$' -bench 'BenchmarkCatalogEvidence' -benchmem -count=5
```

Environment: linux/amd64, AMD Ryzen 9 8945HS, integration base
`ee1388629c65e8b878e588a9a0509ed3d5eed205`.

| Version | Median ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| Before: unfiltered adjacency plus per-edge hydration | 17,841,825 | 9,982,803 | 130,197 |
| After: relation-filtered bounded joined hydration | 135,250 | 10,496 | 261 |

The median time improved 131.9×, allocated bytes 951.1×, and allocations
498.8×. The five before samples ranged from 17,744,557–17,982,057 ns/op; the
five after samples ranged from 133,586–138,579 ns/op. The benchmark reports
`10000 fixture_degree` in its output so future runs retain the workload size.
