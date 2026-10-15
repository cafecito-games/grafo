# Bounded repository investigations

`grafo investigate` and MCP `investigate` assemble implementation candidates,
source excerpts, graph evidence, and suggested follow-up queries for a question.
They return evidence for an agent to interpret, not a generated answer.
CLI and MCP use the same service and limits. Neither requires an embedding
provider or sends source to one.

```sh
grafo investigate "where does checkout charge a payment" --repo . --json
grafo investigate "checkout retry" --repo . --path-prefix internal/payments --language go --max-candidates 3
```

The corresponding MCP call is:

```json
{
  "name": "investigate",
  "arguments": {
    "query": "where does checkout charge a payment",
    "path_prefixes": ["internal/payments"],
    "languages": ["go"],
    "max_candidates": 3
  }
}
```

## Scope and retrieval

`--repo` or `--repos` selects the indexed worktrees opened by the CLI. Within
those indexes, `--repo-name` (MCP `repository`) restricts retrieval to the indexed
repository name. `--component` selects recorded component ownership; it does
not guess membership from a similar directory name. `--path-prefix` accepts
comma-separated or repeated repository-relative segment prefixes. MCP uses
`path_prefixes`. `--language` accepts comma-separated parser language identifiers;
MCP uses `languages`.

Scopes restrict candidates and returned related evidence. Related evidence
whose component membership has not been established is omitted for a
component-scoped request. Cross-repository graph expansion is omitted: inspect
another repository explicitly when the investigation reaches that boundary.
A narrow scope can exhaust the bounded posting scan before all relevant
candidates are found; read the truncation and boundary fields.

Retrieval uses persisted declaration-name, qualified-name, and path tokens.
It splits punctuation, camel case, acronyms, and letter/digit boundaries,
lowercases terms, and removes common grammatical words from the question.
It visits bounded posting lists and ranks inspected candidates before selecting
results. This is provider-free lexical discovery, without domain-specific
synonyms. A paraphrase that shares no useful code terms may miss an implementation.
Score ties prefer function and method candidates over contracts, generated code,
and other declarations; this preference does not establish runtime behavior.

## Read the report as evidence

`candidates` records ranking hints, matched terms, and declaration designations.
`nodes`, `edges`, and `excerpts` provide compact evidence tables. Node identities
are repository-qualified: use the repository together with the node ID when
interpreting a federated result. Edge kinds and source locations support the
specific recorded relationship; two relevant candidates are not thereby related.
Excerpts include their path and line range. They come from the current checkout,
which can differ from an older index snapshot. A completely read file of at most
160 lines can be returned as context when the remaining line budget permits.
The referenced node still identifies the discovery candidate; the excerpt
coordinates identify all returned context. Overlapping candidates reuse that
excerpt. A recorded enclosing declaration can also supply compact source context.

`status` distinguishes candidates, ambiguous results, weak matches, and no match.
It is a retrieval signal, not an answer-confidence probability. `truncated`,
`boundaries`, and `followups` describe incomplete evidence. No match, no returned
edge, or a missing test is never proof of absence. Even an untruncated report is
not an exhaustive repository inventory.

At most two candidates receive optional graph expansion, with up to three
callees and one structural test path per candidate. Tests are discovered through
bounded paths of up to four helper edges. Their presence is
source evidence of a relationship, not runtime execution coverage or proof that
a behavior is correctly tested. Deeper paths and type-level test aggregation
may require `find_tests`. Read truncated implementation excerpts and relevant
callees before making complete behavior claims.

Follow-up selectors can be passed to the ordinary tools:

```sh
grafo source '<node-id>' --repo . --max-lines 400 --json
grafo find-tests '<node-id>' --repo . --depth 4 --limit 30 --json
grafo search 'retryLimit' --repo . --path-prefix internal/payments --json
```

For MCP, use `get_source` with `{"selector":"<node-id>","max_lines":400}`,
`find_tests` with `{"selector":"<node-id>","depth":4,"limit":30}`, or
`search_source` with `{"pattern":"retryLimit","path_prefixes":["internal/payments"]}`.
In a federation with duplicate IDs, narrow the indexed worktrees before using a
follow-up selector. Source, symbol, and test tools also support batching; an
investigation does not replace a competent adaptive workflow with those tools.

## Global limits

Zero or omission selects the default. Values outside the accepted range fail
validation. All limits apply to the whole investigation, not independently to
each candidate or repository.

| CLI option | MCP field | Default | Accepted nonzero range |
|---|---|---:|---:|
| `--max-candidates` | `max_candidates` | 5 | 1–12 |
| `--max-source-lines` | `max_source_lines` | 240 | 1–600 |
| `--max-source-bytes` | `max_source_bytes` | 524,288 | 1–2,097,152 |
| `--max-output-bytes` | `max_output_bytes` | 24,000 | 2,000–24,000 |
| `--max-work` | `max_work` | 12,000 | 32–50,000 |
| `--timeout-ms` | `timeout_ms` | 4,000 | 1–8,000 |

The query accepts 1–2,000 UTF-8 bytes and at most 32 retrieval terms. Each scope
list accepts at most 20 entries. Repository and component selectors accept at
most 256 bytes; each path or language entry accepts at most 1,024 bytes.

`usage` reports postings, hydrated declarations, metadata reads, graph queries, work units,
source bytes read, source lines returned, and output bytes. Source files are
cached within a request; bytes read count once, while repeated returned lines
count again. Source-byte exhaustion can prevent reaching a late declaration in
a large file. It does not authorize an unbounded read followed by trimming.
The work budget bounds retrieval and graph expansion; it is not a CPU instruction
counter. The deadline starts inside the investigation service, after index
acquisition/freshness work. Cancellation can return an error; a deadline reached
after evidence collection can instead produce an explicitly incomplete report.

The output cap counts canonical compact JSON report bytes, including usage and
boundaries. It excludes JSON-RPC transport escaping and envelope bytes and the
CLI's final newline. MCP returns one JSON text block without duplicating the
report in `structuredContent`. The CLI emits compact JSON with or without
`--json`. Optional evidence may be removed to fit the output cap; reported
truncation and work usage remain visible.

## Local source access and index upgrades

Source reads are confined to the owning worktree, including symlink checks, and
use a request-local bounded reader. The installed `grafo mcp` server explicitly
enables this access. Library embedders must opt in with
`WithInvestigationSource(source.NewScopedService)` or a suitable source-service
factory. `WithSource` alone does not enable investigation: its custom callback
might redact or deny content, and investigation must not bypass that policy.
The investigation factory receives the repository and projects from the
acquired generation for that call.

SQLite migration 15 adds derived lexical postings and a dirty queue. Opening an
index writable migrates and backfills postings from existing graph metadata in
resumable batches; it does not require reparsing unchanged source. Subsequent
graph writes update postings transactionally. A read-only investigation cannot
repair an old or incomplete lexical index. Run `grafo index /path/to/repo` with
the current binary to complete the upgrade before querying it. Initial migration,
backfill, and ordinary freshness checks have separate costs from query execution.
Source and indexes remain local.

## Evaluation status

**This entry point did not meet its acceptance criteria and is experimental.** A
blinded held-out evaluation against batched graph tools plus text search returned
a no-go. Answer quality held at the baseline, 7/7 complete positive answers and
21/21 supported required claims in each of two passes, and the bounds and latency
gates passed. The efficiency gate failed: pooled valid MCP calls fell only 9.88%
against a required 20%, while returned response tokens rose 109.76% against an
allowance of 10%. Agents given the tool added it to broad content search instead
of replacing it. Do not treat this command as an established improvement, and do
not cite it as one. The measured results, the reason for the failure and the
confounds are in the [experiment log](investigation-experiments.md).

The experimental protocol and frozen gates are in
[the evaluation protocol](evaluation/251-protocol.md) and
[acceptance criteria](evaluation/251-acceptance.md). The
[experiment log](investigation-experiments.md) records measurements and rejected
approaches. These documents distinguish development cases from held-out
investigations. Availability of this command alone does not establish an
improvement over batched graph tools plus text search; consult the measured
results and limitations before drawing that conclusion.
