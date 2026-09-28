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

func TestParseHTTPRequestAPIsAndSemanticKey(t *testing.T) {
	first, err := projectconfig.Parse([]byte(`components:
  - name: client
    roots: [client]
http:
  request_apis:
    - language: gdscript
      symbol: AuthAPI.request_json
      method_argument: 0
      url_argument: 1
    - language: gdscript
      symbol: MetricsAPI.send
      method_argument: 2
      route_argument: 0
unknown: one
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.HTTP.RequestAPIs) != 2 {
		t.Fatalf("request APIs = %#v", first.HTTP.RequestAPIs)
	}
	api := first.HTTP.RequestAPIs[0]
	if api.Language != "gdscript" || api.Symbol != "AuthAPI.request_json" ||
		api.MethodArgument != 0 || api.URLArgument != 1 || api.Line != 6 {
		t.Fatalf("first request API = %#v", api)
	}
	if got := first.HTTP.RequestAPIs[1].URLArgument; got != 0 {
		t.Fatalf("route_argument alias normalized to URLArgument = %d", got)
	}
	second, err := projectconfig.Parse([]byte(`components:
  - name: other
    roots: [other]
http:
  request_apis:
    - url_argument: 1
      method_argument: 0
      symbol: AuthAPI.request_json
      language: gdscript
    - symbol: MetricsAPI.send
      route_argument: 0
      language: gdscript
      method_argument: 2
unknown: two
`))
	if err != nil {
		t.Fatal(err)
	}
	if first.HTTP.SemanticKey() != second.HTTP.SemanticKey() {
		t.Fatalf("equivalent HTTP config changed semantic key: %q != %q", first.HTTP.SemanticKey(), second.HTTP.SemanticKey())
	}
}

func TestParseRejectsInvalidHTTPRequestAPIContracts(t *testing.T) {
	tests := []struct {
		name, source, want string
	}{
		{name: "http shape", source: "http: []\n", want: "http must be a mapping"},
		{name: "unknown http setting", source: "http:\n  clients: []\n", want: `unknown http setting "clients"`},
		{name: "apis shape", source: "http:\n  request_apis: {}\n", want: "request_apis must be a sequence"},
		{name: "api shape", source: "http:\n  request_apis: [gdscript]\n", want: "request API must be a mapping"},
		{name: "unknown field", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: API.send\n      method_argument: 0\n      url_argument: 1\n      headers_argument: 2\n", want: `unknown request API field "headers_argument"`},
		{name: "unsupported language", source: "http:\n  request_apis:\n    - language: python\n      symbol: API.send\n      method_argument: 0\n      url_argument: 1\n", want: `unsupported request API language "python"`},
		{name: "unsafe symbol", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: request\n      method_argument: 0\n      url_argument: 1\n", want: "exact qualified symbol"},
		{name: "negative method", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: API.send\n      method_argument: -1\n      url_argument: 1\n", want: "method_argument must be a non-negative integer"},
		{name: "missing route", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: API.send\n      method_argument: 0\n", want: "one of url_argument or route_argument is required"},
		{name: "both routes", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: API.send\n      method_argument: 0\n      url_argument: 1\n      route_argument: 1\n", want: "only one of url_argument or route_argument"},
		{name: "same indexes", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: API.send\n      method_argument: 1\n      url_argument: 1\n", want: "must use distinct indexes"},
		{name: "built-in conflict", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: HTTPRequest.request\n      method_argument: 0\n      url_argument: 1\n", want: "conflicts with built-in"},
		{name: "duplicate signature", source: "http:\n  request_apis:\n    - language: gdscript\n      symbol: API.send\n      method_argument: 0\n      url_argument: 1\n    - language: gdscript\n      symbol: API.send\n      method_argument: 2\n      url_argument: 3\n", want: `duplicate request API symbol "API.send"`},
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
