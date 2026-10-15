package sqlcbinding_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/sqlcbinding"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestLoaderDiscoversNestedV2YAMLAndJSONEntries(t *testing.T) {
	tests := []struct {
		name, config, content string
	}{
		{name: "yaml scalar paths", config: "services/api/sqlc.yaml", content: `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen:
      go:
        package: database
        out: generated
`},
		{name: "yml list paths", config: "services/api/sqlc.yml", content: `version: "2"
sql:
  - engine: postgresql
    schema: [schema/base.sql]
    queries: [queries]
    gen:
      go: {package: database, out: generated}
`},
		{name: "json", config: "services/api/sqlc.json", content: `{"version":"2","sql":[{"engine":"sqlite","schema":"schema.sql","queries":"queries.sql","gen":{"go":{"package":"database","out":"generated"}}}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := testtemp.Dir(t)
			files := map[string]string{
				test.config:                      test.content,
				"services/api/schema.sql":        "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);",
				"services/api/schema/base.sql":   "CREATE TABLE users (id bigint PRIMARY KEY, name text);",
				"services/api/queries.sql":       "-- name: GetUser :one\nSELECT id, name FROM users WHERE id = sqlc.arg(id);",
				"services/api/queries/users.sql": "-- name: GetUser :one\nSELECT id, name FROM users WHERE id = sqlc.arg(id);",
			}
			paths := writeFiles(t, root, files)
			registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{
				Root: root, SourcePaths: paths, RepoID: "repo:sample", GoModule: "example.com/sample",
			})
			if err != nil {
				t.Fatal(err)
			}
			config, ok := registry.Config(test.config)
			if !ok || hasUnexpectedDiagnostic(config, "has no indexed generated query files") {
				t.Fatalf("config = %#v, ok = %v", config, ok)
			}
			queryPath := "services/api/queries.sql"
			if strings.Contains(test.content, "queries]") {
				queryPath = "services/api/queries/users.sql"
			}
			entries := registry.EntriesForQuery(queryPath)
			if len(entries) != 1 {
				t.Fatalf("entries for %s = %#v", queryPath, entries)
			}
			wantEngine := "sqlite"
			if strings.Contains(test.content, "postgresql") {
				wantEngine = "postgres"
			}
			if entries[0].Engine != wantEngine || entries[0].Go.Package != "database" || entries[0].Go.Out != "services/api/generated" {
				t.Fatalf("entry = %#v", entries[0])
			}
			if query, ok := registry.Query(queryPath, "GetUser"); !ok || query.Command != ":one" {
				t.Fatalf("query = %#v, ok = %v", query, ok)
			}
		})
	}
}

func TestSemanticInputOnlyIncludesSqlcConfiguration(t *testing.T) {
	for _, path := range []string{"sqlc.yaml", "nested/sqlc.yml", "sqlc.json"} {
		if !sqlcbinding.IsSemanticInput(path) {
			t.Fatalf("configuration %q is not a semantic input", path)
		}
	}
	for _, path := range []string{"dump.sql", "queries.psql", "generated.go", "go.mod"} {
		if sqlcbinding.IsSemanticInput(path) {
			t.Fatalf("ordinary binding input %q was classified as a hidden semantic input", path)
		}
		if !sqlcbinding.IsBindingInput(path) {
			t.Fatalf("ordinary binding input %q was excluded from invalidation", path)
		}
	}
}

func TestLoaderMatchesSqlcDirectoryMembership(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema
    queries: queries
    gen: {go: {package: db, out: generated}}
`,
		"schema/base.sql":         "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"schema/nested/more.sql":  "CREATE TABLE ignored (id INTEGER);",
		"queries/users.sql":       "-- name: GetUser :one\nSELECT id FROM users;",
		"queries/.hidden.sql":     "-- name: Hidden :one\nSELECT id FROM users;",
		"queries/undo.down.sql":   "-- name: Down :one\nSELECT id FROM users;",
		"queries/other.psql":      "-- name: Other :one\nSELECT id FROM users;",
		"queries/nested/deep.sql": "-- name: Deep :one\nSELECT id FROM users;",
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if entries := registry.EntriesForQuery("queries/users.sql"); len(entries) != 1 {
		t.Fatalf("direct .sql entry = %#v", entries)
	}
	for _, ignored := range []string{"queries/.hidden.sql", "queries/undo.down.sql", "queries/other.psql", "queries/nested/deep.sql"} {
		if entries := registry.EntriesForQuery(ignored); len(entries) != 0 {
			t.Fatalf("sqlc-ineligible %s was included: %#v", ignored, entries)
		}
	}
}

func TestLoaderRejectsIncompletePostgreSQLCopiedColumnSets(t *testing.T) {
	tests := []struct {
		name, schema string
	}{
		{name: "inherits", schema: "CREATE TABLE parent (inherited_id bigint); CREATE TABLE child (local_id bigint) INHERITS (parent);"},
		{name: "like", schema: "CREATE TABLE parent (copied_id bigint); CREATE TABLE child (LIKE parent INCLUDING ALL, local_id bigint);"},
		{name: "typed table", schema: "CREATE TYPE pair AS (a int, b int); CREATE TABLE child OF pair (a WITH OPTIONS NOT NULL);"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := testtemp.Dir(t)
			paths := writeFiles(t, root, map[string]string{
				"sqlc.yaml": `version: "2"
sql:
  - engine: postgresql
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: generated}}
`,
				"schema.sql":  test.schema,
				"queries.sql": "-- name: ListChildren :many\nSELECT * FROM child;",
			})
			registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
			if err != nil {
				t.Fatal(err)
			}
			if _, columns, ok := registry.SchemaColumnsForQuery("queries.sql", "child"); ok {
				t.Fatalf("incomplete column set was accepted: %#v", columns)
			}
		})
	}
}

func TestLoaderRejectsDuplicateTableDeclarationsAcrossSchemaFiles(t *testing.T) {
	tests := []struct {
		name, engine, duplicate string
	}{
		{name: "postgres create table as", engine: "postgresql", duplicate: "CREATE TABLE users AS SELECT 1 AS id;"},
		{name: "sqlite create table as", engine: "sqlite", duplicate: "CREATE TABLE users AS SELECT 1 AS id;"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := testtemp.Dir(t)
			paths := writeFiles(t, root, map[string]string{
				"sqlc.yaml": `version: "2"
sql:
  - engine: ` + test.engine + `
    schema: [base.sql, duplicate.sql]
    queries: queries.sql
    gen: {go: {package: db, out: generated}}
`,
				"base.sql":      "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);",
				"duplicate.sql": test.duplicate,
				"queries.sql":   "-- name: ListUsers :many\nSELECT * FROM users;",
			})
			registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
			if err != nil {
				t.Fatal(err)
			}
			if _, columns, ok := registry.SchemaColumnsForQuery("queries.sql", "users"); ok {
				t.Fatalf("duplicate table retained column evidence: %#v", columns)
			}
		})
	}
}

func TestLoaderScopesPostgreSQLSchemaMutationEvidence(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: postgresql
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: generated}}
`,
		"schema.sql": `
CREATE TABLE audit (id bigint PRIMARY KEY);
CREATE TABLE audit.users (id bigint PRIMARY KEY);
DROP SCHEMA audit CASCADE;
`,
		"queries.sql": "-- name: ListAudit :many\nSELECT * FROM audit;",
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if _, columns, ok := registry.SchemaColumnsForQuery("queries.sql", "audit"); !ok || len(columns) != 1 || columns[0] != "id" {
		t.Fatalf("same-named table was invalidated: columns %#v, ok %v", columns, ok)
	}
	if _, columns, ok := registry.SchemaColumnsForQuery("queries.sql", "audit.users"); ok {
		t.Fatalf("dropped schema retained column evidence: %#v", columns)
	}
}

func TestLoaderScopesPostgreSQLTableMutationEvidence(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: postgresql
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: generated}}
`,
		"schema.sql": `
CREATE TABLE public.users (id bigint PRIMARY KEY);
CREATE TABLE audit.users (id bigint PRIMARY KEY);
ALTER TABLE audit.users ADD COLUMN email text;
`,
		"queries.sql": "-- name: ListUsers :many\nSELECT * FROM public.users;",
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if _, columns, ok := registry.SchemaColumnsForQuery("queries.sql", "public.users"); !ok || len(columns) != 1 || columns[0] != "id" {
		t.Fatalf("unmutated table was invalidated: columns %#v, ok %v", columns, ok)
	}
	if _, columns, ok := registry.SchemaColumnsForQuery("queries.sql", "audit.users"); ok {
		t.Fatalf("mutated table retained column evidence: %#v", columns)
	}
}

func TestLoaderRejectsDuplicateNamesWithinOneEntry(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries
    gen: {go: {package: db, out: generated}}
`,
		"schema.sql":    "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries/a.sql": "-- name: Same :one\nSELECT id FROM users;",
		"queries/b.sql": "-- name: Same :many\nSELECT id FROM users;",
		"generated/a.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: a.sql
package db
const same = ` + "`" + `SELECT id FROM users` + "`" + `
type Queries struct{}; func (q *Queries) Same() { q.row(same) }
`,
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.BindingsForQuery("queries/a.sql", "Same"); len(got) != 0 {
		t.Fatalf("duplicate query produced a binding: %#v", got)
	}
	config, _ := registry.Config("sqlc.yaml")
	if !strings.Contains(diagnostics(config), "duplicated") {
		t.Fatalf("diagnostics = %q", diagnostics(config))
	}
}

func TestLoaderScopesDuplicateNamesToOwningEntry(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries/shared.sql
    gen: {go: {package: db, out: gen1}}
`,
		"other/sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: ../schema.sql
    queries: [../queries/shared.sql, ../queries/duplicate.sql]
    gen: {go: {package: db, out: gen2}}
`,
		"schema.sql":            "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries/shared.sql":    "-- name: Same :one\nSELECT id FROM users;",
		"queries/duplicate.sql": "-- name: Same :many\nSELECT id FROM users;",
		"gen1/shared.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: shared.sql
package db
const same = ` + "`" + `SELECT id FROM users` + "`" + `
type Queries struct{}; func (q *Queries) Same() { q.row(same) }
`,
		"other/gen2/shared.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: shared.sql
package db
const same = ` + "`" + `SELECT id FROM users` + "`" + `
type Queries struct{}; func (q *Queries) Same() { q.row(same) }
`,
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths, GoModule: "example.com/sample"})
	if err != nil {
		t.Fatal(err)
	}
	bindings := registry.BindingsForQuery("queries/shared.sql", "Same")
	if len(bindings) != 1 || bindings[0].ConfigPath != "sqlc.yaml" {
		t.Fatalf("entry-scoped duplicate bindings = %#v; diagnostics = %s", bindings, diagnosticsForRegistry(registry))
	}
}

func TestLoaderRejectsDuplicateNamesWithinOneQueryFile(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: generated}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries.sql": "-- name: Same :one\nSELECT id FROM users;\n-- name: Same :many\nSELECT id FROM users;",
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Query("queries.sql", "Same"); ok {
		t.Fatal("duplicate query in one file remained binding-eligible")
	}
	config, _ := registry.Config("sqlc.yaml")
	if !strings.Contains(diagnostics(config), "duplicate sqlc query name") {
		t.Fatalf("diagnostics = %q", diagnostics(config))
	}
}

func TestLoaderRejectsUnsupportedAndEscapingConfiguration(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "1"
sql:
  - engine: mysql
    schema: /outside/schema.sql
    queries: ../outside.sql
    gen:
      go: {package: db, out: ../generated}
`,
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	config, ok := registry.Config("sqlc.yaml")
	if !ok || len(config.Diagnostics) == 0 {
		t.Fatalf("expected rejected config diagnostics, got %#v", config)
	}
	joined := diagnostics(config)
	if !strings.Contains(joined, "version 2") {
		t.Fatalf("diagnostics = %q", joined)
	}
	if len(registry.EntriesForQuery("outside.sql")) != 0 {
		t.Fatal("escaping configuration produced an entry")
	}
}

func TestLoaderBindsCorroboratedGeneratedGoMethod(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen:
      go:
        package: db
        out: internal/db
        emit_prepared_queries: true
        emit_exported_queries: true
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);",
		"queries.sql": "-- name: GetUser :one\nSELECT id, name FROM users WHERE id = sqlc.arg(id);",
		"internal/db/queries.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.31.1
// source: queries.sql

package db

import "context"

const GetUserSQL = ` + "`" + `SELECT id, name FROM users WHERE id = ?` + "`" + `

type Queries struct{}
func (q *Queries) GetUser(ctx context.Context, id int64) { q.queryRow(ctx, GetUserSQL, id) }
func (q *Queries) queryRow(context.Context, string, ...any) {}
`,
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{
		Root: root, SourcePaths: paths, RepoID: "repo:sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings := registry.BindingsForQuery("queries.sql", "GetUser")
	if len(bindings) != 1 {
		t.Fatalf("bindings = %#v; diagnostics = %s", bindings, diagnosticsForRegistry(registry))
	}
	binding := bindings[0]
	if binding.Method != "example.com/sample/internal/db.Queries.GetUser" || binding.GeneratedPath != "internal/db/queries.sql.go" || binding.Constant != "GetUserSQL" {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestLoaderBindsPostgreSQLPgxGeneratedMethod(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: postgresql
    schema: schema.sql
    queries: queries.sql
    gen:
      go:
        package: pgdb
        out: internal/pgdb
        sql_package: pgx/v5
        emit_prepared_queries: true
`,
		"schema.sql":  "CREATE TABLE users (id bigint PRIMARY KEY, name text);",
		"queries.sql": "-- name: GetUser :one\nSELECT id, name FROM users WHERE id = sqlc.arg(id);",
		"internal/pgdb/queries.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.31.1
// source: queries.sql
package pgdb
import "context"
const getUser = ` + "`" + `SELECT id, name FROM users WHERE id = $1` + "`" + `
type Queries struct{}
func (q *Queries) GetUser(ctx context.Context, id int64) { q.queryRow(ctx, getUser, id) }
`,
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{
		Root: root, SourcePaths: paths, GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings := registry.BindingsForQuery("queries.sql", "GetUser")
	if len(bindings) != 1 || bindings[0].Method != "example.com/sample/internal/pgdb.Queries.GetUser" {
		t.Fatalf("bindings = %#v; diagnostics = %s", bindings, diagnosticsForRegistry(registry))
	}
}

func TestLoaderBindsGeneratedGoAtRepositoryRoot(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: .}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries.sql": "-- name: GetUser :one\nSELECT id FROM users;",
		"queries.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT id FROM users` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`,
	})
	loader := sqlcbinding.NewLoader()
	registry, err := loader.Load(context.Background(), parserapi.Input{
		Root: root, SourcePaths: paths, GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bindings := registry.BindingsForQuery("queries.sql", "GetUser"); len(bindings) != 1 {
		t.Fatalf("root output bindings = %#v; diagnostics = %s", bindings, diagnosticsForRegistry(registry))
	}
	queries, configs, known := loader.AffectedPaths(root, []string{"queries.sql.go"})
	if !known || !containsString(queries, "queries.sql") || !containsString(configs, "sqlc.yaml") {
		t.Fatalf("root output affected paths = queries %#v configs %#v known %v", queries, configs, known)
	}
}

func TestLoaderUsesDirectoryQualifiedMethodWithoutGoModule(t *testing.T) {
	root := testtemp.Dir(t)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries.sql": "-- name: GetUser :one\nSELECT id FROM users;",
		"internal/db/queries.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT id FROM users` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`,
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	bindings := registry.BindingsForQuery("queries.sql", "GetUser")
	if len(bindings) != 1 || bindings[0].Method != "internal/db.Queries.GetUser" {
		t.Fatalf("bindings = %#v", bindings)
	}
}

func TestLoaderModuleIdentityAndUnownedSQLConverge(t *testing.T) {
	root := testtemp.Dir(t)
	files := map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql":  "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		"queries.sql": "-- name: GetUser :one\nSELECT id FROM users;",
		"other.sql":   "SELECT 1;",
		"internal/db/queries.sql.go": `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT id FROM users` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`,
	}
	paths := writeFiles(t, root, files)
	loader := sqlcbinding.NewLoader()
	load := func(module string) sqlcbinding.Registry {
		registry, err := loader.Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths, GoModule: module})
		if err != nil {
			t.Fatal(err)
		}
		return registry
	}
	first := load("example.com/old")
	semanticKey := func(path, module string) string {
		t.Helper()
		key, err := loader.FileSemanticKey(context.Background(), parserapi.Input{Root: root, Path: path, SourcePaths: paths, GoModule: module})
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	firstOwnedKey := semanticKey("queries.sql", "example.com/old")
	firstUnownedKey := semanticKey("other.sql", "example.com/old")
	if err := os.WriteFile(filepath.Join(root, "other.sql"), []byte("SELECT 2;"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := load("example.com/old")
	if first.Digest != second.Digest {
		t.Fatal("unowned SQL changed the sqlc registry digest")
	}
	if firstOwnedKey != semanticKey("queries.sql", "example.com/old") || firstUnownedKey != semanticKey("other.sql", "example.com/old") {
		t.Fatal("unowned SQL changed a sqlc file semantic key")
	}
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), []byte("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if firstOwnedKey == semanticKey("queries.sql", "example.com/old") {
		t.Fatal("owned schema change did not change query semantic key")
	}
	if firstUnownedKey != semanticKey("other.sql", "example.com/old") {
		t.Fatal("owned schema change changed unowned SQL semantic key")
	}
	third := load("example.com/new")
	if first.Digest == third.Digest {
		t.Fatal("Go module identity did not change the registry digest")
	}
	bindings := third.BindingsForQuery("queries.sql", "GetUser")
	if len(bindings) != 1 || bindings[0].Method != "example.com/new/internal/db.Queries.GetUser" {
		t.Fatalf("bindings = %#v", bindings)
	}
}

func TestLoaderRejectsOversizedSemanticInput(t *testing.T) {
	root := testtemp.Dir(t)
	generated := `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT id FROM users` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
` + strings.Repeat("// padding\n", 100)
	paths := writeFiles(t, root, map[string]string{
		"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
		"schema.sql": "CREATE TABLE users (id INTEGER PRIMARY KEY);", "queries.sql": "-- name: GetUser :one\nSELECT id FROM users;",
		"internal/db/queries.sql.go": generated,
	})
	registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths, MaxFileSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.BindingsForQuery("queries.sql", "GetUser"); len(got) != 0 {
		t.Fatalf("oversized evidence produced bindings: %#v", got)
	}
	if !strings.Contains(diagnosticsForRegistry(registry), "no indexed generated query files") {
		t.Fatalf("oversized evidence was not diagnosed: %s", diagnosticsForRegistry(registry))
	}
}

func TestLoaderDropsAndRestoresBindingsAsEvidenceChanges(t *testing.T) {
	root := testtemp.Dir(t)
	files := map[string]string{
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
	}
	paths := writeFiles(t, root, files)
	loader := sqlcbinding.NewLoader()
	load := func(sourcePaths []string) sqlcbinding.Registry {
		t.Helper()
		registry, err := loader.Load(context.Background(), parserapi.Input{Root: root, SourcePaths: sourcePaths, GoModule: "example.com/sample"})
		if err != nil {
			t.Fatal(err)
		}
		return registry
	}
	if got := load(paths).BindingsForQuery("queries.sql", "GetUser"); len(got) != 1 {
		t.Fatalf("initial bindings = %#v", got)
	}
	generatedPath := filepath.Join(root, "internal/db/queries.sql.go")
	if err := os.WriteFile(generatedPath, []byte(strings.ReplaceAll(files["internal/db/queries.sql.go"], "SELECT id", "SELECT missing")), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := load(paths).BindingsForQuery("queries.sql", "GetUser"); len(got) != 0 {
		t.Fatalf("stale generated edit retained bindings: %#v", got)
	}
	if err := os.Remove(filepath.Join(root, "sqlc.yaml")); err != nil {
		t.Fatal(err)
	}
	withoutConfig := make([]string, 0, len(paths)-1)
	for _, path := range paths {
		if path != "sqlc.yaml" {
			withoutConfig = append(withoutConfig, path)
		}
	}
	if got := load(withoutConfig).BindingsForQuery("queries.sql", "GetUser"); len(got) != 0 {
		t.Fatalf("removed config retained bindings: %#v", got)
	}
	if err := os.WriteFile(filepath.Join(root, "sqlc.yaml"), []byte(files["sqlc.yaml"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generatedPath, []byte(files["internal/db/queries.sql.go"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := load(paths).BindingsForQuery("queries.sql", "GetUser"); len(got) != 1 {
		t.Fatalf("restored evidence did not restore binding: %#v", got)
	}
}

func TestLoaderRejectsStaleForgedAndUnusedGeneratedEvidence(t *testing.T) {
	tests := []struct {
		name, generated string
	}{
		{name: "missing header", generated: `package db
const getUser = ` + "`" + `SELECT id FROM users WHERE id = ?` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`},
		{name: "header text in body", generated: `// handwritten file
package db
const fakeHeader = "// Code generated by sqlc. DO NOT EDIT."
// source: queries.sql
const getUser = ` + "`" + `SELECT id FROM users WHERE id = ?` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`},
		{name: "stale sql", generated: `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT name FROM users WHERE id = ?` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`},
		{name: "unused constant", generated: `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package db
const getUser = ` + "`" + `SELECT id FROM users WHERE id = ?` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row("different") }
`},
		{name: "wrong package", generated: `// Code generated by sqlc. DO NOT EDIT.
// source: queries.sql
package wrong
const getUser = ` + "`" + `SELECT id FROM users WHERE id = ?` + "`" + `
type Queries struct{}; func (q *Queries) GetUser() { q.row(getUser) }
`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := testtemp.Dir(t)
			paths := writeFiles(t, root, map[string]string{
				"sqlc.yaml": `version: "2"
sql:
  - engine: sqlite
    schema: schema.sql
    queries: queries.sql
    gen: {go: {package: db, out: internal/db}}
`,
				"schema.sql":                 "CREATE TABLE users (id INTEGER PRIMARY KEY);",
				"queries.sql":                "-- name: GetUser :one\nSELECT id FROM users WHERE id = sqlc.arg(id);",
				"internal/db/queries.sql.go": test.generated,
			})
			registry, err := sqlcbinding.NewLoader().Load(context.Background(), parserapi.Input{Root: root, SourcePaths: paths, GoModule: "example.com/sample"})
			if err != nil {
				t.Fatal(err)
			}
			if got := registry.BindingsForQuery("queries.sql", "GetUser"); len(got) != 0 {
				t.Fatalf("rejected evidence produced bindings: %#v", got)
			}
			if diagnosticsForRegistry(registry) == "" {
				t.Fatal("rejected evidence was not explicit")
			}
		})
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) []string {
	t.Helper()
	paths := make([]string, 0, len(files))
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func diagnostics(config sqlcbinding.Config) string {
	var messages []string
	for _, diagnostic := range config.Diagnostics {
		messages = append(messages, diagnostic.Message)
	}
	return strings.Join(messages, "\n")
}

func diagnosticsForRegistry(registry sqlcbinding.Registry) string {
	var messages []string
	for _, config := range registry.Configs() {
		messages = append(messages, diagnostics(config))
	}
	return strings.Join(messages, "\n")
}

func hasUnexpectedDiagnostic(config sqlcbinding.Config, allowed string) bool {
	for _, diagnostic := range config.Diagnostics {
		if !strings.Contains(diagnostic.Message, allowed) {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
