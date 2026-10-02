package gdscript_test

import (
	"context"
	"strings"
	"testing"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestSemanticKeyReusesTheRunsBindingSnapshot is the behavior the workspace key
// exists for. The binding registry is a scan of every Protobuf schema in the
// repository, so a per-file key that rebuilt it would scan the whole repository
// once per script. Editing a schema between two per-file keys proves which
// happened: a reused snapshot cannot see the edit, while a rescan would.
func TestSemanticKeyReusesTheRunsBindingSnapshot(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	writeFile(t, root, "project.godot", "[autoload]\nGame=\"*res://scripts/game.gd\"\n")
	writeFile(t, root, "proto/game.proto", "syntax = \"proto3\";\npackage game;\nmessage Start { string id = 1; }\n")
	paths := []string{"project.godot", "proto/game.proto", "scripts/game.gd", "scripts/hud.gd"}

	parser := gdscriptparser.New()
	workspace := parserapi.Input{Root: root, SourcePaths: paths, Repository: "sample", RepoID: "repo:sample"}
	workspaceKey, err := parser.WorkspaceSemanticKey(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceKey == "" {
		t.Fatal("workspace semantic key is empty")
	}

	fileInput := func(path string) parserapi.Input {
		return parserapi.Input{Root: root, Path: path, SourcePaths: paths, Repository: "sample",
			RepoID: "repo:sample", SemanticKey: workspaceKey}
	}
	first, err := parser.SemanticKey(ctx, fileInput("scripts/game.gd"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "proto/game.proto", "syntax = \"proto3\";\npackage game;\nmessage Start { string id = 1; int32 seed = 2; }\n")
	second, err := parser.SemanticKey(ctx, fileInput("scripts/game.gd"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("per-file semantic key rescanned the schemas: first=%q second=%q", first, second)
	}
	if !strings.Contains(first, workspaceKey) {
		t.Fatalf("per-file semantic key %q does not carry the workspace key %q", first, workspaceKey)
	}

	// The next run is where a schema edit has to be seen, because that is when
	// the workspace key is recomputed.
	rescanned, err := parser.WorkspaceSemanticKey(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if rescanned == workspaceKey {
		t.Fatal("workspace semantic key ignored an edited schema")
	}
}

// TestSemanticKeyStillCoversEveryRepositoryWideInput pins what the split must
// not drop. A script's key has to change when the Godot project that owns it,
// the configured HTTP adapters, the configured test bases, or the binding
// vocabulary changes, whichever side of the split each one now lives on.
func TestSemanticKeyStillCoversEveryRepositoryWideInput(t *testing.T) {
	ctx := context.Background()
	for _, testCase := range []struct {
		name  string
		edit  func(t *testing.T, root string)
		scope string
	}{
		{name: "owning Godot project", scope: "file", edit: func(t *testing.T, root string) {
			writeFile(t, root, "project.godot", "[autoload]\nGame=\"*res://scripts/other.gd\"\n")
		}},
		{name: "configured adapters", scope: "workspace", edit: func(t *testing.T, root string) {
			writeFile(t, root, "grafo.yaml", "adapters:\n- match: {language: gdscript, symbol: API.fetch}\n"+
				"  effects: [{kind: http.request, roles: {method: {argument: 1}, url: {argument: 0}}}]\n")
		}},
		{name: "configured test bases", scope: "workspace", edit: func(t *testing.T, root string) {
			writeFile(t, root, "grafo.yaml", "tests:\n  gdscript:\n    bases:\n      - GutTest\n")
		}},
		{name: "binding vocabulary", scope: "workspace", edit: func(t *testing.T, root string) {
			writeFile(t, root, "proto/game.proto", "syntax = \"proto3\";\npackage game;\nmessage Changed { string id = 1; }\n")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := testtemp.Dir(t)
			writeFile(t, root, "project.godot", "[autoload]\nGame=\"*res://scripts/game.gd\"\n")
			writeFile(t, root, "proto/game.proto", "syntax = \"proto3\";\npackage game;\nmessage Start { string id = 1; }\n")
			paths := []string{"grafo.yaml", "project.godot", "proto/game.proto", "scripts/game.gd"}
			parser := gdscriptparser.New()
			input := parserapi.Input{Root: root, Path: "scripts/game.gd", SourcePaths: paths,
				Repository: "sample", RepoID: "repo:sample"}

			before, err := parser.SemanticKey(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			beforeWorkspace, err := parser.WorkspaceSemanticKey(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			testCase.edit(t, root)
			after, err := parser.SemanticKey(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			afterWorkspace, err := parser.WorkspaceSemanticKey(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				t.Fatalf("editing the %s left the semantic key at %q", testCase.name, before)
			}
			if testCase.scope == "workspace" && beforeWorkspace == afterWorkspace {
				t.Fatalf("editing the %s left the workspace key at %q", testCase.name, beforeWorkspace)
			}
			if testCase.scope == "file" && beforeWorkspace != afterWorkspace {
				t.Fatalf("editing the %s changed the repository-wide key", testCase.name)
			}
		})
	}
}
