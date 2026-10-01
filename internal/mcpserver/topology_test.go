package mcpserver_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/mcpserver"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestServerExposesEndpointAndServiceTopologyTools(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
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
	middleware := graph.Node{ID: graph.NodeID(graph.KindFunction, "shop.Authenticate"), Kind: graph.KindFunction,
		Name: "Authenticate", QualifiedName: "shop.Authenticate", OwnerFile: "routes.go"}
	caller := graph.Node{ID: graph.NodeID(graph.KindFunction, "shop.CallOrders"), Kind: graph.KindFunction,
		Name: "CallOrders", QualifiedName: "shop.CallOrders", OwnerFile: "client.go"}
	clientComponent := graph.Node{ID: "n:component-client", Kind: graph.KindComponent, Name: "client",
		QualifiedName: "shop/client", OwnerFile: "__workspace__"}
	serverComponent := graph.Node{ID: "n:component-server", Kind: graph.KindComponent, Name: "server",
		QualifiedName: "shop/server", OwnerFile: "__workspace__"}
	clientFile := graph.Node{ID: "n:file-client", Kind: graph.KindFile, Name: "client.go",
		QualifiedName: "shop/client.go", Location: graph.Location{Path: "client.go"}, OwnerFile: "client.go"}
	serverFile := graph.Node{ID: "n:file-server", Kind: graph.KindFile, Name: "routes.go",
		QualifiedName: "shop/routes.go", Location: graph.Location{Path: "routes.go"}, OwnerFile: "routes.go"}
	if err := repository.ReplaceOwner(ctx, "__workspace__", graph.ParseResult{
		Nodes: []graph.Node{clientComponent, serverComponent},
		Facts: []graph.Fact{
			{ID: "f:client-file", FromID: clientComponent.ID, Kind: graph.EdgeContains, TargetID: clientFile.ID, OwnerFile: "__workspace__"},
			{ID: "f:server-file", FromID: serverComponent.ID, Kind: graph.EdgeContains, TargetID: serverFile.ID, OwnerFile: "__workspace__"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "routes.go", graph.ParseResult{
		Nodes: []graph.Node{serverFile, endpoint, routes, handler, middleware},
		Facts: []graph.Fact{
			{ID: "f:exposes", FromID: routes.ID, Kind: graph.EdgeExposes, TargetID: endpoint.ID, OwnerFile: "routes.go"},
			{ID: "f:handler", FromID: endpoint.ID, Kind: graph.EdgeHandledBy, TargetID: handler.ID, OwnerFile: "routes.go"},
			{ID: "f:middleware", FromID: endpoint.ID, Kind: graph.EdgeUsesMiddleware, TargetID: middleware.ID,
				OwnerFile: "routes.go", Properties: map[string]string{"form": "use", "order": "0", "resolution": "go/types"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{
		Nodes: []graph.Node{clientFile, caller},
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
	listed := call(t, session, "list_endpoints", map[string]any{"method": "GET", "route": "/orders", "path_prefixes": []string{"routes.go"}})
	if endpoints, ok := listed["endpoints"].([]any); !ok || len(endpoints) != 1 {
		t.Fatalf("unexpected endpoint catalog: %#v", listed)
	} else if endpoint, ok := endpoints[0].(map[string]any); !ok {
		t.Fatalf("endpoint JSON is not an object: %#v", endpoints[0])
	} else if middleware, ok := endpoint["middleware"].([]any); !ok || len(middleware) != 1 {
		t.Fatalf("endpoint middleware evidence is missing: %#v", endpoint)
	}
	outbound := call(t, session, "list_outbound_requests", map[string]any{"method": "GET", "path_prefixes": []string{"client.go"}})
	if requests, ok := outbound["requests"].([]any); !ok || len(requests) != 1 {
		t.Fatalf("unexpected outbound request catalog: %#v", outbound)
	}
	handlers := call(t, session, "find_handler", map[string]any{"route": "/orders"})
	if matches, ok := handlers["matches"].([]any); !ok || len(matches) != 1 {
		t.Fatalf("unexpected handler result: %#v", handlers)
	}
	topology := call(t, session, "get_service_topology", map[string]any{"route": "/orders", "path_prefixes": []string{"routes.go"}})
	if links, ok := topology["links"].([]any); !ok || len(links) != 1 {
		t.Fatalf("unexpected service topology: %#v", topology)
	}
	componentTopology := call(t, session, "get_service_topology", map[string]any{
		"component": "client", "direction": "outgoing", "route": "/orders",
	})
	if links, ok := componentTopology["links"].([]any); !ok || len(links) != 1 {
		t.Fatalf("unexpected component topology: %#v", componentTopology)
	}
	if services, ok := componentTopology["services"].([]any); !ok || len(services) != 2 {
		t.Fatalf("component services missing: %#v", componentTopology)
	}
	unknown := call(t, session, "get_service_topology", map[string]any{"component": "missing"})
	if links, ok := unknown["links"].([]any); !ok || len(links) != 0 {
		t.Fatalf("unknown component fell back to repository: %#v", unknown)
	}

	callExpectingError(t, session, "get_service_topology", map[string]any{"direction": "sideways"})
	callExpectingError(t, session, "list_endpoints", map[string]any{"path_prefixes": []string{"../outside"}})
	callExpectingError(t, session, "get_service_topology", map[string]any{"direction": "outgoing"})
	callExpectingError(t, session, "find_handler", map[string]any{"route": "/orders", "event": "order.placed"})
}
