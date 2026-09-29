package layoutbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// PlanQuery pairs one production query with the representative parameters
// CapturePlans binds when explaining it. Params is ordered by placeholder
// binding: values for each distinct named parameter (@name, the sqlc
// convention) in order of first appearance, followed by values for each
// positional ? placeholder in order of appearance. CapturePlans rewrites the
// named placeholders to numbered ?N parameters, so both kinds bind through
// database/sql positional arguments against any SQLite schema.
//
// HighCardinality marks queries whose nodes/facts/edges access
// must stay index-backed at graph scale. Queries that enumerate a bounded
// queue table, aggregate a whole table through a covering index, or accept a
// substring scan by design are false: flagging them would turn the accepted
// scan into noise.
type PlanQuery struct {
	Name            string `json:"name"`             // matches the sqlc -- name: in queries/*.sql
	SQL             string `json:"sql"`              // the named query body from queries/*.sql
	Params          []any  `json:"-"`                // representative parameters, applied positionally via database/sql
	HighCardinality bool   `json:"high_cardinality"` // true when the path touches nodes/facts/edges at scale
}

// PlanCapture records one explained query. Valid is false when the query
// failed to prepare or ValidatePlans flagged a plan problem; Reason carries
// the offending step details only for invalid captures.
type PlanCapture struct {
	Query  PlanQuery  `json:"query"`
	Valid  bool       `json:"valid"`
	Reason string     `json:"reason,omitempty"`
	Steps  []PlanStep `json:"steps"`
}

// PlanStep is one EXPLAIN QUERY PLAN row stored verbatim.
type PlanStep struct {
	SelectID int64  `json:"select_id"`
	Order    int64  `json:"order"`
	From     int64  `json:"from"`
	Detail   string `json:"detail"`
}

// CapturePlans runs EXPLAIN QUERY PLAN for every query against db and stores
// each step verbatim. A query that fails to rewrite, bind, or prepare (for
// example an INDEXED BY name missing from this schema) is recorded as an
// invalid capture with the error in Reason rather than failing the call, so
// one broken variant query cannot hide the rest. The returned error is
// reserved for failures that stop step iteration itself.
func CapturePlans(ctx context.Context, db *sql.DB, queries []PlanQuery) ([]PlanCapture, error) {
	if db == nil {
		return nil, errors.New("CapturePlans requires a non-nil database")
	}
	captures := make([]PlanCapture, 0, len(queries))
	for _, query := range queries {
		capture := PlanCapture{Query: query, Valid: true}
		rewritten, parameterCount := rewriteNamedParameters(query.SQL)
		if len(query.Params) != parameterCount {
			captures = append(captures, invalidate(capture,
				"query binds %d parameters but %d were provided", parameterCount, len(query.Params)))
			continue
		}
		rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+rewritten, query.Params...)
		if err != nil {
			captures = append(captures, invalidate(capture, "explain query plan: %v", err))
			continue
		}
		for rows.Next() {
			var step PlanStep
			if err := rows.Scan(&step.SelectID, &step.Order, &step.From, &step.Detail); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan plan step for %s: %w", query.Name, err)
			}
			capture.Steps = append(capture.Steps, step)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate plan steps for %s: %w", query.Name, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close plan rows for %s: %w", query.Name, err)
		}
		captures = append(captures, capture)
	}
	return captures, nil
}

func invalidate(capture PlanCapture, format string, arguments ...any) PlanCapture {
	capture.Valid = false
	capture.Reason = fmt.Sprintf(format, arguments...)
	return capture
}

// ValidatePlans annotates every capture in place. A capture is invalid when a
// HighCardinality query has any step that scans a table without an index
// (index-covered scans are valid), or when the SQL pins INDEXED BY a name no
// plan step references because this schema lacks that index.
func ValidatePlans(captures []PlanCapture) []PlanCapture {
	result := make([]PlanCapture, len(captures))
	for index, capture := range captures {
		result[index] = ValidatePlan(capture)
	}
	return result
}

// ValidatePlan applies the ValidatePlans rules to a single capture. Captures
// already marked invalid by CapturePlans keep their reason.
func ValidatePlan(capture PlanCapture) PlanCapture {
	if !capture.Valid {
		return capture
	}
	var reasons []string
	if capture.Query.HighCardinality {
		for _, step := range capture.Steps {
			if isUncoveredScan(step.Detail) {
				reasons = append(reasons, fmt.Sprintf("unindexed full scan on high-cardinality path: %q", step.Detail))
			}
		}
	}
	for _, indexName := range indexedByNames(capture.Query.SQL) {
		if !stepsReferenceIndex(capture.Steps, indexName) {
			reasons = append(reasons, fmt.Sprintf("INDEXED BY %s but no plan step uses that index", indexName))
		}
	}
	if len(reasons) > 0 {
		capture.Valid = false
		capture.Reason = strings.Join(reasons, "; ")
	}
	return capture
}

// isUncoveredScan reports whether a step detail is a table scan the query
// planner cannot serve from an index. SQLite reports index-served scans as
// "SCAN table USING INDEX ..." or "SCAN table USING COVERING INDEX ...";
// everything else after "SCAN" walks the table b-tree itself. Subquery
// materializations ("SCAN subquery N", "SCAN (subquery N)") are not table
// scans: the planner explains their inner steps as separate rows. The json_each
// prefix ephemeris is the one exempt virtual-table scan; every other — a graph
// table behind a virtual-table module — counts as uncovered.
func isUncoveredScan(detail string) bool {
	rest, found := strings.CutPrefix(detail, "SCAN ")
	if !found {
		return false
	}
	target := strings.ToLower(strings.TrimSpace(rest))
	if strings.HasPrefix(target, "subquery") || strings.HasPrefix(target, "(") {
		return false
	}
	if isJSONEachPrefixScan(detail) {
		return false
	}
	return !strings.Contains(detail, "USING INDEX") && !strings.Contains(detail, "USING COVERING INDEX")
}

// isJSONEachPrefixScan reports whether a step detail is the json_each
// ephemeris the path-prefix filter walks. EXPLAIN QUERY PLAN names that
// ephemeris by its query alias, so the match is pinned to the exact detail
// SQLite emits for json_each(@path_prefixes_json) AS prefix:
// "SCAN prefix VIRTUAL TABLE INDEX 1:". The row count is bounded by the
// caller-bound prefixes JSON, never by a graph table.
func isJSONEachPrefixScan(detail string) bool {
	rest, found := strings.CutPrefix(detail, "SCAN prefix ")
	return found && strings.HasPrefix(rest, "VIRTUAL TABLE INDEX")
}

var indexedByPattern = regexp.MustCompile(`(?i)\bINDEXED\s+BY\s+([A-Za-z_][A-Za-z0-9_]*)`)

func indexedByNames(query string) []string {
	matches := indexedByPattern.FindAllStringSubmatch(query, -1)
	names := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		name := match[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// stepsReferenceIndex reports whether any step detail names the index as a
// whole token, so an index whose name merely extends the pinned one (say
// edges_from_v2 for INDEXED BY edges_from) cannot pass for it.
func stepsReferenceIndex(steps []PlanStep, indexName string) bool {
	for _, step := range steps {
		if slices.Contains(strings.FieldsFunc(step.Detail, isIndexTokenSeparator), indexName) {
			return true
		}
	}
	return false
}

func isIndexTokenSeparator(char rune) bool {
	return char != '_' && !unicode.IsLetter(char) && !unicode.IsDigit(char)
}

// rewriteNamedParameters converts @name placeholders to numbered ?N
// parameters: each distinct name is numbered in order of first appearance,
// then positional ? placeholders continue the numbering in order of
// appearance. Placeholder-like text inside string literals and comments is
// left verbatim. It returns the rewritten SQL and the number of values the
// caller must bind.
func rewriteNamedParameters(query string) (string, int) {
	names, positionalCount := scanPlaceholders(query)
	nameNumbers := make(map[string]int, len(names))
	for index, name := range names {
		nameNumbers[name] = index + 1
	}
	var rewritten strings.Builder
	positionalSeen := 0
	walkPlaceholders(query, func(kind byte, name string) {
		if kind == '?' {
			positionalSeen++
			fmt.Fprintf(&rewritten, "?%d", len(names)+positionalSeen)
			return
		}
		fmt.Fprintf(&rewritten, "?%d", nameNumbers[name])
	}, func(segment string) {
		rewritten.WriteString(segment)
	})
	return rewritten.String(), len(names) + positionalCount
}

// scanPlaceholders collects the distinct @name placeholders in order of first
// appearance and counts positional ? placeholders.
func scanPlaceholders(query string) ([]string, int) {
	var names []string
	seen := map[string]bool{}
	positionalCount := 0
	walkPlaceholders(query, func(kind byte, name string) {
		if kind == '?' {
			positionalCount++
			return
		}
		if seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}, nil)
	return names, positionalCount
}

// walkPlaceholders walks query outside of single-quoted strings and comments,
// invoking onPlaceholder for each @name (kind '@') and ? (kind '?') token and
// onSegment for the verbatim runs between tokens.
func walkPlaceholders(query string, onPlaceholder func(kind byte, name string), onSegment func(segment string)) {
	index := 0
	segmentStart := 0
	flushSegment := func(end int) {
		if onSegment != nil && end > segmentStart {
			onSegment(query[segmentStart:end])
		}
	}
	for index < len(query) {
		switch {
		case query[index] == '\'':
			index++ // opening quote
			for index < len(query) {
				if query[index] == '\'' {
					if index+1 < len(query) && query[index+1] == '\'' {
						index += 2 // doubled quote escapes within the literal
						continue
					}
					index++ // closing quote
					break
				}
				index++
			}
		case strings.HasPrefix(query[index:], "--"):
			for index < len(query) && query[index] != '\n' {
				index++
			}
		case strings.HasPrefix(query[index:], "/*"):
			index += 2
			for index < len(query) && !strings.HasPrefix(query[index:], "*/") {
				index++
			}
			index = min(index+2, len(query))
		case query[index] == '?':
			flushSegment(index)
			index++
			// Consume digits of an already-numbered placeholder so it is
			// rewritten, not concatenated onto the new number.
			for index < len(query) && query[index] >= '0' && query[index] <= '9' {
				index++
			}
			if onPlaceholder != nil {
				onPlaceholder('?', "")
			}
			segmentStart = index
		case query[index] == '@':
			end := index + 1
			for end < len(query) && isIdentifierByte(query[end]) {
				end++
			}
			if end == index+1 {
				index++ // a lone @ is literal text
				continue
			}
			flushSegment(index)
			if onPlaceholder != nil {
				onPlaceholder('@', query[index+1:end])
			}
			index = end
			segmentStart = index
		default:
			index++
		}
	}
	flushSegment(len(query))
}

func isIdentifierByte(char byte) bool {
	return char == '_' || (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
}

// PlanQueriesFor starts from the production inventory and swaps in overridden
// SQL by query name, so variant schemas explain the same query inventory. It
// also returns the override names that matched no inventory entry; callers
// must fail loudly on a non-empty unmatched set, because a typo'd query name
// would otherwise silently measure production SQL against a variant schema.
// The returned queries are a fresh slice, so the production inventory is never
// mutated.
func PlanQueriesFor(sqlOverrides map[string]string) ([]PlanQuery, []string) {
	queries := ProductionPlanQueries()
	var unmatched []string
	matched := make(map[string]bool, len(sqlOverrides))
	for index := range queries {
		override, found := sqlOverrides[queries[index].Name]
		if !found {
			continue
		}
		queries[index].SQL = override
		matched[queries[index].Name] = true
	}
	for name := range sqlOverrides {
		if !matched[name] {
			unmatched = append(unmatched, name)
		}
	}
	sort.Strings(unmatched)
	return queries, unmatched
}

// Representative parameter values shared across the inventory. They only feed
// EXPLAIN QUERY PLAN, which prepares without executing, so they must be
// plausible to SQLite, never live.
const (
	planOwner         = "src/fixture-000001.go"
	planNodeID        = "n:fixture-000001"
	planCounterpart   = "n:fixture-000002"
	planFactID        = "f:fixture-000001"
	planName          = "Function000001"
	planQualifiedName = "pkg.Function000001"
	planRelation      = "calls"
	planLimit         = 25
)

// ProductionPlanQueries inventories every named query in
// internal/storage/sqlite/queries/*.sql, sorted by name. The SQL bodies are
// copied verbatim; TestPlanInventoryMatchesQueryFiles fails when either the
// names or the bodies drift from the sqlc sources.
func ProductionPlanQueries() []PlanQuery {
	queries := []PlanQuery{
		// dirty.sql — the reconciliation queue. Its driving tables are bounded
		// by reconciliation, so enumerating them is a scan by design.
		{
			Name:   "MarkDirtyOwner",
			SQL:    `INSERT OR IGNORE INTO dirty_owners(owner_file) VALUES (?);`,
			Params: []any{planOwner},
		},
		{
			Name:   "MarkDirtyNode",
			SQL:    `INSERT OR IGNORE INTO dirty_nodes(node_id) VALUES (?);`,
			Params: []any{planNodeID},
		},
		{
			Name:   "MarkDirtyTarget",
			SQL:    `INSERT OR IGNORE INTO dirty_targets(target, target_kind) VALUES (?, ?);`,
			Params: []any{planName, "function"},
		},
		{
			Name: "MarkOwnedNodesDirty",
			SQL: `INSERT OR IGNORE INTO dirty_nodes(node_id)
SELECT nodes.id FROM nodes WHERE nodes.owner_file = ?;`,
			Params:          []any{planOwner},
			HighCardinality: true,
		},
		{
			Name: "MarkOwnedNamesDirty",
			SQL: `INSERT OR IGNORE INTO dirty_targets(target, target_kind)
SELECT nodes.name, nodes.kind FROM nodes WHERE nodes.owner_file = ?
UNION
SELECT nodes.qualified_name, nodes.kind FROM nodes WHERE nodes.owner_file = ?;`,
			Params:          []any{planOwner, planOwner},
			HighCardinality: true,
		},
		{
			Name: "PruneDirtyFacts",
			SQL: `DELETE FROM dirty_facts
WHERE NOT EXISTS (SELECT 1 FROM facts WHERE facts.id = dirty_facts.fact_id);`,
		},
		{
			// Every facts access is pinned by INDEXED BY, so the INDEXED BY
			// rule guards the at-scale table even though the queue driving
			// tables are scanned.
			Name: "EnqueueDirtyFacts",
			SQL: `INSERT OR IGNORE INTO dirty_facts(fact_id, owner_file)
SELECT facts.id, facts.owner_file
FROM dirty_owners
CROSS JOIN facts INDEXED BY facts_owner
WHERE facts.owner_file = dirty_owners.owner_file
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_nodes
CROSS JOIN facts INDEXED BY facts_from_id
WHERE facts.from_id = dirty_nodes.node_id
  AND (
    NOT EXISTS (
      SELECT 1 FROM nodes
      WHERE nodes.id = facts.from_id AND nodes.external = 0
    )
    OR NOT EXISTS (
      SELECT 1 FROM edges
      WHERE edges.fact_id = facts.id AND edges.from_id = facts.from_id
    )
  )
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_nodes
CROSS JOIN facts INDEXED BY facts_target_id
WHERE facts.target_id = dirty_nodes.node_id
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_targets
CROSS JOIN facts INDEXED BY facts_source
WHERE facts.source = dirty_targets.target
  AND (facts.source_kind = '' OR facts.source_kind = dirty_targets.target_kind)
UNION ALL
SELECT facts.id, facts.owner_file
FROM dirty_targets
CROSS JOIN facts INDEXED BY facts_target
WHERE facts.target = dirty_targets.target
  AND (facts.target_kind = '' OR facts.target_kind = dirty_targets.target_kind);`,
		},
		{
			Name: "DeleteDirtyFactBatch",
			SQL: `DELETE FROM dirty_facts
WHERE fact_id IN (
    SELECT fact_id
    FROM dirty_facts INDEXED BY dirty_facts_order
    ORDER BY owner_file, fact_id
    LIMIT ?
);`,
			Params: []any{planLimit},
		},
		{
			Name: "CountDirtyFacts",
			SQL:  `SELECT COUNT(*) FROM dirty_facts;`,
		},
		{
			Name: "ReconciliationPending",
			SQL: `SELECT EXISTS (
    SELECT 1 FROM dirty_owners
    UNION ALL SELECT 1 FROM dirty_nodes
    UNION ALL SELECT 1 FROM dirty_targets
    UNION ALL SELECT 1 FROM dirty_facts
    UNION ALL SELECT 1 FROM reconciliation_cleanup
);`,
		},
		{
			Name: "MarkReconciliationCleanup",
			SQL: `INSERT OR IGNORE INTO reconciliation_cleanup(id)
SELECT 1 WHERE EXISTS (SELECT 1 FROM dirty_facts);`,
		},
		{
			Name: "ReconciliationCleanupPending",
			SQL:  `SELECT EXISTS (SELECT 1 FROM reconciliation_cleanup);`,
		},
		{
			Name: "ClearReconciliationCleanup",
			SQL:  `DELETE FROM reconciliation_cleanup;`,
		},
		{
			Name: "ClearDirtyOwners",
			SQL:  `DELETE FROM dirty_owners;`,
		},
		{
			Name: "ClearDirtyNodes",
			SQL:  `DELETE FROM dirty_nodes;`,
		},
		{
			Name: "ClearDirtyTargets",
			SQL:  `DELETE FROM dirty_targets;`,
		},

		// edges.sql
		{
			Name: "DeleteAllEdges",
			SQL:  `DELETE FROM edges;`,
		},
		{
			Name: "DeleteEdgesByDirtyFactBatch",
			SQL: `DELETE FROM edges
WHERE fact_id IN (
    SELECT fact_id
    FROM dirty_facts INDEXED BY dirty_facts_order
    ORDER BY owner_file, fact_id
    LIMIT ?
);`,
			Params:          []any{planLimit},
			HighCardinality: true,
		},
		{
			Name:            "DeleteEdgesByOwnerFacts",
			SQL:             `DELETE FROM edges WHERE fact_id IN (SELECT id FROM facts WHERE owner_file = ?);`,
			Params:          []any{planOwner},
			HighCardinality: true,
		},
		{
			Name: "InsertEdge",
			SQL: `INSERT INTO edges(
    id, fact_id, from_id, to_id, kind, producer, path, line, column_no, end_line, properties
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`,
			Params: []any{"e:fixture-000001", planFactID, planNodeID, planCounterpart, planRelation,
				"parser", planOwner, 10, 1, 10, "{}"},
		},
		{
			Name:            "ListEdgesFrom",
			SQL:             `SELECT * FROM edges WHERE from_id = ? ORDER BY kind, to_id, id;`,
			Params:          []any{planNodeID},
			HighCardinality: true,
		},
		{
			Name:            "ListEdgesTo",
			SQL:             `SELECT * FROM edges WHERE to_id = ? ORDER BY kind, from_id, id;`,
			Params:          []any{planNodeID},
			HighCardinality: true,
		},
		{
			Name: "ListIncomingRelationEdges",
			SQL: `SELECT
    edges.id AS edge_id,
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    edges.producer AS edge_producer,
    edges.path AS edge_path,
    edges.line AS edge_line,
    edges.column_no AS edge_column_no,
    edges.end_line AS edge_end_line,
    edges.properties AS edge_properties,
    COALESCE(nodes.id, '') AS counterpart_id,
    COALESCE(nodes.kind, '') AS counterpart_kind,
    COALESCE(nodes.name, '') AS counterpart_name,
    COALESCE(nodes.qualified_name, '') AS counterpart_qualified_name,
    COALESCE(nodes.language, '') AS counterpart_language,
    COALESCE(nodes.path, '') AS counterpart_path,
    COALESCE(nodes.line, 0) AS counterpart_line,
    COALESCE(nodes.column_no, 0) AS counterpart_column_no,
    COALESCE(nodes.end_line, 0) AS counterpart_end_line,
    COALESCE(nodes.properties, '{}') AS counterpart_properties,
    COALESCE(nodes.owner_file, '') AS counterpart_owner_file,
    COALESCE(nodes.external, 0) AS counterpart_external
FROM edges INDEXED BY edges_to
LEFT JOIN nodes ON nodes.id = edges.from_id
WHERE edges.to_id = @subject_id AND edges.kind = @relation
ORDER BY edges.from_id, edges.id
LIMIT @max_results;`,
			Params:          []any{planNodeID, planRelation, planLimit},
			HighCardinality: true,
		},
		{
			Name: "ListOutgoingRelationEdges",
			SQL: `SELECT
    edges.id AS edge_id,
    edges.fact_id AS edge_fact_id,
    edges.from_id AS edge_from_id,
    edges.to_id AS edge_to_id,
    edges.kind AS edge_kind,
    edges.producer AS edge_producer,
    edges.path AS edge_path,
    edges.line AS edge_line,
    edges.column_no AS edge_column_no,
    edges.end_line AS edge_end_line,
    edges.properties AS edge_properties,
    COALESCE(nodes.id, '') AS counterpart_id,
    COALESCE(nodes.kind, '') AS counterpart_kind,
    COALESCE(nodes.name, '') AS counterpart_name,
    COALESCE(nodes.qualified_name, '') AS counterpart_qualified_name,
    COALESCE(nodes.language, '') AS counterpart_language,
    COALESCE(nodes.path, '') AS counterpart_path,
    COALESCE(nodes.line, 0) AS counterpart_line,
    COALESCE(nodes.column_no, 0) AS counterpart_column_no,
    COALESCE(nodes.end_line, 0) AS counterpart_end_line,
    COALESCE(nodes.properties, '{}') AS counterpart_properties,
    COALESCE(nodes.owner_file, '') AS counterpart_owner_file,
    COALESCE(nodes.external, 0) AS counterpart_external
FROM edges INDEXED BY edges_from
LEFT JOIN nodes ON nodes.id = edges.to_id
WHERE edges.from_id = @subject_id AND edges.kind = @relation
ORDER BY edges.to_id, edges.id
LIMIT @max_results;`,
			Params:          []any{planNodeID, planRelation, planLimit},
			HighCardinality: true,
		},
		{
			Name: "ListExternalEdgesMatching",
			SQL: `SELECT edges.*
FROM edges
JOIN nodes ON nodes.id = edges.to_id
WHERE nodes.external = 1
  AND (
      nodes.qualified_name = @qualified_name
      OR nodes.qualified_name = @name
      OR nodes.name = @qualified_name
      OR nodes.name = @name
  )
ORDER BY edges.kind, edges.from_id, edges.id;`,
			Params:          []any{planQualifiedName, planName},
			HighCardinality: true,
		},
		{
			Name: "CountEdges",
			SQL:  `SELECT COUNT(*) FROM edges;`,
		},
		{
			Name: "CountEdgesByKind",
			SQL:  `SELECT kind, COUNT(*) AS count FROM edges GROUP BY kind ORDER BY kind;`,
		},

		// candidates.sql
		{
			Name: "ListSemanticCandidateNodes",
			SQL: `SELECT * FROM nodes
WHERE external = 0
  AND kind IN ('function', 'method', 'type', 'class', 'interface', 'endpoint')
ORDER BY qualified_name, id;`,
			HighCardinality: true,
		},

		// facts.sql
		{
			Name: "UpsertFact",
			SQL: `INSERT INTO facts(
    id, from_id, source, source_kind, kind, producer, target_id, target, target_kind,
    path, line, column_no, end_line, properties, owner_file
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    from_id = excluded.from_id,
    source = excluded.source,
    source_kind = excluded.source_kind,
    kind = excluded.kind,
    producer = excluded.producer,
    target_id = excluded.target_id,
    target = excluded.target,
    target_kind = excluded.target_kind,
    path = excluded.path,
    line = excluded.line,
    column_no = excluded.column_no,
    end_line = excluded.end_line,
    properties = excluded.properties,
    owner_file = excluded.owner_file;`,
			Params: []any{planFactID, planNodeID, planQualifiedName, "function", planRelation, "parser",
				planCounterpart, planQualifiedName, "function", planOwner, 10, 1, 10, "{}", planOwner},
		},
		{
			Name:            "DeleteFactsByOwner",
			SQL:             `DELETE FROM facts WHERE owner_file = ?;`,
			Params:          []any{planOwner},
			HighCardinality: true,
		},
		{
			Name: "CountFacts",
			SQL:  `SELECT COUNT(*) FROM facts;`,
		},
		{
			Name: "ListDirtyFactBatch",
			SQL: `SELECT facts.id, facts.from_id, facts.source, facts.source_kind, facts.kind, facts.producer,
       facts.target_id, facts.target, facts.target_kind, facts.path, facts.line,
       facts.column_no, facts.end_line, facts.properties, facts.owner_file,
       CASE WHEN facts.from_id = '' OR source_node.id IS NOT NULL THEN 1 ELSE 0 END AS source_exists,
       CASE WHEN facts.target_id = '' OR target_node.id IS NOT NULL THEN 1 ELSE 0 END AS target_exists
FROM dirty_facts INDEXED BY dirty_facts_order
CROSS JOIN facts
LEFT JOIN nodes AS source_node ON source_node.id = facts.from_id AND source_node.external = 0
LEFT JOIN nodes AS target_node ON target_node.id = facts.target_id
WHERE facts.id = dirty_facts.fact_id
ORDER BY dirty_facts.owner_file, dirty_facts.fact_id
LIMIT ?;`,
			Params:          []any{planLimit},
			HighCardinality: true,
		},

		// files.sql — the files table is one row per source file and its
		// access patterns are primary-key or whole-table by design.
		{
			Name: "ListFiles",
			SQL:  `SELECT path, hash, language, size, modified_ns, indexed_at FROM files ORDER BY path;`,
		},
		{
			Name: "UpsertFile",
			SQL: `INSERT INTO files(path, hash, language, size, modified_ns, indexed_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
    hash = excluded.hash,
    language = excluded.language,
    size = excluded.size,
    modified_ns = excluded.modified_ns,
    indexed_at = excluded.indexed_at;`,
			Params: []any{planOwner, "0123456789abcdef", "go", 4096, 1767225600000000000, "2026-01-01T00:00:00Z"},
		},
		{
			Name:   "DeleteFile",
			SQL:    `DELETE FROM files WHERE path = ?;`,
			Params: []any{planOwner},
		},
		{
			Name: "CountFiles",
			SQL:  `SELECT COUNT(*) FROM files;`,
		},

		// meta.sql — a tiny key-value table.
		{
			Name: "SetMeta",
			SQL: `INSERT INTO meta(key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value;`,
			Params: []any{"schema_version", "2"},
		},
		{
			Name:   "GetMeta",
			SQL:    `SELECT value FROM meta WHERE key = ?;`,
			Params: []any{"schema_version"},
		},

		// nodes.sql
		{
			Name: "UpsertNode",
			SQL: `INSERT INTO nodes(
    id, kind, name, qualified_name, language, path, line, column_no, end_line,
    properties, owner_file, external, name_folded, qualified_name_folded
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    kind = excluded.kind,
    name = excluded.name,
    qualified_name = excluded.qualified_name,
    language = excluded.language,
    path = excluded.path,
    line = excluded.line,
    column_no = excluded.column_no,
    end_line = excluded.end_line,
    properties = excluded.properties,
    owner_file = excluded.owner_file,
    external = excluded.external,
    name_folded = excluded.name_folded,
    qualified_name_folded = excluded.qualified_name_folded;`,
			Params: []any{planNodeID, "function", planName, planQualifiedName, "go", planOwner, 10, 1, 10,
				"{}", planOwner, 0, planName, planQualifiedName},
		},
		{
			Name:            "DeleteNodesByOwner",
			SQL:             `DELETE FROM nodes WHERE owner_file = ?;`,
			Params:          []any{planOwner},
			HighCardinality: true,
		},
		{
			Name:            "DeleteExternalNodes",
			SQL:             `DELETE FROM nodes WHERE owner_file = '__external__';`,
			HighCardinality: true,
		},
		{
			Name: "DeleteOrphanExternalNodes",
			SQL: `DELETE FROM nodes
WHERE external = 1
  AND NOT EXISTS (SELECT 1 FROM edges WHERE edges.to_id = nodes.id OR edges.from_id = nodes.id);`,
			HighCardinality: true,
		},
		{
			Name:            "GetNode",
			SQL:             `SELECT * FROM nodes WHERE id = ?;`,
			Params:          []any{planNodeID},
			HighCardinality: true,
		},
		{
			Name: "ListExternalNodesMatching",
			SQL: `SELECT * FROM nodes
WHERE external = 1
  AND (
      qualified_name = @qualified_name
      OR qualified_name = @name
      OR name = @qualified_name
      OR name = @name
  )
ORDER BY id;`,
			Params:          []any{planQualifiedName, planName},
			HighCardinality: true,
		},
		{
			Name: "FindNodesExact",
			SQL: `SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_qualified_resolve
WHERE nodes.qualified_name = @target AND nodes.external = 0
UNION ALL
SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_name_resolve
WHERE nodes.name = @target AND nodes.external = 0 AND nodes.qualified_name != @target;`,
			Params:          []any{planQualifiedName},
			HighCardinality: true,
		},
		{
			Name: "FindNodesExactKind",
			SQL: `SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_qualified_resolve
WHERE nodes.qualified_name = @target AND nodes.external = 0 AND nodes.kind = @kind
UNION ALL
SELECT nodes.id, nodes.kind, nodes.qualified_name
FROM nodes INDEXED BY nodes_name_resolve
WHERE nodes.name = @target AND nodes.external = 0 AND nodes.kind = @kind AND nodes.qualified_name != @target;`,
			Params:          []any{planQualifiedName, "function"},
			HighCardinality: true,
		},
		{
			// Substring search cannot be served by the b-tree indexes; the
			// whole-table scan is the accepted production plan.
			Name: "SearchNodes",
			SQL: `SELECT * FROM nodes
WHERE name_folded LIKE '%' || CAST(@term AS TEXT) || '%'
   OR qualified_name_folded LIKE '%' || CAST(@term AS TEXT) || '%'
ORDER BY
    external, length(qualified_name), qualified_name, id
LIMIT @max_results;`,
			Params: []any{planName, planLimit},
		},
		{
			Name: "CountNodes",
			SQL:  `SELECT COUNT(*) FROM nodes;`,
		},
		{
			Name: "CountExternalNodes",
			SQL:  `SELECT COUNT(*) FROM nodes WHERE external = 1;`,
		},
		{
			Name: "CountNodesByKind",
			SQL:  `SELECT kind, COUNT(*) AS count FROM nodes GROUP BY kind ORDER BY kind;`,
		},
		{
			Name: "ListNodesByKind",
			SQL: `SELECT * FROM nodes
WHERE kind = @kind
  AND external >= @min_external
  AND external <= @max_external
  AND (instr(name_folded, @name_fragment) > 0 OR instr(qualified_name_folded, @name_fragment) > 0)
  AND (
    @path_prefixes_json = '[]'
    OR EXISTS (
      SELECT 1 FROM json_each(@path_prefixes_json) AS prefix
      WHERE nodes.path = prefix.value
         OR substr(nodes.path, 1, length(prefix.value) + 1) = prefix.value || '/'
    )
  )
ORDER BY qualified_name, id
LIMIT @max_results;`,
			Params:          []any{"function", 0, 0, planName, "[]", planLimit},
			HighCardinality: true,
		},
		{
			Name: "ListCanonicalMessages",
			SQL: `SELECT * FROM nodes
WHERE kind = 'type'
  AND external = 0
  AND json_extract(properties, '$.declaration') = 'message'
  AND (
      CAST(@package_name AS TEXT) = ''
      OR substr(qualified_name, 1, length(CAST(@package_name AS TEXT)) + 1) = CAST(@package_name AS TEXT) || '.'
  )
  AND (
      CAST(@message_name AS TEXT) = ''
      OR name = CAST(@message_name AS TEXT)
      OR qualified_name = CAST(@message_name AS TEXT)
  )
  AND (
    @path_prefixes_json = '[]'
    OR EXISTS (
      SELECT 1 FROM json_each(@path_prefixes_json) AS prefix
      WHERE nodes.path = prefix.value
         OR substr(nodes.path, 1, length(prefix.value) + 1) = prefix.value || '/'
    )
  )
ORDER BY qualified_name, id
LIMIT @max_results;`,
			Params:          []any{"pkg", "Message", "[]", planLimit},
			HighCardinality: true,
		},
		{
			Name: "MatchNodesByQualifiedName",
			SQL: `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM (
    SELECT nodes.*, CASE WHEN qualified_name = @target THEN 0 ELSE 1 END AS strict_rank
    FROM nodes
    WHERE qualified_name = @target COLLATE NOCASE
      AND external = CAST(@external AS INTEGER)
      AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
)
ORDER BY strict_rank, length(qualified_name), qualified_name, id
LIMIT @max_results;`,
			Params:          []any{planQualifiedName, 0, "", planLimit},
			HighCardinality: true,
		},
		{
			Name: "CountNodeMatchesByQualifiedName",
			SQL: `SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN qualified_name = @target THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE qualified_name = @target COLLATE NOCASE
  AND external = CAST(@external AS INTEGER)
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT));`,
			Params:          []any{planQualifiedName, 0, ""},
			HighCardinality: true,
		},
		{
			Name: "MatchNodesByName",
			SQL: `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM (
    SELECT nodes.*, CASE WHEN name = @target THEN 0 ELSE 1 END AS strict_rank
    FROM nodes
    WHERE name = @target COLLATE NOCASE
      AND external = CAST(@external AS INTEGER)
      AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
)
ORDER BY strict_rank, length(qualified_name), qualified_name, id
LIMIT @max_results;`,
			Params:          []any{planName, 0, "", planLimit},
			HighCardinality: true,
		},
		{
			Name: "CountNodeMatchesByName",
			SQL: `SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN name = @target THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE name = @target COLLATE NOCASE
  AND external = CAST(@external AS INTEGER)
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT));`,
			Params:          []any{planName, 0, ""},
			HighCardinality: true,
		},
		{
			// The substring match level lowers both sides of the LIKE, so no
			// index can serve it; the whole-table scan is the accepted plan.
			Name: "MatchNodesBySubstring",
			SQL: `SELECT id, kind, name, qualified_name, language, path, line, column_no, end_line,
       properties, owner_file, external, name_folded, qualified_name_folded
FROM (
    SELECT nodes.*,
        CASE WHEN instr(qualified_name, @target) > 0 OR instr(name, @target) > 0 THEN 0 ELSE 1 END AS strict_rank
    FROM nodes
    WHERE (lower(name) LIKE '%' || lower(@target) || '%'
        OR lower(qualified_name) LIKE '%' || lower(@target) || '%')
      AND external = CAST(@external AS INTEGER)
      AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT))
)
ORDER BY strict_rank, length(qualified_name), qualified_name, id
LIMIT @max_results;`,
			Params: []any{planName, 0, "", planLimit},
		},
		{
			Name: "CountNodeMatchesBySubstring",
			SQL: `SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN instr(qualified_name, @target) > 0 OR instr(name, @target) > 0 THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE (lower(name) LIKE '%' || lower(@target) || '%'
    OR lower(qualified_name) LIKE '%' || lower(@target) || '%')
  AND external = CAST(@external AS INTEGER)
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT));`,
			Params: []any{planName, 0, ""},
		},
	}
	sort.Slice(queries, func(first, second int) bool { return queries[first].Name < queries[second].Name })
	return queries
}
