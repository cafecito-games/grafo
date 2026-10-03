package parser_test

import (
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/parser"
)

func TestBuilderFileIDUsesRepositoryIdentity(t *testing.T) {
	input := parser.Input{Repository: "same-name", RepoID: "repo:first", Path: "main.go"}
	builder := parser.NewBuilder(input, "go")

	want := graph.NodeID(graph.KindFile, "repo:first:main.go")
	if builder.FileID() != want {
		t.Fatalf("file ID = %q, want %q", builder.FileID(), want)
	}
	if len(builder.Result.Nodes) != 1 || builder.Result.Nodes[0].ID != want {
		t.Fatalf("file node and builder disagree: %#v", builder.Result.Nodes)
	}

	other := parser.NewBuilder(parser.Input{Repository: "same-name", RepoID: "repo:second", Path: "main.go"}, "go")
	if other.FileID() == builder.FileID() {
		t.Fatal("different repository identities produced the same file ID")
	}
}

func TestBuilderDistinguishesExactAndNamedSourceIdentity(t *testing.T) {
	input := parser.Input{Repository: "sample", RepoID: "repo:sample", Path: "main.gd"}
	exact := parser.NewBuilder(input, "gdscript")
	exact.AddFact("Backend.ready", graph.EdgeHandledBy, "handler", "", graph.KindMethod,
		graph.Location{Path: input.Path, Line: 4}, nil)
	named := parser.NewBuilder(input, "gdscript")
	named.AddNamedSourceFact("Backend.ready", graph.KindEvent, graph.EdgeHandledBy,
		"handler", "", graph.KindMethod, graph.Location{Path: input.Path, Line: 4}, nil)

	if len(named.Result.Facts) != 1 {
		t.Fatalf("named facts = %#v", named.Result.Facts)
	}
	fact := named.Result.Facts[0]
	if fact.FromID != "" || fact.Source != "Backend.ready" || fact.SourceKind != graph.KindEvent {
		t.Fatalf("named source fact = %#v", fact)
	}
	if fact.ID == exact.Result.Facts[0].ID {
		t.Fatalf("exact and named source locators collided at %q", fact.ID)
	}
	for _, produced := range []graph.Fact{exact.Result.Facts[0], fact} {
		if produced.Producer != graph.ProducerGDScript {
			t.Fatalf("builder fact producer = %q, want %q: %#v",
				produced.Producer, graph.ProducerGDScript, produced)
		}
	}
}

// TestSemanticLoadMetricsSinceSubtractsEveryCumulativeField pins which fields
// Since subtracts and which it carries. A loader's counters are lifetime
// totals, so a field left out of Since reports zero for every run instead of
// that run's share, and the integration tests cannot see the difference on a
// counter nothing asserts.
//
// The field count is asserted on purpose: a counter added to the struct without
// a matching line in Since fails here rather than silently reporting zero.
func TestSemanticLoadMetricsSinceSubtractsEveryCumulativeField(t *testing.T) {
	const fields = 10
	if actual := reflect.TypeOf(parser.SemanticLoadMetrics{}).NumField(); actual != fields {
		t.Fatalf("SemanticLoadMetrics has %d fields, this test knows %d; "+
			"add the new field to Since and to the expectation below", actual, fields)
	}

	baseline := parser.SemanticLoadMetrics{
		Loads: 1, CacheHits: 2, PersistedHits: 3, PersistedSaves: 4,
		PersistedDecodes: 5, PersistedSegmentWrites: 6,
		PeakConcurrent: 7, LastDurationMS: 8, LoadNS: 9, DerivationNS: 10,
	}
	current := parser.SemanticLoadMetrics{
		Loads: 11, CacheHits: 22, PersistedHits: 33, PersistedSaves: 44,
		PersistedDecodes: 55, PersistedSegmentWrites: 66,
		PeakConcurrent: 77, LastDurationMS: 88, LoadNS: 99, DerivationNS: 110,
	}
	want := parser.SemanticLoadMetrics{
		Loads: 10, CacheHits: 20, PersistedHits: 30, PersistedSaves: 40,
		PersistedDecodes: 50, PersistedSegmentWrites: 60,
		// Observations rather than totals, so they are carried as they stand.
		PeakConcurrent: 77, LastDurationMS: 88,
		LoadNS: 90, DerivationNS: 100,
	}
	if actual := current.Since(baseline); actual != want {
		t.Fatalf("Since = %+v, want %+v", actual, want)
	}
}

// TestSemanticLoadMetricsSinceOfAnEmptyBaselineIsWhole covers the first run
// through a registry, and any language whose parser was absent from an earlier
// snapshot: everything it counted belongs to this run.
func TestSemanticLoadMetricsSinceOfAnEmptyBaselineIsWhole(t *testing.T) {
	current := parser.SemanticLoadMetrics{Loads: 3, LoadNS: 500, DerivationNS: 200}
	if actual := current.Since(parser.SemanticLoadMetrics{}); actual != current {
		t.Fatalf("Since of a zero baseline = %+v, want %+v", actual, current)
	}
}
