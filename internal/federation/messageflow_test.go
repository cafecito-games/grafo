package federation_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestMessageFlowRetainsFederatedRepositoryAndComponentEvidence(t *testing.T) {
	ctx := context.Background()
	workspace := testtemp.Dir(t)
	schemaRoot, clientRoot, gammaRoot := filepath.Join(workspace, "schema"), filepath.Join(workspace, "client"), filepath.Join(workspace, "gamma")
	for _, root := range []string{schemaRoot, clientRoot, gammaRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	message := graph.Node{ID: "n:message", Kind: graph.KindType, Name: "Envelope", QualifiedName: "acme.v1.Envelope",
		OwnerFile: "schema.proto", Language: "protobuf", Properties: map[string]string{"declaration": "message"}}
	field := graph.Node{ID: "n:field", Kind: graph.KindField, Name: "text", QualifiedName: "acme.v1.Envelope.text", OwnerFile: "schema.proto", Language: "protobuf"}
	schemaComponent := graph.Node{ID: "n:schema-component", Kind: graph.KindComponent, Name: "schema-side", QualifiedName: "component:schema-side", OwnerFile: "__workspace__"}
	schemaSamePath := graph.Node{ID: "n:schema-client-file", Kind: graph.KindFile, Name: "client.go", QualifiedName: "client.go", OwnerFile: "client.go", Location: graph.Location{Path: "client.go"}}
	seedCatalogIndex(t, ctx, schemaRoot, "schema.proto", graph.ParseResult{Nodes: []graph.Node{message, field, schemaComponent, schemaSamePath}, Facts: []graph.Fact{
		{ID: "f:field", FromID: message.ID, Kind: graph.EdgeHasField, TargetID: field.ID, OwnerFile: "schema.proto"},
		{ID: "f:schema-component", FromID: schemaComponent.ID, Kind: graph.EdgeContains, TargetID: schemaSamePath.ID, OwnerFile: "schema.proto"},
	}})
	binding := graph.Node{ID: "n:binding", Kind: graph.KindType, Name: "Envelope", QualifiedName: "generated.Envelope",
		OwnerFile: "client.go", Properties: map[string]string{"generator": "protoc-gen-go"}}
	build := graph.Node{ID: "n:build", Kind: graph.KindFunction, Name: "Build", QualifiedName: "client.Build", OwnerFile: "client.go"}
	op := graph.Node{ID: "n:send", Kind: graph.KindTransportOperation, Name: "send", QualifiedName: "client.go:8:2:send",
		OwnerFile: "client.go", Properties: map[string]string{"direction": "send", "channel": "3", "channel_status": "proven", "payload_status": "proven", "reliability": "reliable"}}
	component := graph.Node{ID: "n:component", Kind: graph.KindComponent, Name: "client", QualifiedName: "component:client", OwnerFile: "__workspace__"}
	file := graph.Node{ID: "n:file", Kind: graph.KindFile, Name: "client.go", QualifiedName: "client.go", OwnerFile: "client.go", Location: graph.Location{Path: "client.go"}}
	seedCatalogIndex(t, ctx, clientRoot, "client.go", graph.ParseResult{Nodes: []graph.Node{binding, build, op, component, file}, Facts: []graph.Fact{
		{ID: "f:component", FromID: component.ID, Kind: graph.EdgeContains, TargetID: file.ID, OwnerFile: "client.go"},
		{ID: "f:binding", FromID: binding.ID, Kind: graph.EdgeGeneratedFrom, Target: message.QualifiedName, TargetKind: graph.KindType, OwnerFile: "client.go"},
		{ID: "f:write", FromID: build.ID, Kind: graph.EdgeWrites, Target: field.QualifiedName, TargetKind: graph.KindField, OwnerFile: "client.go"},
		{ID: "f:encode", FromID: build.ID, Kind: graph.EdgeEncodes, Target: message.QualifiedName, TargetKind: graph.KindType, OwnerFile: "client.go"},
		{ID: "f:send", FromID: build.ID, Kind: graph.EdgeSends, TargetID: op.ID, OwnerFile: "client.go"},
		{ID: "f:gamma-send", FromID: build.ID, Kind: graph.EdgeSends, Target: "gamma.network.Send", TargetKind: graph.KindTransportOperation, OwnerFile: "client.go"},
		{ID: "f:carries", FromID: op.ID, Kind: graph.EdgeCarries, Target: message.QualifiedName, TargetKind: graph.KindType, OwnerFile: "client.go"},
	}})
	gammaOp := graph.Node{ID: "n:gamma-send", Kind: graph.KindTransportOperation, Name: "Send", QualifiedName: "gamma.network.Send",
		OwnerFile: "network.go", Properties: map[string]string{"direction": "send", "channel_status": "unknown", "payload_status": "unknown"}}
	gammaComponent := graph.Node{ID: "n:gamma-component", Kind: graph.KindComponent, Name: "netside", QualifiedName: "component:netside", OwnerFile: "__workspace__"}
	gammaFile := graph.Node{ID: "n:gamma-file", Kind: graph.KindFile, Name: "network.go", QualifiedName: "network.go",
		OwnerFile: "network.go", Location: graph.Location{Path: "network.go"}}
	seedCatalogIndex(t, ctx, gammaRoot, "network.go", graph.ParseResult{Nodes: []graph.Node{gammaOp, gammaComponent, gammaFile}, Facts: []graph.Fact{
		{ID: "f:gamma-component", FromID: gammaComponent.ID, Kind: graph.EdgeContains, TargetID: gammaFile.ID, OwnerFile: "network.go"},
	}})

	repository, err := federation.Open(ctx, []string{schemaRoot, clientRoot, gammaRoot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	flow, err := query.NewMessageFlow(repository).Flow(ctx, message.QualifiedName, query.MessageFlowOptions{Repository: "schema", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if flow.Message.Repository != "schema" || len(flow.Encoders) != 1 || flow.Encoders[0].Repository != "client" ||
		flow.Encoders[0].Component != "client" || flow.Encoders[0].ComponentID != component.ID {
		t.Fatalf("federated message evidence attribution = %#v", flow)
	}
	if len(flow.Bindings) != 1 || flow.Bindings[0].Repository != "client" || len(flow.Sends) != 1 || flow.Sends[0].Evidence.Repository != "client" {
		t.Fatalf("federated binding/transport evidence attribution = %#v", flow)
	}
	foundGamma := false
	for _, evidence := range flow.UnknownEvidence {
		if evidence.Node.ID == gammaOp.ID {
			foundGamma = evidence.Repository == "gamma" && evidence.Component == "netside" && evidence.ComponentID == gammaComponent.ID
		}
	}
	if !foundGamma {
		t.Fatalf("outgoing projected evidence lost gamma ownership: %#v", flow.UnknownEvidence)
	}
}
