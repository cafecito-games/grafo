package godotid_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

func TestIdentityCanonicalizesRepositoryRelativePaths(t *testing.T) {
	for _, testCase := range []struct{ path, want string }{
		{path: "scenes/main.tscn", want: "scenes/main"},
		{path: "./scenes/main.tscn", want: "scenes/main"},
		{path: "client/scenes/main.tscn", want: "client/scenes/main"},
		{path: "scenes/main.v2.tscn", want: "scenes/main.v2"},
		{path: "scripts/player.gd", want: "scripts/player"},
		{path: "LICENSE", want: "LICENSE"},
		{path: ".tscn", want: ".tscn"},
		{path: "", want: ""},
	} {
		if got := godotid.Identity(testCase.path); got != testCase.want {
			t.Errorf("Identity(%q) = %q, want %q", testCase.path, got, testCase.want)
		}
	}
}

// TestResolveScopesReferencesToTheirProject pins the rule that broke composition
// for Godot projects below the repository root: a res:// reference belongs to its
// own project, not to the repository.
func TestResolveScopesReferencesToTheirProject(t *testing.T) {
	for _, testCase := range []struct{ name, projectDir, reference, want string }{
		{name: "project at repository root", reference: "res://scenes/main.tscn", want: "scenes/main"},
		{name: "nested project", projectDir: "client", reference: "res://scenes/main.tscn", want: "client/scenes/main"},
		{name: "deeply nested project", projectDir: "tools/probe", reference: "res://scripts/a.gd", want: "tools/probe/scripts/a"},
		{name: "user data path is project scoped too", projectDir: "client", reference: "user://saves/slot.tres", want: "client/saves/slot"},
		{name: "autoload marker is not part of the path", projectDir: "client", reference: "*res://scripts/game.gd", want: "client/scripts/game"},
		{name: "scheme-less reference is repository relative", projectDir: "client", reference: "scenes/main.tscn", want: "scenes/main"},
		{name: "uid alias carries no path", projectDir: "client", reference: "uid://abc123", want: ""},
		{name: "empty reference", projectDir: "client", reference: "", want: ""},
		{name: "redundant segments collapse", projectDir: "client", reference: "res://scenes/../scenes/main.tscn", want: "client/scenes/main"},
		{name: "reference escaping the repository resolves to nothing", reference: "res://../outside.tscn", want: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := godotid.Resolve(testCase.projectDir, testCase.reference); got != testCase.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q",
					testCase.projectDir, testCase.reference, got, testCase.want)
			}
		})
	}
}

func TestAutoloadQualifiedNameIsScopedToItsProject(t *testing.T) {
	root := godotid.AutoloadQualifiedName("project.godot", "Game")
	nested := godotid.AutoloadQualifiedName("client/project.godot", "Game")
	sibling := godotid.AutoloadQualifiedName("tools/probe/project.godot", "Game")
	if root == nested || nested == sibling {
		t.Fatalf("autoload identities collide across projects: %q %q %q", root, nested, sibling)
	}
	if nested != "godot:autoload:client/project.godot:Game" {
		t.Fatalf("unexpected autoload identity %q", nested)
	}
}

func TestParseProjectClassifiesAutoloadDeclarations(t *testing.T) {
	project := godotid.ParseProject("client/project.godot", []byte(`config_version=5

[autoload]
Game="*res://scripts/game.gd"
Disabled="res://scripts/disabled.gd"
Broken=5
Twice="*res://scripts/a.gd"
Twice="*res://scripts/b.gd"
`))
	game, ok := project.Singleton("Game")
	if !ok || game.Target != "client/scripts/game" || !game.Enabled {
		t.Fatalf("enabled autoload = %#v (ok=%t)", game, ok)
	}
	if _, ok := project.Singleton("Disabled"); ok {
		t.Fatal("a disabled autoload must not resolve as a global singleton")
	}
	disabled, ok := project.Autoload("Disabled")
	if !ok || disabled.Enabled || disabled.Target != "client/scripts/disabled" {
		t.Fatalf("disabled autoload declaration = %#v (ok=%t)", disabled, ok)
	}
	if _, ok := project.Autoload("Twice"); ok {
		t.Fatal("a conflicting autoload must not resolve")
	}
	if _, ok := project.Autoload("Broken"); ok {
		t.Fatal("a malformed autoload must not resolve")
	}
	if len(project.Conflicts) != 1 || project.Conflicts[0] != "Twice" {
		t.Fatalf("conflicts = %#v", project.Conflicts)
	}
	if len(project.Malformed) != 1 || project.Malformed[0] != "Broken" {
		t.Fatalf("malformed = %#v", project.Malformed)
	}
	if project.Digest == "" {
		t.Fatal("project digest must fingerprint the autoload vocabulary")
	}
}

// TestAliasesRejectContradictoryEvidence covers the alias table that makes the
// cross-file UID check possible, including that absence is never treated as
// contradiction.
func TestAliasesRejectContradictoryEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "project.godot", "config_version=5\n")
	write(t, root, "scenes/a.tscn", "[gd_scene format=3 uid=\"uid://a\"]\n\n[node name=\"A\" type=\"Node\"]\n")
	write(t, root, "scenes/b.tscn", "[gd_scene format=3 uid=\"uid://b\"]\n\n[node name=\"B\" type=\"Node\"]\n")
	write(t, root, "scripts/player.gd.uid", "uid://player\n")
	write(t, root, "art/icon.png.import", "[remap]\n\npath=\"res://.godot/imported/icon.png-abc.ctex\"\nuid=\"uid://icon\"\n")
	// A skipped directory must not contribute declarations.
	write(t, root, ".godot/imported/copy.tscn", "[gd_scene format=3 uid=\"uid://a\"]\n")

	aliases, err := godotid.LoadAliases(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name     string
		uid      string
		identity string
		want     bool
	}{
		{name: "matching evidence", uid: "uid://a", identity: "scenes/a", want: true},
		{name: "uid declared by another resource", uid: "uid://a", identity: "scenes/b"},
		{name: "undeclared uid cannot contradict a path", uid: "uid://absent", identity: "scenes/b", want: true},
		{name: "neither is declared", uid: "uid://absent", identity: "scenes/missing", want: true},
		{name: "script sidecar", uid: "uid://player", identity: "scripts/player", want: true},
		{name: "imported asset sidecar", uid: "uid://icon", identity: "art/icon", want: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := aliases.Agrees(testCase.uid, testCase.identity); got != testCase.want {
				t.Fatalf("Agrees(%q, %q) = %t, want %t",
					testCase.uid, testCase.identity, got, testCase.want)
			}
		})
	}
	if declared, _ := aliases.Declared("uid://a"); len(declared) != 1 || declared[0] != "scenes/a" {
		t.Fatalf("a skipped directory contributed a declaration: %#v", declared)
	}
	if len(aliases.Projects) != 1 || aliases.Projects[0] != "project.godot" {
		t.Fatalf("projects = %#v", aliases.Projects)
	}
	if aliases.Digest == "" {
		t.Fatal("alias digest must fingerprint the table")
	}
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
