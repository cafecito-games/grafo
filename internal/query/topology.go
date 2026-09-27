package query

import (
	"context"
	"fmt"
	"html"
	"sort"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
)

// BoundaryStatus describes how confidently graph evidence crosses a service
// boundary. Missing means the declaration has no handler edge; unresolved
// means the edge still targets an external node; ambiguous means several exact
// graph candidates exist and none was selected.
type BoundaryStatus string

const (
	BoundaryResolved   BoundaryStatus = "resolved"
	BoundaryUnresolved BoundaryStatus = "unresolved"
	BoundaryAmbiguous  BoundaryStatus = "ambiguous"
	BoundaryMissing    BoundaryStatus = "missing"
)

// TopologyOptions filters and bounds endpoint, handler, request, and service
// topology queries. Repository always names the stable indexed service
// identity; labels are presentation-only.
type TopologyOptions struct {
	Repository string    `json:"repository,omitempty"`
	Method     string    `json:"method,omitempty"`
	Route      string    `json:"route,omitempty"`
	Event      string    `json:"event,omitempty"`
	Direction  Direction `json:"direction,omitempty"`
	Limit      int       `json:"limit,omitempty"`
}

// Endpoint is an HTTP declaration plus the graph evidence that exposes and
// handles it.
type Endpoint struct {
	Resource
	Method        string         `json:"method"`
	Route         string         `json:"route"`
	Exposers      []UsageSite    `json:"exposers"`
	Handlers      []UsageSite    `json:"handlers"`
	HandlerStatus BoundaryStatus `json:"handler_status"`
}

type EndpointList struct {
	Endpoints  []Endpoint `json:"endpoints"`
	Unresolved []Endpoint `json:"unresolved"`
	Truncated  bool       `json:"truncated"`
}

// OutboundRequest reports one underlying requests fact. Destination is filled
// only for one exact endpoint or for an unresolved external boundary. Several
// exact declarations are returned as Candidates and never silently selected.
type OutboundRequest struct {
	Source      Resource       `json:"source"`
	Method      string         `json:"method"`
	Route       string         `json:"route"`
	Status      BoundaryStatus `json:"status"`
	Target      Endpoint       `json:"target"`
	Destination Endpoint       `json:"destination,omitzero"`
	Candidates  []Endpoint     `json:"candidates"`
	Evidence    LinkEvidence   `json:"evidence"`
	Truncated   bool           `json:"truncated"`
}

type OutboundRequestList struct {
	Requests  []OutboundRequest `json:"requests"`
	Truncated bool              `json:"truncated"`
}

type HandlerKind string

const (
	HandlerHTTP  HandlerKind = "http"
	HandlerEvent HandlerKind = "event"
)

type HandlerMatch struct {
	Kind         HandlerKind    `json:"kind"`
	Subject      Resource       `json:"subject"`
	Method       string         `json:"method,omitempty"`
	Route        string         `json:"route,omitempty"`
	Event        string         `json:"event,omitempty"`
	Status       BoundaryStatus `json:"status"`
	Handlers     []UsageSite    `json:"handlers"`
	Repositories []string       `json:"repositories,omitempty"`
}

type HandlerList struct {
	Matches   []HandlerMatch `json:"matches"`
	Truncated bool           `json:"truncated"`
}

type ServiceLinkKind string

const (
	LinkHTTP  ServiceLinkKind = "http"
	LinkEvent ServiceLinkKind = "event"
)

// ServiceNode separates stable repository identity from its presentation
// label. External nodes are explicit unresolved or ambiguous destinations.
type ServiceNode struct {
	ID         string     `json:"id"`
	Repository string     `json:"repository,omitempty"`
	Label      string     `json:"label"`
	External   bool       `json:"external,omitempty"`
	Components []Resource `json:"components"`
}

// LinkEvidence is the complete graph relation used to construct a topology
// link. Structured IDs remain authoritative even when a caller renders a
// diagram.
type LinkEvidence struct {
	EdgeID     string            `json:"edge_id"`
	FactID     string            `json:"fact_id,omitempty"`
	Relation   graph.EdgeKind    `json:"relation"`
	FromID     string            `json:"from_id"`
	ToID       string            `json:"to_id"`
	Location   graph.Location    `json:"location,omitempty"`
	Federated  bool              `json:"federated,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
}

type ServiceLink struct {
	ID            string          `json:"id"`
	FromServiceID string          `json:"from_service_id"`
	ToServiceID   string          `json:"to_service_id"`
	Kind          ServiceLinkKind `json:"kind"`
	Name          string          `json:"name"`
	Status        BoundaryStatus  `json:"status"`
	Method        string          `json:"method,omitempty"`
	Route         string          `json:"route,omitempty"`
	Event         string          `json:"event,omitempty"`
	EndpointIDs   []string        `json:"endpoint_ids,omitempty"`
	EventIDs      []string        `json:"event_ids,omitempty"`
	EdgeIDs       []string        `json:"edge_ids"`
	FactIDs       []string        `json:"fact_ids,omitempty"`
	Federated     bool            `json:"federated,omitempty"`
	SourceNodes   []Resource      `json:"source_nodes"`
	TargetNodes   []Resource      `json:"target_nodes"`
	Evidence      []LinkEvidence  `json:"evidence"`
	Truncated     bool            `json:"truncated"`
}

type ServiceTopology struct {
	Services  []ServiceNode `json:"services"`
	Links     []ServiceLink `json:"links"`
	Truncated bool          `json:"truncated"`
}

// Topology owns endpoint and service-boundary interpretation. Storage only
// enumerates exact node kinds and graph adjacency.
type Topology struct {
	repository graph.CatalogRepository
	catalog    *Catalog
}

func NewTopology(repository graph.CatalogRepository) *Topology {
	return &Topology{repository: repository, catalog: NewCatalog(repository)}
}

func (t *Topology) normalize(ctx context.Context, options TopologyOptions) (TopologyOptions, int, error) {
	options.Repository = strings.TrimSpace(options.Repository)
	options.Method = strings.ToUpper(strings.TrimSpace(options.Method))
	options.Route = strings.TrimSpace(options.Route)
	options.Event = strings.TrimSpace(options.Event)
	options.Direction = Direction(strings.ToLower(strings.TrimSpace(string(options.Direction))))
	if options.Direction == "" {
		options.Direction = Both
	}
	if options.Direction != Incoming && options.Direction != Outgoing && options.Direction != Both {
		return options, 0, fmt.Errorf("invalid direction %q", options.Direction)
	}
	if options.Event != "" && (options.Method != "" || options.Route != "") {
		return options, 0, fmt.Errorf("event and HTTP method/route filters cannot be combined")
	}
	limit, err := t.catalog.bounds(ctx, CatalogOptions{Repository: options.Repository, Limit: options.Limit})
	return options, limit, err
}

func (t *Topology) scoped(ctx context.Context, kinds []graph.NodeKind, visibility graph.NodeVisibility) ([]graph.ScopedNode, error) {
	result, err := t.repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: kinds, Visibility: visibility})
	if err != nil {
		return nil, err
	}
	result = uniqueScopedNodes(result)
	sortScopedNodes(result)
	for index := range result {
		if result[index].Node.External {
			result[index].Repository = ""
		}
	}
	return result, nil
}

func (t *Topology) ownership(ctx context.Context) (map[string]string, error) {
	nodes, err := t.scoped(ctx, graph.NodeKinds(), graph.AllNodes)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]string, len(nodes))
	for _, scoped := range nodes {
		if !scoped.Node.External {
			owners[scoped.Node.ID] = scoped.Repository
		}
	}
	return owners, nil
}

func endpointMethodRoute(node graph.Node) (string, string) {
	method := strings.ToUpper(strings.TrimSpace(node.Properties["method"]))
	route := strings.TrimSpace(node.Properties["route"])
	if method == "" || route == "" {
		label := strings.TrimSpace(node.Name)
		// External endpoint nodes keep the full requested method and route in
		// QualifiedName, while Name is deliberately simplified for generic
		// symbol search (for example "charge").
		if node.External {
			label = strings.TrimSpace(node.QualifiedName)
		}
		if parsedMethod, parsedRoute, ok := strings.Cut(label, " "); ok {
			if method == "" {
				method = strings.ToUpper(strings.TrimSpace(parsedMethod))
			}
			if route == "" {
				route = strings.TrimSpace(parsedRoute)
			}
		}
	}
	return method, route
}

func endpointMatches(node graph.Node, options TopologyOptions) bool {
	method, route := endpointMethodRoute(node)
	if options.Method != "" && method != options.Method {
		return false
	}
	return options.Route == "" || strings.Contains(strings.ToLower(route), strings.ToLower(options.Route))
}

func (t *Topology) endpointReferencedByRepository(ctx context.Context, endpointID, repository string,
	owners map[string]string,
) (bool, error) {
	edges, err := t.repository.EdgesTo(ctx, endpointID)
	if err != nil {
		return false, err
	}
	for _, edge := range edges {
		if edge.Kind == graph.EdgeRequests && owners[edge.FromID] == repository {
			return true, nil
		}
	}
	return false, nil
}

func eventMatches(node graph.Node, event string) bool {
	if event == "" {
		return true
	}
	needle := strings.ToLower(event)
	return strings.Contains(strings.ToLower(node.Name), needle) ||
		strings.Contains(strings.ToLower(node.QualifiedName), needle)
}

func eventName(node graph.Node) string {
	if node.External && strings.TrimSpace(node.QualifiedName) != "" {
		return strings.TrimSpace(node.QualifiedName)
	}
	if strings.TrimSpace(node.Name) != "" {
		return strings.TrimSpace(node.Name)
	}
	return strings.TrimSpace(node.QualifiedName)
}

func (t *Topology) endpoint(ctx context.Context, scoped graph.ScopedNode, limit int) (Endpoint, bool, error) {
	method, route := endpointMethodRoute(scoped.Node)
	exposers, exposedTruncated, err := t.catalog.incoming(ctx, scoped.Node.ID, limit, graph.EdgeExposes)
	if err != nil {
		return Endpoint{}, false, err
	}
	handlers, handlersTruncated, err := t.catalog.outgoing(ctx, scoped.Node.ID, limit, graph.EdgeHandledBy)
	if err != nil {
		return Endpoint{}, false, err
	}
	status := handlerStatus(handlers, handlersTruncated)
	return Endpoint{Resource: newResource(scoped), Method: method, Route: route,
		Exposers: exposers, Handlers: handlers, HandlerStatus: status}, exposedTruncated || handlersTruncated, nil
}

func handlerStatus(handlers []UsageSite, truncated bool) BoundaryStatus {
	if truncated {
		return BoundaryAmbiguous
	}
	byID := map[string]graph.Node{}
	for _, handler := range handlers {
		byID[handler.Node.ID] = handler.Node
	}
	if len(byID) == 0 {
		return BoundaryMissing
	}
	if len(byID) > 1 {
		return BoundaryAmbiguous
	}
	for _, handler := range byID {
		if handler.External {
			return BoundaryUnresolved
		}
	}
	return BoundaryResolved
}

func sortEndpoints(endpoints []Endpoint) {
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Method != endpoints[j].Method {
			return endpoints[i].Method < endpoints[j].Method
		}
		if endpoints[i].Route != endpoints[j].Route {
			return endpoints[i].Route < endpoints[j].Route
		}
		if endpoints[i].Repository != endpoints[j].Repository {
			return endpoints[i].Repository < endpoints[j].Repository
		}
		return endpoints[i].ID < endpoints[j].ID
	})
}

func (t *Topology) Endpoints(ctx context.Context, options TopologyOptions) (EndpointList, error) {
	if options.Direction != "" {
		return EndpointList{}, fmt.Errorf("a direction filter applies only to service topology")
	}
	options, limit, err := t.normalize(ctx, options)
	if err != nil {
		return EndpointList{}, err
	}
	if options.Event != "" {
		return EndpointList{}, fmt.Errorf("an event filter does not apply to endpoint listings")
	}
	var owners map[string]string
	if options.Repository != "" {
		owners, err = t.ownership(ctx)
		if err != nil {
			return EndpointList{}, err
		}
	}
	result := EndpointList{Endpoints: []Endpoint{}, Unresolved: []Endpoint{}}
	for _, visibility := range []graph.NodeVisibility{graph.LocalNodes, graph.ExternalNodes} {
		nodes, err := t.scoped(ctx, []graph.NodeKind{graph.KindEndpoint}, visibility)
		if err != nil {
			return EndpointList{}, err
		}
		for _, scoped := range nodes {
			if options.Repository != "" {
				if scoped.Node.External {
					referenced, err := t.endpointReferencedByRepository(ctx, scoped.Node.ID, options.Repository, owners)
					if err != nil {
						return EndpointList{}, err
					}
					if !referenced {
						continue
					}
				} else if scoped.Repository != options.Repository {
					continue
				}
			}
			if !endpointMatches(scoped.Node, options) {
				continue
			}
			endpoint, truncated, err := t.endpoint(ctx, scoped, limit)
			if err != nil {
				return EndpointList{}, err
			}
			result.Truncated = result.Truncated || truncated
			if scoped.Node.External {
				result.Unresolved = append(result.Unresolved, endpoint)
			} else {
				result.Endpoints = append(result.Endpoints, endpoint)
			}
		}
	}
	sortEndpoints(result.Endpoints)
	sortEndpoints(result.Unresolved)
	if len(result.Endpoints) > limit {
		result.Endpoints, result.Truncated = result.Endpoints[:limit], true
	}
	if len(result.Unresolved) > limit {
		result.Unresolved, result.Truncated = result.Unresolved[:limit], true
	}
	return result, nil
}

func handlerRepositories(subject Resource, handlers []UsageSite, owners map[string]string) []string {
	set := map[string]bool{}
	if subject.Repository != "" {
		set[subject.Repository] = true
	}
	for _, handler := range handlers {
		if repository := owners[handler.Node.ID]; repository != "" {
			set[repository] = true
		}
	}
	result := make([]string, 0, len(set))
	for repository := range set {
		result = append(result, repository)
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (t *Topology) Handlers(ctx context.Context, options TopologyOptions) (HandlerList, error) {
	if options.Direction != "" {
		return HandlerList{}, fmt.Errorf("a direction filter applies only to service topology")
	}
	options, limit, err := t.normalize(ctx, options)
	if err != nil {
		return HandlerList{}, err
	}
	owners, err := t.ownership(ctx)
	if err != nil {
		return HandlerList{}, err
	}
	result := HandlerList{Matches: []HandlerMatch{}}
	if options.Event == "" {
		nodes, err := t.scoped(ctx, []graph.NodeKind{graph.KindEndpoint}, graph.LocalNodes)
		if err != nil {
			return HandlerList{}, err
		}
		for _, scoped := range nodes {
			if !endpointMatches(scoped.Node, options) {
				continue
			}
			endpoint, truncated, err := t.endpoint(ctx, scoped, limit)
			if err != nil {
				return HandlerList{}, err
			}
			result.Truncated = result.Truncated || truncated
			repositories := handlerRepositories(endpoint.Resource, endpoint.Handlers, owners)
			if options.Repository != "" && !containsString(repositories, options.Repository) {
				continue
			}
			result.Matches = append(result.Matches, HandlerMatch{Kind: HandlerHTTP, Subject: endpoint.Resource,
				Method: endpoint.Method, Route: endpoint.Route, Status: endpoint.HandlerStatus,
				Handlers: endpoint.Handlers, Repositories: repositories})
		}
	}
	if options.Method == "" && options.Route == "" {
		nodes, err := t.scoped(ctx, []graph.NodeKind{graph.KindEvent}, graph.LocalNodes)
		if err != nil {
			return HandlerList{}, err
		}
		for _, scoped := range nodes {
			if !eventMatches(scoped.Node, options.Event) {
				continue
			}
			handlers, truncated, err := t.catalog.outgoing(ctx, scoped.Node.ID, limit, graph.EdgeHandledBy)
			if err != nil {
				return HandlerList{}, err
			}
			result.Truncated = result.Truncated || truncated
			subject := newResource(scoped)
			repositories := handlerRepositories(subject, handlers, owners)
			if options.Repository != "" && !containsString(repositories, options.Repository) {
				continue
			}
			name := eventName(scoped.Node)
			result.Matches = append(result.Matches, HandlerMatch{Kind: HandlerEvent, Subject: subject,
				Event: name, Status: handlerStatus(handlers, truncated), Handlers: handlers, Repositories: repositories})
		}
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		if result.Matches[i].Kind != result.Matches[j].Kind {
			return result.Matches[i].Kind < result.Matches[j].Kind
		}
		if result.Matches[i].Subject.QualifiedName != result.Matches[j].Subject.QualifiedName {
			return result.Matches[i].Subject.QualifiedName < result.Matches[j].Subject.QualifiedName
		}
		return result.Matches[i].Subject.ID < result.Matches[j].Subject.ID
	})
	if len(result.Matches) > limit {
		result.Matches, result.Truncated = result.Matches[:limit], true
	}
	return result, nil
}

type requestGroup struct {
	edges []graph.Edge
}

func preferredEdge(edges []graph.Edge, targetID string, nodes map[string]graph.Node) graph.Edge {
	ordered := append([]graph.Edge(nil), edges...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	if targetID != "" {
		for _, edge := range ordered {
			if edge.ToID == targetID {
				return edge
			}
		}
	}
	for _, edge := range ordered {
		if nodes[edge.ToID].External {
			return edge
		}
	}
	return ordered[0]
}

func (t *Topology) collectOutboundRequests(ctx context.Context, options TopologyOptions,
	owners map[string]string) ([]OutboundRequest, error) {
	limit := options.Limit
	if limit <= 0 {
		limit = DefaultCatalogLimit
	}
	scoped, err := t.scoped(ctx, []graph.NodeKind{graph.KindEndpoint}, graph.AllNodes)
	if err != nil {
		return nil, err
	}
	if owners == nil {
		owners, err = t.ownership(ctx)
		if err != nil {
			return nil, err
		}
	}
	nodes := make(map[string]graph.Node, len(scoped))
	scopes := make(map[string]graph.ScopedNode, len(scoped))
	declarations := map[string][]graph.ScopedNode{}
	groups := map[string]*requestGroup{}
	seenEdges := map[string]bool{}
	for _, candidate := range scoped {
		nodes[candidate.Node.ID] = candidate.Node
		scopes[candidate.Node.ID] = candidate
		method, route := endpointMethodRoute(candidate.Node)
		if !candidate.Node.External {
			declarations[method+"\x00"+route] = append(declarations[method+"\x00"+route], candidate)
		}
		edges, err := t.repository.EdgesTo(ctx, candidate.Node.ID)
		if err != nil {
			return nil, err
		}
		for _, edge := range edges {
			if edge.Kind != graph.EdgeRequests || seenEdges[edge.ID] {
				continue
			}
			seenEdges[edge.ID] = true
			key := edge.FactID
			if key == "" {
				key = edge.ID
			}
			if groups[key] == nil {
				groups[key] = &requestGroup{}
			}
			groups[key].edges = append(groups[key].edges, edge)
		}
	}
	result := make([]OutboundRequest, 0, len(groups))
	for _, group := range groups {
		if len(group.edges) == 0 {
			continue
		}
		base := preferredEdge(group.edges, "", nodes)
		target := nodes[base.ToID]
		method, route := endpointMethodRoute(target)
		if !endpointMatches(target, options) {
			continue
		}
		source, err := t.repository.Node(ctx, base.FromID)
		if err != nil {
			return nil, err
		}
		sourceScoped := graph.ScopedNode{Repository: owners[source.ID], Node: source}
		if options.Repository != "" && sourceScoped.Repository != options.Repository {
			continue
		}
		request := OutboundRequest{Source: newResource(sourceScoped), Method: method, Route: route,
			Candidates: []Endpoint{}}
		var requestTruncated bool
		request.Target, requestTruncated, err = t.endpoint(ctx, scopes[target.ID], limit)
		if err != nil {
			return nil, err
		}
		request.Truncated = requestTruncated
		candidates := declarations[method+"\x00"+route]
		sortScopedNodes(candidates)
		switch len(candidates) {
		case 0:
			request.Status = BoundaryUnresolved
			externalScoped := graph.ScopedNode{Node: target}
			request.Destination, requestTruncated, err = t.endpoint(ctx, externalScoped, limit)
			if err != nil {
				return nil, err
			}
			request.Truncated = request.Truncated || requestTruncated
			base = preferredEdge(group.edges, "", nodes)
		case 1:
			request.Status = BoundaryResolved
			request.Destination, requestTruncated, err = t.endpoint(ctx, candidates[0], limit)
			if err != nil {
				return nil, err
			}
			request.Truncated = request.Truncated || requestTruncated
			base = preferredEdge(group.edges, candidates[0].Node.ID, nodes)
		default:
			request.Status = BoundaryAmbiguous
			if len(candidates) > limit {
				candidates = candidates[:limit]
				request.Truncated = true
			}
			for _, candidate := range candidates {
				endpoint, endpointTruncated, endpointErr := t.endpoint(ctx, candidate, limit)
				if endpointErr != nil {
					return nil, endpointErr
				}
				request.Truncated = request.Truncated || endpointTruncated
				request.Candidates = append(request.Candidates, endpoint)
			}
			base = preferredEdge(group.edges, "", nodes)
		}
		request.Evidence = linkEvidence(base)
		result = append(result, request)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Method != result[j].Method {
			return result[i].Method < result[j].Method
		}
		if result[i].Route != result[j].Route {
			return result[i].Route < result[j].Route
		}
		if result[i].Source.Repository != result[j].Source.Repository {
			return result[i].Source.Repository < result[j].Source.Repository
		}
		if result[i].Source.ID != result[j].Source.ID {
			return result[i].Source.ID < result[j].Source.ID
		}
		return result[i].Evidence.EdgeID < result[j].Evidence.EdgeID
	})
	return result, nil
}

func (t *Topology) OutboundRequests(ctx context.Context, options TopologyOptions) (OutboundRequestList, error) {
	if options.Direction != "" {
		return OutboundRequestList{}, fmt.Errorf("a direction filter applies only to service topology")
	}
	options, limit, err := t.normalize(ctx, options)
	if err != nil {
		return OutboundRequestList{}, err
	}
	if options.Event != "" {
		return OutboundRequestList{}, fmt.Errorf("an event filter does not apply to outbound HTTP requests")
	}
	options.Limit = limit
	requests, err := t.collectOutboundRequests(ctx, options, nil)
	if err != nil {
		return OutboundRequestList{}, err
	}
	result := OutboundRequestList{Requests: requests}
	for _, request := range requests {
		result.Truncated = result.Truncated || request.Truncated
	}
	if len(result.Requests) > limit {
		result.Requests, result.Truncated = result.Requests[:limit], true
	}
	return result, nil
}

func serviceID(repository string) string { return "service:" + repository }

func presentationLabel(repository string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(repository), "/")
	if slash := strings.LastIndexByte(trimmed, '/'); slash >= 0 {
		return trimmed[slash+1:]
	}
	return trimmed
}

func repositoryService(repository string) ServiceNode {
	return ServiceNode{ID: serviceID(repository), Repository: repository,
		Label: presentationLabel(repository), Components: []Resource{}}
}

func externalService(kind, name string) ServiceNode {
	return ServiceNode{ID: graph.StableID("external-service", kind, name), Label: name,
		External: true, Components: []Resource{}}
}

func sortResources(resources []Resource) {
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Repository != resources[j].Repository {
			return resources[i].Repository < resources[j].Repository
		}
		if resources[i].Location.Path != resources[j].Location.Path {
			return resources[i].Location.Path < resources[j].Location.Path
		}
		if resources[i].QualifiedName != resources[j].QualifiedName {
			return resources[i].QualifiedName < resources[j].QualifiedName
		}
		return resources[i].ID < resources[j].ID
	})
}

func addService(services map[string]ServiceNode, service ServiceNode, components ...Resource) {
	existing, ok := services[service.ID]
	if !ok {
		existing = service
	}
	byID := map[string]Resource{}
	for _, component := range existing.Components {
		byID[component.ID] = component
	}
	for _, component := range components {
		if component.ID != "" {
			byID[component.ID] = component
		}
	}
	existing.Components = existing.Components[:0]
	for _, component := range byID {
		existing.Components = append(existing.Components, component)
	}
	sortResources(existing.Components)
	services[service.ID] = existing
}

func linkEvidence(edge graph.Edge) LinkEvidence {
	return LinkEvidence{EdgeID: edge.ID, FactID: edge.FactID, Relation: edge.Kind,
		FromID: edge.FromID, ToID: edge.ToID, Location: edge.Location,
		Federated: edge.Properties["federated"] == "true", Properties: edge.Properties}
}

func uniqueSorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func finalizeLink(link *ServiceLink) {
	for _, evidence := range link.Evidence {
		link.EdgeIDs = append(link.EdgeIDs, evidence.EdgeID)
		link.FactIDs = append(link.FactIDs, evidence.FactID)
		link.Federated = link.Federated || evidence.Federated
	}
	link.EdgeIDs = uniqueSorted(link.EdgeIDs)
	link.FactIDs = uniqueSorted(link.FactIDs)
	link.EndpointIDs = uniqueSorted(link.EndpointIDs)
	link.EventIDs = uniqueSorted(link.EventIDs)
	link.SourceNodes = uniqueResources(link.SourceNodes)
	link.TargetNodes = uniqueResources(link.TargetNodes)
	sort.Slice(link.Evidence, func(i, j int) bool { return link.Evidence[i].EdgeID < link.Evidence[j].EdgeID })
	link.ID = graph.StableID("link", string(link.Kind), link.FromServiceID, link.ToServiceID,
		link.Name, strings.Join(link.EdgeIDs, ","))
}

func uniqueResources(resources []Resource) []Resource {
	byID := map[string]Resource{}
	for _, resource := range resources {
		if resource.ID != "" {
			byID[resource.ID] = resource
		}
	}
	result := make([]Resource, 0, len(byID))
	for _, resource := range byID {
		result = append(result, resource)
	}
	sortResources(result)
	return result
}

func preferResolvedEdge(current, candidate graph.Edge, nodes map[string]graph.Node) graph.Edge {
	if current.ID == "" {
		return candidate
	}
	currentExternal, candidateExternal := nodes[current.ToID].External, nodes[candidate.ToID].External
	if currentExternal != candidateExternal {
		if !candidateExternal {
			return candidate
		}
		return current
	}
	if current.Properties["federated"] != "true" && candidate.Properties["federated"] == "true" {
		return candidate
	}
	if candidate.ID < current.ID {
		return candidate
	}
	return current
}

func (t *Topology) eventLinks(ctx context.Context, options TopologyOptions,
	owners map[string]string) ([]ServiceLink, error) {
	scoped, err := t.scoped(ctx, []graph.NodeKind{graph.KindEvent}, graph.AllNodes)
	if err != nil {
		return nil, err
	}
	if owners == nil {
		owners, err = t.ownership(ctx)
		if err != nil {
			return nil, err
		}
	}
	nodes := map[string]graph.Node{}
	groups := map[string][]graph.ScopedNode{}
	for _, event := range scoped {
		nodes[event.Node.ID] = event.Node
		name := eventName(event.Node)
		groups[name] = append(groups[name], event)
	}
	var result []ServiceLink
	for name, events := range groups {
		if !eventMatches(events[0].Node, options.Event) {
			continue
		}
		publishers := map[string]graph.Edge{}
		subscribers := map[string]graph.Edge{}
		var eventIDs []string
		for _, event := range events {
			if !event.Node.External {
				eventIDs = append(eventIDs, event.Node.ID)
			}
			edges, err := t.repository.EdgesTo(ctx, event.Node.ID)
			if err != nil {
				return nil, err
			}
			for _, edge := range edges {
				if edge.Kind != graph.EdgePublishes && edge.Kind != graph.EdgeSubscribes {
					continue
				}
				key := edge.FactID + "\x00" + edge.FromID
				if edge.FactID == "" {
					key = edge.ID
				}
				if edge.Kind == graph.EdgePublishes {
					publishers[key] = preferResolvedEdge(publishers[key], edge, nodes)
				} else {
					subscribers[key] = preferResolvedEdge(subscribers[key], edge, nodes)
				}
			}
		}
		if len(eventIDs) == 0 {
			for _, event := range events {
				eventIDs = append(eventIDs, event.Node.ID)
			}
		}
		eventIDs = uniqueSorted(eventIDs)
		eventIDsTruncated := false
		if len(eventIDs) > options.Limit {
			eventIDs = eventIDs[:options.Limit]
			eventIDsTruncated = true
		}
		for _, publisher := range publishers {
			fromRepository := owners[publisher.FromID]
			if fromRepository == "" {
				continue
			}
			publisherNode, err := t.repository.Node(ctx, publisher.FromID)
			if err != nil {
				return nil, err
			}
			publisherResource := newResource(graph.ScopedNode{Repository: fromRepository, Node: publisherNode})
			for _, subscriber := range subscribers {
				toRepository := owners[subscriber.FromID]
				if toRepository == "" {
					continue
				}
				subscriberNode, err := t.repository.Node(ctx, subscriber.FromID)
				if err != nil {
					return nil, err
				}
				subscriberResource := newResource(graph.ScopedNode{Repository: toRepository, Node: subscriberNode})
				link := ServiceLink{FromServiceID: serviceID(fromRepository), ToServiceID: serviceID(toRepository),
					Kind: LinkEvent, Name: name, Event: name, Status: BoundaryResolved,
					EventIDs:    append([]string(nil), eventIDs...),
					SourceNodes: []Resource{publisherResource}, TargetNodes: []Resource{subscriberResource},
					Evidence:  []LinkEvidence{linkEvidence(publisher), linkEvidence(subscriber)},
					Truncated: eventIDsTruncated}
				finalizeLink(&link)
				link.Federated = link.Federated || fromRepository != toRepository
				result = append(result, link)
			}
		}
	}
	return result, nil
}

func linkMatchesRepository(link ServiceLink, repository string, direction Direction) bool {
	if repository == "" {
		return true
	}
	id := serviceID(repository)
	switch direction {
	case Incoming:
		return link.ToServiceID == id
	case Outgoing:
		return link.FromServiceID == id
	default:
		return link.FromServiceID == id || link.ToServiceID == id
	}
}

func (t *Topology) ServiceTopology(ctx context.Context, options TopologyOptions) (ServiceTopology, error) {
	options, limit, err := t.normalize(ctx, options)
	if err != nil {
		return ServiceTopology{}, err
	}
	if options.Repository == "" && options.Direction != Both {
		return ServiceTopology{}, fmt.Errorf("a %s direction filter requires a repository", options.Direction)
	}
	options.Limit = limit
	requestOptions := options
	requestOptions.Repository = ""
	owners, err := t.ownership(ctx)
	if err != nil {
		return ServiceTopology{}, err
	}
	var requests []OutboundRequest
	if options.Event == "" {
		requests, err = t.collectOutboundRequests(ctx, requestOptions, owners)
		if err != nil {
			return ServiceTopology{}, err
		}
	}
	services := map[string]ServiceNode{}
	links := map[string]ServiceLink{}
	if options.Event == "" {
		for _, request := range requests {
			if request.Source.Repository == "" {
				continue
			}
			from := repositoryService(request.Source.Repository)
			var to ServiceNode
			var endpointIDs []string
			var targetNodes []Resource
			switch request.Status {
			case BoundaryResolved:
				to = repositoryService(request.Destination.Repository)
				endpointIDs = []string{request.Destination.ID}
				targetNodes = []Resource{request.Destination.Resource}
			case BoundaryAmbiguous:
				to = externalService("http-ambiguous", request.Method+" "+request.Route)
				targetNodes = append(targetNodes, request.Target.Resource)
				for _, candidate := range request.Candidates {
					endpointIDs = append(endpointIDs, candidate.ID)
					targetNodes = append(targetNodes, candidate.Resource)
				}
			default:
				to = externalService("http", request.Method+" "+request.Route)
				endpointIDs = []string{request.Destination.ID}
				targetNodes = []Resource{request.Target.Resource}
			}
			link := ServiceLink{FromServiceID: from.ID, ToServiceID: to.ID, Kind: LinkHTTP,
				Name: request.Method + " " + request.Route, Method: request.Method, Route: request.Route,
				Status: request.Status, EndpointIDs: endpointIDs,
				SourceNodes: []Resource{request.Source}, TargetNodes: targetNodes,
				Evidence: []LinkEvidence{request.Evidence}}
			finalizeLink(&link)
			if !linkMatchesRepository(link, options.Repository, options.Direction) {
				continue
			}
			addService(services, from, request.Source)
			if request.Status == BoundaryAmbiguous {
				addService(services, to, request.Target.Resource)
				for _, candidate := range request.Candidates {
					addService(services, repositoryService(candidate.Repository), candidate.Resource)
				}
			} else {
				addService(services, to, targetNodes...)
			}
			links[link.ID] = link
		}
	}
	if options.Method == "" && options.Route == "" {
		events, err := t.eventLinks(ctx, options, owners)
		if err != nil {
			return ServiceTopology{}, err
		}
		for _, link := range events {
			if !linkMatchesRepository(link, options.Repository, options.Direction) {
				continue
			}
			fromRepository := strings.TrimPrefix(link.FromServiceID, "service:")
			toRepository := strings.TrimPrefix(link.ToServiceID, "service:")
			addService(services, repositoryService(fromRepository), link.SourceNodes...)
			addService(services, repositoryService(toRepository), link.TargetNodes...)
			links[link.ID] = link
		}
	}
	result := ServiceTopology{Services: make([]ServiceNode, 0, len(services)), Links: make([]ServiceLink, 0, len(links))}
	for _, request := range requests {
		result.Truncated = result.Truncated || request.Truncated
	}
	for _, service := range services {
		result.Services = append(result.Services, service)
	}
	for _, link := range links {
		result.Links = append(result.Links, link)
		result.Truncated = result.Truncated || link.Truncated
	}
	sort.Slice(result.Services, func(i, j int) bool { return result.Services[i].ID < result.Services[j].ID })
	sort.Slice(result.Links, func(i, j int) bool {
		if result.Links[i].FromServiceID != result.Links[j].FromServiceID {
			return result.Links[i].FromServiceID < result.Links[j].FromServiceID
		}
		if result.Links[i].ToServiceID != result.Links[j].ToServiceID {
			return result.Links[i].ToServiceID < result.Links[j].ToServiceID
		}
		if result.Links[i].Kind != result.Links[j].Kind {
			return result.Links[i].Kind < result.Links[j].Kind
		}
		if result.Links[i].Name != result.Links[j].Name {
			return result.Links[i].Name < result.Links[j].Name
		}
		return result.Links[i].ID < result.Links[j].ID
	})
	if len(result.Links) > limit {
		result.Links, result.Truncated = result.Links[:limit], true
		used := map[string]bool{}
		for _, link := range result.Links {
			used[link.FromServiceID], used[link.ToServiceID] = true, true
		}
		kept := result.Services[:0]
		for _, service := range result.Services {
			if used[service.ID] {
				kept = append(kept, service)
			}
		}
		result.Services = kept
	}
	return result, nil
}

func mermaidLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	value = html.EscapeString(value)
	return strings.NewReplacer(
		"[", "&#91;", "]", "&#93;", "{", "&#123;", "}", "&#125;",
		"(", "&#40;", ")", "&#41;", "|", "&#124;",
	).Replace(value)
}

// RenderMermaid renders a stable presentation of structured topology. The
// generated identifiers never contain graph data, and every label escapes
// Mermaid control syntax.
func RenderMermaid(topology ServiceTopology) string {
	services := append([]ServiceNode(nil), topology.Services...)
	links := append([]ServiceLink(nil), topology.Links...)
	sort.Slice(services, func(i, j int) bool { return services[i].ID < services[j].ID })
	sort.Slice(links, func(i, j int) bool {
		if links[i].FromServiceID != links[j].FromServiceID {
			return links[i].FromServiceID < links[j].FromServiceID
		}
		if links[i].ToServiceID != links[j].ToServiceID {
			return links[i].ToServiceID < links[j].ToServiceID
		}
		if links[i].Name != links[j].Name {
			return links[i].Name < links[j].Name
		}
		return links[i].ID < links[j].ID
	})
	aliases := make(map[string]string, len(services))
	var builder strings.Builder
	builder.WriteString("flowchart LR\n")
	for index, service := range services {
		alias := fmt.Sprintf("s%d", index)
		aliases[service.ID] = alias
		fmt.Fprintf(&builder, "  %s[\"%s\"]\n", alias, mermaidLabel(service.Label))
	}
	for _, link := range links {
		from, fromOK := aliases[link.FromServiceID]
		to, toOK := aliases[link.ToServiceID]
		if !fromOK || !toOK {
			continue
		}
		label := link.Name
		if link.Status != BoundaryResolved {
			label += " (" + string(link.Status) + ")"
		}
		fmt.Fprintf(&builder, "  %s -->|\"%s\"| %s\n", from, mermaidLabel(label), to)
	}
	return builder.String()
}
