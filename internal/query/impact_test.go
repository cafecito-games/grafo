package query_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func impactNode(kind graph.NodeKind, name, path string, line int) graph.Node {
	return graph.Node{
		ID:            graph.NodeID(kind, name),
		Kind:          kind,
		Name:          name,
		QualifiedName: name,
		Location:      graph.Location{Path: path, Line: line, EndLine: line + 1},
	}
}

func nodeSet(nodes ...graph.Node) map[string]graph.Node {
	result := make(map[string]graph.Node, len(nodes))
	for _, value := range nodes {
		result[value.ID] = value
	}
	return result
}

func reachedNames(section query.ImpactSection) []string {
	result := make([]string, 0, len(section.Nodes))
	for _, reached := range section.Nodes {
		result = append(result, reached.Node.Name)
	}
	return result
}

func TestImpactSections(t *testing.T) {
	root := impactNode(graph.KindFunction, "Root", "pkg/root.go", 10)
	caller := impactNode(graph.KindFunction, "Caller", "pkg/caller.go", 5)
	secondCaller := impactNode(graph.KindFunction, "Second", "pkg/caller.go", 40)
	callee := impactNode(graph.KindFunction, "Callee", "pkg/callee.go", 7)
	configKey := impactNode(graph.KindConfigKey, "Setting", "config/app.yaml", 3)
	table := impactNode(graph.KindTable, "Ledger", "db/schema.sql", 12)
	event := impactNode(graph.KindEvent, "Shipped", "", 0)
	remote := impactNode(graph.KindFunction, "Remote", "svc/remote.go", 9)
	remote.Properties = map[string]string{"repository": "payments"}

	tests := []struct {
		name    string
		nodes   map[string]graph.Node
		edges   []graph.Edge
		options query.ImpactOptions
		check   func(*testing.T, query.ImpactReport)
	}{
		{
			name:  "upstream only",
			nodes: nodeSet(root, caller),
			edges: []graph.Edge{{ID: "e-call", FromID: caller.ID, ToID: root.ID, Kind: graph.EdgeCalls}},
			check: func(t *testing.T, report query.ImpactReport) {
				if got := reachedNames(report.Upstream); !reflect.DeepEqual(got, []string{"Root", "Caller"}) {
					t.Fatalf("upstream nodes = %v", got)
				}
				if got := reachedNames(report.Downstream); !reflect.DeepEqual(got, []string{"Root"}) {
					t.Fatalf("downstream nodes = %v", got)
				}
				if report.Upstream.Direction != query.Upstream || report.Downstream.Direction != query.Downstream {
					t.Fatalf("unexpected section directions: %#v", report)
				}
				if len(report.Upstream.Relations) == 0 || len(report.Downstream.Relations) == 0 {
					t.Fatal("sections must record their relation sets")
				}
				if report.Upstream.Depth != 4 || report.Downstream.Depth != 4 {
					t.Fatalf("default depth not applied: %d/%d", report.Upstream.Depth, report.Downstream.Depth)
				}
				if report.Truncated {
					t.Fatal("report must not be truncated")
				}
			},
		},
		{
			name:  "downstream only",
			nodes: nodeSet(root, callee),
			edges: []graph.Edge{{ID: "e-call", FromID: root.ID, ToID: callee.ID, Kind: graph.EdgeCalls}},
			check: func(t *testing.T, report query.ImpactReport) {
				if got := reachedNames(report.Upstream); !reflect.DeepEqual(got, []string{"Root"}) {
					t.Fatalf("upstream nodes = %v", got)
				}
				if got := reachedNames(report.Downstream); !reflect.DeepEqual(got, []string{"Root", "Callee"}) {
					t.Fatalf("downstream nodes = %v", got)
				}
			},
		},
		{
			name:  "both directions are not conflated",
			nodes: nodeSet(root, caller, callee),
			edges: []graph.Edge{
				{ID: "e-in", FromID: caller.ID, ToID: root.ID, Kind: graph.EdgeCalls},
				{ID: "e-out", FromID: root.ID, ToID: callee.ID, Kind: graph.EdgeCalls},
			},
			check: func(t *testing.T, report query.ImpactReport) {
				if got := reachedNames(report.Upstream); !reflect.DeepEqual(got, []string{"Root", "Caller"}) {
					t.Fatalf("upstream nodes = %v", got)
				}
				if got := reachedNames(report.Downstream); !reflect.DeepEqual(got, []string{"Root", "Callee"}) {
					t.Fatalf("downstream nodes = %v", got)
				}
				if len(report.Upstream.Edges) != 1 || report.Upstream.Edges[0].ID != "e-in" {
					t.Fatalf("upstream edges = %#v", report.Upstream.Edges)
				}
				if len(report.Downstream.Edges) != 1 || report.Downstream.Edges[0].ID != "e-out" {
					t.Fatalf("downstream edges = %#v", report.Downstream.Edges)
				}
			},
		},
		{
			name:  "independent truncation",
			nodes: nodeSet(root, caller, secondCaller, callee),
			edges: []graph.Edge{
				{ID: "e-in-1", FromID: caller.ID, ToID: root.ID, Kind: graph.EdgeCalls},
				{ID: "e-in-2", FromID: secondCaller.ID, ToID: root.ID, Kind: graph.EdgeCalls},
				{ID: "e-out", FromID: root.ID, ToID: callee.ID, Kind: graph.EdgeCalls},
			},
			options: query.ImpactOptions{UpstreamLimit: 1, DownstreamLimit: 100},
			check: func(t *testing.T, report query.ImpactReport) {
				if !report.Upstream.Truncated {
					t.Fatal("upstream section must be truncated")
				}
				if report.Downstream.Truncated {
					t.Fatal("downstream section must not be truncated")
				}
				if !report.Truncated {
					t.Fatal("report must be truncated when any section is")
				}
				if got := reachedNames(report.Downstream); !reflect.DeepEqual(got, []string{"Root", "Callee"}) {
					t.Fatalf("downstream nodes = %v", got)
				}
			},
		},
		{
			name:  "impacted files deduplicate but keep every cause",
			nodes: nodeSet(root, caller, secondCaller),
			edges: []graph.Edge{
				{ID: "e-in-1", FromID: caller.ID, ToID: root.ID, Kind: graph.EdgeCalls},
				{ID: "e-in-2", FromID: secondCaller.ID, ToID: root.ID, Kind: graph.EdgeCalls},
			},
			check: func(t *testing.T, report query.ImpactReport) {
				if len(report.ImpactedFiles) != 2 {
					t.Fatalf("impacted files = %#v", report.ImpactedFiles)
				}
				shared := report.ImpactedFiles[0]
				if shared.Path != "pkg/caller.go" {
					t.Fatalf("unexpected ordering: %#v", report.ImpactedFiles)
				}
				if got := len(shared.NodeIDs); got != 2 {
					t.Fatalf("expected both node causes, got %d: %#v", got, shared)
				}
				if !reflect.DeepEqual(shared.EdgeIDs, []string{"e-in-1", "e-in-2"}) {
					t.Fatalf("expected both edge causes: %#v", shared)
				}
				if !reflect.DeepEqual(shared.Directions, []string{"upstream"}) {
					t.Fatalf("directions = %#v", shared.Directions)
				}
				if shared.Federated {
					t.Fatal("local file must not be marked federated")
				}
				if report.ImpactedFiles[1].Path != "pkg/root.go" {
					t.Fatalf("root file missing: %#v", report.ImpactedFiles)
				}
			},
		},
		{
			name:  "federated edge yields a cross repository hop",
			nodes: nodeSet(root, remote),
			edges: []graph.Edge{{
				ID: "e-remote", FromID: root.ID, ToID: remote.ID, Kind: graph.EdgeRequests,
				Properties: map[string]string{"federated": "true"},
			}},
			check: func(t *testing.T, report query.ImpactReport) {
				if len(report.CrossRepository) != 1 {
					t.Fatalf("cross repository hops = %#v", report.CrossRepository)
				}
				hop := report.CrossRepository[0]
				if hop.Direction != query.Downstream || hop.Node.Name != "Remote" || hop.Edge.ID != "e-remote" {
					t.Fatalf("unexpected hop: %#v", hop)
				}
				var federated *query.ImpactedFile
				for i := range report.ImpactedFiles {
					if report.ImpactedFiles[i].Repository == "payments" {
						federated = &report.ImpactedFiles[i]
					}
				}
				if federated == nil || !federated.Federated || federated.Path != "svc/remote.go" {
					t.Fatalf("federated impacted file missing: %#v", report.ImpactedFiles)
				}
			},
		},
		{
			name:  "config data and event relations are partitioned",
			nodes: nodeSet(root, configKey, table, event, callee),
			edges: []graph.Edge{
				{ID: "e-config", FromID: root.ID, ToID: configKey.ID, Kind: graph.EdgeReadsConfig},
				{ID: "e-write", FromID: root.ID, ToID: table.ID, Kind: graph.EdgeWrites},
				{ID: "e-publish", FromID: root.ID, ToID: event.ID, Kind: graph.EdgePublishes},
				{ID: "e-call", FromID: root.ID, ToID: callee.ID, Kind: graph.EdgeCalls},
			},
			check: func(t *testing.T, report query.ImpactReport) {
				if len(report.Config) != 1 || report.Config[0].Node.Name != "Setting" || report.Config[0].Edge.ID != "e-config" {
					t.Fatalf("config relations = %#v", report.Config)
				}
				if len(report.Data) != 1 || report.Data[0].Node.Name != "Ledger" {
					t.Fatalf("data relations = %#v", report.Data)
				}
				if len(report.Events) != 1 || report.Events[0].Node.Name != "Shipped" {
					t.Fatalf("event relations = %#v", report.Events)
				}
				for _, relation := range report.Config {
					if relation.Direction != query.Downstream {
						t.Fatalf("unexpected relation direction: %#v", relation)
					}
				}
				if len(report.Downstream.Edges) != 4 {
					t.Fatalf("partitioning must not remove edges from sections: %#v", report.Downstream.Edges)
				}
				for _, file := range report.ImpactedFiles {
					if file.Path == "" {
						t.Fatalf("node without a location produced a file entry: %#v", file)
					}
				}
			},
		},
		{
			name:  "external targets stay visible and unresolved",
			nodes: nodeSet(root, externalNode("ThirdParty")),
			edges: []graph.Edge{{ID: "e-external", FromID: root.ID, ToID: graph.NodeID(graph.KindExternal, "ThirdParty"), Kind: graph.EdgeCalls}},
			check: func(t *testing.T, report query.ImpactReport) {
				found := false
				for _, reached := range report.Downstream.Nodes {
					if reached.Node.Name == "ThirdParty" {
						found = reached.Node.External
					}
				}
				if !found {
					t.Fatalf("external node must remain visible and external: %#v", report.Downstream.Nodes)
				}
				if len(report.ImpactedFiles) != 1 || report.ImpactedFiles[0].Path != "pkg/root.go" {
					t.Fatalf("external node must not contribute a file: %#v", report.ImpactedFiles)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := query.NewService(&fakeRepository{nodes: test.nodes, edges: test.edges})
			report, err := service.Impact(context.Background(), "Root", test.options)
			if err != nil {
				t.Fatal(err)
			}
			if report.Root.Name != "Root" {
				t.Fatalf("unexpected root: %#v", report.Root)
			}
			test.check(t, report)
		})
	}
}

func externalNode(name string) graph.Node {
	return graph.Node{
		ID:            graph.NodeID(graph.KindExternal, name),
		Kind:          graph.KindExternal,
		Name:          name,
		QualifiedName: name,
		External:      true,
	}
}

func TestImpactSelectorErrors(t *testing.T) {
	root := impactNode(graph.KindFunction, "Root", "pkg/root.go", 1)
	first := impactNode(graph.KindMethod, "Shared", "a/one.go", 1)
	second := impactNode(graph.KindMethod, "Shared", "b/two.go", 2)
	second.ID = graph.NodeID(graph.KindMethod, "Shared", "second")

	tests := []struct {
		name     string
		selector string
		nodes    map[string]graph.Node
		assert   func(*testing.T, error)
	}{
		{
			name:     "not found",
			selector: "Missing",
			nodes:    nodeSet(root),
			assert: func(t *testing.T, err error) {
				if !errors.Is(err, query.ErrNotFound) {
					t.Fatalf("expected ErrNotFound, got %v", err)
				}
			},
		},
		{
			name:     "ambiguous",
			selector: "Shared",
			nodes:    nodeSet(first, second),
			assert: func(t *testing.T, err error) {
				var ambiguous *query.AmbiguousError
				if !errors.As(err, &ambiguous) {
					t.Fatalf("expected *AmbiguousError, got %v", err)
				}
				if len(ambiguous.Candidates) != 2 {
					t.Fatalf("unexpected candidates: %#v", ambiguous.Candidates)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := query.NewService(&fakeRepository{nodes: test.nodes})
			report, err := service.Impact(context.Background(), test.selector, query.ImpactOptions{})
			if err == nil {
				t.Fatalf("expected an error, got report %#v", report)
			}
			if !reflect.DeepEqual(report, query.ImpactReport{}) {
				t.Fatalf("failed resolution must not return a partial report: %#v", report)
			}
			test.assert(t, err)
		})
	}
}

type fakeSourceReader struct {
	err          error
	failFor      map[string]bool
	calls        int
	contextLines int
	maxLines     int
}

func (f *fakeSourceReader) ReadNodeSource(_ context.Context, nodeID string, contextLines, maxLines int) (query.SourceExcerpt, error) {
	f.calls++
	f.contextLines, f.maxLines = contextLines, maxLines
	if f.err != nil {
		return query.SourceExcerpt{}, f.err
	}
	if f.failFor[nodeID] {
		return query.SourceExcerpt{}, errors.New("source unavailable")
	}
	return query.SourceExcerpt{NodeID: "ignored", Path: "pkg/file.go", StartLine: 1, EndLine: 3, Content: "body"}, nil
}

func TestImpactSource(t *testing.T) {
	root := impactNode(graph.KindFunction, "Root", "pkg/root.go", 10)
	caller := impactNode(graph.KindFunction, "Caller", "pkg/caller.go", 5)
	nodes := nodeSet(root, caller)
	edges := []graph.Edge{{ID: "e-in", FromID: caller.ID, ToID: root.ID, Kind: graph.EdgeCalls}}

	tests := []struct {
		name    string
		reader  *fakeSourceReader
		attach  bool
		options query.ImpactOptions
		check   func(*testing.T, query.ImpactReport, *fakeSourceReader)
	}{
		{
			name:    "no reader configured",
			attach:  false,
			options: query.ImpactOptions{IncludeSource: true},
			check: func(t *testing.T, report query.ImpactReport, _ *fakeSourceReader) {
				if len(report.Sources) != 0 {
					t.Fatalf("sources without a reader: %#v", report.Sources)
				}
				if len(report.Upstream.Nodes) != 2 {
					t.Fatal("structural output must be unaffected")
				}
			},
		},
		{
			name:    "source disabled",
			reader:  &fakeSourceReader{},
			attach:  true,
			options: query.ImpactOptions{},
			check: func(t *testing.T, report query.ImpactReport, reader *fakeSourceReader) {
				if len(report.Sources) != 0 || reader.calls != 0 {
					t.Fatalf("reader must not be used: %d calls, %#v", reader.calls, report.Sources)
				}
			},
		},
		{
			name:    "source included with defaults",
			reader:  &fakeSourceReader{},
			attach:  true,
			options: query.ImpactOptions{IncludeSource: true},
			check: func(t *testing.T, report query.ImpactReport, reader *fakeSourceReader) {
				if len(report.Sources) != 2 {
					t.Fatalf("sources = %#v", report.Sources)
				}
				if report.Sources[0].NodeID != root.ID {
					t.Fatalf("root excerpt must come first: %#v", report.Sources)
				}
				if reader.contextLines != 2 || reader.maxLines != 200 {
					t.Fatalf("source defaults not applied: %d/%d", reader.contextLines, reader.maxLines)
				}
			},
		},
		{
			name:    "source limit bounds excerpts",
			reader:  &fakeSourceReader{},
			attach:  true,
			options: query.ImpactOptions{IncludeSource: true, SourceLimit: 1},
			check: func(t *testing.T, report query.ImpactReport, _ *fakeSourceReader) {
				if len(report.Sources) != 1 {
					t.Fatalf("sources = %#v", report.Sources)
				}
			},
		},
		{
			name:    "per item failure keeps structural output",
			reader:  &fakeSourceReader{failFor: map[string]bool{root.ID: true}},
			attach:  true,
			options: query.ImpactOptions{IncludeSource: true},
			check: func(t *testing.T, report query.ImpactReport, _ *fakeSourceReader) {
				if len(report.Sources) != 1 || report.Sources[0].NodeID != caller.ID {
					t.Fatalf("sources = %#v", report.Sources)
				}
				if len(report.Upstream.Nodes) != 2 || len(report.ImpactedFiles) != 2 {
					t.Fatalf("structural output changed: %#v", report)
				}
			},
		},
		{
			name:    "every read failing leaves no sources",
			reader:  &fakeSourceReader{err: errors.New("worktree missing")},
			attach:  true,
			options: query.ImpactOptions{IncludeSource: true},
			check: func(t *testing.T, report query.ImpactReport, _ *fakeSourceReader) {
				if len(report.Sources) != 0 {
					t.Fatalf("sources = %#v", report.Sources)
				}
				if len(report.ImpactedFiles) != 2 {
					t.Fatalf("structural output changed: %#v", report.ImpactedFiles)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := query.NewService(&fakeRepository{nodes: nodes, edges: edges})
			if test.attach {
				service = service.WithSourceReader(test.reader)
			}
			report, err := service.Impact(context.Background(), "Root", test.options)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, report, test.reader)
		})
	}
}

func TestWithSourceReaderDoesNotMutateReceiver(t *testing.T) {
	root := impactNode(graph.KindFunction, "Root", "pkg/root.go", 1)
	service := query.NewService(&fakeRepository{nodes: nodeSet(root)})
	reader := &fakeSourceReader{}
	if service.WithSourceReader(reader) == service {
		t.Fatal("WithSourceReader must return a copy")
	}
	report, err := service.Impact(context.Background(), "Root", query.ImpactOptions{IncludeSource: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sources) != 0 || reader.calls != 0 {
		t.Fatalf("original service gained a reader: %d calls", reader.calls)
	}
}

func TestImpactIsDeterministic(t *testing.T) {
	root := impactNode(graph.KindFunction, "Root", "pkg/root.go", 10)
	caller := impactNode(graph.KindFunction, "Caller", "pkg/caller.go", 5)
	secondCaller := impactNode(graph.KindFunction, "Second", "pkg/caller.go", 40)
	callee := impactNode(graph.KindFunction, "Callee", "pkg/callee.go", 7)
	configKey := impactNode(graph.KindConfigKey, "Setting", "config/app.yaml", 3)
	remote := impactNode(graph.KindFunction, "Remote", "svc/remote.go", 9)
	remote.Properties = map[string]string{"repository": "payments"}

	repository := &fakeRepository{
		nodes: nodeSet(root, caller, secondCaller, callee, configKey, remote),
		edges: []graph.Edge{
			{ID: "e-2", FromID: secondCaller.ID, ToID: root.ID, Kind: graph.EdgeCalls},
			{ID: "e-1", FromID: caller.ID, ToID: root.ID, Kind: graph.EdgeCalls},
			{ID: "e-4", FromID: root.ID, ToID: configKey.ID, Kind: graph.EdgeReadsConfig},
			{ID: "e-3", FromID: root.ID, ToID: callee.ID, Kind: graph.EdgeCalls},
			{ID: "e-5", FromID: root.ID, ToID: remote.ID, Kind: graph.EdgeRequests, Properties: map[string]string{"federated": "true"}},
		},
	}
	service := query.NewService(repository).WithSourceReader(&fakeSourceReader{})
	options := query.ImpactOptions{IncludeSource: true}

	first, err := service.Impact(context.Background(), "Root", options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Impact(context.Background(), "Root", options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same graph produced different reports:\n%#v\n%#v", first, second)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("JSON is not byte stable:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestImpactRelationSetsAreShared(t *testing.T) {
	upstream := query.UpstreamRelations()
	downstream := query.DownstreamRelations()
	if len(upstream) == 0 || !reflect.DeepEqual(upstream, downstream) {
		t.Fatalf("relation sets must be mirrored: %v vs %v", upstream, downstream)
	}
	required := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeHandledBy, graph.EdgeUsesMiddleware, graph.EdgeImports, graph.EdgeExtends,
		graph.EdgeImplements, graph.EdgeEmbeds, graph.EdgeReferences, graph.EdgeReads, graph.EdgeWrites,
		graph.EdgeEncodes, graph.EdgeDecodes,
		graph.EdgeAssigns, graph.EdgeReturns, graph.EdgePasses, graph.EdgeRequests, graph.EdgeDependsOn}
	present := map[graph.EdgeKind]bool{}
	for _, relation := range upstream {
		present[relation] = true
	}
	for _, relation := range required {
		if !present[relation] {
			t.Fatalf("upstream relations must keep the existing blast-radius relation %q", relation)
		}
	}
	upstream[0] = graph.EdgeContains
	if query.UpstreamRelations()[0] == graph.EdgeContains {
		t.Fatal("UpstreamRelations must return a defensive copy")
	}
}

// TestImpactResolvesDeclarationNotItsOwnMembers guards the reported case behind
// this test file's package: "grafo impact query.Service.Impact" was made ambiguous
// by that method's own parameters and local variables, and a silently resolved
// root would have produced a confident report for the wrong symbol.
func TestImpactResolvesDeclarationNotItsOwnMembers(t *testing.T) {
	method := graph.Node{ID: graph.NodeID(graph.KindMethod, "example.com/query.Service.Impact"),
		Kind: graph.KindMethod, Name: "Impact", QualifiedName: "example.com/query.Service.Impact",
		Location: graph.Location{Path: "internal/query/impact.go", Line: 184, EndLine: 220}}
	members := []graph.Node{method}
	for _, member := range []struct {
		kind graph.NodeKind
		name string
	}{
		{graph.KindParameter, "ctx"}, {graph.KindParameter, "selector"},
		{graph.KindParameter, "options"}, {graph.KindVariable, "err@185"},
		{graph.KindVariable, "root@185"}, {graph.KindVariable, "report@200"},
	} {
		qualified := method.QualifiedName + "." + member.name
		members = append(members, graph.Node{ID: graph.NodeID(member.kind, qualified), Kind: member.kind,
			Name: member.name, QualifiedName: qualified,
			Location: graph.Location{Path: "internal/query/impact.go", Line: 185, EndLine: 185}})
	}
	// A case-insensitive-only rival, as the real graph has in impactSection.
	members = append(members, graph.Node{ID: graph.NodeID(graph.KindMethod, "example.com/query.Service.impactSection"),
		Kind: graph.KindMethod, Name: "impactSection", QualifiedName: "example.com/query.Service.impactSection",
		Location: graph.Location{Path: "internal/query/impact.go", Line: 246, EndLine: 260}})

	service := query.NewService(&fakeRepository{nodes: nodeSet(members...)})
	report, err := service.Impact(context.Background(), "Service.Impact", query.ImpactOptions{})
	if err != nil {
		t.Fatalf("expected the method to resolve, got %v", err)
	}
	if report.Root.ID != method.ID {
		t.Fatalf("unexpected impact root: %#v", report.Root)
	}
}

// TestImpactReportsCompleteAmbiguity proves an ambiguous root never yields a
// report and that the error states the true match count.
func TestImpactReportsCompleteAmbiguity(t *testing.T) {
	var nodes []graph.Node
	for index := 0; index < 8; index++ {
		qualified := "example.com/pkg" + string(rune('a'+index)) + ".Service.Charge"
		nodes = append(nodes, graph.Node{ID: graph.NodeID(graph.KindMethod, qualified), Kind: graph.KindMethod,
			Name: "Charge", QualifiedName: qualified, Location: graph.Location{Path: "charge.go", Line: 1, EndLine: 2}})
	}
	service := query.NewService(&fakeRepository{nodes: nodeSet(nodes...), matchLimit: 3})
	report, err := service.Impact(context.Background(), "Charge", query.ImpactOptions{})
	var ambiguous *query.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected *AmbiguousError, got %v", err)
	}
	if ambiguous.Total != 8 || len(ambiguous.Candidates) != 3 {
		t.Fatalf("expected 8 matches with 3 listed, got %d and %d", ambiguous.Total, len(ambiguous.Candidates))
	}
	if !reflect.DeepEqual(report, query.ImpactReport{}) {
		t.Fatalf("failed resolution must not return a partial report: %#v", report)
	}
}

// TestImpactKindNarrowsResolution proves the kind filter reaches Impact.
func TestImpactKindNarrowsResolution(t *testing.T) {
	method := graph.Node{ID: graph.NodeID(graph.KindMethod, "example.com/pkg.Service.Charge"),
		Kind: graph.KindMethod, Name: "Charge", QualifiedName: "example.com/pkg.Service.Charge",
		Location: graph.Location{Path: "charge.go", Line: 1, EndLine: 4}}
	rival := graph.Node{ID: graph.NodeID(graph.KindField, "example.com/pkg.Request.Charge"),
		Kind: graph.KindField, Name: "Charge", QualifiedName: "example.com/pkg.Request.Charge",
		Location: graph.Location{Path: "request.go", Line: 9, EndLine: 9}}
	service := query.NewService(&fakeRepository{nodes: nodeSet(method, rival)})
	if _, err := service.Impact(context.Background(), "Charge", query.ImpactOptions{}); err == nil {
		t.Fatal("expected an unfiltered selector to stay ambiguous")
	}
	report, err := service.Impact(context.Background(), "Charge", query.ImpactOptions{Kind: graph.KindMethod})
	if err != nil {
		t.Fatalf("kind-filtered impact failed: %v", err)
	}
	if report.Root.ID != method.ID {
		t.Fatalf("unexpected impact root: %#v", report.Root)
	}
}
