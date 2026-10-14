# Frozen acceptance criteria: investigation #251

Frozen after the first blinded existing-tool baseline was source-audited and before production implementation/tuning. Later baseline replication must not change these thresholds. The source/gold-assisted control is diagnostic only; the blinded adaptive existing-tool workflow is the primary comparator.

## Baseline at freeze

Ten held-out cases: seven positive investigations, one ambiguity control, one missing-evidence control and one scope-boundary control. Fresh agents received questions/scope only, used real MCP tools with batching and content-search fallbacks, and cited returned source. The evaluator audited required and additional material claims.

- Complete positive answers: **7/7**.
- Supported required positive claims: **21/21**.
- Controls handled correctly: **3/3**.
- Unsupported material claims found: **0**.
- Calls: **42 attempted, 40 valid**. Two malformed initial wrapper requests are recorded but excluded from the efficiency comparison; the tool-name wrapper schema is clarified for subsequent trials.
- MCP text output: **57,776 cl100k_base proxy tokens**, **216,550 bytes**. No model-billed/prompt/reasoning token claim.
- Observed handler time: **16.108 seconds total**, **0.850 seconds median per investigation**. These concurrent discovery-trial timings are descriptive, not the final controlled latency comparison.

Before freezing, one ambiguity scoring clarification accepted any two source-supported distinct plausible implementations, rather than requiring the gold author's particular pair. Ambiguity is not an exhaustive implementation inventory. No production candidate output was viewed.

## Fixed completion gates

All gates below must hold. A held-out failure cannot trigger tuning on those cases. Either report no-go against these criteria or reserve a new untouched cohort for a new experiment; never lower these criteria.

1. **Quality:** In each of two fresh candidate-assisted passes, complete positive answers and supported required positive claims must be no lower than the corresponding baseline pass, and at least 7/7 and 21/21 respectively. All three controls must pass in each candidate pass. No unsupported material claim, fabricated relationship, scope violation presented as in-scope, or absence claim based only on no-match is allowed. Positive abstention is not positive completion.
2. **Repeatable efficiency:** Pool the two candidate passes and compare against the two independent baseline passes. Achieve either at least **20% fewer valid MCP calls** or at least **15% fewer response proxy tokens**. The chosen improvement metric must improve strictly in **each** pass, and the other metric must increase by no more than **10% pooled**. Report attempted calls too. Include all fallbacks, errors from valid tools, and follow-up source/evidence. No credit for omitted required evidence. The corresponding gates for the first baseline alone are <=32 calls or <=49,109 proxy tokens; final pooled denominators use both baseline passes without changing percentages.
3. **Fair tool allowance:** At most **8 attempted MCP calls per case** in either arm. Candidate agents can use `investigate` plus all baseline tools. Each `investigate` response is capped at **24,000 bytes**. Same questions, scope, base prompt, model/settings, index snapshot, and no embedding provider in the primary comparison. Tool-format help may differ only to introduce the new tool and remove wrapper mistakes.
4. **Latency:** Replay each fixed completed transcript sequentially three times on the same read-only index, excluding startup/freshness/indexing and avoiding concurrent workloads. Compare the median total handler time per arm: candidate must be **<=2 times baseline**. Individual investigate calls must have **p95 <=5 seconds** and **maximum <=10 seconds** in these replays. Agent wall time is reported separately and is not interchangeable with handler time.
5. **Bounds and correctness:** Hard output bytes, global source lines/bytes, retrieval/graph work, and deadline limits must have meaningful tests including exhaustion, cancellation, ambiguous/missing evidence and cross-scope evidence. Results retain explicit incomplete/truncated boundaries. Passing byte caps after unbounded enumeration does not pass. Production CLI/MCP share semantics, task check passes, and independent review has no unresolved required fixes.

Source-bearing transcripts, all case corpora, scoring records and the timestamped hash manifest remain local under `/tmp/grafo-251-evaluation`. Corpus hashes are recorded there rather than committing corpus data. No held-out question or implementation-specific finding is supplied to production developers before final evaluation.
