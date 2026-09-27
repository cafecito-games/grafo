package defaults_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
)

func TestRegistryRoutesEverySupportedFixtureFormat(t *testing.T) {
	tests := map[string]string{
		"script.gd":                  "gdscript",
		"project.godot":              "godot",
		"scene.tscn":                 "godot",
		"resource.tres":              "godot",
		"export.escn":                "godot",
		"shader.gdshader":            "godot",
		"include.gdshaderinc":        "godot",
		"main.go":                    "go",
		"Worker.java":                "java",
		"service.py":                 "python",
		"types.pyi":                  "python",
		"schema.proto":               "protobuf",
		"Service.swift":              "swift",
		"worker.ts":                  "typescript",
		"view.tsx":                   "typescript",
		"go.mod":                     "manifest",
		"package.json":               "manifest",
		"requirements-dev.txt":       "manifest",
		"schema.sql":                 "sql",
		"schema.pgsql":               "sql",
		"schema.psql":                "sql",
		".env.local":                 "config",
		"config.yaml":                "config",
		"config.yml":                 "config",
		"settings.properties":        "config",
		"config.json":                "config",
		"config.toml":                "config",
		"README.md":                  "markdown",
		"docs/architecture.markdown": "markdown",
	}
	registry := parserdefaults.NewRegistry()
	for path, language := range tests {
		t.Run(path, func(t *testing.T) {
			parser, ok := registry.For(path)
			if !ok || parser.Language() != language {
				t.Fatalf("registry.For(%q) = (%v, %t), want language %q", path, parser, ok, language)
			}
		})
	}
}

func TestRegistryIncludesSQLiteDialect(t *testing.T) {
	registry := parserdefaults.NewRegistry()
	p, ok := registry.For("schema.sql")
	if !ok {
		t.Fatal("default registry does not support schema.sql")
	}
	result, err := p.Parse(context.Background(), parserapi.Input{
		Path: "schema.sql", Content: []byte("CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT) STRICT;"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTable && node.QualifiedName == "events" && node.Properties["dialect"] == "sqlite" {
			return
		}
	}
	t.Fatalf("default registry did not route SQLite syntax: %#v", result.Nodes)
}
