package sql_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	sqlparser "github.com/cafecito-games/grafo/internal/parser/sql"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
	sqliteparser "github.com/cafecito-games/grafo/internal/parser/sql/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

type fakeDialect struct {
	name       string
	extensions []string
	accept     string
}

func (d fakeDialect) Name() string         { return d.name }
func (d fakeDialect) Extensions() []string { return d.extensions }

func (d fakeDialect) Probe(_ context.Context, input parserapi.Input) error {
	if d.accept == "" || strings.Contains(string(input.Content), d.accept) {
		return nil
	}
	return errors.New("syntax rejected")
}

func (d fakeDialect) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	builder := parserapi.NewBuilder(input, "sql")
	builder.Declare(builder.FileID(), graph.Node{Kind: graph.KindTable, Name: d.name,
		QualifiedName: d.name, Location: graph.Location{Path: input.Path, Line: 1, Column: 1}})
	return builder.Finish(), nil
}

func TestRouterSupportsCommonAndDialectExtensions(t *testing.T) {
	router := sqlparser.New(fakeDialect{name: "postgres", extensions: []string{"pgsql", ".psql"}})
	for _, filePath := range []string{"schema.sql", "function.PGSQL", "console.psql"} {
		if !router.Supports(filePath) {
			t.Errorf("expected support for %s", filePath)
		}
	}
	if router.Supports("schema.sqlite") || router.Supports("schema.mysql") {
		t.Fatal("router claimed an extension not provided by an installed dialect")
	}
}

func TestRouterSelectionPrecedence(t *testing.T) {
	root := testtemp.Dir(t)
	config := `sql:
  default_dialect: sqlite
  paths:
    "db/special/**": sqlite
    "db/special/schema.psql": postgres
`
	if err := os.WriteFile(filepath.Join(root, "grafo.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	router := sqlparser.New(
		fakeDialect{name: "sqlite", extensions: []string{".sqlite"}, accept: "sqlite-only"},
		fakeDialect{name: "postgres", extensions: []string{".psql", ".pgsql"}, accept: "postgres-only"},
	)
	tests := []struct {
		name, path, want string
	}{
		{name: "most specific path mapping overrides extension", path: "db/special/schema.psql", want: "postgres"},
		{name: "path mapping overrides dialect extension", path: "db/special/other.psql", want: "sqlite"},
		{name: "extension overrides default", path: "db/ordinary.psql", want: "postgres"},
		{name: "default handles common extension", path: "db/ordinary.sql", want: "sqlite"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := router.Parse(context.Background(), parserapi.Input{Root: root, Path: test.path, Content: []byte("common")})
			if err != nil {
				t.Fatal(err)
			}
			if got := declaredTable(result); got != test.want {
				t.Fatalf("selected %q, want %q", got, test.want)
			}
			for _, node := range result.Nodes {
				if node.Properties["dialect"] != test.want {
					t.Fatalf("node %q dialect = %q, want %q", node.QualifiedName, node.Properties["dialect"], test.want)
				}
			}
		})
	}
}

func TestRouterProbesPlainSQLDeterministically(t *testing.T) {
	postgres := fakeDialect{name: "postgres", accept: "postgres-only"}
	sqlite := fakeDialect{name: "sqlite", accept: "sqlite-only"}
	router := sqlparser.New(sqlite, postgres)

	result, err := router.Parse(context.Background(), parserapi.Input{Path: "schema.sql", Content: []byte("postgres-only")})
	if err != nil {
		t.Fatal(err)
	}
	if got := declaredTable(result); got != "postgres" {
		t.Fatalf("selected %q, want postgres", got)
	}

	_, err = router.Parse(context.Background(), parserapi.Input{Path: "schema.sql", Content: []byte("postgres-only sqlite-only")})
	if err == nil || !strings.Contains(err.Error(), "ambiguous between postgres, sqlite") {
		t.Fatalf("expected sorted ambiguity error, got %v", err)
	}
	reversed := sqlparser.New(postgres, sqlite)
	_, reversedErr := reversed.Parse(context.Background(), parserapi.Input{Path: "schema.sql", Content: []byte("postgres-only sqlite-only")})
	if reversedErr == nil || reversedErr.Error() != err.Error() {
		t.Fatalf("registration order changed ambiguity error:\nfirst:  %v\nsecond: %v", err, reversedErr)
	}

	_, err = router.Parse(context.Background(), parserapi.Input{Path: "schema.sql", Content: []byte("neither")})
	if err == nil || !strings.Contains(err.Error(), "no dialect accepted") {
		t.Fatalf("expected rejection diagnostic, got %v", err)
	}
}

func TestRouterKeepsPlainSQLUsableWithOnlyPostgreSQLInstalled(t *testing.T) {
	router := sqlparser.New(postgresparser.New())
	result, err := router.Parse(context.Background(), parserapi.Input{
		Path: "schema.sql", Content: []byte("CREATE TABLE events (id bigint PRIMARY KEY)"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := declaredTable(result); got != "events" {
		t.Fatalf("declared table = %q, want events", got)
	}
	for _, node := range result.Nodes {
		if node.Properties["dialect"] != "postgres" {
			t.Fatalf("node dialect = %q, want postgres", node.Properties["dialect"])
		}
	}
}

func TestRouterSelectsSQLiteSpecificSyntax(t *testing.T) {
	router := sqlparser.New(postgresparser.New(), sqliteparser.New())
	result, err := router.Parse(context.Background(), parserapi.Input{
		Path: "schema.sql", Content: []byte("CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT) STRICT;"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := declaredTable(result); got != "events" {
		t.Fatalf("declared table = %q, want events", got)
	}
	for _, node := range result.Nodes {
		if node.Properties["dialect"] != "sqlite" {
			t.Fatalf("node dialect = %q, want sqlite", node.Properties["dialect"])
		}
	}
}

func TestRouterRequiresConfigurationForPortableSQL(t *testing.T) {
	router := sqlparser.New(postgresparser.New(), sqliteparser.New())
	_, err := router.Parse(context.Background(), parserapi.Input{
		Path: "schema.sql", Content: []byte("CREATE TABLE events (id INTEGER PRIMARY KEY);"),
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous between postgres, sqlite") {
		t.Fatalf("expected portable SQL to require a configured dialect, got %v", err)
	}
}

func TestRouterRejectsUnavailableConfiguredDialect(t *testing.T) {
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "grafo.yaml"), []byte("sql:\n  default_dialect: sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	router := sqlparser.New(fakeDialect{name: "postgres"})
	result, err := router.Parse(context.Background(), parserapi.Input{Root: root, Path: "schema.sql", Content: []byte("select 1")})
	if err == nil || !strings.Contains(err.Error(), `unavailable dialect "sqlite"`) {
		t.Fatalf("expected unavailable dialect error, got %v", err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile {
		t.Fatalf("expected error result to retain file node, got %#v", result.Nodes)
	}
}

func TestRouterUsesSqlcEngineOnlyWhenGrafoHasNoSelection(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries.sql": "-- name: GetUser :one\nSELECT id FROM users WHERE id = sqlc.arg(id);",
	})
	router := sqlparser.New(postgresparser.New(), sqliteparser.New())
	result, err := router.Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: []byte("-- name: GetUser :one\nSELECT id FROM users WHERE id = sqlc.arg(id);"),
		SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"}, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range result.Nodes {
		if node.Properties["dialect"] != "sqlite" {
			t.Fatalf("node %q dialect = %q, want sqlite", node.QualifiedName, node.Properties["dialect"])
		}
	}
}

func TestRouterDiagnosesGrafoSqlcEngineMismatchAndDisablesBinding(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"grafo.yaml": "sql:\n  default_dialect: postgres\n",
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries.sql": "-- name: GetUser :one\nSELECT id FROM users WHERE id = sqlc.arg(id);",
		"internal/db/queries.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT id FROM users WHERE id = ?` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`,
	})
	paths := []string{"grafo.yaml", "sqlc.yaml", "schema.sql", "queries.sql", "internal/db/queries.sql.go"}
	content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content, SourcePaths: paths, RepoID: "repo:sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(result, "bindings disabled") {
		t.Fatalf("missing mismatch diagnostic: %#v", result.Diagnostics)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeGeneratedFrom {
			t.Fatalf("mismatch produced binding: %#v", fact)
		}
	}
}

func TestRouterBindsGeneratedMethodToCanonicalQuery(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries.sql": "-- name: GetUser :one\nSELECT id FROM users WHERE id = sqlc.arg(id);",
		"internal/db/queries.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT id FROM users WHERE id = ?` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`,
	})
	paths := []string{"sqlc.yaml", "schema.sql", "queries.sql", "internal/db/queries.sql.go"}
	content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content, SourcePaths: paths, RepoID: "repo:sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeGeneratedFrom && fact.Source == "example.com/sample/internal/db.Queries.GetUser" && fact.TargetID != "" {
			return
		}
	}
	t.Fatalf("missing generated binding: %#v", result.Facts)
}

func TestRouterExpandsSqlcEmbedOnlyWithUniqueConfiguredSchema(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);",
		"queries.sql": "-- name: GetUser :one\nSELECT sqlc.embed(u) FROM users AS u WHERE u.id = sqlc.arg(id);",
	})
	content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content,
		SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"}, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	queryID := ""
	for _, node := range result.Nodes {
		if node.Kind == graph.KindSQLQuery && node.Name == "GetUser" {
			queryID = node.ID
		}
	}
	want := map[string]bool{"users.id": false, "users.name": false}
	for _, fact := range result.Facts {
		if fact.FromID == queryID && fact.Kind == graph.EdgeReads && fact.TargetKind == graph.KindColumn {
			if _, ok := want[fact.Target]; ok {
				want[fact.Target] = true
			}
		}
	}
	for target, found := range want {
		if !found {
			t.Fatalf("missing embed column %s in %#v", target, result.Facts)
		}
	}
	if hasDiagnostic(result, "column expansion lacks") {
		t.Fatalf("complete schema was reported unresolved: %#v", result.Diagnostics)
	}
}

func TestRouterExpandsOrdinaryWildcardOnlyWithCompleteSchema(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);",
		"queries.sql": "-- name: ListUsers :many\nSELECT * FROM users;",
	})
	content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content, SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"users.id", "users.name"} {
		found := false
		for _, fact := range result.Facts {
			found = found || fact.Kind == graph.EdgeReads && fact.Target == target && fact.TargetKind == graph.KindColumn
		}
		if !found {
			t.Fatalf("missing expanded wildcard column %q: %#v", target, result.Facts)
		}
	}
}

func TestRouterDoesNotExpandWildcardAcrossAlteredSchema(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);\nALTER TABLE users ADD COLUMN email TEXT;",
		"queries.sql": "-- name: ListUsers :many\nSELECT * FROM users;",
	})
	content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content, SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(result, "wildcard column usage is unresolved") {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	for _, fact := range result.Facts {
		if fact.TargetKind == graph.KindColumn && fact.Properties["form"] == "wildcard" {
			t.Fatalf("altered schema produced wildcard column evidence: %#v", fact)
		}
	}
}

func TestRouterDoesNotExpandWildcardAcrossCaseDifferentSQLiteAlter(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); ALTER TABLE Users ADD COLUMN email TEXT;",
		"queries.sql": "-- name: ListUsers :many\nSELECT * FROM users;",
	})
	content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content, SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(result, "wildcard column usage is unresolved") {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	for _, fact := range result.Facts {
		if fact.TargetKind == graph.KindColumn && fact.Producer == "sqlcbinding" {
			t.Fatalf("case-different ALTER produced wildcard column evidence: %#v", fact)
		}
	}
}

func TestRouterDoesNotExpandWildcardAcrossPostgreSQLSchemaMutations(t *testing.T) {
	tests := []struct {
		name, schema, query string
	}{
		{name: "rename column", schema: "CREATE TABLE users (id bigint PRIMARY KEY, name text); ALTER TABLE users RENAME COLUMN name TO full_name;", query: "SELECT * FROM users;"},
		{name: "qualified alter", schema: "CREATE TABLE users (id bigint PRIMARY KEY, name text); ALTER TABLE public.users ADD COLUMN email text;", query: "SELECT * FROM users;"},
		{name: "rename table", schema: "CREATE TABLE users (id bigint PRIMARY KEY, name text); ALTER TABLE users RENAME TO members;", query: "SELECT * FROM members;"},
		{name: "drop and recreate", schema: "CREATE TABLE users (id bigint PRIMARY KEY, old_name text); DROP TABLE users; CREATE TABLE users (id bigint PRIMARY KEY, new_name text);", query: "SELECT * FROM users;"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := testtemp.Dir(t)
			writeRouterFiles(t, root, map[string]string{
				"sqlc.yaml": `version: "2"
sql:
  - engine: postgresql
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
				"schema.sql":  test.schema,
				"queries.sql": "-- name: ListUsers :many\n" + test.query,
			})
			content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
				Root: root, Path: "queries.sql", Content: content, SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(result, "wildcard column usage is unresolved") {
				t.Fatalf("diagnostics = %#v", result.Diagnostics)
			}
			for _, fact := range result.Facts {
				if fact.TargetKind == graph.KindColumn && fact.Producer == "sqlcbinding" {
					t.Fatalf("schema mutation produced wildcard column evidence: %#v", fact)
				}
			}
		})
	}
}

func TestRouterDoesNotExpandWildcardAcrossPostgreSQLCopiedColumns(t *testing.T) {
	schemas := []struct {
		name, schema string
	}{
		{name: "inherits", schema: "CREATE TABLE parent (inherited_id bigint); CREATE TABLE child (local_id bigint) INHERITS (parent);"},
		{name: "like", schema: "CREATE TABLE parent (copied_id bigint); CREATE TABLE child (LIKE parent INCLUDING ALL, local_id bigint);"},
		{name: "typed table", schema: "CREATE TYPE pair AS (a int, b int); CREATE TABLE child OF pair (a WITH OPTIONS NOT NULL);"},
	}
	forms := []struct {
		name, query, diagnostic string
	}{
		{name: "wildcard", query: "SELECT * FROM child;", diagnostic: "wildcard column usage is unresolved"},
		{name: "embed", query: "SELECT sqlc.embed(c) FROM child AS c;", diagnostic: "column expansion lacks complete, unique configured schema evidence"},
	}
	for _, schema := range schemas {
		for _, form := range forms {
			t.Run(schema.name+"/"+form.name, func(t *testing.T) {
				root := testtemp.Dir(t)
				writeRouterFiles(t, root, map[string]string{
					"sqlc.yaml": `version: "2"
sql:
  - engine: postgresql
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
					"schema.sql":  schema.schema,
					"queries.sql": "-- name: ListRows :many\n" + form.query,
				})
				content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
				if err != nil {
					t.Fatal(err)
				}
				result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
					Root: root, Path: "queries.sql", Content: content, SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if !hasDiagnostic(result, form.diagnostic) {
					t.Fatalf("diagnostics = %#v", result.Diagnostics)
				}
				for _, fact := range result.Facts {
					if fact.TargetKind == graph.KindColumn && fact.Producer == "sqlcbinding" {
						t.Fatalf("copied schema produced %s column evidence: %#v", form.name, fact)
					}
				}
			})
		}
	}
}

func TestRouterDoesNotGuessPostgreSQLTableSchema(t *testing.T) {
	schemas := []struct {
		name, schema, table string
	}{
		{name: "qualified query", schema: "CREATE TABLE users (id bigint PRIMARY KEY, name text);", table: "audit.users"},
		{name: "qualified declaration", schema: "CREATE TABLE audit.users (id bigint PRIMARY KEY, name text);", table: "users"},
		{name: "quoted table case", schema: "CREATE TABLE users (id bigint PRIMARY KEY, name text);", table: `"Users"`},
		{name: "quoted schema case", schema: "CREATE TABLE public.users (id bigint PRIMARY KEY, name text);", table: `"Public".users`},
	}
	forms := []struct {
		name, queryFormat, diagnostic string
	}{
		{name: "wildcard", queryFormat: "SELECT * FROM %s;", diagnostic: "wildcard column usage is unresolved"},
		{name: "embed", queryFormat: "SELECT sqlc.embed(u) FROM %s AS u;", diagnostic: "column expansion lacks complete, unique configured schema evidence"},
	}
	for _, schema := range schemas {
		for _, form := range forms {
			t.Run(schema.name+"/"+form.name, func(t *testing.T) {
				root := testtemp.Dir(t)
				writeRouterFiles(t, root, map[string]string{
					"sqlc.yaml": `version: "2"
sql:
  - engine: postgresql
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
					"schema.sql":  schema.schema,
					"queries.sql": "-- name: ListRows :many\n" + fmt.Sprintf(form.queryFormat, schema.table),
				})
				content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
				if err != nil {
					t.Fatal(err)
				}
				result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
					Root: root, Path: "queries.sql", Content: content, SourcePaths: []string{"sqlc.yaml", "schema.sql", "queries.sql"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if !hasDiagnostic(result, form.diagnostic) {
					t.Fatalf("diagnostics = %#v", result.Diagnostics)
				}
				for _, fact := range result.Facts {
					if fact.TargetKind == graph.KindColumn && fact.Producer == "sqlcbinding" {
						t.Fatalf("qualified table produced guessed %s column evidence: %#v", form.name, fact)
					}
				}
			})
		}
	}
}

func TestRouterDoesNotUseSqlcSchemaAcrossDialectMismatch(t *testing.T) {
	root := testtemp.Dir(t)
	writeRouterFiles(t, root, map[string]string{
		"grafo.yaml": "sql:\n  default_dialect: postgres\n",
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);",
		"queries.sql": "-- name: ListUsers :many\nSELECT sqlc.embed(u) FROM users AS u;",
	})
	content, err := os.ReadFile(filepath.Join(root, "queries.sql"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content,
		SourcePaths: []string{"grafo.yaml", "sqlc.yaml", "schema.sql", "queries.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(result, "bindings disabled") || !hasDiagnostic(result, "column expansion lacks") {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	for _, fact := range result.Facts {
		if fact.TargetKind == graph.KindColumn && fact.Producer == "sqlcbinding" {
			t.Fatalf("dialect mismatch produced sqlc schema evidence: %#v", fact)
		}
	}
}

func TestRouterKeepsUnresolvedEmbedExplicitWithoutConfiguration(t *testing.T) {
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "grafo.yaml"), []byte("sql:\n  default_dialect: sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := []byte("-- name: GetUser :one\nSELECT sqlc.embed(u) FROM users AS u;")
	result, err := sqlparser.New(postgresparser.New(), sqliteparser.New()).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "queries.sql", Content: content, SourcePaths: []string{"grafo.yaml", "queries.sql"}, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(result, "column expansion lacks complete, unique configured schema evidence") {
		t.Fatalf("missing explicit unresolved embed diagnostic: %#v", result.Diagnostics)
	}
}

func TestCortexSqlcQueryShapesParseInSQLiteAndPostgreSQL(t *testing.T) {
	queries := map[string]string{
		"agents": `-- name: InsertAgent :exec
INSERT INTO agents (id, api_url) VALUES (sqlc.arg(id), sqlc.narg(api_url));`,
		"bank": `-- name: UpsertBankKnowledge :one
INSERT INTO bank_knowledge (agent_id, gold_balance)
VALUES (sqlc.arg(agent_id), sqlc.arg(gold_balance))
ON CONFLICT (agent_id) DO UPDATE SET gold_balance = excluded.gold_balance
RETURNING *;`,
		"goals": `-- name: UpdateGoal :one
UPDATE goals SET status = sqlc.arg(status), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;`,
		"knowledge": `-- name: ListMarkedEncounters :many
SELECT encounter_id FROM encounter_reflections
WHERE agent_id = sqlc.arg(agent_id)
AND encounter_id IN (sqlc.slice(encounter_ids));`,
		"memories": `-- name: PruneMemories :execrows
DELETE FROM memories
WHERE memories.agent_id = sqlc.arg(agent_id)
AND memories.id NOT IN (
  SELECT ranked.id FROM memories AS ranked
  WHERE ranked.agent_id = sqlc.arg(agent_id)
  ORDER BY ranked.id DESC LIMIT sqlc.arg(row_keep)
);`,
		"relationships": `-- name: AdjustReputation :one
INSERT INTO relationships (agent_id, player_name, reputation)
VALUES (sqlc.arg(agent_id), sqlc.arg(player_name), sqlc.arg(delta))
ON CONFLICT (agent_id, player_name) DO UPDATE
SET reputation = relationships.reputation + excluded.reputation
RETURNING *;`,
	}
	type dialectParser interface {
		Parse(context.Context, parserapi.Input) (graph.ParseResult, error)
	}
	dialects := map[string]dialectParser{
		"sqlite":     sqliteparser.New(),
		"postgresql": postgresparser.New(),
	}
	for dialect, dialectParser := range dialects {
		for name, query := range queries {
			t.Run(dialect+"/"+name, func(t *testing.T) {
				result, err := dialectParser.Parse(context.Background(), parserapi.Input{
					Path: name + ".sql", Content: []byte(query), RepoID: "repo:cortex",
				})
				if err != nil {
					t.Fatalf("parse failed: %v", err)
				}
				for _, diagnostic := range result.Diagnostics {
					if diagnostic.Level == "error" || strings.Contains(strings.ToLower(diagnostic.Message), "parse") {
						t.Fatalf("unexpected diagnostic: %#v", diagnostic)
					}
				}
				queryCount := 0
				for _, node := range result.Nodes {
					if node.Kind == graph.KindSQLQuery {
						queryCount++
					}
				}
				if queryCount != 1 {
					t.Fatalf("SQL query nodes = %d, want 1", queryCount)
				}
			})
		}
	}
}

func writeRouterFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func hasDiagnostic(result graph.ParseResult, text string) bool {
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, text) {
			return true
		}
	}
	return false
}

func declaredTable(result graph.ParseResult) string {
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTable {
			return node.QualifiedName
		}
	}
	return ""
}
