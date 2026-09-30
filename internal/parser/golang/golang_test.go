package golang_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
)

type fakeSemanticLoader struct {
	view golangparser.SemanticView
}

func (f fakeSemanticLoader) Load(context.Context, parserapi.Input) (golangparser.SemanticView, error) {
	return f.view, nil
}

func TestParserClassifiesOnlyValidGoTestDeclarations(t *testing.T) {
	content := []byte(`package widget
import check "testing"

func TestValid(t *check.T) {}
func BenchmarkValid(b *check.B) {}
func FuzzValid(f *check.F) {}
func ExampleWidget() { // Output:
}
func Testlower(t *check.T) {}
func TestWrong() {}
func BenchmarkWrong(b check.B) {}
func FuzzWrong(f *check.T) {}
func ExampleWrong(value string) {}
func ExampleNoOutput() {}
func helper() {}
type Suite struct{}
func (Suite) TestMethod(t *check.T) {}
`)
	result, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "widget_test.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTests := map[string]string{
		"TestValid": "test", "BenchmarkValid": "benchmark", "FuzzValid": "fuzz", "ExampleWidget": "example",
	}
	for _, node := range result.Nodes {
		if subtype, ok := wantTests[node.Name]; ok {
			if node.Kind != graph.KindTest || node.Properties["test_subtype"] != subtype ||
				node.Properties["test_framework"] != "go" || node.Properties["test_package"] != "internal" {
				t.Fatalf("valid test %s = %#v", node.Name, node)
			}
			delete(wantTests, node.Name)
		}
	}
	if len(wantTests) != 0 {
		t.Fatalf("missing test declarations: %#v", wantTests)
	}
	for _, name := range []string{"Testlower", "TestWrong", "BenchmarkWrong", "FuzzWrong", "ExampleWrong", "ExampleNoOutput", "TestMethod"} {
		node := nodeNamed(t, result.Nodes, name)
		if node.Kind == graph.KindTest {
			t.Fatalf("invalid test-like declaration %s classified as a test: %#v", name, node)
		}
	}
	helper := nodeNamed(t, result.Nodes, "helper")
	if helper.Kind != graph.KindFunction || helper.Properties["test_role"] != "helper" ||
		helper.Properties["test_package"] != "internal" {
		t.Fatalf("test helper metadata = %#v", helper)
	}

	external, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "external_test.go", Content: []byte("package widget_test\nimport \"testing\"\nfunc TestExternal(t *testing.T) {}\n"),
		Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	externalTest := nodeNamed(t, external.Nodes, "TestExternal")
	if externalTest.Kind != graph.KindTest || externalTest.Properties["test_package"] != "external" {
		t.Fatalf("external-package test identity = %#v", externalTest)
	}

	production, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "widget.go", Content: []byte("package widget\nimport \"testing\"\nfunc TestLooksValid(t *testing.T) {}\n"),
		Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if node := nodeNamed(t, production.Nodes, "TestLooksValid"); node.Kind != graph.KindFunction || node.Properties["test_role"] != "" {
		t.Fatalf("production test-like declaration = %#v", node)
	}
}

func nodeNamed(t *testing.T, nodes []graph.Node, name string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.Name == name {
			return node
		}
	}
	t.Fatalf("node %q not found: %#v", name, nodes)
	return graph.Node{}
}

func TestParserExtractsSymbolsAndWiring(t *testing.T) {
	content := []byte(`package api
import (
  clientpkg "example.com/client"
  "net/http"
  "os"
)
type Server struct{}
func (s *Server) Start(input string) string {
  copy := input
  client := clientpkg.NewClient()
  client.Send(copy)
  s.deliver(copy)
  token := os.Getenv("API_TOKEN")
  _ = token
  http.HandleFunc("/health", health)
  http.Get("/ready")
  publish("user.created")
  return copy
}

func (s *Server) deliver(string) {}
func health(http.ResponseWriter, *http.Request) {}
`)
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Path: "api/server.go", Content: content, Repository: "sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindMethod, "Start")
	assertHasNode(t, result.Nodes, graph.KindParameter, "input")
	assertHasNode(t, result.Nodes, graph.KindVariable, "copy")
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "ANY /health")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "API_TOKEN")
	assertHasFact(t, result.Facts, graph.EdgePublishes, "user.created")
	assertHasFact(t, result.Facts, graph.EdgeRequests, "GET /ready")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
	assertHasFact(t, result.Facts, graph.EdgePasses, "example.com/sample/api.Server.deliver")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/sample/api.Server.deliver")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/client.Client.Send")
}

func TestHTTPExtractionUsesCanonicalIdentityAndKeepsRawEvidence(t *testing.T) {
	content := []byte(`package api
import "net/http"
func Handler(http.ResponseWriter, *http.Request) {}
func Routes() {
	_, _ = http.Get("https://api.example.test/users/{id}/?expand=true#top")
	_, _ = http.Get("https://api.example.test/users/{id}/?expand=false#other")
}
`)
	parser := golangparser.NewWithSemanticLoader(fakeSemanticLoader{view: golangparser.SemanticView{
		Available: true, Included: true,
		ChiEndpoints: []golangparser.SemanticChiEndpoint{
			{Function: "example.com/sample/api.Routes", FunctionKind: graph.KindFunction, Method: "GET", Route: "/users/{characterID}/?view=full#details", Handler: "example.com/sample/api.Handler", HandlerKind: graph.KindFunction, Location: graph.Location{Path: "api/routes.go", Line: 4, Column: 2}},
			{Function: "example.com/sample/api.Routes", FunctionKind: graph.KindFunction, Method: "GET", Route: "/bad/%zz", Handler: "example.com/sample/api.Handler", HandlerKind: graph.KindFunction, Location: graph.Location{Path: "api/routes.go", Line: 4, Column: 2}},
		},
		HTTPRequests: []golangparser.SemanticHTTPRequest{
			{Function: "example.com/sample/api.Routes", FunctionKind: graph.KindFunction, Method: "GET", Route: "https://api.example.test/users/{id}/?expand=true#top", Sink: "net/http.Get", Source: "net/http.Get", Location: graph.Location{Path: "api/routes.go", Line: 5, Column: 9}},
			{Function: "example.com/sample/api.Routes", FunctionKind: graph.KindFunction, Method: "GET", Route: "https://api.example.test/users/{id}/?expand=false#other", Sink: "net/http.Get", Source: "net/http.Get", Location: graph.Location{Path: "api/routes.go", Line: 6, Column: 9}},
		},
	}})
	result, err := parser.Parse(context.Background(), parserapi.Input{
		Path: "api/routes.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	var endpoint graph.Node
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint && node.Properties["route"] == "/users/{_}" {
			endpoint = node
		}
	}
	if endpoint.ID == "" || endpoint.Properties["raw_route"] != "/users/{characterID}/?view=full#details" ||
		endpoint.Properties["query"] != "view=full" || endpoint.Properties["fragment"] != "details" {
		t.Fatalf("canonical endpoint or raw evidence missing: %#v", endpoint)
	}
	var requests []graph.Fact
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			requests = append(requests, fact)
		}
	}
	if len(requests) != 2 || requests[0].Target != "GET https://api.example.test/users/{_}" || requests[1].Target != requests[0].Target {
		t.Fatalf("absolute URL identity contains non-path evidence: %#v", requests)
	}
	foundFirstEvidence := false
	for _, request := range requests {
		if request.Properties["http_query"] == "expand=true" {
			foundFirstEvidence = request.Properties["http_route"] == "/users/{_}" &&
				request.Properties["http_authority"] == "api.example.test" && request.Properties["http_fragment"] == "top"
		}
	}
	if !foundFirstEvidence {
		t.Fatalf("raw request evidence missing: %#v", requests)
	}
	foundDiagnostic := false
	for _, diagnostic := range result.Diagnostics {
		foundDiagnostic = foundDiagnostic || strings.Contains(diagnostic.Message, "invalid HTTP endpoint")
	}
	if !foundDiagnostic {
		t.Fatalf("malformed HTTP route was not diagnosed: %#v", result.Diagnostics)
	}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint && node.Properties["raw_route"] == "/bad/%zz" && node.Properties["http_invalid"] != "true" {
			t.Fatalf("malformed endpoint was not kept fail-closed: %#v", node)
		}
	}
}

func TestSyntaxHTTPVerbCallsRemainOrdinaryWithoutServerEvidence(t *testing.T) {
	content := []byte(`package client
type Client struct{}
func Use(c *Client, body, response any) {
	c.Connect("/connect", body, response)
	c.Delete("/delete", body, response)
	c.Get("/get", body, response)
	c.Head("/head", body, response)
	c.Options("/options", body, response)
	c.Patch("/patch", body, response)
	c.Post("/post", body, response)
	c.Put("/put", body, response)
	c.Trace("/trace", body, response)
}
`)
	result, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "client.go", Content: content, Repository: "client", RepoID: "repo", GoModule: "example.com/client",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint {
			t.Fatalf("verb-named client call invented server endpoint: %#v", node)
		}
	}
	for _, method := range []string{"Connect", "Delete", "Get", "Head", "Options", "Patch", "Post", "Put", "Trace"} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/client.Client."+method)
	}
}

func TestPackageSemanticLoaderClassifiesOnlyProvenOutboundHTTPCalls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/client\n\ngo 1.26\n")
	content := []byte(`package client

import (
	"net/http"
	"net/url"
)

type getter struct{}
func (getter) Get(string) string { return "" }

func Requests() {
	client := &http.Client{}
	request := &http.Request{}
	response := &http.Response{}
	headers := http.Header{}
	values := url.Values{}

	_, _ = http.Get("/package-get")
	_, _ = http.Head("/package-head")
	_, _ = http.Post("/package-post", "text/plain", nil)
	_, _ = http.PostForm("/package-form", nil)
	_, _ = http.DefaultClient.Get("/default-client")
	_, _ = client.Get("/custom-client")

	_ = request.Header.Get("Authorization")
	requestHeaders := request.Header
	_ = requestHeaders.Get("X-Request-Alias")
	_ = response.Header.Get("Content-Type")
	_ = headers.Get("Location")
	_ = values.Get("query")
	var local getter
	_ = local.Get("user-defined")
}
`)
	writeFile(t, filepath.Join(root, "client.go"), string(content))

	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client.go", Content: content, Repository: "client",
		RepoID: "repo", GoModule: "example.com/client",
	})
	if err != nil {
		t.Fatal(err)
	}

	wantRequests := map[string]bool{
		"GET /package-get":    false,
		"HEAD /package-head":  false,
		"POST /package-post":  false,
		"POST /package-form":  false,
		"GET /default-client": false,
		"GET /custom-client":  false,
	}
	requestCount := 0
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeRequests {
			continue
		}
		requestCount++
		if _, ok := wantRequests[fact.Target]; !ok {
			t.Fatalf("accessor or unrelated method emitted outbound request %q: %#v", fact.Target, fact)
		}
		wantRequests[fact.Target] = true
	}
	if requestCount != len(wantRequests) {
		t.Fatalf("outbound request count = %d, want %d: %#v", requestCount, len(wantRequests), result.Facts)
	}
	for target, found := range wantRequests {
		if !found {
			t.Errorf("missing proven outbound request %q", target)
		}
	}
	for _, target := range []string{"net/http.Header.Get", "net/url.Values.Get", "example.com/client.getter.Get"} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, target)
	}

	testContent := []byte(`package client
import (
	"net/http"
	"testing"
)
func TestHeaderRead(t *testing.T) {
	response := &http.Response{}
	_ = response.Header.Get("Content-Type")
	_ = t
}
`)
	writeFile(t, filepath.Join(root, "client_test.go"), string(testContent))
	testResult, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client_test.go", Content: testContent, Repository: "client",
		RepoID: "repo", GoModule: "example.com/client",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range testResult.Facts {
		if fact.Kind == graph.EdgeRequests {
			t.Fatalf("header read in Go test emitted outbound request: %#v", fact)
		}
	}
	assertHasFact(t, testResult.Facts, graph.EdgeCalls, "net/http.Header.Get")
}

func TestSyntaxHTTPFallbackExcludesReceiverMethodsWithoutTypeEvidence(t *testing.T) {
	content := []byte(`package client
import (
	"net/http"
	"net/url"
)
type getter struct{}
func (getter) Get(string) string { return "" }
func Calls(request *http.Request, response *http.Response, headers http.Header, values url.Values, local getter) {
	_, _ = http.Get("/package-get")
	_, _ = http.Head("/package-head")
	_, _ = http.Post("/package-post", "text/plain", nil)
	_, _ = http.PostForm("/package-form", nil)
	_ = request.Header.Get("Authorization")
	requestHeaders := request.Header
	_ = requestHeaders.Get("X-Request-Alias")
	_ = response.Header.Get("Content-Type")
	_ = headers.Get("Location")
	_ = values.Get("query")
	_ = local.Get("user-defined")
}
`)
	result, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "client.go", Content: content, Repository: "client", RepoID: "repo", GoModule: "example.com/client",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRequests := map[string]bool{
		"GET /package-get":   false,
		"HEAD /package-head": false,
		"POST /package-post": false,
		"POST /package-form": false,
	}
	requestCount := 0
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			requestCount++
			if _, ok := wantRequests[fact.Target]; !ok {
				t.Fatalf("receiver method emitted outbound request without type evidence: %#v", fact)
			}
			wantRequests[fact.Target] = true
		}
	}
	if requestCount != len(wantRequests) {
		t.Fatalf("outbound requests = %d, want %d exact package convenience calls: %#v", requestCount, len(wantRequests), result.Facts)
	}
	for target, found := range wantRequests {
		if !found {
			t.Errorf("missing exact package convenience request %q", target)
		}
	}
	for _, target := range []string{"net/http.Header.Get", "net/url.Values.Get", "example.com/client.getter.Get"} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, target)
	}
}

func TestPackageSemanticLoaderExtractsOutboundHTTPThroughWrappers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/client\n\ngo 1.26\n")
	content := []byte(`package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
)

const orders = "/orders/"

func build(ctx context.Context, method, target string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, target, nil)
}

func send(client *http.Client, request *http.Request) (*http.Response, error) {
	return client.Do(request)
}

func invoke(ctx context.Context, client *http.Client, method, target string) (*http.Response, error) {
	request, err := build(ctx, method, target)
	if err != nil { return nil, err }
	return send(client, request)
}

func Call(ctx context.Context, id string) {
	_, _ = invoke(ctx, http.DefaultClient, http.MethodPost,
		fmt.Sprintf("/charge/%s?view=full", url.PathEscape(id)))
}

func Direct(id string) {
	request, _ := http.NewRequest(http.MethodGet, orders+url.PathEscape(id)+"?expand=true", nil)
	_, _ = http.DefaultClient.Do(request)
}

func Convenience() { _, _ = http.Get("/ready") }

func ClientConvenience(client *http.Client) {
	_, _ = client.Get("/client-get")
	_, _ = client.Head("/client-head")
	_, _ = client.Post("/client-post", "text/plain", nil)
	_, _ = client.PostForm("/client-form", url.Values{"ok": {"true"}})
	_, _ = http.DefaultClient.Get("/default-get")
}

func choose(flag bool) string {
	if flag { return "/conditional-left" }
	return "/conditional-right"
}

func chooseAll(flag bool) string {
	if flag { return "/all-left" } else { return "/all-right" }
}

func ConditionalReturn(flag bool) {
	_, _ = http.Get(choose(flag))
	_, _ = http.Get(chooseAll(flag))
}

func loopRoute(flag bool) string {
	for flag { return "/loop-body" }
	return "/loop-after"
}

func rangeRoute(values []string) string {
	for range values { return "/range-body" }
	return "/range-after"
}

func switchRoute(value int) string {
	switch value {
	case 1: return "/switch-one"
	default: return "/switch-default"
	}
}

func typeSwitchRoute(value any) string {
	switch value.(type) {
	case string: return "/type-string"
	default: return "/type-default"
	}
}

func selectRoute(ch <-chan struct{}) string {
	select {
	case <-ch: return "/select-case"
	default: return "/select-default"
	}
}

func ControlReturns(flag bool, values []string, value any, ch <-chan struct{}) {
	_, _ = http.Get(loopRoute(flag))
	_, _ = http.Get(rangeRoute(values))
	_, _ = http.Get(switchRoute(1))
	_, _ = http.Get(typeSwitchRoute(value))
	_, _ = http.Get(selectRoute(ch))
}

func ClauseEffects(value any, ch <-chan struct{}) {
	switch value {
	case "left": _, _ = http.Get("/switch-effect-left")
	default: _, _ = http.Get("/switch-effect-default")
	}
	switch value.(type) {
	case string: _, _ = http.Get("/type-effect-string")
	default: _, _ = http.Get("/type-effect-default")
	}
	select {
	case <-ch: _, _ = http.Get("/select-effect-case")
	default: _, _ = http.Get("/select-effect-default")
	}
}

func BranchFields(flag bool) {
	request := &http.Request{URL: &url.URL{}}
	if flag {
		request.Method = http.MethodGet
		request.URL.Path = "/branch-left"
	} else {
		request.Method = http.MethodPost
		request.URL.Path = "/branch-right"
	}
	_, _ = http.DefaultClient.Do(request)
}

func mutateRequest(request *http.Request) { request.Method = http.MethodPost }

func HelperMutation() {
	request, _ := http.NewRequest(http.MethodGet, "/helper-mutated", nil)
	mutateRequest(request)
	_, _ = http.DefaultClient.Do(request)
}

func rebindRequest(request *http.Request) {
	request, _ = http.NewRequest(http.MethodPost, "/swapped", nil)
}

func HelperRebind() {
	request, _ := http.NewRequest(http.MethodGet, "/original", nil)
	rebindRequest(request)
	_, _ = http.DefaultClient.Do(request)
}

func mutateThenRebind(request *http.Request) {
	request.Method = http.MethodPost
	request, _ = http.NewRequest(http.MethodGet, "/mutate-swapped", nil)
}

func MutateThenRebind() {
	request, _ := http.NewRequest(http.MethodGet, "/mutate-original", nil)
	mutateThenRebind(request)
	_, _ = http.DefaultClient.Do(request)
}

func rebindThenMutateAlias(request *http.Request) {
	alias := request
	request, _ = http.NewRequest(http.MethodGet, "/late-swapped", nil)
	alias.Method = http.MethodPost
}

func RebindThenMutateAlias() {
	request, _ := http.NewRequest(http.MethodGet, "/late-original", nil)
	rebindThenMutateAlias(request)
	_, _ = http.DefaultClient.Do(request)
}

func conditionalRebindAlias(request *http.Request, flag bool) {
	alias := request
	request, _ = http.NewRequest(http.MethodGet, "/conditional-swapped", nil)
	if flag { alias.Method = http.MethodPut }
}

func ConditionalRebindAlias(flag bool) {
	request, _ := http.NewRequest(http.MethodGet, "/conditional-original", nil)
	conditionalRebindAlias(request, flag)
	_, _ = http.DefaultClient.Do(request)
}

func rebindLiteral(request *http.Request) {
	request = &http.Request{Method: http.MethodTrace, URL: &url.URL{Path: "/literal-swapped"}}
}

func CompositeRebind() {
	request := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/literal-original"}}
	rebindLiteral(request)
	_, _ = http.DefaultClient.Do(request)
}

func URLThenPath() {
	request, _ := http.NewRequest(http.MethodGet, "/stale-url", nil)
	request.URL = &url.URL{}
	request.URL.Path = "/actual-url"
	_, _ = http.DefaultClient.Do(request)
}

func DirectURLPath() {
	request, _ := http.NewRequest(http.MethodGet, "/direct-stale", nil)
	request.URL.Path = "/direct-fresh"
	_, _ = http.DefaultClient.Do(request)
}

func URLAlias() {
	request, _ := http.NewRequest(http.MethodGet, "/alias-old", nil)
	alias := request.URL
	alias.Path = "/alias-new"
	alias.RawQuery = "mode=alias"
	_, _ = http.DefaultClient.Do(request)
}

func AlternativeURLAlias(flag bool) {
	target := "/alias-alt-left"
	if flag { target = "/alias-alt-right" }
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	alias := request
	alias.URL.Path = "/alias-alt-final"
	_, _ = http.DefaultClient.Do(request)
}

func URLAliasSpace() {
	request, _ := http.NewRequest(http.MethodGet, "/space direct", nil)
	alias := request.URL
	alias.RawQuery = "q=1"
	_, _ = http.DefaultClient.Do(request)
}

func URLReadForms() {
	uppercase, _ := http.NewRequest(http.MethodGet, "HTTP://api.example.test/upper", nil)
	_ = uppercase.URL.Path
	_, _ = http.DefaultClient.Do(uppercase)
	authority, _ := http.NewRequest(http.MethodGet, "//cdn.example.test/asset", nil)
	_ = authority.URL.Path
	_, _ = http.DefaultClient.Do(authority)
	fragment, _ := http.NewRequest(http.MethodGet, "/section#frag", nil)
	_ = fragment.URL.Path
	_, _ = http.DefaultClient.Do(fragment)
}

func RequestAlias() {
	request, _ := http.NewRequest(http.MethodGet, "/request-alias", nil)
	alias := request
	alias.Method = http.MethodPost
	_, _ = http.DefaultClient.Do(request)
}

func UnrelatedConditional(flag bool) {
	request, _ := http.NewRequest(http.MethodGet, "/always", nil)
	ignored := "left"
	if flag { ignored = "right" }
	_ = ignored
	_, _ = http.DefaultClient.Do(request)
}

func Alternatives(flag bool) {
	method := http.MethodGet
	if flag { method = http.MethodDelete }
	request, _ := http.NewRequest(method, "/alternative", nil)
	_, _ = http.DefaultClient.Do(request)
}

func TooManyAlternatives(flags [9]bool) {
	method := http.MethodGet
	if flags[0] { method = http.MethodPost }
	if flags[1] { method = http.MethodPut }
	if flags[2] { method = http.MethodPatch }
	if flags[3] { method = http.MethodDelete }
	if flags[4] { method = http.MethodHead }
	if flags[5] { method = http.MethodOptions }
	if flags[6] { method = http.MethodConnect }
	if flags[7] { method = http.MethodTrace }
	request, _ := http.NewRequest(method, "/bounded", nil)
	_, _ = http.DefaultClient.Do(request)
}

func ObjectOverflow(flags [9]bool) {
	api := &API{baseURL: "/base-zero"}
	if flags[0] { api = &API{baseURL: "/base-one"} }
	if flags[1] { api = &API{baseURL: "/base-two"} }
	if flags[2] { api = &API{baseURL: "/base-three"} }
	if flags[3] { api = &API{baseURL: "/base-four"} }
	if flags[4] { api = &API{baseURL: "/base-five"} }
	if flags[5] { api = &API{baseURL: "/base-six"} }
	if flags[6] { api = &API{baseURL: "/base-seven"} }
	if flags[7] { api = &API{baseURL: "/base-eight"} }
	if flags[8] { api = &API{baseURL: "/base-nine"} }
	_, _ = http.Get(api.baseURL + "/object-overflow")
}

func RequestFieldOverflow(flags [9]bool) {
	api := &API{baseURL: http.MethodGet}
	if flags[0] { api = &API{baseURL: http.MethodPost} }
	if flags[1] { api = &API{baseURL: http.MethodPut} }
	if flags[2] { api = &API{baseURL: http.MethodPatch} }
	if flags[3] { api = &API{baseURL: http.MethodDelete} }
	if flags[4] { api = &API{baseURL: http.MethodHead} }
	if flags[5] { api = &API{baseURL: http.MethodOptions} }
	if flags[6] { api = &API{baseURL: http.MethodConnect} }
	if flags[7] { api = &API{baseURL: http.MethodTrace} }
	if flags[8] { api = &API{baseURL: "CUSTOM"} }
	constructed, _ := http.NewRequest(http.MethodGet, "/field-overflow", nil)
	constructed.Method = api.baseURL
	_, _ = http.DefaultClient.Do(constructed)
	literal := &http.Request{Method: http.MethodGet, URL: &url.URL{}}
	literal.URL.Path = api.baseURL
	_, _ = http.DefaultClient.Do(literal)
}

type requestBox struct { request *http.Request }

func mutateBox(box *requestBox) { box.request.Method = http.MethodPost }

func HolderMutation() {
	request, _ := http.NewRequest(http.MethodGet, "/holder-mutated", nil)
	box := &requestBox{request: request}
	mutateBox(box)
	_, _ = http.DefaultClient.Do(box.request)
}

func URLAssignmentOverflow(flags [9]bool) {
	target := &url.URL{Path: "/url-zero"}
	if flags[0] { target = &url.URL{Path: "/url-one"} }
	if flags[1] { target = &url.URL{Path: "/url-two"} }
	if flags[2] { target = &url.URL{Path: "/url-three"} }
	if flags[3] { target = &url.URL{Path: "/url-four"} }
	if flags[4] { target = &url.URL{Path: "/url-five"} }
	if flags[5] { target = &url.URL{Path: "/url-six"} }
	if flags[6] { target = &url.URL{Path: "/url-seven"} }
	if flags[7] { target = &url.URL{Path: "/url-eight"} }
	if flags[8] { target = &url.URL{Path: "/url-nine"} }
	direct, _ := http.NewRequest(http.MethodGet, "/direct-url-overflow", nil)
	direct.URL = target
	_, _ = http.DefaultClient.Do(direct)
	box := &requestBox{request: &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/holder-url-overflow"}}}
	box.request.URL = target
	_, _ = http.DefaultClient.Do(box.request)
}

type API struct {
	baseURL string
	client *http.Client
}

func (api *API) fetch(id string) {
	request, _ := http.NewRequest(http.MethodGet, api.baseURL+"/users/"+url.PathEscape(id), nil)
	_, _ = api.client.Do(request)
}

func External(id string) {
	api := &API{baseURL: "https://api.example.test", client: http.DefaultClient}
	api.fetch(id)
}

func (api *API) UnknownAuthority(id string) {
	request, _ := http.NewRequest(http.MethodGet, api.baseURL+"/unknown/"+url.PathEscape(id), nil)
	_, _ = api.client.Do(request)
}

func Composite(id string) {
	request := &http.Request{Method: http.MethodPut, URL: &url.URL{
		Path: "/items/"+url.PathEscape(id), RawQuery: "mode=full",
	}}
	_, _ = http.DefaultClient.Do(request)
}

func Mutated(id string) {
	request := &http.Request{}
	request.Method = http.MethodPatch
	request.URL = &url.URL{}
	request.URL.Path = "/mutable/"+url.PathEscape(id)
	request.URL.RawQuery = "mode=edit"
	_, _ = http.DefaultClient.Do(request)
}

func Unsafe(id string) {
	request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/unsafe/%s", id), nil)
	_, _ = http.DefaultClient.Do(request)
	request = httptest.NewRequest(http.MethodGet, "/synthetic", nil)
	_, _ = http.DefaultClient.Do(request)
}

func cycleA(path string) { cycleB(path) }
func cycleB(path string) {
	cycleA(path)
	_, _ = http.Get(fmt.Sprintf("/cycle/%s", path))
}
func Cyclic(path string) { cycleA(path) }
func CyclicAgain(path string) { cycleA(path) }

type unrelated struct{}
func (unrelated) NewRequest(string, string, any) *http.Request { return nil }
func (unrelated) Do(*http.Request) {}
func NotHTTP() {
	var other unrelated
	request := other.NewRequest(http.MethodGet, "/invented", nil)
	other.Do(request)
}
`)
	writeFile(t, filepath.Join(root, "client.go"), string(content))

	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client.go", Content: content, Repository: "client",
		RepoID: "repo", GoModule: "example.com/client",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]graph.Node{}
	for _, node := range result.Nodes {
		nodes[node.ID] = node
	}
	want := map[string]string{
		"POST /charge/{_}":                       "example.com/client.Call",
		"GET /orders/{_}":                        "example.com/client.Direct",
		"GET /ready":                             "example.com/client.Convenience",
		"GET /client-get":                        "example.com/client.ClientConvenience",
		"HEAD /client-head":                      "example.com/client.ClientConvenience",
		"POST /client-post":                      "example.com/client.ClientConvenience",
		"POST /client-form":                      "example.com/client.ClientConvenience",
		"GET /default-get":                       "example.com/client.ClientConvenience",
		"GET /conditional-left":                  "example.com/client.ConditionalReturn",
		"GET /conditional-right":                 "example.com/client.ConditionalReturn",
		"GET /all-left":                          "example.com/client.ConditionalReturn",
		"GET /all-right":                         "example.com/client.ConditionalReturn",
		"GET /loop-body":                         "example.com/client.ControlReturns",
		"GET /loop-after":                        "example.com/client.ControlReturns",
		"GET /range-body":                        "example.com/client.ControlReturns",
		"GET /range-after":                       "example.com/client.ControlReturns",
		"GET /switch-one":                        "example.com/client.ControlReturns",
		"GET /switch-default":                    "example.com/client.ControlReturns",
		"GET /type-string":                       "example.com/client.ControlReturns",
		"GET /type-default":                      "example.com/client.ControlReturns",
		"GET /select-case":                       "example.com/client.ControlReturns",
		"GET /select-default":                    "example.com/client.ControlReturns",
		"GET /switch-effect-left":                "example.com/client.ClauseEffects",
		"GET /switch-effect-default":             "example.com/client.ClauseEffects",
		"GET /type-effect-string":                "example.com/client.ClauseEffects",
		"GET /type-effect-default":               "example.com/client.ClauseEffects",
		"GET /select-effect-case":                "example.com/client.ClauseEffects",
		"GET /select-effect-default":             "example.com/client.ClauseEffects",
		"GET /branch-left":                       "example.com/client.BranchFields",
		"POST /branch-right":                     "example.com/client.BranchFields",
		"POST /helper-mutated":                   "example.com/client.HelperMutation",
		"GET /original":                          "example.com/client.HelperRebind",
		"POST /mutate-original":                  "example.com/client.MutateThenRebind",
		"POST /late-original":                    "example.com/client.RebindThenMutateAlias",
		"GET /conditional-original":              "example.com/client.ConditionalRebindAlias",
		"PUT /conditional-original":              "example.com/client.ConditionalRebindAlias",
		"GET /literal-original":                  "example.com/client.CompositeRebind",
		"GET /actual-url":                        "example.com/client.URLThenPath",
		"GET /direct-fresh":                      "example.com/client.DirectURLPath",
		"GET /alias-new":                         "example.com/client.URLAlias",
		"GET /alias-alt-final":                   "example.com/client.AlternativeURLAlias",
		"GET /space direct":                      "example.com/client.URLAliasSpace",
		"GET http://api.example.test/upper":      "example.com/client.URLReadForms",
		"GET //cdn.example.test/asset":           "example.com/client.URLReadForms",
		"GET /section":                           "example.com/client.URLReadForms",
		"POST /request-alias":                    "example.com/client.RequestAlias",
		"GET /always":                            "example.com/client.UnrelatedConditional",
		"POST /holder-mutated":                   "example.com/client.HolderMutation",
		"GET /alternative":                       "example.com/client.Alternatives",
		"DELETE /alternative":                    "example.com/client.Alternatives",
		"GET https://api.example.test/users/{_}": "example.com/client.External",
		"GET /unknown/{_}":                       "example.com/client.API.UnknownAuthority",
		"PUT /items/{_}":                         "example.com/client.Composite",
		"PATCH /mutable/{_}":                     "example.com/client.Mutated",
	}
	found := map[string]graph.Fact{}
	requestCount := 0
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeRequests {
			continue
		}
		requestCount++
		if source := nodes[fact.FromID]; source.QualifiedName != want[fact.Target] {
			t.Fatalf("request %q belongs to %q, want %q: %#v", fact.Target, source.QualifiedName, want[fact.Target], fact)
		}
		found[fact.Target] = fact
	}
	if len(found) != len(want) || requestCount != len(want) {
		t.Fatalf("outbound requests = %#v, want %#v; diagnostics = %#v", found, want, result.Diagnostics)
	}
	for target := range want {
		fact, ok := found[target]
		if !ok || fact.Properties["resolution"] != "go/types" || fact.Properties["http_sink"] == "" ||
			fact.Properties["http_source"] == "" {
			t.Fatalf("request %q lacks semantic provenance: %#v", target, fact)
		}
	}
	for _, target := range []string{"GET /alternative", "DELETE /alternative", "GET /branch-left", "POST /branch-right"} {
		if found[target].Properties["conditional"] != "true" {
			t.Fatalf("branch alternative %q lacks conditional provenance: %#v", target, found[target])
		}
	}
	for _, target := range []string{"GET /conditional-original", "PUT /conditional-original"} {
		if found[target].Properties["conditional"] != "true" {
			t.Fatalf("conditional helper alias alternative %q lacks provenance: %#v", target, found[target])
		}
	}
	if found["GET /always"].Properties["conditional"] == "true" {
		t.Fatalf("unrelated branch made an always-executed request conditional: %#v", found["GET /always"])
	}
	wrapped := found["POST /charge/{_}"]
	if !strings.Contains(wrapped.Properties["http_wrapper_chain"], "example.com/client.invoke") ||
		wrapped.Properties["http_query"] != "view=full" || wrapped.Location.Line != 28 {
		t.Fatalf("wrapper request lost its highest callsite, query, or chain: %#v", wrapped)
	}
	external := found["GET https://api.example.test/users/{_}"]
	if external.Properties["http_authority"] != "api.example.test" {
		t.Fatalf("external authority was not preserved: %#v", external)
	}
	unknown := found["GET /unknown/{_}"]
	if unknown.Properties["http_authority_unknown"] != "true" {
		t.Fatalf("receiver base URL uncertainty was not explicit: %#v", unknown)
	}
	composite := found["PUT /items/{_}"]
	if composite.Properties["http_query"] != "mode=full" {
		t.Fatalf("request URL query was not separated: %#v", composite)
	}
	mutated := found["PATCH /mutable/{_}"]
	if mutated.Properties["http_query"] != "mode=edit" {
		t.Fatalf("mutated request URL query was not separated: %#v", mutated)
	}
	if alias := found["GET /alias-new"]; alias.Properties["http_query"] != "mode=alias" {
		t.Fatalf("URL alias mutation lost its query: %#v", alias)
	}
	if alias := found["GET /space direct"]; alias.Properties["http_query"] != "q=1" {
		t.Fatalf("URL alias attachment changed its raw path or lost its query: %#v", alias)
	}
	if fragment := found["GET /section"]; fragment.Properties["http_raw_route"] != "/section#frag" {
		t.Fatalf("URL read lost raw fragment evidence: %#v", fragment)
	}
	for _, forbidden := range []string{"POST /swapped", "GET /mutate-original", "GET /mutate-swapped", "GET /late-original", "GET /late-swapped", "GET /conditional-swapped", "TRACE /literal-swapped", "GET /alias-old", "GET /alias-alt-left", "GET /alias-alt-right", "GET /space%20direct", "GET /request-alias", "GET /direct-stale", "GET /unsafe/{_}", "GET /synthetic", "GET /invented", "GET /cycle"} {
		if _, ok := found[forbidden]; ok {
			t.Fatalf("unproven or unrelated request %q was invented: %#v", forbidden, found[forbidden])
		}
	}
	foundCycleDiagnostics := 0
	foundBoundDiagnostics := 0
	foundAuthorityDiagnostic := false
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, "recursive HTTP wrapper") {
			foundCycleDiagnostics++
		}
		if strings.Contains(diagnostic.Message, "HTTP alternatives exceeded") {
			foundBoundDiagnostics++
		}
		foundAuthorityDiagnostic = foundAuthorityDiagnostic || strings.Contains(diagnostic.Message, "//cdn.example.test/asset")
	}
	if foundCycleDiagnostics != 1 {
		t.Fatalf("recursive wrapper was not diagnosed: %#v", result.Diagnostics)
	}
	if foundBoundDiagnostics < 6 {
		t.Fatalf("alternative bound was not diagnosed: %#v", result.Diagnostics)
	}
	if !foundAuthorityDiagnostic {
		t.Fatalf("authority-form URL was not kept fail-closed: %#v", result.Diagnostics)
	}
}

func TestPackageSemanticLoaderBoundsHTTPExecution(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/bounded\n\ngo 1.26\n")
	var body strings.Builder
	body.WriteString("package bounded\nimport \"net/http\"\nfunc Call(flags []bool) {\nmethod := http.MethodGet\n")
	for index := range 700 {
		fmt.Fprintf(&body, "if flags[%d] { method = http.MethodPost }\n", index)
	}
	body.WriteString("request, _ := http.NewRequest(method, \"/bounded\", nil)\n_, _ = http.DefaultClient.Do(request)\n}\n")
	body.WriteString("func AfterBudget(flags []bool) {\nmethod := http.MethodGet\n")
	for index := range 700 {
		fmt.Fprintf(&body, "if flags[%d] { method = http.MethodPost }\n", index)
	}
	body.WriteString("_, _ = http.Get(\"/root-after\")\n_ = method\n}\n")
	content := []byte(body.String())
	writeFile(t, filepath.Join(root, "client.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client.go", Content: content, Repository: "bounded",
		RepoID: "repo", GoModule: "example.com/bounded",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			t.Fatalf("execution-budget exhaustion emitted a partial request: %#v", fact)
		}
	}
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, "HTTP execution exceeded") {
			return
		}
	}
	t.Fatalf("execution-budget exhaustion was not diagnosed: %#v", result.Diagnostics)
}

func TestCanonicalEndpointsAtSameLineRemainDistinct(t *testing.T) {
	content := []byte(`package api
func Handler() {}
func Routes() { router.Get("/users/{id}", Handler); router.Get("/users/{name}", Handler) }
`)
	parser := golangparser.NewWithSemanticLoader(fakeSemanticLoader{view: golangparser.SemanticView{
		Available: true, Included: true,
		ChiEndpoints: []golangparser.SemanticChiEndpoint{
			{Function: "example.com/sample.Routes", FunctionKind: graph.KindFunction, Method: "GET", Route: "/users/{id}", Handler: "example.com/sample.Handler", HandlerKind: graph.KindFunction, Location: graph.Location{Path: "routes.go", Line: 3, Column: 17}},
			{Function: "example.com/sample.Routes", FunctionKind: graph.KindFunction, Method: "GET", Route: "/users/{name}", Handler: "example.com/sample.Handler", HandlerKind: graph.KindFunction, Location: graph.Location{Path: "routes.go", Line: 3, Column: 54}},
		},
	}})
	result, err := parser.Parse(context.Background(), parserapi.Input{
		Path: "routes.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint && node.Name == "GET /users/{_}" {
			ids[node.ID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("canonical duplicate declarations collapsed: %#v", result.Nodes)
	}
}

func TestParserPrefersInjectedSemanticCallAndImplementationEvidence(t *testing.T) {
	content := []byte(`package sample
type Runner interface { Run() }
type Worker struct{}
func (Worker) Run() {}
func invoke(value Runner) { value.Run() }
`)
	callOffset := strings.Index(string(content), "value.Run") + len("value.Run")
	parser := golangparser.NewWithSemanticLoader(fakeSemanticLoader{view: golangparser.SemanticView{
		Available:    true,
		Included:     true,
		BuildContext: "goos=test;goarch=test",
		Calls: map[int]golangparser.SemanticCall{
			callOffset: {Target: "example.com/sample.Runner.Run"},
		},
		Implementations: []golangparser.SemanticImplementation{{
			Concrete: "example.com/sample.Worker", Interface: "example.com/sample.Runner",
		}},
	}})
	result, err := parser.Parse(context.Background(), parserapi.Input{
		Path: "sample.go", Content: content, Repository: "sample", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/sample.Runner.Run")
	assertHasFact(t, result.Facts, graph.EdgeImplements, "example.com/sample.Runner")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "example.com/sample.Worker.Run" {
			t.Fatalf("interface dispatch fanned out to concrete implementation: %#v", fact)
		}
	}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindFile && node.Properties["go_build_context"] != "goos=test;goarch=test" {
			t.Fatalf("build context not recorded on file node: %#v", node)
		}
	}
}

func TestSyntaxFailureFallbackRequiresUnshadowedBuiltins(t *testing.T) {
	safe := []byte(`package sample
func cleanup() {}
func callbackCleanup() {}
func Register(func()) {}
func Guard(value any) {
	if value != nil { defer cleanup(); panic(value) }
	Register(func() { defer callbackCleanup(); panic(value) })
	func() { _ = recover() }()
	_ = recover()
}
`)
	result, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "safe.go", Content: safe, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	panicFact := assertFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgePanics, "example.com/sample.Guard")
	if panicFact.Properties["conditional"] != "true" || panicFact.Properties["evidence"] != "go/ast" {
		t.Fatalf("syntax panic evidence = %#v", panicFact)
	}
	deferFact := assertFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeDefers, "example.com/sample.Guard")
	if deferFact.Target != "example.com/sample.cleanup" || deferFact.Properties["unresolved"] != "true" {
		t.Fatalf("syntax defer must keep its syntactic target and unresolved evidence: %#v", deferFact)
	}
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeRecovers,
		"example.com/sample.Guard", "builtin.recover", "recover")
	if count := failureFactCountFrom(t, result.Nodes, result.Facts, graph.EdgeRecovers, "example.com/sample.Guard"); count != 1 {
		t.Fatalf("syntax recover facts = %d, want only the named function site", count)
	}
	assertNoFailureTarget(t, result.Facts, graph.EdgeDefers, "example.com/sample.callbackCleanup")
	panicCount := 0
	guardID := graph.NodeID(graph.KindFunction, "example.com/sample.Guard", "repo", "safe.go")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgePanics && fact.FromID == guardID {
			panicCount++
		}
	}
	if panicCount != 1 {
		t.Fatalf("syntax fallback panics = %d, want only the directly executed panic", panicCount)
	}

	shadowed := []byte(`package sample
var panic = func(any) {}
func Guard(value any, recover func() any) { panic(value); _ = recover() }
`)
	result, err = golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Path: "shadowed.go", Content: shadowed, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgePanics, "example.com/sample.Guard")
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeRecovers, "example.com/sample.Guard")
}

func TestPackageSemanticLoaderResolvesTypedCalls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	content := []byte(`package sample
import "reflect"
type Runner interface { Run() }
type Base struct{}
func (Base) Promoted() {}
type Worker struct{ Base }
func (*Worker) Run() {}
type Alias = Worker
type Left struct{}
func (Left) Touch() {}
type Right struct{}
func (Right) Touch() {}
func Generic[T any](T) {}
func invoke(r Runner, w *Worker, left Left, right Right) {
	r.Run()
	w.Promoted()
	method := w.Run
	method()
	(*Worker).Run(w)
	var alias *Alias = w
	alias.Run()
	left.Touch()
	right.Touch()
	Generic[int](1)
	println("ok")
	_ = string([]byte("ok"))
	reflect.ValueOf(w).MethodByName("Run").Call(nil)
}
`)
	writeFile(t, filepath.Join(root, "sample.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "sample.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"example.com/sample.Runner.Run",
		"example.com/sample.Base.Promoted",
		"example.com/sample.Worker.Run",
		"example.com/sample.Left.Touch",
		"example.com/sample.Right.Touch",
		"example.com/sample.Generic",
		"builtin.println",
		"reflect.ValueOf",
		"reflect.Value.MethodByName",
		"reflect.Value.Call",
	} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, target)
	}
	assertHasFact(t, result.Facts, graph.EdgeImplements, "example.com/sample.Runner")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "example.com/sample.string" {
			t.Fatalf("type conversion emitted a call edge: %#v", fact)
		}
	}
}

func TestPackageSemanticLoaderResolvesInternalAndExternalTestCalls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "sample.go"), "package sample\nfunc Produce() {}\n")
	tests := map[string]string{
		"sample_test.go": `package sample
import "testing"
func TestInternal(t *testing.T) { Produce() }
`,
		"external_test.go": `package sample_test
import (
  "testing"
  "example.com/sample"
)
func TestExternal(t *testing.T) { sample.Produce() }
`,
	}
	for path, source := range tests {
		writeFile(t, filepath.Join(root, path), source)
		result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
			Root: root, Path: path, Content: []byte(source), Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
		})
		if err != nil {
			t.Fatalf("Parse(%s): %v", path, err)
		}
		var testNode graph.Node
		for _, node := range result.Nodes {
			if node.Kind == graph.KindTest {
				testNode = node
				break
			}
		}
		found := false
		for _, fact := range result.Facts {
			if fact.Kind == graph.EdgeCalls && fact.FromID == testNode.ID &&
				fact.Target == "example.com/sample.Produce" && fact.Properties["resolution"] == "go/types" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s did not retain exact typed production call: nodes=%#v facts=%#v diagnostics=%#v", path, result.Nodes, result.Facts, result.Diagnostics)
		}
	}
}

func TestPackageSemanticLoaderExcludesInactiveBuildTaggedFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/buildtag\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "active.go"), "package buildtag\nfunc Active() {}\n")
	content := []byte("//go:build grafo_never_enabled\n\npackage buildtag\nfunc Excluded() {}\n")
	writeFile(t, filepath.Join(root, "excluded.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "excluded.go", Content: content, Repository: "buildtag", RepoID: "repo", GoModule: "example.com/buildtag",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile || result.Nodes[0].Properties["go_build_excluded"] != "true" {
		t.Fatalf("excluded file emitted declarations or omitted context: %#v", result.Nodes)
	}
}

func TestPackageSemanticLoaderDoesNotResolveOrDownloadMissingModule(t *testing.T) {
	root := t.TempDir()
	modulePath := filepath.Join(root, "go.mod")
	moduleContent := "module example.com/missing-test\n\ngo 1.26\n"
	writeFile(t, modulePath, moduleContent)
	content := []byte("package missingtest\nimport missing \"example.invalid/not-present\"\nfunc Run() { missing.Call() }\n")
	writeFile(t, filepath.Join(root, "main.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "main.go", Content: content, Repository: "missing-test", RepoID: "repo", GoModule: "example.com/missing-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("expected unavailable-dependency diagnostic")
	}
	after, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != moduleContent {
		t.Fatalf("semantic load modified go.mod:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("semantic load created go.sum: %v", err)
	}
}

func TestPackageSemanticLoaderUsesWorkspaceModuleImportPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse ./service\n")
	writeFile(t, filepath.Join(root, "service", "go.mod"), "module example.com/workspace/service\n\ngo 1.26\n")
	content := []byte("package service\nfunc helper() {}\nfunc Run() { helper() }\n")
	writeFile(t, filepath.Join(root, "service", "service.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "service/service.go", Content: content, Repository: "workspace", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNodeQualified(t, result.Nodes, graph.KindFunction, "example.com/workspace/service.Run")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/workspace/service.helper")
}

func TestPackageSemanticLoaderLoadsSiblingModulesWithoutWorkspace(t *testing.T) {
	root := t.TempDir()
	modules := []struct {
		directory string
		module    string
		function  string
	}{
		{directory: "apps/api", module: "example.com/apps/api", function: "Serve"},
		{directory: "tools/client", module: "example.com/tools/client", function: "Run"},
	}
	parser := golangparser.New()
	for _, module := range modules {
		writeFile(t, filepath.Join(root, module.directory, "go.mod"), "module "+module.module+"\n\ngo 1.26\n")
		content := []byte("package main\nfunc helper() {}\nfunc " + module.function + "() { helper() }\n")
		path := filepath.ToSlash(filepath.Join(module.directory, "main.go"))
		writeFile(t, filepath.Join(root, filepath.FromSlash(path)), string(content))
	}
	for _, module := range modules {
		path := filepath.ToSlash(filepath.Join(module.directory, "main.go"))
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		result, err := parser.Parse(context.Background(), parserapi.Input{
			Root: root, Path: path, Content: content, Repository: "multi", RepoID: "repo",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertHasNodeQualified(t, result.Nodes, graph.KindFunction, module.module+"."+module.function)
		fact := assertFact(t, result.Facts, graph.EdgeCalls, module.module+".helper")
		if fact.Properties["resolution"] != "go/types" {
			t.Fatalf("%s call was not type resolved: %#v", module.directory, fact)
		}
		var file graph.Node
		for _, node := range result.Nodes {
			if node.Kind == graph.KindFile {
				file = node
				break
			}
		}
		if got := file.Properties["go_module"]; got != module.module {
			t.Fatalf("%s file module = %q, want %q; file=%#v", module.directory, got, module.module, file)
		}
	}
}

func TestPackageSemanticLoaderIsolatesBrokenSiblingModule(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "healthy", "go.mod"), "module example.com/healthy\n\ngo 1.26\n")
	healthy := []byte("package healthy\nfunc helper() {}\nfunc Run() { helper() }\n")
	writeFile(t, filepath.Join(root, "healthy", "healthy.go"), string(healthy))
	writeFile(t, filepath.Join(root, "broken", "go.mod"), "module example.com/broken\n\ngo 1.26\nrequire example.invalid/missing v1.0.0\n")
	broken := []byte("package broken\nimport missing \"example.invalid/missing\"\nfunc Run() { missing.Call() }\n")
	writeFile(t, filepath.Join(root, "broken", "broken.go"), string(broken))

	parser := golangparser.New()
	result, err := parser.Parse(context.Background(), parserapi.Input{
		Root: root, Path: "healthy/healthy.go", Content: healthy, Repository: "multi", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	fact := assertFact(t, result.Facts, graph.EdgeCalls, "example.com/healthy.helper")
	if fact.Properties["resolution"] != "go/types" {
		t.Fatalf("broken sibling degraded healthy module: %#v", fact)
	}

	result, err = parser.Parse(context.Background(), parserapi.Input{
		Root: root, Path: "broken/broken.go", Content: broken, Repository: "multi", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, "broken") || strings.Contains(diagnostic.Message, "example.invalid/missing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("broken module failure lacked module provenance: %#v", result.Diagnostics)
	}
}

func TestPackageSemanticLoaderTreatsWorkspaceAsAuthoritativeSubset(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse ./listed\n")
	writeFile(t, filepath.Join(root, "listed", "go.mod"), "module example.com/listed\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "listed", "listed.go"), "package listed\n")
	writeFile(t, filepath.Join(root, "unlisted", "go.mod"), "module example.com/unlisted\n\ngo 1.26\n")
	content := []byte("package unlisted\nfunc helper() {}\nfunc Run() { helper() }\n")
	writeFile(t, filepath.Join(root, "unlisted", "unlisted.go"), string(content))

	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "unlisted/unlisted.go", Content: content, Repository: "workspace", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "example.com/unlisted.helper" && fact.Properties["resolution"] == "go/types" {
			t.Fatalf("unlisted module was loaded outside authoritative workspace: %#v", fact)
		}
	}
	found := false
	for _, diagnostic := range result.Diagnostics {
		found = found || strings.Contains(diagnostic.Message, "omitted source")
	}
	if !found {
		t.Fatalf("workspace-unlisted module omission was not diagnosed: %#v", result.Diagnostics)
	}
}

func TestPackageSemanticLoaderExcludesIgnoredVendoredAndSymlinkedModules(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string) (string, []byte)
	}{
		{
			name: "git ignored",
			prepare: func(t *testing.T, root string) (string, []byte) {
				command := exec.Command("git", "init", "-q", root)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v: %s", err, output)
				}
				writeFile(t, filepath.Join(root, ".gitignore"), "ignored/\n")
				content := []byte("package ignored\nfunc helper() {}\nfunc Run() { helper() }\n")
				writeFile(t, filepath.Join(root, "ignored", "go.mod"), "module example.com/ignored\n\ngo 1.26\n")
				writeFile(t, filepath.Join(root, "ignored", "ignored.go"), string(content))
				return "ignored/ignored.go", content
			},
		},
		{
			name: "vendored",
			prepare: func(t *testing.T, root string) (string, []byte) {
				content := []byte("package vendored\nfunc helper() {}\nfunc Run() { helper() }\n")
				writeFile(t, filepath.Join(root, "vendor", "nested", "go.mod"), "module example.com/vendored\n\ngo 1.26\n")
				writeFile(t, filepath.Join(root, "vendor", "nested", "vendored.go"), string(content))
				return "vendor/nested/vendored.go", content
			},
		},
		{
			name: "symlink escaped",
			prepare: func(t *testing.T, root string) (string, []byte) {
				external := t.TempDir()
				content := []byte("package escaped\nfunc helper() {}\nfunc Run() { helper() }\n")
				writeFile(t, filepath.Join(external, "go.mod"), "module example.com/escaped\n\ngo 1.26\n")
				writeFile(t, filepath.Join(external, "escaped.go"), string(content))
				if err := os.Symlink(external, filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
				return "linked/escaped.go", content
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path, content := test.prepare(t, root)
			result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
				Root: root, Path: path, Content: content, Repository: "excluded", RepoID: "repo",
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range result.Facts {
				if fact.Kind == graph.EdgeCalls && fact.Properties["resolution"] == "go/types" {
					t.Fatalf("excluded module produced typed evidence: %#v", fact)
				}
			}
		})
	}
}

func TestSemanticKeyTracksNestedModulePlanChanges(t *testing.T) {
	root := t.TempDir()
	parser := golangparser.New()
	first, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "nested", "go.mod"), "module example.com/first\n\ngo 1.26\n")
	second, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "nested", "go.mod"), "module example.com/second\n\ngo 1.26\n")
	third, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second == third || first == third {
		t.Fatalf("nested module addition/identity did not invalidate semantic key: %q %q %q", first, second, third)
	}
	if err := os.Remove(filepath.Join(root, "nested", "go.mod")); err != nil {
		t.Fatal(err)
	}
	fourth, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if fourth != first {
		t.Fatalf("nested module removal did not restore semantic plan key: first=%q fourth=%q", first, fourth)
	}
}

func TestSemanticKeyTracksWorkspaceVendorManifestChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse ./service\n")
	writeFile(t, filepath.Join(root, "service", "go.mod"), "module example.com/service\n\ngo 1.26\n")
	manifest := filepath.Join(root, "vendor", "modules.txt")
	writeFile(t, manifest, "# example.com/dependency v1.0.0\n")

	parser := golangparser.New()
	first, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, manifest, "# example.com/dependency v1.1.0\n")
	second, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("workspace vendor manifest change did not invalidate semantic key: %q", first)
	}
}

func TestSemanticModuleDiscoveryHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/canceled\n\ngo 1.26\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := golangparser.New().SemanticKey(ctx, parserapi.Input{Root: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled module discovery error = %v, want context.Canceled", err)
	}
}

func TestPackageSemanticLoaderFallsBackForUnloadedGoFiles(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		prepareRoot func(*testing.T, string)
		content     string
		function    string
	}{
		{
			name: "test file", path: "sample_test.go", function: "TestHelper",
			prepareRoot: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
				writeFile(t, filepath.Join(root, "sample.go"), "package sample\nfunc Helper() {}\n")
			},
			content: "package sample\nfunc TestHelper() { Helper() }\n",
		},
		{
			name: "unlisted nested module", path: "nested/nested.go", function: "Nested",
			prepareRoot: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n\ngo 1.26\n")
				writeFile(t, filepath.Join(root, "outer.go"), "package outer\n")
				writeFile(t, filepath.Join(root, "nested", "go.mod"), "module example.com/nested\n\ngo 1.26\n")
			},
			content: "package nested\nfunc Nested() {}\n",
		},
		{
			name: "cgo source", path: "cgo.go", function: "CGOHelper",
			prepareRoot: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.com/cgo\n\ngo 1.26\n")
			},
			content: "package cgo\n/* static void grafo_noop(void) {} */\nimport \"C\"\nfunc CGOHelper() { C.grafo_noop() }\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.prepareRoot(t, root)
			writeFile(t, filepath.Join(root, filepath.FromSlash(test.path)), test.content)
			result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
				Root: root, Path: test.path, Content: []byte(test.content), Repository: "sample", RepoID: "repo", GoModule: "example.com/outer",
			})
			if err != nil {
				t.Fatal(err)
			}
			assertHasNode(t, result.Nodes, graph.KindFunction, test.function)
			if result.Nodes[0].Properties["go_build_excluded"] == "true" {
				t.Fatalf("unloaded source was misclassified as build-excluded: %#v", result.Nodes[0])
			}
		})
	}
}

func TestSemanticKeySkipsSpecialGoEntries(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/special\n\ngo 1.26\n")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), filepath.Join(root, "dangling.go")); err != nil {
		t.Fatal(err)
	}
	parser := golangparser.New()
	if _, err := parser.SemanticKey(context.Background(), parserapi.Input{Root: root}); err != nil {
		t.Fatalf("special Go entry aborted workspace keying: %v", err)
	}
}

func TestParserSuppressesOnlyCorroboratedProtobufGeneratedGo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "buf.gen.yaml"), `version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: example.com/generated
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen
    opt: paths=source_relative
`)
	writeFile(t, filepath.Join(root, "schema.proto"), "syntax = \"proto3\"; message Message { string value = 1; }\n")
	generated := []byte(`// Code generated by protoc-gen-go. DO NOT EDIT.
// source: schema.proto
package generated
type Message struct{}
func (*Message) Reset() {}
func (*Message) ProtoReflect() any { return nil }
func (*Message) GetValue() string { return "" }
`)
	path := "gen/schema.pb.go"
	writeFile(t, filepath.Join(root, filepath.FromSlash(path)), string(generated))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: path, Content: generated, RepoID: "repo:test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile || result.Nodes[0].Properties["generator"] != "protoc-gen-go" {
		t.Fatalf("generated runtime was not suppressed: %#v", result.Nodes)
	}
	mismatched := []byte(strings.Replace(string(generated), "source: schema.proto", "source: other.proto", 1))
	result, err = golangparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: path, Content: mismatched, RepoID: "repo:test"})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindMethod, "Reset")
	if len(result.Diagnostics) == 0 || !strings.Contains(result.Diagnostics[len(result.Diagnostics)-1].Message, "provenance rejected") {
		t.Fatalf("mismatched generated source was not diagnosed: %#v", result.Diagnostics)
	}
	ordinary := []byte("// Code generated by stringer. DO NOT EDIT.\npackage generated\nfunc Keep() {}\n")
	result, err = golangparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: "ordinary_generated.go", Content: ordinary, RepoID: "repo:test"})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindFunction, "Keep")
}

func TestPackageSemanticLoaderExtractsCanonicalProtobufUsage(t *testing.T) {
	root := protobufUsageFixture(t)
	content, err := os.ReadFile(filepath.Join(root, "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "app.go", Content: content, Repository: "protobuf-usage",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}

	useID := nodeIDByQualified(t, result.Nodes, "example.com/app.Use")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeEncodes, "acme.v1.Envelope", "marshal")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeDecodes, "acme.v1.Envelope", "unmarshal")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.title", "composite_literal")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.text", "oneof_wrapper")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.title", "getter")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.title", "field_selection")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.title", "field_selection")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.text", "type_switch")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "google.golang.org/protobuf/proto.Marshal")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "google.golang.org/protobuf/proto.Unmarshal")

	falsePositiveID := nodeIDByQualified(t, result.Nodes, "example.com/app.FalsePositive")
	for _, fact := range result.Facts {
		if fact.FromID == falsePositiveID && (fact.Kind == graph.EdgeEncodes || fact.Kind == graph.EdgeDecodes ||
			fact.Kind == graph.EdgeReads || fact.Kind == graph.EdgeWrites) && fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("ordinary same-name API produced protocol usage: %#v", fact)
		}
	}
}

func TestPackageSemanticLoaderExtractsENetTransportEvidence(t *testing.T) {
	root := protobufUsageFixture(t)
	writeFile(t, filepath.Join(root, "go.mod"), `module example.com/app

go 1.26

require (
	github.com/cafecito-games/goenet v0.0.0
	google.golang.org/protobuf v0.0.0
)
replace github.com/cafecito-games/goenet => ./third_party/goenet
replace google.golang.org/protobuf => ./third_party/protobuf
`)
	writeFile(t, filepath.Join(root, "third_party", "goenet", "go.mod"), "module github.com/cafecito-games/goenet\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "third_party", "goenet", "pkg", "goenet.go"), `package goenet
type PacketFlag uint32
const (
	PacketFlagReliable PacketFlag = 1
	PacketFlagUnsequenced PacketFlag = 2
)
type Packet struct {
	Data []byte
	Flags PacketFlag
}
type PeerSender interface {
	Send(channelID uint8, packet *Packet) error
}
type Peer struct{}
func (p *Peer) Send(channelID uint8, packet *Packet) error { return nil }
type Host struct{}
func (h *Host) Broadcast(channelID uint8, packet *Packet) error { return nil }
type EventType uint8
type Event struct {
	Type EventType
	Peer *Peer
	ChannelID uint8
	Data uint32
	Packet *Packet
}
`)
	content := []byte(`package app
import (
	goenet "github.com/cafecito-games/goenet/pkg"
	generated "example.com/app/gen"
	wire "google.golang.org/protobuf/proto"
)
const gameplayChannel = 3
func send(peer *goenet.Peer, payload []byte) {
	_ = peer.Send(gameplayChannel, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}
func relay(peer *goenet.Peer, payload []byte) { send(peer, payload) }
func SendViaBoundPacket(peer *goenet.Peer, input *generated.Envelope) {
	payload, _ := wire.Marshal(input)
	packet := &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable}
	_ = peer.Send(gameplayChannel, packet)
}
func sendPacket(peer *goenet.Peer, packet *goenet.Packet) {
	_ = peer.Send(gameplayChannel, packet)
}
func SendViaPacketParameter(peer *goenet.Peer, input *generated.Envelope) {
	payload, _ := wire.Marshal(input)
	sendPacket(peer, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}
func conflicting(peer *goenet.Peer, left, right []byte) {
	_ = peer.Send(1, &goenet.Packet{Data: left, Flags: goenet.PacketFlagReliable})
	_ = peer.Send(2, &goenet.Packet{Data: right, Flags: goenet.PacketFlagUnsequenced})
}
func multiConflict(peer *goenet.Peer, host *goenet.Host, left, right []byte) {
	_ = peer.Send(1, &goenet.Packet{Data: left, Flags: goenet.PacketFlagReliable})
	_ = peer.Send(2, &goenet.Packet{Data: right, Flags: goenet.PacketFlagUnsequenced})
	_ = host.Broadcast(3, &goenet.Packet{Data: left, Flags: goenet.PacketFlagReliable})
	_ = host.Broadcast(4, &goenet.Packet{Data: right, Flags: goenet.PacketFlagUnsequenced})
}
func UseMultiConflict(peer *goenet.Peer, host *goenet.Host, left, right []byte) {
	multiConflict(peer, host, left, right)
}
func cycleA(peer *goenet.Peer, payload []byte) { cycleB(peer, payload) }
func cycleB(peer *goenet.Peer, payload []byte) { cycleA(peer, payload) }
func deep1(peer *goenet.Peer, payload []byte) { send(peer, payload) }
func deep2(peer *goenet.Peer, payload []byte) { deep1(peer, payload) }
func deep3(peer *goenet.Peer, payload []byte) { deep2(peer, payload) }
func deep4(peer *goenet.Peer, payload []byte) { deep3(peer, payload) }
func deep5(peer *goenet.Peer, payload []byte) { deep4(peer, payload) }
func deep6(peer *goenet.Peer, payload []byte) { deep5(peer, payload) }
func deep7(peer *goenet.Peer, payload []byte) { deep6(peer, payload) }
func deep8(peer *goenet.Peer, payload []byte) { deep7(peer, payload) }
func deep9(peer *goenet.Peer, payload []byte) { deep8(peer, payload) }
func UseTransport(peer *goenet.Peer, event goenet.Event, input *generated.Envelope, channel uint8, dynamic *goenet.Packet, unknown []byte) {
	payload, _ := wire.Marshal(input)
	relay(peer, payload)
	deep9(peer, payload)
	conflicting(peer, payload, unknown)
	_ = peer.Send(channel, dynamic)
	received := event.Packet.Data
	_ = event.ChannelID
	_ = wire.Unmarshal(received, input)
}
// WrappedPeer narrows peer behaviour behind a local interface the way uzir's
// transport.Peer does, but embeds goenet's PeerSender so the call keeps the
// upstream method identity.
type WrappedPeer interface {
	Close() error
	goenet.PeerSender
}
func sendViaSender(peer WrappedPeer, payload []byte) {
	_ = peer.Send(gameplayChannel, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}
func UseSenderInterface(peer WrappedPeer, input *generated.Envelope) {
	payload, _ := wire.Marshal(input)
	sendViaSender(peer, payload)
}
// RedeclaredPeer restates Send with goenet's exact signature instead of
// embedding PeerSender, so its identity is local. It must not produce transport
// evidence: it is indistinguishable from the Lookalike decoy below, which is why
// grafo cannot resolve consumer interfaces that redeclare rather than embed.
type RedeclaredPeer interface {
	Send(channelID uint8, packet *goenet.Packet) error
}
func RedeclaredCall(peer RedeclaredPeer, payload []byte) {
	_ = peer.Send(3, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}
type Lookalike struct{}
func (Lookalike) Send(channelID uint8, packet *goenet.Packet) error { return nil }
func OrdinaryCall(value Lookalike, payload []byte) {
	_ = value.Send(3, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}
func UncertainTransport(peer *goenet.Peer, input *generated.Envelope, unknown []byte, condition bool) {
	payload, _ := wire.Marshal(input)
	if condition { payload = unknown }
	_ = peer.Send(3, &goenet.Packet{Data: payload, Flags: goenet.PacketFlagReliable})
}
`)
	writeFile(t, filepath.Join(root, "transport.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "transport.go", Content: content, Repository: "protobuf-transport",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	useID := nodeIDByQualified(t, result.Nodes, "example.com/app.UseTransport")
	send := assertTransportOperation(t, result, useID, graph.EdgeSends, "send", "github.com/cafecito-games/goenet/pkg.Peer.Send")
	if send.Properties["channel"] != "3" || send.Properties["channel_status"] != "proven" ||
		send.Properties["reliability"] != "reliable" || send.Properties["payload_status"] != "proven" ||
		send.Properties["wrapper_depth"] != "2" {
		t.Fatalf("wrapped send evidence = %#v", send.Properties)
	}
	assertTransportCarries(t, result.Facts, send.ID, "acme.v1.Envelope")
	receive := assertTransportOperation(t, result, useID, graph.EdgeReceives, "receive", "github.com/cafecito-games/goenet/pkg.Event.Packet.Data")
	if receive.Properties["channel_status"] != "symbolic" || receive.Properties["payload_status"] != "proven" {
		t.Fatalf("receive evidence = %#v", receive.Properties)
	}
	assertTransportCarries(t, result.Facts, receive.ID, "acme.v1.Envelope")

	boundID := nodeIDByQualified(t, result.Nodes, "example.com/app.SendViaBoundPacket")
	bound := assertTransportOperation(t, result, boundID, graph.EdgeSends, "send", "github.com/cafecito-games/goenet/pkg.Peer.Send")
	if bound.Properties["channel"] != "3" || bound.Properties["channel_status"] != "proven" ||
		bound.Properties["reliability"] != "reliable" || bound.Properties["payload_status"] != "proven" {
		t.Fatalf("locally bound packet evidence = %#v", bound.Properties)
	}
	assertTransportCarries(t, result.Facts, bound.ID, "acme.v1.Envelope")

	senderID := nodeIDByQualified(t, result.Nodes, "example.com/app.UseSenderInterface")
	viaSender := assertTransportOperation(t, result, senderID, graph.EdgeSends, "send",
		"github.com/cafecito-games/goenet/pkg.PeerSender.Send")
	if viaSender.Properties["channel"] != "3" || viaSender.Properties["channel_status"] != "proven" ||
		viaSender.Properties["reliability"] != "reliable" || viaSender.Properties["payload_status"] != "proven" ||
		viaSender.Properties["wrapper_depth"] != "1" {
		t.Fatalf("embedded PeerSender evidence = %#v", viaSender.Properties)
	}
	assertTransportCarries(t, result.Facts, viaSender.ID, "acme.v1.Envelope")

	parameterID := nodeIDByQualified(t, result.Nodes, "example.com/app.SendViaPacketParameter")
	viaParameter := assertTransportOperation(t, result, parameterID, graph.EdgeSends, "send", "github.com/cafecito-games/goenet/pkg.Peer.Send")
	if viaParameter.Properties["channel"] != "3" || viaParameter.Properties["channel_status"] != "proven" ||
		viaParameter.Properties["reliability"] != "reliable" || viaParameter.Properties["payload_status"] != "proven" ||
		viaParameter.Properties["wrapper_depth"] != "1" {
		t.Fatalf("packet parameter wrapper evidence = %#v", viaParameter.Properties)
	}
	assertTransportCarries(t, result.Facts, viaParameter.ID, "acme.v1.Envelope")

	unknownOperation := false
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTransportOperation && node.Properties["direction"] == "send" &&
			node.Properties["channel_status"] == "unknown" && node.Properties["payload_status"] == "unknown" {
			unknownOperation = true
		}
	}
	if !unknownOperation {
		t.Fatalf("dynamic transport evidence missing: %#v", result.Nodes)
	}
	ambiguousOperation := false
	unreliableOperation := false
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTransportOperation && node.Properties["channel"] == "2" && node.Properties["reliability"] == "unreliable" {
			unreliableOperation = true
		}
		if node.Kind == graph.KindTransportOperation && node.Properties["direction"] == "send" &&
			node.Properties["channel_status"] == "ambiguous" && node.Properties["payload_status"] == "ambiguous" &&
			node.Properties["reliability"] == "unknown" {
			ambiguousOperation = true
			for _, fact := range result.Facts {
				if fact.FromID == node.ID && fact.Kind == graph.EdgeCarries {
					t.Fatalf("ambiguous wrapper payload produced carries edge: %#v", fact)
				}
			}
		}
	}
	if !ambiguousOperation {
		t.Fatalf("conflicting wrapper evidence was not preserved as ambiguous: %#v", result.Nodes)
	}
	if !unreliableOperation {
		t.Fatalf("exact unreliable flags were not normalized: %#v", result.Nodes)
	}
	var conflictDiagnostics []string
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, "conflicting ENet wrapper summaries for ") {
			conflictDiagnostics = append(conflictDiagnostics, diagnostic.Message)
		}
	}
	wantConflictDiagnostics := []string{
		"conflicting ENet wrapper summaries for github.com/cafecito-games/goenet/pkg.Host.Broadcast; transport evidence marked ambiguous",
		"conflicting ENet wrapper summaries for github.com/cafecito-games/goenet/pkg.Peer.Send; transport evidence marked ambiguous",
		"conflicting ENet wrapper summaries for github.com/cafecito-games/goenet/pkg.Peer.Send; transport evidence marked ambiguous",
	}
	if !reflect.DeepEqual(conflictDiagnostics, wantConflictDiagnostics) {
		t.Fatalf("multi-API conflict diagnostics = %#v, want %#v", conflictDiagnostics, wantConflictDiagnostics)
	}
	truncatedOperation := false
	nodesByID := map[string]graph.Node{}
	for _, node := range result.Nodes {
		nodesByID[node.ID] = node
	}
	for _, fact := range result.Facts {
		node := nodesByID[fact.TargetID]
		if fact.FromID == useID && fact.Kind == graph.EdgeSends && node.Properties["channel_status"] == "truncated" &&
			node.Properties["payload_status"] == "truncated" {
			truncatedOperation = true
		}
	}
	if !truncatedOperation {
		t.Fatalf("bounded wrapper evidence did not surface truncation: %#v", result.Nodes)
	}
	ordinaryID := nodeIDByQualified(t, result.Nodes, "example.com/app.OrdinaryCall")
	redeclaredID := nodeIDByQualified(t, result.Nodes, "example.com/app.RedeclaredCall")
	uncertainID := nodeIDByQualified(t, result.Nodes, "example.com/app.UncertainTransport")
	for _, fact := range result.Facts {
		if fact.FromID == ordinaryID && (fact.Kind == graph.EdgeSends || fact.Kind == graph.EdgeReceives) {
			t.Fatalf("same-name non-ENet API produced transport fact: %#v", fact)
		}
		if fact.FromID == redeclaredID && (fact.Kind == graph.EdgeSends || fact.Kind == graph.EdgeReceives) {
			t.Fatalf("interface redeclaring Send produced transport fact: %#v", fact)
		}
		if fact.FromID == uncertainID && fact.Kind == graph.EdgeSends {
			for _, carried := range result.Facts {
				if carried.FromID == fact.TargetID && carried.Kind == graph.EdgeCarries {
					t.Fatalf("branch-dependent Go payload produced carries edge: %#v", carried)
				}
			}
		}
	}
}

func assertTransportOperation(t *testing.T, result graph.ParseResult, fromID string, relation graph.EdgeKind, direction, api string) graph.Node {
	t.Helper()
	nodes := map[string]graph.Node{}
	for _, node := range result.Nodes {
		nodes[node.ID] = node
	}
	var matched graph.Node
	for _, fact := range result.Facts {
		node := nodes[fact.TargetID]
		if fact.FromID == fromID && fact.Kind == relation && node.Kind == graph.KindTransportOperation &&
			node.Properties["direction"] == direction && node.Properties["api"] == api {
			if matched.ID == "" || node.Properties["payload_status"] == "proven" {
				matched = node
			}
		}
	}
	if matched.ID != "" {
		return matched
	}
	t.Fatalf("missing %s %s operation for %q: nodes=%#v facts=%#v diagnostics=%#v", direction, api, fromID, result.Nodes, result.Facts, result.Diagnostics)
	return graph.Node{}
}

func assertTransportCarries(t *testing.T, facts []graph.Fact, operationID, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID == operationID && fact.Kind == graph.EdgeCarries && fact.Target == target && fact.TargetID != "" {
			return
		}
	}
	t.Fatalf("operation %q does not carry %q: %#v", operationID, target, facts)
}

func TestPackageSemanticLoaderFailsClosedForIllTypedProtobufUsage(t *testing.T) {
	root := protobufUsageFixture(t)
	content := []byte(`package app
import (
	generated "example.com/app/gen"
	wire "google.golang.org/protobuf/proto"
)
func Broken() { _, _ = wire.Marshal(&generated.Envelope{Title: "x"}); missing() }
`)
	writeFile(t, filepath.Join(root, "app.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "app.go", Content: content, Repository: "protobuf-usage",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("ill-typed package produced no diagnostic")
	}
	for _, fact := range result.Facts {
		if fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("ill-typed package produced protocol usage: %#v", fact)
		}
	}
}

func TestParserSuppressesProtocolUseFromConfiguredGeneratedOutputWithDriftedHeader(t *testing.T) {
	root := protobufUsageFixture(t)
	path := filepath.Join(root, "gen", "schema.pb.go")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "source: schema.proto", "source: drifted.proto", 1))
	writeFile(t, path, string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "gen/schema.pb.go", Content: content, Repository: "protobuf-usage",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindMethod, "GetTitle")
	if len(result.Diagnostics) == 0 || !strings.Contains(result.Diagnostics[len(result.Diagnostics)-1].Message, "provenance rejected") {
		t.Fatalf("drifted generated header was not diagnosed: %#v", result.Diagnostics)
	}
	for _, fact := range result.Facts {
		if fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("configured generated implementation became an application consumer: %#v", fact)
		}
	}
}

func TestParserRejectsAmbiguousProtocolBinding(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n\ngo 1.26\n")
	for _, side := range []string{"left", "right"} {
		writeFile(t, filepath.Join(root, side, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
		writeFile(t, filepath.Join(root, side, "buf.gen.yaml"), `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: ../gen
    opt: paths=source_relative
`)
		writeFile(t, filepath.Join(root, side, "proto", "schema.proto"), `syntax = "proto3";
package `+side+`;
option go_package = "example.com/app/gen;generated";
message Envelope { string value = 1; }
`+map[string]string{"left": "message Unique { string value = 2; }\n"}[side])
	}
	writeFile(t, filepath.Join(root, "gen", "schema.pb.go"), `// Code generated by protoc-gen-go. DO NOT EDIT.
// source: schema.proto
package generated
type Envelope struct { Value string }
func (value *Envelope) GetValue() string { return value.Value }
type Unique struct { Value string }
func (value *Unique) GetValue() string { return value.Value }
`)
	content := []byte(`package app
import generated "example.com/app/gen"
func Read(value *generated.Envelope) string { return value.GetValue() }
`)
	writeFile(t, filepath.Join(root, "app.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "app.go", Content: content, Repository: "ambiguous",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	foundDiagnostic := false
	for _, diagnostic := range result.Diagnostics {
		foundDiagnostic = foundDiagnostic || strings.Contains(diagnostic.Message, "ambiguous Protobuf binding")
	}
	if !foundDiagnostic {
		t.Fatalf("ambiguous binding produced no diagnostic: %#v", result.Diagnostics)
	}
	for _, fact := range result.Facts {
		if fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("ambiguous binding produced canonical usage: %#v", fact)
		}
	}
	generatedContent, err := os.ReadFile(filepath.Join(root, "gen", "schema.pb.go"))
	if err != nil {
		t.Fatal(err)
	}
	generatedResult, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "gen/schema.pb.go", Content: generatedContent, Repository: "ambiguous",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range generatedResult.Facts {
		if fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("conflicted generated output became an application consumer: %#v", fact)
		}
	}
}

func protobufUsageFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), `module example.com/app

go 1.26

require google.golang.org/protobuf v0.0.0
replace google.golang.org/protobuf => ./third_party/protobuf
`)
	writeFile(t, filepath.Join(root, "third_party", "protobuf", "go.mod"), "module google.golang.org/protobuf\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "third_party", "protobuf", "proto", "proto.go"), `package proto
type Message interface{}
func Marshal(Message) ([]byte, error) { return nil, nil }
func Unmarshal([]byte, Message) error { return nil }
`)
	writeFile(t, filepath.Join(root, "buf.gen.yaml"), `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen
    opt: paths=source_relative
`)
	writeFile(t, filepath.Join(root, "schema.proto"), `syntax = "proto3";
package acme.v1;
option go_package = "example.com/app/gen;generated";
message Envelope {
  string title = 1;
  oneof payload { string text = 2; }
}
`)
	writeFile(t, filepath.Join(root, "gen", "schema.pb.go"), `// Code generated by protoc-gen-go. DO NOT EDIT.
// source: schema.proto
package generated
type Envelope struct { Title string; Payload isEnvelope_Payload }
func (value *Envelope) GetTitle() string { return value.Title }
type isEnvelope_Payload interface { isEnvelope_Payload() }
type Envelope_Text struct { Text string }
func (*Envelope_Text) isEnvelope_Payload() {}
`)
	writeFile(t, filepath.Join(root, "app.go"), `package app
import (
	generated "example.com/app/gen"
	wire "google.golang.org/protobuf/proto"
)
type EnvelopeAlias = generated.Envelope
type Ordinary struct { Title string }
func (*Ordinary) GetTitle() string { return "" }
func Use(data []byte, input *generated.Envelope) {
	value := &EnvelopeAlias{Title: "hello", Payload: &generated.Envelope_Text{Text: "world"}}
	encode := wire.Marshal
	_, _ = encode(value)
	_ = wire.Unmarshal(data, input)
	_ = input.GetTitle()
	_ = input.Title
	input.Title = "updated"
	switch input.Payload.(type) {
	case *generated.Envelope_Text:
	}
}
func FalsePositive(value *Ordinary) {
	_ = value.GetTitle()
	_ = value.Title
	_, _ = wire.Marshal(value)
}
`)
	return root
}

func assertProtocolFact(t *testing.T, facts []graph.Fact, fromID string, kind graph.EdgeKind, target, form string) {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID != fromID || fact.Kind != kind || fact.Target != target || fact.Properties["form"] != form {
			continue
		}
		if fact.TargetID == "" || fact.TargetKind == "" || fact.Location.Path == "" || fact.Location.Line == 0 ||
			fact.Properties["protocol"] != "protobuf" || fact.Properties["api"] == "" || fact.Properties["binding"] == "" ||
			fact.Properties["binding_id"] == "" || fact.Properties["static_type"] == "" ||
			fact.Properties["evidence"] != "go/types" {
			t.Fatalf("protocol fact lost canonical evidence: %#v", fact)
		}
		return
	}
	t.Fatalf("missing %s protocol fact from %q to %q with form %q", kind, fromID, target, form)
}

func TestPackageSemanticLoaderRetainsProvenFactsForBrokenPackage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/broken\n\ngo 1.26\n")
	content := []byte("package broken\nfunc helper() {}\nfunc run(value any) { defer helper(); panic(value); _ = recover(); missing() }\n")
	writeFile(t, filepath.Join(root, "broken.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "broken.go", Content: content, Repository: "broken", RepoID: "repo", GoModule: "example.com/broken",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "example.com/broken.helper")
	assertHasFactKind(t, result.Facts, graph.EdgePanics)
	assertHasFact(t, result.Facts, graph.EdgeRecovers, "builtin.recover")
	assertHasFact(t, result.Facts, graph.EdgeDefers, "example.com/broken.helper")
	if len(result.Diagnostics) == 0 {
		t.Fatal("expected type-checking diagnostic")
	}
}

func TestPackageSemanticLoaderExtractsFailureAndCleanupFlow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/failures\n\ngo 1.26\n")
	content := []byte(`package failures
import (
	"errors"
	"fmt"
	"log"
)

var ErrSentinel = errors.New("sentinel")

type Problem struct{}
func (*Problem) Error() string { return "problem" }

type Source struct{}
func (Source) Load() error { return ErrSentinel }

type Cleaner struct{}
func (Cleaner) Close() {}

type Provider interface { Read() (string, error) }
type BigProvider interface { Provider }

func Produce() (int, error) { return 0, ErrSentinel }
func Multi() (error, error) { return ErrSentinel, &Problem{} }
func cleanup() {}
func callbackCleanup() {}
func worker() {}
func Register(func()) {}

func Wrap() error {
	_, err := Produce()
	if err != nil {
		return fmt.Errorf("produce: %w", err)
	}
	return nil
}

func Join(left, right error) error { return errors.Join(left, right) }
func Mixed() error { return fmt.Errorf("seen %v, wrap %w", ErrSentinel, &Problem{}) }
func Method(source Source) error { return source.Load() }

func Handle() {
	_, err := Produce()
	if errors.Is(err, ErrSentinel) { return }
	var problem *Problem
	if errors.As(err, &problem) { return }
	if err == ErrSentinel { return }
}

func Switch() {
	_, err := Produce()
	switch err {
	case ErrSentinel:
		return
	}
}

func Retry() {
	for {
		_, err := Produce()
		if err != nil { continue }
		break
	}
}

func LogOnly() {
	_, err := Produce()
	if err != nil { log.Print(err) }
}

func Guard(value any, cleaner Cleaner) {
	defer cleanup()
	defer cleaner.Close()
	defer func() { _ = recover() }()
	func() { _ = recover() }()
	Register(func() { defer callbackCleanup(); panic(ErrSentinel) })
	go worker()
	panic(value)
}
`)
	writeFile(t, filepath.Join(root, "failures.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "failures.go", Content: content, Repository: "failures",
		RepoID: "repo", GoModule: "example.com/failures",
	})
	if err != nil {
		t.Fatal(err)
	}

	assertNodeProperty(t, result.Nodes, graph.KindVariable, "example.com/failures.ErrSentinel", "error_identity", "sentinel")
	assertNodeProperty(t, result.Nodes, graph.KindType, "example.com/failures.Problem", "error_type", "true")
	assertNodeProperty(t, result.Nodes, graph.KindFunction, "example.com/failures.Multi", "error_result_positions", "0,1")
	assertNodeProperty(t, result.Nodes, graph.KindMethod, "example.com/failures.Provider.Read", "error_result_positions", "1")
	assertNodeProperty(t, result.Nodes, graph.KindMethod, "example.com/failures.BigProvider.Read", "error_result_positions", "1")

	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("returns_error"), "example.com/failures.Multi", "builtin.error", "signature")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeReturnsError, "example.com/failures.Provider.Read", "builtin.error", "signature")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeReturnsError, "example.com/failures.BigProvider.Read", "builtin.error", "signature")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("propagates_error"), "example.com/failures.Produce", "example.com/failures.ErrSentinel", "return")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("wraps_error"), "example.com/failures.Wrap", "example.com/failures.Produce", "wrap")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("propagates_error"), "example.com/failures.Join", "builtin.error", "join")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeWrapsError, "example.com/failures.Mixed", "example.com/failures.Problem", "wrap")
	assertNoFailureFact(t, result.Nodes, result.Facts, graph.EdgeWrapsError, "example.com/failures.Mixed", "example.com/failures.ErrSentinel")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgePropagatesError, "example.com/failures.Method", "example.com/failures.Source.Load", "return_call")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Handle", "example.com/failures.ErrSentinel", "is")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Handle", "example.com/failures.Problem", "as")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Handle", "example.com/failures.ErrSentinel", "comparison")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Switch", "example.com/failures.ErrSentinel", "switch")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.Retry", "example.com/failures.Produce", "comparison")
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeKind("handles_error"), "example.com/failures.LogOnly")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("defers"), "example.com/failures.Guard", "example.com/failures.cleanup", "defer")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeDefers, "example.com/failures.Guard", "example.com/failures.Cleaner.Close", "defer")
	assertNoFailureTarget(t, result.Facts, graph.EdgeKind("defers"), "example.com/failures.worker")
	assertNoFailureTarget(t, result.Facts, graph.EdgeDefers, "example.com/failures.callbackCleanup")
	assertNoFailureTarget(t, result.Facts, graph.EdgePanics, "example.com/failures.ErrSentinel")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgeKind("recovers"), "example.com/failures.Guard", "builtin.recover", "recover")
	if count := failureFactCountFrom(t, result.Nodes, result.Facts, graph.EdgeRecovers, "example.com/failures.Guard"); count != 1 {
		t.Fatalf("semantic recover facts = %d, want only the deferred recovery closure", count)
	}
	panicFact := assertFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgeKind("panics"), "example.com/failures.Guard")
	if panicFact.TargetKind != graph.KindExternal || panicFact.Properties["unresolved"] != "true" {
		t.Fatalf("dynamic panic payload must stay unresolved: %#v", panicFact)
	}
	for _, fact := range result.Facts {
		switch fact.Kind {
		case graph.EdgeKind("returns_error"), graph.EdgeKind("propagates_error"), graph.EdgeKind("handles_error"),
			graph.EdgeKind("wraps_error"), graph.EdgeKind("panics"), graph.EdgeKind("recovers"), graph.EdgeKind("defers"):
			if fact.Location.Path == "" || fact.Location.Line == 0 || fact.Properties["form"] == "" || fact.Properties["evidence"] == "" {
				t.Fatalf("failure fact lost source or evidence: %#v", fact)
			}
		}
	}
}

func TestSyntaxFailureFallbackSeesPackageShadowingAcrossFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "shadow.go"), "package sample\nvar panic = func(any) {}\n")
	content := []byte("package sample\nfunc Guard(value any) { panic(value) }\n")
	writeFile(t, filepath.Join(root, "guard.go"), string(content))

	result, err := golangparser.NewWithSemanticLoader(nil).Parse(context.Background(), parserapi.Input{
		Root: root, Path: "guard.go", Content: content, Repository: "sample", RepoID: "repo", GoModule: "example.com/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoFailureFactFrom(t, result.Nodes, result.Facts, graph.EdgePanics, "example.com/sample.Guard")
}

func TestFailureIdentityUsesPackageObjectsNotAliasesOrSimpleNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/identity\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "left", "left.go"), "package left\ntype Problem struct{}\nfunc (*Problem) Error() string { return \"left\" }\n")
	writeFile(t, filepath.Join(root, "right", "right.go"), "package right\ntype Problem struct{}\nfunc (*Problem) Error() string { return \"right\" }\n")
	content := []byte(`package identity
import (
	l "example.com/identity/left"
	r "example.com/identity/right"
)
func Left() error { return &l.Problem{} }
func Right() error { return &r.Problem{} }
`)
	writeFile(t, filepath.Join(root, "identity.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "identity.go", Content: content, Repository: "identity",
		RepoID: "repo", GoModule: "example.com/identity",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgePropagatesError,
		"example.com/identity.Left", "example.com/identity/left.Problem", "return")
	assertHasFailureFact(t, result.Nodes, result.Facts, graph.EdgePropagatesError,
		"example.com/identity.Right", "example.com/identity/right.Problem", "return")
}

func TestPackageSemanticLoaderComposesChiRoutesAndMiddleware(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), `module example.com/app

go 1.26

require github.com/go-chi/chi/v5 v5.0.0

replace github.com/go-chi/chi/v5 => ./third_party/chi
`)
	writeFile(t, filepath.Join(root, "third_party", "chi", "go.mod"), "module github.com/go-chi/chi/v5\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "third_party", "chi", "chi.go"), `package chi

import "net/http"

type Router interface {
	http.Handler
	Use(...func(http.Handler) http.Handler)
	With(...func(http.Handler) http.Handler) Router
	Group(func(Router)) Router
	Route(string, func(Router)) Router
	Mount(string, http.Handler)
	Get(string, http.HandlerFunc)
	Post(string, http.HandlerFunc)
	Method(string, string, http.Handler)
	MethodFunc(string, string, http.HandlerFunc)
	Handle(string, http.Handler)
	HandleFunc(string, http.HandlerFunc)
}

type Mux struct{}
func NewRouter() *Mux { return &Mux{} }
func (*Mux) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*Mux) Use(...func(http.Handler) http.Handler) {}
func (*Mux) With(...func(http.Handler) http.Handler) Router { return &Mux{} }
func (*Mux) Group(func(Router)) Router { return &Mux{} }
func (*Mux) Route(string, func(Router)) Router { return &Mux{} }
func (*Mux) Mount(string, http.Handler) {}
func (*Mux) Get(string, http.HandlerFunc) {}
func (*Mux) Post(string, http.HandlerFunc) {}
func (*Mux) Method(string, string, http.Handler) {}
func (*Mux) MethodFunc(string, string, http.HandlerFunc) {}
func (*Mux) Handle(string, http.Handler) {}
func (*Mux) HandleFunc(string, http.HandlerFunc) {}
`)
	content := []byte(`package app

import (
	"net/http"
	"github.com/go-chi/chi/v5"
)

const api = "/v1"

func outer(next http.Handler) http.Handler { return next }
func authUse(next http.Handler) http.Handler { return next }
func audit(next http.Handler) http.Handler { return next }
func adminUse(next http.Handler) http.Handler { return next }
func childUse(next http.Handler) http.Handler { return next }
func login(http.ResponseWriter, *http.Request) {}
func me(http.ResponseWriter, *http.Request) {}
func list(http.ResponseWriter, *http.Request) {}

func authRoutes(r chi.Router) {
	r.Use(authUse)
	r.With(audit).Post("/login", login)
	alias := audit
	r.With(alias).Post("/alias", login)
	r.Get("/plain", me)
	r.Group(func(group chi.Router) {
		group.Get("/me", me)
	})
}

func childRoutes() chi.Router {
	r := chi.NewRouter()
	r.Use(childUse)
	r.Get("/items", list)
	return r
}

func dynamicOnly(r chi.Router) { r.Get("/hidden", me) }
func applyHelper(r chi.Router) { r.Use(audit) }
func dynamicMethod(r chi.Router, method string) { r.MethodFunc(method, "/dynamic-method", me) }
func identity(r chi.Router) chi.Router { return r }
func rebindHelper(r chi.Router) {
	r = chi.NewRouter()
	r.Get("/orphan", me)
}

type server struct { router chi.Router }
func (s *server) fieldRoute() { s.router.Get("/field", me) }

func Routes(dynamic string) chi.Router {
	r := chi.NewRouter()
	r.Use(outer)
	helperRouter := chi.NewRouter()
	applyHelper(helperRouter)
	helperRouter.Get("/use", me)
	r.Mount("/helper", helperRouter)
	aliasRouter := chi.NewRouter()
	alias := aliasRouter
	applyHelper(alias)
	aliasRouter.Get("/use", me)
	r.Mount("/copied-helper", aliasRouter)
	dynamicMethod(r, dynamic)
	rebindHelper(r)
	applyHelper(identity(r))
	var fieldServer server
	applyHelper(fieldServer.router)
	if dynamic != "" {
		applyHelper(r)
	}
	r.Get("/after-uncertain", me)
	r.Route(api+"/auth", authRoutes)
	r.Route("/one", authRoutes)
	r.Route("/two", authRoutes)
	r.Group(func(admin chi.Router) {
		admin.Use(adminUse)
		r.Get("/captured-outer", me)
		admin.Get("/scoped", me)
		admin.Mount("/admin", childRoutes())
	})
	r.Mount("/child", childRoutes())
	factory := childRoutes
	r.Mount("/factory", factory())
	r.MethodFunc(http.MethodPatch, "/method", me)
	r.HandleFunc("/any", me)
	if dynamic != "" {
		r.Get("/conditional", me)
	}
	chosen := chooseRouter(dynamic != "", r, childRoutes())
	chosen.Get("/ambiguous", me)
	cycleA(r)
	r.Route(dynamic, authRoutes)
	r.Route(dynamic, dynamicOnly)
	return r
}

func chooseRouter(first bool, left, right chi.Router) chi.Router {
	if first { return left }
	return right
}
func cycleA(r chi.Router) { cycleB(r) }
func cycleB(r chi.Router) { cycleA(r) }

func VoidRoot() {
	r := chi.NewRouter()
	rebindHelper(r)
	r.Get("/void-real", me)
}

type unrelated struct{}
func (unrelated) Get(string, http.HandlerFunc) {}
func NotARouter() { unrelated{}.Get("/invented", login) }
`)
	writeFile(t, filepath.Join(root, "routes.go"), string(content))

	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "routes.go", Content: content, Repository: "app",
		RepoID: "repo", GoModule: "example.com/app",
	})
	if err != nil {
		t.Fatal(err)
	}

	wantMiddleware := map[string][]string{
		"POST /v1/auth/login":    {"example.com/app.outer", "example.com/app.authUse", "example.com/app.audit"},
		"POST /v1/auth/alias":    {"example.com/app.outer", "example.com/app.authUse", "example.com/app.audit"},
		"GET /v1/auth/me":        {"example.com/app.outer", "example.com/app.authUse"},
		"GET /v1/auth/plain":     {"example.com/app.outer", "example.com/app.authUse"},
		"POST /one/login":        {"example.com/app.outer", "example.com/app.authUse", "example.com/app.audit"},
		"POST /two/login":        {"example.com/app.outer", "example.com/app.authUse", "example.com/app.audit"},
		"GET /admin/items":       {"example.com/app.outer", "example.com/app.adminUse", "example.com/app.childUse"},
		"GET /captured-outer":    {"example.com/app.outer"},
		"GET /scoped":            {"example.com/app.outer", "example.com/app.adminUse"},
		"GET /child/items":       {"example.com/app.outer", "example.com/app.childUse"},
		"GET /factory/items":     {"example.com/app.outer", "example.com/app.childUse"},
		"PATCH /method":          {"example.com/app.outer"},
		"ANY /any":               {"example.com/app.outer"},
		"GET /conditional":       {"example.com/app.outer"},
		"GET /helper/use":        {"example.com/app.outer", "example.com/app.audit"},
		"GET /copied-helper/use": {"example.com/app.outer", "example.com/app.audit"},
		"GET /after-uncertain":   {"example.com/app.outer"},
		"GET /void-real":         {},
	}
	endpoints := map[string]graph.Node{}
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint {
			endpoints[node.Name] = node
		}
	}
	for name, middleware := range wantMiddleware {
		endpoint, ok := endpoints[name]
		if !ok {
			t.Fatalf("missing composed endpoint %q; got %#v", name, endpoints)
		}
		got := make([]string, len(middleware))
		for _, fact := range result.Facts {
			if fact.FromID == endpoint.ID && fact.Kind == graph.EdgeKind("uses_middleware") {
				order, orderErr := strconv.Atoi(fact.Properties["order"])
				if orderErr != nil || order < 0 || order >= len(got) || fact.Properties["resolution"] != "go/types" {
					t.Fatalf("middleware evidence is not ordered and typed: %#v", fact)
				}
				got[order] = fact.Target
			}
		}
		if !reflect.DeepEqual(got, middleware) {
			t.Fatalf("middleware for %s = %v, want %v", name, got, middleware)
		}
		wantHandler := "example.com/app.me"
		if strings.Contains(name, "login") || strings.Contains(name, "alias") {
			wantHandler = "example.com/app.login"
		} else if strings.Contains(name, "items") {
			wantHandler = "example.com/app.list"
		}
		foundHandler := false
		for _, fact := range result.Facts {
			foundHandler = foundHandler || fact.FromID == endpoint.ID && fact.Kind == graph.EdgeHandledBy &&
				fact.Target == wantHandler && fact.Properties["resolution"] == "go/types"
		}
		if !foundHandler {
			t.Fatalf("endpoint %s lost its exact handler %s", name, wantHandler)
		}
	}
	for _, forbidden := range []string{"POST /login", "GET /me", "GET /items", "GET /hidden", "GET /invented", "GET /ambiguous", "GET /orphan"} {
		if _, ok := endpoints[forbidden]; ok {
			t.Fatalf("invented or uncomposed endpoint %q: %#v", forbidden, endpoints[forbidden])
		}
	}
	foundDynamicDiagnostic, foundCycleDiagnostic, foundAmbiguousDiagnostic := false, false, false
	foundDynamicMethodDiagnostic, foundRouterReceiverDiagnostic := false, false
	foundArgumentDiagnostic, foundReassignmentDiagnostic, foundConditionalMutationDiagnostic := false, false, false
	for _, diagnostic := range result.Diagnostics {
		foundDynamicDiagnostic = foundDynamicDiagnostic || strings.Contains(diagnostic.Message, "dynamic Chi route prefix")
		foundCycleDiagnostic = foundCycleDiagnostic || strings.Contains(diagnostic.Message, "recursive Chi router composition")
		foundAmbiguousDiagnostic = foundAmbiguousDiagnostic || strings.Contains(diagnostic.Message, "ambiguous Chi router helper result")
		foundDynamicMethodDiagnostic = foundDynamicMethodDiagnostic || strings.Contains(diagnostic.Message, "dynamic Chi endpoint method")
		foundRouterReceiverDiagnostic = foundRouterReceiverDiagnostic || strings.Contains(diagnostic.Message, "Chi router receiver could not be proven")
		foundArgumentDiagnostic = foundArgumentDiagnostic || diagnostic.Message == "Chi router helper argument state could not be propagated; subsequent composition omitted"
		foundReassignmentDiagnostic = foundReassignmentDiagnostic || diagnostic.Message == "Chi router helper argument state could not be propagated after reassignment; subsequent composition omitted"
		foundConditionalMutationDiagnostic = foundConditionalMutationDiagnostic || strings.Contains(diagnostic.Message, "conditional Chi router state mutation")
	}
	if !foundDynamicDiagnostic || !foundCycleDiagnostic || !foundAmbiguousDiagnostic ||
		!foundDynamicMethodDiagnostic || !foundRouterReceiverDiagnostic || !foundArgumentDiagnostic ||
		!foundReassignmentDiagnostic || !foundConditionalMutationDiagnostic {
		t.Fatalf("fail-closed Chi composition was not diagnosed: %#v", result.Diagnostics)
	}
	conditional := endpoints["GET /conditional"]
	for _, fact := range result.Facts {
		if fact.FromID == conditional.ID && fact.Kind == graph.EdgeHandledBy && fact.Properties["conditional"] != "true" {
			t.Fatalf("conditional registration lost its evidence: %#v", fact)
		}
	}
}

func TestPackageSemanticLoaderKeepsProvenHTTPAndChiEvidenceWithUnrelatedErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), `module example.com/partial

go 1.26

require github.com/go-chi/chi/v5 v5.0.0
replace github.com/go-chi/chi/v5 => ./third_party/chi
`)
	writeMinimalChiModule(t, root)
	content := []byte(`package partial
import (
	"net/http"
	"github.com/go-chi/chi/v5"
)
func handler(http.ResponseWriter, *http.Request) {}
func Routes() {
	router := chi.NewRouter()
	router.Route("/v1", func(r chi.Router) { r.Get("/items", handler) })
	_ = unrelatedMissingName
}
func Send(client *http.Client) {
	_, _ = client.Post("/v1/characters", "application/json", nil)
}
`)
	writeFile(t, filepath.Join(root, "routes.go"), string(content))
	result, err := golangparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "routes.go", Content: content, Repository: "partial", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindEndpoint, "GET /v1/items")
	for _, node := range result.Nodes {
		if node.Kind == graph.KindEndpoint && (node.Name == "GET /items" || node.Name == "POST /v1/characters") {
			t.Fatalf("partial semantic evidence invented server endpoint: %#v", node)
		}
	}
	request := assertFact(t, result.Facts, graph.EdgeRequests, "POST /v1/characters")
	if request.Properties["resolution"] != "go/types" {
		t.Fatalf("partial-package HTTP request was not type resolved: %#v", request)
	}
	foundDiagnostic := false
	for _, diagnostic := range result.Diagnostics {
		foundDiagnostic = foundDiagnostic || strings.Contains(diagnostic.Message, "unrelatedMissingName")
	}
	if !foundDiagnostic {
		t.Fatalf("unrelated package error was not retained: %#v", result.Diagnostics)
	}
}

func TestChiHelperEditInvalidatesEveryGoPackageView(t *testing.T) {
	parser := golangparser.New()
	got := parser.SemanticAffectedPaths(
		[]string{"routes.go", "helpers.go", "nested/child.go", "README.md"},
		[]string{"helpers.go"},
	)
	if !reflect.DeepEqual(got, []string{"routes.go", "helpers.go", "nested/child.go"}) {
		t.Fatalf("affected paths = %v, want every Go package view", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMinimalChiModule(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "third_party", "chi", "go.mod"), "module github.com/go-chi/chi/v5\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "third_party", "chi", "chi.go"), `package chi
import "net/http"
type Router interface {
	http.Handler
	Route(string, func(Router)) Router
	Get(string, http.HandlerFunc)
}
type Mux struct{}
func NewRouter() *Mux { return &Mux{} }
func (*Mux) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*Mux) Route(string, func(Router)) Router { return &Mux{} }
func (*Mux) Get(string, http.HandlerFunc) {}
`)
}

func assertHasFactKind(t *testing.T, facts []graph.Fact, kind graph.EdgeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s fact", kind)
}

func assertHasNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, name string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.Name == name {
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, name)
}

func assertHasNodeQualified(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, qualified)
}

func assertHasFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	var available []string
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
		if fact.Kind == kind {
			available = append(available, fact.Target)
		}
	}
	t.Fatalf("missing %s fact to %q; available targets: %v", kind, target, available)
}

func assertFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) graph.Fact {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return fact
		}
	}
	t.Fatalf("missing %s fact targeting %q: %#v", kind, target, facts)
	return graph.Fact{}
}

func assertNodeProperty(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified, key, want string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			if got := node.Properties[key]; got != want {
				t.Fatalf("%s property %s = %q, want %q: %#v", qualified, key, got, want, node)
			}
			return
		}
	}
	t.Fatalf("missing %s node %q", kind, qualified)
}

func assertHasFailureFact(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from, target, form string) {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target && fact.FromID == fromID && fact.Properties["form"] == form {
			return
		}
	}
	t.Fatalf("missing %s fact from %q to %q with form %q", kind, from, target, form)
}

func assertFailureFactFrom(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from string) graph.Fact {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID {
			return fact
		}
	}
	t.Fatalf("missing %s fact from %q", kind, from)
	return graph.Fact{}
}

func assertNoFailureFactFrom(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from string) {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID {
			t.Fatalf("unexpected %s fact from %q: %#v", kind, from, fact)
		}
	}
}

func failureFactCountFrom(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from string) int {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	count := 0
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID {
			count++
		}
	}
	return count
}

func assertNoFailureFact(t *testing.T, nodes []graph.Node, facts []graph.Fact, kind graph.EdgeKind, from, target string) {
	t.Helper()
	fromID := nodeIDByQualified(t, nodes, from)
	for _, fact := range facts {
		if fact.Kind == kind && fact.FromID == fromID && fact.Target == target {
			t.Fatalf("unexpected %s fact from %q to %q: %#v", kind, from, target, fact)
		}
	}
}

func nodeIDByQualified(t *testing.T, nodes []graph.Node, qualified string) string {
	t.Helper()
	for _, node := range nodes {
		if node.QualifiedName == qualified {
			return node.ID
		}
	}
	t.Fatalf("missing node %q", qualified)
	return ""
}

func assertNoFailureTarget(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			t.Fatalf("unexpected %s fact to %q: %#v", kind, target, fact)
		}
	}
}

func TestWorkspaceSemanticEvidenceTracksExternalInputs(t *testing.T) {
	root := t.TempDir()
	externalRoot := t.TempDir()
	external := filepath.Join(externalRoot, "go.work")
	if err := os.WriteFile(external, []byte("go 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", external)
	parser := golangparser.New()
	first, err := parser.WorkspaceSemanticEvidenceKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("go 1.26\nuse ./service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := parser.WorkspaceSemanticEvidenceKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("external go.work content did not change workspace evidence")
	}
	vendorManifest := filepath.Join(externalRoot, "vendor", "modules.txt")
	writeFile(t, vendorManifest, "# example.com/dependency v1.0.0\n")
	third, err := parser.WorkspaceSemanticEvidenceKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if second == third {
		t.Fatal("external workspace vendor manifest did not change workspace evidence")
	}
	t.Setenv("GOFLAGS", "-tags=freshness")
	fourth, err := parser.WorkspaceSemanticEvidenceKey(context.Background(), parserapi.Input{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if third == fourth {
		t.Fatal("Go build environment did not change workspace evidence")
	}
}
