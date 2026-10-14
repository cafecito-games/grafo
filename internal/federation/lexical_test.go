package federation

import (
	"context"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"path/filepath"
	"strings"
	"testing"
)

func TestLexicalFederationSharesWorkCeilings(t *testing.T) {
	ctx := context.Background()
	fed := &Repository{}
	for _, name := range []string{"one", "two"} {
		r, e := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "index.sqlite"))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = r.Close() })
		if e = r.ReplaceOwner(ctx, "a.go", graph.ParseResult{Nodes: []graph.Node{{ID: name, Kind: graph.KindFunction, Name: "RestorePlayer", QualifiedName: "RestorePlayer", OwnerFile: "a.go", Location: graph.Location{Path: "a.go"}}}}); e != nil {
			t.Fatal(e)
		}
		fed.members = append(fed.members, member{project: indexer.Project{Name: name}, repository: r})
	}
	q := graph.DeclarationQuery{Terms: []string{"restore"}, Limit: 5, MaxPostings: 1, MaxCandidates: 1}
	page, e := fed.SearchDeclarations(ctx, q)
	if e != nil || !page.Truncated || page.PostingsRead != 1 || page.CandidatesRead != 1 || len(page.Matches) != 1 {
		t.Fatalf("global budget: %+v %v", page, e)
	}
	q.Repository = "two"
	page, e = fed.SearchDeclarations(ctx, q)
	if e != nil || len(page.Matches) != 1 || page.Matches[0].Repository != "two" {
		t.Fatalf("repository scope: %+v %v", page, e)
	}
	q.Repository = ""
	q.MaxPostings = 10
	q.MaxCandidates = 10
	page, e = fed.SearchDeclarations(ctx, q)
	if e != nil || len(page.Matches) != 2 {
		t.Fatalf("complete: %+v %v", page, e)
	}
}

func TestLexicalFederationBoundsEmptyMemberMetadata(t *testing.T) {
	ctx := context.Background()
	r, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "empty.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	fed := &Repository{}
	for i := 0; i < 1000; i++ {
		fed.members = append(fed.members, member{project: indexer.Project{Name: "empty"}, repository: r})
	}
	q := graph.DeclarationQuery{Terms: []string{"absent", "unknown"}, Limit: 5, MaxPostings: 100, MaxCandidates: 50, MaxMetadata: 13}
	page, err := fed.SearchDeclarations(ctx, q)
	if err != nil || !page.Truncated || page.MetadataReads != 13 || page.PostingsRead != 0 || page.CandidatesRead != 0 {
		t.Fatalf("empty members bounded: %+v %v", page, err)
	}
}

func TestLexicalFederationRejectsUnknownRepositoryScope(t *testing.T) {
	ctx := context.Background()
	r, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.ReplaceOwner(ctx, "a.go", graph.ParseResult{Nodes: []graph.Node{{ID: "one", Kind: graph.KindFunction, Name: "RestorePlayer", QualifiedName: "RestorePlayer", OwnerFile: "a.go", Location: graph.Location{Path: "a.go"}}}}); err != nil {
		t.Fatal(err)
	}
	fed := &Repository{members: []member{{project: indexer.Project{Name: "one"}, repository: r}}}
	q := graph.DeclarationQuery{Terms: []string{"restore"}, Limit: 5, MaxPostings: 10, MaxCandidates: 10, Repository: "onee"}
	page, err := fed.SearchDeclarations(ctx, q)
	if err == nil {
		t.Fatalf("misspelled repository scope read as absent evidence: %+v", page)
	}
	if !strings.Contains(err.Error(), "onee") {
		t.Fatalf("error must name the unmatched scope: %v", err)
	}
	q.Repository = "one"
	page, err = fed.SearchDeclarations(ctx, q)
	if err != nil || len(page.Matches) != 1 {
		t.Fatalf("known repository scope: %+v %v", page, err)
	}
}
