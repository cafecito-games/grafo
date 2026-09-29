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
```

The checked-in report is copied from the output only after verifying its
corpus/Grafo commits and clean corpus state. Databases are removed by default
after closure, compaction, and fingerprinting; pass `--keep-databases` only
when row-level investigation is needed. Scoped-full requires explicit
`GRAFO_DETAIL_FULL_ROOTS` and is never allowed to count #108 path exclusions as
fidelity savings.

One sample is enough to retain an honest raw observation but is not a strong
performance conclusion. A production recommendation requires at least three
fresh-process samples per profile under the same cgroup and filesystem.
