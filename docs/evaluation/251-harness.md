# Reproducing an investigation evaluation

The harness uses actual registered MCP handlers over an in-memory transport and an explicitly indexed local source checkout. It runs no freshness scan. Source and indexes must remain local; put corpora, transcripts and answers outside the repository. It requires a graph index compatible with the binary being tested.

## Preserve comparable snapshots

Build and retain the baseline harness binary before production changes. Preserve its index unchanged. Clone that completed index for the candidate and apply only candidate schema migrations/backfill to the clone. Retain the same clean source commit for both. Record binary hashes, source commit, schema versions, corpus hashes, prompts and model settings locally. Verify original graph nodes/edges/file records agree after migration; derived lexical postings are allowed to differ. Do not silently rebuild one arm against a newer source tree.

```sh
go build -o /tmp/evaluation-candidate ./docs/evaluation/harness
GRAFO_EVALUATION_INDEX=/tmp/candidate-index.sqlite GRAFO_EVALUATION_PREPARE=1 /tmp/evaluation-candidate /path/to/local/source
```

Compare the preserved original tables, including semantic metadata and file records, with read-only deterministic row digests:

```sh
python3 docs/evaluation/harness/compare_indexes.py /tmp/baseline-index.sqlite /tmp/candidate-index.sqlite --output /tmp/index-equivalence.json
```

This excludes migration bookkeeping, candidate-only derived tables, and the `lexical_version` metadata key. It compares all other original table columns and rows, not just counts. Digest time is preparation/audit overhead, not query latency.

Preparation explicitly requires an index override and opens that index writable. Regular operation always opens read-only. Preparation duration, index size growth and freshness costs are separate measurements. Never set PREPARE for an investigation or timing replay. The Python drivers clear inherited preparation/index environment settings and require an explicit `--index` for an override.

### Rebuilding the baseline arm

This harness calls `WithInvestigationSource`, which does not exist at the
integration base `96b20b7d3dbe2c6c51b9d8b35e5e4e988c617adf`, so the committed
file cannot compile there. The frozen baseline binary recorded in the local
freeze manifest remains the actual comparator for every published measurement.
The recipe below exists only to audit that comparator or to reconstruct a
baseline arm if the frozen binary is lost.

```sh
git worktree add --detach /tmp/grafo-baseline-arm 96b20b7
mkdir -p /tmp/grafo-baseline-arm/docs/evaluation/harness
cp docs/evaluation/harness/main.go /tmp/grafo-baseline-arm/docs/evaluation/harness/main.go
```

Remove exactly the investigation opt-in, which is the only base-incompatible
reference, together with the trailing period that chains it to the preceding
call:

```sh
python3 - <<'PY'
path = "/tmp/grafo-baseline-arm/docs/evaluation/harness/main.go"
text = open(path).read()
target = ".\n\t\tWithInvestigationSource(source.NewScopedService)"
assert text.count(target) == 1
open(path, "w").write(text.replace(target, ""))
PY
cd /tmp/grafo-baseline-arm && go build -o /tmp/evaluation-baseline-rebuilt ./docs/evaluation/harness
```

That single removal is the complete delta; `source` stays imported for
`source.NewService`, and no other symbol in the file is newer than the base.
Confirm the result is a genuine baseline arm, which must not expose the tool
under test, and that a real baseline handler still answers:

```sh
printf '%s\n' '{"name":"investigate","arguments":{"query":"any"}}' \
  | GRAFO_EVALUATION_INDEX=/tmp/baseline-index-clone.sqlite /tmp/evaluation-baseline-rebuilt /path/to/local/source
# expected: {"error":"calling \"tools/call\": unknown tool \"investigate\"",...}
printf '%s\n' '{"name":"find_symbols","arguments":{"query":"AnyName"}}' \
  | GRAFO_EVALUATION_INDEX=/tmp/baseline-index-clone.sqlite /tmp/evaluation-baseline-rebuilt /path/to/local/source
```

Read against a clone, so the preserved baseline index stays byte-identical, and
point a base-commit binary only at a schema-14 index, never at a migrated
candidate clone.

A rebuilt binary is not hash-identical to the frozen one and is not a
substitute for it. The frozen baseline predates the index override, so it reads
the source worktree's original index path and ignores `GRAFO_EVALUATION_INDEX`;
preserve that path, or restore the preserved index clone there, when recreating
the source worktree. A rebuild also differs in build time and toolchain
provenance. Reported baseline numbers therefore stay attributed to the frozen
binary.

## Calls and transcripts

The process accepts newline-delimited JSON objects with `name` and `arguments`, emitting a response envelope and handler milliseconds for each line. A malformed wrapper request returns a recorded error; an MCP tool error stays an MCP tool error. It does not synthesize matches or suppress response fields.

```sh
python3 docs/evaluation/harness/record.py --binary /tmp/evaluation-candidate --repository /path/to/local/source --index /tmp/candidate-index.sqlite --transcript /tmp/evaluation-trial/transcript.jsonl --case example --request '{"name":"find_symbols","arguments":{"queries":["Checkout"],"limit":10}}'
```

For a blinded agent trial, provide the fixed questions/scopes and tool documentation only. Do not provide gold, prior transcripts, or evaluation scores. Require all source acquisition through the measured tools, retain errors and fallbacks, and record cited final answers separately. Candidate agents may use the new tool plus the identical existing tool allowance. Any direct shell search must be recorded and accounted for or the trial is invalid. Keep answer obligations and scoring independent from candidate relevance rankings.

## Cost aggregation

Install `tiktoken` in a local Python environment, then run:

```sh
python3 docs/evaluation/harness/aggregate.py /tmp/evaluation-trial/transcript.jsonl
```

This counts UTF-8 bytes and `cl100k_base` proxy tokens for every text content block, once. It excludes the duplicate structuredContent representation but preserves duplication inside the returned text itself. Prompt, reasoning, billing, orchestration-wrapper and final-answer tokens are not included. Unknown-tool errors from a nonempty tool name count as valid attempts; malformed empty-name wrapper requests are separately excluded from valid-call efficiency. Both attempted and valid calls are reported. Source/audit correctness still requires human or independent evaluator review; this script does not grade answers.

## Controlled latency replay

Stop indexing, tests and other substantial competing work before replay. Use the frozen baseline binary with its original index, and the candidate binary with its migrated clone. Run arms sequentially, not concurrently. The same completed transcript is replayed three times for timing stability; these are not three independent agent trials.

```sh
python3 docs/evaluation/harness/replay.py --binary /tmp/evaluation-baseline --repository /path/to/local/source --output /tmp/baseline-timing.json /tmp/baseline-trial/transcript.jsonl
python3 docs/evaluation/harness/replay.py --binary /tmp/evaluation-candidate --repository /path/to/local/source --index /tmp/candidate-index.sqlite --output /tmp/candidate-timing.json /tmp/candidate-trial/transcript.jsonl
```

Handler time excludes process startup and index opening; recording also reports wrapper wall time separately. No filesystem cache flush is performed. Audit replay errors before comparing medians. Compute investigate p95 with a declared percentile convention (nearest rank), and maximum across all its replay calls. Use the frozen acceptance document for quality, efficiency, latency and work-bound gates rather than selecting favorable metrics after seeing results.
