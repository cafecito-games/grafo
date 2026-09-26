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
