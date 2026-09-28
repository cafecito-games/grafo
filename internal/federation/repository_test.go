package federation_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	manifestparser "github.com/cafecito-games/grafo/internal/parser/manifest"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/semantic"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestRepositoryResolvesHTTPAcrossIndexes(t *testing.T) {
	ctx := context.Background()
	clientRoot := t.TempDir()
	serverRoot := t.TempDir()
	write(t, filepath.Join(clientRoot, "go.mod"), "module example.com/client\n\ngo 1.26\n\nrequire example.com/server v0.0.0\n")
	write(t, filepath.Join(clientRoot, "client.go"), `package client
import "net/http"
func Call() { http.Get("/charge") }
type API struct { baseURL string }
func (api *API) Unknown() { http.Get(api.baseURL + "/charge") }
`)
	write(t, filepath.Join(serverRoot, "go.mod"), "module example.com/server\n\ngo 1.26\n")
	write(t, filepath.Join(serverRoot, "server.go"), `package server
func Handler() {}
func Routes() { router.Get("/charge", Handler) }
`)
	index(t, ctx, clientRoot)
	index(t, ctx, serverRoot)

	repository, err := federation.Open(ctx, []string{clientRoot, serverRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	result, err := query.NewService(repository).Neighborhood(ctx, "example.com/client.Call", "", 1,
		query.Outgoing, []graph.EdgeKind{graph.EdgeRequests}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 || result.Nodes[1].Node.Kind != graph.KindEndpoint || result.Nodes[1].Node.Name != "GET /charge" {
		t.Fatalf("cross-repository endpoint was not resolved: %#v", result.Nodes)
	}
	if len(result.Edges) != 1 || result.Edges[0].Properties["federated"] != "true" {
		t.Fatalf("expected a federated request edge: %#v", result.Edges)
	}
	located, err := repository.ProjectForNode(ctx, result.Root.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolvedClientRoot, err := filepath.EvalSymlinks(clientRoot)
	if err != nil {
		t.Fatal(err)
	}
	if located.Root != resolvedClientRoot {
		t.Fatalf("source node mapped to %q, want %q", located.Root, resolvedClientRoot)
	}
	incoming, err := repository.EdgesTo(ctx, result.Nodes[1].Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundIncoming := false
	for _, edge := range incoming {
		if edge.Kind == graph.EdgeRequests && edge.FromID == result.Root.ID && edge.Properties["federated"] == "true" {
			foundIncoming = true
		}
	}
	if !foundIncoming {
		t.Fatalf("incoming federated edge missing: %#v", incoming)
	}
	unknown, err := query.NewService(repository).Neighborhood(ctx, "example.com/client.API.Unknown", "", 1,
		query.Outgoing, []graph.EdgeKind{graph.EdgeRequests}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Edges) != 1 || unknown.Edges[0].Properties["http_authority_unknown"] != "true" ||
		unknown.Edges[0].Properties["federated"] == "true" || len(unknown.Nodes) != 2 || !unknown.Nodes[1].Node.External {
		t.Fatalf("unknown receiver authority crossed the federation boundary: %#v", unknown)
	}
	for _, edge := range incoming {
		if edge.FromID == unknown.Root.ID {
			t.Fatalf("incoming federation invented an unknown-authority request: %#v", incoming)
		}
	}
	dependency, err := query.NewService(repository).Neighborhood(ctx, "example.com/client", "", 1,
		query.Outgoing, []graph.EdgeKind{graph.EdgeDependsOn}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependency.Nodes) != 2 || dependency.Nodes[1].Node.Kind != graph.KindModule || dependency.Nodes[1].Node.QualifiedName != "example.com/server" {
		t.Fatalf("cross-repository module dependency was not resolved: %#v", dependency.Nodes)
	}
	if len(dependency.Edges) != 1 || dependency.Edges[0].Properties["federated"] != "true" {
		t.Fatalf("expected a federated dependency edge: %#v", dependency.Edges)
	}

	semanticService := semantic.NewService(repository, repository, handlerEmbedder{})
	report, err := semanticService.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Candidates < 3 || report.Updated != report.Candidates {
		t.Fatalf("federated embeddings were not distributed to member indexes: %#v", report)
	}
	matches, err := semanticService.Search(ctx, "find a request handler", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches.Matches) != 1 || matches.Matches[0].Node.Name != "Handler" {
		t.Fatalf("unexpected federated semantic match: %#v", matches.Matches)
	}
}

func TestRepositoryProjectsCrossRepositoryTestCoverage(t *testing.T) {
	ctx := context.Background()
	testRoot := t.TempDir()
	productionRoot := t.TempDir()
	write(t, filepath.Join(testRoot, "go.mod"), "module example.com/checkouttests\n\ngo 1.26\n\nrequire example.com/shop v0.0.0\n")
	write(t, filepath.Join(testRoot, "checkout_test.go"), `package checkouttests
import (
	"testing"
	"example.com/shop"
)
func TestCheckout(t *testing.T) { shop.Checkout() }
`)
	write(t, filepath.Join(productionRoot, "go.mod"), "module example.com/shop\n\ngo 1.26\n")
	write(t, filepath.Join(productionRoot, "shop.go"), "package shop\nfunc Checkout() {}\n")
	index(t, ctx, testRoot)
	index(t, ctx, productionRoot)

	repository, err := federation.Open(ctx, []string{testRoot, productionRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := query.NewService(repository)

	coverage, err := service.TestCoverage(ctx, "example.com/checkouttests.TestCheckout", query.TestCoverageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage.Matches) != 1 || !coverage.Matches[0].Direct ||
		coverage.Matches[0].Target.QualifiedName != "example.com/shop.Checkout" ||
		coverage.Matches[0].Edges[0].Properties["federated"] != "true" {
		t.Fatalf("cross-repository test coverage = %#v", coverage)
	}
	tests, err := service.FindTests(ctx, "example.com/shop.Checkout", query.TestCoverageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tests.Matches) != 1 || !tests.Matches[0].Direct ||
		tests.Matches[0].Test.QualifiedName != "example.com/checkouttests.TestCheckout" ||
		tests.Matches[0].Edges[0].Properties["federated"] != "true" {
		t.Fatalf("cross-repository reverse test coverage = %#v", tests)
	}
}

type handlerEmbedder struct{}

func (handlerEmbedder) Model() string { return "federation-test" }

func (handlerEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	result := make([][]float32, 0, len(inputs))
	for _, input := range inputs {
		if strings.Contains(strings.ToLower(input), "handler") {
			result = append(result, []float32{1, 0})
		} else {
			result = append(result, []float32{0, 1})
		}
	}
	return result, nil
}

func index(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New(), manifestparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRepositoryResolvesGodotCompositionAcrossIndexes covers explicit
// federation of the Godot composition vocabulary: a scene instance and a script
// attachment whose target lives in another indexed repository resolve across
// the boundary and stay marked as federated.
func TestRepositoryResolvesGodotCompositionAcrossIndexes(t *testing.T) {
	ctx := context.Background()
	gameRoot := t.TempDir()
	sharedRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gameRoot, "scenes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sharedRoot, "ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(gameRoot, "project.godot"), "config_version=5\n")
	write(t, filepath.Join(gameRoot, "scenes", "main.tscn"),
		"[gd_scene load_steps=3 format=3 uid=\"uid://main123\"]\n\n"+
			"[ext_resource type=\"PackedScene\" path=\"res://ui/panel.tscn\" id=\"1_panel\"]\n"+
			"[ext_resource type=\"Script\" path=\"res://ui/panel.gd\" id=\"2_script\"]\n\n"+
			"[node name=\"Main\" type=\"Node\"]\nscript = ExtResource(\"2_script\")\n\n"+
			"[node name=\"Panel\" parent=\".\" instance=ExtResource(\"1_panel\")]\n")
	write(t, filepath.Join(sharedRoot, "ui", "panel.tscn"),
		"[gd_scene format=3 uid=\"uid://panel123\"]\n\n[node name=\"Panel\" type=\"Control\"]\n")
	write(t, filepath.Join(sharedRoot, "ui", "panel.gd"), "class_name Panel extends Control\n")
	indexGodot(t, ctx, gameRoot)
	indexGodot(t, ctx, sharedRoot)

	repository, err := federation.Open(ctx, []string{gameRoot, sharedRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := query.NewService(repository)
	report, err := service.GodotComposition(ctx, "scenes/main", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertFederatedRelation(t, report.OutboundInstances, "ui/panel", graph.KindGodotScene)
	assertFederatedRelation(t, report.AttachedScripts, "ui/panel", graph.KindModule)
}

func assertFederatedRelation(t *testing.T, relations []query.GodotRelation, qualified string, kind graph.NodeKind) {
	t.Helper()
	for _, relation := range relations {
		if relation.Node.QualifiedName != qualified || relation.Node.Kind != kind {
			continue
		}
		if relation.Node.External {
			t.Fatalf("%q [%s] stayed unresolved: %#v", qualified, kind, relation)
		}
		if !relation.Federated {
			t.Fatalf("%q [%s] was not marked federated: %#v", qualified, kind, relation)
		}
		return
	}
	t.Fatalf("missing federated relation to %q [%s]: %#v", qualified, kind, relations)
}

func indexGodot(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
}
