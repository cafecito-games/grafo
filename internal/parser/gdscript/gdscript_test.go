package gdscript_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
)

func TestParserExtractsGodotSymbolsAndWiring(t *testing.T) {
	content := []byte(`class_name Player extends CharacterBody2D

signal health_changed(value: int)
const Enemy = preload('res://actors/enemy.gd')
const EnemyUID = preload('uid://enemy123')
@export var speed: float = 10.0
var health: int = 100

func take_damage(amount: int) -> int:
	var applied: int = amount
	var enemy: Enemy = Enemy.new()
	health = applied
	health_changed.emit(health)
	health_changed.connect(on_health_changed)
	enemy.attack(applied)
	var mode = OS.get_environment("GAME_MODE")
	return applied
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "actors/player.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindClass, "Player")
	assertHasNode(t, result.Nodes, graph.KindMethod, "take_damage")
	assertHasNode(t, result.Nodes, graph.KindParameter, "amount")
	assertHasNode(t, result.Nodes, graph.KindField, "speed")
	assertHasNode(t, result.Nodes, graph.KindField, "health")
	assertHasNode(t, result.Nodes, graph.KindVariable, "applied")
	assertHasNode(t, result.Nodes, graph.KindEvent, "health_changed")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "CharacterBody2D")
	assertHasFact(t, result.Facts, graph.EdgeImports, "actors/enemy")
	assertHasFact(t, result.Facts, graph.EdgeImports, "uid://enemy123")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "actors/enemy.attack")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "GAME_MODE")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePasses)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
	assertHasFactKind(t, result.Facts, graph.EdgePublishes)
	assertHasFactKind(t, result.Facts, graph.EdgeSubscribes)
}

func TestParserCreatesImplicitScriptClassAndInnerTypes(t *testing.T) {
	content := []byte(`extends Node

enum State { IDLE, RUNNING = 2 }

class Helper extends RefCounted:
	var enabled: bool
	func run() -> void:
		pass

func ready() -> void:
	for child: Node in get_children():
		child.queue_free()
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/controller.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasQualifiedNode(t, result.Nodes, graph.KindClass, "scripts/controller")
	assertHasQualifiedNode(t, result.Nodes, graph.KindClass, "scripts/controller.Helper")
	assertHasNode(t, result.Nodes, graph.KindType, "State")
	assertHasNode(t, result.Nodes, graph.KindVariable, "child")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "Node")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "RefCounted")
}

func TestParserResolvesMembersDeclaredLater(t *testing.T) {
	content := []byte(`class_name Ordered

func start(value: int) -> void:
	later = value
	finished.emit(value)
	finish()

var later: int
signal finished(value: int)
func finish() -> void:
	pass
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "ordered.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "Ordered.finish")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePublishes)
}

func TestParserReturnsPositionedSyntaxErrors(t *testing.T) {
	_, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "broken.gd", Content: []byte("func broken(\n"),
	})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "broken.gd:") {
		t.Fatalf("error does not contain filename and position: %v", err)
	}
}

func TestParserSupportsGDScriptFilesCaseInsensitively(t *testing.T) {
	parser := gdscriptparser.New()
	if !parser.Supports("player.gd") || !parser.Supports("PLAYER.GD") || parser.Supports("player.gdshader") {
		t.Fatal("unexpected GDScript extension support")
	}
}

func TestParserLinksProjectSettingsAndInputActions(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/settings.gd", RepoID: "repo:sample", Content: []byte(`class_name Settings

func load_main_scene() -> String:
	if Input.is_action_just_pressed("jump"):
		return "jump"
	return ProjectSettings.get_setting("application/run/main_scene")
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "application/run/main_scene")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "input/jump")
}

func TestParserLinksNodePathAndRuntimeNodeLookups(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/menu.gd", RepoID: "repo:sample", Content: []byte(`class_name Menu

func bind_nodes() -> void:
	var unique_button = %StartButton
	var status = $Panel/Status
	get_node("Panel/StartButton")
	get_node_or_null(^"Panel/Optional")
	has_node("Panel/Status:visible")
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "StartButton", "unique", "true")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "StartButton", "lookup", "get_node")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "Status", "form", "node_path")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "Optional", "lookup", "get_node_or_null")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "Status", "lookup", "has_node")
}

func assertHasFactKind(t *testing.T, facts []graph.Fact, kind graph.EdgeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s fact; got %#v", kind, facts)
}

func assertHasNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, name string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.Name == name {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, name, nodes)
}

func assertHasQualifiedNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, qualified, nodes)
}

func assertHasFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
}

func assertHasFactWithProperty(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target, key, value string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target && fact.Properties[key] == value {
			return
		}
	}
	t.Fatalf("missing %s fact to %q with %s=%q; got %#v", kind, target, key, value, facts)
}
