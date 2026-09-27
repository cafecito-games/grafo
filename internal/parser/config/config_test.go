package config_test

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
)

func TestParserSupportsTOML(t *testing.T) {
	parser := configparser.New()
	for _, path := range []string{"config.toml", "config/Settings.TOML"} {
		if !parser.Supports(path) {
			t.Errorf("expected support for %s", path)
		}
	}
}

func TestParserExtractsTOMLKeysAndReferences(t *testing.T) {
	content := []byte(`title = "grafo"
ports = [8000, 8001]
"literal.dot" = "${ROOT_KEY}"

[database]
host = "${DATABASE_HOST}"
connection.pool.max = 20
metadata = { region = "us-east-1", endpoint = "${API_URL}" }

[[workers]]
queue = "critical"

[[workers]]
queue = "${DEFAULT_QUEUE}"
`)
	result, err := configparser.New().Parse(context.Background(), parserapi.Input{
		Path: "config/settings.toml", Content: content, RepoID: "repo:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", result.Diagnostics)
	}

	wantKeys := []string{
		"database.connection.pool.max",
		"database.host",
		"database.metadata.endpoint",
		"database.metadata.region",
		"literal.dot",
		"ports",
		"title",
		"workers.queue",
	}
	var gotKeys []string
	for _, node := range result.Nodes {
		if node.Kind != graph.KindConfigKey {
			continue
		}
		gotKeys = append(gotKeys, node.Name)
		if node.Properties["format"] != "toml" {
			t.Errorf("format for %q = %q, want toml", node.Name, node.Properties["format"])
		}
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("config keys = %#v, want %#v", gotKeys, wantKeys)
	}

	wantReferences := map[string]bool{"ROOT_KEY": true, "DATABASE_HOST": true, "API_URL": true, "DEFAULT_QUEUE": true}
	gotReferences := map[string]bool{}
	definitions := 0
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeReferences {
			gotReferences[fact.Target] = true
		}
		if fact.Kind == graph.EdgeDefines {
			definitions++
		}
	}
	if definitions != len(wantKeys) {
		t.Fatalf("definition facts = %d, want %d", definitions, len(wantKeys))
	}
	if !reflect.DeepEqual(gotReferences, wantReferences) {
		t.Fatalf("references = %#v, want %#v", gotReferences, wantReferences)
	}
}

func TestParserReportsMalformedTOML(t *testing.T) {
	result, err := configparser.New().Parse(context.Background(), parserapi.Input{
		Path: "config.toml", Content: []byte("[database\nhost = value\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one", result.Diagnostics)
	}
	diagnostic := result.Diagnostics[0]
	if diagnostic.Line == 0 || diagnostic.Level != "warning" || !strings.Contains(diagnostic.Message, "parse TOML") {
		t.Fatalf("unexpected diagnostic: %#v", diagnostic)
	}
}
