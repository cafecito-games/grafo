# Investigation entry point: experiment log

Issue: [#251](https://github.com/cafecito-games/grafo/issues/251).

## Operating contract

The objective is better complete, evidence-supported Uzir investigations, not
better rankings on the nine original spike questions. Development and held-out
questions have separate ownership. Numerical acceptance criteria must be frozen
after establishing the existing-tool baseline and before implementation tuning.
Held-out failures must not become development examples for the same evaluation.

Implementation worktree: `issue-251`. Integration base:
`96b20b7d3dbe2c6c51b9d8b35e5e4e988c617adf`. Starting spike: `48f9968`,
cherry-picked as `8e473eb`. Corpus: clean Uzir
`8bee8ac48b63f231e3ee89fb8d335902a5e33632` at `/tmp/uzir-251-spike`.
Source-bearing evaluation artifacts remain outside this repository.

The final deliverable is an independently reviewed, unmerged PR with reproducible
evidence, or an evidence-backed no-go if the frozen criteria are not met.
Configured independent code reviewer: `glm-reviewer`.

## 2026-10-14: baseline and architecture preparation

At preparation time no production retrieval changes had been made. An independent
evaluator owns the development/held-out corpus, expected claims, source
verification, baseline transcripts, scoring, and thresholds.

The original spike established useful hypotheses, not a shipping result:

- Alphabetical candidate truncation hides relevant implementations.
- Test helpers and interface signatures can crowd out executable declarations.
- Nested existing reports repeat nodes and exhaust output budgets.
- A candidate in the top five is not a supported answer; implementation evidence
  may still be absent from the response.
- CLI freshness overhead must not be credited as a retrieval speedup.

An independent code inspection confirms the following constraints:

- No existing lexical postings index; substring SQL filters scope before
  alphabetical limits but still scans matching kind populations.
- Hydrated relation reads are bounded per relation, not per investigation.
- Type-root test expansion currently loads complete outgoing adjacency before
  applying its member limit.
- Source readers enforce a five-MiB per-file ceiling, not an aggregate read
  ceiling; source search has no aggregate scan limit.
- Federation can multiply per-repository work and project additional relations.

Design candidates to assess after baseline freeze: a derived lexical postings
index with explicit visit limits; a shared investigation use case over existing
graph ports; compact node/edge references; budget-aware source reading; explicit
weak-match, incomplete, and follow-up outcomes. No Uzir-specific vocabulary or
handwritten relationship inference will be introduced.

## Baseline freeze and first implementation hypothesis

The [acceptance criteria](evaluation/251-acceptance.md) were frozen before
production tuning. Two independent blinded baseline passes each achieved seven
complete positive answers, 21 supported required claims, and all three controls,
with no unsupported material claims. The pooled comparator is 81 valid MCP calls
and 120,507 response-token proxies. The fixed efficiency gate is at most 64 valid
calls or 102,430 tokens, preserving quality in each pass and respecting the
other metric's regression limit. See the protocol for measurement limitations.

Hypothesis: a persisted generic lexical index plus a compact, bounded evidence
report can reduce repeated tool navigation without losing answer evidence.
Candidate retrieval will count postings and declaration hydration; graph
expansion will use bounded local relation reads; source reads will share a
request-local cache and aggregate byte/line budgets. Federation-wide implicit
expansion is excluded because it multiplies work outside the request ledger.
The report must expose those coverage limits and actionable follow-ups.

Rejected implementation shortcut: enumerating all declarations and ranking them
in memory. The spike demonstrated its ranking value but it fails the frozen
bounded-work requirement. Another rejected shortcut is counting the CLI's
freshness cost as retrieval latency improvement.

## Development probe 1: bounded retrieval still needs selection before hydration

The first twelve development probes used the real MCP handler on a migrated clone
of the baseline index. Raw reports remain in `/tmp/251-dev-production`. Observed
handler times ranged from under a millisecond for an empty query result to about
1.8 seconds; these are diagnostic observations during concurrent development,
not acceptance latency measurements. All reports respected the 24,000-byte cap.

The 512-node hydration ceiling was routinely exhausted before applying narrow
scope filters. This lost the normalization implementation entirely and selected
nearby decoder helpers instead of the requested decoder. Other failures included
missing the main death handler, selecting interface declarations ahead of the
friend-request implementation, and incomplete overlay behavior evidence. The
paraphrase remained a weak match requiring an adaptive fallback. No held-out
candidate trials have been run.

Revised hypothesis: gather bounded lightweight posting projections, apply
path/language scope and lexical ranking, then hydrate the best bounded candidate
set. This preserves the global visit ceiling while spending hydration work on
plausible in-scope declarations. Generic implementation preference must not
override substantially stronger relevance.

Source audit also found projected generated bindings whose graph locations point
to generator configuration. Such locations cannot be emitted as implementation
source. The source session now rejects these projections explicitly. MCP source
access will require a dedicated investigation-source opt-in so an embedder's
restricted ordinary-source callback cannot be bypassed.

## Development probe 2: planner bounds and evidence allocation

The second probe exposed four-second retrieval timeouts. Concurrent tests and
the independent graph digest made aggregate latency unsuitable for comparison,
but query-plan inspection identified a real defect: SQLite reordered the
component metadata join into a global relation-kind scan. A final SQL LIMIT
was not enough to bound the work. Explicit bounded join order removed that scan;
a local warm metadata query dropped from roughly 20–24 ms to 0.020–0.027 ms.
An actual-query EXPLAIN regression now rejects the global-scan plan.

Other corrections from development: Go interface membership is recorded through
`has_field`; generated projections have explicit `generated_binding` metadata;
metadata probes themselves need a shared ceiling so empty queries across many
repositories cannot bypass work accounting. These are graph/schema facts, not
question-specific vocabulary.

The friend-request implementation was now present with its full 41-line source,
but optional local declarations and test paths still consumed almost the full
output cap. The next evidence-allocation experiment prioritizes source, limits
optional graph expansion, and considers small enclosing declarations only when
an explicit graph relationship and containing source range establish the link.
Paraphrases and inflections without shared indexed terms still require fallback;
no synonym table or semantic provider has been added.

The first full `task check` caught repository-specific fixture requirements:
tests must use `testtemp.Dir`, and the detail profiler's source-function anchor
must move with the source service. Both were corrected; their focused suites
pass. A full check will be rerun after development changes settle.

## Development probe 4: candidate for independent review

After the query-plan correction and source-priority allocation, all twelve
development calls returned reports within the deadline and byte ceiling.
This sequential diagnostic run observed approximately 0–540 ms per handler and
1,171–15,602 report bytes. These are single-run development measurements, not
the frozen repeated held-out acceptance test. Raw reports remain local in
`/tmp/251-dev-production-v4`.

Complete small-file context now includes the overlay's lifecycle logic and the
polling hook's cleanup/data behavior. The friend-request, respawn, inventory and
snapshot implementations have useful source evidence. The primary death handler
still requires follow-up discovery; the broad paraphrase remains weak, and the
normalization inflection produces no lexical match. These limitations are
retained honestly rather than patched with corpus vocabulary. Final investigators
must use adaptive follow-ups and will be scored on complete answers, including
the costs of those fallbacks.

No optional semantic provider is included: the provider-free candidate must
first establish measured value against the frozen baseline. The production
candidate is now ready for independent code review before held-out trials.

## Held-out evaluation result: no-go against the frozen gates

An independent evaluator ran two fresh candidate-assisted held-out passes, scored
them against source, and replayed both arms. The candidate was the reviewed
commit, frozen by binary hash and re-verified after the trials. The outcome is a
**no-go**: gates 3 and 4 pass, gates 1 and 2 fail. The gates were not adjusted,
and these ten cases cannot now become tuning cases.

| Measure | Baseline pooled | Candidate pooled | Gate |
| --- | --- | --- | --- |
| Valid MCP calls | 81 | 73 (-9.88%) | <=64 (-20%) |
| Response proxy tokens | 120,507 | 252,773 (+109.76%) | <=102,430 (-15%) |

Per pass the candidate used 38 and 35 valid calls against the baseline's 40 and
41, so valid calls did improve strictly in each pass, at -5.00% and -14.63%.
Pooled, that reaches only -9.88% against the frozen -20% bar. Tokens rose
+151.20% and +71.59%. Neither frozen metric choice passes: the token threshold is
missed by roughly a factor of 2.5, and with calls as the selected metric the
other metric's +109.76% exceeds the +10% pooled allowance.

Answer quality held. Both passes returned 7/7 complete positive answers, 21/21
supported required claims and 3/3 correct controls. Gate 1 nevertheless fails
because pass 1 contains one fabricated relationship, and the frozen requirement
is zero per pass. An investigator flattened a `get_callers` tree and read
transitive depth-2 and depth-3 callers as direct call sites, then used that to
question whether another deployment path configured a different value. The
misread tool is a baseline tool present in both arms rather than `investigate`,
and `get_callers` returns per-node depth alongside an `edges` array that carries
none, which invites exactly this error. That is a real ergonomic defect worth
repairing on its own, but it does not convert a failed gate into a pass. The
no-go does not depend on that single judgement: gate 2 fails decisively on its
own arithmetic.

Latency passed narrowly. Three sequential replays per arm, with startup,
freshness and indexing excluded and no competing workloads, gave median total
handler times of 19,668.717 ms for the baseline and 38,697.707 ms for the
candidate, a ratio of 1.9675 against the 2.0 ceiling. Across 54 replayed
`investigate` calls, p95 was 269.67 ms and the maximum 657.37 ms, well inside the
5 s and 10 s limits, with a 60.49 ms median. The ratio must be read carefully:
each arm replays its own transcripts, so it reflects both handler speed and what
each arm's agents chose to call. Candidate handler time is dominated by broad
agent-chosen `search_source` calls, whose slowest singles were 16.8 s, 15.2 s and
14.5 s, not by `investigate`.

The call allowance held. No case exceeded 8 attempted calls, no cap refusal was
triggered, and the largest of 18 `investigate` responses was 18,796 bytes against
the 24,000-byte cap.

### Why it failed

The cost is additive rather than substitutive. Pooled by tool, `investigate`
accounted for 18 calls and 61,690 tokens. Candidate investigators also issued 41
`search_source` calls returning 162,349 tokens, against the baseline's 33 calls
and 59,479 tokens, with substantially larger `max_matches` and `context_lines`.
Meanwhile `find_symbols` fell from 17 calls to 2 and `get_source` from 29 to 10.
The entry point did displace symbol lookup and individual source reads; it did
not stop an agent that is merely told the tool exists from also paying for broad
content search. Any future attempt should target that substitution directly,
and must reserve a new untouched cohort.

### Limitations of this measurement

The original baseline investigators' prompt text was not preserved in the frozen
artifacts, so the evaluator reconstructed the base prompt from the protocol and
harness rules and added only neutral tool documentation; textual equivalence to
the baseline prompt cannot be proven. The baseline investigators' model and
sampling settings are not recorded anywhere either. All four candidate
investigators used one agent type with no model override, so the candidate arm is
internally consistent, but identity of model and settings between arms cannot be
demonstrated. The efficiency comparison therefore carries a confound that its
arithmetic does not remove, and both the pooled call reduction and the token
regression should be read with that in mind. Investigators' tool calls were not
directly observable, so the requirement that all source acquisition go through
the measured tools rests on the claim audit, under which uncited or unreturned
content scores as unsupported.

Response proxy tokens remain `cl100k_base` counts over MCP text content, counted
once, excluding the duplicate `structuredContent` representation. They are not
billed, prompt or reasoning tokens. Handler milliseconds, wrapper wall time and
agent wall time stay distinct; investigator agent wall times were 173, 288, 182
and 215 seconds and are not interchangeable with handler time.

The preserved schema-14 baseline index was verified byte-identical before any
work and after both replays.
