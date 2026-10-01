package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestRequestResolutionHonorsAuthorityAndIsolatesCacheEntries(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	endpoint := graph.Node{ID: "endpoint-users", Kind: graph.KindEndpoint, Name: "GET /users/{id}",
		QualifiedName: "endpoint:GET /users/{id}@server.go:1", OwnerFile: "server.go",
		Properties: map[string]string{"method": "GET", "route": "/users/{id}"}}
	sources := []graph.Node{
		{ID: "source-unknown", Kind: graph.KindFunction, Name: "Unknown", QualifiedName: "client.Unknown", OwnerFile: "client.go"},
		{ID: "source-local", Kind: graph.KindFunction, Name: "Local", QualifiedName: "client.Local", OwnerFile: "client.go"},
		{ID: "source-external", Kind: graph.KindFunction, Name: "External", QualifiedName: "client.External", OwnerFile: "client.go"},
		{ID: "source-proven", Kind: graph.KindFunction, Name: "Proven", QualifiedName: "client.Proven", OwnerFile: "client.go"},
	}
	facts := []graph.Fact{
		{ID: "a-unknown", FromID: sources[0].ID, Kind: graph.EdgeRequests, Target: "GET /users/42", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: requestProperties("/users/42", map[string]string{"http_authority_unknown": "true"})},
		{ID: "b-local", FromID: sources[1].ID, Kind: graph.EdgeRequests, Target: "GET /users/42", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: requestProperties("/users/42", nil)},
		{ID: "c-external", FromID: sources[2].ID, Kind: graph.EdgeRequests, Target: "GET /users/42", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: requestProperties("/users/42", map[string]string{"http_authority": "api.example.test", "http_scheme": "https"})},
		{ID: "d-proven", FromID: sources[3].ID, Kind: graph.EdgeRequests, TargetID: endpoint.ID, TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: requestProperties("/users/42", map[string]string{"http_authority_unknown": "true"})},
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: sources, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "server.go", graph.ParseResult{Nodes: []graph.Node{endpoint}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	assertRequestResolution(t, ctx, repository, sources[0].ID, true, httpmodel.DestinationUnresolved, httpmodel.EvidenceUnknownAuthority)
	assertRequestResolution(t, ctx, repository, sources[1].ID, false, httpmodel.DestinationResolved, httpmodel.EvidenceRoute)
	assertRequestResolution(t, ctx, repository, sources[2].ID, true, httpmodel.DestinationExternal, httpmodel.EvidenceExplicitAuthority)
	assertRequestResolution(t, ctx, repository, sources[3].ID, false, httpmodel.DestinationResolved, httpmodel.EvidenceExactTarget)
}

func TestRequestResolutionPersistsAmbiguity(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	source := graph.Node{ID: "source", Kind: graph.KindFunction, Name: "Call", QualifiedName: "client.Call", OwnerFile: "client.go"}
	fact := graph.Fact{ID: "request", FromID: source.ID, Kind: graph.EdgeRequests, Target: "GET /teams/blue",
		TargetKind: graph.KindEndpoint, OwnerFile: "client.go", Properties: requestProperties("/teams/blue", nil)}
	endpoints := []graph.Node{
		{ID: "team-a", Kind: graph.KindEndpoint, Name: "GET /teams/{id}", QualifiedName: "endpoint:a", OwnerFile: "a.go", Properties: map[string]string{"method": "GET", "route": "/teams/{id}"}},
		{ID: "team-b", Kind: graph.KindEndpoint, Name: "GET /teams/{teamID}", QualifiedName: "endpoint:b", OwnerFile: "b.go", Properties: map[string]string{"method": "GET", "route": "/teams/{teamID}"}},
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: []graph.Node{source}, Facts: []graph.Fact{fact}}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range endpoints {
		if err := repository.ReplaceOwner(ctx, endpoint.OwnerFile, graph.ParseResult{Nodes: []graph.Node{endpoint}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertRequestResolution(t, ctx, repository, source.ID, true, httpmodel.DestinationAmbiguous, httpmodel.EvidenceRoute)
}

func TestRequestResolutionRanksExactMethodOverANYAndExcludesHost(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	endpoints := []graph.Node{
		{ID: "any", Kind: graph.KindEndpoint, Name: "ANY /items/42", QualifiedName: "endpoint:any", OwnerFile: "server.go",
			Properties: map[string]string{"method": "ANY", "route": "/items/42"}},
		{ID: "get", Kind: graph.KindEndpoint, Name: "GET /items/{id}", QualifiedName: "endpoint:get", OwnerFile: "server.go",
			Properties: map[string]string{"method": "GET", "route": "/items/{id}"}},
		{ID: "host", Kind: graph.KindEndpoint, Name: "GET //api.example.test/items/42", QualifiedName: "endpoint:host", OwnerFile: "server.go",
			Properties: map[string]string{"method": "GET", "route": "/items/42", "authority": "api.example.test"}},
		{ID: "fallback-a", Kind: graph.KindEndpoint, Name: "ANY /fallback/{id}", QualifiedName: "endpoint:fallback-a", OwnerFile: "server.go",
			Properties: map[string]string{"method": "ANY", "route": "/fallback/{id}"}},
		{ID: "fallback-b", Kind: graph.KindEndpoint, Name: "ANY /fallback/{name}", QualifiedName: "endpoint:fallback-b", OwnerFile: "server.go",
			Properties: map[string]string{"method": "ANY", "route": "/fallback/{name}"}},
	}
	sources := []graph.Node{
		{ID: "get-source", Kind: graph.KindFunction, Name: "Get", QualifiedName: "client.Get", OwnerFile: "client.go"},
		{ID: "post-source", Kind: graph.KindFunction, Name: "Post", QualifiedName: "client.Post", OwnerFile: "client.go"},
		{ID: "exact-host-source", Kind: graph.KindFunction, Name: "ExactHost", QualifiedName: "client.ExactHost", OwnerFile: "client.go"},
		{ID: "fallback-source", Kind: graph.KindFunction, Name: "Fallback", QualifiedName: "client.Fallback", OwnerFile: "client.go"},
	}
	facts := []graph.Fact{
		{ID: "get-request", FromID: sources[0].ID, Kind: graph.EdgeRequests, Target: "GET /items/42", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: requestProperties("/items/42", nil)},
		{ID: "post-request", FromID: sources[1].ID, Kind: graph.EdgeRequests, Target: "POST /items/42", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: map[string]string{"http_method": "POST", "http_route": "/items/42", "http_raw_route": "/items/42"}},
		{ID: "exact-host-request", FromID: sources[2].ID, Kind: graph.EdgeRequests, TargetID: endpoints[2].ID,
			Target: "GET /items/42", TargetKind: graph.KindEndpoint, OwnerFile: "client.go", Properties: requestProperties("/items/42", nil)},
		{ID: "fallback-request", FromID: sources[3].ID, Kind: graph.EdgeRequests, Target: "POST /fallback/42",
			TargetKind: graph.KindEndpoint, OwnerFile: "client.go",
			Properties: map[string]string{"http_method": "POST", "http_route": "/fallback/42", "http_raw_route": "/fallback/42"}},
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: sources, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "server.go", graph.ParseResult{Nodes: endpoints}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	assertResolvedTarget(t, ctx, repository, sources[0].ID, endpoints[1].ID)
	assertResolvedTarget(t, ctx, repository, sources[1].ID, endpoints[0].ID)
	assertRequestResolution(t, ctx, repository, sources[2].ID, true,
		httpmodel.DestinationUnresolved, httpmodel.EvidenceRoute)
	assertRequestResolution(t, ctx, repository, sources[3].ID, true,
		httpmodel.DestinationAmbiguous, httpmodel.EvidenceRoute)
}

func TestRequestResolutionPreservesCaseSensitiveMethods(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	endpoints := []graph.Node{
		{ID: "lower", Kind: graph.KindEndpoint, Name: "get /case", QualifiedName: "endpoint:lower", OwnerFile: "server.go",
			Properties: map[string]string{"method": "get", "route": "/case"}},
		{ID: "upper", Kind: graph.KindEndpoint, Name: "GET /case", QualifiedName: "endpoint:upper", OwnerFile: "server.go",
			Properties: map[string]string{"method": "GET", "route": "/case"}},
	}
	sources := []graph.Node{
		{ID: "lower-source", Kind: graph.KindFunction, Name: "Lower", QualifiedName: "client.Lower", OwnerFile: "client.go"},
		{ID: "upper-source", Kind: graph.KindFunction, Name: "Upper", QualifiedName: "client.Upper", OwnerFile: "client.go"},
		{ID: "exact-lower-source", Kind: graph.KindFunction, Name: "ExactLower", QualifiedName: "client.ExactLower", OwnerFile: "client.go"},
	}
	facts := []graph.Fact{
		{ID: "lower-request", FromID: sources[0].ID, Kind: graph.EdgeRequests, Target: "get /case", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: map[string]string{"http_method": "get", "http_route": "/case"}},
		{ID: "upper-request", FromID: sources[1].ID, Kind: graph.EdgeRequests, Target: "GET /case", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: map[string]string{"http_method": "GET", "http_route": "/case"}},
		{ID: "exact-lower-request", FromID: sources[2].ID, Kind: graph.EdgeRequests, TargetID: endpoints[0].ID,
			Target: "get /case", TargetKind: graph.KindEndpoint, OwnerFile: "client.go",
			Properties: map[string]string{"http_method": "get", "http_route": "/case"}},
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: sources, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "server.go", graph.ParseResult{Nodes: endpoints}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertResolvedTarget(t, ctx, repository, sources[0].ID, endpoints[0].ID)
	assertResolvedTarget(t, ctx, repository, sources[1].ID, endpoints[1].ID)
	assertResolvedTarget(t, ctx, repository, sources[2].ID, endpoints[0].ID)
}

func assertResolvedTarget(t *testing.T, ctx context.Context, repository *Repository, sourceID, targetID string) {
	t.Helper()
	edges, err := repository.EdgesFrom(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].ToID != targetID ||
		edges[0].Properties[httpmodel.PropertyDestinationResolution] != string(httpmodel.DestinationResolved) {
		t.Fatalf("resolved edge from %s = %#v, want target %s", sourceID, edges, targetID)
	}
}

func TestRequestResolutionReconcilesLegacyUnsafeLocalEdge(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	source := graph.Node{ID: "source", Kind: graph.KindFunction, Name: "Call", QualifiedName: "client.Call", OwnerFile: "client.go"}
	endpoint := graph.Node{ID: "endpoint", Kind: graph.KindEndpoint, Name: "GET /users", QualifiedName: "endpoint:users", OwnerFile: "server.go",
		Properties: map[string]string{"method": "GET", "route": "/users"}}
	fact := graph.Fact{ID: "request", FromID: source.ID, Kind: graph.EdgeRequests, Target: "GET /users", TargetKind: graph.KindEndpoint,
		OwnerFile: "client.go", Properties: requestProperties("/users", map[string]string{"http_authority_unknown": "true"})}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: []graph.Node{source}, Facts: []graph.Fact{fact}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "server.go", graph.ParseResult{Nodes: []graph.Node{endpoint}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	legacyProperties, err := json.Marshal(fact.Properties)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.db.ExecContext(ctx, `UPDATE edges SET to_id = ?, properties = ? WHERE fact_id = ?`, endpoint.ID, legacyProperties, fact.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.queries.MarkDirtyOwner(ctx, fact.OwnerFile); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertRequestResolution(t, ctx, repository, source.ID, true, httpmodel.DestinationUnresolved, httpmodel.EvidenceUnknownAuthority)
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertRequestResolution(t, ctx, repository, source.ID, true, httpmodel.DestinationUnresolved, httpmodel.EvidenceUnknownAuthority)
}

func TestRequestResolutionRejectsInvalidExactTargets(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	sources := []graph.Node{
		{ID: "source-function", Kind: graph.KindFunction, Name: "WrongFunction", QualifiedName: "client.WrongFunction", OwnerFile: "client.go"},
		{ID: "source-type", Kind: graph.KindFunction, Name: "WrongType", QualifiedName: "client.WrongType", OwnerFile: "client.go"},
		{ID: "source-route", Kind: graph.KindFunction, Name: "WrongRoute", QualifiedName: "client.WrongRoute", OwnerFile: "client.go"},
	}
	wrongFunction := graph.Node{ID: "not-endpoint-function", Kind: graph.KindFunction, Name: "Handler", QualifiedName: "server.Handler", OwnerFile: "server.go"}
	wrongType := graph.Node{ID: "not-endpoint-type", Kind: graph.KindType, Name: "Response", QualifiedName: "server.Response", OwnerFile: "server.go"}
	wrongRoute := graph.Node{ID: "wrong-route", Kind: graph.KindEndpoint, Name: "POST /other", QualifiedName: "endpoint:POST /other@server.go:1",
		OwnerFile: "server.go", Properties: map[string]string{"method": "POST", "route": "/other"}}
	facts := []graph.Fact{
		{ID: "request-function", FromID: sources[0].ID, Kind: graph.EdgeRequests, TargetID: wrongFunction.ID, Target: "GET /users/42",
			TargetKind: graph.KindEndpoint, OwnerFile: "client.go", Properties: requestProperties("/users/42", map[string]string{"http_authority_unknown": "true"})},
		{ID: "request-type", FromID: sources[1].ID, Kind: graph.EdgeRequests, TargetID: wrongType.ID, Target: "GET /users/42",
			TargetKind: graph.KindEndpoint, OwnerFile: "client.go", Properties: requestProperties("/users/42", nil)},
		{ID: "request-route", FromID: sources[2].ID, Kind: graph.EdgeRequests, TargetID: wrongRoute.ID, Target: "GET /users/42",
			TargetKind: graph.KindEndpoint, OwnerFile: "client.go", Properties: requestProperties("/users/42", nil)},
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: sources, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "server.go", graph.ParseResult{Nodes: []graph.Node{wrongFunction, wrongType, wrongRoute}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertRequestResolution(t, ctx, repository, sources[0].ID, true,
		httpmodel.DestinationUnresolved, httpmodel.EvidenceUnknownAuthority)
	for _, source := range sources[1:] {
		assertRequestResolution(t, ctx, repository, source.ID, true,
			httpmodel.DestinationUnresolved, httpmodel.EvidenceRoute)
	}
}

func TestExternalRequestEdgesPagesHydratedBoundaries(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(testtemp.Dir(t), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()

	sources := []graph.Node{
		{ID: "source-a", Kind: graph.KindFunction, Name: "A", QualifiedName: "client.A", OwnerFile: "client.go"},
		{ID: "source-b", Kind: graph.KindFunction, Name: "B", QualifiedName: "client.B", OwnerFile: "client.go"},
	}
	facts := []graph.Fact{
		{ID: "request-a", FromID: sources[0].ID, Kind: graph.EdgeRequests, Target: "GET /a", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: requestProperties("/a", nil)},
		{ID: "request-b", FromID: sources[1].ID, Kind: graph.EdgeRequests, Target: "GET /b", TargetKind: graph.KindEndpoint,
			OwnerFile: "client.go", Properties: requestProperties("/b", nil)},
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: sources, Facts: facts}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	first, err := repository.ExternalRequestEdges(ctx, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Next == "" || first.Items[0].Source.ID == "" || !first.Items[0].Target.External {
		t.Fatalf("first external request page = %#v", first)
	}
	second, err := repository.ExternalRequestEdges(ctx, first.Next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Next != "" || second.Items[0].Edge.ID == first.Items[0].Edge.ID ||
		second.Items[0].Source.ID == "" || !second.Items[0].Target.External {
		t.Fatalf("second external request page = %#v", second)
	}
}

func requestProperties(route string, extra map[string]string) map[string]string {
	properties := map[string]string{"http_method": "GET", "http_route": route, "http_raw_route": route}
	for key, value := range extra {
		properties[key] = value
	}
	return properties
}

func assertRequestResolution(t *testing.T, ctx context.Context, repository *Repository, sourceID string,
	external bool, resolution httpmodel.DestinationResolution, evidence httpmodel.DestinationEvidence,
) {
	t.Helper()
	edges, err := repository.EdgesFrom(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("request edges from %s = %#v", sourceID, edges)
	}
	target, err := repository.Node(ctx, edges[0].ToID)
	if err != nil {
		t.Fatal(err)
	}
	if target.External != external || edges[0].Properties[httpmodel.PropertyDestinationResolution] != string(resolution) ||
		edges[0].Properties[httpmodel.PropertyDestinationEvidence] != string(evidence) {
		t.Fatalf("request from %s target=%#v edge=%#v", sourceID, target, edges[0])
	}
}
