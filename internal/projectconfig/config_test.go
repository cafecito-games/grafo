package projectconfig_test

import (
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/projectconfig"
)

func TestParseComponentsNormalizesAndRetainsLocations(t *testing.T) {
	config, err := projectconfig.Parse([]byte(`unknown:
  owned_elsewhere: true
components:
  - name: client
    roots: [./client, web/ui/]
  - name: server
    roots:
      - apps/server
sql:
  default_dialect: postgres
  paths:
    "db/**": postgres
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Components) != 2 {
		t.Fatalf("components = %#v", config.Components)
	}
	client := config.Components[0]
	if client.Name != "client" || client.Line != 4 || len(client.Roots) != 2 || client.Roots[0].Path != "client" || client.Roots[0].Line != 5 || client.Roots[1].Path != "web/ui" {
		t.Fatalf("client = %#v", client)
	}
	if config.SQL.DefaultDialect != "postgres" || len(config.SQL.Paths) != 1 || config.SQL.Paths[0].Pattern != "db/**" {
		t.Fatalf("sql = %#v", config.SQL)
	}
	if sections := config.UnknownSections["unknown"]; len(sections) != 1 || sections[0].Kind == 0 {
		t.Fatalf("unknown sections were not retained: %#v", config.UnknownSections)
	}
}

func TestParseRejectsInvalidComponentContracts(t *testing.T) {
	tests := []struct {
		name, source, want string
	}{
		{name: "components shape", source: "components: client\n", want: "components must be a sequence"},
		{name: "unknown field", source: "components:\n  - name: client\n    roots: [client]\n    deploy: true\n", want: `unknown component field "deploy"`},
		{name: "duplicate field", source: "components:\n  - name: client\n    name: other\n    roots: [client]\n", want: `duplicate component field "name"`},
		{name: "unsafe name", source: "components:\n  - name: ../client\n    roots: [client]\n", want: "invalid component name"},
		{name: "duplicate name", source: "components:\n  - name: client\n    roots: [client]\n  - name: ' client '\n    roots: [other]\n", want: `duplicate component name "client"`},
		{name: "missing roots", source: "components:\n  - name: client\n", want: "roots must contain at least one root"},
		{name: "traversal", source: "components:\n  - name: client\n    roots: [../client]\n", want: "escapes the repository"},
		{name: "absolute", source: "components:\n  - name: client\n    roots: [/client]\n", want: "must be repository-relative"},
		{name: "backslash", source: "components:\n  - name: client\n    roots: ['client\\ui']\n", want: "must use slash separators"},
		{name: "glob", source: "components:\n  - name: client\n    roots: ['client/**']\n", want: "must not contain glob syntax"},
		{name: "normalized duplicate", source: "components:\n  - name: client\n    roots: [client, ./client/]\n", want: `duplicate component root "client"`},
		{name: "same component overlap", source: "components:\n  - name: client\n    roots: [client, client/ui]\n", want: "overlaps"},
		{name: "cross component overlap", source: "components:\n  - name: client\n    roots: [apps]\n  - name: server\n    roots: [apps/server]\n", want: "overlaps"},
		{name: "duplicate top level", source: "components: []\ncomponents: []\n", want: `duplicate top-level section "components"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := projectconfig.Parse([]byte(test.source))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsMalformedAndInvalidSQLBeforeConsumersRun(t *testing.T) {
	tests := []struct {
		source, want string
	}{
		{source: "components: [", want: "decode YAML"},
		{source: "sql: postgres\n", want: "sql must be a mapping"},
		{source: "sql:\n  unknown: true\n", want: `unknown sql setting "unknown"`},
		{source: "sql:\n  default_dialect: ''\n", want: "default_dialect must be a non-empty scalar"},
		{source: "sql:\n  paths:\n    '[broken': postgres\n", want: "invalid path pattern"},
	}
	for _, test := range tests {
		_, err := projectconfig.Parse([]byte(test.source))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("Parse(%q) error = %v, want substring %q", test.source, err, test.want)
		}
	}
}

func TestSQLSemanticKeyIgnoresUnownedSectionsAndComponentOnlyEdits(t *testing.T) {
	first, err := projectconfig.Parse([]byte("components:\n  - name: client\n    roots: [client]\nsql:\n  default_dialect: postgres\nunknown: one\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := projectconfig.Parse([]byte("components:\n  - name: web\n    roots: [web]\nsql:\n  default_dialect: postgres\nunknown: two\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first.SQL.SemanticKey() != second.SQL.SemanticKey() {
		t.Fatalf("component-only edit changed SQL semantic key: %q != %q", first.SQL.SemanticKey(), second.SQL.SemanticKey())
	}
}

func TestParsePreservesEmptySQLConfiguration(t *testing.T) {
	for _, source := range []string{"sql:\n", "sql:\n  paths:\n"} {
		config, err := projectconfig.Parse([]byte(source))
		if err != nil {
			t.Fatalf("Parse(%q): %v", source, err)
		}
		if config.SQL.DefaultDialect != "" || len(config.SQL.Paths) != 0 {
			t.Fatalf("Parse(%q) SQL = %#v", source, config.SQL)
		}
	}
}
