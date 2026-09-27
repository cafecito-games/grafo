package godotid_test

import (
	"os"
	"path/filepath"
	"strings"
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

// TestAliasesNeverTreatTruncatedEvidenceAsApproval covers the rule that unknown
// evidence must never be read as approval: a UID declared beyond the scanner's
// byte bound must not look undeclared, because an undeclared UID is treated as
// agreeing with whatever path a reference pairs with it.
func TestAliasesNeverTreatTruncatedEvidenceAsApproval(t *testing.T) {
	root := t.TempDir()
	write(t, root, "project.godot", "config_version=5\n")
	// A header long enough that the UID sits past any fixed prefix read.
	padding := strings.Repeat("x", 32<<10)
	write(t, root, "scenes/target.tscn",
		"[gd_scene load_steps=2 format=3 script_class=\""+padding+"\" uid=\"uid://target\"]\n\n"+
			"[node name=\"Target\" type=\"Node\"]\n")
	write(t, root, "scripts/late.gd.uid", strings.Repeat("\n", 4<<10)+"uid://late\n")

	aliases, err := godotid.LoadAliases(root)
	if err != nil {
		t.Fatal(err)
	}
	if aliases.Agrees("uid://target", "scenes/other") {
		t.Fatal("a UID beyond the scan bound was treated as undeclared, approving a contradictory path")
	}
	if !aliases.Agrees("uid://target", "scenes/target") {
		t.Fatal("the declaring resource must still agree with its own UID")
	}
	if aliases.Agrees("uid://late", "scripts/other") {
		t.Fatal("a UID beyond the .uid scan bound was treated as undeclared")
	}
}

// TestResolveRefusesToLeaveTheOwningProject pins the project boundary: res:// is
// project-relative, so a reference that traverses out of its project is invalid
// rather than a reference to whatever it lands on.
func TestResolveRefusesToLeaveTheOwningProject(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		projectDir string
		reference  string
		want       string
		escapes    bool
	}{
		{name: "into a sibling project", projectDir: "client", reference: "res://../tools/probe/x.tscn", escapes: true},
		{name: "to the repository root", projectDir: "client", reference: "res://../x.tscn", escapes: true},
		{name: "out of a deep project", projectDir: "tools/probe", reference: "res://../../client/x.tscn", escapes: true},
		{name: "out of the repository", projectDir: "", reference: "res://../outside.tscn", escapes: true},
		{name: "user data escaping too", projectDir: "client", reference: "user://../../etc/passwd", escapes: true},
		{name: "traversal that stays inside", projectDir: "client", reference: "res://scenes/../scenes/x.tscn", want: "client/scenes/x"},
		{name: "project root itself", projectDir: "client", reference: "res://x.tscn", want: "client/x"},
		{name: "sibling directory name is not a prefix match", projectDir: "client", reference: "res://../clientele/x.tscn", escapes: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := godotid.Resolve(testCase.projectDir, testCase.reference)
			if got != testCase.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q",
					testCase.projectDir, testCase.reference, got, testCase.want)
			}
			if escaped := godotid.EscapesProject(testCase.projectDir, testCase.reference); escaped != testCase.escapes {
				t.Fatalf("EscapesProject(%q, %q) = %t, want %t",
					testCase.projectDir, testCase.reference, escaped, testCase.escapes)
			}
		})
	}
}

// TestAliasesFailClosedWhileAnyFileIsUnknown covers the general rule behind the
// truncation case: while the table cannot prove a UID is undeclared, it must not
// report agreement for a UID it simply did not find.
func TestAliasesFailClosedWhileAnyFileIsUnknown(t *testing.T) {
	root := t.TempDir()
	write(t, root, "project.godot", "config_version=5\n")
	write(t, root, "scenes/ok.tscn", "[gd_scene format=3 uid=\"uid://ok\"]\n\n[node name=\"A\" type=\"Node\"]\n")

	complete, err := godotid.LoadAliases(root)
	if err != nil {
		t.Fatal(err)
	}
	if complete.Incomplete() {
		t.Fatalf("a readable project is not incomplete: %#v", complete.Unknown)
	}
	if !complete.Agrees("uid://absent", "scenes/anything") {
		t.Fatal("a provably undeclared UID must leave exact path evidence standing")
	}

	// A first section that never ends within the scan budget leaves this file's
	// declaration status unknown.
	write(t, root, "scenes/huge.tscn", "[gd_scene format=3 script_class=\""+strings.Repeat("x", 2<<20)+"\"]\n")
	incomplete, err := godotid.LoadAliases(root)
	if err != nil {
		t.Fatal(err)
	}
	if !incomplete.Incomplete() || len(incomplete.Unknown) != 1 || incomplete.Unknown[0] != "scenes/huge.tscn" {
		t.Fatalf("unknown files = %#v", incomplete.Unknown)
	}
	if incomplete.Agrees("uid://absent", "scenes/anything") {
		t.Fatal("unknown evidence was read as approval")
	}
	// Nothing is provable while a file is unknown, in either direction: a UID
	// found once among the readable files is not a UID declared once.
	if incomplete.Agrees("uid://ok", "scenes/ok") {
		t.Fatal("a found UID was accepted on unproven uniqueness")
	}
	if incomplete.Digest == complete.Digest {
		t.Fatal("an unknown file must change the digest so dependent files reparse")
	}
}

// TestProjectDigestCoversEveryDeclarationField pins the rule that keeps an
// incremental index equal to a clean rebuild: any field an extractor might read
// has to change the digest, so a change no parser is watching cannot leave a
// stale edge behind.
func TestProjectDigestCoversEveryDeclarationField(t *testing.T) {
	base := "[autoload]\nGame=\"*res://scripts/game.gd\"\n"
	for _, testCase := range []struct{ name, content string }{
		{name: "singleton marker removed", content: "[autoload]\nGame=\"res://scripts/game.gd\"\n"},
		{name: "target changed", content: "[autoload]\nGame=\"*res://scripts/other.gd\"\n"},
		{name: "name changed", content: "[autoload]\nOther=\"*res://scripts/game.gd\"\n"},
		{name: "declaration line moved", content: "\n\n[autoload]\nGame=\"*res://scripts/game.gd\"\n"},
		{name: "uid form instead of a path", content: "[autoload]\nGame=\"*uid://game123\"\n"},
		{name: "became a conflict", content: "[autoload]\nGame=\"*res://scripts/game.gd\"\nGame=\"*res://scripts/two.gd\"\n"},
		{name: "became malformed", content: "[autoload]\nGame=5\n"},
		{name: "another autoload added", content: base + "Menu=\"*res://scripts/menu.gd\"\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			before := godotid.ParseProject("project.godot", []byte(base))
			after := godotid.ParseProject("project.godot", []byte(testCase.content))
			if before.Digest == after.Digest {
				t.Fatalf("digest unchanged; dependent scripts would keep stale edges")
			}
			if before.SemanticKey() == after.SemanticKey() {
				t.Fatalf("semantic key unchanged")
			}
		})
	}
	same := godotid.ParseProject("project.godot", []byte(base))
	again := godotid.ParseProject("project.godot", []byte(base))
	if same.Digest != again.Digest {
		t.Fatal("digest is not stable for identical input")
	}
	if nested := godotid.ParseProject("client/project.godot", []byte(base)); nested.SemanticKey() == same.SemanticKey() {
		t.Fatal("the owning project path must be part of the semantic key")
	}
}

// TestAliasesRefuseAFoundUIDWhileAnyFileIsUnknown covers the positive direction
// of the same rule: finding a UID once among the files that could be read is not
// proof that it is declared once, because an unscanned file may declare it too.
// Uniqueness is what licenses the edge, so an unproven declaration must fail
// closed and must name the file that prevented the proof.
func TestAliasesRefuseAFoundUIDWhileAnyFileIsUnknown(t *testing.T) {
	root := t.TempDir()
	write(t, root, "project.godot", "config_version=5\n")
	write(t, root, "scenes/a.tscn", "[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"A\" type=\"Node\"]\n")

	healthy, err := godotid.LoadAliases(root)
	if err != nil {
		t.Fatal(err)
	}
	if healthy.Incomplete() || len(healthy.Unknown) != 0 {
		t.Fatalf("a healthy repository must leave nothing unknown: %#v", healthy.Unknown)
	}
	if !healthy.Agrees("uid://shared", "scenes/a") {
		t.Fatal("a uniquely declared UID must agree with its declaring resource")
	}

	// One unreadable candidate file is enough to make every declaration unproven.
	write(t, root, "scenes/huge.tscn", "[gd_scene format=3 script_class=\""+strings.Repeat("x", 2<<20)+"\"]\n")
	incomplete, err := godotid.LoadAliases(root)
	if err != nil {
		t.Fatal(err)
	}
	if incomplete.Agrees("uid://shared", "scenes/a") {
		t.Fatal("a found UID was accepted while another file could declare it too")
	}
	reason := incomplete.DisagreementReason("uid://shared", "scenes/a")
	if !strings.Contains(reason, "scenes/huge.tscn") {
		t.Fatalf("the reason must name the unreadable file; got %q", reason)
	}
	if !strings.Contains(reason, "uid://shared") {
		t.Fatalf("the reason must name the UID; got %q", reason)
	}
}
