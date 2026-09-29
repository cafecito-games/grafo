package detailprofile_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/detailprofile"
	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestClaimedQueriesAreFullEquivalentOnMixedLanguageCrossFileFixture(t *testing.T) {
	full := profileQuerySuite(t, detailprofile.ProfileFull)
	structural := profileQuerySuite(t, detailprofile.ProfileStructural)
	if !reflect.DeepEqual(full, structural) {
		for name, want := range full {
			if got := structural[name]; got != want {
				t.Errorf("%s differs\nfull: %s\nstructural: %s", name, want, got)
			}
		}
	}
}

func TestClaimedCrossFileCallsRemainEquivalentAcrossFederatedIndexes(t *testing.T) {
	full := federatedCallQuery(t, detailprofile.ProfileFull)
	structural := federatedCallQuery(t, detailprofile.ProfileStructural)
	if full != structural {
		t.Fatalf("federated calls differ\nfull: %s\nstructural: %s", full, structural)
	}
}

func profileQuerySuite(t *testing.T, profile detailprofile.Profile) map[string]string {
	t.Helper()
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	projector := mustProjector(t, detailprofile.Options{Profile: profile})
	validator := detailprofile.NewAggregateResolutionValidator()
	var projected []graph.ParseResult
	for _, source := range mixedLanguageFixture() {
		result, err := projector.Transform(ctx, parserapi.Input{Path: source.path}, source.result)
		if err != nil {
			t.Fatalf("%s: %v", source.path, err)
		}
		validator.Observe(source.result, result)
		projected = append(projected, result)
		if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: source.path, Hash: "hash-" + source.path,
			Language: source.language, Size: 1, IndexedAt: graph.NowUTC()}, result); err != nil {
			t.Fatal(err)
		}
	}
	if err := validator.Validate(); err != nil {
		t.Fatal(err)
	}
	workspace := mixedLanguageWorkspace()
	projected = append(projected, workspace)
	if err := detailprofile.ValidateClosure(projected); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "__workspace__", workspace); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"n:scene-main", "n:scene-child", "n:move", "n:action"} {
		if _, err := repository.Node(ctx, id); err != nil {
			t.Fatalf("fixture node %s: %v", id, err)
		}
	}

	service := query.NewService(repository)
	catalog := query.NewCatalog(repository)
	topology := query.NewTopology(repository)
	suite := map[string]any{}
	found, err := service.Find(ctx, "web.Handle", 100)
	suite["find"] = mustQuery(t, found, err)
	shown, err := service.ResolveKind(ctx, "app.Call", graph.KindFunction)
	suite["show"] = mustQuery(t, shown, err)
	calls, err := service.Neighborhood(ctx, "app.Call", graph.KindFunction, 2, query.Outgoing, []graph.EdgeKind{graph.EdgeCalls}, 100)
	suite["neighbors-and-callees"] = mustQuery(t, calls, err)
	callers, err := service.Neighborhood(ctx, "web.Handle", graph.KindFunction, 1, query.Incoming, []graph.EdgeKind{graph.EdgeCalls}, 100)
	suite["callers"] = mustQuery(t, callers, err)
	path, err := service.ShortestPath(ctx, "app.Call", "web.Handle", graph.KindFunction, query.Outgoing, []graph.EdgeKind{graph.EdgeCalls}, 100)
	suite["path"] = mustQuery(t, path, err)
	resources, err := catalog.DataResources(ctx, nil, query.CatalogOptions{Limit: 100})
	suite["data-resources"] = mustQuery(t, resources, err)
	usage, err := catalog.DataResourceUsage(ctx, "orders", query.CatalogOptions{Limit: 100})
	suite["data-usage"] = mustQuery(t, usage, err)
	config, err := catalog.ConfigKeys(ctx, query.CatalogOptions{Limit: 100})
	suite["config"] = mustQuery(t, config, err)
	events, err := catalog.Events(ctx, query.CatalogOptions{Limit: 100})
	suite["events"] = mustQuery(t, events, err)
	endpoints, err := topology.Endpoints(ctx, query.TopologyOptions{Limit: 100})
	suite["endpoints"] = mustQuery(t, endpoints, err)
	outbound, err := topology.OutboundRequests(ctx, query.TopologyOptions{Limit: 100})
	suite["outbound"] = mustQuery(t, outbound, err)
	handlers, err := topology.Handlers(ctx, query.TopologyOptions{Method: "POST", Route: "/items", Limit: 100})
	suite["handlers"] = mustQuery(t, handlers, err)
	topologyResult, err := topology.ServiceTopology(ctx, query.TopologyOptions{Limit: 100})
	suite["topology"] = mustQuery(t, topologyResult, err)
	findTests, err := service.FindTests(ctx, "app.Call", query.TestCoverageOptions{Kind: graph.KindFunction, Limit: 100})
	suite["find-tests"] = mustQuery(t, findTests, err)
	testCoverage, err := service.TestCoverage(ctx, "app.TestCall", query.TestCoverageOptions{Limit: 100})
	suite["test-coverage"] = mustQuery(t, testCoverage, err)
	composition, err := service.GodotComposition(ctx, "res://main.tscn", query.GodotCompositionOptions{Kind: graph.KindGodotScene, Limit: 100})
	suite["godot-composition"] = mustQuery(t, composition, err)
	interactions, err := service.GodotInteractions(ctx, "game.move", query.GodotInteractionsOptions{Kind: graph.KindMethod, Limit: 100})
	suite["godot-interactions"] = mustQuery(t, interactions, err)
	result := map[string]string{}
	for name, value := range suite {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = string(encoded)
	}
	return result
}

func federatedCallQuery(t *testing.T, profile detailprofile.Profile) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	sources := mixedLanguageFixture()[:2]
	projects := make([]indexer.Project, 0, len(sources))
	projector := mustProjector(t, detailprofile.Options{Profile: profile})
	validator := detailprofile.NewAggregateResolutionValidator()
	for index, source := range sources {
		projectRoot := filepath.Join(root, []string{"api", "web"}[index])
		indexPath := filepath.Join(projectRoot, ".grafo", "indexes", "main.sqlite")
		if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
			t.Fatal(err)
		}
		projected, err := projector.Transform(ctx, parserapi.Input{Path: source.path}, source.result)
		if err != nil {
			t.Fatal(err)
		}
		validator.Observe(source.result, projected)
		repository, err := sqlite.Open(ctx, indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: source.path, Hash: "hash", Language: source.language, IndexedAt: graph.NowUTC()}, projected); err != nil {
			_ = repository.Close()
			t.Fatal(err)
		}
		if err := repository.Reconcile(ctx); err != nil {
			_ = repository.Close()
			t.Fatal(err)
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		projects = append(projects, indexer.Project{Root: projectRoot, Name: []string{"api", "web"}[index], ID: "repository-" + source.language, Branch: "main", IndexPath: indexPath})
	}
	if err := validator.Validate(); err != nil {
		t.Fatal(err)
	}
	repository, err := federation.OpenProjects(ctx, projects)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	result, err := query.NewService(repository).Neighborhood(ctx, "app.Call", graph.KindFunction, 1, query.Outgoing, []graph.EdgeKind{graph.EdgeCalls}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 || result.Edges[0].Properties["federated"] != "true" {
		t.Fatalf("federated call evidence = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func mustQuery[T any](t *testing.T, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

type profileSource struct {
	path, language string
	result         graph.ParseResult
}

func mixedLanguageFixture() []profileSource {
	loc := func(path string, line int) graph.Location { return graph.Location{Path: path, Line: line, Column: 1} }
	node := func(id string, kind graph.NodeKind, name, qualified, language, path string) graph.Node {
		return graph.Node{ID: "n:" + id, Kind: kind, Name: name, QualifiedName: qualified, Language: language, Location: loc(path, 1), OwnerFile: path}
	}
	fact := func(id, from string, kind graph.EdgeKind, targetID, target string, targetKind graph.NodeKind, producer, path string) graph.Fact {
		if targetID != "" {
			targetID = "n:" + targetID
		}
		return graph.Fact{ID: id, FromID: "n:" + from, Kind: kind, TargetID: targetID, Target: target, TargetKind: targetKind,
			Producer: producer, Location: loc(path, 2), OwnerFile: path}
	}
	goPath := "apps/api/app.go"
	goNodes := []graph.Node{
		node("file-go", graph.KindFile, "app.go", goPath, "go", goPath), node("call", graph.KindFunction, "Call", "app.Call", "go", goPath),
		node("local", graph.KindVariable, "payload", "app.Call.payload", "go", goPath), node("endpoint", graph.KindEndpoint, "POST /items", "POST /items", "go", goPath),
		node("event", graph.KindEvent, "item.created", "item.created", "go", goPath), node("config", graph.KindConfigKey, "API_TOKEN", "API_TOKEN", "go", goPath),
		node("table", graph.KindTable, "orders", "orders", "sql", goPath), node("test", graph.KindTest, "TestCall", "app.TestCall", "go", goPath),
		node("message", graph.KindType, "Item", "acme.Item", "protobuf", goPath), node("field", graph.KindField, "id", "acme.Item.id", "protobuf", goPath),
		node("operation", graph.KindTransportOperation, "send", "app.Call.send@9", "go", goPath),
	}
	goNodes[3].Properties = map[string]string{"method": "POST", "route": "/items"}
	goFacts := []graph.Fact{
		fact("go-declare-call", "file-go", graph.EdgeDeclares, "call", "", "", "go", goPath),
		fact("go-declare-local", "call", graph.EdgeDeclares, "local", "", "", "go", goPath),
		fact("go-assign", "local", graph.EdgeAssigns, "local", "", "", "go", goPath),
		fact("go-call", "call", graph.EdgeCalls, "", "web.Handle", graph.KindFunction, "go", goPath),
		fact("go-request", "call", graph.EdgeRequests, "endpoint", "", "", "go", goPath),
		fact("go-exposes", "call", graph.EdgeExposes, "endpoint", "", "", "go", goPath),
		fact("go-handler", "endpoint", graph.EdgeHandledBy, "", "web.Handle", graph.KindFunction, "go", goPath),
		fact("go-publish", "call", graph.EdgePublishes, "event", "", "", "go", goPath),
		fact("go-config", "call", graph.EdgeReadsConfig, "config", "", "", "go", goPath),
		fact("go-read", "call", graph.EdgeReads, "table", "", "", "go", goPath),
		fact("go-write", "call", graph.EdgeWrites, "table", "", "", "go", goPath),
		fact("go-test", "test", graph.EdgeTests, "call", "", "", "go", goPath),
		fact("go-field", "message", graph.EdgeHasField, "field", "", "", "protobuf", goPath),
		fact("go-send", "call", graph.EdgeSends, "operation", "", "", "go", goPath),
		fact("go-carries", "operation", graph.EdgeCarries, "message", "", "", "go", goPath),
		fact("go-encodes", "call", graph.EdgeEncodes, "message", "", "", "go", goPath),
	}
	tsPath := "apps/web/handler.ts"
	tsNodes := []graph.Node{
		node("file-ts", graph.KindFile, "handler.ts", tsPath, "typescript", tsPath), node("handler", graph.KindFunction, "Handle", "web.Handle", "typescript", tsPath),
		node("binding", graph.KindType, "ItemBinding", "web.ItemBinding", "protobufbinding", tsPath),
	}
	tsFacts := []graph.Fact{
		fact("ts-declare-handler", "file-ts", graph.EdgeDeclares, "handler", "", "", "typescript", tsPath),
		fact("ts-subscribe", "handler", graph.EdgeSubscribes, "event", "", "", "typescript", tsPath),
		fact("ts-binding", "binding", graph.EdgeGeneratedFrom, "message", "", "", "protobufbinding", tsPath),
		fact("ts-decodes", "handler", graph.EdgeDecodes, "message", "", "", "typescript", tsPath),
	}
	gdPath := "client/game.gd"
	gdNodes := []graph.Node{
		node("file-gd", graph.KindFile, "game.gd", gdPath, "gdscript", gdPath), node("move", graph.KindMethod, "move", "game.move", "gdscript", gdPath),
		node("scene-main", graph.KindGodotScene, "main.tscn", "res://main.tscn", "godot", gdPath), node("scene-child", graph.KindGodotScene, "child.tscn", "res://child.tscn", "godot", gdPath),
		node("action", graph.KindGodotInputAction, "move_left", "move_left", "godot", gdPath), node("group", graph.KindGodotNodeGroup, "players", "players", "godot", gdPath),
	}
	gdFacts := []graph.Fact{
		fact("gd-declare", "file-gd", graph.EdgeDeclares, "move", "", "", "gdscript", gdPath),
		fact("gd-scene", "scene-main", graph.EdgeInstantiates, "scene-child", "", "", "godot", gdPath),
		fact("gd-action", "move", graph.EdgeUsesInputAction, "action", "", "", "gdscript", gdPath),
		fact("gd-group", "move", graph.EdgeUsesGroup, "group", "", "", "gdscript", gdPath),
	}
	return []profileSource{
		{goPath, "go", graph.ParseResult{Nodes: goNodes, Facts: goFacts}},
		{tsPath, "typescript", graph.ParseResult{Nodes: tsNodes, Facts: tsFacts}},
		{gdPath, "gdscript", graph.ParseResult{Nodes: gdNodes, Facts: gdFacts}},
	}
}

func mixedLanguageWorkspace() graph.ParseResult {
	repository := graph.Node{ID: "n:repo", Kind: graph.KindRepository, Name: "fixture", QualifiedName: "fixture", OwnerFile: "__workspace__"}
	api := graph.Node{ID: "n:component-api", Kind: graph.KindComponent, Name: "api", QualifiedName: "fixture/api", OwnerFile: "__workspace__"}
	web := graph.Node{ID: "n:component-web", Kind: graph.KindComponent, Name: "web", QualifiedName: "fixture/web", OwnerFile: "__workspace__"}
	client := graph.Node{ID: "n:component-client", Kind: graph.KindComponent, Name: "client", QualifiedName: "fixture/client", OwnerFile: "__workspace__"}
	fact := func(id, from, to string) graph.Fact {
		return graph.Fact{ID: id, FromID: "n:" + from, Kind: graph.EdgeContains, TargetID: "n:" + to, Producer: graph.ProducerIndexer, OwnerFile: "__workspace__"}
	}
	return graph.ParseResult{Nodes: []graph.Node{repository, api, web, client}, Facts: []graph.Fact{
		fact("repo-api", "repo", "component-api"), fact("repo-web", "repo", "component-web"), fact("repo-client", "repo", "component-client"),
		fact("api-file", "component-api", "file-go"), fact("web-file", "component-web", "file-ts"), fact("client-file", "component-client", "file-gd"),
	}}
}
