package federation_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestTopologyKeepsFederatedHTTPAmbiguityAndEventLinksExplicit(t *testing.T) {
	ctx := context.Background()
	workspace := testtemp.Dir(t)
	roots := map[string]string{}
	for _, name := range []string{"client", "payments-a", "payments-b", "consumer"} {
		root := filepath.Join(workspace, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, "go.mod"), "module example.com/"+name+"\n\ngo 1.26\n")
		roots[name] = root
	}
	enableChi(t, roots["payments-a"])
	enableChi(t, roots["payments-b"])
	write(t, filepath.Join(roots["client"], "client.go"), `package client
import "net/http"
type Bus interface { Publish(string) }
func Call(bus Bus) {
	_, _ = http.Post("/charge/42", "text/plain", nil)
	_, _ = http.Get("/orders/42?view=full")
	bus.Publish("order.placed")
}
`)
	write(t, filepath.Join(roots["payments-a"], "server.go"), `package paymentsa
import (
	"net/http"
	"github.com/go-chi/chi/v5"
)
func ChargeA(http.ResponseWriter, *http.Request) {}
func Orders(http.ResponseWriter, *http.Request) {}
func Routes() {
	router := chi.NewRouter()
	router.Post("/charge/{chargeID}", ChargeA)
	router.Get("/charge/{chargeID}", ChargeA)
	router.Get("/orders/{orderID}/", Orders)
}
`)
	write(t, filepath.Join(roots["payments-b"], "server.go"), `package paymentsb
import (
	"net/http"
	"github.com/go-chi/chi/v5"
)
func ChargeB(http.ResponseWriter, *http.Request) {}
func Routes() { router := chi.NewRouter(); router.Post("/charge/{id}", ChargeB) }
`)
	write(t, filepath.Join(roots["consumer"], "consumer.go"), `package consumer
type Bus interface { Subscribe(string) }
func Watch(bus Bus) { bus.Subscribe("order.placed") }
`)
	for _, name := range []string{"client", "payments-a", "payments-b", "consumer"} {
		index(t, ctx, roots[name])
	}

	repository, err := federation.Open(ctx, []string{
		roots["client"], roots["payments-a"], roots["payments-b"], roots["consumer"],
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := query.NewTopology(repository)

	requests, err := service.OutboundRequests(ctx, query.TopologyOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]query.OutboundRequest{}
	for _, request := range requests.Requests {
		byName[request.Method+" "+request.Route] = request
	}
	charge := byName["POST /charge/42"]
	if charge.Status != query.BoundaryAmbiguous || len(charge.Candidates) != 2 {
		t.Fatalf("duplicate federated endpoints were not reported as ambiguous: %#v; all requests: %#v", charge, requests.Requests)
	}
	for _, candidate := range charge.Candidates {
		if candidate.Method != http.MethodPost {
			t.Fatalf("method-incompatible endpoint became a candidate: %#v", charge.Candidates)
		}
	}
	chargeCall, err := query.NewService(repository).Neighborhood(ctx, "example.com/client.Call", "", 1,
		query.Outgoing, []graph.EdgeKind{graph.EdgeRequests}, 20)
	if err != nil {
		t.Fatal(err)
	}
	chargeEdges := 0
	for _, edge := range chargeCall.Edges {
		if edge.Properties["http_method"] != http.MethodPost {
			continue
		}
		chargeEdges++
		if edge.Properties[httpmodel.PropertyDestinationResolution] != string(httpmodel.DestinationAmbiguous) ||
			edge.Properties[httpmodel.PropertyDestinationEvidence] != string(httpmodel.EvidenceFederated) {
			t.Fatalf("generic adjacency did not preserve federated ambiguity: %#v", edge)
		}
		for _, node := range chargeCall.Nodes {
			if node.Node.ID == edge.ToID && !node.Node.External {
				t.Fatalf("ambiguous request fanned out to local endpoint: edge=%#v node=%#v", edge, node)
			}
		}
	}
	if chargeEdges != 1 {
		t.Fatalf("ambiguous generic adjacency edges = %d, want one unresolved edge: %#v", chargeEdges, chargeCall)
	}
	orders := byName["GET /orders/42"]
	if orders.Status != query.BoundaryResolved || orders.Destination.Repository != "payments-a" ||
		!orders.Evidence.Federated {
		t.Fatalf("unique federated endpoint did not resolve: %#v", orders)
	}
	incoming, err := repository.EdgesTo(ctx, orders.Destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	var incomingRequest graph.Edge
	for _, edge := range incoming {
		if edge.Kind == graph.EdgeRequests && edge.Properties["http_route"] == "/orders/42" {
			incomingRequest = edge
		}
	}
	if incomingRequest.ID == "" || incomingRequest.Properties["federated"] != "true" {
		t.Fatalf("incoming adjacency did not apply template route projection: %#v", incoming)
	}
	incomingRelations, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: orders.Destination.ID,
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeRequests}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	foundIncomingRelation := false
	for _, item := range incomingRelations.Items {
		if item.Edge.Properties["http_route"] == "/orders/42" && item.Edge.Properties["federated"] == "true" {
			foundIncomingRelation = true
		}
	}
	if !foundIncomingRelation {
		t.Fatalf("incoming relation page did not apply template route projection: %#v", incomingRelations)
	}

	topology, err := service.ServiceTopology(ctx, query.TopologyOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var eventFound, resolvedOrders bool
	for _, link := range topology.Links {
		switch {
		case link.Kind == query.LinkHTTP && link.Name == "GET /orders/42":
			resolvedOrders = link.Status == query.BoundaryResolved && link.Federated
		case link.Kind == query.LinkEvent && link.Name == "order.placed":
			eventFound = link.FromServiceID == "service:client" &&
				link.ToServiceID == "service:consumer" && link.Federated && len(link.EventIDs) > 0
		case link.Kind == query.LinkHTTP && link.Name == "POST /charge/42" && link.Status == query.BoundaryResolved:
			t.Fatalf("ambiguous HTTP request created a confirmed service link: %#v", link)
		}
	}
	if !resolvedOrders || !eventFound {
		t.Fatalf("topology lost federated HTTP or event links: %#v", topology.Links)
	}
}
