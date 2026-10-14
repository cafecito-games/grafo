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
