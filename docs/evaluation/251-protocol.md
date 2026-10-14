# Investigation evaluation protocol (#251)

## Corpus and separation

Uzir source and index are local at commit `8bee8ac48b63f231e3ee89fb8d335902a5e33632`. Development consists of the original nine spike discovery questions plus three focused behavior questions covering TypeScript polling, TypeScript snapshot validation, and Go chat normalization. The local `/tmp/grafo-251-evaluation/development.json` adds source-verified answer obligations; corpus data is not committed. These are minimum useful answer claims, not permission to invent additional behavior.

Ten held-out questions and source-verified expected claims are held by the evaluator outside the repository. They span Go, TypeScript and GDScript, seven focused positive investigations, one ambiguous investigation with two plausible implementations, one missing-evidence control, and one scope-boundary control. Questions and answers remain hidden from implementation until final evaluation. Once evaluated they cannot become tuning cases without a new untouched holdout cohort.

## Existing-tool comparator

A local Go adapter opens the completed SQLite index read-only, registers the real MCP service with source and search services, and calls the actual MCP handlers using SDK in-memory transports. It omits freshness scanning equally for all arms. It preserves batching, error envelopes, scalar duplication, returned source bounds, and actual existing retrieval behavior. No embedding provider is configured. Startup/freshness/indexing costs are separate from measured handler latency.

An evaluator-authored control exercises all 22 questions. Selection is source/gold-assisted, with actual discovery/fallback calls retained. It is a diagnostic, **not a blinded agent trial**. It found all expected target sets after one additional qualified-name follow-up on the development paraphrase. Across development it made 35 calls and returned 50,997 `cl100k_base` proxy tokens; held-out totals were 34 calls and 64,351 proxy tokens. These totals cannot establish actual agent performance.

The primary comparison uses fresh independent investigators given questions and scope only. They cannot read gold, prior transcripts, spike answers, or source outside the measured tools. Each question allows at most eight MCP calls, with independent queries/sources batched where useful. Existing tools include symbol finding, bounded source, graph relationships, test evidence, and scoped content search. Investigators adapt after each result and write cited final answers with explicit limitations. Two fresh passes are planned per arm. Candidate-arm investigators receive `investigate` plus the same existing tools and fallback rights, with a 24,000-byte cap on each investigation response.

## Scoring and accounting

For each case/pass, the evaluator checks every required claim against source, then checks additional material answer claims against returned evidence. A positive answer is complete only when all required claims have support; correctly saying evidence is insufficient does not pass a positive behavior question. Ambiguity and negative controls require proper boundaries, not repository-wide absence assertions. A source excerpt can prove a direct call without a graph edge; a tool relevance score cannot prove a relationship. Structural test paths do not establish execution coverage.

Report complete positive answers, supported required claims, unsupported material claims, control outcomes, calls, response bytes, and response-token proxy totals. Token counts use `tiktoken` `cl100k_base` over MCP text content once; the duplicate MCP `structuredContent` representation is not counted a second time. Duplication within the returned text itself is counted. Prompts, reasoning, and provider-billed model tokens are unavailable and excluded explicitly. Final answer tokens can be reported separately. Handler milliseconds, wrapper wall time, and agent wall time are different quantities and must not be conflated.

Deterministic tool transcripts can be replayed for timing stability. Such replays do not constitute independent agent investigations. Thresholds will be frozen after the first blinded baseline has been scored, before production implementation/tuning; the second baseline pass is replication and cannot change those thresholds.

Raw questions, Uzir source-bearing transcripts, local adapter, scoring artifacts, and held-out gold remain under `/tmp/grafo-251-evaluation`, outside version control.

## Independent baseline replication

A second pair of fresh investigators completed the same ten held-out questions without access to earlier answers or gold. Source audit again found 7/7 complete positives, 21/21 supported required positive claims, 3/3 correct controls and no unsupported material claims. The second pass used 42 attempted/41 valid calls, 62,731 response proxy tokens, and 228,696 response bytes. One malformed wrapper request is recorded and excluded from valid-call efficiency. The two-pass comparator is therefore **81 valid calls and 120,507 response proxy tokens**. Frozen efficiency percentages and quality requirements remain unchanged.

## Snapshot equivalence

A read-only ordered SHA-256 digest audit verified all original graph tables and non-lexical metadata are identical between the preserved schema-14 baseline and migrated schema-15 candidate clone: 745,728 nodes, 2,924,958 facts, 3,030,718 edges, 14,253 file records and 14,254 path records. Dirty/reconciliation tables also agree. Only migration bookkeeping, candidate-only derived tables and `lexical_version` metadata are excluded. The audit took 195.6 seconds; this is preparation cost, not handler latency. Local row digests are stored in `/tmp/grafo-251-evaluation/index-equivalence.json`.
