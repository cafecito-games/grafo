package query_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
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
	if got := []string{endpoint.Middleware[0].Node.QualifiedName, endpoint.Middleware[1].Node.QualifiedName}; !reflect.DeepEqual(got, []string{"orders.RequestID", "orders.Authenticate"}) {
		t.Fatalf("endpoint middleware is not in inherited order: %#v", endpoint.Middleware)
	}
	if endpoint.Middleware[0].Evidence["form"] != "use" || endpoint.Middleware[1].Evidence["order"] != "1" ||
		len(endpoint.UnresolvedMiddleware) != 1 || endpoint.UnresolvedMiddleware[0].Node.QualifiedName != "dynamicMiddleware" {
		t.Fatalf("endpoint middleware evidence or unresolved boundary is missing: %#v", endpoint)
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

func TestEndpointMiddlewareEvidenceIsBounded(t *testing.T) {
	result, err := query.NewTopology(newTopologyFixture()).Endpoints(context.Background(), query.TopologyOptions{
		Repository: "orders", Method: "GET", Route: "/orders", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Endpoints) != 1 || !result.Truncated || !result.Endpoints[0].MiddlewareTruncated ||
		len(result.Endpoints[0].Middleware)+len(result.Endpoints[0].UnresolvedMiddleware) != 1 {
		t.Fatalf("middleware bound was not propagated: %#v", result)
	}
}

func TestTopologyPathPrefixesSelectAnchorsAndRetainCounterparts(t *testing.T) {
	repository := newTopologyFixture()
	external := repository.nodes["n:external-missing"]
	external.Location.Path = "external/missing.http"
	repository.nodes[external.ID] = external
	service := query.NewTopology(repository)
	endpoints, err := service.Endpoints(context.Background(), query.TopologyOptions{PathPrefixes: []string{"routes.go"}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints.Endpoints) != 1 || endpoints.Endpoints[0].ID != "n:endpoint-orders" ||
		len(endpoints.Endpoints[0].Handlers) != 1 || endpoints.Endpoints[0].Handlers[0].Location.Path != "routes.go" {
		t.Fatalf("path-scoped endpoints lost complete evidence: %#v", endpoints)
	}
	requests, err := service.OutboundRequests(context.Background(), query.TopologyOptions{PathPrefixes: []string{"client.go"}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests.Requests) != 3 {
		t.Fatalf("source-scoped requests = %#v", requests)
	}
	empty, err := service.OutboundRequests(context.Background(), query.TopologyOptions{PathPrefixes: []string{"client"}, Limit: 20})
	if err != nil || len(empty.Requests) != 0 || empty.Truncated {
		t.Fatalf("segment no-match requests = %#v, %v", empty, err)
	}
	topology, err := service.ServiceTopology(context.Background(), query.TopologyOptions{PathPrefixes: []string{"routes.go"}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, link := range topology.Links {
		if link.Name == "GET /orders" {
			found = true
			if len(link.SourceNodes) != 1 || len(link.TargetNodes) == 0 {
				t.Fatalf("selected topology link lost opposite boundary: %#v", link)
			}
		}
	}
	if !found {
		t.Fatalf("target-anchored topology link missing: %#v", topology)
	}
	externalOnly, err := service.ServiceTopology(context.Background(), query.TopologyOptions{
		PathPrefixes: []string{"external/missing.http"}, Limit: 20,
	})
	if err != nil || len(externalOnly.Links) != 0 || len(externalOnly.Services) != 0 || externalOnly.Truncated {
		t.Fatalf("external-only path created a scoped topology result: %#v, %v", externalOnly, err)
	}
	unmatched, err := service.ServiceTopology(context.Background(), query.TopologyOptions{
		PathPrefixes: []string{"not-present"}, Limit: 1,
	})
	if err != nil || len(unmatched.Links) != 0 || len(unmatched.Services) != 0 || unmatched.Truncated {
		t.Fatalf("out-of-scope truncation leaked into an empty topology: %#v, %v", unmatched, err)
	}
}

func TestEndpointRepositoryFilterKeepsReferencedUnresolvedTargets(t *testing.T) {
	repository := newTopologyFixture()
	repository.add("orders", graph.Node{ID: "n:orders-delete-other", Kind: graph.KindFunction,
		Name: "DeleteOther", QualifiedName: "orders.DeleteOther"})
	repository.add("orders", graph.Node{ID: "n:external-other", Kind: graph.KindEndpoint,
		Name: "DELETE /other", QualifiedName: "DELETE /other", External: true,
		Properties: map[string]string{"unresolved": "true"}})
	repository.edges = append(repository.edges, graph.Edge{ID: "e:request-other", FactID: "f:request-other",
		FromID: "n:orders-delete-other", ToID: "n:external-other", Kind: graph.EdgeRequests})
	service := query.NewTopology(repository)
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

func TestOutboundRequestsRejectCorruptDestinationEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*catalogRepository)
	}{
		{name: "legacy local target lacks proof", mutate: func(repository *catalogRepository) {
			for index := range repository.edges {
				if repository.edges[index].ID == "e:request-orders" {
					repository.edges[index].Properties = map[string]string{"federated": "true"}
				}
			}
		}},
		{name: "explicit authority conflicts with unknown", mutate: func(repository *catalogRepository) {
			for index := range repository.edges {
				if repository.edges[index].ID == "e:request-missing" {
					repository.edges[index].Properties[httpmodel.PropertyAuthority] = "api.example.test"
					repository.edges[index].Properties[httpmodel.PropertyAuthorityUnknown] = "true"
				}
			}
		}},
		{name: "resolved marker targets external node", mutate: func(repository *catalogRepository) {
			for index := range repository.edges {
				if repository.edges[index].ID == "e:request-missing" {
					repository.edges[index].Properties = httpmodel.WithDestinationEvidence(nil,
						httpmodel.DestinationResolved, httpmodel.EvidenceRoute)
				}
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newTopologyFixture()
			test.mutate(repository)
			result, err := query.NewTopology(repository).OutboundRequests(context.Background(), query.TopologyOptions{Limit: 20})
			if err == nil || len(result.Requests) != 0 {
				t.Fatalf("corrupt request evidence returned partial results: %#v, %v", result, err)
			}
		})
	}
}

func TestOutboundRequestsRankCanonicalRouteCompatibility(t *testing.T) {
	repository := newHTTPCompatibilityFixture()
	service := query.NewTopology(repository)
	result, err := service.OutboundRequests(context.Background(), query.TopologyOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	bySource := map[string]query.OutboundRequest{}
	for _, request := range result.Requests {
		bySource[request.Source.Name] = request
	}

	tests := []struct {
		source, route, destination, scheme, authority string
		status                                        query.BoundaryStatus
		candidates                                    int
	}{
		{source: "Exact", route: "/users/42", destination: "literal", status: query.BoundaryResolved},
		{source: "Template", route: "/people/42", destination: "parameter", status: query.BoundaryResolved},
		{source: "Regex", route: "/codes/42", destination: "regex", status: query.BoundaryResolved},
		{source: "Catchall", route: "/assets/css/app.css", destination: "catchall", status: query.BoundaryResolved},
		{source: "Ambiguous", route: "/teams/blue", status: query.BoundaryAmbiguous, candidates: 2},
		{source: "RegexMismatch", route: "/codes/nope", status: query.BoundaryUnresolved},
		{source: "UnknownRegex", route: "/codes/{_}", status: query.BoundaryUnresolved},
		{source: "MethodMismatch", route: "/people/42", status: query.BoundaryUnresolved},
		{source: "ExternalAuthority", route: "/users/42", scheme: "https", authority: "external.test", status: query.BoundaryUnresolved},
		{source: "HTTPAuthority", route: "/users/42", scheme: "http", authority: "external.test", status: query.BoundaryUnresolved},
		{source: "LegacyAuthority", route: "/users/42", scheme: "https", authority: "legacy.test", status: query.BoundaryUnresolved},
		{source: "UnknownAuthority", route: "/users/42", status: query.BoundaryUnresolved},
		{source: "Invalid", route: "/users/%zz", status: query.BoundaryUnresolved},
	}
	for _, test := range tests {
		t.Run(test.source, func(t *testing.T) {
			request := bySource[test.source]
			if request.Status != test.status || request.Route != test.route || request.Scheme != test.scheme || request.Authority != test.authority || len(request.Candidates) != test.candidates {
				t.Fatalf("request = %#v", request)
			}
			if test.destination != "" && request.Destination.Name != test.destination {
				t.Fatalf("destination = %#v", request.Destination)
			}
			if request.Evidence.EdgeID == "" || request.Evidence.Location.Path != "client.go" {
				t.Fatalf("request evidence was lost: %#v", request.Evidence)
			}
		})
	}
	if got := bySource["Exact"].Evidence.Properties["http_raw_route"]; got != "/users/42/?expand=true#details" {
		t.Fatalf("raw route evidence = %q", got)
	}
	topology, err := service.ServiceTopology(context.Background(), query.TopologyOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	externalIDs := map[string]bool{}
	for _, link := range topology.Links {
		if link.Route == "/users/42" && link.ToServiceID != "service:server" {
			externalIDs[link.ToServiceID] = true
		}
	}
	if len(externalIDs) != 4 {
		t.Fatalf("distinct authorities shared an external service: %#v", topology)
	}
}

func TestRouteFiltersUseCanonicalCompatibility(t *testing.T) {
	service := query.NewTopology(newHTTPCompatibilityFixture())
	result, err := service.Endpoints(context.Background(), query.TopologyOptions{
		Method: "get", Route: "/people/{id}/", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Endpoints) != 1 || result.Endpoints[0].Name != "parameter" {
		t.Fatalf("compatible template filter = %#v", result.Endpoints)
	}
	if _, err := service.Endpoints(context.Background(), query.TopologyOptions{Route: "/bad/%zz"}); err == nil {
		t.Fatal("malformed route filter succeeded")
	}
}

func newHTTPCompatibilityFixture() *catalogRepository {
	repository := &catalogRepository{nodes: map[string]graph.Node{}, owners: map[string]string{}, repositories: []string{"client", "server"}}
	addEndpoint := func(id, name, method, route string) {
		repository.add("server", graph.Node{ID: id, Kind: graph.KindEndpoint, Name: name,
			QualifiedName: "endpoint:" + method + " " + route + "@routes.go:1",
			Properties:    map[string]string{"method": method, "route": route}})
	}
	addEndpoint("n:literal", "literal", "GET", "/users/42")
	addEndpoint("n:user-param", "user parameter", "GET", "/users/{id}")
	addEndpoint("n:parameter", "parameter", "GET", "/people/{characterID}")
	addEndpoint("n:regex", "regex", "GET", `/codes/{id:[0-9]+}`)
	addEndpoint("n:catchall", "catchall", "GET", "/assets/{path...}")
	addEndpoint("n:team-a", "team a", "GET", "/teams/{id}")
	addEndpoint("n:team-b", "team b", "GET", "/teams/{teamID}")

	requests := []struct {
		name, method, raw, canonical, scheme, authority string
		legacy                                          bool
	}{
		{name: "Exact", method: "GET", raw: "/users/42/?expand=true#details", canonical: "/users/42"},
		{name: "Template", method: "GET", raw: "/people/42", canonical: "/people/42"},
		{name: "Regex", method: "GET", raw: "/codes/42", canonical: "/codes/42"},
		{name: "Catchall", method: "GET", raw: "/assets/css/app.css", canonical: "/assets/css/app.css"},
		{name: "Ambiguous", method: "GET", raw: "/teams/blue", canonical: "/teams/blue"},
		{name: "RegexMismatch", method: "GET", raw: "/codes/nope", canonical: "/codes/nope"},
		{name: "UnknownRegex", method: "GET", raw: "/codes/{value}", canonical: "/codes/{_}"},
		{name: "MethodMismatch", method: "POST", raw: "/people/42", canonical: "/people/42"},
		{name: "ExternalAuthority", method: "GET", raw: "https://external.test/users/42", canonical: "/users/42", scheme: "https", authority: "external.test"},
		{name: "HTTPAuthority", method: "GET", raw: "http://external.test/users/42", canonical: "/users/42", scheme: "http", authority: "external.test"},
		{name: "LegacyAuthority", method: "GET", raw: "https://legacy.test/users/42?view=full#details", canonical: "/users/42", legacy: true},
		{name: "UnknownAuthority", method: "GET", raw: "/users/42", canonical: "/users/42"},
		{name: "Invalid", method: "GET", raw: "/users/%zz", canonical: "/users/%zz"},
	}
	for index, request := range requests {
		sourceID := fmt.Sprintf("n:request-source-%d", index)
		targetID := fmt.Sprintf("n:request-target-%d", index)
		edgeID := fmt.Sprintf("e:request-%d", index)
		repository.add("client", graph.Node{ID: sourceID, Kind: graph.KindFunction, Name: request.name,
			QualifiedName: "client." + request.name})
		repository.add("", graph.Node{ID: targetID, Kind: graph.KindEndpoint, Name: request.method + " " + request.raw,
			QualifiedName: request.method + " " + request.raw, External: true, Properties: map[string]string{"unresolved": "true"}})
		properties := map[string]string{"http_method": request.method, "http_raw_route": request.raw, "http_route": request.canonical}
		if request.legacy {
			properties = nil
		}
		if request.authority != "" {
			properties["http_authority"] = request.authority
			properties["http_scheme"] = request.scheme
		}
		if request.name == "Invalid" {
			properties["http_invalid"] = "true"
		}
		if request.name == "UnknownAuthority" {
			properties["http_authority_unknown"] = "true"
		}
		resolution, evidence := httpmodel.DestinationUnresolved, httpmodel.EvidenceRoute
		resolvedTarget := ""
		switch request.name {
		case "Exact":
			resolvedTarget = "n:literal"
		case "Template":
			resolvedTarget = "n:parameter"
		case "Regex":
			resolvedTarget = "n:regex"
		case "Catchall":
			resolvedTarget = "n:catchall"
		case "Ambiguous":
			resolution = httpmodel.DestinationAmbiguous
		case "ExternalAuthority", "HTTPAuthority", "LegacyAuthority":
			resolution, evidence = httpmodel.DestinationExternal, httpmodel.EvidenceExplicitAuthority
		case "UnknownAuthority":
			evidence = httpmodel.EvidenceUnknownAuthority
		}
		if resolvedTarget != "" {
			targetID = resolvedTarget
			resolution = httpmodel.DestinationResolved
		}
		properties = httpmodel.WithDestinationEvidence(properties, resolution, evidence)
		repository.edges = append(repository.edges, graph.Edge{ID: edgeID, FactID: edgeID, FromID: sourceID,
			ToID: targetID, Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: index + 1}, Properties: properties})
	}
	return repository
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

func TestServiceTopologyUsesComponentOwnershipAndRepositoryFallback(t *testing.T) {
	repository := newComponentTopologyFixture()
	service := query.NewTopology(repository)
	result, err := service.ServiceTopology(context.Background(), query.TopologyOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}

	services := map[string]query.ServiceNode{}
	for _, node := range result.Services {
		services[node.Repository+"/"+node.Component] = node
	}
	client := services["monorepo/client"]
	server := services["monorepo/server"]
	fallback := services["monorepo/"]
	if client.ComponentID != "n:component-client" || client.Label != "monorepo/client" {
		t.Fatalf("client component service identity = %#v", client)
	}
	if server.ComponentID != "n:component-server" || server.Label != "monorepo/server" {
		t.Fatalf("server component service identity = %#v", server)
	}
	if fallback.ID != "service:monorepo" || fallback.Component != "" || fallback.ComponentID != "" {
		t.Fatalf("unassigned evidence did not retain repository fallback: %#v", fallback)
	}
	if client.ID == server.ID || client.ID == fallback.ID || server.ID == fallback.ID {
		t.Fatalf("component and fallback service IDs collided: %#v", result.Services)
	}

	links := map[string]query.ServiceLink{}
	for _, link := range result.Links {
		links[string(link.Kind)+":"+link.Name] = link
	}
	httpLink := links["http:GET /orders"]
	if httpLink.FromServiceID != client.ID || httpLink.ToServiceID != server.ID {
		t.Fatalf("HTTP component boundary = %#v", httpLink)
	}
	if len(httpLink.SourceNodes) != 1 || httpLink.SourceNodes[0].Component != "client" ||
		httpLink.SourceNodes[0].ComponentID != client.ComponentID ||
		len(httpLink.TargetNodes) != 1 || httpLink.TargetNodes[0].Component != "server" {
		t.Fatalf("HTTP resources lost component identity: %#v", httpLink)
	}
	eventLink := links["event:order.placed"]
	if eventLink.FromServiceID != client.ID || eventLink.ToServiceID != server.ID {
		t.Fatalf("event component boundary = %#v", eventLink)
	}
	selfLink := links["event:client.refreshed"]
	if selfLink.FromServiceID != client.ID || selfLink.ToServiceID != client.ID {
		t.Fatalf("same-component event did not remain a self-link: %#v", selfLink)
	}
	fallbackLink := links["http:GET /health"]
	if fallbackLink.FromServiceID != client.ID || fallbackLink.ToServiceID != fallback.ID {
		t.Fatalf("component-to-unassigned boundary = %#v", fallbackLink)
	}
}

func TestServiceTopologyFiltersExactComponentScope(t *testing.T) {
	service := query.NewTopology(newComponentTopologyFixture())
	tests := []struct {
		name    string
		options query.TopologyOptions
		links   int
	}{
		{name: "repository", options: query.TopologyOptions{Repository: "monorepo", Limit: 20}, links: 4},
		{name: "component", options: query.TopologyOptions{Component: "server", Limit: 20}, links: 2},
		{name: "combined", options: query.TopologyOptions{Repository: "monorepo", Component: "client", Direction: query.Outgoing, Limit: 20}, links: 4},
		{name: "incoming", options: query.TopologyOptions{Component: "server", Direction: query.Incoming, Limit: 20}, links: 2},
		{name: "unknown", options: query.TopologyOptions{Component: "missing", Limit: 20}, links: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := service.ServiceTopology(context.Background(), test.options)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Links) != test.links {
				t.Fatalf("links = %d, want %d: %#v", len(result.Links), test.links, result)
			}
		})
	}
	if _, err := service.ServiceTopology(context.Background(), query.TopologyOptions{Direction: query.Outgoing}); err == nil {
		t.Fatal("direction without repository or component scope succeeded")
	}
	bounded, err := service.ServiceTopology(context.Background(), query.TopologyOptions{Component: "client", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Links) != 1 || !bounded.Truncated {
		t.Fatalf("component scope lost link truncation: %#v", bounded)
	}
}

func TestTopologyJSONOmitsAbsentComponentIdentity(t *testing.T) {
	encoded, err := json.Marshal(query.ServiceTopology{Services: []query.ServiceNode{{
		ID: "service:shop", Repository: "shop", Label: "shop", Components: []query.Resource{{
			ID: "n:caller", Repository: "shop", Kind: graph.KindFunction, Name: "Caller",
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"component"`) || strings.Contains(string(encoded), `"component_id"`) {
		t.Fatalf("no-component JSON changed incompatibly: %s", encoded)
	}
}

func TestServiceTopologyKeepsDuplicateComponentNamesDistinctAcrossRepositories(t *testing.T) {
	repository := newComponentTopologyFixture()
	repository.repositories = append(repository.repositories, "peer")
	repository.add("peer", graph.Node{ID: "n:peer-component-client", Kind: graph.KindComponent, Name: "client",
		QualifiedName: "peer/client", OwnerFile: "__workspace__"})
	repository.add("peer", graph.Node{ID: "n:peer-file", Kind: graph.KindFile, Name: "client.go",
		QualifiedName: "peer/client.go", Location: graph.Location{Path: "client.go"}, OwnerFile: "client.go"})
	repository.add("peer", graph.Node{ID: "n:peer-subscriber", Kind: graph.KindFunction, Name: "PeerSubscriber",
		QualifiedName: "peer.PeerSubscriber", OwnerFile: "client.go"})
	repository.edges = append(repository.edges,
		graph.Edge{ID: "e:peer-membership", FromID: "n:peer-component-client", ToID: "n:peer-file", Kind: graph.EdgeContains},
		graph.Edge{ID: "e:peer-subscribe", FactID: "f:peer-subscribe", FromID: "n:peer-subscriber", ToID: "n:event-order", Kind: graph.EdgeSubscribes, Properties: map[string]string{"federated": "true"}},
	)

	result, err := query.NewTopology(repository).ServiceTopology(context.Background(), query.TopologyOptions{
		Component: "client", Event: "order.placed", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, node := range result.Services {
		if node.Component == "client" {
			ids[node.Repository] = node.ID
		}
	}
	if ids["monorepo"] == "" || ids["peer"] == "" || ids["monorepo"] == ids["peer"] {
		t.Fatalf("federated component identities collided: %#v", result.Services)
	}
}

func TestServiceTopologyRejectsCorruptMultipleComponentOwners(t *testing.T) {
	repository := newComponentTopologyFixture()
	repository.edges = append(repository.edges,
		graph.Edge{ID: "e:corrupt-membership", FromID: "n:component-server", ToID: "n:file-client", Kind: graph.EdgeContains})
	result, err := query.NewTopology(repository).ServiceTopology(context.Background(), query.TopologyOptions{Limit: 20})
	if err == nil {
		t.Fatalf("multiple component owners returned partial topology: %#v", result)
	}
	if len(result.Services) != 0 || len(result.Links) != 0 {
		t.Fatalf("corrupt ownership returned partial topology: %#v", result)
	}
}

func newComponentTopologyFixture() *catalogRepository {
	repository := &catalogRepository{nodes: map[string]graph.Node{}, owners: map[string]string{}, repositories: []string{"monorepo"}}
	for _, item := range []struct {
		id, name, path string
	}{
		{id: "n:component-client", name: "client", path: "client/client.go"},
		{id: "n:component-server", name: "server", path: "server/server.go"},
	} {
		repository.add("monorepo", graph.Node{ID: item.id, Kind: graph.KindComponent, Name: item.name,
			QualifiedName: "monorepo/" + item.name, OwnerFile: "__workspace__"})
		fileID := "n:file-" + item.name
		repository.add("monorepo", graph.Node{ID: fileID, Kind: graph.KindFile, Name: item.path,
			QualifiedName: "monorepo/" + item.path, Location: graph.Location{Path: item.path}, OwnerFile: item.path})
		repository.edges = append(repository.edges, graph.Edge{ID: "e:membership-" + item.name,
			FromID: item.id, ToID: fileID, Kind: graph.EdgeContains})
	}
	repository.add("monorepo", graph.Node{ID: "n:file-root", Kind: graph.KindFile, Name: "health.go",
		QualifiedName: "monorepo/health.go", Location: graph.Location{Path: "health.go"}, OwnerFile: "health.go"})
	repository.add("monorepo", graph.Node{ID: "n:client-call", Kind: graph.KindFunction, Name: "CallOrders",
		QualifiedName: "client.CallOrders", OwnerFile: "client/client.go"})
	repository.add("monorepo", graph.Node{ID: "n:server-endpoint", Kind: graph.KindEndpoint, Name: "GET /orders",
		QualifiedName: "endpoint:GET /orders@server/server.go:5", OwnerFile: "server/server.go",
		Properties: map[string]string{"method": "GET", "route": "/orders"}})
	repository.add("monorepo", graph.Node{ID: "n:health-endpoint", Kind: graph.KindEndpoint, Name: "GET /health",
		QualifiedName: "endpoint:GET /health@health.go:5", OwnerFile: "health.go",
		Properties: map[string]string{"method": "GET", "route": "/health"}})
	repository.add("monorepo", graph.Node{ID: "n:publisher", Kind: graph.KindFunction, Name: "PublishOrder",
		QualifiedName: "client.PublishOrder", OwnerFile: "client/client.go"})
	repository.add("monorepo", graph.Node{ID: "n:subscriber", Kind: graph.KindFunction, Name: "SubscribeOrder",
		QualifiedName: "server.SubscribeOrder", OwnerFile: "server/server.go"})
	repository.add("monorepo", graph.Node{ID: "n:client-subscriber", Kind: graph.KindFunction, Name: "RefreshClient",
		QualifiedName: "client.RefreshClient", OwnerFile: "client/client.go"})
	repository.add("monorepo", graph.Node{ID: "n:event-order", Kind: graph.KindEvent, Name: "order.placed",
		QualifiedName: "order.placed", OwnerFile: "server/server.go"})
	repository.add("monorepo", graph.Node{ID: "n:event-client", Kind: graph.KindEvent, Name: "client.refreshed",
		QualifiedName: "client.refreshed", OwnerFile: "client/client.go"})
	repository.edges = append(repository.edges,
		graph.Edge{ID: "e:request-orders", FactID: "f:request-orders", FromID: "n:client-call", ToID: "n:server-endpoint", Kind: graph.EdgeRequests,
			Properties: httpmodel.WithDestinationEvidence(nil, httpmodel.DestinationResolved, httpmodel.EvidenceRoute)},
		graph.Edge{ID: "e:request-health", FactID: "f:request-health", FromID: "n:client-call", ToID: "n:health-endpoint", Kind: graph.EdgeRequests,
			Properties: httpmodel.WithDestinationEvidence(nil, httpmodel.DestinationResolved, httpmodel.EvidenceRoute)},
		graph.Edge{ID: "e:publish-order", FactID: "f:publish-order", FromID: "n:publisher", ToID: "n:event-order", Kind: graph.EdgePublishes},
		graph.Edge{ID: "e:subscribe-order", FactID: "f:subscribe-order", FromID: "n:subscriber", ToID: "n:event-order", Kind: graph.EdgeSubscribes},
		graph.Edge{ID: "e:publish-client", FactID: "f:publish-client", FromID: "n:publisher", ToID: "n:event-client", Kind: graph.EdgePublishes},
		graph.Edge{ID: "e:subscribe-client", FactID: "f:subscribe-client", FromID: "n:client-subscriber", ToID: "n:event-client", Kind: graph.EdgeSubscribes},
	)
	return repository
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
	add("orders", graph.Node{ID: "n:middleware-z", Kind: graph.KindFunction, Name: "RequestID",
		QualifiedName: "orders.RequestID", Location: graph.Location{Path: "middleware.go", Line: 4}})
	add("orders", graph.Node{ID: "n:middleware-a", Kind: graph.KindFunction, Name: "Authenticate",
		QualifiedName: "orders.Authenticate", Location: graph.Location{Path: "middleware.go", Line: 8}})
	add("orders", graph.Node{ID: "n:middleware-external", Kind: graph.KindExternal, Name: "dynamicMiddleware",
		QualifiedName: "dynamicMiddleware", External: true, Properties: map[string]string{"unresolved": "true"}})

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
	add("payments-a", graph.Node{ID: "n:any-charge", Kind: graph.KindEndpoint, Name: "ANY /charge",
		QualifiedName: "endpoint:ANY /charge@a.go:9", Properties: map[string]string{"method": "ANY", "route": "/charge"}})
	add("payments-a", graph.Node{ID: "n:host-charge", Kind: graph.KindEndpoint, Name: "POST //api.example.test/charge",
		QualifiedName: "endpoint:POST //api.example.test/charge@a.go:10",
		Properties:    map[string]string{"method": "POST", "route": "/charge", "authority": "api.example.test"}})
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
		{ID: "e:middleware-0", FactID: "f:middleware-0", FromID: "n:endpoint-orders", ToID: "n:middleware-z",
			Kind: graph.EdgeUsesMiddleware, Location: graph.Location{Path: "routes.go", Line: 5},
			Properties: map[string]string{"form": "use", "order": "0", "resolution": "go/types"}},
		{ID: "e:middleware-1", FactID: "f:middleware-1", FromID: "n:endpoint-orders", ToID: "n:middleware-a",
			Kind: graph.EdgeUsesMiddleware, Location: graph.Location{Path: "routes.go", Line: 6},
			Properties: map[string]string{"form": "with", "order": "1", "resolution": "go/types"}},
		{ID: "e:middleware-2", FactID: "f:middleware-2", FromID: "n:endpoint-orders", ToID: "n:middleware-external",
			Kind: graph.EdgeUsesMiddleware, Location: graph.Location{Path: "routes.go", Line: 7},
			Properties: map[string]string{"form": "use", "order": "2", "resolution": "go/types", "unresolved": "true"}},
		{ID: "e:request-orders", FactID: "f:request-orders", FromID: "n:client-call", ToID: "n:endpoint-orders",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 10},
			Properties: httpmodel.WithDestinationEvidence(map[string]string{"federated": "true"},
				httpmodel.DestinationResolved, httpmodel.EvidenceFederated)},
		{ID: "e:request-charge", FactID: "f:request-charge", FromID: "n:client-charge", ToID: "n:external-charge",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 20},
			Properties: httpmodel.WithDestinationEvidence(nil, httpmodel.DestinationUnresolved, httpmodel.EvidenceRoute)},
		{ID: "e:request-charge-a", FactID: "f:request-charge", FromID: "n:client-charge", ToID: "n:charge-a",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 20},
			Properties: httpmodel.WithDestinationEvidence(map[string]string{"federated": "true"},
				httpmodel.DestinationResolved, httpmodel.EvidenceFederated)},
		{ID: "e:request-charge-b", FactID: "f:request-charge", FromID: "n:client-charge", ToID: "n:charge-b",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 20},
			Properties: httpmodel.WithDestinationEvidence(map[string]string{"federated": "true"},
				httpmodel.DestinationResolved, httpmodel.EvidenceFederated)},
		{ID: "e:request-missing", FactID: "f:request-missing", FromID: "n:client-missing", ToID: "n:external-missing",
			Kind: graph.EdgeRequests, Location: graph.Location{Path: "client.go", Line: 30},
			Properties: httpmodel.WithDestinationEvidence(nil, httpmodel.DestinationUnresolved, httpmodel.EvidenceRoute)},
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
