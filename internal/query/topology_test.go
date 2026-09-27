package query_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestEndpointCatalogAndHandlerResolutionUseGraphEvidence(t *testing.T) {
	repository := newTopologyFixture()
	service := query.NewTopology(repository)

	endpoints, err := service.Endpoints(context.Background(), query.TopologyOptions{
		Repository: "orders", Method: "get", Route: "/orders", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints.Endpoints) != 1 {
		t.Fatalf("expected one filtered endpoint, got %#v", endpoints)
	}
	endpoint := endpoints.Endpoints[0]
	if endpoint.Method != http.MethodGet || endpoint.Route != "/orders" || endpoint.Repository != "orders" {
		t.Fatalf("normalized endpoint metadata or service attribution is missing: %#v", endpoint)
	}
	if endpoint.Location.Path != "routes.go" || endpoint.Location.Line != 7 {
		t.Fatalf("endpoint source evidence is missing: %#v", endpoint.Location)
	}
	if len(endpoint.Exposers) != 1 || endpoint.Exposers[0].EdgeID != "e:exposes-orders" {
		t.Fatalf("endpoint declaration evidence is missing: %#v", endpoint.Exposers)
	}
	if endpoint.HandlerStatus != query.BoundaryResolved || len(endpoint.Handlers) != 1 ||
		endpoint.Handlers[0].Node.QualifiedName != "orders.List" {
		t.Fatalf("endpoint handler did not resolve from handled_by evidence: %#v", endpoint)
	}

	handlers, err := service.Handlers(context.Background(), query.TopologyOptions{
		Repository: "orders", Method: "GET", Route: "/orders", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(handlers.Matches) != 1 || handlers.Matches[0].Kind != query.HandlerHTTP ||
		handlers.Matches[0].Status != query.BoundaryResolved {
		t.Fatalf("find handler returned the wrong HTTP match: %#v", handlers)
	}

	eventHandlers, err := service.Handlers(context.Background(), query.TopologyOptions{
		Repository: "billing", Event: "order.placed", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(eventHandlers.Matches) != 1 || eventHandlers.Matches[0].Kind != query.HandlerEvent ||
		len(eventHandlers.Matches[0].Handlers) != 1 ||
		eventHandlers.Matches[0].Handlers[0].Node.QualifiedName != "billing.OnOrderPlaced" {
		t.Fatalf("find handler returned the wrong event match: %#v", eventHandlers)
	}
}

func TestHandlerResolutionReportsAmbiguousAndUnresolvedTargets(t *testing.T) {
	service := query.NewTopology(newTopologyFixture())
	result, err := service.Handlers(context.Background(), query.TopologyOptions{Method: "POST", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]query.BoundaryStatus{}
	for _, match := range result.Matches {
		statuses[match.Subject.Name] = match.Status
	}
	if statuses["POST /ambiguous"] != query.BoundaryAmbiguous {
		t.Fatalf("multiple declared handlers were not reported as ambiguous: %#v", result.Matches)
	}
	if statuses["POST /unresolved"] != query.BoundaryUnresolved {
		t.Fatalf("an external handler was not kept unresolved: %#v", result.Matches)
	}
}

func TestEndpointRepositoryFilterKeepsReferencedUnresolvedTargets(t *testing.T) {
	service := query.NewTopology(newTopologyFixture())
	result, err := service.Endpoints(context.Background(), query.TopologyOptions{
		Repository: "client", Method: "DELETE", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Endpoints) != 0 || len(result.Unresolved) != 1 {
		t.Fatalf("repository filter hid its unresolved endpoint reference: %#v", result)
	}
	if result.Unresolved[0].Route != "/missing" || result.Unresolved[0].Repository != "" {
		t.Fatalf("unresolved endpoint gained a false home or wrong route: %#v", result.Unresolved[0])
	}
}

func TestOutboundRequestsResolveExactEndpointsAndFailClosedOnAmbiguity(t *testing.T) {
	service := query.NewTopology(newTopologyFixture())
	result, err := service.OutboundRequests(context.Background(), query.TopologyOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]query.OutboundRequest{}
	for _, request := range result.Requests {
		byTarget[request.Method+" "+request.Route] = request
	}

	resolved := byTarget["GET /orders"]
	if resolved.Status != query.BoundaryResolved || resolved.Destination.Repository != "orders" ||
		resolved.Evidence.EdgeID != "e:request-orders" {
		t.Fatalf("exact request did not resolve with its evidence: %#v", resolved)
	}
	if !resolved.Evidence.Federated {
		t.Fatalf("cross-repository request lost its federation marker: %#v", resolved)
	}

	ambiguous := byTarget["POST /charge"]
	if ambiguous.Status != query.BoundaryAmbiguous || len(ambiguous.Candidates) != 2 {
		t.Fatalf("duplicate routes did not remain explicit: %#v", ambiguous)
	}
	if ambiguous.Destination.ID != "" {
		t.Fatalf("an ambiguous request selected a destination: %#v", ambiguous.Destination)
	}
	if ambiguous.Target.ID == "" || ambiguous.Evidence.ToID != ambiguous.Target.ID {
		t.Fatalf("ambiguous request lost its original graph boundary: %#v", ambiguous)
	}
	for _, candidate := range ambiguous.Candidates {
		if candidate.Method != http.MethodPost {
			t.Fatalf("an incompatible HTTP method became a candidate: %#v", ambiguous.Candidates)
		}
	}

	unresolved := byTarget["DELETE /missing"]
	if unresolved.Status != query.BoundaryUnresolved || !unresolved.Destination.Unresolved {
		t.Fatalf("an unknown target was not returned as an external destination: %#v", unresolved)
	}
}

func TestOutboundRequestBoundsAmbiguousCandidates(t *testing.T) {
	service := query.NewTopology(newTopologyFixture())
	result, err := service.OutboundRequests(context.Background(), query.TopologyOptions{
		Method: "POST", Route: "/charge", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Requests) != 1 || len(result.Requests[0].Candidates) != 1 ||
		!result.Requests[0].Truncated || !result.Truncated {
		t.Fatalf("ambiguous candidates were not bounded explicitly: %#v", result)
	}
}

func TestServiceTopologyIncludesHTTPAndEventLinksWithEvidence(t *testing.T) {
	service := query.NewTopology(newTopologyFixture())
	result, err := service.ServiceTopology(context.Background(), query.TopologyOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]query.ServiceLink{}
	for _, link := range result.Links {
		links[string(link.Kind)+":"+link.Name] = link
	}

	http := links["http:GET /orders"]
	if http.Status != query.BoundaryResolved || http.FromServiceID != "service:client" ||
		http.ToServiceID != "service:orders" || len(http.EndpointIDs) != 1 ||
		!reflect.DeepEqual(http.EdgeIDs, []string{"e:request-orders"}) {
		t.Fatalf("HTTP service link lost identity or evidence: %#v", http)
	}
	if !http.Federated {
		t.Fatalf("HTTP service link lost federation evidence: %#v", http)
	}
	if len(http.SourceNodes) != 1 || len(http.TargetNodes) != 1 || http.Evidence[0].ToID == "" {
		t.Fatalf("HTTP link lost code and endpoint detail: %#v", http)
	}

	event := links["event:order.placed"]
	if event.FromServiceID != "service:orders" || event.ToServiceID != "service:billing" ||
		len(event.EventIDs) == 0 ||
		!reflect.DeepEqual(event.EdgeIDs, []string{"e:publish-order", "e:subscribe-order"}) {
		t.Fatalf("asynchronous service link lost identity or evidence: %#v", event)
	}
	if !event.Federated {
		t.Fatalf("event service link lost federation evidence: %#v", event)
	}
	if len(event.SourceNodes) != 1 || len(event.TargetNodes) != 1 {
		t.Fatalf("event link lost publisher or subscriber detail: %#v", event)
	}

	for _, link := range result.Links {
		if link.Name == "POST /charge" && link.Status == query.BoundaryResolved {
			t.Fatalf("an ambiguous route created a confirmed service link: %#v", link)
		}
	}

	filtered, err := service.ServiceTopology(context.Background(), query.TopologyOptions{
		Repository: "billing", Direction: query.Incoming, Event: "order.placed", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Links) != 1 || filtered.Links[0].Kind != query.LinkEvent {
		t.Fatalf("service, event, and direction filters disagreed: %#v", filtered.Links)
	}
}

func TestTopologyBoundsAndMermaidAreStableAndEscaped(t *testing.T) {
	service := query.NewTopology(newTopologyFixture())
	bounded, err := service.Endpoints(context.Background(), query.TopologyOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Endpoints) != 1 || !bounded.Truncated {
		t.Fatalf("endpoint bound was not reported: %#v", bounded)
	}
	if _, err := service.ServiceTopology(context.Background(), query.TopologyOptions{Direction: "sideways"}); err == nil {
		t.Fatal("an unsupported direction must be rejected")
	}
	if _, err := service.ServiceTopology(context.Background(), query.TopologyOptions{Direction: query.Outgoing}); err == nil {
		t.Fatal("a directional topology without a repository must be rejected")
	}
	if _, err := service.Endpoints(context.Background(), query.TopologyOptions{Direction: query.Incoming}); err == nil {
		t.Fatal("an inapplicable direction must not be accepted and ignored")
	}

	topology := query.ServiceTopology{
		Services: []query.ServiceNode{
			{ID: "service:bad", Repository: "bad", Label: "bad\"]\nflowchart TB"},
			{ID: "service:good", Repository: "good", Label: "good"},
		},
		Links: []query.ServiceLink{{ID: "link:1", FromServiceID: "service:bad", ToServiceID: "service:good",
			Kind: query.LinkHTTP, Name: "GET /x|y", Status: query.BoundaryResolved}},
	}
	first := query.RenderMermaid(topology)
	second := query.RenderMermaid(topology)
	if first != second {
		t.Fatalf("Mermaid output is not stable:\n%s\n%s", first, second)
	}
	if strings.Contains(first, "bad\"]") || strings.Contains(first, "\nflowchart TB\n") ||
		strings.Contains(first, "GET /x|y") {
		t.Fatalf("Mermaid control syntax was not escaped: %s", first)
	}
}

func newTopologyFixture() *catalogRepository {
	repository := &catalogRepository{nodes: map[string]graph.Node{}, owners: map[string]string{},
		repositories: []string{"billing", "client", "orders", "payments-a", "payments-b"}}
	add := repository.add

	add("client", graph.Node{ID: "n:client-call", Kind: graph.KindFunction, Name: "CallOrders",
		QualifiedName: "client.CallOrders", Location: graph.Location{Path: "client.go", Line: 10}})
	add("client", graph.Node{ID: "n:client-charge", Kind: graph.KindFunction, Name: "Charge",
		QualifiedName: "client.Charge", Location: graph.Location{Path: "client.go", Line: 20}})
	add("client", graph.Node{ID: "n:client-missing", Kind: graph.KindFunction, Name: "DeleteMissing",
		QualifiedName: "client.DeleteMissing", Location: graph.Location{Path: "client.go", Line: 30}})

	add("orders", graph.Node{ID: "n:endpoint-orders", Kind: graph.KindEndpoint, Name: "GET /orders",
		QualifiedName: "endpoint:GET /orders@routes.go:7", Location: graph.Location{Path: "routes.go", Line: 7},
		Properties: map[string]string{"method": "GET", "route": "/orders"}})
	add("orders", graph.Node{ID: "n:routes", Kind: graph.KindFunction, Name: "Routes",
		QualifiedName: "orders.Routes", Location: graph.Location{Path: "routes.go", Line: 5}})
	add("orders", graph.Node{ID: "n:list-orders", Kind: graph.KindFunction, Name: "List",
		QualifiedName: "orders.List", Location: graph.Location{Path: "handlers.go", Line: 12}})

	for _, candidate := range []struct{ repository, id, path string }{
		{"payments-a", "n:charge-a", "a.go"}, {"payments-b", "n:charge-b", "b.go"},
	} {
		add(candidate.repository, graph.Node{ID: candidate.id, Kind: graph.KindEndpoint, Name: "POST /charge",
			QualifiedName: "endpoint:POST /charge@" + candidate.path + ":4",
			Location:      graph.Location{Path: candidate.path, Line: 4},
			Properties:    map[string]string{"method": "POST", "route": "/charge"}})
	}
	add("payments-a", graph.Node{ID: "n:get-charge", Kind: graph.KindEndpoint, Name: "GET /charge",
		QualifiedName: "endpoint:GET /charge@a.go:8", Properties: map[string]string{"method": "GET", "route": "/charge"}})
	add("client", graph.Node{ID: "n:external-charge", Kind: graph.KindEndpoint, Name: "POST /charge",
		QualifiedName: "POST /charge", External: true,
		Properties: map[string]string{"unresolved": "true"}})
	add("client", graph.Node{ID: "n:external-missing", Kind: graph.KindEndpoint, Name: "DELETE /missing",
		QualifiedName: "DELETE /missing", External: true,
		Properties: map[string]string{"unresolved": "true"}})

	add("orders", graph.Node{ID: "n:endpoint-ambiguous", Kind: graph.KindEndpoint, Name: "POST /ambiguous",
		QualifiedName: "endpoint:POST /ambiguous@routes.go:20",
		Properties:    map[string]string{"method": "POST", "route": "/ambiguous"}})
	add("orders", graph.Node{ID: "n:handler-a", Kind: graph.KindFunction, Name: "First",
		QualifiedName: "orders.First"})
	add("orders", graph.Node{ID: "n:handler-b", Kind: graph.KindFunction, Name: "Second",
		QualifiedName: "orders.Second"})
	add("orders", graph.Node{ID: "n:endpoint-unresolved", Kind: graph.KindEndpoint, Name: "POST /unresolved",
		QualifiedName: "endpoint:POST /unresolved@routes.go:30",
		Properties:    map[string]string{"method": "POST", "route": "/unresolved"}})
	add("orders", graph.Node{ID: "n:external-handler", Kind: graph.KindFunction, Name: "DynamicHandler",
		QualifiedName: "DynamicHandler", External: true, Properties: map[string]string{"unresolved": "true"}})

	add("orders", graph.Node{ID: "n:event-order", Kind: graph.KindEvent, Name: "order.placed",
		QualifiedName: "order.placed", Location: graph.Location{Path: "events.go", Line: 4}})
	add("orders", graph.Node{ID: "n:publish-order", Kind: graph.KindFunction, Name: "PublishOrder",
		QualifiedName: "orders.PublishOrder"})
	add("billing", graph.Node{ID: "n:subscribe-order", Kind: graph.KindFunction, Name: "SubscribeOrder",
		QualifiedName: "billing.SubscribeOrder"})
	add("billing", graph.Node{ID: "n:handle-order", Kind: graph.KindFunction, Name: "OnOrderPlaced",
		QualifiedName: "billing.OnOrderPlaced"})

	repository.edges = []graph.Edge{
		{ID: "e:exposes-orders", FactID: "f:exposes-orders", FromID: "n:routes", ToID: "n:endpoint-orders",
			Kind: graph.EdgeExposes, Location: graph.Location{Path: "routes.go", Line: 7}},
		{ID: "e:handle-orders", FactID: "f:handle-orders", FromID: "n:endpoint-orders", ToID: "n:list-orders",
			Kind: graph.EdgeHandledBy, Location: graph.Location{Path: "routes.go", Line: 7}},
		{ID: "e:request-orders", FactID: "f:request-orders", FromID: "n:client-call", ToID: "n:endpoint-orders",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 10},
			Properties: map[string]string{"federated": "true"}},
		{ID: "e:request-charge", FactID: "f:request-charge", FromID: "n:client-charge", ToID: "n:external-charge",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 20}},
		{ID: "e:request-charge-a", FactID: "f:request-charge", FromID: "n:client-charge", ToID: "n:charge-a",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 20},
			Properties: map[string]string{"federated": "true"}},
		{ID: "e:request-charge-b", FactID: "f:request-charge", FromID: "n:client-charge", ToID: "n:charge-b",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 20},
			Properties: map[string]string{"federated": "true"}},
		{ID: "e:request-missing", FactID: "f:request-missing", FromID: "n:client-missing", ToID: "n:external-missing",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 30}},
		{ID: "e:handle-ambiguous-a", FactID: "f:handle-ambiguous-a", FromID: "n:endpoint-ambiguous", ToID: "n:handler-a", Kind: graph.EdgeHandledBy},
		{ID: "e:handle-ambiguous-b", FactID: "f:handle-ambiguous-b", FromID: "n:endpoint-ambiguous", ToID: "n:handler-b", Kind: graph.EdgeHandledBy},
		{ID: "e:handle-unresolved", FactID: "f:handle-unresolved", FromID: "n:endpoint-unresolved", ToID: "n:external-handler", Kind: graph.EdgeHandledBy},
		{ID: "e:publish-order", FactID: "f:publish-order", FromID: "n:publish-order", ToID: "n:event-order", Kind: graph.EdgePublishes},
		{ID: "e:subscribe-order", FactID: "f:subscribe-order", FromID: "n:subscribe-order", ToID: "n:event-order", Kind: graph.EdgeSubscribes,
			Properties: map[string]string{"federated": "true"}},
		{ID: "e:handle-order", FactID: "f:handle-order", FromID: "n:event-order", ToID: "n:handle-order", Kind: graph.EdgeHandledBy,
			Properties: map[string]string{"federated": "true"}},
	}
	return repository
}
