package layoutbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
)

// QueryMetric records one access pattern of the deterministic query workload:
// the median of the pattern's timed repetitions over the final compacted
// database and, once the report assembles ratios, the candidate's median
// divided by the control's median for the same pattern. The control's own
// ratios are 1.0.
type QueryMetric struct {
	Pattern        string  `json:"pattern"`
	Repetitions    int     `json:"repetitions"`
	MedianNS       int64   `json:"median_ns"`
	RatioToControl float64 `json:"ratio_to_control"`
}

// WorkloadParameters are the fixed query parameters of one workload run,
// derived once from the fixture's structural identifiers or from the final
// content of a corpus database, so the control layout and every candidate
// execute byte-identical statements against comparable row sets.
type WorkloadParameters struct {
	HubID                 string
	HubName               string
	HubQualifiedName      string
	AmbiguousName         string
	ExternalQualifiedName string
	ExternalName          string
}

// FixtureWorkloadParameters pins the fixture's structural probe targets: the
// hub node carries the fixture's fan-in/fan-out adjacency, the ambiguous plain
// name is declared in two files, and external symbol zero of the shared pool
// is referenced by many textual facts.
func FixtureWorkloadParameters() WorkloadParameters {
	external := externalSymbolName(0)
	return WorkloadParameters{
		HubID:                 longNodeID("hub", 0),
		HubName:               "HubDispatch",
		HubQualifiedName:      "pkg.HubDispatch",
		AmbiguousName:         ambiguousPlainName,
		ExternalQualifiedName: external,
		ExternalName:          graph.SimpleName(external),
	}
}

// DeriveWorkloadParameters derives the probe targets from the final content of
// an indexed database. ListNodesByKind orders by qualified name, so the
// derived parameters are a deterministic function of the graph content — which
// the equivalence gate proves identical across layouts — and therefore fixed
// per database yet identical between the control and every candidate.
func DeriveWorkloadParameters(ctx context.Context, repository EquivalenceRepository) (WorkloadParameters, error) {
	parameters := WorkloadParameters{}
	functions, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: []graph.NodeKind{graph.KindFunction}, Visibility: graph.LocalNodes, Limit: 1})
	if err != nil {
		return parameters, err
	}
	if len(functions) > 0 {
		hub := functions[0].Node
		parameters.HubID = hub.ID
		parameters.HubName = hub.Name
		parameters.HubQualifiedName = hub.QualifiedName
		parameters.AmbiguousName = hub.Name
	}
	externals, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: []graph.NodeKind{graph.KindExternal}, Visibility: graph.ExternalNodes, Limit: 1})
	if err != nil {
		return parameters, err
	}
	if len(externals) > 0 {
		parameters.ExternalQualifiedName = externals[0].Node.QualifiedName
		parameters.ExternalName = externals[0].Node.Name
	}
	return parameters, nil
}

// WorkloadQuery is one deterministic access-pattern probe of the production
// query surface. Run must consume its result fully so the timing covers real
// row hydration.
type WorkloadQuery struct {
	Pattern string
	Run     func(ctx context.Context, repository EquivalenceRepository) error
}

// BuildQueryWorkload renders the suite for one parameter set: node lookup,
// adjacency, hydrated relation pages, selector matching at every level,
// substring search, kind enumeration, external-node and external-edge probes,
// and the aggregate counts. Probes whose parameters are absent (an empty
// database) are skipped rather than run against invented values.
func BuildQueryWorkload(parameters WorkloadParameters) []WorkloadQuery {
	queries := []WorkloadQuery{}
	if parameters.HubID != "" {
		hubID := parameters.HubID
		queries = append(queries,
			WorkloadQuery{Pattern: "node-lookup-hub", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.Node(ctx, hubID)
				return err
			}},
			WorkloadQuery{Pattern: "edges-from-hub", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.EdgesFrom(ctx, hubID)
				return err
			}},
			WorkloadQuery{Pattern: "edges-to-hub", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.EdgesTo(ctx, hubID)
				return err
			}},
			WorkloadQuery{Pattern: "relation-edges-incoming", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: hubID,
					Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls}, Limit: 25})
				return err
			}},
			WorkloadQuery{Pattern: "relation-edges-outgoing", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: hubID,
					Direction: graph.OutgoingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls}, Limit: 25})
				return err
			}},
		)
	}
	if parameters.HubQualifiedName != "" {
		selector := parameters.HubQualifiedName
		queries = append(queries, WorkloadQuery{Pattern: "match-qualified-name", Run: func(ctx context.Context, repository EquivalenceRepository) error {
			_, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: selector})
			return err
		}})
	}
	if parameters.AmbiguousName != "" {
		selector := parameters.AmbiguousName
		queries = append(queries, WorkloadQuery{Pattern: "match-ambiguous-name", Run: func(ctx context.Context, repository EquivalenceRepository) error {
			_, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: selector})
			return err
		}})
	}
	if parameters.HubName != "" {
		term := parameters.HubName
		queries = append(queries,
			WorkloadQuery{Pattern: "match-substring", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: term, Limit: 25})
				return err
			}},
			WorkloadQuery{Pattern: "search-substring", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.SearchNodes(ctx, term, 100)
				return err
			}},
		)
	}
	queries = append(queries, WorkloadQuery{Pattern: "list-nodes-by-kind", Run: func(ctx context.Context, repository EquivalenceRepository) error {
		_, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{
			Kinds: []graph.NodeKind{graph.KindFunction}, Visibility: graph.LocalNodes, Limit: 50})
		return err
	}})
	if parameters.ExternalQualifiedName != "" {
		probe := graph.Node{QualifiedName: parameters.ExternalQualifiedName, Name: parameters.ExternalName}
		queries = append(queries,
			WorkloadQuery{Pattern: "external-node-probe", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.ExternalNodesMatching(ctx, probe)
				return err
			}},
			WorkloadQuery{Pattern: "external-edges-probe", Run: func(ctx context.Context, repository EquivalenceRepository) error {
				_, err := repository.ExternalEdgesTo(ctx, probe)
				return err
			}},
		)
	}
	queries = append(queries, WorkloadQuery{Pattern: "counts", Run: func(ctx context.Context, repository EquivalenceRepository) error {
		_, err := repository.Counts(ctx)
		return err
	}})
	return queries
}

// RunQuerySuite times every pattern repetitions times against the repository
// and records the median. One untimed warm-up invocation precedes the timed
// repetitions so a cold statement prepare or page cache miss does not pose as
// a layout regression. A pattern whose query fails fails the whole suite; a
// not-found lookup is a successful query and stays timed.
func RunQuerySuite(ctx context.Context, repository EquivalenceRepository, queries []WorkloadQuery, repetitions int) ([]QueryMetric, error) {
	if repository == nil {
		return nil, errors.New("query workload needs a repository")
	}
	if repetitions < 1 {
		return nil, fmt.Errorf("query workload needs at least one repetition, got %d", repetitions)
	}
	if len(queries) == 0 {
		return nil, errors.New("query workload needs at least one pattern")
	}
	metrics := make([]QueryMetric, 0, len(queries))
	for _, query := range queries {
		probe := func() error {
			if err := query.Run(ctx, repository); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return nil
		}
		if err := probe(); err != nil {
			return nil, fmt.Errorf("workload pattern %s: %w", query.Pattern, err)
		}
		timings := make([]time.Duration, 0, repetitions)
		for range repetitions {
			started := time.Now()
			if err := probe(); err != nil {
				return nil, fmt.Errorf("workload pattern %s: %w", query.Pattern, err)
			}
			timings = append(timings, time.Since(started))
		}
		metrics = append(metrics, QueryMetric{Pattern: query.Pattern,
			Repetitions: repetitions, MedianNS: medianDuration(timings).Nanoseconds()})
	}
	return metrics, nil
}

func medianDuration(timings []time.Duration) time.Duration {
	values := append([]time.Duration(nil), timings...)
	sort.Slice(values, func(first, second int) bool { return values[first] < values[second] })
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

// PlanInventoryForSpec returns the EXPLAIN QUERY PLAN inventory for one
// layout: the production queries with the spec's overrides applied, the
// InsertEdge parameter list fixed up for a layout whose edge row binds fewer
// values, and every candidate-only statement of the spec appended with
// representative parameters. Every returned query binds exactly as many
// values as its placeholder count requires, so CapturePlans never records a
// parameter-count invalidation for a well-formed spec.
func PlanInventoryForSpec(spec LayoutSpec) ([]PlanQuery, error) {
	queries, unmatched := PlanQueriesFor(spec.SQL)
	if edgeOverride, overridden := spec.SQL[edgeStatement]; overridden {
		_, placeholders := rewriteNamedParameters(edgeOverride)
		values := []any{"e:fixture-000001", planFactID, planNodeID, planCounterpart, planRelation,
			"parser", planOwner, 10, 1, 10, "{}", planNodeID, planCounterpart}
		if placeholders > len(values) {
			return nil, fmt.Errorf("layout %q overrides %s with more placeholders (%d) than representative values exist",
				spec.Name, edgeStatement, placeholders)
		}
		for index := range queries {
			if queries[index].Name == edgeStatement {
				queries[index].Params = values[:placeholders]
			}
		}
	}
	for _, name := range unmatched {
		_, placeholders := rewriteNamedParameters(spec.SQL[name])
		parameters := make([]any, placeholders)
		for index := range parameters {
			parameters[index] = planOwner
		}
		queries = append(queries, PlanQuery{Name: name, SQL: spec.SQL[name], Params: parameters})
	}
	sort.Slice(queries, func(first, second int) bool { return queries[first].Name < queries[second].Name })
	return queries, nil
}
