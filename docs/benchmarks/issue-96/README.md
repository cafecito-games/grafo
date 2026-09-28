# MCP freshness benchmark

Issue #96 compares the unconditional per-tool refresh at baseline
`60028383c88b0e1578ac3df31b8da396831543c3` with freshness generations at
corrected implementation head `f831ebb9914bde045740729a53fe5cf02188f2ae`.

The representative fixture is one immutable `git archive` of benchmark-candidate
head `f7aa246e146b97ef163347cff42c9be54e87beca`. The corrected implementation was
then measured against this unchanged corpus. Its sorted path-and-content manifest hashes to
`2f31814697653297045df9bf0e12813508e12c82a7ff412ef008c48f5fb9af7f`.
Both revisions received that exact tree, created a fresh Git repository, warmed
one in-memory MCP session, resolved `DiscoverProject` to an exact qualified
name, and timed 100 individual `get_node` calls. Every timed call asserted the
exact qualified-name payload. The baseline wrapper additionally proved at least
100 `SetMeta` and `Counts` calls; the current wrapper proved the generation
pointer and `indexed_at` remained unchanged.

```sh
# current, from f831ebb
GRAFO_MCP_BENCHMARK_CORPUS=/home/coder/review-artifacts/issue-96/frozen-corpus \
  TMPDIR=/home/coder/.tmp-issue96 GOMAXPROCS=8 \
  go test ./internal/mcpserver -run '^TestMCPFreshnessCorpusSamples$' -count=1 -v

# baseline, from a detached 60028383 worktree with the byte-identical
# compatibility harness (SHA-256 cc189e24...faaf2ef)
GRAFO_MCP_BENCHMARK_CORPUS=/home/coder/review-artifacts/issue-96/frozen-corpus \
  TMPDIR=/home/coder/.tmp-issue96 GOMAXPROCS=8 \
  go test ./internal/mcpserver -run '^TestMCPUnconditionalRefreshCorpusSamples$' -count=1 -v
```

| Representative Grafo corpus | Baseline | Current | Improvement |
| --- | ---: | ---: | ---: |
| Median, 100 warm calls | 105.138 ms | 9.309 ms | **11.29x** |
| p95 | 111.120 ms | 10.803 ms | 10.29x |

The representative result satisfies the issue's 10x median target after the
reviewed correction that also fingerprints semantic inputs hidden by Git ignore
rules. A second, deliberately tiny 64-file synthetic fixture exposes the fixed
cost honestly. A syscall-delay profile attributed 90.9%
of traced unchanged-call delay to the mandatory process-backed Git status probe
(mostly `waitid`), so tiny repositories reach that strong-freshness floor before
they can reach 10x. No TTL, watcher-only authority, or stale-success cache was
introduced to improve this number. `results.json` retains the original tiny-fixture
baseline samples and the final corrected current-side rerun; it does not retain the
superseded current-side samples. Host-contention outliers make the representative
corpus the acceptance authority.

Raw nanosecond samples, min/max values, exact commands, fixture hashes, and
machine/cgroup metadata are retained in [`results.json`](results.json). The
issue-prescribed `-benchtime=100x -count=10` scenario suite remains the changed,
batch, repeated-dirty, and federation regression benchmark; the explicit tests
above are used for the statistically literal 100 individual warm-call median.
