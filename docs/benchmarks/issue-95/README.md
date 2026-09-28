# Issue 95 query-only SQLite benchmark

These measurements were collected on the integrated issue branch at code commit
`9dbbecfea99759c8da1b847ca1d3cd8def3d5cb2`, based on
`d4e03c3fee41cc735187b5e2805ca415c66ba343`. The Coder workspace exposed an
8-CPU, 20 GiB cgroup on Linux/amd64 with Go 1.26.6 and an AMD Ryzen 9 8945HS.

The open benchmark used 100 independent samples of 10 warm iterations:

```sh
GOMAXPROCS=8 go test ./internal/storage/sqlite -run '^$' \
  -bench '^BenchmarkRepositoryOpenAndExactNode$' -benchtime=10x -count=100
```

The query-only median was 428,202.5 ns/op, versus 1,480,748 ns/op for writable
`Open`: a 3.458x speedup. This exceeds the required 2x median improvement.

Representative steady-state queries used 30 independent samples of 100 warm
iterations:

```sh
GOMAXPROCS=8 go test ./internal/storage/sqlite -run '^$' \
  -bench '^BenchmarkReadOnlyRepresentativeQueries$' -benchtime=100x -count=30
```

| Query | Query-only median | Writable median | Query-only change |
| --- | ---: | ---: | ---: |
| Exact node | 10,471 ns/op | 10,083 ns/op | +3.8% |
| Symbol match | 61,862.5 ns/op | 62,823.5 ns/op | -1.5% |
| Callers | 11,175 ns/op | 13,057 ns/op | -14.4% |
| Bounded path | 154,499 ns/op | 168,479 ns/op | -8.3% |

The only slower representative median is exact-node lookup at 3.8%, which is
not a material regression. The raw observations and environment evidence are
preserved in `results.json`.
