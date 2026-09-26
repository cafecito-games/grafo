package postgres_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
)

func TestParserExtractsPostgreSQLSchemaAndDataAccess(t *testing.T) {
	content := []byte(`CREATE TABLE public.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  owner_id uuid NOT NULL,
  parent_id bigint REFERENCES public.parent_accounts(id)
);

CREATE VIEW public.active_accounts AS
  SELECT id, owner_id FROM public.accounts WHERE archived_at IS NULL;

CREATE MATERIALIZED VIEW public.account_totals AS
  SELECT owner_id, count(*) FROM public.accounts GROUP BY owner_id;

CREATE UNIQUE INDEX accounts_owner_idx ON public.accounts USING btree (owner_id);

CREATE FUNCTION public.accounts_for(owner uuid)
RETURNS SETOF public.accounts
LANGUAGE SQL
BEGIN ATOMIC
  SELECT * FROM public.accounts WHERE owner_id = owner;
END;

CREATE FUNCTION public.account_by_id(account_id bigint)
RETURNS public.accounts
LANGUAGE SQL
AS $$ SELECT * FROM public.accounts WHERE id = account_id $$;

INSERT INTO public.account_audit (account_id)
SELECT id FROM public.accounts;

UPDATE public.accounts
SET owner_id = owners.id
FROM public.owners
WHERE owners.legacy_id = accounts.owner_id;

DELETE FROM public.account_audit
USING public.accounts
WHERE accounts.id = account_audit.account_id;

MERGE INTO public.accounts AS current
USING public.incoming_accounts AS incoming
ON current.id = incoming.id
WHEN MATCHED THEN UPDATE SET owner_id = incoming.owner_id;

COPY public.account_imports FROM STDIN;
COPY public.accounts TO STDOUT;
TRUNCATE public.account_archive;
`)
	result, err := postgresparser.New().Parse(context.Background(), parserapi.Input{
		Path: "db/schema.sql", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}

	assertNode(t, result.Nodes, graph.KindTable, "public.accounts")
	assertNode(t, result.Nodes, graph.KindColumn, "public.accounts.owner_id")
	assertNode(t, result.Nodes, graph.KindView, "public.active_accounts")
	assertNode(t, result.Nodes, graph.KindView, "public.account_totals")
	assertNode(t, result.Nodes, graph.KindIndex, "public.accounts_owner_idx")
	assertNode(t, result.Nodes, graph.KindFunction, "public.accounts_for")
	assertNode(t, result.Nodes, graph.KindFunction, "public.account_by_id")
	assertNode(t, result.Nodes, graph.KindParameter, "public.accounts_for.owner")

	assertFact(t, result.Facts, graph.EdgeHasField, "", "")
	assertFact(t, result.Facts, graph.EdgeReferences, "public.parent_accounts", "")
	assertFactFrom(t, result, graph.KindView, "public.active_accounts", graph.EdgeReads, "public.accounts")
	assertFactFrom(t, result, graph.KindView, "public.account_totals", graph.EdgeReads, "public.accounts")
	assertFactFrom(t, result, graph.KindFunction, "public.accounts_for", graph.EdgeReads, "public.accounts")
	assertFactFrom(t, result, graph.KindFunction, "public.account_by_id", graph.EdgeReads, "public.accounts")
	assertFactFrom(t, result, graph.KindIndex, "public.accounts_owner_idx", graph.EdgeReferences, "public.accounts")
	assertFact(t, result.Facts, graph.EdgeWrites, "public.account_audit", "")
	assertFact(t, result.Facts, graph.EdgeWrites, "public.accounts", "")
	assertFact(t, result.Facts, graph.EdgeWrites, "public.account_imports", "")
	assertFact(t, result.Facts, graph.EdgeWrites, "public.account_archive", "")
	assertFact(t, result.Facts, graph.EdgeReads, "public.owners", "")
	assertFact(t, result.Facts, graph.EdgeReads, "public.incoming_accounts", "")
}

func TestParserDeclaresPostgreSQLDialect(t *testing.T) {
	parser := postgresparser.New()
	if parser.Name() != "postgres" {
		t.Fatalf("dialect name = %q", parser.Name())
	}
	extensions := parser.Extensions()
	if len(extensions) != 2 || extensions[0] != ".pgsql" || extensions[1] != ".psql" {
		t.Fatalf("unexpected PostgreSQL extensions: %#v", extensions)
	}
}

func TestParserKeepsOverloadedFunctionsDistinct(t *testing.T) {
	content := []byte(`
CREATE FUNCTION public.lookup(value bigint) RETURNS bigint LANGUAGE SQL AS $$ SELECT value $$;
CREATE FUNCTION public.lookup(value text) RETURNS text LANGUAGE SQL AS $$ SELECT value $$;
`)
	result, err := postgresparser.New().Parse(context.Background(), parserapi.Input{
		Path: "db/functions.sql", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindFunction && node.QualifiedName == "public.lookup" {
			ids[node.ID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected two distinct overloads, got %#v", result.Nodes)
	}
}

func TestParserReportsInvalidPostgreSQL(t *testing.T) {
	result, err := postgresparser.New().Parse(context.Background(), parserapi.Input{
		Path: "broken.sql", Content: []byte("CREATE TABLE missing ("), Repository: "sample",
	})
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile {
		t.Fatalf("expected the file node to be preserved, got %#v", result.Nodes)
	}
}

func assertNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, qualified, nodes)
}

func assertFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string, targetKind graph.NodeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && (target == "" || fact.Target == target) && (targetKind == "" || fact.TargetKind == targetKind) {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
}

func assertFactFrom(t *testing.T, result graph.ParseResult, sourceKind graph.NodeKind, source string, kind graph.EdgeKind, target string) {
	t.Helper()
	var sourceID string
	for _, node := range result.Nodes {
		if node.Kind == sourceKind && node.QualifiedName == source {
			sourceID = node.ID
			break
		}
	}
	if sourceID == "" {
		t.Fatalf("missing source %s node %q", sourceKind, source)
	}
	for _, fact := range result.Facts {
		if fact.FromID == sourceID && fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact from %q to %q; got %#v", kind, source, target, result.Facts)
}
