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

func TestServerExposesScalarBatchAndCoverageMessageTools(t *testing.T) {
	ctx := context.Background()
	repository, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if err := repository.SetMeta(ctx, "root", "/tmp/example/shop"); err != nil {
		t.Fatal(err)
	}
	message := graph.Node{ID: "n:message", Kind: graph.KindType, Name: "Envelope", QualifiedName: "acme.v1.Envelope",
		OwnerFile: "schema.proto", Properties: map[string]string{"declaration": "message"}}
	field := graph.Node{ID: "n:field", Kind: graph.KindField, Name: "text", QualifiedName: "acme.v1.Envelope.text", OwnerFile: "schema.proto"}
	producer := graph.Node{ID: "n:producer", Kind: graph.KindFunction, Name: "Build", QualifiedName: "client.Build", OwnerFile: "client.go"}
	binding := graph.Node{ID: "n:binding", Kind: graph.KindType, Name: "Envelope", QualifiedName: "generated.Envelope",
		OwnerFile: "generated.pb.go", Properties: map[string]string{"generator": "protoc-gen-go"}}
	if err := repository.ReplaceOwner(ctx, "schema.proto", graph.ParseResult{Nodes: []graph.Node{message, field}, Facts: []graph.Fact{{
		ID: "f:field", FromID: message.ID, Kind: graph.EdgeHasField, TargetID: field.ID, OwnerFile: "schema.proto",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "client.go", graph.ParseResult{Nodes: []graph.Node{producer}, Facts: []graph.Fact{{
		ID: "f:write", FromID: producer.ID, Kind: graph.EdgeWrites, TargetID: field.ID, OwnerFile: "client.go",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceOwner(ctx, "generated.pb.go", graph.ParseResult{Nodes: []graph.Node{binding}, Facts: []graph.Fact{{
		ID: "f:binding", FromID: binding.ID, Kind: graph.EdgeGeneratedFrom, TargetID: message.ID, OwnerFile: "generated.pb.go",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	session := connect(t, mcpserver.New(repository, indexer.Project{Name: "shop", Branch: "main"}))
	flow := call(t, session, "get_message_flow", map[string]any{"selector": "acme.v1.Envelope", "repository": "shop"})
	if message, ok := flow["message"].(map[string]any); !ok || message["qualified_name"] != "acme.v1.Envelope" {
		t.Fatalf("unexpected scalar flow: %#v", flow)
	}
	batch := call(t, session, "get_message_flow", map[string]any{"selectors": []string{"acme.v1.Envelope", "missing"}})
	if results, ok := batch["results"].([]any); !ok || len(results) != 2 {
		t.Fatalf("batch did not preserve result/error envelopes: %#v", batch)
	}
	coverage := call(t, session, "list_message_coverage", map[string]any{"package": "acme.v1", "status": "missing_evidence"})
	if messages, ok := coverage["messages"].([]any); !ok || len(messages) != 1 {
		t.Fatalf("unexpected coverage: %#v", coverage)
	}
	callExpectingError(t, session, "list_message_coverage", map[string]any{"status": "maybe"})
}
