# Issue 113 detail-profile benchmark

This directory holds versioned raw evidence for the capability-safe
reduced-detail spike. The source of truth is the JSON report emitted by the
benchmark; prose does not replace raw samples.

Run from a clean committed Grafo worktree, with the clean pinned Uzir checkout
and output on the persistent home filesystem:

```sh
GRAFO_BENCH_REPO=/home/coder/workspace/uzir \
GRAFO_BENCH_OUTPUT=$HOME/grafo-detail-profiles/issue-113 \
GRAFO_DETAIL_PROFILE=full \
GRAFO_DETAIL_SAMPLES=1 \
task bench:detail-profiles

GRAFO_BENCH_REPO=/home/coder/workspace/uzir \
GRAFO_BENCH_OUTPUT=$HOME/grafo-detail-profiles/issue-113 \
GRAFO_DETAIL_PROFILE=structural-v1 \
GRAFO_DETAIL_BASELINE=$HOME/grafo-detail-profiles/issue-113/full-report.json \
GRAFO_DETAIL_SAMPLES=1 \
task bench:detail-profiles

GRAFO_BENCH_REPO=/home/coder/workspace/uzir \
GRAFO_BENCH_OUTPUT=$HOME/grafo-detail-profiles/issue-113 \
GRAFO_DETAIL_PROFILE=scoped-full-v1 \
GRAFO_DETAIL_FULL_ROOTS='apps/api/**' \
GRAFO_DETAIL_BASELINE=$HOME/grafo-detail-profiles/issue-113/full-report.json \
GRAFO_DETAIL_SAMPLES=1 \
task bench:detail-profiles
```

The checked-in `results.json` preserves exact provenance, counts, phase/resource
measurements, raw-report SHA-256 digests, capability comparison, and the scoped
failure from the much larger emitted reports. The original reports remain at
the recorded persistent paths. Databases are removed by default
after closure, compaction, and fingerprinting; pass `--keep-databases` only
when row-level investigation is needed. Scoped-full requires explicit
`GRAFO_DETAIL_FULL_ROOTS` and is never allowed to count #108 path exclusions as
fidelity savings.

One sample is enough to retain an honest raw observation but is not a strong
performance conclusion. A production recommendation requires at least three
fresh-process samples per profile under the same cgroup and filesystem.

The retained 2026-09-29 run uses one fresh-process sample for each completed
profile. `structural-v1` cleared the size and resource gates but failed exact
`catalogs_topology` equivalence. `scoped-full-v1` with `apps/api/**` failed
closed before a report because aggregate resolution changed a retained
`wraps_error` locator from one candidate to zero. These correctness failures
make the recommendation full-only regardless of sample-count uncertainty.
