package sql_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	sqlparser "github.com/cafecito-games/grafo/internal/parser/sql"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
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
	root := t.TempDir()
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

func TestRouterRejectsUnavailableConfiguredDialect(t *testing.T) {
	root := t.TempDir()
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

func declaredTable(result graph.ParseResult) string {
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTable {
			return node.QualifiedName
		}
	}
	return ""
}
