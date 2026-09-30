package federation

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type countingHTTPRepository struct {
	*sqlite.Repository
	localEndpointLoads   int
	externalRequestLoads int
}

func (r *countingHTTPRepository) ListNodesByKind(ctx context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	if len(request.Kinds) == 1 && request.Kinds[0] == graph.KindEndpoint &&
		(request.Visibility == "" || request.Visibility == graph.LocalNodes) {
		r.localEndpointLoads++
	}
	return r.Repository.ListNodesByKind(ctx, request)
}

func (r *countingHTTPRepository) ExternalRequestEdges(ctx context.Context, after string, limit int) (graph.ExternalRequestEdgePage, error) {
	r.externalRequestLoads++
	return r.Repository.ExternalRequestEdges(ctx, after, limit)
}

func TestHTTPProjectionLoadsEachMemberSnapshotOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	client := openHTTPProjectionMember(t, ctx, filepath.Join(dir, "client.sqlite"), filepath.Join(dir, "client"))
	server := openHTTPProjectionMember(t, ctx, filepath.Join(dir, "server.sqlite"), filepath.Join(dir, "server"))

	sources := []graph.Node{
		{ID: "client-source-users", Kind: graph.KindFunction, Name: "Users", QualifiedName: "client.Users", OwnerFile: "client.go"},
		{ID: "client-source-orders", Kind: graph.KindFunction, Name: "Orders", QualifiedName: "client.Orders", OwnerFile: "client.go"},
	}
	facts := []graph.Fact{
		{ID: "client-request-users", FromID: sources[0].ID, Kind: graph.EdgeRequests, Target: "GET /users/42",
			TargetKind: graph.KindEndpoint, OwnerFile: "client.go", Properties: httpRequestProperties("/users/42")},
		{ID: "client-request-orders", FromID: sources[1].ID, Kind: graph.EdgeRequests, Target: "GET /orders/42",
			TargetKind: graph.KindEndpoint, OwnerFile: "client.go", Properties: httpRequestProperties("/orders/42")},
	}
	if err := client.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: sources, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := client.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	endpoints := []graph.Node{
		{ID: "server-endpoint-users", Kind: graph.KindEndpoint, Name: "GET /users/{id}", QualifiedName: "endpoint:users",
			OwnerFile: "server.go", Properties: map[string]string{"method": "GET", "route": "/users/{id}"}},
		{ID: "server-endpoint-orders", Kind: graph.KindEndpoint, Name: "GET /orders/{id}", QualifiedName: "endpoint:orders",
			OwnerFile: "server.go", Properties: map[string]string{"method": "GET", "route": "/orders/{id}"}},
	}
	if err := server.ReplaceOwner(ctx, "server.go", graph.ParseResult{Nodes: endpoints}); err != nil {
		t.Fatal(err)
	}
	if err := server.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	clientCounter := &countingHTTPRepository{Repository: client}
	serverCounter := &countingHTTPRepository{Repository: server}
	repository := &Repository{members: []member{
		{project: indexer.Project{Name: "client", Root: filepath.Join(dir, "client")}, repository: clientCounter},
		{project: indexer.Project{Name: "server", Root: filepath.Join(dir, "server")}, repository: serverCounter},
	}}
	defer func() { _ = repository.Close() }()

	for _, endpoint := range endpoints {
		edges, err := repository.EdgesTo(ctx, endpoint.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(edges) != 1 || edges[0].Properties["federated"] != "true" {
			t.Fatalf("incoming edges for %s = %#v", endpoint.ID, edges)
		}
		page, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: endpoint.ID,
			Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeRequests}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].Counterpart.ID == "" {
			t.Fatalf("incoming relations for %s = %#v", endpoint.ID, page)
		}
	}

	for name, counted := range map[string]*countingHTTPRepository{"client": clientCounter, "server": serverCounter} {
		if counted.localEndpointLoads != 1 || counted.externalRequestLoads != 1 {
			t.Fatalf("%s projection loads: local endpoints=%d external request pages=%d, want 1 each",
				name, counted.localEndpointLoads, counted.externalRequestLoads)
		}
	}
}

func openHTTPProjectionMember(t *testing.T, ctx context.Context, path, root string) *sqlite.Repository {
	t.Helper()
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "root", root); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	return repository
}

func httpRequestProperties(route string) map[string]string {
	return map[string]string{"http_method": "GET", "http_route": route, "http_raw_route": route}
}
