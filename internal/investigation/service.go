package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/lexical"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/source"
)

type Service struct {
	repository graph.ReadRepository
	sources    *source.Service
}

func New(repository graph.ReadRepository, sources *source.Service) *Service {
	return &Service{repository, sources}
}

func normalize(r Request) (Request, error) {
	r.Query = strings.TrimSpace(r.Query)
	if r.Query == "" || len(r.Query) > 2000 {
		return r, errors.New("query must contain 1 to 2000 bytes")
	}
	for _, v := range append(append([]string{}, r.PathPrefixes...), r.Languages...) {
		if len(v) > 1024 {
			return r, errors.New("scope term exceeds 1024 bytes")
		}
	}
	var err error
	r.PathPrefixes, err = query.NormalizePathPrefixes(r.PathPrefixes)
	if err != nil {
		return r, err
	}
	for _, b := range []struct {
		v             *int
		def, max, min int
		name          string
	}{{&r.MaxCandidates, 5, 12, 1, "max_candidates"}, {&r.MaxSourceLines, 240, 600, 1, "max_source_lines"}, {&r.MaxSourceBytes, 524288, 2097152, 1, "max_source_bytes"}, {&r.MaxOutputBytes, 24000, 24000, 2000, "max_output_bytes"}, {&r.MaxWork, 12000, 50000, 32, "max_work"}, {&r.TimeoutMS, 4000, 8000, 1, "timeout_ms"}} {
		if *b.v == 0 {
			*b.v = b.def
		}
		if *b.v < b.min || *b.v > b.max {
			return r, fmt.Errorf("%s must be between %d and %d", b.name, b.min, b.max)
		}
	}
	if len(r.PathPrefixes) > 20 || len(r.Languages) > 20 || len(r.Repository) > 256 || len(r.Component) > 256 {
		return r, errors.New("scope exceeds supported bounds")
	}
	return r, nil
}

// QueryTerms removes grammatical words only; it does not expand domain synonyms.
func QueryTerms(q string) []string {
	stop := " a an and are as at be by can do does for from how i in is it of on or that the their this to was what when where which who why will with "
	var terms []string
	for _, t := range lexical.Terms(q) {
		if !strings.Contains(stop, " "+t+" ") {
			terms = append(terms, t)
		}
	}
	return terms
}
func (s *Service) Investigate(ctx context.Context, r Request) (Report, error) {
	r, err := normalize(r)
	if err != nil {
		return Report{}, err
	}
	if err = ctx.Err(); err != nil {
		return Report{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutMS)*time.Millisecond)
	defer cancel()
	repo, ok := s.repository.(graph.DeclarationRepository)
	if !ok {
		return Report{}, errors.New("declaration retrieval unavailable; reindex with a writable current Grafo version")
	}
	out := Report{Query: r.Query, Status: "candidates", Candidates: []Candidate{}, Nodes: []Node{}, Edges: []Edge{}, Excerpts: []Excerpt{}, Followups: []Followup{}, Boundaries: []string{"Lexical relevance is a discovery hint, not proof of behavior or a relationship. Source excerpts require interpretation; structural tests do not establish runtime coverage.", "This bounded investigation is not exhaustive. Missing matches or edges do not prove absence. Source files reflect the current checkout and may differ from the index. Excerpts can include complete small-file context; their line ranges, not the referenced symbol alone, describe the returned evidence."}}
	terms := QueryTerms(r.Query)
	if len(terms) > 32 {
		return Report{}, errors.New("query has more than 32 retrieval terms; narrow the question")
	}
	retrieval := r.MaxWork * 3 / 4
	metadata := min(graph.DefaultDeclarationMetadata, retrieval/4)
	retrieval -= metadata
	maxCandidates := min(512, retrieval/4)
	page, err := repo.SearchDeclarations(ctx, graph.DeclarationQuery{Terms: terms, Repository: r.Repository, Component: r.Component, PathPrefixes: r.PathPrefixes, Languages: r.Languages, Limit: min(36, r.MaxCandidates*3), MaxPostings: retrieval - maxCandidates, MaxCandidates: maxCandidates, MaxMetadata: metadata})
	if err != nil {
		return Report{}, err
	}
	out.Usage.Postings = page.PostingsRead
	out.Usage.Declarations = page.CandidatesRead
	out.Usage.MetadataReads = page.MetadataReads
	out.Usage.Work = page.PostingsRead + page.CandidatesRead + page.MetadataReads
	out.Truncated = page.Truncated
	if page.Truncated {
		out.Boundaries = append(out.Boundaries, "Declaration retrieval exhausted a work or result bound; ranking covers only inspected postings.")
	}
	// Generic implementation preference cannot turn an interface or generated schema into executable proof.
	sort.SliceStable(page.Matches, func(i, j int) bool {
		a, b := page.Matches[i], page.Matches[j]
		pa, pb := preference(a), preference(b)
		if a.Score == b.Score && pa != pb {
			return pa > pb
		}
		return a.Score > b.Score
	})
	nodes := map[string]bool{}
	edges := map[string]bool{}
	addNode := func(n graph.Node, repository string) {
		key := repository + "\x00" + n.ID
		if nodes[key] {
			return
		}
		nodes[key] = true
		out.Nodes = append(out.Nodes, Node{n.ID, repository, n.QualifiedName, n.Kind, n.Location.Path, n.Location.Line, n.Location.EndLine})
	}
	matches := page.Matches
	if len(matches) > r.MaxCandidates {
		matches = matches[:r.MaxCandidates]
		out.Truncated = true
	}
	for _, m := range matches {
		designation := "declaration"
		if m.Generated {
			designation = "generated declaration"
		} else if m.InterfaceMember || m.Node.Kind == graph.KindInterface {
			designation = "interface contract"
		} else if graph.IsTestSupportNode(m.Node) {
			designation = "test support"
		} else if m.Node.Kind == graph.KindFunction || m.Node.Kind == graph.KindMethod {
			designation = "implementation candidate"
		}
		out.Candidates = append(out.Candidates, Candidate{m.Node.ID, m.Repository, m.Score, m.MatchedTerms, designation})
		addNode(m.Node, m.Repository)
	}
	if len(matches) == 0 {
		out.Status = "no_match"
		out.Followups = append(out.Followups, Followup{Tool: "search_source", Selector: "", Reason: "Try another term or a narrower subsystem; lexical retrieval cannot bridge every paraphrase."})
	} else if len(matches) > 1 && (matches[0].Score == matches[1].Score || len(terms) == 1) {
		out.Status = "ambiguous"
		out.Followups = append(out.Followups, Followup{Tool: "investigate", Selector: "", Reason: "Disambiguate by repository, component, path, language, or a more specific symbol."})
	}
	var session *source.Session
	if s.sources != nil {
		session = s.sources.NewSession(source.SourceBudget{MaxBytes: r.MaxSourceBytes, MaxLines: r.MaxSourceLines, SmallFileContext: 160})
	}
	// Primary source precedes optional graph evidence so graph fan-out cannot crowd it out.
	for _, m := range matches {
		if ctx.Err() != nil {
			out.Truncated = true
			break
		}
		if session == nil {
			break
		}
		if out.Usage.Work+1 > r.MaxWork {
			out.Truncated = true
			break
		}
		out.Usage.Work++
		if covered(out.Excerpts, m.Node, m.Repository) {
			continue
		}
		allowance := min(160, r.MaxSourceLines-session.Usage().LinesReturned)
		if allowance < 1 {
			out.Truncated = true
			break
		}
		sourceNode := s.enclosingSource(ctx, m, r, &out, addNode, edges)

		ex, e := session.ReadNode(ctx, sourceNode, m.Repository, 0, allowance)
		if e != nil {
			out.Truncated = true
			out.Followups = append(out.Followups, Followup{Tool: "get_source", Selector: m.Node.ID, Repository: m.Repository, Reason: "Source unavailable within this request: " + bounded(e.Error(), 180)})
			continue
		}
		out.Excerpts = append(out.Excerpts, Excerpt{sourceNode.ID, ex.Repository, ex.Path, ex.StartLine, ex.EndLine, ex.Content, ex.Truncated})
		if ex.Truncated {
			out.Truncated = true
			out.Followups = append(out.Followups, Followup{Tool: "get_source", Selector: m.Node.ID, Repository: m.Repository, Reason: "Continue reading this truncated declaration before making complete behavior claims."})
		}
	}
	for index, m := range matches {
		if index >= 2 {
			break
		}
		s.testEvidence(ctx, m, r, &out, addNode, edges)
		relations, hasRelations := s.localRelations(m.Repository)
		if hasRelations {
			// Each page requests one exact relation: the global ledger reserves the query and its lookahead.
			for _, spec := range []struct {
				direction graph.RelationDirection
				kind      graph.EdgeKind
			}{{graph.OutgoingRelations, graph.EdgeCalls}, {graph.OutgoingRelations, graph.EdgeDeclares}} {
				if ctx.Err() != nil {
					out.Truncated = true
					break
				}
				if spec.kind == graph.EdgeDeclares && (m.Node.Kind == graph.KindMethod || m.Node.Kind == graph.KindFunction) {
					continue
				}
				remaining := r.MaxWork - out.Usage.Work
				if remaining < 3 {
					out.Truncated = true
					break
				}
				limit := min(3, remaining-2)
				out.Usage.GraphQueries++
				out.Usage.Work += 2
				p, e := relations.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: m.Node.ID, Direction: spec.direction, Relations: []graph.EdgeKind{spec.kind}, Limit: limit})
				if e != nil {
					out.Truncated = true
					continue
				}
				out.Usage.Work += len(p.Items)
				out.Truncated = out.Truncated || p.Truncated
				for _, item := range p.Items {
					repository := item.Repository
					if repository == "" {
						repository = m.Repository
					}
					if !within(item.Counterpart, repository, r) || repository != m.Repository || r.Component != "" {
						out.Truncated = true
						continue
					}
					if edges[repository+"\x00"+item.Edge.ID] {
						continue
					}
					edges[repository+"\x00"+item.Edge.ID] = true
					addNode(item.Counterpart, repository)
					out.Edges = append(out.Edges, Edge{ID: item.Edge.ID, Repository: repository, From: item.Edge.FromID, To: item.Edge.ToID, Kind: item.Edge.Kind, Path: item.Edge.Location.Path, Line: item.Edge.Location.Line})
					if spec.kind == graph.EdgeCalls {
						out.Followups = append(out.Followups, Followup{Tool: "get_source", Selector: item.Counterpart.ID, Repository: repository, Reason: "Resolved callee; read if needed to establish behavior beyond the candidate."})
					}
				}
			}
		}
	}
	if session != nil {
		u := session.Usage()
		out.Usage.SourceBytes = u.BytesRead
		out.Usage.SourceLines = u.LinesReturned
	}
	if ctx.Err() != nil {
		out.Boundaries = append(out.Boundaries, "Deadline reached; evidence is incomplete.")
		out.Truncated = true
	}
	if r.Component != "" {
		out.Boundaries = append(out.Boundaries, "Related nodes are omitted because component membership was not independently established.")
	}
	out.Boundaries = append(out.Boundaries, "Graph evidence is local to each candidate repository. At most two candidates receive callees and one structural test path each, through at most four helper edges. Other candidates and deeper paths require follow-up; omitted paths are not negative coverage evidence.")
	if len(matches) > 0 && len(matches[0].MatchedTerms)*2 < len(terms) {
		out.Status = "weak_match"
		out.Followups = append(out.Followups, Followup{Tool: "search_source", Selector: "", Reason: "Most question terms are unmatched; try alternative code terminology or inspect textual evidence."})
	}
	return fit(out, r.MaxOutputBytes), nil
}
func preference(m graph.DeclarationMatch) int {
	if m.Generated || m.InterfaceMember || graph.IsTestSupportNode(m.Node) {
		return 0
	}
	if m.Node.Kind == graph.KindFunction || m.Node.Kind == graph.KindMethod {
		return 2
	}
	return 1
}
func within(n graph.Node, repo string, r Request) bool {
	if n.External {
		return false
	}
	if r.Repository != "" && repo != "" && repo != r.Repository {
		return false
	}
	if len(r.Languages) > 0 {
		ok := false
		for _, v := range r.Languages {
			ok = ok || n.Language == v
		}
		if !ok {
			return false
		}
	}
	if len(r.PathPrefixes) > 0 {
		ok := false
		for _, p := range r.PathPrefixes {
			p = strings.TrimSuffix(p, "/")
			ok = ok || n.Location.Path == p || strings.HasPrefix(n.Location.Path, p+"/")
		}
		if !ok {
			return false
		}
	}
	return true
}
func bounded(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// fit removes optional evidence first, then clips source by complete lines. Every
// omission remains explicit. Serialization is bounded before either transport.
func fit(r Report, limit int) Report {
	for {
		for i := 0; i < 3; i++ {
			data, _ := json.Marshal(r)
			r.Usage.OutputBytes = len(data)
		}
		data, _ := json.Marshal(r)
		if len(data) <= limit {
			return r
		}
		r.Truncated = true
		if len(r.Followups) > 1 {
			r.Followups = r.Followups[:len(r.Followups)-1]
			continue
		}
		if len(r.Edges) > 0 {
			r.Edges = r.Edges[:len(r.Edges)-1]
			continue
		}
		needed := map[string]bool{}
		for _, ex := range r.Excerpts {
			needed[ex.Repository+"\x00"+ex.NodeID] = true
		}
		for _, c := range r.Candidates {
			needed[c.Repository+"\x00"+c.NodeID] = true
		}
		for _, e := range r.Edges {
			needed[e.Repository+"\x00"+e.From] = true
			needed[e.Repository+"\x00"+e.To] = true
		}
		pruned := false
		for i := len(r.Nodes) - 1; i >= 0; i-- {
			n := r.Nodes[i]
			if !needed[n.Repository+"\x00"+n.ID] {
				r.Nodes = append(r.Nodes[:i], r.Nodes[i+1:]...)
				pruned = true
			}
		}
		if pruned {
			continue
		}
		if len(r.Excerpts) > 0 {
			i := len(r.Excerpts) - 1
			ex := &r.Excerpts[i]
			lines := strings.Split(ex.Content, "\n")
			if len(lines) > 8 {
				lines = lines[:len(lines)/2]
				ex.Content = strings.Join(lines, "\n")
				ex.EndLine = ex.StartLine + len(lines) - 1
				ex.Truncated = true
			} else {
				r.Excerpts = r.Excerpts[:i]
			}
			continue
		}
		if len(r.Nodes) > 0 {
			removed := r.Nodes[len(r.Nodes)-1]
			r.Nodes = r.Nodes[:len(r.Nodes)-1]
			kept := r.Candidates[:0]
			for _, c := range r.Candidates {
				if c.NodeID != removed.ID || c.Repository != removed.Repository {
					kept = append(kept, c)
				}
			}
			r.Candidates = kept
			continue
		}
		if len(r.Candidates) > 0 {
			r.Candidates = r.Candidates[:len(r.Candidates)-1]
			continue
		}
		r.Query = bounded(r.Query, 200)
		r.Boundaries = []string{"Output budget exhausted. Evidence is incomplete; no absence or behavior conclusion is established."}
		r.Followups = nil
	}
}

// Avoid federation fan-out: adjacency evidence is explicitly local to the
// candidate's indexed repository. Cross-repository traversal is a follow-up.
func (s *Service) localRelations(repository string) (graph.RelationEdgeRepository, bool) {
	if members, ok := s.repository.(interface{ Members() []federation.Member }); ok {
		for _, m := range members.Members() {
			if m.Project.Name == repository || m.Project.ID == repository {
				r, ok := m.Files.(graph.RelationEdgeRepository)
				return r, ok
			}
		}
		return nil, false
	}
	r, ok := s.repository.(graph.RelationEdgeRepository)
	return r, ok
}
