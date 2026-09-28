package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/query"
)

func TestTopologyCommandsExposeEndpointsRequestsHandlersAndServices(t *testing.T) {
	root := topologyFixture(t)
	run(t, "index", root)

	var endpoints query.EndpointList
	runJSON(t, &endpoints, "endpoints", "--repo", root, "--method", "GET", "--route", "/orders", "--json")
	if len(endpoints.Endpoints) != 1 || endpoints.Endpoints[0].HandlerStatus != query.BoundaryResolved {
		t.Fatalf("endpoint command lost its handler: %#v", endpoints)
	}
	if endpoints.Endpoints[0].Location.Path != "server.go" {
		t.Fatalf("endpoint command lost source evidence: %#v", endpoints.Endpoints[0])
	}
	if len(endpoints.Endpoints[0].Middleware) != 1 ||
		endpoints.Endpoints[0].Middleware[0].Node.QualifiedName != "example.com/topology.Authenticate" {
		t.Fatalf("endpoint command lost middleware evidence: %#v", endpoints.Endpoints[0])
	}
	if output := run(t, "endpoints", "--repo", root, "--method", "GET", "--route", "/orders"); !strings.Contains(output, "middleware") || !strings.Contains(output, "example.com/topology.Authenticate") {
		t.Fatalf("human endpoint output lost middleware evidence: %s", output)
	}

	var requests query.OutboundRequestList
	runJSON(t, &requests, "outbound-requests", "--repo", root, "--method", "GET", "--json")
	if len(requests.Requests) != 1 || requests.Requests[0].Status != query.BoundaryResolved {
		t.Fatalf("outbound request command did not resolve the local endpoint: %#v", requests)
	}

	var handlers query.HandlerList
	runJSON(t, &handlers, "find-handler", "--repo", root, "--route", "/orders", "--json")
	if len(handlers.Matches) != 1 || len(handlers.Matches[0].Handlers) != 1 ||
		handlers.Matches[0].Handlers[0].Node.Name != "Handler" {
		t.Fatalf("handler command returned the wrong graph evidence: %#v", handlers)
	}

	var topology query.ServiceTopology
	runJSON(t, &topology, "service-topology", "--repo", root, "--route", "/orders", "--json")
	if len(topology.Services) != 1 || len(topology.Links) != 1 || topology.Links[0].Kind != query.LinkHTTP {
		t.Fatalf("service topology command returned the wrong graph: %#v", topology)
	}
	mermaid := run(t, "service-topology", "--repo", root, "--route", "/orders", "--mermaid")
	if !strings.HasPrefix(mermaid, "flowchart LR\n") || !strings.Contains(mermaid, "GET /orders") {
		t.Fatalf("service topology did not render Mermaid: %s", mermaid)
	}
}

func TestTopologyCommandsValidateFiltersAndBounds(t *testing.T) {
	root := topologyFixture(t)
	run(t, "index", root)
	if code, _, stderr := execute(t, "service-topology", "--repo", root, "--direction", "sideways"); code == 0 {
		t.Fatalf("invalid direction was accepted: %s", stderr)
	}
	if code, _, stderr := execute(t, "service-topology", "--repo", root, "--direction", "outgoing"); code == 0 {
		t.Fatalf("direction without --repo-name was accepted: %s", stderr)
	}
	if code, _, stderr := execute(t, "endpoints", "--repo", root, "--event", "order.placed"); code == 0 {
		t.Fatalf("an inert endpoint event filter was accepted: %s", stderr)
	}
	if code, _, stderr := execute(t, "service-topology", "--repo", root, "--json", "--mermaid"); code == 0 {
		t.Fatalf("two output formats were accepted: %s", stderr)
	}
	if code, _, stderr := execute(t, "outbound-requests", "--repo", root, "--limit", "0"); code == 0 {
		t.Fatalf("a non-positive bound was accepted: %s", stderr)
	}
}

func TestServiceTopologyCommandFiltersByIndexedComponent(t *testing.T) {
	root := componentTopologyFixture(t)
	run(t, "index", root)

	var topology query.ServiceTopology
	runJSON(t, &topology, "service-topology", "--repo", root, "--component", "client",
		"--direction", "outgoing", "--json")
	if len(topology.Links) != 1 || len(topology.Services) != 2 {
		t.Fatalf("component-scoped topology = %#v", topology)
	}
	services := map[string]query.ServiceNode{}
	for _, service := range topology.Services {
		services[service.Component] = service
	}
	if services["client"].ComponentID == "" || services["server"].ComponentID == "" ||
		services["client"].ID == services["server"].ID {
		t.Fatalf("component identities missing from CLI JSON: %#v", topology.Services)
	}

	var unknown query.ServiceTopology
	runJSON(t, &unknown, "service-topology", "--repo", root, "--component", "missing", "--json")
	if len(unknown.Services) != 0 || len(unknown.Links) != 0 {
		t.Fatalf("unknown component fell back to repository topology: %#v", unknown)
	}
}

func TestTopologyRefusesAMixedFreshnessFederation(t *testing.T) {
	indexed := topologyFixture(t)
	run(t, "index", indexed)
	unindexed := topologyFixture(t)
	code, stdout, stderr := execute(t, "service-topology", "--repos", indexed+","+unindexed, "--json")
	if code == 0 {
		t.Fatalf("a federation that cannot be refreshed must not answer: %s", stdout)
	}
	if stdout != "" {
		t.Fatalf("partial topology was written before the refresh failure: %s", stdout)
	}
	if !strings.Contains(stderr, "has no index") {
		t.Fatalf("unexpected federation failure: %s", stderr)
	}
}

func topologyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(`module example.com/topology

go 1.26

require github.com/go-chi/chi/v5 v5.0.0

replace github.com/go-chi/chi/v5 => ./third_party/chi
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "third_party", "chi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "third_party", "chi", "go.mod"),
		[]byte("module github.com/go-chi/chi/v5\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "third_party", "chi", "chi.go"), []byte(`package chi
import "net/http"
type Router interface {
	http.Handler
	Use(...func(http.Handler) http.Handler)
	Get(string, http.HandlerFunc)
}
type Mux struct{}
func NewRouter() *Mux { return &Mux{} }
func (*Mux) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*Mux) Use(...func(http.Handler) http.Handler) {}
func (*Mux) Get(string, http.HandlerFunc) {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	source := `package topology

import (
	"net/http"
	"github.com/go-chi/chi/v5"
)

func Authenticate(next http.Handler) http.Handler { return next }
func Handler(http.ResponseWriter, *http.Request) {}

func Routes() {
	router := chi.NewRouter()
	router.Use(Authenticate)
	router.Get("/orders", Handler)
}

func CallOrders() {
	_, _ = http.Get("/orders")
}
`
	if err := os.WriteFile(filepath.Join(root, "server.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func componentTopologyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/topology\n\ngo 1.26\n")
	write("grafo.yaml", "components:\n  - name: client\n    roots: [client]\n  - name: server\n    roots: [server]\n")
	write("client/client.go", `package client

import "net/http"

func CallOrders() {
	_, _ = http.Get("/orders")
}
`)
	write("server/server.go", `package server

func Handler() {}

func Routes() {
	router.Get("/orders", Handler)
}
`)
	return root
}
