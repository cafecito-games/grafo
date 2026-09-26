package manifest_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	manifestparser "github.com/cafecito-games/grafo/internal/parser/manifest"
)

func TestParserExtractsGoModuleDependencies(t *testing.T) {
	content := []byte(`module example.com/checkout

go 1.26

require (
	github.com/acme/payments v1.2.3
	github.com/acme/telemetry v0.9.0 // indirect
)

replace github.com/acme/payments => ../payments
`)
	result, err := manifestparser.New().Parse(context.Background(), parserapi.Input{
		Path: "go.mod", Content: content, RepoID: "repo:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	module := findNode(t, result.Nodes, graph.KindModule, "example.com/checkout")
	payment := findFact(t, result.Facts, module.ID, graph.EdgeDependsOn, "github.com/acme/payments")
	if payment.Properties["version"] != "v1.2.3" || payment.Properties["replacement"] != "../payments" || payment.Properties["ecosystem"] != "go" {
		t.Fatalf("unexpected payment dependency: %#v", payment)
	}
	telemetry := findFact(t, result.Facts, module.ID, graph.EdgeDependsOn, "github.com/acme/telemetry")
	if telemetry.Properties["indirect"] != "true" {
		t.Fatalf("indirect dependency not preserved: %#v", telemetry)
	}
}

func TestParserExtractsNPMDependencyScopesDeterministically(t *testing.T) {
	content := []byte(`{
  "name": "@cafecito/checkout",
  "version": "2.0.0",
  "dependencies": {"zod": "^4.0.0", "shared": "workspace:*"},
  "optionalDependencies": {"native-addon": "1.0.0"},
  "devDependencies": {"vitest": "^3.0.0", "shared": "ignored-lower-priority"}
}`)
	result, err := manifestparser.New().Parse(context.Background(), parserapi.Input{
		Path: "web/package.json", Content: content, RepoID: "repo:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	module := findNode(t, result.Nodes, graph.KindModule, "@cafecito/checkout")
	shared := findFact(t, result.Facts, module.ID, graph.EdgeDependsOn, "shared")
	if shared.Properties["scope"] != "runtime" || shared.Properties["version"] != "workspace:*" {
		t.Fatalf("dependency precedence changed: %#v", shared)
	}
	optional := findFact(t, result.Facts, module.ID, graph.EdgeDependsOn, "native-addon")
	if optional.Properties["scope"] != "optional" {
		t.Fatalf("optional scope missing: %#v", optional)
	}
	development := findFact(t, result.Facts, module.ID, graph.EdgeDependsOn, "vitest")
	if development.Properties["scope"] != "development" {
		t.Fatalf("development scope missing: %#v", development)
	}
}

func TestParserExtractsPythonRequirements(t *testing.T) {
	content := []byte(`Django>=5.2; python_version >= "3.12"
google_cloud_storage[grpc]==3.1.0 --hash=sha256:abc
-r requirements-base.txt
https://example.com/direct-wheel.whl
`)
	result, err := manifestparser.New().Parse(context.Background(), parserapi.Input{
		Path: "requirements-prod.txt", Content: content, RepoID: "repo:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	django := findFact(t, result.Facts, "repo:test", graph.EdgeDependsOn, "django")
	if django.Properties["version"] != `>=5.2; python_version >= "3.12"` {
		t.Fatalf("Python constraint missing: %#v", django)
	}
	storage := findFact(t, result.Facts, "repo:test", graph.EdgeDependsOn, "google-cloud-storage")
	if storage.Properties["version"] != "==3.1.0" {
		t.Fatalf("Python distribution normalization failed: %#v", storage)
	}
	if storage.Properties["scope"] != "runtime" {
		t.Fatalf("unexpected production requirement scope: %#v", storage)
	}
	if len(result.Facts) != 2 {
		t.Fatalf("options or direct URL became dependencies: %#v", result.Facts)
	}
}

func TestParserSupportsOnlyKnownManifestNames(t *testing.T) {
	parser := manifestparser.New()
	for _, path := range []string{"go.mod", "package.json", "requirements.txt", "config/requirements-dev.txt"} {
		if !parser.Supports(path) {
			t.Errorf("expected support for %s", path)
		}
	}
	for _, path := range []string{"go.sum", "other.json", "requirements.in"} {
		if parser.Supports(path) {
			t.Errorf("unexpected support for %s", path)
		}
	}
}

func findNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return node
		}
	}
	t.Fatalf("missing %s node %q: %#v", kind, qualified, nodes)
	return graph.Node{}
}

func findFact(t *testing.T, facts []graph.Fact, from string, kind graph.EdgeKind, target string) graph.Fact {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID == from && fact.Kind == kind && fact.Target == target {
			return fact
		}
	}
	t.Fatalf("missing %s fact from %q to %q: %#v", kind, from, target, facts)
	return graph.Fact{}
}
