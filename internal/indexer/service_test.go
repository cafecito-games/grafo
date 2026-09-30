package indexer_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	configparser "github.com/cafecito-games/grafo/internal/parser/config"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
	godotparser "github.com/cafecito-games/grafo/internal/parser/godot"
	golangparser "github.com/cafecito-games/grafo/internal/parser/golang"
	manifestparser "github.com/cafecito-games/grafo/internal/parser/manifest"
	markdownparser "github.com/cafecito-games/grafo/internal/parser/markdown"
	protobufparser "github.com/cafecito-games/grafo/internal/parser/protobuf"
	"github.com/cafecito-games/grafo/internal/parser/protobufbinding"
	pythonparser "github.com/cafecito-games/grafo/internal/parser/python"
	sqlparser "github.com/cafecito-games/grafo/internal/parser/sql"
	postgresparser "github.com/cafecito-games/grafo/internal/parser/sql/postgres"
	typescriptparser "github.com/cafecito-games/grafo/internal/parser/typescript"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type deletingParser struct{}

func (deletingParser) Language() string          { return "deleting-test" }
func (deletingParser) Supports(path string) bool { return strings.HasSuffix(path, ".race") }
func (deletingParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	if input.Path == "app/a.race" {
		if err := os.Remove(filepath.Join(input.Root, "app", "b.race")); err != nil {
			return graph.ParseResult{}, err
		}
	}
	return parserapi.NewBuilder(input, "deleting-test").Finish(), nil
}

type forgingProducerParser struct{}

func (forgingProducerParser) Language() string          { return "go" }
func (forgingProducerParser) Supports(path string) bool { return strings.HasSuffix(path, ".forge") }
func (forgingProducerParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	builder := parserapi.NewBuilder(input, "go")
	method := graph.Node{ID: graph.NodeID(graph.KindMethod, "Forged.call"), Kind: graph.KindMethod,
		Name: "call", QualifiedName: "Forged.call", Location: graph.Location{Path: input.Path, Line: 1}}
	event := graph.Node{ID: graph.NodeID(graph.KindEvent, "Forged.ready"), Kind: graph.KindEvent,
		Name: "ready", QualifiedName: "Forged.ready", Location: graph.Location{Path: input.Path, Line: 1}}
	builder.AddNode(method)
	builder.AddNode(event)
	builder.AddFact(method.ID, graph.EdgeSubscribes, event.ID, "", graph.KindEvent,
		graph.Location{Path: input.Path, Line: 1}, map[string]string{"form": "connect"})
	result := builder.Finish()
	for index := range result.Facts {
		result.Facts[index].Producer = graph.ProducerGodot
	}
	return result, nil
}

type legacyVerbEndpointParser struct{}

func (legacyVerbEndpointParser) Language() string          { return "go" }
func (legacyVerbEndpointParser) Supports(path string) bool { return strings.HasSuffix(path, ".go") }
func (legacyVerbEndpointParser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	builder := parserapi.NewBuilder(input, "go")
	location := graph.Location{Path: input.Path, Line: 3, Column: 2, EndLine: 3}
	endpoint := builder.AddNode(graph.Node{Kind: graph.KindEndpoint, Name: "POST /v1/characters",
		QualifiedName: "endpoint:POST /v1/characters@client.go:3:2", Location: location})
	builder.AddFact(builder.FileID(), graph.EdgeExposes, endpoint, "", "", location, nil)
	return builder.Finish(), nil
}

func TestServiceHTTPAuthorityResolutionConvergesIncrementalAndClean(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/service\n\ngo 1.26\n")
	enableIndexerChi(t, root)
	writeHTTPFixture := func(call string) {
		write(t, filepath.Join(root, "service.go"), `package service
import (
	"net/http"
	"github.com/go-chi/chi/v5"
)
func Handler(http.ResponseWriter, *http.Request) {}
func Routes() { router := chi.NewRouter(); router.Get("/users/{id}", Handler) }
type API struct { baseURL string }
func (api *API) Call() { _, _ = http.Get(`+call+`) }
`)
	}
	writeHTTPFixture(`api.baseURL + "/users/42"`)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New(), manifestparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertIndexedRequestResolution(t, ctx, repository, true, httpmodel.DestinationUnresolved)

	writeHTTPFixture(`"/users/42"`)
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	incremental := indexedRequestEdges(t, ctx, repository)
	assertIndexedRequestResolution(t, ctx, repository, false, httpmodel.DestinationResolved)
	if _, err := service.Run(ctx, project, indexer.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	clean := indexedRequestEdges(t, ctx, repository)
	if !reflect.DeepEqual(incremental, clean) {
		t.Fatalf("incremental request resolution differs from clean rebuild:\n%#v\n%#v", incremental, clean)
	}

	writeHTTPFixture(`api.baseURL + "/users/42"`)
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	incremental = indexedRequestEdges(t, ctx, repository)
	assertIndexedRequestResolution(t, ctx, repository, true, httpmodel.DestinationUnresolved)
	if _, err := service.Run(ctx, project, indexer.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	clean = indexedRequestEdges(t, ctx, repository)
	if !reflect.DeepEqual(incremental, clean) {
		t.Fatalf("incremental unknown-authority resolution differs from clean rebuild:\n%#v\n%#v", incremental, clean)
	}
}

func enableIndexerChi(t *testing.T, root string) {
	t.Helper()
	modulePath := filepath.Join(root, "go.mod")
	content, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	write(t, modulePath, string(content)+"\nrequire github.com/go-chi/chi/v5 v5.0.0\nreplace github.com/go-chi/chi/v5 => ./third_party/chi\n")
	if err := os.MkdirAll(filepath.Join(root, "third_party", "chi"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "third_party", "chi", "go.mod"), "module github.com/go-chi/chi/v5\n\ngo 1.26\n")
	write(t, filepath.Join(root, "third_party", "chi", "chi.go"), `package chi
import "net/http"
type Router interface {
	http.Handler
	Get(string, http.HandlerFunc)
}
type Mux struct{}
func NewRouter() *Mux { return &Mux{} }
func (*Mux) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (*Mux) Get(string, http.HandlerFunc) {}
`)
}

func indexedRequestEdges(t *testing.T, ctx context.Context, repository *sqlite.Repository) []graph.Edge {
	t.Helper()
	call, err := query.NewService(repository).Resolve(ctx, "example.com/service.API.Call")
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, call.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := []graph.Edge{}
	for _, edge := range edges {
		if edge.Kind == graph.EdgeRequests {
			result = append(result, edge)
		}
	}
	return result
}

func assertIndexedRequestResolution(t *testing.T, ctx context.Context, repository *sqlite.Repository,
	external bool, resolution httpmodel.DestinationResolution,
) {
	t.Helper()
	edges := indexedRequestEdges(t, ctx, repository)
	if len(edges) != 1 {
		t.Fatalf("request edges = %#v", edges)
	}
	target, err := repository.Node(ctx, edges[0].ToID)
	if err != nil {
		t.Fatal(err)
	}
	if target.External != external || edges[0].Properties[httpmodel.PropertyDestinationResolution] != string(resolution) {
		t.Fatalf("request target=%#v edge=%#v", target, edges[0])
	}
}

func TestServiceIncrementallyRemovesLegacyVerbOnlyEndpoint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/client\n\ngo 1.26\n")
	source := "package client\nfunc post(string, any) {}\nfunc Run() { post(\"/v1/characters\", nil) }\n"
	write(t, filepath.Join(root, "client.go"), source)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	legacy := indexer.NewService(repository, parserapi.NewRegistry(legacyVerbEndpointParser{}))
	if _, err := legacy.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.ByKind[string(graph.KindEndpoint)] != 1 {
		t.Fatalf("legacy fixture did not persist false endpoint: %#v", counts.ByKind)
	}

	write(t, filepath.Join(root, "client.go"), source+"\n")
	current := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	report, err := current.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 1 || report.Counts.ByKind[string(graph.KindEndpoint)] != 0 {
		t.Fatalf("incremental refresh retained legacy false endpoint: %#v", report)
	}
	clean, err := current.Run(ctx, project, indexer.Options{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Counts, clean.Counts) {
		t.Fatalf("incremental endpoint removal differs from clean rebuild:\n%#v\n%#v", report.Counts, clean.Counts)
	}
}

func TestServiceSemanticRebuildRemovesLegacyHeaderRequest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/client\n\ngo 1.26\n")
	source := []byte(`package client
import "net/http"
func Run(request *http.Request) {
	_ = request.Header.Get("Authorization")
	_, _ = http.Get("/health")
}
`)
	write(t, filepath.Join(root, "client.go"), string(source))
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertHTTPRequestSet(t, ctx, repository, []string{"GET /health"})

	input := parserapi.Input{Root: root, Path: "client.go", Content: source, Repository: project.Name,
		RepoID: project.ID, GoModule: project.GoModule}
	legacy := parserapi.NewBuilder(input, "go")
	legacy.AddFact(legacy.FileID(), graph.EdgeRequests, "", "GET Authorization", graph.KindEndpoint,
		graph.Location{Path: "client.go", Line: 4, Column: 6}, map[string]string{
			"http_method": "GET", "http_raw_method": "get", "http_raw_route": "Authorization",
		})
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "client.go", Hash: "legacy-header-request",
		Language: "go", Size: int64(len(source)), IndexedAt: graph.NowUTC()}, legacy.Finish()); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", "31"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertHTTPRequestSet(t, ctx, repository, []string{"GET Authorization"})

	rebuilt, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Rebuild != "semantic schema changed" || !reflect.DeepEqual(rebuilt.Updated, []string{"client.go"}) {
		t.Fatalf("legacy request did not trigger a semantic rebuild: %#v", rebuilt)
	}
	assertHTTPRequestSet(t, ctx, repository, []string{"GET /health"})

	unchanged, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Updated) != 0 || unchanged.Unchanged != 1 {
		t.Fatalf("unchanged reindex did not converge: %#v", unchanged)
	}
	clean, err := service.Run(ctx, project, indexer.Options{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rebuilt.Counts, clean.Counts) {
		t.Fatalf("semantic rebuild differs from clean rebuild:\n%#v\n%#v", rebuilt.Counts, clean.Counts)
	}
}

func TestServiceNormalizesProducerBeforePersistence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "forged.forge"), "untrusted parser output")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(forgingProducerParser{}))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	methodID := graph.NodeID(graph.KindMethod, "Forged.call")
	edges, err := repository.EdgesFrom(ctx, methodID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Producer != "go" {
		t.Fatalf("persisted forged edge = %#v, want authoritative go producer", edges)
	}
	fileID := graph.NodeID(graph.KindFile, project.ID+":forged.forge")
	contains, err := repository.EdgesFrom(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundContainment := false
	for _, edge := range contains {
		if edge.Kind == graph.EdgeContains && edge.ToID == fileID {
			foundContainment = edge.Producer == graph.ProducerIndexer
		}
	}
	if !foundContainment {
		t.Fatalf("repository containment producer was not indexer: %#v", contains)
	}
	report, err := query.NewService(repository).GodotInteractions(ctx, "Forged.call", query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outbound) != 0 || len(report.Inbound) != 0 {
		t.Fatalf("forged producer entered Godot report: %#v", report)
	}
}

func TestServiceLinksDocumentationSectionsToCode(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "README.md"), "# Runtime flow\nThe function `Serve` delegates to class `Checkout` through endpoint `GET /orders`.\nSee [the implementation](server.ts).\n")
	write(t, filepath.Join(root, "server.ts"), "export class Checkout {}\nexport function Serve() {}\nconst app = express();\napp.get(\"/orders\", Serve);\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(markdownparser.New(), typescriptparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 2 || report.Counts.ByKind[string(graph.KindDocSection)] != 1 || report.Counts.ByEdge[string(graph.EdgeDocuments)] != 4 {
		t.Fatalf("unexpected documentation index report: %#v", report)
	}

	queries := query.NewService(repository)
	flow, err := queries.Neighborhood(ctx, "README.md#runtime-flow", "", 1, query.Outgoing, []graph.EdgeKind{graph.EdgeDocuments}, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, flow, "server.ts")
	assertReached(t, flow, "server.Serve")
	assertReached(t, flow, "server.Checkout")
	for _, reached := range flow.Nodes {
		if reached.Node.Kind == graph.KindEndpoint && reached.Node.Name == "GET /orders" && !reached.Node.External {
			return
		}
	}
	t.Fatalf("documentation did not resolve to endpoint: %#v", flow.Nodes)
}

func TestServiceReconcilesTypeScriptBindingsAfterBarrelAndConfigEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["src/*"]}}}`)
	write(t, filepath.Join(root, "src", "service.ts"), `export class Service { run() {} }`)
	write(t, filepath.Join(root, "src", "other.ts"), `export class Other { run() {} }`)
	write(t, filepath.Join(root, "src", "barrel.ts"), `export { Service } from "./service";`)
	write(t, filepath.Join(root, "src", "app.ts"), `
import { Service } from "@app/barrel";
export function execute() { const service = new Service(); service.run(); }
`)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	typeScriptParser := typescriptparser.New()
	service := indexer.NewService(repository, parserapi.NewRegistry(typeScriptParser, configparser.New()))
	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 5 {
		t.Fatalf("unexpected first TypeScript report: %#v", first)
	}
	if metrics := typeScriptParser.ResolutionMetrics(); metrics.CatalogLoads != 1 || metrics.CacheHits != 3 || metrics.CacheEntries != 1 || metrics.CachedModules != 4 {
		t.Fatalf("TypeScript catalog cache was not bounded and reused: %#v", metrics)
	}
	assertOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/service.Service.run")

	write(t, filepath.Join(root, "src", "barrel.ts"), `export { Other as Service } from "./other";`)
	second, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 4 { // all TypeScript modules, but not unchanged tsconfig JSON
		t.Fatalf("barrel change did not invalidate the TypeScript catalog: %#v", second)
	}
	assertOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/other.Other.run")
	assertNoOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/service.Service.run")

	write(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["missing/*"]}}}`)
	third, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Updated) != 5 {
		t.Fatalf("config change did not invalidate TypeScript modules: %#v", third)
	}
	assertNoOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/other.Other.run")

	clean, err := service.Run(ctx, project, indexer.Options{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Updated) != 5 {
		t.Fatalf("forced clean rebuild did not converge: %#v", clean)
	}
	assertNoOutgoingTarget(t, ctx, repository, "src/app.execute", graph.EdgeCalls, "src/other.Other.run")
}

func TestServiceReconcilesTypeScriptFetchMethodsAndTopology(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "server.ts"), `
function health() {}
function createOrder() {}
function updateOrder() {}
const app = express();
app.get("/health", health);
app.post("/orders", createOrder);
app.patch("/orders", updateOrder);
`)
	client := func(method string) string {
		return fmt.Sprintf(`
export function defaultHealth() { return fetch("/health"); }
export function writeOrder() { return fetch("/orders", { method: %q }); }
export function wrongMethod() { return fetch("/orders", { method: "DELETE" }); }
export function unknownMethod(method: string) { return fetch("/orders", { method }); }
export function dynamicURL(url: string) { return fetch(url, { method: "POST" }); }
`, method)
	}
	write(t, filepath.Join(root, "client.ts"), client("POST"))
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(typescriptparser.New()))
	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 2 {
		t.Fatalf("initial TypeScript report = %#v", first)
	}
	assertTypeScriptFetchTopology(t, ctx, repository, "POST")

	write(t, filepath.Join(root, "client.ts"), client("PATCH"))
	incremental, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental.Updated) != 2 || incremental.Updated[0] != "client.ts" || incremental.Updated[1] != "server.ts" {
		t.Fatalf("incremental TypeScript report = %#v", incremental)
	}
	assertTypeScriptFetchTopology(t, ctx, repository, "PATCH")
	incrementalCounts := incremental.Counts

	clean, err := service.Run(ctx, project, indexer.Options{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incrementalCounts, clean.Counts) {
		t.Fatalf("incremental fetch reconciliation differs from clean rebuild:\n%#v\n%#v", incrementalCounts, clean.Counts)
	}
	assertTypeScriptFetchTopology(t, ctx, repository, "PATCH")
}

func assertTypeScriptFetchTopology(t *testing.T, ctx context.Context, repository graph.TopologyRepository, writeMethod string) {
	t.Helper()
	requests, err := query.NewTopology(repository).OutboundRequests(ctx, query.TopologyOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	bySource := map[string]query.OutboundRequest{}
	for _, request := range requests.Requests {
		bySource[request.Source.Name] = request
	}
	if len(bySource) != 4 {
		t.Fatalf("persisted fetch requests = %#v", requests)
	}
	tests := []struct {
		source, method string
		status         query.BoundaryStatus
	}{
		{source: "defaultHealth", method: "GET", status: query.BoundaryResolved},
		{source: "writeOrder", method: writeMethod, status: query.BoundaryResolved},
		{source: "wrongMethod", method: "DELETE", status: query.BoundaryUnresolved},
		{source: "unknownMethod", method: "ANY", status: query.BoundaryUnresolved},
	}
	for _, test := range tests {
		request := bySource[test.source]
		if request.Method != test.method || request.Status != test.status {
			t.Fatalf("%s request = %#v", test.source, request)
		}
	}
	if bySource["unknownMethod"].Evidence.Properties["http_method_unknown"] != "true" {
		t.Fatalf("unknown method evidence = %#v", bySource["unknownMethod"].Evidence)
	}
	topology, err := query.NewTopology(repository).ServiceTopology(ctx, query.TopologyOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	resolved := map[string]bool{}
	for _, link := range topology.Links {
		if link.Status == query.BoundaryResolved {
			resolved[link.Method+" "+link.Route] = true
		}
	}
	for _, request := range []string{"GET /health", writeMethod + " /orders"} {
		if !resolved[request] {
			t.Fatalf("topology did not resolve %s: %#v", request, topology)
		}
	}
}

func TestServiceIndexesOnlyChangedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "main.go"), "package main\nfunc main() { helper() }\nfunc helper() {}\n")
	write(t, filepath.Join(root, "worker.py"), "def run():\n    return True\n")
	write(t, filepath.Join(root, "player.gd"), "class_name Player\nfunc run():\n\tpass\n")
	write(t, filepath.Join(root, "web.ts"), "export function handler() { return process.env.API_URL }\n")
	write(t, filepath.Join(root, "schema.sql"), "CREATE TABLE events (id bigint PRIMARY KEY);\n")
	write(t, filepath.Join(root, ".env"), "API_URL=http://localhost:8080\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(gdscriptparser.New(), golangparser.New(), pythonparser.New(), typescriptparser.New(), sqlparser.New(postgresparser.New()), configparser.New())
	service := indexer.NewService(repository, registry)

	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 6 || first.Unchanged != 0 {
		t.Fatalf("unexpected initial report: %#v", first)
	}
	second, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 0 || second.Unchanged != 6 {
		t.Fatalf("expected incremental no-op: %#v", second)
	}

	write(t, filepath.Join(root, "main.go"), "package main\nfunc main() { helper() }\nfunc helper() { println(\"changed\") }\n")
	if err := os.Remove(filepath.Join(root, "web.ts")); err != nil {
		t.Fatal(err)
	}
	third, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Updated) != 1 || len(third.Removed) != 1 || third.Removed[0] != "web.ts" {
		t.Fatalf("unexpected incremental update: %#v", third)
	}
}

func TestServiceReindexesGoDependentsWhenTypeEvidenceChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(root, "contract"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\ntype Runner interface { Run() }\n")
	write(t, filepath.Join(root, "worker", "worker.go"), "package worker\ntype Worker struct{}\nfunc (Worker) Run() {}\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	goParser := golangparser.New()
	service := indexer.NewService(repository, parserapi.NewRegistry(goParser))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 2 {
		t.Fatalf("initial index: report=%#v err=%v", report, err)
	}
	if metrics := goParser.SemanticLoadMetrics(); metrics.Loads != 1 || metrics.PeakConcurrent != 1 {
		t.Fatalf("semantic package loading was not bounded and shared: %#v", metrics)
	}

	write(t, filepath.Join(root, "contract", "runner.go"), "package contract\ntype Runner interface { Run(); Stop() }\n")
	reindexed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reindexed.Updated) != 2 {
		t.Fatalf("expected changed package and semantic dependents to reindex, got %#v", reindexed)
	}
	if reindexed.Counts.ByEdge[string(graph.EdgeImplements)] != 0 {
		t.Fatalf("stale implements edge survived interface edit: %#v", reindexed.Counts.ByEdge)
	}
	if metrics := goParser.SemanticLoadMetrics(); metrics.Loads != 2 {
		t.Fatalf("semantic cache did not invalidate after interface edit: %#v", metrics)
	}
	changedGOOS := "linux"
	if runtime.GOOS == changedGOOS {
		changedGOOS = "darwin"
	}
	t.Setenv("GOOS", changedGOOS)
	contextChanged, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(contextChanged.Updated) != 2 {
		t.Fatalf("build-context change did not invalidate Go semantic evidence: %#v", contextChanged)
	}
}

func TestServiceFailureFlowEditsConvergeWithCleanRebuild(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/failureedit\n\ngo 1.26\n")
	write(t, filepath.Join(root, "producer.go"), `package failureedit
import "errors"
var ErrLoad = errors.New("load")
func Load() error { return ErrLoad }
`)
	write(t, filepath.Join(root, "consumer.go"), `package failureedit
func Run() error { return Load() }
`)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 2 {
		t.Fatalf("initial failure index: report=%#v err=%v", report, err)
	}
	assertOutgoingTarget(t, ctx, repository, "example.com/failureedit.Run", graph.EdgePropagatesError, "example.com/failureedit.Load")

	write(t, filepath.Join(root, "producer.go"), `package failureedit
import "errors"
var ErrLoad = errors.New("load")
func Load() (int, error) { return 0, ErrLoad }
`)
	write(t, filepath.Join(root, "consumer.go"), `package failureedit
import "fmt"
func Run() error {
	_, err := Load()
	if err != nil { return fmt.Errorf("run: %w", err) }
	return nil
}
`)
	updated, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Updated) != 2 {
		t.Fatalf("signature/body edit did not invalidate both semantic files: %#v", updated)
	}
	assertOutgoingTarget(t, ctx, repository, "example.com/failureedit.Run", graph.EdgeWrapsError, "example.com/failureedit.Load")
	assertNoOutgoingTarget(t, ctx, repository, "example.com/failureedit.Run", graph.EdgePropagatesError, "example.com/failureedit.Load")
	incremental, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	clean, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental.ByKind, clean.ByKind) || !reflect.DeepEqual(incremental.ByEdge, clean.ByEdge) || incremental.External != clean.External {
		t.Fatalf("incremental failure flow differs from clean rebuild:\n%#v\n%#v", incremental, clean)
	}
}

func TestServiceSurfacesTrackedSymlinksWithoutFollowingThem(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 1 }\n")
	if err := os.Symlink("main.go", filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "main.go", "alias.go")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "fixture")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	report, err := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New())).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 1 || report.Updated[0] != "main.go" || len(report.Skipped) != 1 || report.Skipped[0] != "alias.go" {
		t.Fatalf("unexpected symlink report: %#v", report)
	}
	if report.Counts.Files != 1 {
		t.Fatalf("symlink was followed into the graph: %#v", report.Counts)
	}
}

func TestServiceIndexesAndLinksGodotProjectSources(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "project.godot"), "config_version=5\n\n[application]\nrun/main_scene=\"res://scenes/main.tscn\"\n")
	write(t, filepath.Join(root, "settings.gd"), "class_name Settings\nfunc main_scene():\n\treturn ProjectSettings.get_setting(\"application/run/main_scene\")\nfunc bind_button():\n\treturn %StartButton\nfunc lookup_button():\n\treturn get_node(\"Main/StartButton\")\n")
	if err := os.MkdirAll(filepath.Join(root, "scenes"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "scenes", "main.tscn"), "[gd_scene format=3]\n\n[node name=\"Main\" type=\"Node\"]\n\n[node name=\"StartButton\" type=\"Button\" parent=\".\" unique_name_in_owner=true]\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 3 || len(report.Diagnostics) != 0 {
		t.Fatalf("unexpected Godot index report: %#v", report)
	}

	queries := query.NewService(repository)
	setting, err := queries.Neighborhood(ctx, "config:project.godot:application/run/main_scene", "", 1,
		query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, setting, "scenes/main")
	reader, err := queries.Neighborhood(ctx, "Settings.main_scene", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, reader, "config:project.godot:application/run/main_scene")
	lookup, err := queries.Neighborhood(ctx, "Settings.bind_button", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, lookup, "scenes/main:Main/StartButton")
	lookup, err = queries.Neighborhood(ctx, "Settings.lookup_button", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, lookup, "scenes/main:Main/StartButton")
}

func TestServiceReindexesSQLWhenDialectConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "schema.sql"), "CREATE TABLE events (id bigint PRIMARY KEY);\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	registry := parserapi.NewRegistry(sqlparser.New(postgresparser.New()), configparser.New())
	service := indexer.NewService(repository, registry)

	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Updated) != 1 || first.Updated[0] != "schema.sql" {
		t.Fatalf("unexpected initial report: %#v", first)
	}

	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  default_dialect: postgres\n")
	second, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 2 || second.Updated[0] != "grafo.yaml" || second.Updated[1] != "schema.sql" {
		t.Fatalf("expected config and unchanged SQL to be reindexed: %#v", second)
	}

	third, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Updated) != 0 || third.Unchanged != 2 {
		t.Fatalf("expected incremental no-op: %#v", third)
	}

	write(t, filepath.Join(root, "grafo.yaml"), "components:\n  - name: api\n    roots: [app]\nsql:\n  default_dialect: postgres\n")
	componentOnly, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(componentOnly.Updated) != 1 || componentOnly.Updated[0] != "grafo.yaml" || componentOnly.Unchanged != 1 {
		t.Fatalf("component-only config edit reparsed unchanged SQL: %#v", componentOnly)
	}
}

func TestServiceReconcilesProtobufBindingSchemaAndConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "buf.gen.yaml"), `version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: example.com/sample/gen
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-gdscript
    out: gd
`)
	write(t, filepath.Join(root, "schema.proto"), "syntax = \"proto3\"; message Before { string value = 1; }\n")
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "main.go"), `package sample
import generated "example.com/sample/gen"
func Keep(value *generated.Before) string { return value.GetValue() }
`)
	if err := os.MkdirAll(filepath.Join(root, "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "gen", "schema.pb.go"), "// Code generated by protoc-gen-go. DO NOT EDIT.\n// source: schema.proto\npackage generated\ntype Before struct{ Value string }\nfunc (*Before) Reset() {}\nfunc (value *Before) GetValue() string { return value.Value }\n")
	write(t, filepath.Join(root, "project.godot"), "config_version=5\n")
	write(t, filepath.Join(root, "client.gd"), "extends Node\nfunc keep(value: SchemaBefore) -> void:\n\tvalue.set_value(\"kept\")\n")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	bindings := protobufbinding.NewLoader()
	registry := parserapi.NewRegistry(protobufbinding.New(bindings), gdscriptparser.NewWithBindingLoader(bindings), godotparser.New(), golangparser.NewWithBindingLoader(bindings), protobufparser.New())
	service := indexer.NewService(repository, registry)
	if _, err = service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertBindingCanonical(t, ctx, repository, "Before", true)
	assertProtocolTarget(t, ctx, repository, "example.com/sample.Keep", "Before.value", true)
	assertProtocolRelation(t, ctx, repository, "client.keep", "Before.value", graph.EdgeWrites, true)

	write(t, filepath.Join(root, "schema.proto"), "syntax = \"proto3\"; message After { string value = 1; }\n")
	changed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"buf.gen.yaml", "client.gd", "main.go", "schema.proto"} {
		if !slices.Contains(changed.Updated, path) {
			t.Fatalf("schema change did not invalidate %s: %#v", path, changed.Updated)
		}
	}
	assertBindingCanonical(t, ctx, repository, "Before", false)
	assertBindingCanonical(t, ctx, repository, "After", true)
	assertProtocolTarget(t, ctx, repository, "example.com/sample.Keep", "Before.value", false)
	assertProtocolRelation(t, ctx, repository, "client.keep", "Before.value", graph.EdgeWrites, false)
	stable, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stable.Updated) != 0 {
		t.Fatalf("unchanged bindings were not idempotent: %#v", stable.Updated)
	}
	if err := os.Remove(filepath.Join(root, "gen", "schema.pb.go")); err != nil {
		t.Fatal(err)
	}
	removed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(removed.Removed, "gen/schema.pb.go") {
		t.Fatalf("tracked binding removal was not reconciled: %#v", removed)
	}
	assertBindingCanonical(t, ctx, repository, "After", true)

	write(t, filepath.Join(root, "buf.gen.yaml"), strings.ReplaceAll(`version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: example.com/sample/gen
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.11
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-gdscript
    out: gd
`, "example.com/sample/gen", "example.com/reconfigured"))
	if _, err = service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertBindingQualifiedContains(t, ctx, repository, "After", "example.com/reconfigured.After")

	if err := os.Remove(filepath.Join(root, "buf.gen.yaml")); err != nil {
		t.Fatal(err)
	}
	removed, err = service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(removed.Removed, "buf.gen.yaml") {
		t.Fatalf("binding configuration removal was not reconciled: %#v", removed)
	}
	assertBindingCanonical(t, ctx, repository, "After", false)
}

func assertProtocolTarget(t *testing.T, ctx context.Context, repository graph.TopologyRepository, from, target string, want bool) {
	assertProtocolRelation(t, ctx, repository, from, target, graph.EdgeReads, want)
}

func assertProtocolRelation(t *testing.T, ctx context.Context, repository graph.TopologyRepository, from, target string, relation graph.EdgeKind, want bool) {
	t.Helper()
	result, err := query.NewService(repository).Neighborhood(ctx, from, "", 1, query.Outgoing, []graph.EdgeKind{relation}, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, reached := range result.Nodes {
		found = found || reached.Node.Language == "protobuf" && reached.Node.QualifiedName == target
	}
	if found != want {
		t.Fatalf("protocol target %q reached=%v, want %v: %#v", target, found, want, result)
	}
}

func assertBindingCanonical(t *testing.T, ctx context.Context, repository graph.CatalogRepository, name string, want bool) {
	t.Helper()
	nodes, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindType, graph.KindClass}, Name: name, Visibility: graph.LocalNodes})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, node := range nodes {
		if node.Node.Properties["canonical"] == name {
			found = true
		}
	}
	if found != want {
		t.Fatalf("canonical binding %q present=%v, want %v; nodes=%#v", name, found, want, nodes)
	}
}

func assertBindingQualifiedContains(t *testing.T, ctx context.Context, repository graph.CatalogRepository, name, qualified string) {
	t.Helper()
	nodes, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindType}, Name: name, Visibility: graph.LocalNodes})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.Node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("missing reconfigured binding %q in %#v", qualified, nodes)
}

func TestServiceTracksGitDirtyPathsAcrossRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 1 }\n")
	runGit(t, root, "add", "main.go")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 1 || report.Checked != 1 {
		t.Fatalf("initial index: report=%#v err=%v", report, err)
	}

	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 2 }\n")
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 1 || report.Checked != 1 {
		t.Fatalf("dirty index: report=%#v err=%v", report, err)
	}
	runGit(t, root, "checkout", "--", "main.go")
	restored, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Updated) != 1 || restored.Updated[0] != "main.go" || restored.Checked != 1 {
		t.Fatalf("restored tracked file was not reindexed: %#v", restored)
	}
	stable, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stable.Updated) != 0 || stable.Unchanged != 1 || stable.Checked != 0 {
		t.Fatalf("clean worktree was not an incremental no-op: %#v", stable)
	}
}

func TestServicePropagatesGitConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "schema.sql"), "CREATE TABLE events (id bigint PRIMARY KEY);\n")
	runGit(t, root, "add", "schema.sql")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(sqlparser.New(postgresparser.New()), configparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  default_dialect: postgres\n")
	reconfigured, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reconfigured.Updated) != 2 || reconfigured.Updated[0] != "grafo.yaml" || reconfigured.Updated[1] != "schema.sql" || reconfigured.Checked != 2 {
		t.Fatalf("semantic dependency did not propagate through Git change selection: %#v", reconfigured)
	}
}

func TestServiceResumesSemanticRebuildWithoutReplacingCompletedFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "main.go"), "package sample\nfunc Value() int { return 1 }\n")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if report, err := service.Run(ctx, project, indexer.Options{}); err != nil || len(report.Updated) != 1 {
		t.Fatalf("initial index: report=%#v err=%v", report, err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", "interrupted-version"); err != nil {
		t.Fatal(err)
	}

	resumed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Rebuild == "" || len(resumed.Updated) != 0 || resumed.Unchanged != 1 || resumed.Checked != 1 {
		t.Fatalf("semantic rebuild did not resume from per-file hashes: %#v", resumed)
	}
}

func TestServiceIndexesExplicitComponentsWithExactEligibleMembership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, directory := range []string{"client", "client-old", "web/ui", "apps/server", "node_modules/client"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "grafo.yaml"), `components:
  - name: client
    roots: [client, web/ui]
  - name: server
    roots: [apps/server]
  - name: empty
    roots: [unused]
`)
	write(t, filepath.Join(root, "client", "main.py"), "def client():\n    return True\n")
	write(t, filepath.Join(root, "web", "ui", "view.py"), "def view():\n    return True\n")
	write(t, filepath.Join(root, "apps", "server", "main.py"), "def server():\n    return True\n")
	write(t, filepath.Join(root, "client-old", "other.py"), "def other():\n    return True\n")
	write(t, filepath.Join(root, "node_modules", "client", "ignored.py"), "def ignored():\n    return True\n")
	write(t, filepath.Join(root, "client", "oversized.py"), strings.Repeat("#", 256))
	write(t, filepath.Join(root, "client", "unsupported.txt"), "not indexed")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(pythonparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{MaxFileSize: 128})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.ByKind[string(graph.KindComponent)] != 3 || report.Counts.Files != 4 {
		t.Fatalf("unexpected component/file counts: %#v", report.Counts)
	}
	if len(report.Skipped) != 1 || report.Skipped[0] != "client/oversized.py" {
		t.Fatalf("oversized membership candidate was not skipped: %#v", report)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Level != "warning" || !strings.Contains(report.Diagnostics[0].Message, `component "empty" matches no indexed files`) {
		t.Fatalf("empty component warning = %#v", report.Diagnostics)
	}

	client := componentNode(t, ctx, repository, project.Name+"/client")
	if client.ID != graph.NodeID(graph.KindComponent, project.ID+":client") || client.Location.Path != "grafo.yaml" || client.Location.Line != 2 {
		t.Fatalf("client identity/provenance = %#v", client)
	}
	assertOutgoingQualifiedSet(t, ctx, repository, client.ID, graph.EdgeContains, []string{"client/main.py", "web/ui/view.py"})
	server := componentNode(t, ctx, repository, project.Name+"/server")
	assertOutgoingQualifiedSet(t, ctx, repository, server.ID, graph.EdgeContains, []string{"apps/server/main.py"})
	empty := componentNode(t, ctx, repository, project.Name+"/empty")
	assertOutgoingQualifiedSet(t, ctx, repository, empty.ID, graph.EdgeContains, nil)

	repositoryNode, err := repository.Node(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repositoryNode.Kind != graph.KindRepository {
		t.Fatalf("repository node = %#v", repositoryNode)
	}
	assertOutgoingQualifiedSet(t, ctx, repository, project.ID, graph.EdgeContains, []string{
		"apps/server/main.py", "client-old/other.py", "client/main.py", project.Name + "/client", project.Name + "/empty", project.Name + "/server", "web/ui/view.py",
	})
}

func TestServiceReconcilesComponentConfigurationWithoutReparsingSources(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "app", "main.py"), "def main():\n    return True\n")
	write(t, filepath.Join(root, "grafo.yaml"), "components:\n  - name: api\n    roots: [app]\n")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(pythonparser.New()))
	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil || len(first.Updated) != 1 {
		t.Fatalf("initial index: report=%#v err=%v", first, err)
	}
	firstComponent := componentNode(t, ctx, repository, project.Name+"/api")

	write(t, filepath.Join(root, "grafo.yaml"), "components:\n  - name: worker\n    roots: [app]\n")
	renamed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed.Updated) != 0 || renamed.Unchanged != 1 {
		t.Fatalf("component-only edit reparsed source: %#v", renamed)
	}
	if _, err := repository.Node(ctx, firstComponent.ID); err == nil {
		t.Fatal("renamed component survived workspace replacement")
	}
	worker := componentNode(t, ctx, repository, project.Name+"/worker")
	assertOutgoingQualifiedSet(t, ctx, repository, worker.ID, graph.EdgeContains, []string{"app/main.py"})

	if err := os.Remove(filepath.Join(root, "grafo.yaml")); err != nil {
		t.Fatal(err)
	}
	removed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed.Updated) != 0 || removed.Unchanged != 1 || removed.Counts.ByKind[string(graph.KindComponent)] != 0 {
		t.Fatalf("config deletion did not reconcile without reparsing: %#v", removed)
	}
	if _, err := repository.Node(ctx, worker.ID); err == nil {
		t.Fatal("removed component survived workspace replacement")
	}
}

func TestServiceRejectsInvalidComponentEditBeforeMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "app", "main.py"), "def main():\n    return True\n")
	write(t, filepath.Join(root, "grafo.yaml"), "components:\n  - name: api\n    roots: [app]\n")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(pythonparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	api := componentNode(t, ctx, repository, project.Name+"/api")
	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "grafo.yaml"), "components:\n  - name: broken\n    roots: [app, app/nested]\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("invalid edit error = %v", err)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid edit mutated index:\nbefore=%#v\nafter=%#v", before, after)
	}
	persisted, err := repository.Node(ctx, api.ID)
	if err != nil || persisted.QualifiedName != project.Name+"/api" {
		t.Fatalf("last valid component was not preserved: node=%#v err=%v", persisted, err)
	}
}

func TestServiceReconcilesGDScriptHTTPRequestConfigurationAndRejectsInvalidEditBeforeMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "project.godot"), "config_version=5\n")
	write(t, filepath.Join(root, "client.gd"), `extends Node
func send(api: AuthAPI) -> void:
	api.first(HTTPClient.METHOD_GET, "/first")
	api.second(HTTPClient.METHOD_POST, "/second")
`)
	write(t, filepath.Join(root, "grafo.yaml"), `http:
  request_apis:
    - language: gdscript
      symbol: AuthAPI.first
      method_argument: 0
      url_argument: 1
`)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New(), configparser.New()))
	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertHTTPRequestSet(t, ctx, repository, []string{"GET /first"})

	write(t, filepath.Join(root, "grafo.yaml"), `http:
  request_apis:
    - language: gdscript
      symbol: AuthAPI.second
      method_argument: 0
      route_argument: 1
`)
	changed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(changed.Updated, "client.gd") || !slices.Contains(changed.Updated, "grafo.yaml") {
		t.Fatalf("HTTP config edit did not invalidate GDScript: first=%#v changed=%#v", first.Updated, changed.Updated)
	}
	assertHTTPRequestSet(t, ctx, repository, []string{"POST /second"})

	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "grafo.yaml"), `http:
  request_apis:
    - language: gdscript
      symbol: AuthAPI.second
      method_argument: -1
      route_argument: 1
`)
	if _, err := service.Run(ctx, project, indexer.Options{}); err == nil || !strings.Contains(err.Error(), "method_argument") {
		t.Fatalf("invalid HTTP config error = %v", err)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid HTTP config mutated index:\nbefore=%#v\nafter=%#v", before, after)
	}
	assertHTTPRequestSet(t, ctx, repository, []string{"POST /second"})

	if err := os.Remove(filepath.Join(root, "grafo.yaml")); err != nil {
		t.Fatal(err)
	}
	removed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(removed.Updated, "client.gd") || !slices.Contains(removed.Removed, "grafo.yaml") {
		t.Fatalf("HTTP config removal did not invalidate GDScript: %#v", removed)
	}
	assertHTTPRequestSet(t, ctx, repository, nil)
}

func TestServiceReconcilesConfiguredGDScriptEventEffectsAndPreservesInvalidEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "project.godot"), "config_version=5\n")
	write(t, filepath.Join(root, "source.gd"), `class_name Source extends Node
signal tapped(value: int)
signal candidate_selected(value: int)
signal armed_changed(value: int)
func publish(value: int) -> void:
	tapped.emit(value)
	candidate_selected.emit(value)
	armed_changed.emit(value)
`)
	write(t, filepath.Join(root, "consumer.gd"), `class_name Consumer extends Node
func wire(source: Source) -> void:
	Signals.wire(source.tapped, _on_changed)
	Signals.wire(source.candidate_selected, _on_changed)
	Signals.wire(source.armed_changed, _on_changed)
func _on_changed(_value: int) -> void:
	pass
`)
	configured := `adapters:
  - match: {language: gdscript, symbol: Signals.wire}
    effects:
      - kind: event.subscribe
        roles: {event: {argument: 0}, handler: {argument: 1}}
`
	write(t, filepath.Join(root, "grafo.yaml"), configured)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New(), configparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"Source.tapped", "Source.candidate_selected", "Source.armed_changed"} {
		assertEventConsumption(t, ctx, repository, event, true)
	}

	write(t, filepath.Join(root, "grafo.yaml"), strings.Replace(configured, "event.subscribe", "event.unsubscribe", 1))
	changed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(changed.Updated, "consumer.gd") || !slices.Contains(changed.Updated, "grafo.yaml") {
		t.Fatalf("adapter edit did not invalidate GDScript: %#v", changed)
	}
	for _, event := range []string{"Source.tapped", "Source.candidate_selected", "Source.armed_changed"} {
		assertEventConsumption(t, ctx, repository, event, false)
	}

	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "grafo.yaml"), strings.Replace(configured, "argument: 1", "argument: -1", 1))
	if _, err := service.Run(ctx, project, indexer.Options{}); err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("invalid adapter config error = %v", err)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid adapter edit mutated index:\nbefore=%#v\nafter=%#v", before, after)
	}

	write(t, filepath.Join(root, "grafo.yaml"), configured)
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"Source.tapped", "Source.candidate_selected", "Source.armed_changed"} {
		assertEventConsumption(t, ctx, repository, event, true)
	}
	if err := os.Remove(filepath.Join(root, "grafo.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"Source.tapped", "Source.candidate_selected", "Source.armed_changed"} {
		assertEventConsumption(t, ctx, repository, event, false)
	}
}

func assertEventConsumption(t *testing.T, ctx context.Context, repository graph.CatalogRepository, qualified string, consumed bool) {
	t.Helper()
	events, err := query.NewCatalog(repository).Events(ctx, query.CatalogOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Events {
		if event.QualifiedName != qualified {
			continue
		}
		got := len(event.Consumers) > 0 && len(event.Handlers) > 0
		if got != consumed {
			t.Fatalf("event %s consumed=%v, want %v: %#v", qualified, got, consumed, event)
		}
		orphans, err := query.NewCatalog(repository).OrphanedEvents(ctx, query.CatalogOptions{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, orphan := range orphans.Events {
			if orphan.Event.QualifiedName == qualified && consumed {
				t.Fatalf("configured consumer left false orphan: %#v", orphan)
			}
		}
		return
	}
	t.Fatalf("event %s not found: %#v", qualified, events)
}

func TestServiceReconcilesFirstClassTestsAcrossConfigInheritanceAndRenameEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module example.com/sample\n\ngo 1.26\n")
	write(t, filepath.Join(root, "run.go"), "package sample\nfunc Run() {}\n")
	write(t, filepath.Join(root, "run_test.go"), "package sample\nimport \"testing\"\nfunc TestGo(t *testing.T) { Run() }\n")
	write(t, filepath.Join(root, "target.gd"), "class_name Target\nextends Node\nfunc run() -> void:\n\tpass\n")
	write(t, filepath.Join(root, "spec.gd"), "class_name Spec\nextends SpecBase\nfunc test_run() -> void:\n\trun()\n")
	write(t, filepath.Join(root, "grafo.yaml"), "tests:\n  gdscript_bases: [SpecBase]\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New(), gdscriptparser.New(), configparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertIndexedTestNames(t, ctx, repository, []string{"TestGo", "test_run"})

	write(t, filepath.Join(root, "grafo.yaml"), "tests:\n  gdscript_bases: [OtherBase]\n")
	changed, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(changed.Updated, "grafo.yaml") || !slices.Contains(changed.Updated, "spec.gd") {
		t.Fatalf("test-base config did not invalidate GDScript: %#v", changed)
	}
	assertIndexedTestNames(t, ctx, repository, []string{"TestGo"})

	write(t, filepath.Join(root, "grafo.yaml"), "tests:\n  gdscript_bases: [42]\n")
	invalid, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatalf("invalid test config must keep indexing with built-ins only: %v", err)
	}
	foundWarning := false
	for _, diagnostic := range invalid.Diagnostics {
		foundWarning = foundWarning || diagnostic.Path == "spec.gd" && diagnostic.Level == "warning" && strings.Contains(diagnostic.Message, "gdscript_bases")
	}
	if !foundWarning {
		t.Fatalf("invalid test config warning missing: %#v", invalid.Diagnostics)
	}
	assertIndexedTestNames(t, ctx, repository, []string{"TestGo"})

	write(t, filepath.Join(root, "spec.gd"), "class_name Spec\nextends GutTest\nfunc test_changed() -> void:\n\trun()\n")
	write(t, filepath.Join(root, "run_test.go"), "package sample\nimport \"testing\"\nfunc TestRenamed(t *testing.T) { Run() }\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertIndexedTestNames(t, ctx, repository, []string{"TestRenamed", "test_changed"})

	write(t, filepath.Join(root, "run_test.go"), "package sample_test\nimport (\"testing\"; \"example.com/sample\")\nfunc TestRenamed(t *testing.T) { sample.Run() }\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertIndexedTestNames(t, ctx, repository, []string{"TestRenamed", "test_changed"})
	tests, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindTest}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	foundExternal := false
	for _, item := range tests {
		foundExternal = foundExternal || item.Node.Name == "TestRenamed" && item.Node.Properties["test_package"] == "external"
	}
	if !foundExternal {
		t.Fatalf("external test package identity was not reconciled: %#v", tests)
	}
	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("clean rebuild did not converge:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func assertIndexedTestNames(t *testing.T, ctx context.Context, repository *sqlite.Repository, want []string) {
	t.Helper()
	items, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindTest}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.Node.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("indexed tests = %v, want %v", got, want)
	}
}

func assertHTTPRequestSet(t *testing.T, ctx context.Context, repository graph.TopologyRepository, want []string) {
	t.Helper()
	result, err := query.NewTopology(repository).OutboundRequests(ctx, query.TopologyOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(result.Requests))
	for _, request := range result.Requests {
		got = append(got, request.Method+" "+request.Route)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("HTTP requests = %v, want %v; result=%#v", got, want, result)
	}
}

func TestServiceIndexesServeMuxRoutesForAuthorityAwareTopology(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/uzir\n\ngo 1.26\n")
	write(t, filepath.Join(root, "routes.go"), `package uzir
import "net/http"
func handler(http.ResponseWriter, *http.Request) {}
func Routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agents", handler)
	mux.HandleFunc("GET /v1/status", handler)
	mux.HandleFunc("POST /v1/observe", handler)
	mux.HandleFunc("POST /v1/map", handler)
	mux.HandleFunc("GET /v1/events", handler)
	mux.HandleFunc("POST /v1/actions", handler)
	mux.HandleFunc("POST /v1/shutdown", handler)
	mux.HandleFunc("GET cortex.example/v1/status", handler)
}
`)
	write(t, filepath.Join(root, "client.go"), `package uzir
import "net/http"
func Call() {
	http.Get("/api/agents")
	http.Get("/v1/status")
	http.Post("/v1/observe", "application/json", nil)
	http.Post("/v1/map", "application/json", nil)
	http.Get("/v1/events")
	http.Post("/v1/actions", "application/json", nil)
	http.Post("/v1/shutdown", "application/json", nil)
}
`)
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(golangparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	topology := query.NewTopology(repository)
	endpoints, err := topology.Endpoints(ctx, query.TopologyOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"GET /api/agents": true, "GET /v1/status": true, "POST /v1/observe": true,
		"POST /v1/map": true, "GET /v1/events": true, "POST /v1/actions": true,
		"POST /v1/shutdown": true, "GET //cortex.example/v1/status": true,
	}
	for _, endpoint := range endpoints.Endpoints {
		delete(want, endpoint.Name)
		if endpoint.Name == "GET //cortex.example/v1/status" && endpoint.Authority != "cortex.example" {
			t.Fatalf("host-qualified endpoint lost authority: %#v", endpoint)
		}
		if endpoint.HandlerStatus != query.BoundaryResolved || len(endpoint.Handlers) != 1 ||
			endpoint.Handlers[0].Node.QualifiedName != "example.com/uzir.handler" {
			t.Fatalf("ServeMux handler evidence = %#v", endpoint)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing Uzir-shaped endpoints: %#v; catalog=%#v", want, endpoints)
	}
	requests, err := topology.OutboundRequests(ctx, query.TopologyOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests.Requests) != 7 {
		t.Fatalf("outbound requests = %#v", requests)
	}
	for _, request := range requests.Requests {
		if request.Status != query.BoundaryResolved || request.Destination.Authority != "" {
			t.Fatalf("unqualified request crossed a host-qualified boundary: %#v", request)
		}
	}

	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("ServeMux clean rebuild differs: before=%#v after=%#v", before, after)
	}
}

func TestServiceDoesNotPersistMembershipForFileLostAfterDiscovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "grafo.yaml"), "components:\n  - name: app\n    roots: [app]\n")
	write(t, filepath.Join(root, "app", "a.race"), "a")
	write(t, filepath.Join(root, "app", "b.race"), "b")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	report, err := indexer.NewService(repository, parserapi.NewRegistry(deletingParser{})).Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 1 || report.Updated[0] != "app/a.race" || report.Counts.Files != 1 {
		t.Fatalf("lost file remained indexed: %#v", report)
	}
	component := componentNode(t, ctx, repository, project.Name+"/app")
	assertOutgoingQualifiedSet(t, ctx, repository, component.ID, graph.EdgeContains, []string{"app/a.race"})
	if report.Counts.External != 0 {
		t.Fatalf("lost file produced an external placeholder: %#v", report.Counts)
	}
}

func componentNode(t *testing.T, ctx context.Context, repository *sqlite.Repository, qualified string) graph.Node {
	t.Helper()
	nodes, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindComponent}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, scoped := range nodes {
		if scoped.Node.QualifiedName == qualified {
			return scoped.Node
		}
	}
	t.Fatalf("component %q not found in %#v", qualified, nodes)
	return graph.Node{}
}

func assertOutgoingQualifiedSet(t *testing.T, ctx context.Context, repository graph.Repository, fromID string, kind graph.EdgeKind, want []string) {
	t.Helper()
	edges, err := repository.EdgesFrom(ctx, fromID)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, edge := range edges {
		if edge.Kind != kind {
			continue
		}
		target, err := repository.Node(ctx, edge.ToID)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, target.QualifiedName)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("outgoing %s from %s = %v, want %v", kind, fromID, got, want)
	}
}

func write(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertReached(t *testing.T, traversal query.Traversal, qualified string) {
	t.Helper()
	for _, reached := range traversal.Nodes {
		if reached.Node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("did not reach %q: %#v", qualified, traversal)
}

func assertOutgoingTarget(t *testing.T, ctx context.Context, repository graph.Repository, from string, kind graph.EdgeKind, target string) {
	t.Helper()
	service := query.NewService(repository)
	node, err := service.Resolve(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind != kind {
			continue
		}
		to, err := repository.Node(ctx, edge.ToID)
		if err != nil {
			t.Fatal(err)
		}
		if to.QualifiedName == target {
			return
		}
	}
	t.Fatalf("missing %s edge from %s to %s; got %#v", kind, from, target, edges)
}

func assertNoOutgoingTarget(t *testing.T, ctx context.Context, repository graph.Repository, from string, kind graph.EdgeKind, target string) {
	t.Helper()
	service := query.NewService(repository)
	node, err := service.Resolve(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesFrom(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind != kind {
			continue
		}
		to, err := repository.Node(ctx, edge.ToID)
		if err != nil {
			t.Fatal(err)
		}
		if to.QualifiedName == target {
			t.Fatalf("unexpected %s edge from %s to %s", kind, from, target)
		}
	}
}

// TestServiceModelsGodotCompositionAcrossFiles indexes a small Godot project and
// asserts the composition vocabulary resolves across files, that re-indexing an
// unchanged project is a no-op, and that editing an autoload target reconciles
// to exactly the clean-rebuild result.
func TestServiceModelsGodotCompositionAcrossFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeGodotProject(t, root, "res://scripts/game.gd")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", report.Diagnostics)
	}
	queries := query.NewService(repository)

	scene, err := queries.GodotComposition(ctx, "scenes/main", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, scene.OutboundInstances, "scenes/enemy", graph.KindGodotScene)
	assertComposition(t, scene.AttachedScripts, "scripts/player", graph.KindModule)

	autoload, err := queries.GodotComposition(ctx, "godot:autoload:project.godot:Game", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, autoload.AutoloadTargets, "scripts/game", graph.KindModule)
	use, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, use, "godot:autoload:project.godot:Game")

	before, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repeat.Updated) != 0 || len(repeat.Removed) != 0 {
		t.Fatalf("re-indexing an unchanged Godot project changed the graph: %#v", repeat)
	}
	after, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("counts changed on an unchanged re-index:\n%#v\n%#v", before, after)
	}

	// Toggling only the singleton marker must reparse dependent scripts: a
	// disabled autoload is not a global identifier, so the script-side edge has
	// to disappear even though the script itself did not change.
	writeGodotProjectWithMarker(t, root, "res://scripts/game.gd", false)
	disabled, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(disabled.Updated, "scripts/player.gd") {
		t.Fatalf("disabling an autoload did not reparse its dependent script: %#v", disabled.Updated)
	}
	stale, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, reached := range stale.Nodes {
		if reached.Node.Kind == graph.KindGodotAutoload {
			t.Fatalf("a disabled autoload kept its script-side edge: %#v", reached.Node)
		}
	}
	writeGodotProject(t, root, "res://scripts/game.gd")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	writeGodotProject(t, root, "res://scripts/other.gd")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	moved, err := queries.GodotComposition(ctx, "godot:autoload:project.godot:Game", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, moved.AutoloadTargets, "scripts/other", graph.KindModule)
	if len(moved.AutoloadTargets) != 1 {
		t.Fatalf("autoload target was not reconciled: %#v", moved.AutoloadTargets)
	}
	incremental, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rebuiltRoot := t.TempDir()
	writeGodotProject(t, rebuiltRoot, "res://scripts/other.gd")
	rebuiltProject, err := indexer.DiscoverProject(ctx, rebuiltRoot)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := sqlite.Open(ctx, rebuiltProject.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuilt.Close() }()
	if _, err := indexer.NewService(rebuilt, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New())).
		Run(ctx, rebuiltProject, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	clean, err := rebuilt.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental.ByKind, clean.ByKind) || !reflect.DeepEqual(incremental.ByEdge, clean.ByEdge) {
		t.Fatalf("incremental edit differs from a clean rebuild:\n%#v\n%#v", incremental, clean)
	}
}

func writeGodotProject(t *testing.T, root, autoloadTarget string) {
	t.Helper()
	writeGodotProjectWithMarker(t, root, autoloadTarget, true)
}

func writeGodotProjectWithMarker(t *testing.T, root, autoloadTarget string, enabled bool) {
	t.Helper()
	marker := "*"
	if !enabled {
		marker = ""
	}
	for _, directory := range []string{"scenes", "scripts"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "project.godot"),
		"config_version=5\n\n[autoload]\nGame=\""+marker+autoloadTarget+"\"\n")
	write(t, filepath.Join(root, "scripts", "game.gd"), "class_name Game extends Node\nfunc start() -> void:\n\tpass\n")
	write(t, filepath.Join(root, "scripts", "other.gd"), "extends Node\nfunc start() -> void:\n\tpass\n")
	write(t, filepath.Join(root, "scripts", "player.gd"), "extends Node\nfunc ready() -> void:\n\tGame.start()\n")
	write(t, filepath.Join(root, "scenes", "enemy.tscn"),
		"[gd_scene format=3 uid=\"uid://enemy123\"]\n\n[node name=\"Enemy\" type=\"Node2D\"]\n")
	write(t, filepath.Join(root, "scenes", "main.tscn"),
		"[gd_scene load_steps=3 format=3 uid=\"uid://main123\"]\n\n"+
			"[ext_resource type=\"Script\" path=\"res://scripts/player.gd\" id=\"1_player\"]\n"+
			"[ext_resource type=\"PackedScene\" uid=\"uid://enemy123\" path=\"res://scenes/enemy.tscn\" id=\"2_enemy\"]\n\n"+
			"[node name=\"Main\" type=\"Node\"]\nscript = ExtResource(\"1_player\")\n\n"+
			"[node name=\"Enemy\" parent=\".\" instance=ExtResource(\"2_enemy\")]\n")
}

func assertComposition(t *testing.T, relations []query.GodotRelation, qualified string, kind graph.NodeKind) {
	t.Helper()
	for _, relation := range relations {
		if relation.Node.QualifiedName != qualified {
			continue
		}
		if relation.Node.External {
			t.Fatalf("%q resolved to an external node: %#v", qualified, relation.Node)
		}
		if relation.Node.Kind != kind {
			t.Fatalf("%q resolved to kind %q, want %q", qualified, relation.Node.Kind, kind)
		}
		return
	}
	t.Fatalf("missing composition relation to %q: %#v", qualified, relations)
}

// TestServiceRebuildReplacesLegacyGodotRepresentations proves the semantic
// rebuild leaves no duplicate generic representation of a migrated Godot
// concept: a stale index row that still models a scene as a module and a scene
// node as a variable is replaced, not joined, by the new vocabulary.
func TestServiceRebuildReplacesLegacyGodotRepresentations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeGodotProject(t, root, "res://scripts/game.gd")
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	legacy := graph.ParseResult{Nodes: []graph.Node{
		{ID: graph.NodeID(graph.KindModule, "scenes/main", "legacy"), Kind: graph.KindModule,
			Name: "main", QualifiedName: "scenes/main", OwnerFile: "scenes/main.tscn"},
		{ID: graph.NodeID(graph.KindVariable, "scenes/main:Main", "legacy"), Kind: graph.KindVariable,
			Name: "Main", QualifiedName: "scenes/main:Main", OwnerFile: "scenes/main.tscn"},
	}}
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "scenes/main.tscn",
		Hash: "legacy", Language: "godot"}, legacy); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetMeta(ctx, "semantic_index_version", "legacy"); err != nil {
		t.Fatal(err)
	}
	rebuild, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuild.Rebuild == "" {
		t.Fatalf("stale semantic version did not trigger a rebuild: %#v", rebuild)
	}
	assertNodeKinds(t, repository, "scenes/main", map[graph.NodeKind]int{graph.KindGodotScene: 1})
	assertNodeKinds(t, repository, "scenes/main:Main", map[graph.NodeKind]int{graph.KindGodotSceneNode: 1})
}

func assertNodeKinds(t *testing.T, repository graph.ReadRepository, qualified string, want map[graph.NodeKind]int) {
	t.Helper()
	found, err := repository.SearchNodes(context.Background(), qualified, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[graph.NodeKind]int{}
	for _, node := range found {
		if node.QualifiedName != qualified || node.External {
			continue
		}
		counts[node.Kind]++
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("node kinds for %q = %#v, want %#v", qualified, counts, want)
	}
}

// TestServiceScopesGodotProjectsNestedInOneRepository covers a monorepo whose
// Godot projects live in subdirectories. res:// references are project-relative,
// so each project's relationships must resolve inside its own directory, and two
// sibling projects that both declare an autoload named Game must stay
// independently addressable instead of colliding.
func TestServiceScopesGodotProjectsNestedInOneRepository(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, project := range []string{"client", "tools/probe"} {
		write(t, mkdirFor(t, root, project+"/project.godot"),
			"config_version=5\n\n[autoload]\nGame=\"*res://scripts/game.gd\"\n")
		write(t, mkdirFor(t, root, project+"/scripts/game.gd"),
			"extends Node\nfunc start() -> void:\n\tpass\n")
		write(t, mkdirFor(t, root, project+"/scripts/hud.gd"),
			"extends Node\nfunc ready() -> void:\n\tGame.start()\n")
		write(t, mkdirFor(t, root, project+"/scenes/enemy.tscn"),
			"[gd_scene format=3 uid=\"uid://"+strings.ReplaceAll(project, "/", "_")+"_enemy\"]\n\n[node name=\"Enemy\" type=\"Node\"]\n")
		write(t, mkdirFor(t, root, project+"/scenes/main.tscn"),
			"[gd_scene load_steps=3 format=3]\n\n"+
				"[ext_resource type=\"Script\" path=\"res://scripts/hud.gd\" id=\"1_hud\"]\n"+
				"[ext_resource type=\"PackedScene\" path=\"res://scenes/enemy.tscn\" id=\"2_enemy\"]\n\n"+
				"[node name=\"Main\" type=\"Node\"]\nscript = ExtResource(\"1_hud\")\n\n"+
				"[node name=\"Enemy\" parent=\".\" instance=ExtResource(\"2_enemy\")]\n")
	}

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if _, err := indexer.NewService(repository,
		parserapi.NewRegistry(gdscriptparser.New(), godotparser.New())).Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	queries := query.NewService(repository)

	for _, directory := range []string{"client", "tools/probe"} {
		scene, err := queries.GodotComposition(ctx, directory+"/scenes/main", query.GodotCompositionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertComposition(t, scene.OutboundInstances, directory+"/scenes/enemy", graph.KindGodotScene)
		assertComposition(t, scene.AttachedScripts, directory+"/scripts/hud", graph.KindModule)

		// Inbound attribution must also stay inside the project: the instancing
		// scene is found from the scene node's own project-scoped identity.
		instanced, err := queries.GodotComposition(ctx, directory+"/scenes/enemy", query.GodotCompositionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertComposition(t, instanced.InboundInstances, directory+"/scenes/main", graph.KindGodotScene)
		if instanced.InboundInstances[0].Via == nil ||
			instanced.InboundInstances[0].Via.QualifiedName != directory+"/scenes/main:Main/Enemy" {
			t.Fatalf("inbound instance lost its scene node evidence: %#v", instanced.InboundInstances[0])
		}

		autoload, err := queries.GodotComposition(ctx,
			"godot:autoload:"+directory+"/project.godot:Game", query.GodotCompositionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertComposition(t, autoload.AutoloadTargets, directory+"/scripts/game", graph.KindModule)
		use, err := queries.Neighborhood(ctx, directory+"/scripts/hud.ready", "", 1, query.Outgoing, nil, 20)
		if err != nil {
			t.Fatal(err)
		}
		assertReached(t, use, "godot:autoload:"+directory+"/project.godot:Game")
	}

	// Nothing may be attributed to a repository-root identity that no file owns.
	for _, qualified := range []string{"scenes/main", "scenes/enemy", "scripts/game", "godot:autoload:Game"} {
		found, err := repository.SearchNodes(ctx, qualified, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range found {
			if node.QualifiedName == qualified {
				t.Fatalf("project-relative reference leaked to the repository root: %#v", node)
			}
		}
	}
}

func mkdirFor(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestServiceKeepsContradictedGodotUIDsUnresolved proves the cross-file UID
// check runs in the real pipeline: the workspace semantic key builds the alias
// table, so a scene whose ext_resource pairs a UID with a path another resource
// declares produces a diagnostic and no composition edge, and repairing the
// path resolves it on the next incremental run.
func TestServiceKeepsContradictedGodotUIDsUnresolved(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, mkdirFor(t, root, "project.godot"), "config_version=5\n")
	write(t, mkdirFor(t, root, "scenes/a.tscn"),
		"[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"A\" type=\"Node\"]\n")
	write(t, mkdirFor(t, root, "scenes/b.tscn"),
		"[gd_scene format=3 uid=\"uid://other\"]\n\n[node name=\"B\" type=\"Node\"]\n")
	caller := func(path string) string {
		return "[gd_scene load_steps=2 format=3]\n\n" +
			"[ext_resource type=\"PackedScene\" uid=\"uid://shared\" path=\"" + path + "\" id=\"1_x\"]\n\n" +
			"[node name=\"Root\" type=\"Node\"]\n\n" +
			"[node name=\"Child\" parent=\".\" instance=ExtResource(\"1_x\")]\n"
	}
	write(t, mkdirFor(t, root, "scenes/caller.tscn"), caller("res://scenes/b.tscn"))

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	report, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	contradiction := false
	for _, diagnostic := range report.Diagnostics {
		if strings.Contains(diagnostic.Message, "uid://shared") {
			contradiction = true
		}
	}
	if !contradiction {
		t.Fatalf("contradicted UID produced no diagnostic: %#v", report.Diagnostics)
	}
	scene, err := query.NewService(repository).GodotComposition(ctx, "scenes/caller", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.OutboundInstances) != 0 {
		t.Fatalf("contradicted UID produced a composition edge: %#v", scene.OutboundInstances)
	}

	// Repairing the path to the resource that actually declares the UID makes
	// the same reference resolve, which shows the check is evidence-driven
	// rather than a blanket refusal.
	write(t, filepath.Join(root, "scenes", "caller.tscn"), caller("res://scenes/a.tscn"))
	repaired, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range repaired.Diagnostics {
		if strings.Contains(diagnostic.Message, "uid://shared") {
			t.Fatalf("repaired reference still diagnosed: %#v", repaired.Diagnostics)
		}
	}
	fixed, err := query.NewService(repository).GodotComposition(ctx, "scenes/caller", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, fixed.OutboundInstances, "scenes/a", graph.KindGodotScene)

	// Moving the UID to another resource contradicts a caller that was not
	// itself edited, so the workspace semantic key must reparse it.
	write(t, filepath.Join(root, "scenes", "a.tscn"),
		"[gd_scene format=3 uid=\"uid://moved\"]\n\n[node name=\"A\" type=\"Node\"]\n")
	write(t, filepath.Join(root, "scenes", "b.tscn"),
		"[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"B\" type=\"Node\"]\n")
	moved, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(moved.Updated, "scenes/caller.tscn") {
		t.Fatalf("a moved UID did not reparse the unchanged caller: %#v", moved.Updated)
	}
	stale, err := query.NewService(repository).GodotComposition(ctx, "scenes/caller", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale.OutboundInstances) != 0 {
		t.Fatalf("moved UID left a stale composition edge: %#v", stale.OutboundInstances)
	}
}

func TestServiceBoundsGodotAliasesToGitMembership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, ".gitignore"), ".worktrees/\nignored/\n")
	write(t, mkdirFor(t, root, "project.godot"), "config_version=5\n")
	write(t, mkdirFor(t, root, "scenes/target.tscn"),
		"[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"Target\" type=\"Node\"]\n")
	write(t, mkdirFor(t, root, "scenes/caller.tscn"),
		"[gd_scene load_steps=2 format=3]\n\n"+
			"[ext_resource type=\"PackedScene\" uid=\"uid://shared\" path=\"res://scenes/target.tscn\" id=\"1_target\"]\n\n"+
			"[node name=\"Root\" type=\"Node\"]\n\n"+
			"[node name=\"Child\" parent=\".\" instance=ExtResource(\"1_target\")]\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "fixture")

	child := filepath.Join(root, ".worktrees", "child")
	runGit(t, root, "worktree", "add", "-b", "child-boundary", child)
	t.Cleanup(func() {
		command := exec.Command("git", "-C", root, "worktree", "remove", "--force", child)
		_ = command.Run()
	})
	write(t, mkdirFor(t, root, "ignored/copy.tscn"),
		"[gd_scene format=3 uid=\"uid://shared\"]\n\n[node name=\"Ignored\" type=\"Node\"]\n")
	write(t, mkdirFor(t, root, "scenes/untracked.tscn"),
		"[gd_scene format=3 uid=\"uid://untracked\"]\n\n[node name=\"Untracked\" type=\"Node\"]\n")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(godotparser.New()))
	first, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(first.Updated, "scenes/untracked.tscn") {
		t.Fatalf("eligible untracked resource was not indexed: %#v", first.Updated)
	}
	for _, diagnostic := range first.Diagnostics {
		if strings.Contains(diagnostic.Message, "uid://shared") {
			t.Fatalf("excluded duplicate UID affected parent checkout: %#v", first.Diagnostics)
		}
	}
	composition, err := query.NewService(repository).GodotComposition(ctx, "scenes/caller", query.GodotCompositionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertComposition(t, composition.OutboundInstances, "scenes/target", graph.KindGodotScene)

	// The linked child remains a valid repository root in its own right.
	childProject, err := indexer.DiscoverProject(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if childProject.Root != child {
		t.Fatalf("child worktree root = %q, want %q", childProject.Root, child)
	}
	childRepository, err := sqlite.Open(ctx, childProject.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	childService := indexer.NewService(childRepository, parserapi.NewRegistry(godotparser.New()))
	childReport, err := childService.Run(ctx, childProject, indexer.Options{})
	if closeErr := childRepository.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range childReport.Diagnostics {
		if strings.Contains(diagnostic.Message, "uid://shared") {
			t.Fatalf("child worktree could not index its own resources: %#v", childReport.Diagnostics)
		}
	}

	write(t, filepath.Join(child, "scenes", "target.tscn"),
		"[gd_scene format=3 uid=\"uid://changed-in-child\"]\n\n[node name=\"Target\" type=\"Node\"]\n")
	write(t, filepath.Join(root, "ignored", "copy.tscn"),
		"[gd_scene format=3 uid=\"uid://changed-while-ignored\"]\n\n[node name=\"Ignored\" type=\"Node\"]\n")
	repeated, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Updated) != 0 || len(repeated.Removed) != 0 {
		t.Fatalf("excluded edits dirtied parent Godot semantics: %#v", repeated)
	}
}

// TestServiceReparsesScriptsWhenAutoloadEnablementChangesUnderGit exercises the
// Git selection path, where only changed paths are re-hashed: toggling an
// autoload's singleton marker must still reach the scripts whose resolution it
// changes, even though those scripts are untouched and unstaged.
func TestServiceReparsesScriptsWhenAutoloadEnablementChangesUnderGit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	write(t, mkdirFor(t, root, "project.godot"),
		"config_version=5\n\n[autoload]\nGame=\"*res://scripts/game.gd\"\n")
	write(t, mkdirFor(t, root, "scripts/game.gd"), "extends Node\nfunc start() -> void:\n\tpass\n")
	write(t, mkdirFor(t, root, "scripts/player.gd"), "extends Node\nfunc ready() -> void:\n\tGame.start()\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "initial")

	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	service := indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New()))
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	queries := query.NewService(repository)
	use, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertReached(t, use, "godot:autoload:project.godot:Game")

	write(t, filepath.Join(root, "project.godot"),
		"config_version=5\n\n[autoload]\nGame=\"res://scripts/game.gd\"\n")
	runGit(t, root, "add", "project.godot")
	runGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "disable")

	disabled, err := service.Run(ctx, project, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(disabled.Updated, "scripts/player.gd") {
		t.Fatalf("Git selection did not reach the dependent script: %#v", disabled.Updated)
	}
	stale, err := queries.Neighborhood(ctx, "scripts/player.ready", "", 1, query.Outgoing, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, reached := range stale.Nodes {
		if reached.Node.Kind == graph.KindGodotAutoload {
			t.Fatalf("a disabled autoload kept its script-side edge: %#v", reached.Node)
		}
	}
}

// TestServiceReconcilesGodotInteractionsAfterProjectEdits is the integrity test
// for the interaction vocabulary: adding and then removing a [global_group]
// declaration must move the scene's and the script's membership edges between the
// declared group and an unresolved boundary node, and each incremental state must
// equal what a clean rebuild of the same tree produces.
func TestServiceReconcilesGodotInteractionsAfterProjectEdits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, mkdirFor(t, root, "project.godot"),
		"config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n")
	write(t, mkdirFor(t, root, "scenes/arena.tscn"),
		"[gd_scene format=3]\n\n[node name=\"Arena\" type=\"Node2D\" groups=[\"enemies\"]]\n")
	write(t, mkdirFor(t, root, "scripts/spawner.gd"),
		"extends Node\n\nfunc poll() -> void:\n"+
			"\tif Input.is_action_pressed(\"jump\"):\n\t\tget_tree().call_group(\"enemies\", \"die\")\n")

	service, repository, project := openGodotIndex(t, ctx, root)
	defer func() { _ = repository.Close() }()
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	// Before the project declares the group, every membership and dispatch points
	// at an unresolved boundary node: the wiring is visible and unproven.
	assertGodotInteraction(t, ctx, repository, "scenes/arena", "godot:node_group:project.godot:enemies", true)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:node_group:project.godot:enemies", true)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:input_action:project.godot:jump", false)
	assertCleanRebuildMatches(t, ctx, root, repository)

	write(t, filepath.Join(root, "project.godot"),
		"config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n\n[global_group]\nenemies=\"Hostile\"\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	// The scene was not reparsed, yet its membership now resolves to the
	// declaration: by-name reconciliation is what keeps the two in step.
	assertGodotInteraction(t, ctx, repository, "scenes/arena", "godot:node_group:project.godot:enemies", false)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:node_group:project.godot:enemies", false)
	assertCleanRebuildMatches(t, ctx, root, repository)

	write(t, filepath.Join(root, "project.godot"), "config_version=5\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	// Removing both declarations must leave no stale edge to a deleted node and
	// no stale action either.
	assertGodotInteraction(t, ctx, repository, "scenes/arena", "godot:node_group:project.godot:enemies", true)
	assertGodotInteraction(t, ctx, repository, "scripts/spawner.poll", "godot:input_action:project.godot:jump", true)
	assertCleanRebuildMatches(t, ctx, root, repository)
}

func TestServiceProducerProvenanceConvergesAcrossForcedAndCleanIndexing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, mkdirFor(t, root, "project.godot"),
		"config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n\n[global_group]\nenemies=\"Hostile\"\n")
	write(t, mkdirFor(t, root, "scenes/arena.tscn"),
		"[gd_scene format=3]\n\n[node name=\"Arena\" type=\"Node2D\" groups=[\"enemies\"]]\n")
	write(t, mkdirFor(t, root, "scripts/player.gd"),
		"extends Node\n\nfunc poll() -> void:\n\tInput.is_action_pressed(\"jump\")\n\tget_tree().call_group(\"enemies\", \"wake\")\n")
	service, repository, project := openGodotIndex(t, ctx, root)
	defer func() { _ = repository.Close() }()
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	want := godotProducerSnapshot(t, ctx, repository, "scenes/arena", "scripts/player.poll")
	if _, err := service.Run(ctx, project, indexer.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := godotProducerSnapshot(t, ctx, repository, "scenes/arena", "scripts/player.poll"); !reflect.DeepEqual(got, want) {
		t.Fatalf("forced producer snapshot differs:\nwant=%v\ngot=%v", want, got)
	}

	cleanRoot := t.TempDir()
	if err := copyTree(t, root, cleanRoot); err != nil {
		t.Fatal(err)
	}
	cleanService, cleanRepository, cleanProject := openGodotIndex(t, ctx, cleanRoot)
	defer func() { _ = cleanRepository.Close() }()
	if _, err := cleanService.Run(ctx, cleanProject, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := godotProducerSnapshot(t, ctx, cleanRepository, "scenes/arena", "scripts/player.poll"); !reflect.DeepEqual(got, want) {
		t.Fatalf("clean producer snapshot differs:\nwant=%v\ngot=%v", want, got)
	}
}

func godotProducerSnapshot(t *testing.T, ctx context.Context, repository graph.Repository,
	selectors ...string) []string {
	t.Helper()
	var snapshot []string
	for _, selector := range selectors {
		report, err := query.NewService(repository).GodotInteractions(ctx, selector,
			query.GodotInteractionsOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, interaction := range append(report.Outbound, report.Inbound...) {
			snapshot = append(snapshot, strings.Join([]string{selector, string(interaction.Category),
				string(interaction.Edge.Kind), interaction.Edge.Producer, interaction.Form,
				interaction.Node.QualifiedName, fmt.Sprint(interaction.Node.External)}, "|"))
		}
	}
	slices.Sort(snapshot)
	return snapshot
}

func TestServiceReconcilesCrossFileSignalHandlers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, mkdirFor(t, root, "backend.gd"), "class_name Backend extends Node\nsignal sign_in_success\n")
	write(t, mkdirFor(t, root, "other.gd"), "class_name Other extends Node\nsignal ready\n")
	write(t, mkdirFor(t, root, "kit.gd"), `class_name Kit extends Node

var _backend: Backend
var _unknown

func wire() -> void:
	_backend.sign_in_success.connect(_on_sign_in)
	_backend.connect("sign_in_success", _on_legacy)
	_unknown.connect("ready", _on_unknown)
	_unknown.ready.connect(_on_unknown_member)

func _on_sign_in() -> void:
	pass

func _on_legacy() -> void:
	pass

func _on_unknown() -> void:
	pass

func _on_unknown_member() -> void:
	pass
`)
	service, repository, project := openGodotIndex(t, ctx, root)
	defer func() { _ = repository.Close() }()
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, "Kit._on_sign_in", "Backend.sign_in_success", false)
	assertCrossFileHandler(t, ctx, repository, "Kit._on_legacy", "Backend.sign_in_success", false)
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown")
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown_member")
	assertCleanRebuildMatches(t, ctx, root, repository)

	write(t, filepath.Join(root, "duplicate.gd"), "class_name Backend extends Node\nsignal sign_in_success\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, "Kit._on_sign_in", "Backend.sign_in_success", true)
	assertCrossFileHandler(t, ctx, repository, "Kit._on_legacy", "Backend.sign_in_success", true)
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown")
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown_member")
	assertCleanRebuildMatches(t, ctx, root, repository)

	if err := os.Remove(filepath.Join(root, "duplicate.gd")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, "Kit._on_sign_in", "Backend.sign_in_success", false)
	assertCrossFileHandler(t, ctx, repository, "Kit._on_legacy", "Backend.sign_in_success", false)
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown")
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown_member")

	write(t, filepath.Join(root, "backend.gd"), "class_name Backend extends Node\nsignal signed_in\n")
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	assertCrossFileHandler(t, ctx, repository, "Kit._on_sign_in", "Backend.sign_in_success", true)
	assertCrossFileHandler(t, ctx, repository, "Kit._on_legacy", "Backend.sign_in_success", true)
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown")
	assertNoHandledBy(t, ctx, repository, "Kit._on_unknown_member")
	assertCleanRebuildMatches(t, ctx, root, repository)
}

func assertCrossFileHandler(t *testing.T, ctx context.Context, repository graph.Repository, handlerName, signalName string, external bool) {
	t.Helper()
	handler, err := query.NewService(repository).Resolve(ctx, handlerName)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesTo(ctx, handler.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind != graph.EdgeHandledBy {
			continue
		}
		source, err := repository.Node(ctx, edge.FromID)
		if err != nil {
			t.Fatal(err)
		}
		if source.QualifiedName != signalName || source.Kind != graph.KindEvent || source.External != external {
			t.Fatalf("handled_by source = %#v, want event external=%v", source, external)
		}
		return
	}
	t.Fatalf("missing handled_by edge to %#v; inbound=%#v", handler, edges)
}

func assertNoHandledBy(t *testing.T, ctx context.Context, repository graph.Repository, handlerName string) {
	t.Helper()
	handler, err := query.NewService(repository).Resolve(ctx, handlerName)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := repository.EdgesTo(ctx, handler.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind == graph.EdgeHandledBy {
			t.Fatalf("unproven legacy receiver produced handled_by edge %#v", edge)
		}
	}
}

func openGodotIndex(t *testing.T, ctx context.Context, root string) (*indexer.Service, *sqlite.Repository, indexer.Project) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	return indexer.NewService(repository, parserapi.NewRegistry(gdscriptparser.New(), godotparser.New())),
		repository, project
}

// assertGodotInteraction checks that one interaction reaches the named far side
// and that its resolution state is what the evidence licenses.
func assertGodotInteraction(t *testing.T, ctx context.Context, repository graph.Repository, selector, qualified string, external bool) {
	t.Helper()
	report, err := query.NewService(repository).GodotInteractions(ctx, selector,
		query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatalf("interactions for %s: %v", selector, err)
	}
	for _, interaction := range report.Outbound {
		if interaction.Node.QualifiedName != qualified {
			continue
		}
		if interaction.Node.External != external {
			t.Fatalf("%s -> %s external = %v, want %v", selector, qualified,
				interaction.Node.External, external)
		}
		if interaction.Edge.Producer != graph.ProducerGDScript && interaction.Edge.Producer != graph.ProducerGodot {
			t.Fatalf("%s -> %s producer = %q", selector, qualified, interaction.Edge.Producer)
		}
		return
	}
	t.Fatalf("%s has no interaction with %s; got %#v", selector, qualified, report.Outbound)
}

// assertCleanRebuildMatches indexes the same tree into a fresh index and compares
// the edge and node census, which is the property an incremental index has to
// preserve for the interaction vocabulary to be trustworthy.
func assertCleanRebuildMatches(t *testing.T, ctx context.Context, root string, incremental graph.Repository) {
	t.Helper()
	clean := t.TempDir()
	if err := copyTree(t, root, clean); err != nil {
		t.Fatal(err)
	}
	service, repository, project := openGodotIndex(t, ctx, clean)
	defer func() { _ = repository.Close() }()
	if _, err := service.Run(ctx, project, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	want, err := repository.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := incremental.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want.ByKind, got.ByKind) || !reflect.DeepEqual(want.ByEdge, got.ByEdge) {
		t.Fatalf("incremental index differs from a clean rebuild:\nkinds %v vs %v\nedges %v vs %v",
			got.ByKind, want.ByKind, got.ByEdge, want.ByEdge)
	}
	if want.External != got.External {
		t.Fatalf("external node count = %d, clean rebuild = %d", got.External, want.External)
	}
}

func copyTree(t *testing.T, from, to string) error {
	t.Helper()
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		// The index lives inside the tree; copying it would make the rebuild
		// incremental instead of clean.
		if strings.HasPrefix(relative, ".grafo") {
			return filepath.SkipDir
		}
		target := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
}
