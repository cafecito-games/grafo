package query_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
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

// TestGodotCompositionResolvesResourcePathSelectors pins the defect this package
// used to have: a Godot resource is identified by its path with the extension
// dropped, so the two forms a caller actually holds - the path as the repository
// tracks it, and the res:// path as the project writes it - resolved to nothing.
func TestGodotCompositionResolvesResourcePathSelectors(t *testing.T) {
	project := godotNodeAt(graph.KindFile, "client/project.godot", "client/project.godot", nil)
	scene := godotNodeAt(graph.KindGodotScene, "client/screens/game", "client/screens/game.tscn", nil)
	script := godotNodeAt(graph.KindModule, "client/screens/game", "client/screens/game.gd", nil)
	sceneNode := godotNode(graph.KindGodotSceneNode, "client/screens/game:Game",
		map[string]string{"scene": "client/screens/game"})
	child := godotNodeAt(graph.KindGodotScene, "client/screens/hud", "client/screens/hud.tscn", nil)
	repository := godotProjects(&fakeRepository{
		nodes: nodeSet(project, scene, script, sceneNode, child),
		edges: []graph.Edge{
			{ID: "e-declares-node", FromID: scene.ID, ToID: sceneNode.ID, Kind: graph.EdgeDeclares},
			{ID: "e-instance", FromID: sceneNode.ID, ToID: child.ID, Kind: graph.EdgeInstantiates},
			{ID: "e-script", FromID: sceneNode.ID, ToID: script.ID, Kind: graph.EdgeAttachesScript},
		},
	})
	service := query.NewService(repository)

	for _, selector := range []string{
		"client/screens/game.tscn",
		"res://screens/game.tscn",
	} {
		report, err := service.GodotComposition(context.Background(), selector,
			query.GodotCompositionOptions{Kind: graph.KindGodotScene})
		if err != nil {
			t.Fatalf("%s: %v", selector, err)
		}
		if report.Root.ID != scene.ID {
			t.Fatalf("%s resolved %s [%s]", selector, report.Root.QualifiedName, report.Root.Kind)
		}
		if len(report.OutboundInstances) != 1 || len(report.AttachedScripts) != 1 {
			t.Fatalf("%s: instances = %#v scripts = %#v", selector, report.OutboundInstances, report.AttachedScripts)
		}
	}

	// The extension is the evidence the shared identity dropped, so it names the
	// scene rather than tying with the same-named script.
	scriptReport, err := service.GodotComposition(context.Background(), "client/screens/game.gd",
		query.GodotCompositionOptions{Kind: graph.KindModule})
	if err != nil {
		t.Fatal(err)
	}
	if scriptReport.Root.ID != script.ID {
		t.Fatalf("script selector resolved %s [%s]", scriptReport.Root.QualifiedName, scriptReport.Root.Kind)
	}

	// A selector that is not a Godot resource path keeps the resolution error it
	// always had rather than being read as one.
	if _, err := service.GodotComposition(context.Background(), "internal/missing.go",
		query.GodotCompositionOptions{}); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("non-Godot path selector error = %v", err)
	}
}

// TestGodotCompositionRefusesAmbiguousResourcePaths pins that a res:// selector
// reachable in more than one indexed project is refused rather than resolved
// into whichever project happens to come first, and that a reference escaping
// its project resolves nothing.
func TestGodotCompositionRefusesAmbiguousResourcePaths(t *testing.T) {
	first := godotNodeAt(graph.KindFile, "client/project.godot", "client/project.godot", nil)
	second := godotNodeAt(graph.KindFile, "server/project.godot", "server/project.godot", nil)
	left := godotNodeAt(graph.KindGodotScene, "client/screens/game", "client/screens/game.tscn", nil)
	right := godotNodeAt(graph.KindGodotScene, "server/screens/game", "server/screens/game.tscn", nil)
	service := query.NewService(godotProjects(&fakeRepository{
		nodes: nodeSet(first, second, left, right),
	}))

	_, err := service.GodotComposition(context.Background(), "res://screens/game.tscn",
		query.GodotCompositionOptions{Kind: graph.KindGodotScene})
	var ambiguous *query.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Fatalf("candidates = %#v", ambiguous.Candidates)
	}

	if _, err := service.GodotComposition(context.Background(), "res://../outside/game.tscn",
		query.GodotCompositionOptions{Kind: graph.KindGodotScene}); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("escaping selector error = %v", err)
	}
}

// TestGodotCompositionAggregatesContainerRoots pins the defect where naming a
// tracked file resolved it and then reported nothing: a file owns no Godot
// evidence of its own, so every fact about it hangs off what it declares.
func TestGodotCompositionAggregatesContainerRoots(t *testing.T) {
	sceneFile := godotNodeAt(graph.KindFile, "client/screens/game.tscn", "client/screens/game.tscn", nil)
	scene := godotNodeAt(graph.KindGodotScene, "client/screens/game", "client/screens/game.tscn", nil)
	sceneNode := godotNode(graph.KindGodotSceneNode, "client/screens/game:Game",
		map[string]string{"scene": "client/screens/game"})
	scriptFile := godotNodeAt(graph.KindFile, "client/screens/game.gd", "client/screens/game.gd", nil)
	script := godotNodeAt(graph.KindModule, "client/screens/game", "client/screens/game.gd", nil)
	child := godotNodeAt(graph.KindGodotScene, "client/screens/hud", "client/screens/hud.tscn", nil)
	projectFile := godotNodeAt(graph.KindFile, "client/project.godot", "client/project.godot", nil)
	autoload := godotNode(graph.KindGodotAutoload, "godot:autoload:client/project.godot:Game", nil)
	repository := godotProjects(&fakeRepository{
		nodes: nodeSet(sceneFile, scene, sceneNode, scriptFile, script, child, projectFile, autoload),
		edges: []graph.Edge{
			{ID: "e-file-scene", FromID: sceneFile.ID, ToID: scene.ID, Kind: graph.EdgeDeclares},
			{ID: "e-scene-node", FromID: scene.ID, ToID: sceneNode.ID, Kind: graph.EdgeDeclares},
			{ID: "e-instance", FromID: sceneNode.ID, ToID: child.ID, Kind: graph.EdgeInstantiates},
			{ID: "e-script", FromID: sceneNode.ID, ToID: script.ID, Kind: graph.EdgeAttachesScript},
			{ID: "e-file-script", FromID: scriptFile.ID, ToID: script.ID, Kind: graph.EdgeDeclares},
			{ID: "e-file-autoload", FromID: projectFile.ID, ToID: autoload.ID, Kind: graph.EdgeDeclares},
			{ID: "e-autoload", FromID: autoload.ID, ToID: script.ID, Kind: graph.EdgeAutoloads},
		},
	})
	service := query.NewService(repository)

	sceneReport, err := service.GodotComposition(context.Background(), "client/screens/game.tscn",
		query.GodotCompositionOptions{Kind: graph.KindFile})
	if err != nil {
		t.Fatal(err)
	}
	if sceneReport.Root.ID != sceneFile.ID {
		t.Fatalf("root = %s [%s]", sceneReport.Root.QualifiedName, sceneReport.Root.Kind)
	}
	if len(sceneReport.SceneNodes) != 1 || sceneReport.SceneNodes[0].ID != sceneNode.ID {
		t.Fatalf("scene nodes = %#v", sceneReport.SceneNodes)
	}
	if sceneReport.Members == nil || len(sceneReport.Members.Members) != 1 ||
		sceneReport.Members.Members[0].ID != scene.ID {
		t.Fatalf("members = %#v", sceneReport.Members)
	}
	if len(sceneReport.OutboundInstances) != 1 || len(sceneReport.AttachedScripts) != 1 {
		t.Fatalf("instances = %#v scripts = %#v", sceneReport.OutboundInstances, sceneReport.AttachedScripts)
	}

	// A script file answers the inbound question through the module it declares.
	scriptReport, err := service.GodotComposition(context.Background(), "client/screens/game.gd",
		query.GodotCompositionOptions{Kind: graph.KindFile})
	if err != nil {
		t.Fatal(err)
	}
	if len(scriptReport.ScriptAttachments) != 1 ||
		scriptReport.ScriptAttachments[0].Node.QualifiedName != "client/screens/game" {
		t.Fatalf("attached to = %#v", scriptReport.ScriptAttachments)
	}
	if len(scriptReport.AutoloadExposures) != 1 {
		t.Fatalf("autoload exposures = %#v", scriptReport.AutoloadExposures)
	}

	// A project file answers through the autoloads it declares.
	projectReport, err := service.GodotComposition(context.Background(), "client/project.godot",
		query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(projectReport.AutoloadTargets) != 1 ||
		projectReport.AutoloadTargets[0].Node.QualifiedName != "client/screens/game" {
		t.Fatalf("autoload targets = %#v", projectReport.AutoloadTargets)
	}

	// A scene root owns its own evidence, so it reports no member expansion.
	direct, err := service.GodotComposition(context.Background(), "client/screens/game",
		query.GodotCompositionOptions{Kind: graph.KindGodotScene})
	if err != nil {
		t.Fatal(err)
	}
	if direct.Members != nil {
		t.Fatalf("scene root member aggregation = %#v", direct.Members)
	}
}

// godotNodeAt builds a Godot node with the canonical location its parser records,
// which is the file the identity drops the extension from.
func godotNodeAt(kind graph.NodeKind, qualified, path string, properties map[string]string) graph.Node {
	node := godotNode(kind, qualified, properties)
	node.Name = graph.SimpleName(path)
	if kind != graph.KindFile {
		node.Name = graph.SimpleName(qualified)
	}
	node.Location = graph.Location{Path: path, Line: 1, Column: 1}
	return node
}

// godotProjects adds the optional node-listing capability a res:// selector needs
// to find the projects it could be relative to.
func godotProjects(repository *fakeRepository) *godotProjectRepository {
	return &godotProjectRepository{fakeRepository: repository}
}

type godotProjectRepository struct {
	*fakeRepository
}

func (r *godotProjectRepository) ListNodesByKind(_ context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	result := []graph.ScopedNode{}
	for _, kind := range request.Kinds {
		for _, node := range r.nodes {
			if node.Kind != kind || node.External {
				continue
			}
			if request.Name != "" && !strings.Contains(node.QualifiedName, request.Name) {
				continue
			}
			result = append(result, graph.ScopedNode{Node: node})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Node.ID < result[j].Node.ID })
	return result, nil
}

// TestGodotCompositionRefusesWeakerThanExactResourcePathEvidence pins the guard
// that keeps a resource path from resolving to the wrong node. An identity is the
// exact qualified name a Godot node carries, so a selector canonicalizing to one
// must not be satisfied by a node whose name merely contains it or merely folds
// to it - those are the matches ordinary selector resolution accepts, and here
// they are evidence about a different node.
func TestGodotCompositionRefusesWeakerThanExactResourcePathEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		resident graph.Node
	}{
		{
			name:     "substring",
			resident: godotNodeAt(graph.KindGodotScene, "vendor/client/screens/game_over", "vendor/client/screens/game_over.tscn", nil),
		},
		{
			name:     "case folded",
			resident: godotNodeAt(graph.KindGodotScene, "client/Screens/Game", "client/Screens/Game.tscn", nil),
		},
		{
			name: "bare name",
			resident: func() graph.Node {
				node := godotNodeAt(graph.KindGodotScene, "elsewhere/game", "elsewhere/game.tscn", nil)
				node.Name = "client/screens/game"
				return node
			}(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := query.NewService(godotProjects(&fakeRepository{nodes: nodeSet(test.resident)}))
			report, err := service.GodotComposition(context.Background(), "client/screens/game.tscn",
				query.GodotCompositionOptions{Kind: graph.KindGodotScene})
			if !errors.Is(err, query.ErrNotFound) {
				t.Fatalf("resolved %s [%s] on %s evidence (err = %v)",
					report.Root.QualifiedName, report.Root.Kind, test.name, err)
			}
		})
	}
}
