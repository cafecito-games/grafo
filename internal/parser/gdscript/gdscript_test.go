package gdscript_test

import (
	"context"
	"os"
	"path/filepath"
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

func TestParserDeclaresScriptModuleForResourceIdentity(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/player.gd", Content: []byte("class_name Player extends Node\n"),
		Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	module := findQualifiedNode(t, result.Nodes, graph.KindModule, "scripts/player")
	if module.Properties["form"] != "script" {
		t.Fatalf("script module properties = %#v", module.Properties)
	}
	class := findQualifiedNode(t, result.Nodes, graph.KindClass, "Player")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeDeclares && fact.FromID == module.ID && fact.TargetID == class.ID {
			return
		}
	}
	t.Fatalf("script module does not declare its class; got %#v", result.Facts)
}

func TestParserResolvesAutoloadUsesFromProjectDeclarations(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "client/project.godot", `config_version=5

[autoload]
Game="*res://scripts/game.gd"
Menu="*res://scenes/menu.tscn"
Disabled="res://scripts/disabled.gd"
Twice="*res://scripts/a.gd"
Twice="*res://scripts/b.gd"
`)
	content := []byte(`extends Node

func ready() -> void:
	Game.start()
	var scene = Menu
	Disabled.start()
	Twice.start()
	Unknown.start()
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/hud.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	call := findFactWithTarget(t, result.Facts, graph.EdgeReferences, "godot:autoload:client/project.godot:Game")
	if call.TargetKind != graph.KindGodotAutoload || call.Properties["form"] != "autoload_call" ||
		call.Properties["member"] != "start" {
		t.Fatalf("autoload call fact = %#v", call)
	}
	reference := findFactWithTarget(t, result.Facts, graph.EdgeReferences, "godot:autoload:client/project.godot:Menu")
	if reference.Properties["form"] != "autoload_reference" {
		t.Fatalf("autoload reference fact = %#v", reference)
	}
	// A conflicting declaration, an undeclared name, and an autoload Godot does
	// not expose as a global singleton must all resolve nothing.
	for _, name := range []string{"Twice", "Unknown", "Disabled"} {
		for _, fact := range result.Facts {
			if strings.HasSuffix(fact.Target, ":"+name) && strings.HasPrefix(fact.Target, "godot:autoload:") {
				t.Fatalf("autoload %q must not resolve: %#v", name, fact)
			}
		}
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findQualifiedNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return node
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, qualified, nodes)
	return graph.Node{}
}

func findFactWithTarget(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) graph.Fact {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return fact
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
	return graph.Fact{}
}

// TestParserKeepsProjectEscapingPreloadsUnresolved covers the project boundary on
// the script side: a preload that traverses out of its own project must not
// resolve into a sibling project.
func TestParserKeepsProjectEscapingPreloadsUnresolved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "client/project.godot", "config_version=5\n")
	writeFile(t, root, "tools/probe/project.godot", "config_version=5\n")

	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/hud.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte("extends Node\n" +
			"const Inside = preload('res://scripts/inside.gd')\n" +
			"const Outside = preload('res://../tools/probe/scripts/probe.gd')\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeImports, "client/scripts/inside")
	for _, fact := range result.Facts {
		if strings.HasPrefix(fact.Target, "tools/probe") {
			t.Fatalf("a preload escaped its project: %#v", fact)
		}
		if fact.Kind == graph.EdgeImports && fact.Target == "" {
			t.Fatalf("an unresolvable preload emitted an empty target: %#v", fact)
		}
	}
}

// TestParserLinksLiteralInputActionUses covers the typed action vocabulary: a
// literal action name on a recognized action API resolves to the project-scoped
// action, a computed name resolves nothing, and a literal argument that is not an
// action name on an Input call is no longer mistaken for one.
func TestParserLinksLiteralInputActionUses(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "client/project.godot", "config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/hud.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`extends Node

const ACTION = "crouch"

func poll(event: InputEvent) -> void:
	if Input.is_action_just_pressed("jump"):
		pass
	var strength = Input.get_action_strength("attack")
	var lean = Input.get_axis("lean_left", "lean_right")
	if event.is_action_pressed("cancel"):
		pass
	if InputMap.has_action(ACTION):
		pass
	Input.set_custom_mouse_cursor("res://art/cursor.png")
	if is_action_bar_visible():
		pass

func is_action_bar_visible() -> bool:
	return true
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ action, form string }{
		{action: "jump", form: "query"},
		{action: "attack", form: "query"},
		{action: "lean_left", form: "query"},
		{action: "lean_right", form: "query"},
		{action: "cancel", form: "query"},
	} {
		target := "godot:input_action:client/project.godot:" + testCase.action
		fact := findFactWithTarget(t, result.Facts, graph.EdgeUsesInputAction, target)
		if fact.TargetKind != graph.KindGodotInputAction || fact.Properties["form"] != testCase.form ||
			fact.Properties["action"] != testCase.action {
			t.Fatalf("input action fact = %#v", fact)
		}
		// The generic configuration reader stays, so config catalogs keep the
		// same evidence they had before actions became their own kind.
		assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "input/"+testCase.action)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeUsesInputAction && fact.Target == "" {
			t.Fatalf("an action use emitted an empty target: %#v", fact)
		}
		if fact.Kind != graph.EdgeUsesInputAction {
			continue
		}
		switch fact.Properties["action"] {
		case "crouch":
			t.Fatalf("a computed action name must resolve nothing: %#v", fact)
		case "res://art/cursor.png":
			t.Fatalf("a non-action argument must not become an action: %#v", fact)
		}
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeReadsConfig && strings.HasPrefix(fact.Target, "input/res://") {
			t.Fatalf("a cursor path must not be read as an input action: %#v", fact)
		}
	}
}

// TestParserLinksLiteralGroupOperations covers the node-group vocabulary:
// membership, membership tests, lookups, and dispatches each keep their operation
// form, a dispatch records its method as evidence without inventing a handler, and
// a computed group name resolves nothing.
func TestParserLinksLiteralGroupOperations(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "client/project.godot", "config_version=5\n\n[global_group]\nenemies=\"Hostile\"\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/spawner.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`extends Node

const OPT_OUT = "opted_out"

func wire(other: Node) -> void:
	add_to_group("enemies")
	other.add_to_group("damageable")
	other.remove_from_group("damageable")
	if other.is_in_group("enemies"):
		pass
	var all = get_tree().get_nodes_in_group("enemies")
	get_tree().call_group("enemies", "die")
	get_tree().notify_group("enemies", 1)
	other.add_to_group(OPT_OUT)
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	enemies := "godot:node_group:client/project.godot:enemies"
	damageable := "godot:node_group:client/project.godot:damageable"
	for _, testCase := range []struct {
		kind   graph.EdgeKind
		target string
		form   string
	}{
		{kind: graph.EdgeInGroup, target: enemies, form: "add"},
		{kind: graph.EdgeInGroup, target: damageable, form: "add"},
		{kind: graph.EdgeInGroup, target: damageable, form: "remove"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "membership_test"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "lookup"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "call"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "notify"},
	} {
		assertHasFactWithProperty(t, result.Facts, testCase.kind, testCase.target, "form", testCase.form)
	}
	// A membership test is never membership evidence.
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeInGroup && fact.Properties["form"] == "membership_test" {
			t.Fatalf("is_in_group must not assert membership: %#v", fact)
		}
		if fact.Kind == graph.EdgeUsesGroup || fact.Kind == graph.EdgeInGroup {
			if fact.Target == "" {
				t.Fatalf("a group operation emitted an empty target: %#v", fact)
			}
			if fact.TargetKind != graph.KindGodotNodeGroup {
				t.Fatalf("a group operation must target a node group: %#v", fact)
			}
			if fact.Properties["group"] == "" {
				t.Fatalf("a group operation is missing its name evidence: %#v", fact)
			}
		}
		if strings.HasSuffix(fact.Target, ":opted_out") {
			t.Fatalf("a computed group name must resolve nothing: %#v", fact)
		}
	}
	// A group dispatch keeps its literal method as evidence and never becomes a
	// call edge to a guessed receiver.
	assertHasFactWithProperty(t, result.Facts, graph.EdgeUsesGroup, enemies, "method", "die")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "die" {
			t.Fatalf("a group dispatch must not guess a handler: %#v", fact)
		}
	}
}

// TestParserRoutesLiteralSignalConnections covers signal routing from scripts:
// connect subscribes and names its handler, emit publishes, and disconnect and
// is_connected are routing evidence that never become subscriptions.
func TestParserRoutesLiteralSignalConnections(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/hud.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Hud extends Node

signal ready_changed(value: bool)

func wire() -> void:
	ready_changed.connect(on_ready_changed)
	ready_changed.emit(true)
	if ready_changed.is_connected(on_ready_changed):
		ready_changed.disconnect(on_ready_changed)
	emit_signal("ready_changed", false)

func on_ready_changed(_value: bool) -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	event := findQualifiedNode(t, result.Nodes, graph.KindEvent, "Hud.ready_changed")
	handled := false
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeHandledBy && fact.FromID == event.ID &&
			fact.Target == "Hud.on_ready_changed" && fact.Properties["form"] == "connect" {
			handled = true
		}
		if fact.Kind == graph.EdgeSubscribes && fact.Properties["form"] != "connect" {
			t.Fatalf("only a connect may subscribe: %#v", fact)
		}
	}
	if !handled {
		t.Fatalf("a literal connect must name its handler; got %#v", result.Facts)
	}
	// The route's destination is recorded on the routing fact as well, which is
	// the only place it can live when the signal's owner is another file.
	subscribed := false
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeSubscribes && fact.Properties["handler"] == "Hud.on_ready_changed" {
			subscribed = true
		}
	}
	if !subscribed {
		t.Fatalf("a connect must record its handler as evidence; got %#v", result.Facts)
	}
	for _, form := range []string{"signal_disconnect", "signal_connection_test"} {
		found := false
		for _, fact := range result.Facts {
			if fact.Kind == graph.EdgeReferences && fact.TargetID == event.ID &&
				fact.Properties["form"] == form {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s evidence; got %#v", form, result.Facts)
		}
	}
	publishes := 0
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgePublishes && fact.TargetID == event.ID {
			publishes++
			if fact.Properties["form"] != "emit" {
				t.Fatalf("a publish must record its form: %#v", fact)
			}
		}
	}
	if publishes != 2 {
		t.Fatalf("expected both emit forms to publish, got %d", publishes)
	}
}

// TestParserRecordsHandlerWhenSignalOwnerIsAnotherFile is the case the smoke run
// exposed: nearly every real connect names a signal another file declares, so no
// handled_by edge can be hung on a declaration this parser cannot see. The route
// must still record where it goes, and it must not invent an owner for the signal.
func TestParserRecordsHandlerWhenSignalOwnerIsAnotherFile(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/kit.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Kit extends Node

var _backend: Backend

func wire() -> void:
	_backend.sign_in_success.connect(_on_sign_in)
	_backend.sign_in_failed.connect(func(): pass)

func _on_sign_in() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	routed := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_success")
	if routed.Properties["form"] != "connect" || routed.Properties["handler"] != "Kit._on_sign_in" {
		t.Fatalf("cross-file connect fact = %#v", routed)
	}
	lambda := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_failed")
	if _, ok := lambda.Properties["handler"]; ok {
		t.Fatalf("a lambda proves no method; got %#v", lambda)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeHandledBy {
			t.Fatalf("a signal this script does not declare must not gain a handler edge: %#v", fact)
		}
	}
}

// TestParserDoesNotReadANonSignalLiteralAsASignalName covers the ambiguity a
// project can create for itself: a connect(url) of its own must not name an event
// after its argument, and a disconnect on an object whose type is unknown must not
// invent a signal either.
func TestParserDoesNotReadANonSignalLiteralAsASignalName(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/socket.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Socket extends Node

var _client: WebSocketPeer

func open() -> void:
	_client.connect("wss://example.invalid/stream")
	inherited_signal.emit(42)
	inherited_signal.connect(_on_inherited)

func _on_inherited() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.TargetKind == graph.KindEvent && strings.Contains(fact.Target, "://") {
			t.Fatalf("a URL must never become a signal: %#v", fact)
		}
	}
	// Nor does the receiver become one: a literal that can be neither a Callable
	// nor a signal name is proof this is not a signal connection, so the call
	// stays an ordinary call.
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeSubscribes && fact.Target != "inherited_signal" {
			t.Fatalf("a connect(url) must not subscribe to anything: %#v", fact)
		}
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "WebSocketPeer.connect")
	// A signal a base class declares is emitted and connected by its bare name.
	// It cannot be proved here, so it stays an unresolved event that the graph
	// resolves by name only when exactly one declaration owns it.
	assertHasFactWithProperty(t, result.Facts, graph.EdgePublishes, "inherited_signal", "form", "emit")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeSubscribes, "inherited_signal", "handler",
		"Socket._on_inherited")
}

// TestParserKeepsLocallyDeclaredActionMethodsOutOfTheVocabulary covers both
// spellings of a call on this object. A script is free to declare its own
// is_action_pressed, and neither the bare nor the self-qualified call to it
// involves a Godot input API, so neither may produce a typed action edge.
func TestParserKeepsLocallyDeclaredActionMethodsOutOfTheVocabulary(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "client/project.godot", "config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/pane.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Pane extends Control

func poll() -> void:
	if is_action_pressed("jump"):
		pass
	if self.is_action_pressed("jump"):
		pass
	if Pane.is_action_pressed("jump"):
		pass
	if add_to_group("enemies"):
		pass
	if self.add_to_group("enemies"):
		pass

func is_action_pressed(_name: String) -> bool:
	return false

func add_to_group(_name: String) -> bool:
	return false
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		switch fact.Kind {
		case graph.EdgeUsesInputAction, graph.EdgeInGroup, graph.EdgeUsesGroup:
			t.Fatalf("a locally declared method must not produce a Godot interaction: %#v", fact)
		case graph.EdgeReadsConfig:
			if strings.HasPrefix(fact.Target, "input/") {
				t.Fatalf("a locally declared method must not read an input action: %#v", fact)
			}
		}
	}
}

// TestParserResolvesActionQueriesOnInputEventReceivers covers the InputEvent
// action-query methods. is_action is an InputEvent method rather than an Input or
// InputMap one, so requiring an Input receiver discarded correct code.
func TestParserResolvesActionQueriesOnInputEventReceivers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "client/project.godot", "config_version=5\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/input.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`extends Node

func _input(event: InputEvent) -> void:
	if event.is_action("ui_accept"):
		pass
	if event.is_action_pressed("ui_cancel"):
		pass
	if event.is_action_released("ui_left"):
		pass
	var strength = event.get_action_strength("ui_right")
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"ui_accept", "ui_cancel", "ui_left", "ui_right"} {
		fact := findFactWithTarget(t, result.Facts, graph.EdgeUsesInputAction,
			"godot:input_action:client/project.godot:"+action)
		if fact.Properties["form"] != "query" || fact.Properties["receiver"] != "InputEvent" {
			t.Fatalf("InputEvent action query fact = %#v", fact)
		}
		assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "input/"+action)
	}
}

// TestParserSourcesCrossFileConnectionsAtTheirHandler is the traversal that was
// missing: a connect to a signal another file declares can still be walked from
// the resolved signal back to the method that handles it, because the route is
// sourced at the handler this script declares rather than at the statement that
// wired it. A handled_by edge cannot express this - its source would have to be
// the foreign declaration, and a fact resolves only its target by name.
func TestParserSourcesCrossFileConnectionsAtTheirHandler(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/kit.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Kit extends Node

var _backend: Backend

func wire() -> void:
	_backend.sign_in_success.connect(_on_sign_in)
	_backend.sign_in_failed.connect(func(): pass)

func _on_sign_in() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Kit._on_sign_in")
	routed := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_success")
	if routed.FromID != handler.ID {
		t.Fatalf("a resolved handler must source the route: %#v", routed)
	}
	if routed.Properties["handler"] != "Kit._on_sign_in" || routed.Properties["site"] != "Kit.wire" {
		t.Fatalf("the route lost its handler or its call site: %#v", routed)
	}
	// A lambda proves no method, so the route stays at the wiring statement.
	wire := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Kit.wire")
	lambda := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_failed")
	if lambda.FromID != wire.ID {
		t.Fatalf("an unprovable handler must leave the route at its call site: %#v", lambda)
	}
	if _, ok := lambda.Properties["handler"]; ok {
		t.Fatalf("a lambda proves no method; got %#v", lambda)
	}
}
