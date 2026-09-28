package transport_test

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/transport"
)

func TestExactAdapterMatchesOnlyRegisteredAPI(t *testing.T) {
	adapter := transport.ExactAdapter{
		"example.Peer.Send": {Protocol: "enet", API: "example.Peer.Send", Direction: transport.Send,
			ChannelPosition: 1, PayloadPosition: 0, ReliabilityPosition: 2},
	}
	spec, ok := adapter.Match("example.Peer.Send")
	if !ok || spec.Direction != transport.Send || spec.PayloadPosition != 0 {
		t.Fatalf("Match() = %#v, %v", spec, ok)
	}
	if _, ok := adapter.Match("example.Lookalike.Send"); ok {
		t.Fatal("same-name non-transport API matched")
	}
}

func TestEmitDeclaresStableOperationAndCanonicalPayload(t *testing.T) {
	input := parserapi.Input{Path: "client.go", RepoID: "repo"}
	b := parserapi.NewBuilder(input, "go")
	loc := graph.Location{Path: input.Path, Line: 12, Column: 3, EndLine: 12}
	operationID := transport.Emit(b, "function", transport.Operation{
		Spec: transport.Spec{Protocol: "enet", API: "github.com/cafecito-games/goenet/pkg.Peer.Send",
			Direction: transport.Send, ChannelPosition: 0, PayloadPosition: 1, ReliabilityPosition: 1,
			PayloadField: "Data", ReliabilityField: "Flags"},
		Channel: "3", ChannelStatus: "proven", Reliability: "reliable",
		PayloadStatus: "proven", Proof: "go/types", WrapperDepth: 1, Location: loc,
		MessageID: "message-id", Message: "acme.v1.Envelope", Binding: "example.com/gen.Envelope",
	})
	result := b.Finish()
	if operationID == "" || len(result.Nodes) != 2 {
		t.Fatalf("operation not declared: id=%q nodes=%#v", operationID, result.Nodes)
	}
	var operation graph.Node
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTransportOperation {
			operation = node
		}
	}
	if operation.ID != operationID || operation.Properties["channel"] != "3" ||
		operation.Properties["reliability"] != "reliable" || operation.Properties["payload_status"] != "proven" ||
		operation.Properties["wrapper_depth"] != "1" {
		t.Fatalf("operation evidence = %#v", operation)
	}
	if operation.Properties["payload_field"] != "Data" || operation.Properties["reliability_field"] != "Flags" {
		t.Fatalf("struct field evidence = %#v", operation.Properties)
	}
	if _, ok := operation.Properties["channel_field"]; ok {
		t.Fatalf("positional channel declared a field path: %#v", operation.Properties)
	}
	assertFact(t, result.Facts, "function", graph.EdgeSends, operationID)
	assertFact(t, result.Facts, operationID, graph.EdgeCarries, "message-id")
	assertFactProperty(t, result.Facts, operationID, graph.EdgeCarries, "payload_field", "Data")

	b2 := parserapi.NewBuilder(input, "go")
	unknownID := transport.Emit(b2, "function", transport.Operation{
		Spec: transport.Spec{Protocol: "enet", API: "example.Peer.Send", Direction: transport.Send,
			ChannelPosition: 1, PayloadPosition: 0, ReliabilityPosition: 2},
		ChannelStatus: "unknown", Reliability: "unknown", PayloadStatus: "ambiguous", Proof: "go/types", Location: loc,
	})
	unknown := b2.Finish()
	assertFact(t, unknown.Facts, "function", graph.EdgeSends, unknownID)
	for _, fact := range unknown.Facts {
		if fact.Kind == graph.EdgeCarries {
			t.Fatalf("unproven payload emitted carries edge: %#v", fact)
		}
	}
}

func assertFact(t *testing.T, facts []graph.Fact, from string, kind graph.EdgeKind, targetID string) {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID == from && fact.Kind == kind && fact.TargetID == targetID {
			return
		}
	}
	t.Fatalf("missing %s fact from %q to %q: %#v", kind, from, targetID, facts)
}

func assertFactProperty(t *testing.T, facts []graph.Fact, fromID string, kind graph.EdgeKind, key, want string) {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID == fromID && fact.Kind == kind {
			if fact.Properties[key] != want {
				t.Fatalf("fact %s property %q = %q, want %q", kind, key, fact.Properties[key], want)
			}
			return
		}
	}
	t.Fatalf("missing %s fact from %q: %#v", kind, fromID, facts)
}
