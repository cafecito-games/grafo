package sqlcbinding

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/parser/sqltemplate"
)

func TestEquivalentSQLReusesNamedParameters(t *testing.T) {
	handwritten := `SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN instr(qualified_name, @target) > 0 OR instr(name, @target) > 0 THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE (lower(name) LIKE '%' || lower(@target) || '%'
    OR lower(qualified_name) LIKE '%' || lower(@target) || '%')
  AND external = CAST(@external AS INTEGER)
  AND (CAST(@kind AS TEXT) = '' OR kind = CAST(@kind AS TEXT));`
	generated := `-- name: CountNodeMatchesBySubstring :one
SELECT
    COUNT(*) AS total,
    CAST(COALESCE(SUM(CASE WHEN instr(qualified_name, ?1) > 0 OR instr(name, ?1) > 0 THEN 1 ELSE 0 END), 0) AS INTEGER) AS strict_matches
FROM nodes
WHERE (lower(name) LIKE '%' || lower(?1) || '%'
    OR lower(qualified_name) LIKE '%' || lower(?1) || '%')
  AND external = CAST(?2 AS INTEGER)
  AND (CAST(?3 AS TEXT) = '' OR kind = CAST(?3 AS TEXT))
`
	if !equivalentSQL(handwritten, generated, "sqlite", nil) {
		document := sqltemplate.Analyze([]byte(handwritten), "sqlite")
		t.Logf("handwritten: %q", comparableSQL(string(document.SQL)))
		t.Logf("generated:   %q", comparableSQL(generated))
		t.Fatal("reused named parameters were reported stale")
	}
}

func TestEquivalentSQLExpandsProjectionWildcardFromSchema(t *testing.T) {
	schemas := map[string]schemaTable{
		"nodes": {columns: []string{"id", "kind", "name"}},
	}
	handwritten := `SELECT * FROM nodes WHERE id = ?;`
	generated := `SELECT id, kind, name FROM nodes WHERE id = ?`
	if !equivalentSQL(handwritten, generated, "sqlite", schemas) {
		t.Fatal("schema-backed wildcard expansion was reported stale")
	}
}

func TestSchemaColumnsRequiresExactOrDefaultSchema(t *testing.T) {
	tests := []struct {
		name, selector, engine string
		tables                 map[string]schemaTable
		wantOK                 bool
	}{
		{name: "postgres unqualified exact", selector: "users", engine: "postgres", tables: schemaTables("users"), wantOK: true},
		{name: "postgres qualified non-default selector", selector: "audit.users", engine: "postgres", tables: schemaTables("users"), wantOK: false},
		{name: "postgres unqualified selector with non-default table", selector: "users", engine: "postgres", tables: schemaTables("audit.users"), wantOK: false},
		{name: "postgres unqualified selector with default table", selector: "users", engine: "postgres", tables: schemaTables("public.users"), wantOK: true},
		{name: "postgres default selector with unqualified table", selector: "public.users", engine: "postgres", tables: schemaTables("users"), wantOK: true},
		{name: "postgres duplicate default declarations", selector: "PUBLIC.users", engine: "postgres", tables: schemaTables("users", "public.users"), wantOK: false},
		{name: "postgres multiple non-default declarations", selector: "users", engine: "postgres", tables: schemaTables("audit.users", "other.users"), wantOK: false},
		{name: "sqlite unqualified selector with default table", selector: "users", engine: "sqlite", tables: schemaTables("main.users"), wantOK: true},
		{name: "sqlite default selector with unqualified table", selector: "main.users", engine: "sqlite", tables: schemaTables("users"), wantOK: true},
		{name: "sqlite postgres schema is non-default", selector: "users", engine: "sqlite", tables: schemaTables("public.users"), wantOK: false},
		{name: "sqlite custom schema is non-default", selector: "users", engine: "sqlite", tables: schemaTables("audit.users"), wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, ok := schemaColumns(test.tables, test.selector, test.engine)
			if ok != test.wantOK {
				t.Fatalf("schemaColumns(%q) ok = %v, want %v", test.selector, ok, test.wantOK)
			}
		})
	}
}

func schemaTables(names ...string) map[string]schemaTable {
	tables := make(map[string]schemaTable, len(names))
	for _, name := range names {
		tables[name] = schemaTable{columns: []string{"id", "name"}}
	}
	return tables
}
