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

func TestEquivalentSQLNormalizesPostgreSQLTableIdentifiers(t *testing.T) {
	tests := []struct {
		name, handwritten, generated string
		schemas                      map[string]schemaTable
	}{
		{
			name: "unquoted mixed case", handwritten: `SELECT * FROM Users`, generated: `SELECT id, name FROM Users`,
			schemas: schemaTables("users"),
		},
		{
			name: "quoted mixed case", handwritten: `SELECT * FROM "Users"`, generated: `SELECT id, name FROM "Users"`,
			schemas: schemaTables("Users"),
		},
		{
			name: "qualified quoted mixed case", handwritten: `SELECT * FROM public."Users"`, generated: `SELECT id, name FROM public."Users"`,
			schemas: schemaTables("public.Users"),
		},
		{
			name: "unquoted chooses folded table", handwritten: `SELECT * FROM Users`, generated: `SELECT id, name FROM Users`,
			schemas: schemaTables("users", "Users"),
		},
		{
			name: "unquoted preserves non ascii case", handwritten: `SELECT * FROM Üsers`, generated: `SELECT id, name FROM Üsers`,
			schemas: schemaTables("Üsers", "üsers"),
		},
		{
			name: "embed unquoted mixed case", handwritten: `SELECT sqlc.embed(u) FROM Users AS u`, generated: `SELECT u.id, u.name FROM Users AS u`,
			schemas: schemaTables("users"),
		},
		{
			name: "embed quoted mixed case", handwritten: `SELECT sqlc.embed(u) FROM "Users" AS u`, generated: `SELECT u.id, u.name FROM "Users" AS u`,
			schemas: schemaTables("Users"),
		},
		{
			name: "embed qualified quoted mixed case", handwritten: `SELECT sqlc.embed(u) FROM public."Users" AS u`, generated: `SELECT u.id, u.name FROM public."Users" AS u`,
			schemas: schemaTables("public.Users"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !equivalentSQL(test.handwritten, test.generated, "postgres", test.schemas) {
				t.Fatal("equivalent PostgreSQL was rejected")
			}
		})
	}
}

func TestEquivalentSQLScopesProjectionWildcards(t *testing.T) {
	schemas := map[string]schemaTable{
		"users":  {columns: []string{"id", "name"}},
		"orders": {columns: []string{"id", "user_id"}},
	}
	tests := []struct {
		name, handwritten, generated string
		want                         bool
	}{
		{name: "insert returning", handwritten: `INSERT INTO users (name) VALUES ($1) RETURNING *`, generated: `INSERT INTO users (name) VALUES ($1) RETURNING id, name`, want: true},
		{name: "update returning", handwritten: `UPDATE users SET name = $1 RETURNING *`, generated: `UPDATE users SET name = $1 RETURNING id, name`, want: true},
		{name: "update from returning", handwritten: `UPDATE users SET name = orders.id::text FROM orders RETURNING *`, generated: `UPDATE users SET name = orders.id::text FROM orders RETURNING id, name`, want: true},
		{name: "insert select returning", handwritten: `INSERT INTO users (name) SELECT id::text FROM orders RETURNING *`, generated: `INSERT INTO users (name) SELECT id::text FROM orders RETURNING id, name`, want: true},
		{name: "delete returning", handwritten: `DELETE FROM users RETURNING *`, generated: `DELETE FROM users RETURNING id, name`, want: true},
		{name: "nested select", handwritten: `SELECT * FROM users WHERE id IN (SELECT user_id FROM orders)`, generated: `SELECT id, name FROM users WHERE id IN (SELECT user_id FROM orders)`, want: true},
		{name: "literal from", handwritten: `SELECT * FROM users WHERE name = 'from orders'`, generated: `SELECT id, name FROM users WHERE name = 'from orders'`, want: true},
		{name: "cte source unresolved", handwritten: `WITH r AS (SELECT id FROM users) SELECT * FROM r`, generated: `WITH r AS (SELECT id FROM users) SELECT id FROM r`, want: false},
		{name: "derived source unresolved", handwritten: `SELECT * FROM (SELECT id FROM users) AS r`, generated: `SELECT id FROM (SELECT id FROM users) AS r`, want: false},
		{name: "join unresolved", handwritten: `SELECT * FROM users JOIN orders ON orders.user_id = users.id`, generated: `SELECT id, name FROM users JOIN orders ON orders.user_id = users.id`, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := equivalentSQL(test.handwritten, test.generated, "postgres", schemas); got != test.want {
				t.Fatalf("equivalentSQL() = %v, want %v", got, test.want)
			}
		})
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
		{name: "postgres duplicate defaults from unqualified selector", selector: "users", engine: "postgres", tables: schemaTables("users", "public.users"), wantOK: false},
		{name: "postgres duplicate defaults from qualified selector", selector: "public.users", engine: "postgres", tables: schemaTables("users", "public.users"), wantOK: false},
		{name: "postgres quoted table case differs", selector: "Users", engine: "postgres", tables: schemaTables("users"), wantOK: false},
		{name: "postgres quoted schema case differs", selector: "Public.users", engine: "postgres", tables: schemaTables("public.users"), wantOK: false},
		{name: "postgres multiple non-default declarations", selector: "users", engine: "postgres", tables: schemaTables("audit.users", "other.users"), wantOK: false},
		{name: "sqlite unqualified selector with default table", selector: "users", engine: "sqlite", tables: schemaTables("main.users"), wantOK: true},
		{name: "sqlite default selector with unqualified table", selector: "main.users", engine: "sqlite", tables: schemaTables("users"), wantOK: true},
		{name: "sqlite duplicate defaults from unqualified selector", selector: "users", engine: "sqlite", tables: schemaTables("users", "main.users"), wantOK: false},
		{name: "sqlite duplicate defaults from qualified selector", selector: "main.users", engine: "sqlite", tables: schemaTables("users", "main.users"), wantOK: false},
		{name: "sqlite postgres schema is non-default", selector: "users", engine: "sqlite", tables: schemaTables("public.users"), wantOK: false},
		{name: "sqlite custom schema is non-default", selector: "users", engine: "sqlite", tables: schemaTables("audit.users"), wantOK: false},
		{name: "sqlite table case folds", selector: "Users", engine: "sqlite", tables: schemaTables("users"), wantOK: true},
		{name: "sqlite default schema case folds", selector: "Main.users", engine: "sqlite", tables: schemaTables("main.users"), wantOK: true},
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
