package query

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

// CoverageStatus distinguishes complete evidence from proven gaps and cases
// where the graph cannot safely prove that evidence is absent.
type CoverageStatus string

const (
	CoverageResolved        CoverageStatus = "resolved"
	CoverageMissingEvidence CoverageStatus = "missing_evidence"
	CoverageUnknown         CoverageStatus = "unknown"
)

// CoverageCategory names a concrete message-flow gap.
type CoverageCategory = string

const (
	ProducedWithoutConsumer CoverageCategory = "produced_without_consumer"
	EncodedWithoutSend      CoverageCategory = "encoded_without_send"
	ReceivedWithoutDecode   CoverageCategory = "received_without_decode"
	ChannelMismatch         CoverageCategory = "channel_mismatch"
	UnusedMessageMember     CoverageCategory = "unused_message_member"
	UnknownEvidence         CoverageCategory = "unknown_evidence"
)

type MessageFlowOptions struct {
	Repository string    `json:"repository,omitempty"`
	Component  string    `json:"component,omitempty"`
	Direction  Direction `json:"direction,omitempty"`
	Limit      int       `json:"limit,omitempty"`
}

type MessageCoverageOptions struct {
	Repository string         `json:"repository,omitempty"`
	Package    string         `json:"package,omitempty"`
	Message    string         `json:"message,omitempty"`
	Oneof      string         `json:"oneof,omitempty"`
	Direction  Direction      `json:"direction,omitempty"`
	Component  string         `json:"component,omitempty"`
	Status     CoverageStatus `json:"status,omitempty"`
	Limit      int            `json:"limit,omitempty"`
}

// FlowEvidence retains the exact fact and graph edge behind one result.
type FlowEvidence struct {
	Repository  string            `json:"repository,omitempty"`
	Component   string            `json:"component,omitempty"`
	ComponentID string            `json:"component_id,omitempty"`
	Relation    graph.EdgeKind    `json:"relation"`
	EdgeID      string            `json:"edge_id"`
	FactID      string            `json:"fact_id,omitempty"`
	Node        Resource          `json:"node"`
	Location    graph.Location    `json:"location,omitempty"`
	Properties  map[string]string `json:"properties,omitempty"`
}

type TransportFlow struct {
	Evidence    FlowEvidence   `json:"evidence"`
	Direction   string         `json:"direction"`
	API         string         `json:"api,omitempty"`
	Channel     string         `json:"channel,omitempty"`
	Reliability string         `json:"reliability,omitempty"`
	Status      CoverageStatus `json:"status"`
	Sources     []FlowEvidence `json:"sources"`
}

type MessageHandler struct {
	Evidence FlowEvidence   `json:"evidence"`
	Callers  []FlowEvidence `json:"callers"`
}

type CoverageGap struct {
	Category CoverageCategory `json:"category"`
	Message  string           `json:"message,omitempty"`
	Evidence []FlowEvidence   `json:"evidence,omitempty"`
}

type ChannelConflict struct {
	Send    TransportFlow `json:"send"`
	Receive TransportFlow `json:"receive"`
}

type FlowUncertainty struct {
	Reason     string         `json:"reason"`
	Message    string         `json:"message"`
	Evidence   []FlowEvidence `json:"evidence"`
	Candidates []Resource     `json:"candidates,omitempty"`
}

type MessageMemberFlow struct {
	Field     Resource       `json:"field"`
	Oneof     string         `json:"oneof,omitempty"`
	Status    CoverageStatus `json:"status"`
	Producers []FlowEvidence `json:"producers"`
	Consumers []FlowEvidence `json:"consumers"`
	Gaps      []CoverageGap  `json:"gaps"`
}

type MessageFlow struct {
	Message           Resource            `json:"message"`
	Status            CoverageStatus      `json:"status"`
	Bindings          []FlowEvidence      `json:"bindings"`
	Members           []MessageMemberFlow `json:"members"`
	Encoders          []FlowEvidence      `json:"encoders"`
	Decoders          []FlowEvidence      `json:"decoders"`
	Sends             []TransportFlow     `json:"sends"`
	Receives          []TransportFlow     `json:"receives"`
	Handlers          []MessageHandler    `json:"handlers"`
	Gaps              []CoverageGap       `json:"gaps"`
	UnknownEvidence   []FlowEvidence      `json:"unknown_evidence"`
	Uncertainties     []FlowUncertainty   `json:"uncertainties"`
	ChannelMismatches []ChannelConflict   `json:"channel_mismatches"`
	Truncated         bool                `json:"truncated"`
}

type MessageCoverage struct {
	Message         Resource            `json:"message"`
	Status          CoverageStatus      `json:"status"`
	Gaps            []CoverageGap       `json:"gaps"`
	UnknownEvidence []FlowEvidence      `json:"unknown_evidence"`
	Uncertainties   []FlowUncertainty   `json:"uncertainties"`
	Members         []MessageMemberFlow `json:"members"`
	Truncated       bool                `json:"truncated"`
}

type MessageCoverageList struct {
	Messages  []MessageCoverage `json:"messages"`
	Truncated bool              `json:"truncated"`
}

type MessageFlowService struct {
	repository graph.TopologyRepository
}

// MessageFlowAttempt preserves one selector's result or error in batch order.
// Error stays adapter-neutral so callers can retain typed ambiguity candidates.
type MessageFlowAttempt struct {
	Flow  MessageFlow
	Error error
}

func NewMessageFlow(repository graph.TopologyRepository) *MessageFlowService {
	return &MessageFlowService{repository: repository}
}

func (s *MessageFlowService) Flow(ctx context.Context, selector string, options MessageFlowOptions) (MessageFlow, error) {
	attempts := s.Flows(ctx, []string{selector}, options)
	return attempts[0].Flow, attempts[0].Error
}

// Flows evaluates several selectors against one bounded component snapshot.
// Results preserve selector order and isolate errors per selector.
func (s *MessageFlowService) Flows(ctx context.Context, selectors []string, options MessageFlowOptions) []MessageFlowAttempt {
	result := make([]MessageFlowAttempt, len(selectors))
	initializeError := func(err error) []MessageFlowAttempt {
		for index := range result {
			result[index].Error = err
		}
		return result
	}
	if _, err := messageFlowLimit(options.Limit); err != nil {
		return initializeError(err)
	}
	if options.Direction != "" && options.Direction != Both && options.Direction != Incoming && options.Direction != Outgoing {
		return initializeError(fmt.Errorf("unknown message-flow direction %q", options.Direction))
	}
	components, truncated, err := s.componentIndex(ctx, "")
	if err != nil {
		return initializeError(err)
	}
	for index, selector := range selectors {
		if err := ctx.Err(); err != nil {
			result[index].Error = err
			continue
		}
		result[index].Flow, result[index].Error = s.flow(ctx, selector, options, components, truncated)
	}
	return result
}

func (s *MessageFlowService) flow(ctx context.Context, selector string, options MessageFlowOptions,
	components map[string]componentIdentity, componentsTruncated bool,
) (MessageFlow, error) {
	limit, err := messageFlowLimit(options.Limit)
	if err != nil {
		return MessageFlow{}, err
	}
	if options.Direction == "" {
		options.Direction = Both
	}
	if options.Direction != Both && options.Direction != Incoming && options.Direction != Outgoing {
		return MessageFlow{}, fmt.Errorf("unknown message-flow direction %q", options.Direction)
	}
	scoped, err := s.resolveMessage(ctx, selector, options.Repository)
	if err != nil {
		return MessageFlow{}, err
	}
	flow := MessageFlow{
		Message: newResource(scoped), Status: CoverageResolved, Bindings: []FlowEvidence{}, Members: []MessageMemberFlow{},
		Encoders: []FlowEvidence{}, Decoders: []FlowEvidence{}, Sends: []TransportFlow{}, Receives: []TransportFlow{},
		Handlers: []MessageHandler{}, Gaps: []CoverageGap{}, UnknownEvidence: []FlowEvidence{}, Uncertainties: []FlowUncertainty{},
		ChannelMismatches: []ChannelConflict{}, Truncated: componentsTruncated,
	}
	load := func(direction graph.RelationDirection, relations ...graph.EdgeKind) (graph.RelationEdgePage, error) {
		page, err := s.repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: scoped.Node.ID, Direction: direction, Relations: relations, Limit: limit})
		flow.Truncated = flow.Truncated || page.Truncated
		return page, err
	}
	bindings, err := load(graph.IncomingRelations, graph.EdgeGeneratedFrom)
	if err != nil {
		return MessageFlow{}, err
	}
	flow.Bindings = evidenceFromPage(bindings, scoped.Repository, components)
	if len(flow.Bindings) == 0 {
		flow.Uncertainties = append(flow.Uncertainties, FlowUncertainty{Reason: "binding_evidence_missing",
			Message: "no supported generated binding evidence is indexed for this canonical message", Evidence: []FlowEvidence{}})
	}
	for _, binding := range flow.Bindings {
		generator := binding.Node.Properties["generator"]
		if generator == "" {
			generator = binding.Properties["generator"]
		}
		if binding.Node.Unresolved {
			flow.Uncertainties = append(flow.Uncertainties, FlowUncertainty{Reason: "unresolved_binding",
				Message: "a binding projection remains unresolved", Evidence: []FlowEvidence{binding}})
		} else if generator != "protoc-gen-go" && generator != "protoc-gen-gdscript" {
			flow.Uncertainties = append(flow.Uncertainties, FlowUncertainty{Reason: "unsupported_generator",
				Message: "the binding generator is missing or has no message-flow adapter", Evidence: []FlowEvidence{binding}})
		}
	}
	codecs, err := load(graph.IncomingRelations, graph.EdgeEncodes, graph.EdgeDecodes)
	if err != nil {
		return MessageFlow{}, err
	}
	for _, evidence := range evidenceFromPage(codecs, scoped.Repository, components) {
		switch evidence.Relation {
		case graph.EdgeEncodes:
			flow.Encoders = append(flow.Encoders, evidence)
		case graph.EdgeDecodes:
			flow.Decoders = append(flow.Decoders, evidence)
		}
	}
	fields, err := load(graph.OutgoingRelations, graph.EdgeHasField)
	if err != nil {
		return MessageFlow{}, err
	}
	for _, item := range fields.Items {
		member := MessageMemberFlow{Field: newResource(graph.ScopedNode{Repository: scoped.Repository, Node: item.Counterpart}),
			Oneof: item.Counterpart.Properties["oneof"], Status: CoverageResolved, Producers: []FlowEvidence{}, Consumers: []FlowEvidence{}, Gaps: []CoverageGap{}}
		uses, err := s.repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: item.Counterpart.ID, Direction: graph.IncomingRelations,
			Relations: []graph.EdgeKind{graph.EdgeWrites, graph.EdgeReads}, Limit: limit})
		if err != nil {
			return MessageFlow{}, err
		}
		flow.Truncated = flow.Truncated || uses.Truncated
		for _, evidence := range evidenceFromPage(uses, scoped.Repository, components) {
			switch evidence.Relation {
			case graph.EdgeWrites:
				member.Producers = append(member.Producers, evidence)
			case graph.EdgeReads:
				member.Consumers = append(member.Consumers, evidence)
			}
		}
		classifyMember(&member, flow.Truncated)
		filterEvidenceByDirection(&member, options.Direction)
		flow.Members = append(flow.Members, member)
	}
	carriers, err := load(graph.IncomingRelations, graph.EdgeCarries)
	if err != nil {
		return MessageFlow{}, err
	}
	for _, item := range carriers.Items {
		transport, err := s.transport(ctx, item, scoped.Repository, components, limit)
		if err != nil {
			return MessageFlow{}, err
		}
		flow.Truncated = flow.Truncated || transport.truncated
		if transport.flow.Status == CoverageUnknown {
			flow.UnknownEvidence = append(flow.UnknownEvidence, transport.flow.Evidence)
			flow.Uncertainties = append(flow.Uncertainties, FlowUncertainty{Reason: "dynamic_transport",
				Message: "transport channel or payload evidence is not proven", Evidence: []FlowEvidence{transport.flow.Evidence}})
		}
		if transport.flow.Direction == "send" {
			flow.Sends = append(flow.Sends, transport.flow)
		}
		if transport.flow.Direction == "receive" {
			flow.Receives = append(flow.Receives, transport.flow)
		}
	}
	// Dynamic payload operations have no carries edge. They can only be
	// associated with this message through the callable that encodes/decodes it.
	seenCodecDirection := map[string]bool{}
	for _, codec := range append(append([]FlowEvidence{}, flow.Encoders...), flow.Decoders...) {
		relation := graph.EdgeSends
		if codec.Relation == graph.EdgeDecodes {
			relation = graph.EdgeReceives
		}
		key := codec.Node.ID + "\x00" + string(relation)
		if seenCodecDirection[key] {
			continue
		}
		seenCodecDirection[key] = true
		page, err := s.repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: codec.Node.ID, Direction: graph.OutgoingRelations,
			Relations: []graph.EdgeKind{relation}, Limit: limit})
		if err != nil {
			return MessageFlow{}, err
		}
		flow.Truncated = flow.Truncated || page.Truncated
		for _, item := range page.Items {
			if item.Counterpart.Properties["payload_status"] == "proven" || transportPresent(flow, item.Counterpart.ID) {
				continue
			}
			evidence := evidenceFromItem(item, scoped.Repository, components)
			flow.UnknownEvidence = append(flow.UnknownEvidence, evidence)
			flow.Uncertainties = append(flow.Uncertainties, FlowUncertainty{Reason: "dynamic_transport",
				Message: "transport payload provenance is not proven", Evidence: []FlowEvidence{evidence}})
		}
	}
	if options.Direction == Outgoing {
		flow.Decoders = []FlowEvidence{}
		flow.Receives = []TransportFlow{}
	}
	if options.Direction == Incoming {
		flow.Encoders = []FlowEvidence{}
		flow.Sends = []TransportFlow{}
	}
	handlers, truncated, err := s.handlers(ctx, append(append([]FlowEvidence{}, flow.Decoders...), memberConsumers(flow.Members)...), scoped.Repository, components, limit)
	if err != nil {
		return MessageFlow{}, err
	}
	flow.Handlers = handlers
	flow.Truncated = flow.Truncated || truncated
	if flow.Truncated {
		flow.Uncertainties = append(flow.Uncertainties, FlowUncertainty{Reason: "truncated",
			Message: "one or more bounded evidence relations reached the requested limit", Evidence: []FlowEvidence{}})
	}
	if hasFlowUncertainty(flow.Uncertainties, "binding_evidence_missing", "unresolved_binding", "unsupported_generator") {
		for index := range flow.Members {
			classifyMember(&flow.Members[index], true)
		}
	}
	flow.ChannelMismatches = channelConflicts(flow.Sends, flow.Receives)
	classifyFlow(&flow)
	filterFlowComponent(&flow, options.Component)
	sortMessageFlow(&flow)
	return flow, nil
}

func (s *MessageFlowService) Coverage(ctx context.Context, options MessageCoverageOptions) (MessageCoverageList, error) {
	limit, err := messageFlowLimit(options.Limit)
	if err != nil {
		return MessageCoverageList{}, err
	}
	if options.Direction == "" {
		options.Direction = Both
	}
	if options.Direction != Both && options.Direction != Incoming && options.Direction != Outgoing {
		return MessageCoverageList{}, fmt.Errorf("unknown message-flow direction %q", options.Direction)
	}
	if options.Status != "" && options.Status != CoverageResolved && options.Status != CoverageMissingEvidence && options.Status != CoverageUnknown {
		return MessageCoverageList{}, fmt.Errorf("unknown message coverage status %q", options.Status)
	}
	nodes, err := s.repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindType},
		Name: options.Message, Repository: options.Repository, Visibility: graph.LocalNodes, Limit: MaxCatalogLimit + 1})
	if err != nil {
		return MessageCoverageList{}, err
	}
	result := MessageCoverageList{Messages: []MessageCoverage{}, Truncated: len(nodes) > MaxCatalogLimit}
	if len(nodes) > MaxCatalogLimit {
		nodes = nodes[:MaxCatalogLimit]
	}
	components, componentsTruncated, err := s.componentIndex(ctx, "")
	if err != nil {
		return MessageCoverageList{}, err
	}
	for _, scoped := range nodes {
		if scoped.Node.Properties["declaration"] != "message" || !messageNameMatches(scoped.Node, options.Package, options.Message) {
			continue
		}
		flow, err := s.flow(ctx, scoped.Node.ID, MessageFlowOptions{Repository: scoped.Repository, Component: options.Component, Direction: options.Direction, Limit: limit},
			components, componentsTruncated)
		if err != nil {
			return MessageCoverageList{}, err
		}
		if options.Component != "" && !hasApplicationEvidence(flow) {
			continue
		}
		members := flow.Members
		if options.Oneof != "" {
			members = nil
			for _, member := range flow.Members {
				if member.Oneof == options.Oneof {
					members = append(members, member)
				}
			}
			if len(members) == 0 {
				continue
			}
		}
		coverageGaps := append([]CoverageGap{}, flow.Gaps...)
		for _, member := range members {
			coverageGaps = append(coverageGaps, member.Gaps...)
		}
		item := MessageCoverage{Message: flow.Message, Status: flow.Status, Gaps: coverageGaps,
			UnknownEvidence: flow.UnknownEvidence, Uncertainties: flow.Uncertainties, Members: members, Truncated: flow.Truncated}
		if options.Oneof != "" {
			item.Status, item.Gaps = coverageForMembers(members, flow.Truncated || len(flow.Uncertainties) > 0, flow.UnknownEvidence)
		}
		if options.Status != "" && item.Status != options.Status {
			continue
		}
		result.Truncated = result.Truncated || item.Truncated
		result.Messages = append(result.Messages, item)
		if len(result.Messages) > limit {
			result.Truncated = true
			result.Messages = result.Messages[:limit]
			break
		}
	}
	sort.Slice(result.Messages, func(i, j int) bool {
		if result.Messages[i].Message.Repository != result.Messages[j].Message.Repository {
			return result.Messages[i].Message.Repository < result.Messages[j].Message.Repository
		}
		return result.Messages[i].Message.QualifiedName < result.Messages[j].Message.QualifiedName
	})
	return result, nil
}

func messageFlowLimit(value int) (int, error) {
	if value == 0 {
		return DefaultCatalogLimit, nil
	}
	if value < 0 || value > MaxCatalogLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", MaxCatalogLimit)
	}
	return value, nil
}

func (s *MessageFlowService) resolveMessage(ctx context.Context, selector, repository string) (graph.ScopedNode, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return graph.ScopedNode{}, fmt.Errorf("message selector is required")
	}
	node, err := NewService(s.repository).ResolveKindInRepository(ctx, selector, graph.KindType, repository)
	if err != nil {
		return graph.ScopedNode{}, err
	}
	if node.External || node.Properties["declaration"] != "message" {
		return graph.ScopedNode{}, fmt.Errorf("%q resolves to %s %q, not a canonical protocol message", selector, node.Kind, node.QualifiedName)
	}
	nodes, err := s.repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindType}, Name: node.QualifiedName,
		Repository: repository, Visibility: graph.LocalNodes, Limit: MaxCatalogLimit + 1})
	if err != nil {
		return graph.ScopedNode{}, err
	}
	for _, scoped := range nodes {
		if scoped.Node.ID == node.ID {
			return scoped, nil
		}
	}
	return graph.ScopedNode{Repository: repository, Node: node}, nil
}

type componentIdentity struct{ repository, name, id string }

func (s *MessageFlowService) componentIndex(ctx context.Context, repository string) (map[string]componentIdentity, bool, error) {
	nodes, err := s.repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: graph.NodeKinds(), Repository: repository,
		Visibility: graph.LocalNodes, Limit: MaxCatalogLimit + 1})
	if err != nil {
		return nil, false, err
	}
	truncated := false
	counts := map[graph.NodeKind]int{}
	components := []graph.ScopedNode{}
	kept := make([]graph.ScopedNode, 0, len(nodes))
	for _, scoped := range nodes {
		counts[scoped.Node.Kind]++
		if counts[scoped.Node.Kind] > MaxCatalogLimit {
			truncated = true
			continue
		}
		kept = append(kept, scoped)
		if scoped.Node.Kind == graph.KindComponent {
			components = append(components, scoped)
		}
	}
	fileOwners := map[string]componentIdentity{}
	for _, component := range components {
		page, err := s.repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: component.Node.ID, Direction: graph.OutgoingRelations, Relations: []graph.EdgeKind{graph.EdgeContains}, Limit: MaxCatalogLimit})
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || page.Truncated
		for _, item := range page.Items {
			identity := componentIdentity{repository: component.Repository, name: component.Node.Name, id: component.Node.ID}
			for _, key := range []string{item.Counterpart.ID, item.Counterpart.Name, item.Counterpart.QualifiedName, item.Counterpart.Location.Path} {
				if key != "" {
					fileOwners[component.Repository+"\x00"+key] = identity
				}
			}
		}
	}
	result := map[string]componentIdentity{}
	for _, scoped := range kept {
		identity := componentIdentity{repository: scoped.Repository}
		for _, key := range []string{scoped.Node.ID, scoped.Node.OwnerFile, scoped.Node.Location.Path} {
			if owner, ok := fileOwners[scoped.Repository+"\x00"+key]; ok {
				identity = owner
				break
			}
		}
		result[scoped.Node.ID] = identity
	}
	return result, truncated, nil
}

func evidenceFromPage(page graph.RelationEdgePage, repository string, components map[string]componentIdentity) []FlowEvidence {
	result := make([]FlowEvidence, 0, len(page.Items))
	for _, item := range page.Items {
		result = append(result, evidenceFromItem(item, repository, components))
	}
	sortEvidence(result)
	return result
}

func evidenceFromItem(item graph.HydratedRelationEdge, repository string, components map[string]componentIdentity) FlowEvidence {
	identity := components[item.Counterpart.ID]
	if identity.repository == "" {
		identity.repository = repository
	}
	resource := newResource(graph.ScopedNode{Repository: identity.repository, Node: item.Counterpart})
	resource.Component, resource.ComponentID = identity.name, identity.id
	return FlowEvidence{Repository: identity.repository, Component: identity.name, ComponentID: identity.id, Relation: item.Edge.Kind,
		EdgeID: item.Edge.ID, FactID: item.Edge.FactID, Node: resource, Location: item.Edge.Location, Properties: item.Edge.Properties}
}

type transportResult struct {
	flow      TransportFlow
	truncated bool
}

func (s *MessageFlowService) transport(ctx context.Context, carrier graph.HydratedRelationEdge, repository string, components map[string]componentIdentity, limit int) (transportResult, error) {
	op := carrier.Counterpart
	relation := graph.EdgeSends
	if op.Properties["direction"] == "receive" {
		relation = graph.EdgeReceives
	}
	page, err := s.repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: op.ID, Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{relation}, Limit: limit})
	if err != nil {
		return transportResult{}, err
	}
	evidence := evidenceFromItem(carrier, repository, components)
	evidence.Node = newResource(graph.ScopedNode{Repository: repository, Node: op})
	identity := components[op.ID]
	if identity.repository == "" {
		identity.repository = repository
	}
	evidence.Repository, evidence.Node.Repository = identity.repository, identity.repository
	evidence.Component, evidence.ComponentID = identity.name, identity.id
	evidence.Node.Component, evidence.Node.ComponentID = identity.name, identity.id
	status := CoverageResolved
	if op.External || op.Properties["payload_status"] != "proven" || op.Properties["channel_status"] != "proven" {
		status = CoverageUnknown
	}
	return transportResult{flow: TransportFlow{Evidence: evidence, Direction: op.Properties["direction"], API: op.Properties["api"], Channel: op.Properties["channel"],
		Reliability: op.Properties["reliability"], Status: status, Sources: evidenceFromPage(page, repository, components)}, truncated: page.Truncated}, nil
}

func (s *MessageFlowService) handlers(ctx context.Context, consumers []FlowEvidence, repository string, components map[string]componentIdentity, limit int) ([]MessageHandler, bool, error) {
	seen := map[string]bool{}
	result := []MessageHandler{}
	truncated := false
	for _, consumer := range consumers {
		if seen[consumer.Node.ID] {
			continue
		}
		seen[consumer.Node.ID] = true
		page, err := s.repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: consumer.Node.ID, Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls}, Limit: limit})
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || page.Truncated
		result = append(result, MessageHandler{Evidence: consumer, Callers: evidenceFromPage(page, repository, components)})
	}
	return result, truncated, nil
}

func classifyMember(member *MessageMemberFlow, uncertain bool) {
	member.Gaps = []CoverageGap{}
	if uncertain {
		member.Status = CoverageUnknown
		return
	}
	switch {
	case len(member.Producers) == 0 && len(member.Consumers) == 0:
		member.Gaps = append(member.Gaps, CoverageGap{Category: UnusedMessageMember})
	case len(member.Producers) > 0 && len(member.Consumers) == 0:
		member.Gaps = append(member.Gaps, CoverageGap{Category: ProducedWithoutConsumer, Evidence: member.Producers})
	case len(member.Producers) == 0 && len(member.Consumers) > 0:
		member.Gaps = append(member.Gaps, CoverageGap{Category: string(ConsumedWithoutProducer), Evidence: member.Consumers})
	}
	if len(member.Gaps) > 0 {
		member.Status = CoverageMissingEvidence
	} else {
		member.Status = CoverageResolved
	}
}

func classifyFlow(flow *MessageFlow) {
	flow.Gaps = []CoverageGap{}
	uncertain := flow.Truncated || len(flow.UnknownEvidence) > 0 || len(flow.Uncertainties) > 0
	if !uncertain && len(flow.Encoders) > 0 && len(flow.Sends) == 0 {
		flow.Gaps = append(flow.Gaps, CoverageGap{Category: EncodedWithoutSend, Evidence: flow.Encoders})
	}
	if !uncertain && len(flow.Receives) > 0 && len(flow.Decoders) == 0 {
		flow.Gaps = append(flow.Gaps, CoverageGap{Category: ReceivedWithoutDecode})
	}
	for _, conflict := range flow.ChannelMismatches {
		flow.Gaps = append(flow.Gaps, CoverageGap{Category: ChannelMismatch, Evidence: []FlowEvidence{conflict.Send.Evidence, conflict.Receive.Evidence}})
	}
	status := CoverageResolved
	for _, member := range flow.Members {
		if member.Status == CoverageMissingEvidence {
			status = CoverageMissingEvidence
		}
	}
	if len(flow.Gaps) > 0 {
		status = CoverageMissingEvidence
	}
	if uncertain {
		status = CoverageUnknown
	}
	flow.Status = status
}

func coverageForMembers(members []MessageMemberFlow, truncated bool, unknown []FlowEvidence) (CoverageStatus, []CoverageGap) {
	status := CoverageResolved
	gaps := []CoverageGap{}
	for _, member := range members {
		gaps = append(gaps, member.Gaps...)
		if member.Status == CoverageMissingEvidence {
			status = CoverageMissingEvidence
		}
		if member.Status == CoverageUnknown {
			status = CoverageUnknown
		}
	}
	if truncated || len(unknown) > 0 {
		status = CoverageUnknown
	}
	return status, gaps
}

func channelConflicts(sends, receives []TransportFlow) []ChannelConflict {
	result := []ChannelConflict{}
	for _, send := range sends {
		for _, receive := range receives {
			if send.Status != CoverageResolved || receive.Status != CoverageResolved || send.Channel == "" || receive.Channel == "" || send.Channel == receive.Channel {
				continue
			}
			result = append(result, ChannelConflict{Send: send, Receive: receive})
		}
	}
	return result
}

func hasFlowUncertainty(items []FlowUncertainty, reasons ...string) bool {
	wanted := map[string]bool{}
	for _, reason := range reasons {
		wanted[reason] = true
	}
	for _, item := range items {
		if wanted[item.Reason] {
			return true
		}
	}
	return false
}

func filterEvidenceByDirection(member *MessageMemberFlow, direction Direction) {
	if direction == Outgoing {
		member.Consumers = []FlowEvidence{}
	}
	if direction == Incoming {
		member.Producers = []FlowEvidence{}
	}
}
func memberConsumers(members []MessageMemberFlow) []FlowEvidence {
	result := []FlowEvidence{}
	for _, member := range members {
		result = append(result, member.Consumers...)
	}
	return result
}
func transportPresent(flow MessageFlow, id string) bool {
	for _, item := range append(flow.Sends, flow.Receives...) {
		if item.Evidence.Node.ID == id {
			return true
		}
	}
	return false
}

func filterFlowComponent(flow *MessageFlow, component string) {
	if component == "" {
		return
	}
	filter := func(items []FlowEvidence) []FlowEvidence {
		result := []FlowEvidence{}
		for _, item := range items {
			if item.Component == component || item.ComponentID == component {
				result = append(result, item)
			}
		}
		return result
	}
	flow.Encoders = filter(flow.Encoders)
	flow.Decoders = filter(flow.Decoders)
	flow.UnknownEvidence = filter(flow.UnknownEvidence)
	uncertainties := flow.Uncertainties[:0]
	for _, uncertainty := range flow.Uncertainties {
		if len(uncertainty.Evidence) == 0 || messageWideUncertainty(uncertainty.Reason) {
			uncertainties = append(uncertainties, uncertainty)
			continue
		}
		uncertainty.Evidence = filter(uncertainty.Evidence)
		if len(uncertainty.Evidence) > 0 {
			uncertainties = append(uncertainties, uncertainty)
		}
	}
	flow.Uncertainties = uncertainties
	for i := range flow.Members {
		flow.Members[i].Producers = filter(flow.Members[i].Producers)
		flow.Members[i].Consumers = filter(flow.Members[i].Consumers)
		classifyMember(&flow.Members[i], flow.Truncated || hasFlowUncertainty(flow.Uncertainties,
			"binding_evidence_missing", "unresolved_binding", "unsupported_generator"))
	}
	transports := func(items []TransportFlow) []TransportFlow {
		result := []TransportFlow{}
		for _, item := range items {
			item.Sources = filter(item.Sources)
			if item.Evidence.Component == component || item.Evidence.ComponentID == component || len(item.Sources) > 0 {
				result = append(result, item)
			}
		}
		return result
	}
	flow.Sends, flow.Receives = transports(flow.Sends), transports(flow.Receives)
	flow.Handlers = slicesDeleteFunc(flow.Handlers, func(item MessageHandler) bool {
		return item.Evidence.Component != component && item.Evidence.ComponentID != component
	})
	flow.ChannelMismatches = channelConflicts(flow.Sends, flow.Receives)
	classifyFlow(flow)
}

func messageWideUncertainty(reason string) bool {
	switch reason {
	case "binding_evidence_missing", "unresolved_binding", "unsupported_generator", "truncated":
		return true
	default:
		return false
	}
}

func hasApplicationEvidence(flow MessageFlow) bool {
	if len(flow.Encoders)+len(flow.Decoders)+len(flow.Sends)+len(flow.Receives)+len(flow.Handlers)+len(flow.UnknownEvidence) > 0 {
		return true
	}
	for _, member := range flow.Members {
		if len(member.Producers)+len(member.Consumers) > 0 {
			return true
		}
	}
	return false
}

func slicesDeleteFunc[T any](items []T, remove func(T) bool) []T {
	result := items[:0]
	for _, item := range items {
		if !remove(item) {
			result = append(result, item)
		}
	}
	return result
}

func messageNameMatches(node graph.Node, pkg, message string) bool {
	if pkg != "" && !strings.HasPrefix(node.QualifiedName, strings.TrimSuffix(pkg, ".")+".") {
		return false
	}
	if message != "" && node.Name != message && node.QualifiedName != message {
		return false
	}
	return true
}

func sortEvidence(items []FlowEvidence) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Repository != items[j].Repository {
			return items[i].Repository < items[j].Repository
		}
		if items[i].Node.QualifiedName != items[j].Node.QualifiedName {
			return items[i].Node.QualifiedName < items[j].Node.QualifiedName
		}
		return items[i].EdgeID < items[j].EdgeID
	})
}
func sortMessageFlow(flow *MessageFlow) {
	for _, items := range [][]FlowEvidence{flow.Bindings, flow.Encoders, flow.Decoders, flow.UnknownEvidence} {
		sortEvidence(items)
	}
	sort.Slice(flow.Members, func(i, j int) bool { return flow.Members[i].Field.QualifiedName < flow.Members[j].Field.QualifiedName })
	sort.Slice(flow.Sends, func(i, j int) bool { return flow.Sends[i].Evidence.EdgeID < flow.Sends[j].Evidence.EdgeID })
	sort.Slice(flow.Receives, func(i, j int) bool { return flow.Receives[i].Evidence.EdgeID < flow.Receives[j].Evidence.EdgeID })
	for index := range flow.Handlers {
		sortEvidence(flow.Handlers[index].Callers)
	}
	sort.Slice(flow.Handlers, func(i, j int) bool {
		if flow.Handlers[i].Evidence.Node.QualifiedName != flow.Handlers[j].Evidence.Node.QualifiedName {
			return flow.Handlers[i].Evidence.Node.QualifiedName < flow.Handlers[j].Evidence.Node.QualifiedName
		}
		return flow.Handlers[i].Evidence.EdgeID < flow.Handlers[j].Evidence.EdgeID
	})
	sort.Slice(flow.Uncertainties, func(i, j int) bool {
		if flow.Uncertainties[i].Reason != flow.Uncertainties[j].Reason {
			return flow.Uncertainties[i].Reason < flow.Uncertainties[j].Reason
		}
		left, right := "", ""
		if len(flow.Uncertainties[i].Evidence) > 0 {
			left = flow.Uncertainties[i].Evidence[0].EdgeID
		}
		if len(flow.Uncertainties[j].Evidence) > 0 {
			right = flow.Uncertainties[j].Evidence[0].EdgeID
		}
		return left < right
	})
	sort.Slice(flow.ChannelMismatches, func(i, j int) bool {
		left := flow.ChannelMismatches[i].Send.Evidence.EdgeID + "\x00" + flow.ChannelMismatches[i].Receive.Evidence.EdgeID
		right := flow.ChannelMismatches[j].Send.Evidence.EdgeID + "\x00" + flow.ChannelMismatches[j].Receive.Evidence.EdgeID
		return left < right
	})
}
