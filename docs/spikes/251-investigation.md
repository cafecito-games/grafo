# Spike #251: task-oriented investigation on Uzir

## Decision

Continue with a bounded retrieval/evidence-design iteration, but do not ship the wrapper from this spike. Composition is useful for finding entry points when question terms resemble code names. Naive candidate caps, test-helper pollution, duplicate evidence payloads, and weak-match abstention must be addressed. This experiment does **not** establish that an agent answers complete investigations more accurately or with fewer total tokens.

## Reproduction and provenance

The spike wrapper and its runner scripts are deliberately not retained in this
repository, per the decision above. The commands below reproduce the experiment
only from the spike branch, where that code still exists; the production
`grafo investigate` entry point supersedes it.

- Grafo base: `96b20b7d3dbe2c6c51b9d8b35e5e4e988c617adf`; spike branch `spike/251-investigation`, commit `48f9968`, which holds `cmd/grafo-investigate-spike/` and `docs/spikes/run-251*.py`. That branch was local to the original author's machine and was never published, so the experiment is reproducible from this history only where it is present.
- Uzir: detached clean worktree at `8bee8ac48b63f231e3ee89fb8d335902a5e33632`, excluding the user's active uncommitted work.
- Corpus checkout: `/tmp/uzir-251-spike`; prototype checkout: `/tmp/grafo-251-spike`.
- Fresh index completed successfully: 14,253 updated files, 402,587 ms, 37 reconciliation batches. Index and source remain local.
- Index reported 2,764 diagnostics. The largest group is 2,515 Go-module diagnostics for `tools/playctl` (for example undefined generated protocol fields); other groups include 139 recoverable TypeScript syntax errors and unresolved imports. A complete index is not complete language/relationship coverage.
- No embedding service, LLM, generated synonyms, corpus-specific ranking rules, source-content search, or external source upload.
- Nine manually selected cases were fixed before retrieval. Expected code locations were inspected directly. Two scoring corrections were made after inspecting responses: friend-request success requires the implementation in `internal/social/actions.go`, not an interface with the same method name; protocol scope legitimately returns generated friend-request declarations, so that case checks scope adherence rather than expecting an empty result. Existing raw results were rescored without rerunning ranking.

```sh
task build
./grafo index /tmp/uzir-251-spike --json
go build -o /tmp/grafo-investigate-spike ./cmd/grafo-investigate-spike
python3 docs/spikes/run-251.py /tmp/uzir-251-spike /tmp/251-results
SPIKE_ARGS=-full-candidates python3 docs/spikes/run-251.py /tmp/uzir-251-spike /tmp/251-results-full
python3 docs/spikes/run-251-cli.py /tmp/uzir-251-spike /tmp/251-results
```

The binary requires an explicitly refreshed index. It does not integrate with freshness leasing and must not be treated as a production tool. Flags precede the question. Example:

```sh
/tmp/grafo-investigate-spike -repo /tmp/uzir-251-spike -full-candidates   -path-prefix apps/server 'where is player death handled'
```

## Experiment

The initial variant queries up to 100 declarations per term and kind using existing scoped node queries, ranks identifier-word matches, and returns five candidates. It enriches up to three with existing relation-edge queries, FindTests, and source reading. Allocation is at most 12 direct relations and 30 source lines per root, with test traversal depth 2/limit 12 and a hard 24,000-byte compact JSON response cap. Returned graph edges retain their actual IDs; relevance scores are explicitly lexical inferences.

The diagnostic `-full-candidates` variant materializes all semantic candidate declarations, then filters scope/test-source paths and ranks before truncation. It also prefers shorter declaration names on equal scores. These are bundled diagnostic changes, not an isolated ablation. This variant intentionally violates production work-bounding requirements to test whether retrieval quality improves when premature candidate truncation is removed.

The comparison Find baseline uses manually chosen terms in the case file and the same enrichment/response allocation. Find has no corresponding path scope; this is the existing one-shot discovery behavior, not the best possible adaptive investigation. An agent can use better qualified names, additional queries, or text search. Do not interpret baseline misses as Grafo's inability to answer the question.

Three fresh-process samples per case, sequential prototype then baseline. Times include process startup, repository discovery, read-only index opening, retrieval, and enrichment. Filesystem caches were not flushed; these are not cold-query measurements. The cold index is reported separately. Some runs overlapped the real CLI baseline, so timings are observational rather than controlled performance claims.

## Retrieval results

Ranks below are within the top five, for a source-verified relevant declaration. The broader death/respawn/overlay criteria accept a declaration in the relevant implementation file; this is entry-point discovery, not full answer correctness.

| Question | Existing Find rank | Capped rank | Full-pool rank | Capped / full median ms | Full response tokens |
|---|---:|---:|---:|---:|---:|
| death | miss | miss | 1 | 456.7 / 908.3 | 3897 |
| respawn | miss | 1 | 1 | 544.8 / 945.1 | 4558 |
| friend | miss | miss | 4 | 485.4 / 723.7 | 1406 |
| inventory | 1 | 1 | 1 | 614.3 / 698.9 | 3901 |
| overlay | 1 | 1 | 1 | 483.8 / 723.3 | 4212 |
| paraphrase | miss | miss | miss | 744.6 / 741.5 | 4458 |

Token counts use `tiktoken`'s `cl100k_base` on the raw final JSON response; they are a reproducible proxy, not a claim about a particular agent's billed tokens. Prompts, reasoning, and follow-ups are excluded.

- Initial retrieval finds the intended implementation area in 3/6 positive questions; the diagnostic variant finds it in 5/6. This is a small, non-random, development corpus, not a general accuracy estimate.
- **Death:** capped retrieval returns persistence/test helpers. Full-pool retrieval returns `DeathSystem.planPlayerDeath` first and `DeathSystem.resolvePlayerDeath` fourth.
- **Friend request:** capped retrieval returns interfaces, adapters, and test fakes. Full-pool retrieval includes `social.Service.SendFriendRequest` fourth, still behind interfaces/adapters. Because only three roots are enriched, it does not supply implementation evidence for that fourth candidate.
- **Respawn:** `PlayerRespawnSystem` ranks first, but a 30-line type excerpt mostly describes fields; method-oriented enrichment is needed for a behavior answer.
- **Inventory projection:** `projectInventoryChanged` ranks first and its source shows construction of the wire event. The wrapper does not automatically prove an end-to-end message flow.
- **Death overlay:** finds the actual countdown methods and returns one structural test match.
- **Paraphrase:** both variants fail to connect “bring a slain character back to life” with respawn. They return weak character-name matches; a production tool should report insufficient relevance and suggest narrower terms or optional semantic retrieval.
- **Ambiguous `Init`:** multiple candidates remain; no uniqueness claim is made. The capped variant loses all evidence to the output budget.
- **No match:** “quantum banana teleportation” returns no candidates and does not assert behavior absence.
- **Protocol-only scope:** returns matching generated protocol declarations, all inside scope, rather than the server implementation. Scope adherence is correct, but matching vocabulary alone does not make a useful answer.

## Evidence and budget findings

Wrapping existing report structs repeats nodes and paths. Under the same 24 KB cap, the friend-request responses retain only one root's evidence, and the initial Init response retains none. The simple trimming strategy removes whole evidence sections in reverse rank order; it is deliberately not a production allocation policy.

Only the overlay question returns a structural test match among the six positive questions in either variant. This is not evidence that the other behaviors lack tests: traversal is shallow, work-limited, and responses may be clipped. Broader/deeper evidence requires explicit follow-ups and must preserve uncertainty.

All 54 prototype responses across the two variants and nine cases remained under the 24,000-byte cap. Three repeats were identical apart from timing. A 2,048-byte death-query request produced valid 1,591-byte JSON with truncation explicitly set. Candidate enumeration and payload construction are not fully memory/work bounded, and a context timeout returns an error rather than a partial response. No production global-budget claim is made.

## Real multi-call CLI sample

For two questions, the harness actually ran one Find plus neighbors, find-tests, and source for each of the first three candidates: ten scalar CLI calls. Results:

| Case | Calls | Wall seconds | Compact response bytes | Response tokens |
|---|---:|---:|---:|---:|
| friend | 10 | 118.97 | 8471 | 2189 |
| overlay | 10 | 127.94 | 23142 | 6749 |

The prototype takes one invocation, but these timings are **not** evidence of a comparable MCP speedup: existing CLI calls run normal freshness checks, while the prototype assumes an already-refreshed index. Existing MCP batching could reduce the corresponding interaction count to four (discovery, relationships, tests, source). The fixed baseline also follows the first three Find results, which can be generated protocol declarations rather than the desired implementation. Response content and coverage differ; no equal-answer token-saving claim is justified. The friend prototype's small response partly reflects dropped evidence, not efficiency.

## Recommended next experiment

1. Rank within the scoped candidate population before truncating. Test a bounded lexical retrieval port or reusable lexical index against this full-pool reference. Exclude test helpers from primary production entry points while still retrieving tests as evidence.
2. Prefer executable implementations over interface signatures and generated accessors, while preserving those as relevant secondary evidence. Diversify candidates so several methods of one type do not consume the entire shortlist.
3. Use one deduplicated node/edge table and small section references. Allocate space for source, one useful test path, uncertainty, and follow-ups before optional graph detail; never drop all evidence silently behind an otherwise rich candidate list.
4. Add an explicit weak-match/ambiguous outcome and matched/unmatched terms. Evaluate optional embeddings or bounded content retrieval on the paraphrase failure, without presenting inferred matches as relationships.
5. Then run actual agent investigations, with a fixed model/prompt and multiple trials, against an adaptive existing-tool baseline. Measure supported final answers, unsupported claims, full token use, latency, calls, and necessary search fallbacks. Coordinate that harness with #258; richer missing-evidence diagnosis remains #252.

Acceptance for another spike: preserve all five successful discovery cases, retain implementation source and useful test evidence when available within the response budget, abstain on the paraphrase rather than confidently offering unrelated matches, and match full-pool retrieval without scanning the whole corpus per request.

## Verification

`task check` passed after the final Go changes. Focused ranking tests passed. Corpus checks verified response byte limits and repeat determinism. Raw source-bearing artifacts are in `/tmp/251-results` and `/tmp/251-results-full`, outside the repository; index report is `/tmp/uzir-251-index.json`. No parser, storage schema, production CLI, or MCP behavior was changed.
