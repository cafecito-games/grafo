package query_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

// interactionGraph builds one Godot project wired the way a real one is: a scene
// whose node joins a group and connects a signal declaratively, a script that
// reads an input action and dispatches to the same group, and one unresolved
// group nobody declared.
func interactionGraph() (*fakeRepository, map[string]graph.Node) {
	scene := godotNode(graph.KindGodotScene, "scenes/arena", nil)
	sceneNode := godotNode(graph.KindGodotSceneNode, "scenes/arena:Arena/Enemy",
		map[string]string{"scene": "scenes/arena"})
	action := godotNode(graph.KindGodotInputAction, "godot:input_action:project.godot:jump", nil)
	group := godotNode(graph.KindGodotNodeGroup, "godot:node_group:project.godot:enemies", nil)
	missing := godotNode(graph.KindGodotNodeGroup, "godot:node_group:project.godot:ghosts", nil)
	missing.External = true
	method := godotNode(graph.KindMethod, "Hud.poll", nil)
	event := godotNode(graph.KindEvent, "Hud.ready_changed", nil)
	configKey := godotNode(graph.KindConfigKey, "config:project.godot:input/jump", nil)
	nodes := map[string]graph.Node{scene.ID: scene, sceneNode.ID: sceneNode, action.ID: action,
		group.ID: group, missing.ID: missing, method.ID: method, event.ID: event, configKey.ID: configKey}
	edges := []graph.Edge{
		{ID: "e-declares", FromID: scene.ID, ToID: sceneNode.ID, Kind: graph.EdgeDeclares},
		{ID: "e-member", FromID: sceneNode.ID, ToID: group.ID, Kind: graph.EdgeInGroup,
			Properties: map[string]string{"form": "declared", "group": "enemies"}},
		{ID: "e-subscribes", FromID: sceneNode.ID, ToID: event.ID, Kind: graph.EdgeSubscribes,
			Properties: map[string]string{"form": "connect", "declared": "true"}},
		{ID: "e-action", FromID: method.ID, ToID: action.ID, Kind: graph.EdgeUsesInputAction,
			Properties: map[string]string{"form": "query", "action": "jump"}},
		{ID: "e-dispatch", FromID: method.ID, ToID: group.ID, Kind: graph.EdgeUsesGroup,
			Properties: map[string]string{"form": "call", "group": "enemies", "method": "die"}},
		{ID: "e-missing", FromID: method.ID, ToID: missing.ID, Kind: graph.EdgeUsesGroup,
			Properties: map[string]string{"form": "lookup", "group": "ghosts"}},
		{ID: "e-defines", FromID: configKey.ID, ToID: action.ID, Kind: graph.EdgeDefines},
		// A generic configuration read of the same key must not show up as an
		// action interaction: the far side is a configuration key, not an action.
		{ID: "e-reads", FromID: method.ID, ToID: configKey.ID, Kind: graph.EdgeReadsConfig},
	}
	return &fakeRepository{nodes: nodes, edges: edges}, nodes
}

// TestGodotInteractionsUnifiesActionsGroupsAndSignals covers the one report the
// issue asks for: a scene's declarative wiring and a script's wiring read the same
// way, every interaction keeps its operation form, and an unresolved far side is
// counted rather than hidden.
func TestGodotInteractionsUnifiesActionsGroupsAndSignals(t *testing.T) {
	repository, _ := interactionGraph()
	service := query.NewService(repository)
	ctx := context.Background()

	scene, err := service.GodotInteractions(ctx, "scenes/arena", query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.SceneNodes) != 1 {
		t.Fatalf("scene nodes = %#v", scene.SceneNodes)
	}
	// The scene's own wiring includes the wiring its nodes declare, and the
	// scene node stays as the evidence that carried it.
	found := map[query.GodotInteractionCategory]string{}
	for _, interaction := range scene.Outbound {
		if interaction.Via == nil || interaction.Via.QualifiedName != "scenes/arena:Arena/Enemy" {
			t.Fatalf("scene interaction lost its scene-node evidence: %#v", interaction)
		}
		found[interaction.Category] = interaction.Form
	}
	if found[query.GodotGroupInteraction] != "declared" || found[query.GodotSignalInteraction] != "connect" {
		t.Fatalf("scene outbound interactions = %#v", scene.Outbound)
	}

	script, err := service.GodotInteractions(ctx, "Hud.poll", query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(script.Outbound) != 3 {
		t.Fatalf("script outbound interactions = %#v", script.Outbound)
	}
	if script.Unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", script.Unresolved)
	}
	for _, interaction := range script.Outbound {
		if interaction.Edge.Kind == graph.EdgeReadsConfig {
			t.Fatalf("a generic configuration read is not an interaction: %#v", interaction)
		}
	}
	again, err := service.GodotInteractions(ctx, "Hud.poll", query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(script, again) {
		t.Fatal("interactions report is not deterministic")
	}

	// Inbound from the group answers "who is in this group and who dispatches
	// to it", and a scene node's membership is attributed to its scene.
	group, err := service.GodotInteractions(ctx, "godot:node_group:project.godot:enemies",
		query.GodotInteractionsOptions{Direction: query.Incoming})
	if err != nil {
		t.Fatal(err)
	}
	if len(group.Outbound) != 0 || len(group.Inbound) != 2 {
		t.Fatalf("group report = %#v", group)
	}
	sceneSide := false
	for _, interaction := range group.Inbound {
		if interaction.Node.QualifiedName == "scenes/arena" && interaction.Via != nil &&
			interaction.Via.QualifiedName == "scenes/arena:Arena/Enemy" {
			sceneSide = true
		}
	}
	if !sceneSide {
		t.Fatalf("a scene node's membership must be attributed to its scene: %#v", group.Inbound)
	}
}

// TestGodotInteractionsFiltersByCategoryAndDirection pins the filter contract: a
// named category narrows the report, an unknown one is an error rather than a
// filter that can never match, and a direction reports only that side.
func TestGodotInteractionsFiltersByCategoryAndDirection(t *testing.T) {
	repository, _ := interactionGraph()
	service := query.NewService(repository)
	ctx := context.Background()

	actions, err := service.GodotInteractions(ctx, "Hud.poll", query.GodotInteractionsOptions{
		Categories: []query.GodotInteractionCategory{query.GodotActionInteraction}})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions.Outbound) != 1 || actions.Outbound[0].Category != query.GodotActionInteraction {
		t.Fatalf("action filter = %#v", actions.Outbound)
	}

	outbound, err := service.GodotInteractions(ctx, "godot:input_action:project.godot:jump",
		query.GodotInteractionsOptions{Direction: query.Outgoing})
	if err != nil {
		t.Fatal(err)
	}
	if len(outbound.Inbound) != 0 {
		t.Fatalf("an outgoing report must not include inbound interactions: %#v", outbound.Inbound)
	}
	inbound, err := service.GodotInteractions(ctx, "godot:input_action:project.godot:jump",
		query.GodotInteractionsOptions{Direction: query.Incoming})
	if err != nil {
		t.Fatal(err)
	}
	// The declaration and the use both point at the action.
	if len(inbound.Inbound) != 2 {
		t.Fatalf("action inbound = %#v", inbound.Inbound)
	}

	if _, err := query.ParseGodotInteractionCategory("groups"); err == nil {
		t.Fatal("an unknown filter must be an error, not a filter that matches nothing")
	}
	for _, value := range []string{"", "action", "GROUP", " signal "} {
		if _, err := query.ParseGodotInteractionCategory(value); err != nil {
			t.Fatalf("ParseGodotInteractionCategory(%q) = %v", value, err)
		}
	}
}
