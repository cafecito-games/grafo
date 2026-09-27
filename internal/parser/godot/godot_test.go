package godot_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

func TestParserSupportsGodotSourceFormats(t *testing.T) {
	parser := godotparser.New()
	for _, path := range []string{
		"project.godot", "PROJECT.GODOT", "export_presets.cfg", "example.GDEXTENSION",
		"texture.png.import", "scene.tscn.remap", "player.gd.uid", "main.tscn", "data.TRES",
		"import.escn", "water.gdshader", "common.GDSHADERINC",
	} {
		if !parser.Supports(path) {
			t.Errorf("expected support for %s", path)
		}
	}
	for _, path := range []string{"player.gd", "level.scn", "asset.res"} {
		if parser.Supports(path) {
			t.Errorf("unexpected support for %s", path)
		}
	}
}

func TestParserExtractsConfigFilesResourcesAndUIDs(t *testing.T) {
	result := parse(t, "addons/example/plugin.cfg", `[plugin]
name="Example"
script="res://addons/example/plugin_script.gd"
identity="uid://c3m2k2i8we5da"

[deploy]
run_script="#!/usr/bin/env bash
echo deploying"
after="value"
`)
	module := assertNode(t, result.Nodes, graph.KindModule, "addons/example/plugin")
	if module.Properties["format"] != "cfg" {
		t.Fatalf("config module format = %q", module.Properties["format"])
	}
	assertNode(t, result.Nodes, graph.KindConfigKey, "config:addons/example/plugin.cfg:plugin/script")
	assertNode(t, result.Nodes, graph.KindConfigKey, "config:addons/example/plugin.cfg:deploy/after")
	assertFactTarget(t, result.Facts, graph.EdgeReferences, "addons/example/plugin_script")
	assertFactTarget(t, result.Facts, graph.EdgeReferences, "uid://c3m2k2i8we5da")

	uid := parse(t, "scripts/player.gd.uid", "uid://c3m2k2i8we5da\n")
	uidNode := assertNode(t, uid.Nodes, graph.KindConfigKey, "uid://c3m2k2i8we5da")
	if uidNode.Properties["resource"] != "scripts/player.gd" {
		t.Fatalf("UID resource = %q", uidNode.Properties["resource"])
	}
	assertFactTarget(t, uid.Facts, graph.EdgeReferences, "scripts/player")
}

func TestParserExtractsProjectSettingsAndResources(t *testing.T) {
	result := parse(t, "project.godot", `config_version=5

[application]
config/name="Sample"
run/main_scene="res://scenes/main.tscn"

[autoload]
Game="*res://scripts/game.gd"

[input]
jump={"deadzone": 0.5, "events": []}
`)
	assertNode(t, result.Nodes, graph.KindModule, "project")
	assertNode(t, result.Nodes, graph.KindConfigKey, "config:project.godot:application/run/main_scene")
	assertNode(t, result.Nodes, graph.KindConfigKey, "config:project.godot:autoload/Game")
	assertNode(t, result.Nodes, graph.KindConfigKey, "config:project.godot:input/jump")
	assertFactTarget(t, result.Facts, graph.EdgeReferences, "scenes/main")
	assertFactTarget(t, result.Facts, graph.EdgeReferences, "scripts/game")
}

func TestParserExtractsSceneNodesResourcesPropertiesAndSignals(t *testing.T) {
	result := parse(t, "scenes/main.tscn", `[gd_scene load_steps=3 format=3 uid="uid://scene123"]

[ext_resource type="Script" uid="uid://script123" path="res://scripts/player.gd" id="1_script"]

[sub_resource type="StyleBoxFlat" id="Style_button"]
bg_color = Color(1, 0, 0, 1)

[node name="Player" type="Node2D"]
script = ExtResource("1_script")
style = SubResource("Style_button")
target = ^"Button"

[node name="Button" type="Button" parent="." unique_name_in_owner=true]
text = "Play"

[connection signal="pressed" from="Button" to="." method="_on_button_pressed"]
`)
	assertNode(t, result.Nodes, graph.KindGodotScene, "scenes/main")
	assertNode(t, result.Nodes, graph.KindGodotResource, "scenes/main#Style_button")
	player := assertNode(t, result.Nodes, graph.KindGodotSceneNode, "scenes/main:Player")
	button := assertNode(t, result.Nodes, graph.KindGodotSceneNode, "scenes/main:Player/Button")
	if button.Properties["unique_name_in_owner"] != "true" {
		t.Fatalf("scene node unique_name_in_owner = %q", button.Properties["unique_name_in_owner"])
	}
	assertNode(t, result.Nodes, graph.KindField, "scenes/main:Player.script")
	assertNode(t, result.Nodes, graph.KindEvent, "scenes/main:Player/Button.pressed")
	assertFactTarget(t, result.Facts, graph.EdgeImports, "scripts/player")
	assertFactTarget(t, result.Facts, graph.EdgeAttachesScript, "scripts/player")
	assertFactTarget(t, result.Facts, graph.EdgeReferences, "uid://scene123")
	assertFactTarget(t, result.Facts, graph.EdgeReferences, "uid://script123")
	assertFactToID(t, result.Facts, graph.EdgeDeclares, button.ID)
	assertFactFromTo(t, result.Facts, graph.EdgeSubscribes, player.ID,
		findNode(t, result.Nodes, graph.KindEvent, "scenes/main:Player/Button.pressed").ID)
	assertFactTarget(t, result.Facts, graph.EdgeHandledBy, "_on_button_pressed")
}

func TestParserExtractsShaderSymbolsCallsUniformReferencesAndIncludes(t *testing.T) {
	result := parse(t, "shaders/water.gdshader", `#include "res://shaders/common.gdshaderinc"
shader_type spatial;
render_mode blend_mix, depth_draw_opaque;
group_uniforms Surface.Main;
uniform vec4 tint : source_color = vec4(1.0);
group_uniforms;

struct MaterialData {
	vec3 normal;
	float roughness;
};

float shade(float amount) {
	return amount;
}

void fragment() {
	float strength = shade(tint.r);
	ALBEDO = vec3(strength);
}
`)
	assertNode(t, result.Nodes, graph.KindModule, "shaders/water")
	tint := assertNode(t, result.Nodes, graph.KindField, "shaders/water.tint")
	assertNode(t, result.Nodes, graph.KindType, "shaders/water.MaterialData")
	assertNode(t, result.Nodes, graph.KindField, "shaders/water.MaterialData.normal")
	shade := assertNode(t, result.Nodes, graph.KindFunction, "shaders/water.shade")
	fragment := assertNode(t, result.Nodes, graph.KindFunction, "shaders/water.fragment")
	assertNode(t, result.Nodes, graph.KindParameter, "shaders/water.shade.amount")
	assertFactTarget(t, result.Facts, graph.EdgeImports, "shaders/common")
	assertFactFromTo(t, result.Facts, graph.EdgeCalls, fragment.ID, shade.ID)
	assertFactFromTo(t, result.Facts, graph.EdgeReferences, fragment.ID, tint.ID)
}

func TestParserReturnsPositionedErrors(t *testing.T) {
	_, err := godotparser.New().Parse(context.Background(), parserapi.Input{
		Path: "broken.tres", Content: []byte("[gd_resource format=3]\n[resource]\nvalue = [1, nope(]\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "broken.tres:3:") {
		t.Fatalf("expected positioned error, got %v", err)
	}
}

func parse(t *testing.T, path, content string) graph.ParseResult {
	t.Helper()
	result, err := godotparser.New().Parse(context.Background(), parserapi.Input{
		Path: path, Content: []byte(content), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) graph.Node {
	t.Helper()
	return findNode(t, nodes, kind, qualified)
}

func findNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return node
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, qualified, nodes)
	return graph.Node{}
}

func assertFactTarget(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
}

func assertFactToID(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, targetID string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.TargetID == targetID {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, targetID, facts)
}

func assertFactFromTo(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, fromID, targetID string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID && fact.TargetID == targetID {
			return
		}
	}
	t.Fatalf("missing %s fact from %q to %q; got %#v", kind, fromID, targetID, facts)
}

func TestParserModelsScenesResourcesScriptsAndInstances(t *testing.T) {
	result := parse(t, "scenes/main.tscn", `[gd_scene load_steps=5 format=3 uid="uid://scene123"]

[ext_resource type="Script" uid="uid://script123" path="res://scripts/player.gd" id="1_script"]
[ext_resource type="PackedScene" uid="uid://enemy123" path="res://scenes/enemy.tscn" id="2_enemy"]
[ext_resource type="Texture2D" path="res://art/icon.png" id="3_icon"]

[sub_resource type="StyleBoxFlat" id="Style_button"]
bg_color = Color(1, 0, 0, 1)

[node name="Player" type="Node2D"]
script = ExtResource("1_script")
style = SubResource("Style_button")

[node name="Enemy" parent="." instance=ExtResource("2_enemy")]

[node name="Ghost" parent="." instance_placeholder="res://scenes/enemy.tscn"]
`)
	scene := assertNode(t, result.Nodes, graph.KindGodotScene, "scenes/main")
	if scene.Properties["form"] != "scene" || scene.Properties["uid"] != "uid://scene123" {
		t.Fatalf("scene properties = %#v", scene.Properties)
	}
	assertNode(t, result.Nodes, graph.KindGodotResource, "scenes/main#Style_button")
	player := assertNode(t, result.Nodes, graph.KindGodotSceneNode, "scenes/main:Player")
	enemy := assertNode(t, result.Nodes, graph.KindGodotSceneNode, "scenes/main:Player/Enemy")
	ghost := assertNode(t, result.Nodes, graph.KindGodotSceneNode, "scenes/main:Player/Ghost")
	alias := assertNode(t, result.Nodes, graph.KindConfigKey, "uid://scene123")
	if alias.Properties["resource"] != "scenes/main.tscn" {
		t.Fatalf("uid alias properties = %#v", alias.Properties)
	}

	attaches := assertFact(t, result.Facts, graph.EdgeAttachesScript, player.ID, "scripts/player")
	if attaches.TargetKind != graph.KindModule || attaches.Properties["resource"] != "res://scripts/player.gd" {
		t.Fatalf("attaches_script fact = %#v", attaches)
	}
	instance := assertFact(t, result.Facts, graph.EdgeInstantiates, enemy.ID, "scenes/enemy")
	if instance.TargetKind != graph.KindGodotScene || instance.Properties["form"] != "nested" ||
		instance.Properties["resource_id"] != "2_enemy" || instance.Properties["uid"] != "uid://enemy123" {
		t.Fatalf("instantiates fact = %#v", instance)
	}
	placeholder := assertFact(t, result.Facts, graph.EdgeInstantiates, ghost.ID, "scenes/enemy")
	if placeholder.Properties["instance_placeholder"] != "true" {
		t.Fatalf("placeholder instantiates fact = %#v", placeholder)
	}
	if fact := findFact(result.Facts, graph.EdgeImports, "scenes/enemy"); fact.TargetKind != graph.KindGodotScene {
		t.Fatalf("ext_resource scene import kind = %q", fact.TargetKind)
	}
	if fact := findFact(result.Facts, graph.EdgeImports, "art/icon"); fact.TargetKind != graph.KindGodotResource {
		t.Fatalf("ext_resource asset import kind = %q", fact.TargetKind)
	}
}

func TestParserModelsInheritedScenesAndResourceScripts(t *testing.T) {
	inherited := parse(t, "scenes/hero.tscn", `[gd_scene load_steps=2 format=3 uid="uid://hero123"]

[ext_resource type="PackedScene" path="res://scenes/actor.tscn" id="1_actor"]

[node name="Actor" instance=ExtResource("1_actor")]
`)
	scene := assertNode(t, inherited.Nodes, graph.KindGodotScene, "scenes/hero")
	root := assertNode(t, inherited.Nodes, graph.KindGodotSceneNode, "scenes/hero:Actor")
	sceneLevel := assertFact(t, inherited.Facts, graph.EdgeInstantiates, scene.ID, "scenes/actor")
	if sceneLevel.Properties["form"] != "inherited" {
		t.Fatalf("scene-level instantiates fact = %#v", sceneLevel)
	}
	if fact := assertFact(t, inherited.Facts, graph.EdgeInstantiates, root.ID, "scenes/actor"); fact.Properties["form"] != "inherited" {
		t.Fatalf("root instantiates fact = %#v", fact)
	}

	resource := parse(t, "resources/theme.tres", `[gd_resource type="Resource" script_class="Theme" load_steps=2 format=3 uid="uid://theme123"]

[ext_resource type="Script" path="res://scripts/theme.gd" id="1_theme"]

[sub_resource type="Resource" id="Sub_1"]
script = ExtResource("1_theme")

[resource]
script = ExtResource("1_theme")
`)
	resourceNode := assertNode(t, resource.Nodes, graph.KindGodotResource, "resources/theme")
	if resourceNode.Properties["form"] != "resource" || resourceNode.Properties["script_class"] != "Theme" {
		t.Fatalf("resource properties = %#v", resourceNode.Properties)
	}
	sub := assertNode(t, resource.Nodes, graph.KindGodotResource, "resources/theme#Sub_1")
	assertFact(t, resource.Facts, graph.EdgeAttachesScript, resourceNode.ID, "scripts/theme")
	assertFact(t, resource.Facts, graph.EdgeAttachesScript, sub.ID, "scripts/theme")
}

func TestParserKeepsAmbiguousResourceEvidenceUnresolved(t *testing.T) {
	result := parse(t, "scenes/broken.tscn", `[gd_scene load_steps=3 format=3]

[ext_resource type="PackedScene" uid="uid://shared" path="res://scenes/a.tscn" id="1_a"]
[ext_resource type="PackedScene" uid="uid://shared" path="res://scenes/b.tscn" id="2_b"]
[ext_resource type="PackedScene" path="res://scenes/c.tscn" id="1_a"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_a")]
`)
	assertDiagnostic(t, result.Diagnostics, "uid://shared")
	assertDiagnostic(t, result.Diagnostics, `"1_a"`)
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeInstantiates {
			t.Fatalf("ambiguous ExtResource produced an instantiates fact: %#v", fact)
		}
		if fact.Target == "uid://shared" {
			t.Fatalf("ambiguous uid produced a reference fact: %#v", fact)
		}
	}
}

func TestParserModelsAutoloadDeclarations(t *testing.T) {
	result := parse(t, "project.godot", `config_version=5

[autoload]
Game="*res://scripts/game.gd"
Menu="res://scenes/menu.tscn"
Broken=5
`)
	assertNode(t, result.Nodes, graph.KindConfigKey, "config:project.godot:autoload/Broken")
	game := assertNode(t, result.Nodes, graph.KindGodotAutoload, "godot:autoload:project.godot:Game")
	if game.Name != "Game" || game.Properties["singleton"] != "true" || game.Properties["enabled"] != "true" {
		t.Fatalf("autoload properties = %#v", game)
	}
	if game.Properties["resource"] != "res://scripts/game.gd" {
		t.Fatalf("autoload resource = %#v", game.Properties)
	}
	menu := assertNode(t, result.Nodes, graph.KindGodotAutoload, "godot:autoload:project.godot:Menu")
	if menu.Properties["enabled"] != "false" {
		t.Fatalf("disabled autoload properties = %#v", menu.Properties)
	}
	script := assertFact(t, result.Facts, graph.EdgeAutoloads, game.ID, "scripts/game")
	if script.TargetKind != graph.KindModule {
		t.Fatalf("autoload script target kind = %q", script.TargetKind)
	}
	if fact := assertFact(t, result.Facts, graph.EdgeAutoloads, menu.ID, "scenes/menu"); fact.TargetKind != graph.KindGodotScene {
		t.Fatalf("autoload scene target kind = %q", fact.TargetKind)
	}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindGodotAutoload && node.Name == "Broken" {
			t.Fatalf("malformed autoload produced a node: %#v", node)
		}
	}
	assertDiagnostic(t, result.Diagnostics, "Broken")
}

func TestParserKeepsConflictingAutoloadNamesUnresolved(t *testing.T) {
	result := parse(t, "project.godot", `[autoload]
Game="*res://scripts/game.gd"
Game="*res://scripts/other.gd"
`)
	assertDiagnostic(t, result.Diagnostics, "Game")
	for _, node := range result.Nodes {
		if node.Kind == graph.KindGodotAutoload {
			t.Fatalf("conflicting autoload produced a node: %#v", node)
		}
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeAutoloads {
			t.Fatalf("conflicting autoload produced an edge: %#v", fact)
		}
	}
}

func assertFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, fromID, target string) graph.Fact {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID && fact.Target == target {
			return fact
		}
	}
	t.Fatalf("missing %s fact from %q to %q; got %#v", kind, fromID, target, facts)
	return graph.Fact{}
}

func findFact(facts []graph.Fact, kind graph.EdgeKind, target string) graph.Fact {
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return fact
		}
	}
	return graph.Fact{}
}

func assertDiagnostic(t *testing.T, diagnostics []graph.Diagnostic, contains string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, contains) {
			return
		}
	}
	t.Fatalf("missing diagnostic containing %q; got %#v", contains, diagnostics)
}

// TestParserKeepsUIDPathDisagreementUnresolved covers the fail-closed contract
// for UID evidence that contradicts its path: the UID is declared by another
// resource, so trusting the path would produce a confident edge to the wrong
// scene.
func TestParserKeepsUIDPathDisagreementUnresolved(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "project.godot", "config_version=5\n")
	writeProjectFile(t, root, "scenes/a.tscn", "[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"A\" type=\"Node\"]\n")
	writeProjectFile(t, root, "scenes/b.tscn", "[gd_scene format=3 uid=\"uid://other\"]\n\n[node name=\"B\" type=\"Node\"]\n")

	for _, testCase := range []struct {
		name       string
		content    string
		diagnostic string
		resolves   bool
	}{
		{
			name: "uid declared by another resource",
			content: `[gd_scene load_steps=2 format=3]

[ext_resource type="PackedScene" uid="uid://shared" path="res://scenes/b.tscn" id="1_b"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_b")]
`,
			diagnostic: "uid://shared",
		},
		{
			name: "uid agrees with its path",
			content: `[gd_scene load_steps=2 format=3]

[ext_resource type="PackedScene" uid="uid://other" path="res://scenes/b.tscn" id="1_b"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_b")]
`,
			resolves: true,
		},
		{
			name: "unknown uid cannot be contradicted",
			content: `[gd_scene load_steps=2 format=3]

[ext_resource type="PackedScene" uid="uid://absent" path="res://scenes/missing.tscn" id="1_m"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_m")]
`,
			resolves: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := parseIn(t, root, "scenes/caller.tscn", testCase.content)
			instantiates := findFact(result.Facts, graph.EdgeInstantiates, "scenes/b")
			if testCase.resolves {
				if len(result.Diagnostics) != 0 {
					t.Fatalf("unexpected diagnostics: %#v", result.Diagnostics)
				}
				found := false
				for _, fact := range result.Facts {
					if fact.Kind == graph.EdgeInstantiates {
						found = true
					}
				}
				if !found {
					t.Fatalf("agreeing evidence produced no instantiates fact: %#v", result.Facts)
				}
				return
			}
			assertDiagnostic(t, result.Diagnostics, testCase.diagnostic)
			if instantiates.Kind != "" {
				t.Fatalf("contradicted UID produced an instantiates fact: %#v", instantiates)
			}
			for _, fact := range result.Facts {
				if fact.Kind == graph.EdgeInstantiates || fact.Target == "scenes/b" {
					t.Fatalf("contradicted UID produced a resolved fact: %#v", fact)
				}
			}
		})
	}
}

// TestParserKeepsRepeatedResourceIDsUnresolved covers sticky ambiguity: any
// multiply declared ExtResource id must stay unresolved, including when the
// repeats agree and when a later declaration repeats the previous path.
func TestParserKeepsRepeatedResourceIDsUnresolved(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{
			name: "repeated with the same path",
			content: `[gd_scene load_steps=3 format=3]

[ext_resource type="PackedScene" path="res://scenes/a.tscn" id="1_a"]
[ext_resource type="PackedScene" path="res://scenes/a.tscn" id="1_a"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_a")]
`,
		},
		{
			name: "third declaration repeats the previous path",
			content: `[gd_scene load_steps=4 format=3]

[ext_resource type="PackedScene" path="res://scenes/a.tscn" id="1_a"]
[ext_resource type="PackedScene" path="res://scenes/b.tscn" id="1_a"]
[ext_resource type="PackedScene" path="res://scenes/b.tscn" id="1_a"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_a")]
`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := parse(t, "scenes/repeat.tscn", testCase.content)
			assertDiagnostic(t, result.Diagnostics, `"1_a"`)
			for _, fact := range result.Facts {
				if fact.Kind == graph.EdgeInstantiates {
					t.Fatalf("repeated ExtResource id produced an instantiates fact: %#v", fact)
				}
			}
		})
	}
}

// TestParserResolvesResourcesWithinNestedProject covers a Godot project below
// the repository root: a res:// reference is relative to the project, so it must
// canonicalize to a repository-relative identity under that project directory.
func TestParserResolvesResourcesWithinNestedProject(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "client/project.godot", "config_version=5\n\n[autoload]\nGame=\"*res://scripts/game.gd\"\n")
	writeProjectFile(t, root, "client/scenes/enemy.tscn", "[gd_scene format=3 uid=\"uid://enemy123\"]\n\n[node name=\"Enemy\" type=\"Node\"]\n")

	scene := parseIn(t, root, "client/scenes/main.tscn", `[gd_scene load_steps=3 format=3 uid="uid://main123"]

[ext_resource type="Script" path="res://scripts/player.gd" id="1_player"]
[ext_resource type="PackedScene" uid="uid://enemy123" path="res://scenes/enemy.tscn" id="2_enemy"]

[node name="Main" type="Node"]
script = ExtResource("1_player")

[node name="Enemy" parent="." instance=ExtResource("2_enemy")]
`)
	assertNode(t, scene.Nodes, graph.KindGodotScene, "client/scenes/main")
	assertFactTarget(t, scene.Facts, graph.EdgeInstantiates, "client/scenes/enemy")
	assertFactTarget(t, scene.Facts, graph.EdgeAttachesScript, "client/scripts/player")
	assertFactTarget(t, scene.Facts, graph.EdgeImports, "client/scenes/enemy")

	config := parseIn(t, root, "client/project.godot", "config_version=5\n\n[autoload]\nGame=\"*res://scripts/game.gd\"\n")
	autoload := assertNode(t, config.Nodes, graph.KindGodotAutoload, "godot:autoload:client/project.godot:Game")
	assertFact(t, config.Facts, graph.EdgeAutoloads, autoload.ID, "client/scripts/game")
}

func parseIn(t *testing.T, root, path, content string) graph.ParseResult {
	t.Helper()
	result, err := godotparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: path, Content: []byte(content), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeProjectFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestParserTrustsPathWhenUIDIsDeclaredNowhere pins the limit of the UID check:
// absence of a declaration is not contradiction, so an undeclared alias leaves
// exact path evidence standing. Canonical identity drops the extension, which
// means a scene and its script share one identity while each declares its own
// UID, so "this path declares some other UID" is not evidence of disagreement.
func TestParserTrustsPathWhenUIDIsDeclaredNowhere(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "project.godot", "config_version=5\n")
	writeProjectFile(t, root, "scenes/target.tscn", "[gd_scene format=3 uid=\"uid://current\"]\n\n[node name=\"T\" type=\"Node\"]\n")
	writeProjectFile(t, root, "scenes/target.gd.uid", "uid://script\n")

	result := parseIn(t, root, "scenes/caller.tscn", `[gd_scene load_steps=2 format=3]

[ext_resource type="PackedScene" uid="uid://current" path="res://scenes/target.tscn" id="1_t"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_t")]
`)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("a scene sharing its identity with a script was diagnosed: %#v", result.Diagnostics)
	}
	assertFactTarget(t, result.Facts, graph.EdgeInstantiates, "scenes/target")
}

// TestParserKeepsEveryReferenceThroughABadUIDUnresolved covers stickiness on the
// UID table: once a UID is contradicted or multiply declared it is permanently
// unusable, so a later declaration reusing it under a different id must not
// resolve from its path either.
func TestParserKeepsEveryReferenceThroughABadUIDUnresolved(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "project.godot", "config_version=5\n")
	writeProjectFile(t, root, "scenes/a.tscn", "[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"A\" type=\"Node\"]\n")
	writeProjectFile(t, root, "scenes/b.tscn", "[gd_scene format=3 uid=\"uid://b\"]\n\n[node name=\"B\" type=\"Node\"]\n")
	writeProjectFile(t, root, "scenes/c.tscn", "[gd_scene format=3 uid=\"uid://c\"]\n\n[node name=\"C\" type=\"Node\"]\n")

	for _, testCase := range []struct {
		name    string
		content string
	}{
		{
			name: "later declaration with a different id reuses the contradicted uid",
			content: `[gd_scene load_steps=3 format=3]

[ext_resource type="PackedScene" uid="uid://shared" path="res://scenes/b.tscn" id="1_b"]
[ext_resource type="PackedScene" uid="uid://shared" path="res://scenes/c.tscn" id="2_c"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("2_c")]
`,
		},
		{
			name: "later declaration reuses a uid already ambiguous within the document",
			content: `[gd_scene load_steps=4 format=3]

[ext_resource type="PackedScene" uid="uid://dup" path="res://scenes/b.tscn" id="1_b"]
[ext_resource type="PackedScene" uid="uid://dup" path="res://scenes/c.tscn" id="2_c"]
[ext_resource type="PackedScene" uid="uid://dup" path="res://scenes/a.tscn" id="3_a"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("3_a")]
`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := parseIn(t, root, "scenes/caller.tscn", testCase.content)
			if len(result.Diagnostics) == 0 {
				t.Fatalf("expected a diagnostic; got none")
			}
			for _, fact := range result.Facts {
				switch fact.Kind {
				case graph.EdgeInstantiates, graph.EdgeAttachesScript:
					t.Fatalf("a reference through a bad UID resolved: %#v", fact)
				case graph.EdgeImports:
					t.Fatalf("a bad UID still produced an import: %#v", fact)
				}
			}
		})
	}
}

// TestParserKeepsProjectEscapingReferencesUnresolved covers the project boundary:
// res:// is project-relative, so a reference that traverses out of its own
// project is not a valid reference and must not resolve into a sibling project.
func TestParserKeepsProjectEscapingReferencesUnresolved(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "client/project.godot", "config_version=5\n")
	writeProjectFile(t, root, "tools/probe/project.godot", "config_version=5\n")
	writeProjectFile(t, root, "tools/probe/scenes/probe.tscn",
		"[gd_scene format=3 uid=\"uid://probe\"]\n\n[node name=\"Probe\" type=\"Node\"]\n")

	result := parseIn(t, root, "client/scenes/main.tscn", `[gd_scene load_steps=2 format=3]

[ext_resource type="PackedScene" path="res://../tools/probe/scenes/probe.tscn" id="1_probe"]

[node name="Main" type="Node"]

[node name="Probe" parent="." instance=ExtResource("1_probe")]
`)
	assertDiagnostic(t, result.Diagnostics, "res://../tools/probe/scenes/probe.tscn")
	for _, fact := range result.Facts {
		if fact.Target == "tools/probe/scenes/probe" {
			t.Fatalf("a res:// reference escaped its project: %#v", fact)
		}
		if fact.Kind == graph.EdgeInstantiates {
			t.Fatalf("an escaping reference produced an instantiates fact: %#v", fact)
		}
	}
}

// TestParserNamesTheUnreadableFileThatBlockedAUIDProof covers the end of the
// evidence chain: a reference whose UID is declared exactly once among the
// readable files still resolves to nothing while another file could declare it
// too, and the diagnostic names the file that prevented the proof so it can be
// fixed rather than merely reported.
func TestParserNamesTheUnreadableFileThatBlockedAUIDProof(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "project.godot", "config_version=5\n")
	writeProjectFile(t, root, "scenes/target.tscn",
		"[gd_scene format=3 uid=\"uid://target\"]\n\n[node name=\"T\" type=\"Node\"]\n")
	content := `[gd_scene load_steps=2 format=3]

[ext_resource type="PackedScene" uid="uid://target" path="res://scenes/target.tscn" id="1_t"]

[node name="Root" type="Node"]

[node name="Child" parent="." instance=ExtResource("1_t")]
`

	// While every file is readable the reference is proven and resolves.
	healthy := parseIn(t, root, "scenes/caller.tscn", content)
	if len(healthy.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics on readable input: %#v", healthy.Diagnostics)
	}
	assertFactTarget(t, healthy.Facts, graph.EdgeInstantiates, "scenes/target")

	// One unreadable candidate file removes the proof of uniqueness. The indexer
	// rescans the repository once per run, which is what picks the new file up;
	// within a run the table stays fixed so every file sees the same evidence.
	writeProjectFile(t, root, "scenes/unreadable.tscn",
		"[gd_scene format=3 script_class=\""+strings.Repeat("x", 2<<20)+"\"]\n")
	if _, err := godotid.LoadAliases(root); err != nil {
		t.Fatal(err)
	}
	blocked := parseIn(t, root, "scenes/caller.tscn", content)
	assertDiagnostic(t, blocked.Diagnostics, "scenes/unreadable.tscn")
	assertDiagnostic(t, blocked.Diagnostics, "uid://target")
	for _, fact := range blocked.Facts {
		switch fact.Kind {
		case graph.EdgeInstantiates, graph.EdgeImports:
			t.Fatalf("an unproven UID still produced a resolved fact: %#v", fact)
		}
	}
}

// TestParserDeclaresInputActionsAndGlobalGroups covers the two project.godot
// sections this change promotes into first-class Godot vocabulary, including the
// declarations that must resolve nothing: the generic configuration key stays
// exactly where it was, and the action or group node is layered on top of it.
func TestParserDeclaresInputActionsAndGlobalGroups(t *testing.T) {
	result := parse(t, "project.godot", `config_version=5

[input]

jump={
"deadzone": 0.25,
"events": [Object(InputEventKey,"keycode":32)]
}
attack={"deadzone": 0.5, "events": []}
attack={"deadzone": 0.9, "events": []}
broken=5

[global_group]

enemies="Hostile actors"
props=7
`)
	action := assertNode(t, result.Nodes, graph.KindGodotInputAction, "godot:input_action:project.godot:jump")
	if action.Name != "jump" || action.Properties["form"] != "input_action" ||
		action.Properties["deadzone"] != "0.25" || action.Properties["events"] != "1" ||
		action.Properties["config_key"] != "input/jump" {
		t.Fatalf("unexpected input action node: %#v", action)
	}
	group := assertNode(t, result.Nodes, graph.KindGodotNodeGroup, "godot:node_group:project.godot:enemies")
	if group.Name != "enemies" || group.Properties["form"] != "node_group" ||
		group.Properties["global"] != "true" || group.Properties["description"] != "Hostile actors" ||
		group.Properties["config_key"] != "global_group/enemies" {
		t.Fatalf("unexpected node group node: %#v", group)
	}
	// The generic configuration key is still defined, and the declaration hangs
	// off it, so config catalogs keep working without a name-prefix heuristic.
	key := assertNode(t, result.Nodes, graph.KindConfigKey, "config:project.godot:input/jump")
	assertFactFromTo(t, result.Facts, graph.EdgeDefines, key.ID, action.ID)
	groupKey := assertNode(t, result.Nodes, graph.KindConfigKey, "config:project.godot:global_group/enemies")
	assertFactFromTo(t, result.Facts, graph.EdgeDefines, groupKey.ID, group.ID)

	for _, unwanted := range []struct {
		kind      graph.NodeKind
		qualified string
	}{
		{kind: graph.KindGodotInputAction, qualified: "godot:input_action:project.godot:attack"},
		{kind: graph.KindGodotInputAction, qualified: "godot:input_action:project.godot:broken"},
		{kind: graph.KindGodotNodeGroup, qualified: "godot:node_group:project.godot:props"},
	} {
		for _, node := range result.Nodes {
			if node.Kind == unwanted.kind && node.QualifiedName == unwanted.qualified {
				t.Fatalf("%s %q must not be declared from ambiguous or malformed configuration",
					unwanted.kind, unwanted.qualified)
			}
		}
	}
	for _, want := range []string{
		`input action "attack" is declared more than once`,
		`input action "broken" is not an action dictionary`,
		`node group "props" has no description string`,
	} {
		found := false
		for _, diagnostic := range result.Diagnostics {
			if strings.Contains(diagnostic.Message, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing diagnostic %q; got %#v", want, result.Diagnostics)
		}
	}
}

// TestParserRecordsSceneNodeGroupMembership covers declarative group membership:
// every group a scene node declares becomes one project-scoped membership edge
// carrying the node path that proves it, and an item that is not a group name is
// diagnosed instead of guessed.
func TestParserRecordsSceneNodeGroupMembership(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "client/project.godot", "config_version=5\n")
	result := parseIn(t, root, "client/scenes/arena.tscn", `[gd_scene format=3]

[node name="Arena" type="Node2D" groups=["arenas"]]

[node name="Enemy" type="Node2D" parent="." groups=["enemies", "damageable"]]

[node name="Broken" type="Node2D" parent="." groups=[7]]
`)
	arena := assertNode(t, result.Nodes, graph.KindGodotSceneNode, "client/scenes/arena:Arena")
	enemy := assertNode(t, result.Nodes, graph.KindGodotSceneNode, "client/scenes/arena:Arena/Enemy")
	assertFact(t, result.Facts, graph.EdgeInGroup, arena.ID,
		"godot:node_group:client/project.godot:arenas")
	for _, group := range []string{"enemies", "damageable"} {
		assertFact(t, result.Facts, graph.EdgeInGroup, enemy.ID,
			"godot:node_group:client/project.godot:"+group)
	}
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeInGroup {
			continue
		}
		if fact.TargetKind != graph.KindGodotNodeGroup {
			t.Fatalf("membership fact must target a node group: %#v", fact)
		}
		if fact.Properties["form"] != "declared" || fact.Properties["node_path"] == "" ||
			fact.Properties["group"] == "" {
			t.Fatalf("membership fact is missing its evidence: %#v", fact)
		}
	}
	found := false
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, `scene node "Arena/Broken" declares a group that is not a name`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("a non-name group item must be diagnosed; got %#v", result.Diagnostics)
	}
}
