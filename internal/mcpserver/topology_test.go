package mcpserver_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

func TestServerExposesEndpointAndServiceTopologyTools(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.SetMeta(ctx, "root", "/tmp/example/shop"); err != nil {
		t.Fatal(err)
	}

	endpoint := graph.Node{ID: graph.NodeID(graph.KindEndpoint, "GET /orders", "routes.go:5"),
		Kind: graph.KindEndpoint, Name: "GET /orders", QualifiedName: "endpoint:GET /orders@routes.go:5",
		Location: graph.Location{Path: "routes.go", Line: 5}, OwnerFile: "routes.go",
		Properties: map[string]string{"method": "GET", "route": "/orders"}}
	routes := graph.Node{ID: graph.NodeID(graph.KindFunction, "shop.Routes"), Kind: graph.KindFunction,
		Name: "Routes", QualifiedName: "shop.Routes", OwnerFile: "routes.go"}
	handler := graph.Node{ID: graph.NodeID(graph.KindFunction, "shop.Handler"), Kind: graph.KindFunction,
		Name: "Handler", QualifiedName: "shop.Handler", OwnerFile: "routes.go"}
	caller := graph.Node{ID: graph.NodeID(graph.KindFunction, "shop.CallOrders"), Kind: graph.KindFunction,
		Name: "CallOrders", QualifiedName: "shop.CallOrders", OwnerFile: "client.go"}
	if err := repository.ReplaceOwner(ctx, "routes.go", graph.ParseResult{
		Nodes: []graph.Node{endpoint, routes, handler},
		Facts: []graph.Fact{
			{ID: "f:exposes", FromID: routes.ID, Kind: graph.EdgeExposes, TargetID: endpoint.ID, OwnerFile: "routes.go"},
			{ID: "f:handler", FromID: endpoint.ID, Kind: graph.EdgeHandledBy, TargetID: handler.ID, OwnerFile: "routes.go"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{
		Nodes: []graph.Node{caller},
		Facts: []graph.Fact{{ID: "f:request", FromID: caller.ID, Kind: graph.EdgeRequests,
			Target: "GET /orders", TargetKind: graph.KindEndpoint, OwnerFile: "client.go",
			Location: graph.Location{Path: "client.go", Line: 9}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	session := connect(t, mcpserver.New(repository, indexer.Project{Name: "shop", Branch: "main"}))
	listed := call(t, session, "list_endpoints", map[string]any{"method": "GET", "route": "/orders"})
	if endpoints, ok := listed["endpoints"].([]any); !ok || len(endpoints) != 1 {
		t.Fatalf("unexpected endpoint catalog: %#v", listed)
	}
	outbound := call(t, session, "list_outbound_requests", map[string]any{"method": "GET"})
	if requests, ok := outbound["requests"].([]any); !ok || len(requests) != 1 {
		t.Fatalf("unexpected outbound request catalog: %#v", outbound)
	}
	handlers := call(t, session, "find_handler", map[string]any{"route": "/orders"})
	if matches, ok := handlers["matches"].([]any); !ok || len(matches) != 1 {
		t.Fatalf("unexpected handler result: %#v", handlers)
	}
	topology := call(t, session, "get_service_topology", map[string]any{"route": "/orders"})
	if links, ok := topology["links"].([]any); !ok || len(links) != 1 {
		t.Fatalf("unexpected service topology: %#v", topology)
	}

	callExpectingError(t, session, "get_service_topology", map[string]any{"direction": "sideways"})
	callExpectingError(t, session, "find_handler", map[string]any{"route": "/orders", "event": "order.placed"})
}
