package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	sqliteparser "github.com/cafecito-games/grafo/internal/parser/sql/sqlite"
)

func TestParserExtractsSQLiteSchemaAndDataAccess(t *testing.T) {
	content := []byte(`CREATE TABLE accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_id TEXT NOT NULL UNIQUE,
  parent_id INTEGER REFERENCES parent_accounts(id),
  display_name TEXT GENERATED ALWAYS AS (owner_id || ':' || id) STORED
) STRICT;

CREATE TABLE account_audit (
  account_id INTEGER NOT NULL,
  action TEXT NOT NULL,
  FOREIGN KEY (account_id) REFERENCES accounts(id)
) WITHOUT ROWID;

CREATE VIRTUAL TABLE search_documents USING fts5(title, body);

CREATE VIEW active_accounts AS
  SELECT id, owner_id FROM accounts WHERE archived_at IS NULL;

CREATE UNIQUE INDEX accounts_owner_idx ON accounts(owner_id)
  WHERE owner_id IS NOT NULL;

CREATE TRIGGER audit_account_update
AFTER UPDATE OF owner_id ON accounts
BEGIN
  INSERT INTO account_audit(account_id, action) VALUES (new.id, 'update');
END;

INSERT INTO accounts(owner_id) SELECT id FROM imported_owners;

UPDATE accounts
SET owner_id = owners.id
FROM owners
WHERE owners.legacy_id = accounts.owner_id;

DELETE FROM account_audit
WHERE account_id IN (SELECT id FROM accounts);

WITH recent_accounts AS (SELECT id FROM accounts)
SELECT * FROM recent_accounts;
`)
	result, err := sqliteparser.New().Parse(context.Background(), parserapi.Input{
		Path: "db/schema.sql", Content: content, Repository: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}

	assertNode(t, result.Nodes, graph.KindTable, "accounts")
	assertNode(t, result.Nodes, graph.KindColumn, "accounts.owner_id")
	assertNode(t, result.Nodes, graph.KindTable, "account_audit")
	assertNode(t, result.Nodes, graph.KindTable, "search_documents")
	assertNode(t, result.Nodes, graph.KindView, "active_accounts")
	assertNode(t, result.Nodes, graph.KindIndex, "accounts_owner_idx")
	assertNode(t, result.Nodes, graph.KindFunction, "audit_account_update")

	assertNodeProperty(t, result.Nodes, graph.KindTable, "accounts", "strict", "true")
	assertNodeProperty(t, result.Nodes, graph.KindTable, "account_audit", "without_rowid", "true")
	assertNodeProperty(t, result.Nodes, graph.KindTable, "search_documents", "module", "fts5")
	assertNodeProperty(t, result.Nodes, graph.KindColumn, "accounts.id", "autoincrement", "true")
	assertNodeProperty(t, result.Nodes, graph.KindColumn, "accounts.display_name", "generated_kind", "stored")
	assertNodeProperty(t, result.Nodes, graph.KindFunction, "audit_account_update", "object_kind", "trigger")

	assertFact(t, result.Facts, graph.EdgeReferences, "parent_accounts")
	assertFactFrom(t, result, graph.KindView, "active_accounts", graph.EdgeReads, "accounts")
	assertFactFrom(t, result, graph.KindIndex, "accounts_owner_idx", graph.EdgeReferences, "accounts")
	assertFactFrom(t, result, graph.KindFunction, "audit_account_update", graph.EdgeReferences, "accounts")
	assertFactFrom(t, result, graph.KindFunction, "audit_account_update", graph.EdgeWrites, "account_audit")
	assertFact(t, result.Facts, graph.EdgeWrites, "accounts")
	assertFact(t, result.Facts, graph.EdgeWrites, "account_audit")
	assertFact(t, result.Facts, graph.EdgeReads, "imported_owners")
	assertFact(t, result.Facts, graph.EdgeReads, "owners")
	assertFact(t, result.Facts, graph.EdgeReads, "accounts")
	assertNoFact(t, result.Facts, graph.EdgeReads, "recent_accounts")
	assertRelationFactsRemainTableOrViewAgnostic(t, result.Facts)
}

func TestParserExtractsCreateTableAsSelect(t *testing.T) {
	result, err := sqliteparser.New().Parse(context.Background(), parserapi.Input{
		Path: "db/report.sql", Content: []byte("CREATE TABLE report AS SELECT * FROM events;"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFactFrom(t, result, graph.KindTable, "report", graph.EdgeReads, "events")
}

func TestParserDeclaresSQLiteDialectWithoutBinaryExtensions(t *testing.T) {
	p := sqliteparser.New()
	if p.Name() != "sqlite" || p.Language() != "sqlite" {
		t.Fatalf("unexpected dialect identity: %q %q", p.Name(), p.Language())
	}
	if extensions := p.Extensions(); len(extensions) != 0 {
		t.Fatalf("SQLite parser claimed binary-looking extensions: %#v", extensions)
	}
}

func TestParserReportsInvalidSQLiteWithLocation(t *testing.T) {
	result, err := sqliteparser.New().Parse(context.Background(), parserapi.Input{
		Path: "broken.sql", Content: []byte("CREATE TABLE missing ("), Repository: "sample",
	})
	if err == nil || !strings.Contains(err.Error(), "parse SQLite") || !strings.Contains(err.Error(), "1:23") {
		t.Fatalf("expected positioned SQLite parse error, got %v", err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile {
		t.Fatalf("expected the file node to be preserved, got %#v", result.Nodes)
	}
}

func TestParserHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := sqliteparser.New().Parse(ctx, parserapi.Input{Path: "schema.sql", Content: []byte("SELECT 1")})
	if err != context.Canceled {
		t.Fatalf("expected context cancellation, got %v", err)
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

func assertNodeProperty(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified, key, value string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			if node.Properties[key] != value {
				t.Fatalf("%s %q property %q = %q, want %q", kind, qualified, key, node.Properties[key], value)
			}
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, qualified)
}

func assertFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
}

func assertNoFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			t.Fatalf("unexpected %s fact to %q", kind, target)
		}
	}
}

func assertRelationFactsRemainTableOrViewAgnostic(t *testing.T, facts []graph.Fact) {
	t.Helper()
	for _, fact := range facts {
		if (fact.Kind == graph.EdgeReads || fact.Kind == graph.EdgeWrites) && fact.TargetKind != "" {
			t.Fatalf("%s fact to %q unexpectedly requires target kind %q", fact.Kind, fact.Target, fact.TargetKind)
		}
	}
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
