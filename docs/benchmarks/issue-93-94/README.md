# Refresh optimization benchmark

Issues #93 and #94 use one compatibility benchmark source at baseline
`60028383c88b0e1578ac3df31b8da396831543c3` and current
`001e947bbbb6acf66f260a9552caede30f1d19de`. The source hashes matched exactly;
the baseline checkout contained only that untracked benchmark file. Every timed
sample followed a complete warm-up index of the same generated 64-file Git
fixture. Each of the 30 samples reports the mean of 10 warm iterations.

```sh
GOMAXPROCS=8 go test ./internal/indexer -run '^$' \
  -bench 'BenchmarkCompat(Unchanged|QueryTriggered)Refresh$' \
  -benchtime=10x -count=30
```

| Scenario | Baseline median | Current median | Speedup |
| --- | ---: | ---: | ---: |
| Unchanged refresh (#93) | 15.405 ms/op | 3.529 ms/op | 4.36× |
| Project discovery + query-triggered refresh (#94) | 24.082 ms/op | 10.567 ms/op | 2.28× |

The current phase benchmark also reports Git-command count and Git,
membership, change-probe, and persistence nanoseconds separately:

```sh
GOMAXPROCS=8 go test ./internal/indexer -run '^$' \
  -bench 'Benchmark(Unchanged|QueryTriggered)Refresh$' -benchmem
```

On the recorded environment, the unchanged service refresh used one Git status
command after initial discovery. A fresh query-triggered refresh used two Git
commands after the remote-origin cache was warm; the first process-local lookup
uses three. Exact arguments and the three-command upper bound are asserted by
the injected-runner tests rather than inferred from timing.

The raw 30-sample arrays and cgroup/toolchain metadata are retained in
[`refresh-comparison.json`](refresh-comparison.json).
