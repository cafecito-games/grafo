package defaults_test

import (
	"testing"

	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
)

func TestRegistryRoutesEverySupportedFixtureFormat(t *testing.T) {
	tests := map[string]string{
		"script.gd":            "gdscript",
		"project.godot":        "godot",
		"scene.tscn":           "godot",
		"resource.tres":        "godot",
		"export.escn":          "godot",
		"shader.gdshader":      "godot",
		"include.gdshaderinc":  "godot",
		"main.go":              "go",
		"service.py":           "python",
		"types.pyi":            "python",
		"worker.ts":            "typescript",
		"view.tsx":             "typescript",
		"go.mod":               "manifest",
		"package.json":         "manifest",
		"requirements-dev.txt": "manifest",
		"schema.sql":           "sql",
		"schema.pgsql":         "sql",
		"schema.psql":          "sql",
		".env.local":           "config",
		"config.yaml":          "config",
		"config.yml":           "config",
		"settings.properties":  "config",
		"config.json":          "config",
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
