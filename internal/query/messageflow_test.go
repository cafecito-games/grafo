package query_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestMessageFlowAggregatesCanonicalEvidenceAndOneofCoverage(t *testing.T) {
	repository := newMessageFlowFixture()
	service := query.NewMessageFlow(repository)

	flow, err := service.Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if flow.Message.ID != "n:message" || flow.Status != query.CoverageMissingEvidence || flow.Truncated {
		t.Fatalf("message flow identity/status = %#v", flow)
	}
	if got := evidenceIDs(flow.Bindings); !reflect.DeepEqual(got, []string{"e:binding-gd", "e:binding-go"}) {
		t.Fatalf("bindings lost canonical evidence: %v", got)
	}
	if len(flow.Encoders) != 1 || flow.Encoders[0].Component != "client" || flow.Encoders[0].FactID != "f:encode" {
		t.Fatalf("encoder component/evidence = %#v", flow.Encoders)
	}
	if len(flow.Decoders) != 1 || flow.Decoders[0].Component != "server" || flow.Decoders[0].EdgeID != "e:decode" {
		t.Fatalf("decoder component/evidence = %#v", flow.Decoders)
	}
	if len(flow.Sends) != 1 || flow.Sends[0].Channel != "3" || flow.Sends[0].Reliability != "reliable" ||
		len(flow.Sends[0].Sources) != 1 || flow.Sends[0].Sources[0].EdgeID != "e:sends" {
		t.Fatalf("send transport evidence = %#v", flow.Sends)
	}
	if len(flow.Receives) != 1 || flow.Receives[0].Channel != "3" || len(flow.ChannelMismatches) != 0 {
		t.Fatalf("receive or channel pairing = receives=%#v mismatches=%#v", flow.Receives, flow.ChannelMismatches)
	}
	if len(flow.Handlers) == 0 || flow.Handlers[0].Evidence.Node.ID == "" {
		t.Fatalf("consumer/decoder handlers missing: %#v", flow.Handlers)
	}
	var text, image, receipt, unused query.MessageMemberFlow
	for _, member := range flow.Members {
		switch member.Field.Name {
		case "text":
			text = member
		case "image":
			image = member
		case "receipt":
			receipt = member
		case "unused":
			unused = member
		}
	}
	if text.Status != query.CoverageResolved || text.Oneof != "payload" || len(text.Producers) != 1 || len(text.Consumers) != 1 {
		t.Fatalf("complete oneof arm = %#v", text)
	}
	if image.Status != query.CoverageMissingEvidence || !hasCoverageCategory(image.Gaps, query.ProducedWithoutConsumer) {
		t.Fatalf("produced-only arm = %#v", image)
	}
	if receipt.Status != query.CoverageMissingEvidence || !hasCoverageCategory(receipt.Gaps, string(query.ConsumedWithoutProducer)) {
		t.Fatalf("consumed-only arm = %#v", receipt)
	}
	if unused.Status != query.CoverageMissingEvidence || !hasCoverageCategory(unused.Gaps, query.UnusedMessageMember) {
		t.Fatalf("unused arm = %#v", unused)
	}
	if repository.legacyEdgeCalls != 0 {
		t.Fatalf("message flow used unbounded adjacency %d times", repository.legacyEdgeCalls)
	}
}

func TestMessageCoverageClassifiesMismatchUnknownBoundsAndFilters(t *testing.T) {
	repository := newMessageFlowFixture()
	service := query.NewMessageFlow(repository)

	coverage, err := service.Coverage(context.Background(), query.MessageCoverageOptions{
		Repository: "transport", Package: "acme.v1", Direction: query.Both, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := coverageNames(coverage.Messages); !reflect.DeepEqual(got, []string{"acme.v1.Envelope", "acme.v1.Mismatch"}) {
		t.Fatalf("coverage ordering/filter = %v", got)
	}
	if coverage.Messages[1].Status != query.CoverageMissingEvidence ||
		!hasCoverageCategory(coverage.Messages[1].Gaps, query.ChannelMismatch) {
		t.Fatalf("proven channel mismatch not classified: %#v", coverage.Messages[1])
	}
	if !hasCoverageCategory(coverage.Messages[0].Gaps, query.ProducedWithoutConsumer) ||
		!hasCoverageCategory(coverage.Messages[0].Gaps, string(query.ConsumedWithoutProducer)) {
		t.Fatalf("member gaps were omitted from message coverage: %#v", coverage.Messages[0])
	}

	unknown, err := service.Coverage(context.Background(), query.MessageCoverageOptions{
		Repository: "uncertain", Status: query.CoverageUnknown, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Messages) != 1 || unknown.Messages[0].Message.QualifiedName != "acme.v1.Dynamic" ||
		unknown.Messages[0].Status != query.CoverageUnknown || len(unknown.Messages[0].UnknownEvidence) != 1 ||
		!hasUncertainty(unknown.Messages[0].Uncertainties, "binding_evidence_missing") ||
		!hasUncertainty(unknown.Messages[0].Uncertainties, "dynamic_transport") {
		t.Fatalf("dynamic payload absence was not unknown: %#v", unknown)
	}

	bounded, err := service.Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated || bounded.Status != query.CoverageUnknown {
		t.Fatalf("bounded flow did not downgrade absence: %#v", bounded)
	}
	if !hasUncertainty(bounded.Uncertainties, "truncated") {
		t.Fatalf("truncation has no structured unknown reason: %#v", bounded.Uncertainties)
	}
	if repository.legacyEdgeCalls != 0 {
		t.Fatalf("coverage used unbounded adjacency %d times", repository.legacyEdgeCalls)
	}
}

func hasUncertainty(items []query.FlowUncertainty, reason string) bool {
	for _, item := range items {
		if item.Reason == reason {
			return true
		}
	}
	return false
}

func TestMessageFlowPreservesAmbiguityAndRepositoryScope(t *testing.T) {
	repository := newMessageFlowFixture()
	repository.add("uncertain", graph.Node{ID: "n:shadow-message", Kind: graph.KindType, Name: "Envelope",
		QualifiedName: "acme.v1.Envelope", OwnerFile: "shadow.proto", Properties: map[string]string{"declaration": "message"}})
	service := query.NewMessageFlow(repository)

	if _, err := service.Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20}); err == nil {
		t.Fatal("an ambiguous canonical message selector was guessed")
	} else if ambiguous, ok := err.(*query.AmbiguousError); !ok || len(ambiguous.Candidates) != 2 {
		t.Fatalf("ambiguity candidates were not preserved: %T %v", err, err)
	}
	flow, err := service.Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Repository: "transport", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if flow.Message.ID != "n:message" || flow.Message.Repository != "transport" {
		t.Fatalf("repository-scoped selector chose the wrong declaration: %#v", flow.Message)
	}
}

func TestMessageFlowBatchReusesOneComponentSnapshotAndIsolatesErrors(t *testing.T) {
	repository := newMessageFlowFixture()
	attempts := query.NewMessageFlow(repository).Flows(context.Background(),
		[]string{"acme.v1.Envelope", "missing", "acme.v1.Mismatch"}, query.MessageFlowOptions{Limit: 20})
	if len(attempts) != 3 || attempts[0].Error != nil || attempts[0].Flow.Message.QualifiedName != "acme.v1.Envelope" ||
		attempts[1].Error == nil || attempts[2].Error != nil || attempts[2].Flow.Message.QualifiedName != "acme.v1.Mismatch" {
		t.Fatalf("batch order/error isolation = %#v", attempts)
	}
	if repository.componentScans != 1 {
		t.Fatalf("batch rebuilt component snapshot %d times", repository.componentScans)
	}
}

func TestMessageFlowComponentSnapshotIgnoresUnrelatedGraphSize(t *testing.T) {
	repository := newMessageFlowFixture()
	for index := 0; index <= query.MaxCatalogLimit; index++ {
		repository.add("transport", graph.Node{
			ID:            fmt.Sprintf("n:unrelated:%04d", index),
			Kind:          graph.KindFunction,
			Name:          fmt.Sprintf("Unrelated%04d", index),
			QualifiedName: fmt.Sprintf("unrelated.Function%04d", index),
			OwnerFile:     "unrelated.go",
		})
	}

	flow, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if flow.Truncated {
		t.Fatalf("unrelated functions made component evidence uncertain: %#v", flow.Uncertainties)
	}
}

func TestMessageCoverageBoundsCanonicalMessagesAfterFilteringOrdinaryTypes(t *testing.T) {
	repository := newMessageFlowFixture()
	for index := 0; index <= query.MaxCatalogLimit; index++ {
		repository.add("transport", graph.Node{
			ID:            fmt.Sprintf("n:ordinary:%04d", index),
			Kind:          graph.KindType,
			Name:          fmt.Sprintf("Ordinary%04d", index),
			QualifiedName: fmt.Sprintf("aaa.Ordinary%04d", index),
			Properties:    map[string]string{"declaration": "struct"},
		})
	}

	coverage, err := query.NewMessageFlow(repository).Coverage(context.Background(), query.MessageCoverageOptions{
		Repository: "transport", Package: "acme.v1", Message: "Envelope", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Truncated || len(coverage.Messages) != 1 || coverage.Messages[0].Message.QualifiedName != "acme.v1.Envelope" {
		t.Fatalf("ordinary types consumed canonical message bound: %#v", coverage)
	}
}

func TestMessageFlowRejectsContradictoryComponentOwnership(t *testing.T) {
	repository := newMessageFlowFixture()
	repository.add("transport", graph.Node{ID: "n:component-shadow", Kind: graph.KindComponent, Name: "shadow", QualifiedName: "component:shadow"})
	addMessageFlowEdge(repository, "e:owns-client-shadow", "n:component-shadow", "n:file-client", graph.EdgeContains, nil)

	_, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20})
	if err == nil || !strings.Contains(err.Error(), "client") || !strings.Contains(err.Error(), "shadow") {
		t.Fatalf("contradictory component ownership error = %v", err)
	}
}

func TestMessageFlowDoesNotConflateEqualFileBasenames(t *testing.T) {
	repository := newMessageFlowFixture()
	clientFile := repository.nodes["n:file-client"]
	clientFile.Name = "client.go"
	clientFile.QualifiedName = "a/client.go"
	clientFile.OwnerFile = "a/client.go"
	clientFile.Location.Path = "a/client.go"
	repository.nodes[clientFile.ID] = clientFile
	build := repository.nodes["n:build"]
	build.OwnerFile = "a/client.go"
	repository.nodes[build.ID] = build

	repository.add("transport", graph.Node{ID: "n:component-other", Kind: graph.KindComponent,
		Name: "other", QualifiedName: "component:other"})
	repository.add("transport", graph.Node{ID: "n:file-other-client", Kind: graph.KindFile,
		Name: "client.go", QualifiedName: "b/client.go", OwnerFile: "b/client.go", Location: graph.Location{Path: "b/client.go"}})
	addMessageFlowEdge(repository, "e:owns-other-client", "n:component-other", "n:file-other-client", graph.EdgeContains, nil)

	flow, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(flow.Encoders) != 1 || flow.Encoders[0].Component != "client" {
		t.Fatalf("path-qualified owner was lost: %#v", flow.Encoders)
	}
}

func TestMessageFlowReportsOnlyGenuineComponentBounds(t *testing.T) {
	t.Run("component enumeration", func(t *testing.T) {
		repository := newMessageFlowFixture()
		for index := 0; index <= query.MaxCatalogLimit; index++ {
			repository.add("transport", graph.Node{ID: fmt.Sprintf("n:component-extra:%04d", index),
				Kind: graph.KindComponent, Name: fmt.Sprintf("extra-%04d", index), QualifiedName: fmt.Sprintf("component:extra-%04d", index)})
		}
		flow, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if !flow.Truncated || !hasUncertainty(flow.Uncertainties, "truncated") {
			t.Fatalf("component overflow was not reported: %#v", flow)
		}
	})

	t.Run("component membership", func(t *testing.T) {
		repository := newMessageFlowFixture()
		for index := 0; index <= query.MaxCatalogLimit; index++ {
			id, path := fmt.Sprintf("n:file-extra:%04d", index), fmt.Sprintf("extra/%04d.go", index)
			repository.add("transport", graph.Node{ID: id, Kind: graph.KindFile, Name: path, QualifiedName: path,
				OwnerFile: path, Location: graph.Location{Path: path}})
			addMessageFlowEdge(repository, "e:owns-extra:"+id, "n:component-client", id, graph.EdgeContains, nil)
		}
		flow, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if !flow.Truncated || !hasUncertainty(flow.Uncertainties, "truncated") {
			t.Fatalf("component membership overflow was not reported: %#v", flow)
		}
	})

	t.Run("no components", func(t *testing.T) {
		repository := newMessageFlowFixture()
		delete(repository.nodes, "n:component-client")
		delete(repository.nodes, "n:component-server")
		repository.edges = slices.DeleteFunc(repository.edges, func(edge graph.Edge) bool { return edge.Kind == graph.EdgeContains })
		flow, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if flow.Truncated {
			t.Fatalf("empty component map was uncertain: %#v", flow.Uncertainties)
		}
		for _, evidence := range append(flow.Encoders, flow.Decoders...) {
			if evidence.Component != "" || evidence.ComponentID != "" {
				t.Fatalf("unassigned evidence invented a component: %#v", evidence)
			}
		}
	})
}

func TestMessageFlowCatalogFailuresRemainFailClosed(t *testing.T) {
	repository := newMessageFlowFixture()
	want := errors.New("component membership unavailable")
	repository.relationEdgeErr = want
	if _, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Limit: 20}); !errors.Is(err, want) {
		t.Fatalf("component adapter error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := query.NewMessageFlow(newMessageFlowFixture()).Coverage(cancelled, query.MessageCoverageOptions{Limit: 20}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled canonical coverage error = %v", err)
	}

	repository = newMessageFlowFixture()
	want = errors.New("canonical catalog unavailable")
	repository.canonicalErr = want
	if _, err := query.NewMessageFlow(repository).Coverage(context.Background(), query.MessageCoverageOptions{Limit: 20}); !errors.Is(err, want) {
		t.Fatalf("canonical adapter error = %v", err)
	}
	withoutCatalog := struct{ graph.TopologyRepository }{TopologyRepository: newMessageFlowFixture()}
	if _, err := query.NewMessageFlow(withoutCatalog).Coverage(context.Background(), query.MessageCoverageOptions{Limit: 20}); err == nil || !strings.Contains(err.Error(), "canonical message catalogs") {
		t.Fatalf("unsupported canonical catalog error = %v", err)
	}
}

func TestMessageCoverageClassifiesCodecAndTransportPipelineGaps(t *testing.T) {
	repository := newMessageFlowFixture()
	addMessage := func(id, name string) graph.Node {
		message := graph.Node{ID: id, Kind: graph.KindType, Name: name, QualifiedName: "gaps.v1." + name,
			OwnerFile: "gaps.proto", Properties: map[string]string{"declaration": "message"}}
		repository.add("transport", message)
		binding := graph.Node{ID: id + ":binding", Kind: graph.KindType, Name: name, QualifiedName: "gen." + name,
			OwnerFile: "generated.pb.go", Properties: map[string]string{"generator": "protoc-gen-go"}}
		repository.add("transport", binding)
		addMessageFlowEdge(repository, id+":binding-edge", binding.ID, id, graph.EdgeGeneratedFrom, nil)
		return message
	}
	encoded := addMessage("n:encoded-only", "EncodedOnly")
	addMessageFlowEdge(repository, "e:encoded-only", "n:build", encoded.ID, graph.EdgeEncodes, nil)
	received := addMessage("n:receive-only", "ReceiveOnly")
	receiveOp := graph.Node{ID: "n:receive-only-op", Kind: graph.KindTransportOperation, Name: "receive",
		QualifiedName: "server.go:90:2:receive", OwnerFile: "server.go", Properties: map[string]string{
			"direction": "receive", "channel": "9", "channel_status": "proven", "payload_status": "proven"}}
	repository.add("transport", receiveOp)
	addMessageFlowEdge(repository, "e:receive-only", "n:handle", receiveOp.ID, graph.EdgeReceives, nil)
	addMessageFlowEdge(repository, "e:receive-only-carries", receiveOp.ID, received.ID, graph.EdgeCarries, nil)

	service := query.NewMessageFlow(repository)
	encodedFlow, err := service.Flow(context.Background(), encoded.ID, query.MessageFlowOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if encodedFlow.Status != query.CoverageMissingEvidence || !hasCoverageCategory(encodedFlow.Gaps, query.EncodedWithoutSend) {
		t.Fatalf("encoded-only gap = %#v", encodedFlow)
	}
	receivedFlow, err := service.Flow(context.Background(), received.ID, query.MessageFlowOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if receivedFlow.Status != query.CoverageMissingEvidence || !hasCoverageCategory(receivedFlow.Gaps, query.ReceivedWithoutDecode) {
		t.Fatalf("receive-only gap = %#v", receivedFlow)
	}
}

func TestMessageCoveragePathPrefixSelectsCanonicalDeclaration(t *testing.T) {
	repository := newMessageFlowFixture()
	message := repository.nodes["n:message"]
	message.Location = graph.Location{Path: "proto/schema.proto", Line: 1}
	repository.nodes[message.ID] = message
	service := query.NewMessageFlow(repository)
	result, err := service.Coverage(context.Background(), query.MessageCoverageOptions{
		PathPrefixes: []string{"proto"}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || result.Messages[0].Message.ID != "n:message" {
		t.Fatalf("path-scoped coverage = %#v", result)
	}
	empty, err := service.Coverage(context.Background(), query.MessageCoverageOptions{
		PathPrefixes: []string{"protocol"}, Limit: 10,
	})
	if err != nil || len(empty.Messages) != 0 || empty.Truncated {
		t.Fatalf("segment no-match coverage = %#v, %v", empty, err)
	}
}

func TestMessageFlowDowngradesUnsupportedAndUnresolvedBindings(t *testing.T) {
	repository := newMessageFlowFixture()
	for _, test := range []struct {
		id, name, reason, generator string
		external                    bool
	}{
		{id: "unsupported", name: "Unsupported", reason: "unsupported_generator", generator: "mystery-generator"},
		{id: "missing", name: "Missing", reason: "unsupported_generator"},
		{id: "unresolved", name: "Unresolved", reason: "unresolved_binding", generator: "mystery-generator", external: true},
	} {
		messageID, bindingID := "n:"+test.id, "n:"+test.id+":binding"
		repository.add("transport", graph.Node{ID: messageID, Kind: graph.KindType, Name: test.name,
			QualifiedName: "unknown.v1." + test.name, Properties: map[string]string{"declaration": "message"}})
		repository.add("transport", graph.Node{ID: bindingID, Kind: graph.KindType, Name: test.name,
			QualifiedName: "generated." + test.name, External: test.external, Properties: map[string]string{"generator": test.generator}})
		addMessageFlowEdge(repository, "e:"+test.id+":binding", bindingID, messageID, graph.EdgeGeneratedFrom, nil)
		flow, err := query.NewMessageFlow(repository).Flow(context.Background(), messageID, query.MessageFlowOptions{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if flow.Status != query.CoverageUnknown || !hasUncertainty(flow.Uncertainties, test.reason) {
			t.Fatalf("%s binding did not fail closed: %#v", test.reason, flow)
		}
		componentFlow, err := query.NewMessageFlow(repository).Flow(context.Background(), messageID,
			query.MessageFlowOptions{Component: "client", Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if componentFlow.Status != query.CoverageUnknown || !hasUncertainty(componentFlow.Uncertainties, test.reason) {
			t.Fatalf("component filter discarded message-wide %s uncertainty: %#v", test.reason, componentFlow)
		}
	}
}

func TestMessageFlowDirectionDoesNotRelabelKnownUseAsUnused(t *testing.T) {
	service := query.NewMessageFlow(newMessageFlowFixture())
	flow, err := service.Flow(context.Background(), "acme.v1.Envelope", query.MessageFlowOptions{Direction: query.Outgoing, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range flow.Members {
		if member.Field.Name != "receipt" {
			continue
		}
		if hasCoverageCategory(member.Gaps, query.UnusedMessageMember) ||
			!hasCoverageCategory(member.Gaps, string(query.ConsumedWithoutProducer)) {
			t.Fatalf("direction filter changed global member coverage: %#v", member)
		}
		if len(member.Consumers) != 0 {
			t.Fatalf("outgoing presentation retained consumers: %#v", member.Consumers)
		}
		return
	}
	t.Fatal("receipt member missing")
}

func TestMessageFlowComponentRetainsUnknownTransportThroughMatchingSource(t *testing.T) {
	repository := newMessageFlowFixture()
	operation := repository.nodes["n:send"]
	operation.OwnerFile = "vendor/generated_transport.go"
	operation.Properties["channel_status"] = "unknown"
	repository.nodes[operation.ID] = operation

	flow, err := query.NewMessageFlow(repository).Flow(context.Background(), "acme.v1.Envelope",
		query.MessageFlowOptions{Component: "client", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(flow.Sends) != 1 || len(flow.Sends[0].Sources) != 1 || flow.Sends[0].Sources[0].Component != "client" {
		t.Fatalf("component-matching source did not retain transport: %#v", flow.Sends)
	}
	if flow.Status != query.CoverageUnknown || !hasUncertainty(flow.Uncertainties, "dynamic_transport") ||
		len(flow.UnknownEvidence) != 1 || flow.UnknownEvidence[0].EdgeID != "e:carries-send" {
		t.Fatalf("retained unknown transport lost fail-closed evidence: %#v", flow)
	}
}

func addMessageFlowEdge(repository *catalogRepository, id, from, to string, kind graph.EdgeKind, properties map[string]string) {
	repository.edges = append(repository.edges, graph.Edge{ID: id, FactID: "f:" + id, FromID: from, ToID: to,
		Kind: kind, Properties: properties, Location: graph.Location{Path: "flow.go", Line: len(repository.edges) + 1}})
}

func evidenceIDs(evidence []query.FlowEvidence) []string {
	result := make([]string, 0, len(evidence))
	for _, item := range evidence {
		result = append(result, item.EdgeID)
	}
	return result
}

func hasCoverageCategory(gaps []query.CoverageGap, category query.CoverageCategory) bool {
	for _, gap := range gaps {
		if gap.Category == category {
			return true
		}
	}
	return false
}

func coverageNames(items []query.MessageCoverage) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.Message.QualifiedName)
	}
	return result
}

func newMessageFlowFixture() *catalogRepository {
	repository := &catalogRepository{nodes: map[string]graph.Node{}, owners: map[string]string{}, repositories: []string{"transport", "uncertain"}}
	add := func(owner string, node graph.Node) { repository.add(owner, node) }
	edge := func(id, fact, from, to string, kind graph.EdgeKind, properties map[string]string) {
		repository.edges = append(repository.edges, graph.Edge{ID: id, FactID: fact, FromID: from, ToID: to,
			Kind: kind, Properties: properties, Location: graph.Location{Path: "flow.go", Line: len(repository.edges) + 1}})
	}

	add("transport", graph.Node{ID: "n:component-client", Kind: graph.KindComponent, Name: "client", QualifiedName: "component:client"})
	add("transport", graph.Node{ID: "n:component-server", Kind: graph.KindComponent, Name: "server", QualifiedName: "component:server"})
	add("transport", graph.Node{ID: "n:file-client", Kind: graph.KindFile, Name: "client.go", QualifiedName: "client.go", Location: graph.Location{Path: "client.go"}})
	add("transport", graph.Node{ID: "n:file-server", Kind: graph.KindFile, Name: "server.go", QualifiedName: "server.go", Location: graph.Location{Path: "server.go"}})
	edge("e:owns-client", "f:owns-client", "n:component-client", "n:file-client", graph.EdgeContains, nil)
	edge("e:owns-server", "f:owns-server", "n:component-server", "n:file-server", graph.EdgeContains, nil)

	add("transport", graph.Node{ID: "n:message", Kind: graph.KindType, Name: "Envelope", QualifiedName: "acme.v1.Envelope",
		OwnerFile: "schema.proto", Location: graph.Location{Path: "schema.proto", Line: 1}, Properties: map[string]string{"declaration": "message"}})
	for _, field := range []graph.Node{
		{ID: "n:text", Kind: graph.KindField, Name: "text", QualifiedName: "acme.v1.Envelope.text", OwnerFile: "schema.proto", Properties: map[string]string{"oneof": "payload"}},
		{ID: "n:image", Kind: graph.KindField, Name: "image", QualifiedName: "acme.v1.Envelope.image", OwnerFile: "schema.proto", Properties: map[string]string{"oneof": "payload"}},
		{ID: "n:receipt", Kind: graph.KindField, Name: "receipt", QualifiedName: "acme.v1.Envelope.receipt", OwnerFile: "schema.proto", Properties: map[string]string{"oneof": "payload"}},
		{ID: "n:unused", Kind: graph.KindField, Name: "unused", QualifiedName: "acme.v1.Envelope.unused", OwnerFile: "schema.proto", Properties: map[string]string{"oneof": "payload"}},
	} {
		add("transport", field)
		edge("e:field-"+field.Name, "f:field-"+field.Name, "n:message", field.ID, graph.EdgeHasField, nil)
	}
	add("transport", graph.Node{ID: "n:binding-go", Kind: graph.KindType, Name: "Envelope", QualifiedName: "gen.Envelope", OwnerFile: "generated.pb.go"})
	add("transport", graph.Node{ID: "n:binding-gd", Kind: graph.KindClass, Name: "Envelope", QualifiedName: "EnvelopeMessage", OwnerFile: "generated.gd"})
	edge("e:binding-go", "f:binding-go", "n:binding-go", "n:message", graph.EdgeGeneratedFrom, map[string]string{"generator": "protoc-gen-go"})
	edge("e:binding-gd", "f:binding-gd", "n:binding-gd", "n:message", graph.EdgeGeneratedFrom, map[string]string{"generator": "protoc-gen-gdscript"})

	add("transport", graph.Node{ID: "n:build", Kind: graph.KindFunction, Name: "Build", QualifiedName: "client.Build", OwnerFile: "client.go"})
	add("transport", graph.Node{ID: "n:decode", Kind: graph.KindFunction, Name: "Decode", QualifiedName: "server.Decode", OwnerFile: "server.go"})
	add("transport", graph.Node{ID: "n:handle", Kind: graph.KindFunction, Name: "Handle", QualifiedName: "server.Handle", OwnerFile: "server.go"})
	edge("e:write-text", "f:write-text", "n:build", "n:text", graph.EdgeWrites, map[string]string{"protocol": "protobuf"})
	edge("e:read-text", "f:read-text", "n:handle", "n:text", graph.EdgeReads, map[string]string{"protocol": "protobuf"})
	edge("e:write-image", "f:write-image", "n:build", "n:image", graph.EdgeWrites, map[string]string{"protocol": "protobuf"})
	edge("e:read-receipt", "f:read-receipt", "n:handle", "n:receipt", graph.EdgeReads, map[string]string{"protocol": "protobuf"})
	edge("e:encode", "f:encode", "n:build", "n:message", graph.EdgeEncodes, map[string]string{"protocol": "protobuf"})
	edge("e:decode", "f:decode", "n:decode", "n:message", graph.EdgeDecodes, map[string]string{"protocol": "protobuf"})
	edge("e:call-decode", "f:call-decode", "n:handle", "n:decode", graph.EdgeCalls, nil)

	add("transport", graph.Node{ID: "n:send", Kind: graph.KindTransportOperation, Name: "send", QualifiedName: "client.go:20:2:send",
		OwnerFile: "client.go", Properties: map[string]string{"direction": "send", "api": "Peer.SendBytes", "channel": "3", "channel_status": "proven", "reliability": "reliable", "payload_status": "proven"}})
	add("transport", graph.Node{ID: "n:receive", Kind: graph.KindTransportOperation, Name: "receive", QualifiedName: "server.go:20:2:receive",
		OwnerFile: "server.go", Properties: map[string]string{"direction": "receive", "api": "Packet.GetData", "channel": "3", "channel_status": "proven", "reliability": "reliable", "payload_status": "proven"}})
	edge("e:sends", "f:sends", "n:build", "n:send", graph.EdgeSends, map[string]string{"protocol": "enet"})
	edge("e:receives", "f:receives", "n:decode", "n:receive", graph.EdgeReceives, map[string]string{"protocol": "enet"})
	edge("e:carries-send", "f:carries-send", "n:send", "n:message", graph.EdgeCarries, map[string]string{"protocol": "enet"})
	edge("e:carries-receive", "f:carries-receive", "n:receive", "n:message", graph.EdgeCarries, map[string]string{"protocol": "enet"})

	add("transport", graph.Node{ID: "n:mismatch", Kind: graph.KindType, Name: "Mismatch", QualifiedName: "acme.v1.Mismatch",
		OwnerFile: "schema.proto", Properties: map[string]string{"declaration": "message"}})
	add("transport", graph.Node{ID: "n:mismatch-binding", Kind: graph.KindType, Name: "Mismatch", QualifiedName: "gen.Mismatch",
		OwnerFile: "generated.pb.go", Properties: map[string]string{"generator": "protoc-gen-go"}})
	edge("e:mismatch-binding", "f:mismatch-binding", "n:mismatch-binding", "n:mismatch", graph.EdgeGeneratedFrom, nil)
	add("transport", graph.Node{ID: "n:mismatch-field", Kind: graph.KindField, Name: "value", QualifiedName: "acme.v1.Mismatch.value", OwnerFile: "schema.proto"})
	edge("e:mismatch-field", "f:mismatch-field", "n:mismatch", "n:mismatch-field", graph.EdgeHasField, nil)
	edge("e:mismatch-write", "f:mismatch-write", "n:build", "n:mismatch-field", graph.EdgeWrites, map[string]string{"protocol": "protobuf"})
	edge("e:mismatch-read", "f:mismatch-read", "n:handle", "n:mismatch-field", graph.EdgeReads, map[string]string{"protocol": "protobuf"})
	edge("e:mismatch-encode", "f:mismatch-encode", "n:build", "n:mismatch", graph.EdgeEncodes, map[string]string{"protocol": "protobuf"})
	edge("e:mismatch-decode", "f:mismatch-decode", "n:decode", "n:mismatch", graph.EdgeDecodes, map[string]string{"protocol": "protobuf"})
	add("transport", graph.Node{ID: "n:mismatch-send", Kind: graph.KindTransportOperation, Name: "send", QualifiedName: "client.go:30:2:send", OwnerFile: "client.go", Properties: map[string]string{"direction": "send", "channel": "4", "channel_status": "proven", "payload_status": "proven"}})
	add("transport", graph.Node{ID: "n:mismatch-receive", Kind: graph.KindTransportOperation, Name: "receive", QualifiedName: "server.go:30:2:receive", OwnerFile: "server.go", Properties: map[string]string{"direction": "receive", "channel": "5", "channel_status": "proven", "payload_status": "proven"}})
	edge("e:mismatch-sends", "f:mismatch-sends", "n:build", "n:mismatch-send", graph.EdgeSends, nil)
	edge("e:mismatch-receives", "f:mismatch-receives", "n:decode", "n:mismatch-receive", graph.EdgeReceives, nil)
	edge("e:mismatch-carries-send", "f:mismatch-carries-send", "n:mismatch-send", "n:mismatch", graph.EdgeCarries, nil)
	edge("e:mismatch-carries-receive", "f:mismatch-carries-receive", "n:mismatch-receive", "n:mismatch", graph.EdgeCarries, nil)

	add("uncertain", graph.Node{ID: "n:dynamic-message", Kind: graph.KindType, Name: "Dynamic", QualifiedName: "acme.v1.Dynamic", OwnerFile: "dynamic.proto", Properties: map[string]string{"declaration": "message"}})
	add("uncertain", graph.Node{ID: "n:dynamic-build", Kind: graph.KindFunction, Name: "Build", QualifiedName: "dynamic.Build", OwnerFile: "dynamic.go"})
	add("uncertain", graph.Node{ID: "n:dynamic-op", Kind: graph.KindTransportOperation, Name: "send", QualifiedName: "dynamic.go:8:2:send", OwnerFile: "dynamic.go", Properties: map[string]string{"direction": "send", "channel_status": "unknown", "payload_status": "unknown"}})
	edge("e:dynamic-encode", "f:dynamic-encode", "n:dynamic-build", "n:dynamic-message", graph.EdgeEncodes, nil)
	edge("e:dynamic-send", "f:dynamic-send", "n:dynamic-build", "n:dynamic-op", graph.EdgeSends, nil)

	return repository
}
