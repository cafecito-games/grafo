package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
)

type fakeRepository struct {
	graph.ReadRepository
	page         graph.DeclarationPage
	query        graph.DeclarationQuery
	relations    int
	relationPage graph.RelationEdgePage
}

func (f *fakeRepository) SearchDeclarations(ctx context.Context, q graph.DeclarationQuery) (graph.DeclarationPage, error) {
	f.query = q
	return f.page, ctx.Err()
}
func (f *fakeRepository) RelationEdges(ctx context.Context, q graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	f.relations++
	if len(q.Relations) != 1 {
		panic("multiplying relation limit")
	}
	p := f.relationPage
	if len(p.Items) > q.Limit {
		p.Items = p.Items[:q.Limit]
		p.Truncated = true
	}
	return p, ctx.Err()
}
func declaration(id, name, path string) graph.DeclarationMatch {
	return graph.DeclarationMatch{Node: graph.Node{ID: id, Name: name, QualifiedName: name, Kind: graph.KindFunction, Language: "go", Location: graph.Location{Path: path, Line: 1, EndLine: 30}}, Repository: "repo", Score: 100, MatchedTerms: []string{strings.ToLower(name)}}
}
func TestBoundedRetrievalAndGlobalGraphWork(t *testing.T) {
	f := &fakeRepository{page: graph.DeclarationPage{Matches: []graph.DeclarationMatch{declaration("1", "Run", "src/run.go")}, PostingsRead: 16, CandidatesRead: 6}}
	for i := 0; i < 30; i++ {
		n := declaration("other", "Other", "src/other.go").Node
		f.relationPage.Items = append(f.relationPage.Items, graph.HydratedRelationEdge{Counterpart: n, Repository: "repo", Edge: graph.Edge{ID: "edge", FromID: "1", ToID: n.ID, Kind: graph.EdgeCalls}})
	}
	r, err := New(f, nil).Investigate(context.Background(), Request{Query: "Run", MaxWork: 32, Repository: "repo", PathPrefixes: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.query.MaxPostings+f.query.MaxCandidates > 24 || f.query.Limit > 36 {
		t.Fatalf("unbounded query: %+v", f.query)
	}
	if r.Usage.Work > 32 || !r.Truncated || f.relations > 3 {
		t.Fatalf("work not bounded: %+v", r)
	}
	if f.query.Repository != "repo" || len(f.query.PathPrefixes) != 1 {
		t.Fatal("scope not forwarded")
	}
}
func TestNoMatchIsNotAbsenceAndAmbiguityIsExplicit(t *testing.T) {
	f := &fakeRepository{}
	r, err := New(f, nil).Investigate(context.Background(), Request{Query: "unknown mechanism"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "no_match" || !strings.Contains(strings.Join(r.Boundaries, " "), "do not prove absence") {
		t.Fatalf("negative claim: %+v", r)
	}
	f.page.Matches = []graph.DeclarationMatch{declaration("one", "Init", "one.go"), declaration("two", "Init", "two.go")}
	r, err = New(f, nil).Investigate(context.Background(), Request{Query: "Init"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ambiguous" || len(r.Candidates) != 2 {
		t.Fatalf("ambiguity lost: %+v", r)
	}
}
func TestCrossScopeEdgesAreOmitted(t *testing.T) {
	root := declaration("root", "Run", "src/run.go")
	foreign := declaration("foreign", "Other", "elsewhere/other.go").Node
	f := &fakeRepository{page: graph.DeclarationPage{Matches: []graph.DeclarationMatch{root}}, relationPage: graph.RelationEdgePage{Items: []graph.HydratedRelationEdge{{Counterpart: foreign, Repository: "repo", Edge: graph.Edge{ID: "edge", FromID: "root", ToID: "foreign", Kind: graph.EdgeCalls}}}}}
	r, err := New(f, nil).Investigate(context.Background(), Request{Query: "Run", PathPrefixes: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Edges) != 0 || len(r.Nodes) != 1 || !r.Truncated {
		t.Fatalf("scope leaked: %+v", r)
	}
}
func TestCancellationAndLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(&fakeRepository{}, nil).Investigate(ctx, Request{Query: "Run"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	for _, req := range []Request{{Query: "x", MaxWork: 31}, {Query: "x", MaxOutputBytes: 24001}, {Query: "x", TimeoutMS: 8001}, {Query: "x", PathPrefixes: []string{"../outside"}}, {Query: "x", MaxSourceLines: 601}} {
		if _, err := New(&fakeRepository{}, nil).Investigate(context.Background(), req); err == nil {
			t.Fatalf("invalid request accepted %+v", req)
		}
	}
}
func TestOutputBudgetKeepsValidReferencesAndExactByteCount(t *testing.T) {
	r := Report{Query: strings.Repeat("\"", 2000), Status: "candidates", Boundaries: []string{"Not exhaustive"}}
	for i := 0; i < 12; i++ {
		id := strings.Repeat("n", 200)
		r.Nodes = append(r.Nodes, Node{ID: id, Name: strings.Repeat("huge", 500)})
		r.Candidates = append(r.Candidates, Candidate{NodeID: id})
		r.Excerpts = append(r.Excerpts, Excerpt{NodeID: id, Content: strings.Repeat("line with escaped \"quotes\"\n", 600), StartLine: 1, EndLine: 600})
	}
	got := fit(r, 2000)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 2000 || got.Usage.OutputBytes != len(data) || !got.Truncated {
		t.Fatalf("invalid output bounds: bytes=%d usage=%d", len(data), got.Usage.OutputBytes)
	}
	nodes := map[string]bool{}
	for _, n := range got.Nodes {
		nodes[n.ID] = true
	}
	for _, c := range got.Candidates {
		if !nodes[c.NodeID] {
			t.Fatal("orphan candidate")
		}
	}
}
func TestInterfaceDoesNotBecomeImplementationAndWeakMatch(t *testing.T) {
	m := declaration("contract", "Execute", "api.go")
	m.InterfaceMember = true
	f := &fakeRepository{page: graph.DeclarationPage{Matches: []graph.DeclarationMatch{m}}}
	r, err := New(f, nil).Investigate(context.Background(), Request{Query: "execute unusual unrelated behavior"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Candidates[0].Designation != "interface contract" || r.Status != "weak_match" {
		t.Fatalf("overstated evidence: %+v", r)
	}
}

// A helper-mediated path must retain the existing query engine's structural
// designation and actual edges instead of fabricating a direct tests edge.
type helperRelations struct {
	graph.ReadRepository
	page graph.DeclarationPage
}

func (h *helperRelations) SearchDeclarations(context.Context, graph.DeclarationQuery) (graph.DeclarationPage, error) {
	return h.page, nil
}
func (h *helperRelations) RelationEdges(ctx context.Context, q graph.RelationEdgeQuery) (graph.RelationEdgePage, error) {
	if q.Direction != graph.IncomingRelations || q.Relations[0] != graph.EdgeCalls {
		return graph.RelationEdgePage{}, nil
	}
	var n graph.Node
	var e graph.Edge
	switch q.SubjectID {
	case "root":
		n = graph.Node{ID: "helper", Kind: graph.KindFunction, QualifiedName: "helper", Location: graph.Location{Path: "src/helper_test.go"}, Properties: map[string]string{"test_role": "helper"}}
		e = graph.Edge{ID: "helper-call", Kind: graph.EdgeCalls, FromID: "helper", ToID: "root"}
	case "helper":
		n = graph.Node{ID: "test", Kind: graph.KindTest, QualifiedName: "TestRun", Location: graph.Location{Path: "src/helper_test.go"}}
		e = graph.Edge{ID: "test-call", Kind: graph.EdgeCalls, FromID: "test", ToID: "helper"}
	default:
		return graph.RelationEdgePage{}, nil
	}
	return graph.RelationEdgePage{Items: []graph.HydratedRelationEdge{{Counterpart: n, Edge: e, Repository: "repo"}}}, ctx.Err()
}
func TestHelperTestEvidenceUsesActualPath(t *testing.T) {
	f := &helperRelations{page: graph.DeclarationPage{Matches: []graph.DeclarationMatch{declaration("root", "Run", "src/run.go")}}}
	r, err := New(f, nil).Investigate(context.Background(), Request{Query: "Run", PathPrefixes: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Edges) != 2 {
		t.Fatalf("missing helper path: %+v", r.Edges)
	}
	for _, e := range r.Edges {
		if e.Kind != graph.EdgeCalls || e.Repository != "repo" {
			t.Fatalf("invented relationship: %+v", e)
		}
	}
	found := false
	for _, n := range r.Nodes {
		found = found || n.ID == "test"
	}
	if !found {
		t.Fatal("test not retained")
	}
	if r.Usage.Work > 12000 {
		t.Fatal("global work exceeded")
	}
}

func TestEnclosingSourceRequiresRecordedContainmentAndScope(t *testing.T) {
	child := declaration("child", "Tick", "src/widget.gd")
	child.Node.Location.Line = 20
	child.Node.Location.EndLine = 25
	parent := graph.Node{ID: "parent", Kind: graph.KindClass, Location: graph.Location{Path: "src/widget.gd", Line: 1, EndLine: 80}}
	f := &fakeRepository{relationPage: graph.RelationEdgePage{Items: []graph.HydratedRelationEdge{{Counterpart: parent, Repository: "repo", Edge: graph.Edge{ID: "declares", FromID: "parent", ToID: "child", Kind: graph.EdgeDeclares}}}}}
	svc := New(f, nil)
	for _, tc := range []struct {
		name      string
		request   Request
		parentEnd int
		want      string
	}{{"contained", Request{MaxWork: 32}, 80, "parent"}, {"oversized", Request{MaxWork: 32}, 200, "child"}, {"scope", Request{MaxWork: 32, PathPrefixes: []string{"different"}}, 80, "child"}, {"component unverified", Request{MaxWork: 32, Component: "component"}, 80, "child"}, {"budget", Request{MaxWork: 2}, 80, "child"}} {
		t.Run(tc.name, func(t *testing.T) {
			f.relationPage.Items[0].Counterpart.Location.EndLine = tc.parentEnd
			report := Report{}
			got := svc.enclosingSource(context.Background(), child, tc.request, &report, func(graph.Node, string) {}, map[string]bool{})
			if got.ID != tc.want {
				t.Fatalf("got %s want %s", got.ID, tc.want)
			}
			if report.Usage.Work > tc.request.MaxWork {
				t.Fatal("work exceeded")
			}
		})
	}
}
func TestCoveredExcerptRequiresRepositoryAndFullSpan(t *testing.T) {
	n := graph.Node{Location: graph.Location{Path: "a.go", Line: 4, EndLine: 8}}
	ex := []Excerpt{{Repository: "repo", Path: "a.go", StartLine: 1, EndLine: 8}}
	if !covered(ex, n, "repo") || covered(ex, n, "other") {
		t.Fatal("repository scope lost")
	}
	ex[0].EndLine = 7
	if covered(ex, n, "repo") {
		t.Fatal("partial declaration considered covered")
	}
}

func TestSmallFileContextIsDeduplicatedWithinSourceBudget(t *testing.T) {
	root := testtemp.Dir(t)
	content := "package example\nfunc One() {}\nfunc Two() {}\n"
	if err := os.WriteFile(filepath.Join(root, "code.go"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	a, b := declaration("a", "One", "code.go"), declaration("b", "Two", "code.go")
	a.Node.Location.Line = 2
	a.Node.Location.EndLine = 2
	b.Node.Location.Line = 3
	b.Node.Location.EndLine = 3
	f := &fakeRepository{page: graph.DeclarationPage{Matches: []graph.DeclarationMatch{a, b}}}
	svc := New(f, source.NewScopedService(f, []indexer.Project{{Name: "repo", Root: root}}))
	r, err := svc.Investigate(context.Background(), Request{Query: "One Two", MaxSourceLines: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Excerpts) != 1 || r.Excerpts[0].StartLine != 1 || r.Excerpts[0].EndLine != 3 || r.Excerpts[0].NodeID != "a" {
		t.Fatalf("small file context not reused: %+v", r.Excerpts)
	}
	if r.Usage.SourceLines != 3 || r.Usage.SourceBytes != len(content) {
		t.Fatalf("incorrect source accounting: %+v", r.Usage)
	}
}
