package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/identity"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/migrations"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"github.com/pressly/goose/v3"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite/sqlcgen"
)

func lexicalFixture(t *testing.T) (*Repository, string) {
	t.Helper()
	path := filepath.Join(testtemp.Dir(t), "index.sqlite")
	r, e := Open(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, path
}
func declaration(id, name, path string) graph.Node {
	return graph.Node{ID: id, Kind: graph.KindFunction, Name: name, QualifiedName: "pkg." + name, OwnerFile: path, Language: "go", Location: graph.Location{Path: path, Line: 1}}
}
func lexicalQuery(terms ...string) graph.DeclarationQuery {
	return graph.DeclarationQuery{Terms: terms, Limit: 5, MaxPostings: 100, MaxCandidates: 50}
}
func TestLexicalReplacementsRollbackAndRestart(t *testing.T) {
	ctx := context.Background()
	r, path := lexicalFixture(t)
	first, last := declaration("same", "OldName", "a.go"), declaration("same", "NewName", "a.go")
	if e := r.ReplaceOwner(ctx, "a.go", graph.ParseResult{Nodes: []graph.Node{first, last}}); e != nil {
		t.Fatal(e)
	}
	search := func(term string, want int) {
		t.Helper()
		page, e := r.SearchDeclarations(ctx, lexicalQuery(term))
		if e != nil || len(page.Matches) != want {
			t.Fatalf("%s: %+v %v", term, page, e)
		}
	}
	search("old", 0)
	search("new", 1)
	err := r.inTransaction(ctx, func(q *sqlcgen.Queries, w *batchWriter) error {
		if e := q.DeleteNodesByOwner(ctx, "a.go"); e != nil {
			return e
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("missing rollback")
	}
	search("new", 1)
	if e := r.Close(); e != nil {
		t.Fatal(e)
	}
	r, e := Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	search("new", 1)
	if e = r.RemoveFiles(ctx, []string{"a.go"}); e != nil {
		t.Fatal(e)
	}
	search("new", 0)
	var count int
	if e = r.db.QueryRow("SELECT count(*) FROM lexical_terms").Scan(&count); e != nil || count != 0 {
		t.Fatalf("stale term populations: %d %v", count, e)
	}
}
func TestLexicalBackfillAndIncompleteRead(t *testing.T) {
	ctx := context.Background()
	r, path := lexicalFixture(t)
	if e := r.ReplaceOwner(ctx, "a.go", graph.ParseResult{Nodes: []graph.Node{declaration("a", "RestorePlayer", "a.go")}}); e != nil {
		t.Fatal(e)
	}
	if _, e := r.db.Exec("DELETE FROM lexical_postings; INSERT INTO lexical_dirty SELECT id FROM nodes; DELETE FROM meta WHERE key='lexical_version'"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.SearchDeclarations(ctx, lexicalQuery("restore")); e == nil {
		t.Fatal("incomplete accepted")
	}
	if e := r.Close(); e != nil {
		t.Fatal(e)
	}
	r, e := Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	page, e := r.SearchDeclarations(ctx, lexicalQuery("restore"))
	if e != nil || len(page.Matches) != 1 {
		t.Fatalf("backfill: %+v %v", page, e)
	}
	if e = r.backfillLexical(ctx); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = r.db.QueryRow("SELECT population FROM lexical_terms WHERE term='restore'").Scan(&count); e != nil || count != 1 {
		t.Fatalf("nonidempotent population: %d %v", count, e)
	}
}
func TestLexicalRankingScopeAndBudgets(t *testing.T) {
	ctx := context.Background()
	r, _ := lexicalFixture(t)
	nodes := []graph.Node{declaration("a", "RestorePlayer", "src/a.go"), declaration("b", "RestoreCache", "src/b.go"), declaration("c", "RestorePlayer", "test/c.go"), declaration("d", "RestorePlayer", "src/d.go")}
	nodes[2].Properties = map[string]string{"test_role": "helper"}
	nodes[3].External = true
	if e := r.ReplaceOwner(ctx, "fixture", graph.ParseResult{Nodes: nodes}); e != nil {
		t.Fatal(e)
	}
	page, e := r.SearchDeclarations(ctx, lexicalQuery("restore", "player"))
	if e != nil || len(page.Matches) != 2 || page.Matches[0].Node.ID != "a" {
		t.Fatalf("ranking: %+v %v", page, e)
	}
	q := lexicalQuery("restore")
	q.MaxPostings = 1
	q.MaxCandidates = 1
	page, e = r.SearchDeclarations(ctx, q)
	if e != nil || !page.Truncated || page.PostingsRead > 1 || page.CandidatesRead > 1 {
		t.Fatalf("bounds: %+v %v", page, e)
	}
	q = lexicalQuery("restore")
	q.PathPrefixes = []string{"src/b.go"}
	page, e = r.SearchDeclarations(ctx, q)
	if e != nil || len(page.Matches) != 1 || page.Matches[0].Node.ID != "b" {
		t.Fatalf("scope: %+v %v", page, e)
	}
	q.Languages = []string{"gdscript"}
	page, e = r.SearchDeclarations(ctx, q)
	if e != nil || len(page.Matches) != 0 {
		t.Fatalf("language scope: %+v %v", page, e)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = r.SearchDeclarations(cancelled, lexicalQuery("restore")); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel: %v", e)
	}
}
func TestLexicalQueriesUseIndexes(t *testing.T) {
	r, _ := lexicalFixture(t)
	for _, query := range []string{"SELECT node_id FROM lexical_postings WHERE term='player' ORDER BY node_id LIMIT 10", "WITH visited AS (SELECT node_id FROM lexical_postings WHERE term='player' ORDER BY node_id LIMIT 10) SELECT nodes.id,nodes.name,nodes.path FROM visited CROSS JOIN nodes ON nodes.id=visited.node_id", "SELECT population FROM lexical_terms WHERE term='player'", "SELECT id FROM nodes WHERE path='a.go' AND kind='file'"} {
		rows, e := r.db.Query("EXPLAIN QUERY PLAN " + query)
		if e != nil {
			t.Fatal(e)
		}
		detail := ""
		for rows.Next() {
			var id, parent, unused int
			var line string
			if e = rows.Scan(&id, &parent, &unused, &line); e != nil {
				t.Fatal(e)
			}
			detail += line
		}
		rows.Close()
		if !strings.Contains(detail, "SEARCH") || strings.Contains(detail, "SCAN nodes") || strings.Contains(detail, "SCAN lexical_") {
			t.Fatalf("unindexed query %s: %s", query, detail)
		}
	}
}

func TestLexicalEvidenceFlagsAndComponentMembership(t *testing.T) {
	ctx := context.Background()
	r, _ := lexicalFixture(t)
	implementation := declaration("impl", "RestorePlayer", "src/impl.go")
	signature := declaration("signature", "RestorePlayer", "src/interface.go")
	generated := declaration("generated", "RestorePlayer", "gen/impl.go")
	nodes := []graph.Node{implementation, signature, generated,
		{ID: "iface", Kind: graph.KindInterface, Name: "Restorer", OwnerFile: "src/interface.go"},
		{ID: "file", Kind: graph.KindFile, Name: "impl.go", OwnerFile: "src/impl.go", Location: graph.Location{Path: "src/impl.go"}},
		{ID: "genfile", Kind: graph.KindFile, Name: "impl.go", OwnerFile: "gen/impl.go", Location: graph.Location{Path: "gen/impl.go"}, Properties: map[string]string{"generated": "true"}},
		{ID: "component", Kind: graph.KindComponent, Name: "game", OwnerFile: "workspace"}}
	facts := []graph.Fact{{ID: "interface-declares", FromID: "iface", TargetID: "signature", Kind: graph.EdgeHasField, OwnerFile: "workspace"}, {ID: "component-contains", FromID: "component", TargetID: "file", Kind: graph.EdgeContains, OwnerFile: "workspace"}}
	if e := r.ReplaceOwner(ctx, "workspace", graph.ParseResult{Nodes: nodes, Facts: facts}); e != nil {
		t.Fatal(e)
	}
	if e := r.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	page, e := r.SearchDeclarations(ctx, lexicalQuery("restore", "player"))
	if e != nil || len(page.Matches) != 3 || page.Matches[0].Node.ID != "impl" {
		t.Fatalf("implementation preference: %+v %v", page, e)
	}
	for _, m := range page.Matches {
		if m.Node.ID == "signature" && !m.InterfaceMember {
			t.Fatal("missing proven interface parent")
		}
		if m.Node.ID == "generated" && !m.Generated {
			t.Fatal("missing file generated marker")
		}
	}
	q := lexicalQuery("restore")
	q.Component = "game"
	page, e = r.SearchDeclarations(ctx, q)
	if e != nil || len(page.Matches) != 1 || page.Matches[0].Node.ID != "impl" {
		t.Fatalf("component membership: %+v %v", page, e)
	}
}

func TestLexicalMigrationBackfillsExistingDeclarations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testtemp.Dir(t), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.UpTo(ctx, 14); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO nodes(id,kind,name,qualified_name,owner_file,path) VALUES(?,'function','RestorePlayer','pkg.RestorePlayer','a.go','a.go')", identity.Key("legacy")); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	page, err := r.SearchDeclarations(ctx, lexicalQuery("restore"))
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Node.ID != "legacy" {
		t.Fatalf("migration backfill: %+v %v", page, err)
	}
	if _, err = r.db.Exec("INSERT INTO lexical_dirty SELECT id FROM nodes"); err != nil {
		t.Fatal(err)
	}
	reader := &ReadRepository{reader: r}
	if _, err = reader.SearchDeclarations(ctx, lexicalQuery("restore")); err == nil {
		t.Fatal("read-only accepted pending postings")
	}
	if _, err = r.Node(ctx, "legacy"); err != nil {
		t.Fatalf("legacy read capability: %v", err)
	}
}

func TestLexicalRanksAndScopesBeforeHydrationBudget(t *testing.T) {
	ctx := context.Background()
	r, _ := lexicalFixture(t)
	nodes := []graph.Node{}
	for i := 0; i < 40; i++ {
		nodes = append(nodes, declaration(fmt.Sprintf("a%02d", i), "CommonHandler", "outside/a.go"))
	}
	nodes = append(nodes, declaration("z", "TargetCommon", "inside/a.go"))
	if err := r.ReplaceOwner(ctx, "fixture", graph.ParseResult{Nodes: nodes}); err != nil {
		t.Fatal(err)
	}
	for _, scoped := range []bool{false, true} {
		q := lexicalQuery("common", "target")
		q.MaxCandidates = 1
		if scoped {
			q.PathPrefixes = []string{"inside"}
		}
		page, err := r.SearchDeclarations(ctx, q)
		if err != nil || len(page.Matches) != 1 || page.Matches[0].Node.ID != "z" || page.CandidatesRead != 1 || page.PostingsRead != 42 {
			t.Fatalf("scope=%v: %+v %v", scoped, page, err)
		}
	}
}
func TestLexicalGeneratedBindingIsNotImplementation(t *testing.T) {
	ctx := context.Background()
	r, _ := lexicalFixture(t)
	implementation := declaration("z", "BuildEnvelope", "impl.go")
	binding := declaration("a", "BuildEnvelope", "buf.gen.yaml")
	binding.Properties = map[string]string{"generated_binding": "true"}
	if err := r.ReplaceOwner(ctx, "fixture", graph.ParseResult{Nodes: []graph.Node{implementation, binding}}); err != nil {
		t.Fatal(err)
	}
	page, err := r.SearchDeclarations(ctx, lexicalQuery("build", "envelope"))
	if err != nil || len(page.Matches) != 2 || page.Matches[0].Node.ID != "z" || !page.Matches[1].Generated {
		t.Fatalf("binding: %+v %v", page, err)
	}
}

func TestLexicalMetadataBudgetBoundsEmptyTerms(t *testing.T) {
	ctx := context.Background()
	r, _ := lexicalFixture(t)
	q := lexicalQuery("absent", "missing", "unknown")
	q.MaxMetadata = 4
	page, err := r.SearchDeclarations(ctx, q)
	if err != nil || !page.Truncated || page.MetadataReads != 4 || page.PostingsRead != 0 || page.CandidatesRead != 0 {
		t.Fatalf("metadata budget: %+v %v", page, err)
	}
	q.MaxMetadata = 2
	page, err = r.SearchDeclarations(ctx, q)
	if err != nil || !page.Truncated || page.MetadataReads > 2 {
		t.Fatalf("tiny metadata budget: %+v %v", page, err)
	}
}

func TestLexicalMetadataPlanStaysWithinCandidateNeighborhood(t *testing.T) {
	r, _ := lexicalFixture(t)
	source, err := os.ReadFile("queries/lexical.sql")
	if err != nil {
		t.Fatal(err)
	}
	query := strings.SplitN(string(source), "-- name: LexicalFlags :one", 2)[1]
	rows, err := r.db.Query("EXPLAIN QUERY PLAN "+query, sql.Named("path", "a.go"), sql.Named("id", identity.Key("a")), sql.Named("component", "game"), sql.Named("component_id", identity.Key("game")))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "edges_kind") || strings.Contains(detail, "nodes_kind") || strings.Contains(detail, "SCAN nodes") || strings.Contains(detail, "SCAN edges") {
			t.Fatalf("global scan in candidate metadata: %s", detail)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
