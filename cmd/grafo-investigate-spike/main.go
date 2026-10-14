// Experimental lexical investigation harness for #251. Requires an explicitly
// refreshed index; it intentionally does not register a production CLI/MCP tool.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/pathscope"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/source"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type candidate struct {
	Node    graph.Node `json:"node"`
	Score   int        `json:"relevance_score"`
	Matched []string   `json:"matched_terms"`
}
type evidence struct {
	Selector   string                       `json:"selector"`
	Relations  []graph.HydratedRelationEdge `json:"relations,omitempty"`
	Tests      *query.TestCoverageReport    `json:"tests,omitempty"`
	Source     *source.Excerpt              `json:"source,omitempty"`
	Boundaries []string                     `json:"boundaries,omitempty"`
	Followup   string                       `json:"followup"`
}
type report struct {
	Query        string      `json:"query"`
	Terms        []string    `json:"terms"`
	Candidates   []candidate `json:"candidates"`
	Evidence     []evidence  `json:"evidence"`
	Notes        []string    `json:"notes"`
	Truncated    bool        `json:"truncated"`
	Milliseconds int64       `json:"milliseconds"`
}

func words(s string) []string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
			b.WriteByte(' ')
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}
func terms(s string) []string {
	stop := " where is are the a an how does do to of for in on and with when what which handled handle happens can i we "
	var out []string
	seen := map[string]bool{}
	for _, w := range words(s) {
		if !strings.Contains(stop, " "+w+" ") && !seen[w] {
			out = append(out, w)
			seen[w] = true
		}
	}
	return out
}
func rank(n graph.Node, ts []string) candidate {
	c := candidate{Node: n}
	name := " " + strings.Join(words(n.Name), " ") + " "
	qualified := " " + strings.Join(words(n.QualifiedName), " ") + " "
	for _, t := range ts {
		score := 0
		switch {
		case strings.Contains(name, " "+t+" "):
			score = 12
		case strings.Contains(strings.ToLower(n.Name), t):
			score = 6
		case strings.Contains(qualified, " "+t+" "):
			score = 3
		case strings.Contains(strings.ToLower(n.QualifiedName), t):
			score = 1
		}
		if score > 0 {
			c.Matched = append(c.Matched, t)
			c.Score += score
		}
	}
	c.Score += len(c.Matched) * len(c.Matched) * 10
	return c
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	root := flag.String("repo", "", "already indexed worktree")
	scope := flag.String("path-prefix", "", "entry candidate scope")
	baseline := flag.Bool("baseline", false, "existing Find using the supplied literal term")
	full := flag.Bool("full-candidates", false, "diagnostic variant: materialize all semantic candidate declarations before ranking; NOT a bounded production retrieval strategy")
	limit := flag.Int("limit", 5, "candidate count, 1..10")
	maxBytes := flag.Int("max-output-bytes", 24000, "hard compact JSON cap, minimum 2048")
	flag.Parse()
	if *limit < 1 || *limit > 10 || *maxBytes < 2048 {
		return fmt.Errorf("invalid bounds")
	}
	var prefixes []string
	if *scope != "" {
		var err error
		prefixes, err = query.NormalizePathPrefixes([]string{*scope})
		if err != nil {
			return err
		}
	}
	question := strings.Join(flag.Args(), " ")
	ts := terms(question)
	if len(ts) == 0 || len(ts) > 12 {
		return fmt.Errorf("query needs 1..12 content terms")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	project, err := indexer.DiscoverProject(ctx, *root)
	if err != nil {
		return err
	}
	repo, err := sqlite.OpenReadOnly(ctx, project.IndexPath)
	if err != nil {
		return err
	}
	defer repo.Close()
	r := report{Query: question, Terms: ts, Candidates: []candidate{}, Evidence: []evidence{}, Notes: []string{"Relevance is lexical inference; only returned edges prove relationships.", "Index freshness must be established separately. Scope selects roots; evidence may cross scope.", "No match is not proof that behavior is absent. No embedding provider or source-content fallback is used."}}
	if *baseline {
		nodes, e := query.NewService(repo).Find(ctx, question, *limit)
		if e != nil {
			return e
		}
		for _, n := range nodes {
			r.Candidates = append(r.Candidates, candidate{Node: n})
		}
	} else {
		kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindClass, graph.KindInterface, graph.KindEndpoint}
		pool := map[string]graph.Node{}
		if *full {
			nodes, e := repo.CandidateNodes(ctx)
			if e != nil {
				return e
			}
			for _, n := range nodes {
				p := n.Location.Path
				if len(prefixes) > 0 && !pathscope.HasPrefix(p, prefixes[0]) {
					continue
				}
				if strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "_test.gd") || strings.Contains(p, "/tests/") || strings.Contains(p, "/test/") {
					continue
				}
				if rank(n, ts).Score > 0 {
					pool[n.ID] = n
				}
			}
			r.Notes = append(r.Notes, "Diagnostic full-candidate scan: candidate enumeration is not work-bounded; test-source declarations excluded.")
		} else {
			for _, t := range ts {
				for _, kind := range kinds {
					q := graph.NodeListQuery{Kinds: []graph.NodeKind{kind}, Name: t, Limit: 101}
					q.PathPrefixes = prefixes
					nodes, e := repo.ListNodesByKind(ctx, q)
					if e != nil {
						return e
					}
					if len(nodes) > 100 {
						nodes = nodes[:100]
						r.Truncated = true
					}
					for _, n := range nodes {
						pool[n.Node.ID] = n.Node
					}
				}
			}
		}
		for _, n := range pool {
			r.Candidates = append(r.Candidates, rank(n, ts))
		}
		sort.Slice(r.Candidates, func(i, j int) bool {
			a, b := r.Candidates[i], r.Candidates[j]
			if a.Score != b.Score {
				return a.Score > b.Score
			}
			if *full && len(a.Node.Name) != len(b.Node.Name) {
				return len(a.Node.Name) < len(b.Node.Name)
			}
			if a.Node.QualifiedName != b.Node.QualifiedName {
				return a.Node.QualifiedName < b.Node.QualifiedName
			}
			return a.Node.ID < b.Node.ID
		})
		if len(r.Candidates) > *limit {
			r.Candidates = r.Candidates[:*limit]
			r.Truncated = true
		}
	}
	reader := source.NewService(repo, source.NewSingleProjectLocator(repo, project))
	service := query.NewService(repo)
	// Fixed allocation: at most three roots, 12 edges/root, 30 source lines/root.
	for i, c := range r.Candidates {
		if i == 3 {
			r.Truncated = true
			break
		}
		e := evidence{Selector: c.Node.ID, Followup: "grafo impact " + c.Node.ID + " --depth 2"}
		for _, direction := range []graph.RelationDirection{graph.IncomingRelations, graph.OutgoingRelations} {
			page, err := repo.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: c.Node.ID, Direction: direction, Relations: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeTests}, Limit: 2})
			if err != nil {
				return err
			}
			e.Relations = append(e.Relations, page.Items...)
			if page.Truncated {
				e.Boundaries = append(e.Boundaries, "relationship limit reached")
				r.Truncated = true
			}
			for _, item := range page.Items {
				if item.Counterpart.External {
					e.Boundaries = append(e.Boundaries, "unresolved target: "+item.Counterpart.QualifiedName)
				}
			}
		}
		tests, err := service.FindTests(ctx, c.Node.ID, query.TestCoverageOptions{Depth: 2, Limit: 12})
		if err != nil {
			e.Boundaries = append(e.Boundaries, err.Error())
		} else {
			e.Tests = &tests
			if tests.Truncated {
				r.Truncated = true
				e.Boundaries = append(e.Boundaries, "test traversal incomplete")
			}
		}
		excerpt, err := reader.Read(ctx, c.Node.ID, 0, 30)
		if err != nil {
			e.Boundaries = append(e.Boundaries, err.Error())
		} else {
			e.Source = &excerpt
			if excerpt.Truncated {
				r.Truncated = true
			}
		}
		r.Evidence = append(r.Evidence, e)
	}
	r.Milliseconds = time.Since(start).Milliseconds()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data)+1 > *maxBytes {
		r.Truncated = true
		r.Notes = append(r.Notes, "Output budget exhausted; evidence/candidates omitted in reverse rank order.")
		for len(data)+1 > *maxBytes {
			if len(r.Evidence) > 0 {
				r.Evidence = r.Evidence[:len(r.Evidence)-1]
			} else if len(r.Candidates) > 0 {
				r.Candidates = r.Candidates[:len(r.Candidates)-1]
			} else {
				return fmt.Errorf("metadata exceeds output budget")
			}
			data, err = json.Marshal(r)
			if err != nil {
				return err
			}
		}
	}
	_, err = os.Stdout.Write(append(data, '\n'))
	return err
}
