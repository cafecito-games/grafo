package query

import (
	"context"
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/httpmodel"
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
// topology queries. Repository and Component select independently stable
// indexed ownership identities; labels are presentation-only.
type TopologyOptions struct {
	Repository   string    `json:"repository,omitempty"`
	Component    string    `json:"component,omitempty"`
	Method       string    `json:"method,omitempty"`
	Route        string    `json:"route,omitempty"`
	Event        string    `json:"event,omitempty"`
	Direction    Direction `json:"direction,omitempty"`
	PathPrefixes []string  `json:"path_prefixes,omitempty"`
	Limit        int       `json:"limit,omitempty"`
}

// Endpoint is an HTTP declaration plus the graph evidence that exposes and
// handles it.
type Endpoint struct {
	Resource
	Method        string         `json:"method"`
	Route         string         `json:"route"`
	Authority     string         `json:"authority,omitempty"`
	Exposers      []UsageSite    `json:"exposers"`
	Handlers      []UsageSite    `json:"handlers"`
	HandlerStatus BoundaryStatus `json:"handler_status"`
	// Middleware is the resolved outer-to-inner chain. UnresolvedMiddleware
	// keeps dynamic boundaries explicit instead of silently dropping them.
	Middleware           []UsageSite `json:"middleware"`
	UnresolvedMiddleware []UsageSite `json:"unresolved_middleware"`
	MiddlewareTruncated  bool        `json:"middleware_truncated"`
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
	Scheme      string         `json:"scheme,omitempty"`
	Authority   string         `json:"authority,omitempty"`
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

// ServiceNode separates stable repository/component identity from its
// presentation label. External nodes are explicit unresolved or ambiguous
// destinations.
type ServiceNode struct {
	ID          string     `json:"id"`
	Repository  string     `json:"repository,omitempty"`
	Component   string     `json:"component,omitempty"`
	ComponentID string     `json:"component_id,omitempty"`
	Label       string     `json:"label"`
	External    bool       `json:"external,omitempty"`
	Components  []Resource `json:"components"`
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
	Scheme        string          `json:"scheme,omitempty"`
	Authority     string          `json:"authority,omitempty"`
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
	repository graph.TopologyRepository
	catalog    *Catalog
}

func NewTopology(repository graph.TopologyRepository) *Topology {
	return &Topology{repository: repository, catalog: NewCatalog(repository)}
}

func (t *Topology) normalize(ctx context.Context, options TopologyOptions) (TopologyOptions, int, error) {
	options.Repository = strings.TrimSpace(options.Repository)
	options.Component = strings.TrimSpace(options.Component)
	options.Method = strings.Trim(options.Method, " ")
	if options.Method != "" {
		method, err := httpmodel.PreserveMethod(options.Method)
		if err != nil {
			return options, 0, err
		}
		options.Method = method
	}
	options.Route = strings.Trim(options.Route, " ")
	if options.Route != "" {
		route, err := httpmodel.ParseRoute(options.Route)
		if err != nil {
			return options, 0, err
		}
		options.Route = route.Canonical
	}
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
	var err error
	options.PathPrefixes, err = normalizePathPrefixes(options.PathPrefixes)
	if err != nil {
		return options, 0, err
	}
	limit, err := t.catalog.bounds(ctx, CatalogOptions{Repository: options.Repository, Limit: options.Limit})
	return options, limit, err
}

func (t *Topology) scoped(ctx context.Context, kinds []graph.NodeKind, visibility graph.NodeVisibility) ([]graph.ScopedNode, error) {
	return t.scopedWithPaths(ctx, kinds, visibility, nil)
}

func (t *Topology) scopedWithPaths(ctx context.Context, kinds []graph.NodeKind, visibility graph.NodeVisibility, prefixes []string) ([]graph.ScopedNode, error) {
	result, err := t.repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: kinds, Visibility: visibility, PathPrefixes: prefixes})
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

type serviceIdentity struct {
	Repository  string
	Component   string
	ComponentID string
}

type topologyOwnership struct {
	byNode map[string]serviceIdentity
}

func (o *topologyOwnership) node(id string) serviceIdentity {
	if o == nil {
		return serviceIdentity{}
	}
	return o.byNode[id]
}

func (o *topologyOwnership) annotate(resource *Resource) {
	identity := o.node(resource.ID)
	resource.Repository = identity.Repository
	resource.Component = identity.Component
	resource.ComponentID = identity.ComponentID
}

func ownershipPathKey(repository, path string) string {
	return repository + "\x00" + path
}

func (t *Topology) ownership(ctx context.Context) (*topologyOwnership, error) {
	nodes, err := t.scoped(ctx, graph.NodeKinds(), graph.AllNodes)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]graph.ScopedNode, len(nodes))
	components := []graph.ScopedNode{}
	for _, scoped := range nodes {
		byID[scoped.Node.ID] = scoped
		if scoped.Node.Kind == graph.KindComponent && !scoped.Node.External {
			components = append(components, scoped)
		}
	}
	sortScopedNodes(components)
	fileComponents := map[string]serviceIdentity{}
	pathComponents := map[string]serviceIdentity{}
	for _, component := range components {
		identity := serviceIdentity{Repository: component.Repository, Component: component.Node.Name,
			ComponentID: component.Node.ID}
		edges, err := t.repository.EdgesFrom(ctx, component.Node.ID)
		if err != nil {
			return nil, err
		}
		for _, edge := range edges {
			if edge.Kind != graph.EdgeContains {
				continue
			}
			file, ok := byID[edge.ToID]
			if !ok || file.Node.External || file.Node.Kind != graph.KindFile || file.Repository != component.Repository {
				continue
			}
			if prior, ok := fileComponents[file.Node.ID]; ok && prior.ComponentID != identity.ComponentID {
				return nil, fmt.Errorf("file %q in repository %q belongs to multiple components %q and %q",
					file.Node.Location.Path, component.Repository, prior.Component, identity.Component)
			}
			fileComponents[file.Node.ID] = identity
			for _, path := range uniqueSorted([]string{file.Node.OwnerFile, file.Node.Location.Path}) {
				key := ownershipPathKey(component.Repository, path)
				if prior, ok := pathComponents[key]; ok && prior.ComponentID != identity.ComponentID {
					return nil, fmt.Errorf("file %q in repository %q belongs to multiple components %q and %q",
						path, component.Repository, prior.Component, identity.Component)
				}
				pathComponents[key] = identity
			}
		}
	}
	owners := &topologyOwnership{byNode: make(map[string]serviceIdentity, len(nodes))}
	for _, scoped := range nodes {
		if scoped.Node.External {
			continue
		}
		identity := serviceIdentity{Repository: scoped.Repository}
		if component, ok := fileComponents[scoped.Node.ID]; ok {
			identity = component
		} else if component, ok := pathComponents[ownershipPathKey(scoped.Repository, scoped.Node.OwnerFile)]; ok {
			identity = component
		}
		owners.byNode[scoped.Node.ID] = identity
	}
	return owners, nil
}

func rawEndpointMethodRoute(node graph.Node) (string, string) {
	method := strings.TrimSpace(node.Properties["method"])
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
				method = strings.TrimSpace(parsedMethod)
			}
			if route == "" {
				route = strings.TrimSpace(parsedRoute)
			}
		}
	}
	return method, route
}

func endpointMethodRoute(node graph.Node) (string, string) {
	method, route := rawEndpointMethodRoute(node)
	if normalized, err := httpmodel.PreserveMethod(method); err == nil {
		method = normalized
	}
	if parsed, err := httpmodel.ParseRoute(route); err == nil {
		route = parsed.Canonical
	}
	return method, route
}

func endpointMatches(node graph.Node, options TopologyOptions) bool {
	method, route := endpointMethodRoute(node)
	if options.Method != "" && method != options.Method {
		return false
	}
	if options.Route == "" {
		return true
	}
	declaration, declarationErr := httpmodel.ParseRoute(route)
	filter, filterErr := httpmodel.ParseRoute(options.Route)
	return declarationErr == nil && filterErr == nil && httpmodel.Compatibility(declaration, filter) != httpmodel.RankNone
}

func (t *Topology) endpointReferencedByRepository(ctx context.Context, endpointID, repository string,
	owners *topologyOwnership,
) (bool, error) {
	edges, err := t.repository.EdgesTo(ctx, endpointID)
	if err != nil {
		return false, err
	}
	for _, edge := range edges {
		if edge.Kind == graph.EdgeRequests && owners.node(edge.FromID).Repository == repository {
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

func (t *Topology) endpoint(ctx context.Context, scoped graph.ScopedNode, limit int,
	owners *topologyOwnership,
) (Endpoint, bool, error) {
	method, route := endpointMethodRoute(scoped.Node)
	exposers, exposedTruncated, err := t.catalog.incoming(ctx, scoped.Node.ID, limit, graph.EdgeExposes)
	if err != nil {
		return Endpoint{}, false, err
	}
	handlers, handlersTruncated, err := t.catalog.outgoing(ctx, scoped.Node.ID, limit, graph.EdgeHandledBy)
	if err != nil {
		return Endpoint{}, false, err
	}
	middleware, middlewareTruncated, err := t.catalog.outgoing(ctx, scoped.Node.ID, limit, graph.EdgeUsesMiddleware)
	if err != nil {
		return Endpoint{}, false, err
	}
	sortMiddleware(middleware)
	resolvedMiddleware, unresolvedMiddleware := []UsageSite{}, []UsageSite{}
	for _, site := range middleware {
		if site.Node.External {
			unresolvedMiddleware = append(unresolvedMiddleware, site)
		} else {
			resolvedMiddleware = append(resolvedMiddleware, site)
		}
	}
	status := handlerStatus(handlers, handlersTruncated)
	resource := newResource(scoped)
	owners.annotate(&resource)
	return Endpoint{Resource: resource, Method: method, Route: route,
		Authority: strings.TrimSpace(scoped.Node.Properties["authority"]),
		Exposers:  exposers, Handlers: handlers, HandlerStatus: status,
		Middleware: resolvedMiddleware, UnresolvedMiddleware: unresolvedMiddleware,
		MiddlewareTruncated: middlewareTruncated}, exposedTruncated || handlersTruncated || middlewareTruncated, nil
}

func sortMiddleware(sites []UsageSite) {
	sort.SliceStable(sites, func(i, j int) bool {
		left, leftErr := strconv.Atoi(sites[i].Evidence["order"])
		right, rightErr := strconv.Atoi(sites[j].Evidence["order"])
		if leftErr == nil && rightErr == nil && left != right {
			return left < right
		}
		if leftErr == nil != (rightErr == nil) {
			return leftErr == nil
		}
		if sites[i].Node.QualifiedName != sites[j].Node.QualifiedName {
			return sites[i].Node.QualifiedName < sites[j].Node.QualifiedName
		}
		return sites[i].EdgeID < sites[j].EdgeID
	})
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
	if options.Component != "" {
		return EndpointList{}, fmt.Errorf("a component filter applies only to service topology")
	}
	if options.Event != "" {
		return EndpointList{}, fmt.Errorf("an event filter does not apply to endpoint listings")
	}
	owners, err := t.ownership(ctx)
	if err != nil {
		return EndpointList{}, err
	}
	result := EndpointList{Endpoints: []Endpoint{}, Unresolved: []Endpoint{}}
	for _, visibility := range []graph.NodeVisibility{graph.LocalNodes, graph.ExternalNodes} {
		nodes, err := t.scopedWithPaths(ctx, []graph.NodeKind{graph.KindEndpoint}, visibility, options.PathPrefixes)
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
			endpoint, truncated, err := t.endpoint(ctx, scoped, limit, owners)
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

func handlerRepositories(subject Resource, handlers []UsageSite, owners *topologyOwnership) []string {
	set := map[string]bool{}
	if subject.Repository != "" {
		set[subject.Repository] = true
	}
	for _, handler := range handlers {
		if repository := owners.node(handler.Node.ID).Repository; repository != "" {
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
	if len(options.PathPrefixes) > 0 {
		return HandlerList{}, fmt.Errorf("path prefixes do not apply to an explicit handler lookup")
	}
	if options.Component != "" {
		return HandlerList{}, fmt.Errorf("a component filter applies only to service topology")
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
			endpoint, truncated, err := t.endpoint(ctx, scoped, limit, owners)
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
			owners.annotate(&subject)
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

type routeCandidate struct {
	scoped graph.ScopedNode
	method string
	route  httpmodel.Route
}

func endpointRouteCandidate(scoped graph.ScopedNode) (routeCandidate, bool) {
	method, route := endpointMethodRoute(scoped.Node)
	normalizedMethod, methodErr := httpmodel.PreserveMethod(method)
	parsedRoute, routeErr := httpmodel.ParseRoute(route)
	if methodErr != nil || routeErr != nil || scoped.Node.Properties["http_invalid"] == "true" {
		return routeCandidate{}, false
	}
	parsedRoute.Authority = strings.TrimSpace(scoped.Node.Properties["authority"])
	return routeCandidate{scoped: scoped, method: normalizedMethod, route: parsedRoute}, true
}

func requestMethodRoute(edge graph.Edge, target graph.Node) (string, httpmodel.Route, bool) {
	fallbackMethod, fallbackRoute := rawEndpointMethodRoute(target)
	method := strings.TrimSpace(edge.Properties["http_method"])
	route := strings.TrimSpace(edge.Properties["http_route"])
	if method == "" || route == "" {
		if method == "" {
			method = fallbackMethod
		}
		if route == "" {
			route = fallbackRoute
		}
	}
	normalizedMethod, methodErr := httpmodel.PreserveMethod(method)
	parsedRoute, routeErr := httpmodel.ParseRoute(route)
	if targetRoute, err := httpmodel.ParseRoute(fallbackRoute); err == nil && parsedRoute.Authority == "" {
		parsedRoute.Scheme = targetRoute.Scheme
		parsedRoute.Authority = targetRoute.Authority
	}
	valid := methodErr == nil && routeErr == nil && edge.Properties["http_invalid"] != "true"
	return normalizedMethod, parsedRoute, valid
}

func routeMatchesOptions(method string, route httpmodel.Route, valid bool, options TopologyOptions) bool {
	if options.Method != "" && method != options.Method {
		return false
	}
	if options.Route == "" {
		return true
	}
	filter, err := httpmodel.ParseRoute(options.Route)
	return valid && err == nil && httpmodel.Compatibility(filter, route) != httpmodel.RankNone
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
	owners *topologyOwnership) ([]OutboundRequest, error) {
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
	declarations := []routeCandidate{}
	groups := map[string]*requestGroup{}
	seenEdges := map[string]bool{}
	for _, candidate := range scoped {
		nodes[candidate.Node.ID] = candidate.Node
		scopes[candidate.Node.ID] = candidate
		if !candidate.Node.External {
			if declaration, ok := endpointRouteCandidate(candidate); ok {
				declarations = append(declarations, declaration)
			}
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
		method, routeModel, validRoute := requestMethodRoute(base, target)
		route := strings.TrimSpace(base.Properties["http_route"])
		if validRoute {
			route = routeModel.Canonical
		} else if route == "" {
			_, route = endpointMethodRoute(target)
		}
		if !routeMatchesOptions(method, routeModel, validRoute, options) {
			continue
		}
		source, err := t.repository.Node(ctx, base.FromID)
		if err != nil {
			return nil, err
		}
		sourceOwner := owners.node(source.ID)
		sourceScoped := graph.ScopedNode{Repository: sourceOwner.Repository, Node: source}
		if options.Repository != "" && sourceScoped.Repository != options.Repository {
			continue
		}
		request := OutboundRequest{Source: newResource(sourceScoped), Method: method, Route: route,
			Scheme: routeModel.Scheme, Authority: routeModel.Authority, Candidates: []Endpoint{}}
		owners.annotate(&request.Source)
		if scheme := strings.TrimSpace(base.Properties["http_scheme"]); scheme != "" {
			request.Scheme = strings.ToLower(scheme)
		}
		if authority := strings.TrimSpace(base.Properties["http_authority"]); authority != "" {
			request.Authority = authority
		}
		var requestTruncated bool
		request.Target, requestTruncated, err = t.endpoint(ctx, scopes[target.ID], limit, owners)
		if err != nil {
			return nil, err
		}
		request.Truncated = requestTruncated
		request.Target.Method, request.Target.Route = method, route
		localTargets := map[string]graph.ScopedNode{}
		hasEligibleBoundary := false
		var authorityState httpmodel.AuthorityState
		for index, edge := range group.edges {
			edgeTarget, ok := nodes[edge.ToID]
			if !ok {
				return nil, fmt.Errorf("request edge %s targets missing node %s", edge.ID, edge.ToID)
			}
			_, edgeRoute, _ := requestMethodRoute(edge, edgeTarget)
			contract, contractErr := httpmodel.ParseDestinationContract(edge.Properties, edgeRoute)
			if contractErr != nil {
				return nil, fmt.Errorf("request edge %s: %w", edge.ID, contractErr)
			}
			if err := contract.ValidateTarget(edgeTarget.External); err != nil {
				return nil, fmt.Errorf("request edge %s: %w", edge.ID, err)
			}
			if index > 0 && contract.Authority != authorityState {
				return nil, fmt.Errorf("request fact %s has conflicting authority states", base.FactID)
			}
			authorityState = contract.Authority
			if contract.Authority == httpmodel.AuthorityLocal &&
				(contract.Resolution == httpmodel.DestinationAmbiguous || contract.Resolution == httpmodel.DestinationUnresolved) {
				hasEligibleBoundary = true
			}
			if !edgeTarget.External {
				localTargets[edgeTarget.ID] = scopes[edgeTarget.ID]
			}
		}
		candidates := make([]graph.ScopedNode, 0, len(localTargets))
		for _, candidate := range localTargets {
			candidates = append(candidates, candidate)
		}
		if len(candidates) == 0 && hasEligibleBoundary && validRoute {
			compatible := make([]httpmodel.EndpointCandidate, 0, len(declarations))
			for _, declaration := range declarations {
				compatible = append(compatible, httpmodel.EndpointCandidate{Method: declaration.method,
					Authority: declaration.route.Authority, Route: declaration.route})
			}
			for _, index := range httpmodel.BestCandidateIndexes(method, routeModel, compatible) {
				candidates = append(candidates, declarations[index].scoped)
			}
		}
		sortScopedNodes(candidates)
		switch {
		case len(candidates) == 0:
			request.Status = BoundaryUnresolved
			externalScoped := graph.ScopedNode{Node: target}
			request.Destination, requestTruncated, err = t.endpoint(ctx, externalScoped, limit, owners)
			if err != nil {
				return nil, err
			}
			request.Truncated = request.Truncated || requestTruncated
			request.Destination.Method, request.Destination.Route = method, route
			base = preferredEdge(group.edges, "", nodes)
		case len(candidates) == 1:
			request.Status = BoundaryResolved
			request.Destination, requestTruncated, err = t.endpoint(ctx, candidates[0], limit, owners)
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
				endpoint, endpointTruncated, endpointErr := t.endpoint(ctx, candidate, limit, owners)
				if endpointErr != nil {
					return nil, endpointErr
				}
				request.Truncated = request.Truncated || endpointTruncated
				request.Candidates = append(request.Candidates, endpoint)
			}
			base = preferredEdge(group.edges, "", nodes)
		}
		request.Evidence = linkEvidence(base)
		if request.Status == BoundaryResolved && request.Source.Repository != "" && request.Destination.Repository != "" &&
			request.Source.Repository != request.Destination.Repository {
			request.Evidence.Federated = true
		}
		if request.Status == BoundaryAmbiguous && request.Source.Repository != "" {
			for _, candidate := range request.Candidates {
				request.Evidence.Federated = request.Evidence.Federated ||
					candidate.Repository != "" && candidate.Repository != request.Source.Repository
			}
		}
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
	if options.Component != "" {
		return OutboundRequestList{}, fmt.Errorf("a component filter applies only to service topology")
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
	if len(options.PathPrefixes) > 0 {
		result.Requests = nil
		for _, request := range requests {
			anchor := request.Source.Location.Path
			if anchor == "" {
				anchor = request.Evidence.Location.Path
			}
			if matchesPathPrefixes(anchor, options.PathPrefixes) {
				result.Requests = append(result.Requests, request)
			}
		}
		requests = result.Requests
	}
	for _, request := range requests {
		result.Truncated = result.Truncated || request.Truncated
	}
	if len(result.Requests) > limit {
		result.Requests, result.Truncated = result.Requests[:limit], true
	}
	return result, nil
}

func serviceID(repository string) string { return "service:" + repository }

func componentServiceID(repository, componentID string) string {
	return graph.StableID("service", repository, componentID)
}

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

func ownedService(identity serviceIdentity) ServiceNode {
	if identity.ComponentID == "" {
		return repositoryService(identity.Repository)
	}
	return ServiceNode{ID: componentServiceID(identity.Repository, identity.ComponentID),
		Repository: identity.Repository, Component: identity.Component, ComponentID: identity.ComponentID,
		Label: presentationLabel(identity.Repository) + "/" + identity.Component, Components: []Resource{}}
}

func resourceService(resource Resource) ServiceNode {
	return ownedService(serviceIdentity{Repository: resource.Repository, Component: resource.Component,
		ComponentID: resource.ComponentID})
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
		if resources[i].ComponentID != resources[j].ComponentID {
			return resources[i].ComponentID < resources[j].ComponentID
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
	owners *topologyOwnership) ([]ServiceLink, error) {
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
			fromOwner := owners.node(publisher.FromID)
			if fromOwner.Repository == "" {
				continue
			}
			publisherNode, err := t.repository.Node(ctx, publisher.FromID)
			if err != nil {
				return nil, err
			}
			publisherResource := newResource(graph.ScopedNode{Repository: fromOwner.Repository, Node: publisherNode})
			owners.annotate(&publisherResource)
			for _, subscriber := range subscribers {
				toOwner := owners.node(subscriber.FromID)
				if toOwner.Repository == "" {
					continue
				}
				subscriberNode, err := t.repository.Node(ctx, subscriber.FromID)
				if err != nil {
					return nil, err
				}
				subscriberResource := newResource(graph.ScopedNode{Repository: toOwner.Repository, Node: subscriberNode})
				owners.annotate(&subscriberResource)
				link := ServiceLink{FromServiceID: ownedService(fromOwner).ID, ToServiceID: ownedService(toOwner).ID,
					Kind: LinkEvent, Name: name, Event: name, Status: BoundaryResolved,
					EventIDs:    append([]string(nil), eventIDs...),
					SourceNodes: []Resource{publisherResource}, TargetNodes: []Resource{subscriberResource},
					Evidence:  []LinkEvidence{linkEvidence(publisher), linkEvidence(subscriber)},
					Truncated: eventIDsTruncated}
				finalizeLink(&link)
				link.Federated = link.Federated || fromOwner.Repository != toOwner.Repository
				result = append(result, link)
			}
		}
	}
	return result, nil
}

func serviceMatchesScope(service ServiceNode, options TopologyOptions) bool {
	if service.External {
		return false
	}
	if options.Repository != "" && service.Repository != options.Repository {
		return false
	}
	return options.Component == "" || service.Component == options.Component
}

func linkMatchesScope(from, to ServiceNode, options TopologyOptions) bool {
	if options.Repository == "" && options.Component == "" {
		return true
	}
	switch options.Direction {
	case Incoming:
		return serviceMatchesScope(to, options)
	case Outgoing:
		return serviceMatchesScope(from, options)
	default:
		return serviceMatchesScope(from, options) || serviceMatchesScope(to, options)
	}
}

func (t *Topology) ServiceTopology(ctx context.Context, options TopologyOptions) (ServiceTopology, error) {
	options, limit, err := t.normalize(ctx, options)
	if err != nil {
		return ServiceTopology{}, err
	}
	if options.Repository == "" && options.Component == "" && options.Direction != Both {
		return ServiceTopology{}, fmt.Errorf("a %s direction filter requires a repository or component", options.Direction)
	}
	options.Limit = limit
	requestOptions := options
	requestOptions.Repository = ""
	// A topology link is selected when either local boundary is in scope. The
	// outbound-request source-only anchor therefore cannot be applied while the
	// complete link is still being assembled.
	requestOptions.PathPrefixes = nil
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
			from := resourceService(request.Source)
			boundaryName := request.Method + " " + request.Route
			if request.Authority != "" {
				prefix := "//"
				if request.Scheme != "" {
					prefix = request.Scheme + "://"
				}
				boundaryName = request.Method + " " + prefix + request.Authority + request.Route
			}
			var to ServiceNode
			var endpointIDs []string
			var targetNodes []Resource
			switch request.Status {
			case BoundaryResolved:
				to = resourceService(request.Destination.Resource)
				endpointIDs = []string{request.Destination.ID}
				targetNodes = []Resource{request.Destination.Resource}
			case BoundaryAmbiguous:
				to = externalService("http-ambiguous", boundaryName)
				targetNodes = append(targetNodes, request.Target.Resource)
				for _, candidate := range request.Candidates {
					endpointIDs = append(endpointIDs, candidate.ID)
					targetNodes = append(targetNodes, candidate.Resource)
				}
			default:
				to = externalService("http", boundaryName)
				endpointIDs = []string{request.Destination.ID}
				targetNodes = []Resource{request.Target.Resource}
			}
			link := ServiceLink{FromServiceID: from.ID, ToServiceID: to.ID, Kind: LinkHTTP,
				Name: boundaryName, Method: request.Method, Route: request.Route,
				Scheme: request.Scheme, Authority: request.Authority,
				Status: request.Status, EndpointIDs: endpointIDs,
				SourceNodes: []Resource{request.Source}, TargetNodes: targetNodes,
				Evidence: []LinkEvidence{request.Evidence}, Truncated: request.Truncated}
			finalizeLink(&link)
			if !linkMatchesPathPrefixes(link, options.PathPrefixes) {
				continue
			}
			if !linkMatchesScope(from, to, options) {
				continue
			}
			addService(services, from, request.Source)
			if request.Status == BoundaryAmbiguous {
				addService(services, to, request.Target.Resource)
				for _, candidate := range request.Candidates {
					addService(services, resourceService(candidate.Resource), candidate.Resource)
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
			if len(link.SourceNodes) == 0 || len(link.TargetNodes) == 0 {
				continue
			}
			from := resourceService(link.SourceNodes[0])
			to := resourceService(link.TargetNodes[0])
			if !linkMatchesPathPrefixes(link, options.PathPrefixes) {
				continue
			}
			if !linkMatchesScope(from, to, options) {
				continue
			}
			addService(services, from, link.SourceNodes...)
			addService(services, to, link.TargetNodes...)
			links[link.ID] = link
		}
	}
	result := ServiceTopology{Services: make([]ServiceNode, 0, len(services)), Links: make([]ServiceLink, 0, len(links))}
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

func linkMatchesPathPrefixes(link ServiceLink, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, group := range [][]Resource{link.SourceNodes, link.TargetNodes} {
		for _, resource := range group {
			if !resource.Unresolved && resource.Repository != "" && matchesPathPrefixes(resource.Location.Path, prefixes) {
				return true
			}
		}
	}
	return false
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
