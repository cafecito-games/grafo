package layoutbench

import (
	"embed"
	"io/fs"
)

// indexReformEmbedded holds the index-reform candidate's Goose migrations.
// The files mirror the production migration set with the same version
// numbers, so a database pre-seeded by them satisfies the production
// adapter's migration check: Goose records version ids only, and the
// production adapter then prepares its unmodified statements against the
// reformed DDL. The reform is pure DDL — same tables, same columns, same
// statement text — with the small natural-key tables stored WITHOUT ROWID.
//
//go:embed variants/indexreform/*.sql
var indexReformEmbedded embed.FS

// indexReformMigrations is the migration filesystem the pre-seeded opener
// hands to Goose before the production adapter opens the database.
var indexReformMigrations = func() fs.FS {
	migrations, err := fs.Sub(indexReformEmbedded, "variants/indexreform")
	if err != nil {
		// The embedded directory is fixed at compile time, so a failed sub
		// walk means the embed directive and this path disagree.
		panic("layoutbench: embed index-reform migrations: " + err.Error())
	}
	return migrations
}()

// indexReformWithoutRowidTables lists the tables the reform set re-creates
// WITHOUT ROWID: each keeps its production primary key, and the implicit
// sqlite_autoindex a rowid table needs to enforce that key disappears
// because the key becomes the clustered b-tree.
var indexReformWithoutRowidTables = []string{
	"meta",
	"files",
	"dirty_owners",
	"dirty_nodes",
	"dirty_targets",
	"dirty_facts",
}

// IndexReformClusteredKeyTables returns the tables the reform migration set
// stores WITHOUT ROWID, for callers driving OpenPreSeeded with
// IndexReformSpec's migrations.
func IndexReformClusteredKeyTables() []string {
	return append([]string(nil), indexReformWithoutRowidTables...)
}

// IndexReformDecision records one measured outcome of the index-reform
// candidate: Decision is "keep" (index unchanged in the reformed schema),
// "reformed" (WITHOUT ROWID re-creation), or "reverted" (an attempted
// reform that measurably degraded a plan and was rolled back). PlanEvidence
// quotes the EXPLAIN QUERY PLAN behavior that justifies the outcome.
type IndexReformDecision struct {
	Index        string `json:"index"`
	Decision     string `json:"decision"`
	Reason       string `json:"reason"`
	PlanEvidence string `json:"plan_evidence"`
}

// IndexReformObjectBytes is one object of the baseline dbstat ranking.
type IndexReformObjectBytes struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Pages int64  `json:"pages"`
}

// IndexReformReport is the machine-readable evidence artifact of the
// index-reform candidate: the baseline attribution ranking the decisions were
// derived from, and every keep/reform/revert decision with its plan
// evidence. Task 8's report folds this in verbatim.
type IndexReformReport struct {
	BaselineSeed    int                      `json:"baseline_seed"`
	BaselineScale   int                      `json:"baseline_scale"`
	BaselineCounts  string                   `json:"baseline_counts"`
	BaselineNote    string                   `json:"baseline_note"`
	BaselineObjects []IndexReformObjectBytes `json:"baseline_objects"`
	// BaselineQueueConfigBytes totals the queue/config cluster of
	// BaselineObjects: the six WITHOUT ROWID tables, their implicit
	// autoindexes, and dirty_facts_order. BaselineAutoindexBytesDropped is
	// the autoindex share the reform eliminates. Both are pinned literals;
	// TestIndexReformDecisionsAreComplete re-derives them from the object
	// entries so a transcription slip fails loudly.
	BaselineQueueConfigBytes      int64                 `json:"baseline_queue_config_bytes"`
	BaselineAutoindexBytesDropped int64                 `json:"baseline_autoindex_bytes_dropped"`
	Decisions                     []IndexReformDecision `json:"decisions"`
}

// IndexReformReportEvidence returns the recorded measurement record of the
// candidate. The baseline ranking was captured with the Task 1 attribution
// tooling over a fixture database built through the production adapter (seed
// 2, scale 500: 55 files, 5,071 nodes, 6,003 facts, 6,004 edges, 65
// external nodes); the plan evidence quotes the captured EXPLAIN QUERY PLAN
// steps that TestIndexReformPlansRemainCovered and
// TestIndexReformCollationConsolidationReverts keep honest.
func IndexReformReportEvidence() IndexReformReport {
	return IndexReformReport{
		BaselineSeed:   2,
		BaselineScale:  500,
		BaselineCounts: "files=55 nodes=5071 facts=6003 edges=6004 external=65",
		BaselineNote: "dbstat objects of the control fixture database, ranked by bytes; " +
			"the queue/config cluster (meta, files, the four dirty_* tables, their implicit " +
			"autoindexes, and dirty_facts_order) totals 81920 bytes there, of which the " +
			"WITHOUT ROWID reform drops the 36864 bytes of autoindex b-trees at this scale",
		BaselineQueueConfigBytes:      81920,
		BaselineAutoindexBytesDropped: 36864,
		BaselineObjects: []IndexReformObjectBytes{
			{Name: "facts", Bytes: 6164480, Pages: 1505},
			{Name: "edges", Bytes: 4866048, Pages: 1188},
			{Name: "nodes", Bytes: 4128768, Pages: 1008},
			{Name: "edges_to", Bytes: 3305472, Pages: 807},
			{Name: "edges_from", Bytes: 3239936, Pages: 791},
			{Name: "facts_from_id", Bytes: 1748992, Pages: 427},
			{Name: "nodes_qualified_resolve", Bytes: 1630208, Pages: 398},
			{Name: "nodes_name_resolve", Bytes: 1622016, Pages: 396},
			{Name: "sqlite_autoindex_nodes_1", Bytes: 1495040, Pages: 365},
			{Name: "facts_target_id", Bytes: 1478656, Pages: 361},
			{Name: "facts_owner", Bytes: 1175552, Pages: 287},
			{Name: "nodes_owner", Bytes: 987136, Pages: 241},
			{Name: "nodes_qualified", Bytes: 225280, Pages: 55},
			{Name: "sqlite_autoindex_facts_1", Bytes: 204800, Pages: 50},
			{Name: "sqlite_autoindex_edges_1", Bytes: 200704, Pages: 49},
			{Name: "nodes_name", Bytes: 200704, Pages: 49},
			{Name: "edges_fact", Bytes: 196608, Pages: 48},
			{Name: "facts_target", Bytes: 135168, Pages: 33},
			{Name: "nodes_kind", Bytes: 94208, Pages: 23},
			{Name: "facts_source", Bytes: 65536, Pages: 16},
			{Name: "files", Bytes: 20480, Pages: 5},
			{Name: "sqlite_autoindex_files_1", Bytes: 16384, Pages: 4},
			{Name: "embeddings_model", Bytes: 4096, Pages: 1},
			{Name: "nodes_external", Bytes: 4096, Pages: 1},
			{Name: "meta", Bytes: 4096, Pages: 1},
			{Name: "sqlite_autoindex_meta_1", Bytes: 4096, Pages: 1},
			{Name: "dirty_facts", Bytes: 4096, Pages: 1},
			{Name: "sqlite_autoindex_dirty_facts_1", Bytes: 4096, Pages: 1},
			{Name: "dirty_facts_order", Bytes: 4096, Pages: 1},
			{Name: "dirty_nodes", Bytes: 4096, Pages: 1},
			{Name: "sqlite_autoindex_dirty_nodes_1", Bytes: 4096, Pages: 1},
			{Name: "dirty_owners", Bytes: 4096, Pages: 1},
			{Name: "sqlite_autoindex_dirty_owners_1", Bytes: 4096, Pages: 1},
			{Name: "dirty_targets", Bytes: 4096, Pages: 1},
			{Name: "sqlite_autoindex_dirty_targets_1", Bytes: 4096, Pages: 1},
		},
		Decisions: []IndexReformDecision{
			{
				Index:    "nodes_qualified_resolve",
				Decision: "keep",
				Reason: "the measured NOCASE consolidation (drop nodes_qualified, widen this index " +
					"to COLLATE NOCASE) reverted: a binary equality cannot seek a NOCASE b-tree, so " +
					"every resolution query regressed; the binary collation is load-bearing",
				PlanEvidence: "FindNodesExact: SEARCH nodes USING COVERING INDEX nodes_qualified_resolve " +
					"(qualified_name=? AND external=?); under the attempted NOCASE composite the same " +
					"query degrades to SCAN nodes USING COVERING INDEX nodes_qualified_resolve (full index scan)",
			},
			{
				Index:    "nodes_name_resolve",
				Decision: "keep",
				Reason: "same reversion as nodes_qualified_resolve: the binary collation serves " +
					"FindNodesExact arm two and the external matching joins",
				PlanEvidence: "FindNodesExact arm two and ListExternalNodesMatching MULTI-INDEX OR arms: " +
					"SEARCH nodes USING INDEX nodes_name_resolve (name=? AND external=?); the NOCASE " +
					"composite attempt degrades them to SCAN nodes USING COVERING INDEX nodes_name_resolve",
			},
			{
				Index:    "nodes_qualified",
				Decision: "keep",
				Reason: "not redundant with the resolve composite: it is the only NOCASE b-tree on " +
					"qualified_name, and the Match/Count selector queries compare COLLATE NOCASE",
				PlanEvidence: "MatchNodesByQualifiedName / CountNodeMatchesByQualifiedName: " +
					"SEARCH nodes USING INDEX nodes_qualified (qualified_name=?)",
			},
			{
				Index:    "nodes_name",
				Decision: "keep",
				Reason:   "not redundant with the resolve composite: it is the only NOCASE b-tree on name",
				PlanEvidence: "MatchNodesByName / CountNodeMatchesByName: " +
					"SEARCH nodes USING INDEX nodes_name (name=?)",
			},
			{
				Index:    "nodes_owner",
				Decision: "keep",
				Reason:   "owner-scoped deletion and dirty-node marking seek it; no composite covers owner_file",
				PlanEvidence: "DeleteNodesByOwner: SEARCH nodes USING INDEX nodes_owner (owner_file=?); " +
					"MarkOwnedNodesDirty seeks the same",
			},
			{
				Index:    "nodes_kind",
				Decision: "keep",
				Reason: "retention proven twice: the aggregate covering-scans it and the kind-filtered " +
					"listing seeks it; dropping it would degrade ListNodesByKind to an uncovered table scan",
				PlanEvidence: "CountNodesByKind: SCAN nodes USING COVERING INDEX nodes_kind; " +
					"ListNodesByKind: SEARCH nodes USING INDEX nodes_kind (kind=?)",
			},
			{
				Index:    "nodes_external",
				Decision: "keep",
				Reason: "the partial covering index serves both the orphan cleanup and the external " +
					"count without touching the wide table",
				PlanEvidence: "DeleteOrphanExternalNodes: SCAN nodes USING COVERING INDEX nodes_external; " +
					"CountExternalNodes: SCAN nodes USING COVERING INDEX nodes_external",
			},
			{
				Index:    "edges_from",
				Decision: "keep",
				Reason:   "pinned by INDEXED BY in ListOutgoingRelationEdges; name must survive unchanged",
				PlanEvidence: "ListOutgoingRelationEdges / ListEdgesFrom: SEARCH edges USING INDEX " +
					"edges_from (from_id=? AND kind=?)",
			},
			{
				Index:    "edges_to",
				Decision: "keep",
				Reason:   "pinned by INDEXED BY in ListIncomingRelationEdges; name must survive unchanged",
				PlanEvidence: "ListIncomingRelationEdges / ListEdgesTo: SEARCH edges USING INDEX " +
					"edges_to (to_id=? AND kind=?)",
			},
			{
				Index:    "edges_fact",
				Decision: "keep",
				Reason: "the only fact_id-keyed b-tree: the bounded edge deletion covering-seeks it " +
					"and the reconciliation existence probe seeks it; widening to (fact_id, from_id) " +
					"was rejected as a byte increase that eliminates no scan",
				PlanEvidence: "DeleteEdgesByDirtyFactBatch: SEARCH edges USING COVERING INDEX " +
					"edges_fact (fact_id=?); EnqueueDirtyFacts probe: SEARCH edges USING INDEX " +
					"edges_fact (fact_id=?)",
			},
			{
				Index:    "facts_owner",
				Decision: "keep",
				Reason: "pinned by INDEXED BY in the first EnqueueDirtyFacts arm; also serves the " +
					"owner-scoped fact deletion",
				PlanEvidence: "EnqueueDirtyFacts arm one: SEARCH facts USING INDEX facts_owner " +
					"(owner_file=?); DeleteFactsByOwner seeks the same",
			},
			{
				Index:        "facts_from_id",
				Decision:     "keep",
				Reason:       "pinned by INDEXED BY in the second EnqueueDirtyFacts arm",
				PlanEvidence: "EnqueueDirtyFacts arm two: SEARCH facts USING INDEX facts_from_id (from_id=?)",
			},
			{
				Index:        "facts_target_id",
				Decision:     "keep",
				Reason:       "pinned by INDEXED BY in the third EnqueueDirtyFacts arm",
				PlanEvidence: "EnqueueDirtyFacts arm three: SEARCH facts USING INDEX facts_target_id (target_id=?)",
			},
			{
				Index:    "facts_source",
				Decision: "keep",
				Reason:   "pinned by INDEXED BY in the fourth EnqueueDirtyFacts arm",
				PlanEvidence: "EnqueueDirtyFacts arm four: SEARCH facts USING INDEX facts_source (source=? " +
					"AND source_kind=?)",
			},
			{
				Index:    "facts_target",
				Decision: "keep",
				Reason:   "pinned by INDEXED BY in the fifth EnqueueDirtyFacts arm",
				PlanEvidence: "EnqueueDirtyFacts arm five: SEARCH facts USING INDEX facts_target (target=? " +
					"AND target_kind=?)",
			},
			{
				Index:    "dirty_facts_order",
				Decision: "keep",
				Reason: "pinned by INDEXED BY in three queue statements; the WITHOUT ROWID re-creation " +
					"of dirty_facts leaves this secondary index untouched",
				PlanEvidence: "DeleteDirtyFactBatch / DeleteEdgesByDirtyFactBatch / ListDirtyFactBatch: " +
					"SCAN dirty_facts USING COVERING INDEX dirty_facts_order (the bounded batch walk)",
			},
			{
				Index:    "embeddings_model",
				Decision: "keep",
				Reason: "serves the per-model listing with its ORDER BY node_id satisfied by the " +
					"second key column",
				PlanEvidence: "ListEmbeddingsByModel: SEARCH embeddings USING INDEX embeddings_model (model=?)",
			},
			{
				Index:    "meta",
				Decision: "reformed",
				Reason: "natural-key table re-created WITHOUT ROWID: the primary key becomes the " +
					"clustered b-tree and sqlite_autoindex_meta_1 disappears",
				PlanEvidence: "GetMeta: SEARCH meta USING INDEX sqlite_autoindex_meta_1 (key=?) in " +
					"production; the reformed schema serves the same probe from the clustered key " +
					"with no autoindex object",
			},
			{
				Index:    "files",
				Decision: "reformed",
				Reason: "natural-key table re-created WITHOUT ROWID; at the baseline scale the table " +
					"plus its autoindex held 36864 bytes, of which the autoindex's 16384 bytes drop",
				PlanEvidence: "ListFiles walks the clustered key in path order; UpsertFile and " +
					"DeleteFile probe the path primary key, which the WITHOUT ROWID storage serves " +
					"without the implicit autoindex",
			},
			{
				Index:    "dirty_owners",
				Decision: "reformed",
				Reason:   "bounded natural-key queue re-created WITHOUT ROWID; autoindex drops",
				PlanEvidence: "MarkDirtyOwner / ClearDirtyOwners operate on the clustered primary key; " +
					"the queue enumeration scans the single b-tree",
			},
			{
				Index:    "dirty_nodes",
				Decision: "reformed",
				Reason:   "bounded natural-key queue re-created WITHOUT ROWID; autoindex drops",
				PlanEvidence: "MarkDirtyNode / ClearDirtyNodes operate on the clustered primary key; " +
					"the EnqueueDirtyFacts driving scan stays a bounded queue walk",
			},
			{
				Index:    "dirty_targets",
				Decision: "reformed",
				Reason: "composite primary key (target, target_kind) re-created WITHOUT ROWID so it " +
					"clusters the table instead of costing sqlite_autoindex_dirty_targets_1",
				PlanEvidence: "MarkDirtyTarget INSERT OR IGNORE enforces the composite key on the " +
					"clustered b-tree; the EnqueueDirtyFacts driving scan stays a bounded queue walk",
			},
			{
				Index:    "dirty_facts",
				Decision: "reformed",
				Reason: "natural-key queue re-created WITHOUT ROWID; the pinned dirty_facts_order " +
					"secondary index stays",
				PlanEvidence: "the three INDEXED BY dirty_facts_order statements keep their covering " +
					"batch walk; the fact_id primary-key probes (PruneDirtyFacts) hit the clustered key",
			},
			{
				Index:    "nodes_qualified_resolve COLLATE NOCASE composite (replacing nodes_qualified + nodes_qualified_resolve)",
				Decision: "reverted",
				Reason: "fail-closed reversion: MatchNodesByQualifiedName kept its seek on the widened " +
					"composite, but every binary-collation resolution query regressed from a covering " +
					"seek to a full index scan, so the element was rolled back to the production pair",
				PlanEvidence: "attempt: SEARCH nodes USING INDEX nodes_qualified_resolve " +
					"(qualified_name=? AND external=?) for the NOCASE match; regression: FindNodesExact / " +
					"FindNodesExactKind degrade to SCAN nodes USING COVERING INDEX " +
					"nodes_qualified_resolve and ListExternalNodesMatching loses its seek entirely " +
					"(SCAN nodes USING INDEX nodes_external)",
			},
			{
				Index:    "nodes_name_resolve COLLATE NOCASE composite (replacing nodes_name + nodes_name_resolve)",
				Decision: "reverted",
				Reason:   "fail-closed reversion, same measurement as the qualified-name pair",
				PlanEvidence: "attempt: SEARCH nodes USING INDEX nodes_name_resolve (name=? AND " +
					"external=?) for the NOCASE match; regression: FindNodesExact arm two degrades to " +
					"SCAN nodes USING COVERING INDEX nodes_name_resolve and the external matching " +
					"joins lose their name_resolve seek",
			},
		},
	}
}
