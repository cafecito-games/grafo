package sqltemplate_test

import (
	"bytes"
	"testing"

	"github.com/cafecito-games/grafo/internal/parser/sqltemplate"
)

func TestAnalyzeNormalizesSupportedMacrosWithoutMovingOffsets(t *testing.T) {
	source := []byte("-- name: GetUser :one\r\n" +
		"SELECT sqlc.embed(u), u.name FROM users AS u\n" +
		"WHERE u.id = sqlc.arg('id') AND u.nick = sqlc.narg(\"nick\")\n" +
		"AND u.role_id IN (sqlc.slice(roles)) AND u.owner = @owner;\n")

	for _, dialect := range []string{"sqlite", "postgres"} {
		document := sqltemplate.Analyze(source, dialect)
		if len(document.Diagnostics) != 0 {
			t.Fatalf("%s diagnostics: %#v", dialect, document.Diagnostics)
		}
		if len(document.SQL) != len(source) {
			t.Fatalf("%s normalized length = %d, want %d", dialect, len(document.SQL), len(source))
		}
		if len(document.Queries) != 1 {
			t.Fatalf("%s queries = %#v", dialect, document.Queries)
		}
		query := document.Queries[0]
		if query.Name != "GetUser" || query.Command != ":one" {
			t.Fatalf("%s query = %#v", dialect, query)
		}
		if got := string(source[query.StatementStart:query.StatementEnd]); got != string(bytes.TrimSpace(source[bytes.Index(source, []byte("SELECT")):])) {
			t.Fatalf("%s original statement span changed: %q", dialect, got)
		}
		if !bytes.Contains(document.SQL, []byte("u.*")) {
			t.Fatalf("%s embed was not normalized: %q", dialect, document.SQL)
		}
		if dialect == "sqlite" && bytes.Contains(document.SQL, []byte("sqlc.")) {
			t.Fatalf("SQLite macros remain: %q", document.SQL)
		}
		if dialect == "postgres" && !bytes.Contains(document.SQL, []byte("$1")) {
			t.Fatalf("PostgreSQL placeholder missing: %q", document.SQL)
		}
		for _, macro := range document.Macros {
			if got := string(source[macro.Start:macro.End]); got != macro.Original {
				t.Fatalf("%s macro span %q != %q", dialect, got, macro.Original)
			}
		}
	}
}

func TestAnalyzeIgnoresMacroLookingTextOutsideSQLCode(t *testing.T) {
	source := []byte("-- sqlc.arg(commented)\n" +
		"SELECT 'sqlc.narg(literal)', \"sqlc.slice(identifier)\", $$sqlc.embed(dollar)$$, col\n" +
		"FROM things /* sqlc.arg(blocked) */ WHERE id = sqlc.arg(id);")
	document := sqltemplate.Analyze(source, "postgres")
	if len(document.Diagnostics) != 0 {
		t.Fatalf("diagnostics: %#v", document.Diagnostics)
	}
	if len(document.Macros) != 1 || document.Macros[0].Name != "arg" {
		t.Fatalf("macros = %#v", document.Macros)
	}
	for _, untouched := range [][]byte{
		[]byte("sqlc.arg(commented)"), []byte("sqlc.narg(literal)"),
		[]byte("sqlc.slice(identifier)"), []byte("sqlc.embed(dollar)"), []byte("sqlc.arg(blocked)"),
	} {
		if !bytes.Contains(document.SQL, untouched) {
			t.Fatalf("non-code text %q changed in %q", untouched, document.SQL)
		}
	}
}

func TestAnalyzeFindsOnlyProjectionWildcards(t *testing.T) {
	source := []byte("SELECT *, u.*, DISTINCT *, a, *, \"q\".*, public.users.*, price * quantity, count(*), 'literal *' FROM users AS u; -- SELECT *\n")
	document := sqltemplate.Analyze(source, "sqlite")
	if len(document.Wildcards) != 6 {
		t.Fatalf("wildcards = %#v", document.Wildcards)
	}
	qualifiers := []string{"", "u", "", "", "q", "public.users"}
	for index, qualifier := range qualifiers {
		if document.Wildcards[index].Qualifier != qualifier {
			t.Fatalf("wildcards = %#v", document.Wildcards)
		}
	}
	if document.HasFatalDiagnostics() {
		t.Fatalf("wildcards = %#v", document.Wildcards)
	}
}

func TestAnalyzeUsesDialectSpecificQuotingAndOperators(t *testing.T) {
	postgres := []byte("-- display name: array probe\nSELECT tags <@ ARRAY[sqlc.arg(tag), @other], values[sqlc.arg(idx)];")
	document := sqltemplate.Analyze(postgres, "postgres")
	if len(document.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", document.Diagnostics)
	}
	if len(document.Macros) != 3 {
		t.Fatalf("macros = %#v", document.Macros)
	}
	if document.Macros[0].Argument != "tag" || document.Macros[1].Argument != "other" || document.Macros[2].Argument != "idx" {
		t.Fatalf("macros = %#v", document.Macros)
	}
	if bytes.Contains(document.SQL, []byte("sqlc.arg")) || bytes.Contains(document.SQL, []byte("@other")) {
		t.Fatalf("PostgreSQL array macros were not normalized: %q", document.SQL)
	}
}

func TestAnalyzeHandlesPostgreSQLEscapeStringsAndSQLiteComments(t *testing.T) {
	postgres := []byte("SELECT E'it\\'s sqlc.arg(fake)';\n-- name: Real :one\nSELECT sqlc.arg(id);")
	document := sqltemplate.Analyze(postgres, "postgres")
	if len(document.Diagnostics) != 0 || len(document.Macros) != 1 || len(document.Queries) != 1 {
		t.Fatalf("document = %#v", document)
	}

	sqlite := sqltemplate.Analyze([]byte("/* outer /* old */ SELECT sqlc.arg(id);"), "sqlite")
	if sqlite.HasFatalDiagnostics() || len(sqlite.Macros) != 1 {
		t.Fatalf("SQLite comment handling = %#v", sqlite)
	}

	unterminated := sqltemplate.Analyze([]byte("SELECT 'missing"), "sqlite")
	if !unterminated.HasFatalDiagnostics() {
		t.Fatalf("unterminated quote was not fatal: %#v", unterminated.Diagnostics)
	}
}

func TestAnalyzeAssociatesSeparateNamedQueriesAndLeavesUnannotatedSQL(t *testing.T) {
	source := []byte("-- name: First :one\nSELECT id FROM alpha WHERE id = @id;\n" +
		"SELECT id FROM ordinary;\n" +
		"/* name: Second :execrows */\nUPDATE beta SET name = sqlc.arg(name) WHERE id = sqlc.arg(id);\n")
	document := sqltemplate.Analyze(source, "sqlite")
	if len(document.Diagnostics) != 0 {
		t.Fatalf("diagnostics: %#v", document.Diagnostics)
	}
	if len(document.Queries) != 2 {
		t.Fatalf("queries = %#v", document.Queries)
	}
	if document.Queries[0].Name != "First" || document.Queries[1].Name != "Second" {
		t.Fatalf("queries = %#v", document.Queries)
	}
	if bytes.Contains(source[document.Queries[0].StatementStart:document.Queries[0].StatementEnd], []byte("ordinary")) {
		t.Fatal("first query consumed the unannotated statement")
	}
	if !bytes.Contains(source[document.Queries[1].StatementStart:document.Queries[1].StatementEnd], []byte("UPDATE beta")) {
		t.Fatal("second query was not associated with UPDATE")
	}
}

func TestAnalyzeDiagnosesMalformedAndAmbiguousEvidence(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{name: "unknown macro", source: "SELECT sqlc.unknown(id);"},
		{name: "unterminated macro", source: "SELECT sqlc.arg(id;"},
		{name: "empty macro argument", source: "SELECT sqlc.arg();"},
		{name: "unsupported command", source: "-- name: Bad :wat\nSELECT 1;"},
		{name: "duplicate annotations", source: "-- name: One :one\n-- name: Two :one\nSELECT 1;"},
		{name: "annotation without statement", source: "-- name: Missing :one\n"},
		{name: "annotation inside statement", source: "SELECT 1 -- name: Wrong :one\n;"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := sqltemplate.Analyze([]byte(test.source), "sqlite")
			if len(document.Diagnostics) == 0 {
				t.Fatalf("expected diagnostic, got %#v", document)
			}
			if len(document.Queries) != 0 {
				t.Fatalf("invalid evidence produced queries: %#v", document.Queries)
			}
		})
	}
}

func TestAnalyzePreservesUnicodeAndCRLFLocations(t *testing.T) {
	source := []byte("-- café\r\n-- name: ByName :many\r\nSELECT 名称 FROM 店舗 WHERE 名称 = sqlc.arg('名称');\r\n")
	document := sqltemplate.Analyze(source, "sqlite")
	if len(document.Diagnostics) != 0 || len(document.Queries) != 1 {
		t.Fatalf("document = %#v", document)
	}
	query := document.Queries[0]
	line, column := document.Position(query.StatementStart)
	if line != 3 || column != 1 {
		t.Fatalf("statement position = %d:%d, want 3:1", line, column)
	}
	macro := document.Macros[0]
	line, column = document.Position(macro.Start)
	if line != 3 || column <= 1 {
		t.Fatalf("macro position = %d:%d", line, column)
	}
}
