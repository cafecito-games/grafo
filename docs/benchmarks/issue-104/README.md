# Issue 104 warm-status compatibility benchmark

The compatibility benchmark in `internal/cli/status_benchmark_test.go` was
byte-identical at both revisions (SHA-256
`32d7031ac3922dc9e187c1101f587338b39a400536fa5d2e7f24e5d8a4370933`). It
creates and indexes the same two-file non-Git Go fixture outside the timed
region, then times one warm `grafo status` call. Every timed call asserts the
normal status payload and zero buffered-stderr bytes from default `auto`
progress.

Revisions:

- compatibility base: `fdd487bd7c3ec66161684afafa2e10e7a3c7f8db`
- implementation: `869e6dcf1ee33907b5e13ba379d28eaa9af591af`

Command, run once in each checkout with no competing repository test process:

```sh
TMPDIR=/home/coder/.tmp-issue104 GOMAXPROCS=8 \
  go test ./internal/cli -run '^$' -bench '^BenchmarkWarmStatus$' \
  -benchtime=1x -count=30
```

The retained raw values are in `results.json`. The base median was
85.5211625 ms and the implementation median was 86.1245475 ms, a 0.7055%
regression. This passes the issue's maximum 10% median-regression gate. The
nearest-rank p95 values were 106.226674 ms (base) and 102.603493 ms
(implementation).

Environment: Go 1.26.6, Linux 6.8.0-137-generic amd64, AMD Ryzen 9 8945HS,
cgroup quota 8 CPUs and 20 GiB memory. The observed spread comes primarily
from the process-backed Git/project probes shared by both revisions; using the
median and an identical one-call harness keeps that cost represented on both
sides.
