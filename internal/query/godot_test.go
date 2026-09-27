package query_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestGodotCompositionReportsInstancesScriptsAndAutoloads(t *testing.T) {
	scene := godotNode(graph.KindGodotScene, "scenes/main", nil)
	enemy := godotNode(graph.KindGodotScene, "scenes/enemy", nil)
	root := godotNode(graph.KindGodotSceneNode, "scenes/main:Main", map[string]string{"scene": "scenes/main"})
	child := godotNode(graph.KindGodotSceneNode, "scenes/main:Main/Enemy", map[string]string{"scene": "scenes/main"})
	script := godotNode(graph.KindModule, "scripts/player", nil)
	autoload := godotNode(graph.KindGodotAutoload, "godot:autoload:Game", nil)
	game := godotNode(graph.KindModule, "scripts/game", nil)
	nodes := map[string]graph.Node{scene.ID: scene, enemy.ID: enemy, root.ID: root, child.ID: child,
		script.ID: script, autoload.ID: autoload, game.ID: game}
	repository := &fakeRepository{nodes: nodes, edges: []graph.Edge{
		{ID: "e-declares-root", FromID: scene.ID, ToID: root.ID, Kind: graph.EdgeDeclares},
		{ID: "e-declares-child", FromID: root.ID, ToID: child.ID, Kind: graph.EdgeDeclares},
		{ID: "e-instance", FromID: child.ID, ToID: enemy.ID, Kind: graph.EdgeInstantiates,
			Properties: map[string]string{"form": "nested", "resource": "res://scenes/enemy.tscn"}},
		{ID: "e-script", FromID: root.ID, ToID: script.ID, Kind: graph.EdgeAttachesScript},
		{ID: "e-autoload", FromID: autoload.ID, ToID: game.ID, Kind: graph.EdgeAutoloads},
	}}
	service := query.NewService(repository)

	report, err := service.GodotComposition(context.Background(), "scenes/main", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SceneNodes) != 2 {
		t.Fatalf("scene nodes = %#v", report.SceneNodes)
	}
	if len(report.OutboundInstances) != 1 || report.OutboundInstances[0].Node.QualifiedName != "scenes/enemy" ||
		report.OutboundInstances[0].Via == nil || report.OutboundInstances[0].Via.QualifiedName != "scenes/main:Main/Enemy" {
		t.Fatalf("outbound instances = %#v", report.OutboundInstances)
	}
	if len(report.AttachedScripts) != 1 || report.AttachedScripts[0].Node.QualifiedName != "scripts/player" {
		t.Fatalf("attached scripts = %#v", report.AttachedScripts)
	}
	again, err := service.GodotComposition(context.Background(), "scenes/main", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report, again) {
		t.Fatal("composition report is not deterministic")
	}

	inbound, err := service.GodotComposition(context.Background(), "scenes/enemy", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbound.InboundInstances) != 1 {
		t.Fatalf("inbound instances = %#v", inbound.InboundInstances)
	}
	if inbound.InboundInstances[0].Node.QualifiedName != "scenes/main" ||
		inbound.InboundInstances[0].Via == nil ||
		inbound.InboundInstances[0].Via.QualifiedName != "scenes/main:Main/Enemy" {
		t.Fatalf("inbound instance did not resolve its owning scene: %#v", inbound.InboundInstances[0])
	}

	exposure, err := service.GodotComposition(context.Background(), "scripts/game", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(exposure.AutoloadExposures) != 1 || exposure.AutoloadExposures[0].Node.QualifiedName != "godot:autoload:Game" {
		t.Fatalf("autoload exposures = %#v", exposure.AutoloadExposures)
	}
	target, err := service.GodotComposition(context.Background(), "godot:autoload:Game", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(target.AutoloadTargets) != 1 || target.AutoloadTargets[0].Node.QualifiedName != "scripts/game" {
		t.Fatalf("autoload targets = %#v", target.AutoloadTargets)
	}
}

func godotNode(kind graph.NodeKind, qualified string, properties map[string]string) graph.Node {
	return graph.Node{ID: graph.NodeID(kind, qualified), Kind: kind, Name: graph.SimpleName(qualified),
		QualifiedName: qualified, Properties: properties}
}
