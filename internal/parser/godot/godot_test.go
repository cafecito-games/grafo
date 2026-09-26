package godot_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
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
	assertNode(t, result.Nodes, graph.KindModule, "scenes/main")
	assertNode(t, result.Nodes, graph.KindVariable, "scenes/main#Style_button")
	player := assertNode(t, result.Nodes, graph.KindVariable, "scenes/main:Player")
	button := assertNode(t, result.Nodes, graph.KindVariable, "scenes/main:Player/Button")
	if button.Properties["unique_name_in_owner"] != "true" {
		t.Fatalf("scene node unique_name_in_owner = %q", button.Properties["unique_name_in_owner"])
	}
	assertNode(t, result.Nodes, graph.KindField, "scenes/main:Player.script")
	assertNode(t, result.Nodes, graph.KindEvent, "scenes/main:Player/Button.pressed")
	assertFactTarget(t, result.Facts, graph.EdgeImports, "scripts/player")
	assertFactTarget(t, result.Facts, graph.EdgeReferences, "scripts/player")
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
