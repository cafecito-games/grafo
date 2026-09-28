package query

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

const (
	// DefaultCatalogLimit bounds a catalog response when the caller does not
	// ask for a specific bound.
	DefaultCatalogLimit = 100
	// MaxCatalogLimit is the largest bound a caller may request. A catalog
	// that reaches it reports truncation instead of streaming without bounds.
	MaxCatalogLimit = 1000
)

// CatalogOptions bounds and filters a catalog request.
type CatalogOptions struct {
	Repository string
	Name       string
	Limit      int
}

// normalized trims the name fragment so a blank filter means the same thing at
// every surface. Without it a whitespace-only fragment would narrow one catalog
// to nothing and be ignored by another.
func (o CatalogOptions) normalized() CatalogOptions {
	o.Name = strings.TrimSpace(o.Name)
	return o
}

// Catalog answers read-only inventory and usage questions about data
// resources, configuration keys, and events. Classification lives here so
// storage adapters only enumerate.
type Catalog struct {
	repository graph.CatalogRepository
}

func NewCatalog(repository graph.CatalogRepository) *Catalog {
	return &Catalog{repository: repository}
}

// Resource describes one catalog entry. Common metadata is normalized into
// typed fields while the parser's own node properties remain available.
type Resource struct {
	Repository    string            `json:"repository,omitempty"`
	Component     string            `json:"component,omitempty"`
	ComponentID   string            `json:"component_id,omitempty"`
	ID            string            `json:"id"`
	Kind          graph.NodeKind    `json:"kind"`
	Name          string            `json:"name"`
	QualifiedName string            `json:"qualified_name"`
	ObjectKind    string            `json:"object_kind,omitempty"`
	Dialect       string            `json:"dialect,omitempty"`
	Persistence   string            `json:"persistence,omitempty"`
	Language      string            `json:"language,omitempty"`
	Location      graph.Location    `json:"location,omitempty"`
	Unresolved    bool              `json:"unresolved,omitempty"`
	Properties    map[string]string `json:"properties,omitempty"`
}

// DataResourceList is the deterministic catalog of stored data resources.
// Unresolved holds external targets that no indexed declaration matched; their
// presence is uncertainty, never proof that a resource is absent.
type DataResourceList struct {
	Resources  []Resource `json:"resources"`
	Unresolved []Resource `json:"unresolved"`
	Truncated  bool       `json:"truncated"`
}

// UsageSite is one edge of evidence that a node uses a catalog entry.
type UsageSite struct {
	Relation  graph.EdgeKind    `json:"relation"`
	EdgeID    string            `json:"edge_id"`
	FactID    string            `json:"fact_id,omitempty"`
	Node      graph.Node        `json:"node"`
	Location  graph.Location    `json:"location,omitempty"`
	Federated bool              `json:"federated,omitempty"`
	Evidence  map[string]string `json:"evidence,omitempty"`
}

// DataResourceUsage partitions the edges that reach one data resource by
// semantic direction.
type DataResourceUsage struct {
	Resource   Resource    `json:"resource"`
	Readers    []UsageSite `json:"readers"`
	Writers    []UsageSite `json:"writers"`
	References []UsageSite `json:"references"`
	Truncated  bool        `json:"truncated"`
}

// ConfigKey reports where a configuration key is defined and read. Stored
// values are never included.
type ConfigKey struct {
	Resource
	Format  string `json:"format,omitempty"`
	Section string `json:"section,omitempty"`
	Defined bool   `json:"defined"`
	// WithheldProperties names node properties excluded from Properties
	// because they are not classified as non-secret configuration metadata.
	WithheldProperties []string    `json:"withheld_properties,omitempty"`
	Definitions        []UsageSite `json:"definitions"`
	Readers            []UsageSite `json:"readers"`
	References         []UsageSite `json:"references"`
}

// ConfigKeyList is the deterministic catalog of configuration keys.
type ConfigKeyList struct {
	Keys       []ConfigKey `json:"keys"`
	Unresolved []ConfigKey `json:"unresolved"`
	Truncated  bool        `json:"truncated"`
}

// Event reports an event declaration with its publishers, subscribers, and
// handlers.
type Event struct {
	Resource
	Form         string      `json:"form,omitempty"`
	Parameters   string      `json:"parameters,omitempty"`
	Declarations []UsageSite `json:"declarations"`
	Producers    []UsageSite `json:"producers"`
	Consumers    []UsageSite `json:"consumers"`
	Handlers     []UsageSite `json:"handlers"`
}

// EventList is the deterministic catalog of events.
type EventList struct {
	Events     []Event `json:"events"`
	Unresolved []Event `json:"unresolved"`
	Truncated  bool    `json:"truncated"`
}

// OrphanCategory names the shape of a missing counterpart.
type OrphanCategory string

const (
	PublishedWithoutConsumer OrphanCategory = "published_without_consumer"
	ConsumedWithoutProducer  OrphanCategory = "consumed_without_producer"
	DeclaredWithNeither      OrphanCategory = "declared_with_neither"
)

// OrphanStatus separates a confirmed orphan from one whose missing counterpart
// may exist behind an unresolved target.
type OrphanStatus string

const (
	OrphanConfirmed OrphanStatus = "orphaned"
	OrphanUnknown   OrphanStatus = "unknown"
)

// OrphanedEvent reports one event whose local evidence is one-sided.
type OrphanedEvent struct {
	Event    Event          `json:"event"`
	Category OrphanCategory `json:"category"`
	Status   OrphanStatus   `json:"status"`
	// UnresolvedProducers and UnresolvedConsumers count edges that reach an
	// unresolved external target this event may be. Any such edge makes the
	// status unknown rather than orphaned. Counterpart evidence uses the
	// fixed MaxCatalogLimit rather than the caller's bound, because a tighter
	// bound would only turn more findings unknown.
	UnresolvedProducers    int         `json:"unresolved_producers"`
	UnresolvedConsumers    int         `json:"unresolved_consumers"`
	UnresolvedCounterparts []UsageSite `json:"unresolved_counterparts,omitempty"`
}

// OrphanedEventList is the deterministic orphan report.
type OrphanedEventList struct {
	Events    []OrphanedEvent `json:"events"`
	Truncated bool            `json:"truncated"`
}

// configMetadataProperties classifies the configuration-key node properties
// that describe a key rather than hold its value. Anything else a parser
// records may be the value itself, so it is withheld.
var configMetadataProperties = map[string]bool{
	"defined": true, "format": true, "section": true, "resource": true,
	"uid": true, "unresolved": true,
}

// DataResources catalogs stored data resources. Passing no kinds catalogs
// every kind the graph model classifies as a data resource.
func (c *Catalog) DataResources(ctx context.Context, kinds []graph.NodeKind, options CatalogOptions) (DataResourceList, error) {
	options = options.normalized()
	if len(kinds) == 0 {
		kinds = graph.DataResourceKinds()
	}
	requested := make([]graph.NodeKind, 0, len(kinds))
	seenKind := make(map[graph.NodeKind]bool, len(kinds))
	for _, kind := range kinds {
		if !graph.IsDataResourceKind(kind) {
			return DataResourceList{}, fmt.Errorf("%q is not a data resource kind; supported kinds are %s",
				kind, joinKinds(graph.DataResourceKinds()))
		}
		if seenKind[kind] {
			continue
		}
		seenKind[kind] = true
		requested = append(requested, kind)
	}
	kinds = requested
	limit, err := c.bounds(ctx, options)
	if err != nil {
		return DataResourceList{}, err
	}
	result := DataResourceList{Resources: []Resource{}, Unresolved: []Resource{}}
	declared, truncated, err := c.list(ctx, kinds, graph.LocalNodes, options, limit)
	if err != nil {
		return DataResourceList{}, err
	}
	result.Truncated = truncated
	for _, scoped := range declared {
		result.Resources = append(result.Resources, newResource(scoped))
	}
	unresolved, truncated, err := c.list(ctx, kinds, graph.ExternalNodes, options, limit)
	if err != nil {
		return DataResourceList{}, err
	}
	result.Truncated = result.Truncated || truncated
	for _, scoped := range unresolved {
		result.Unresolved = append(result.Unresolved, newResource(scoped))
	}
	return result, nil
}

// DataResourceUsage reports the readers and writers of one data resource. An
// ambiguous name returns candidates instead of guessing one of them.
func (c *Catalog) DataResourceUsage(ctx context.Context, selector string, options CatalogOptions) (DataResourceUsage, error) {
	options = options.normalized()
	// The selector already names the resource. Accepting a name filter here and
	// ignoring it would let a caller believe it narrowed a result it did not.
	if options.Name != "" {
		return DataResourceUsage{}, fmt.Errorf("a name filter does not apply to one data resource; pass the name as the selector")
	}
	limit, err := c.bounds(ctx, options)
	if err != nil {
		return DataResourceUsage{}, err
	}
	scoped, err := c.resolveDataResource(ctx, selector, options)
	if err != nil {
		return DataResourceUsage{}, err
	}
	edges, truncated, err := c.incoming(ctx, scoped.Node.ID, limit,
		graph.EdgeReads, graph.EdgeWrites, graph.EdgeReferences)
	if err != nil {
		return DataResourceUsage{}, err
	}
	result := DataResourceUsage{Resource: newResource(scoped), Truncated: truncated,
		Readers: []UsageSite{}, Writers: []UsageSite{}, References: []UsageSite{}}
	for _, site := range edges {
		switch site.Relation {
		case graph.EdgeReads:
			result.Readers = append(result.Readers, site)
		case graph.EdgeWrites:
			result.Writers = append(result.Writers, site)
		default:
			result.References = append(result.References, site)
		}
	}
	return result, nil
}

// ConfigKeys catalogs configuration keys with their definitions and readers.
// Stored values never appear in the response.
func (c *Catalog) ConfigKeys(ctx context.Context, options CatalogOptions) (ConfigKeyList, error) {
	options = options.normalized()
	limit, err := c.bounds(ctx, options)
	if err != nil {
		return ConfigKeyList{}, err
	}
	kinds := []graph.NodeKind{graph.KindConfigKey}
	result := ConfigKeyList{Keys: []ConfigKey{}, Unresolved: []ConfigKey{}}
	for _, visibility := range []graph.NodeVisibility{graph.LocalNodes, graph.ExternalNodes} {
		scopedNodes, truncated, err := c.list(ctx, kinds, visibility, options, limit)
		if err != nil {
			return ConfigKeyList{}, err
		}
		result.Truncated = result.Truncated || truncated
		for _, scoped := range scopedNodes {
			key, keyTruncated, err := c.configKey(ctx, scoped, limit)
			if err != nil {
				return ConfigKeyList{}, err
			}
			result.Truncated = result.Truncated || keyTruncated
			if visibility == graph.ExternalNodes {
				result.Unresolved = append(result.Unresolved, key)
				continue
			}
			result.Keys = append(result.Keys, key)
		}
	}
	return result, nil
}

// Events catalogs event declarations with their producers, consumers, and
// handlers.
func (c *Catalog) Events(ctx context.Context, options CatalogOptions) (EventList, error) {
	options = options.normalized()
	limit, err := c.bounds(ctx, options)
	if err != nil {
		return EventList{}, err
	}
	kinds := []graph.NodeKind{graph.KindEvent}
	result := EventList{Events: []Event{}, Unresolved: []Event{}}
	for _, visibility := range []graph.NodeVisibility{graph.LocalNodes, graph.ExternalNodes} {
		scopedNodes, truncated, err := c.list(ctx, kinds, visibility, options, limit)
		if err != nil {
			return EventList{}, err
		}
		result.Truncated = result.Truncated || truncated
		for _, scoped := range scopedNodes {
			event, eventTruncated, err := c.event(ctx, scoped, limit)
			if err != nil {
				return EventList{}, err
			}
			result.Truncated = result.Truncated || eventTruncated
			if visibility == graph.ExternalNodes {
				result.Unresolved = append(result.Unresolved, event)
				continue
			}
			result.Events = append(result.Events, event)
		}
	}
	return result, nil
}

// OrphanedEvents reports declared events whose local evidence is one-sided. An
// unresolved target that may be the missing counterpart downgrades the finding
// to unknown; it is never reported as a confirmed orphan.
func (c *Catalog) OrphanedEvents(ctx context.Context, options CatalogOptions) (OrphanedEventList, error) {
	options = options.normalized()
	limit, err := c.bounds(ctx, options)
	if err != nil {
		return OrphanedEventList{}, err
	}
	declared, truncated, err := c.list(ctx, []graph.NodeKind{graph.KindEvent}, graph.LocalNodes, options, limit)
	if err != nil {
		return OrphanedEventList{}, err
	}
	counterparts, err := c.unresolvedEventIndex(ctx, options)
	if err != nil {
		return OrphanedEventList{}, err
	}
	declaredNames, err := c.declaredEventNames(ctx, options)
	if err != nil {
		return OrphanedEventList{}, err
	}
	result := OrphanedEventList{Events: []OrphanedEvent{}, Truncated: truncated}
	for _, scoped := range declared {
		event, eventTruncated, err := c.event(ctx, scoped, limit)
		if err != nil {
			return OrphanedEventList{}, err
		}
		result.Truncated = result.Truncated || eventTruncated
		orphan, ok := classifyOrphan(event)
		if !ok {
			continue
		}
		sites, counterpartsTruncated, err := c.unresolvedCounterparts(ctx, event, counterparts)
		if err != nil {
			return OrphanedEventList{}, err
		}
		result.Truncated = result.Truncated || counterpartsTruncated
		orphan.UnresolvedCounterparts = sites
		for _, site := range sites {
			if site.Relation == graph.EdgePublishes {
				orphan.UnresolvedProducers++
			} else {
				orphan.UnresolvedConsumers++
			}
		}
		produced, consumed := len(event.Producers) > 0, len(event.Consumers) > 0 || len(event.Handlers) > 0
		if (!produced && orphan.UnresolvedProducers > 0) || (!consumed && orphan.UnresolvedConsumers > 0) {
			orphan.Status = OrphanUnknown
		}
		// Evidence the bound cut off may hold the missing counterpart, so a
		// truncated event is uncertain rather than a confirmed orphan.
		if eventTruncated || counterpartsTruncated {
			orphan.Status = OrphanUnknown
		}
		result.Events = append(result.Events, orphan)
	}
	unresolved, unresolvedTruncated, err := c.list(ctx, []graph.NodeKind{graph.KindEvent}, graph.ExternalNodes, options, limit)
	if err != nil {
		return OrphanedEventList{}, err
	}
	result.Truncated = result.Truncated || unresolvedTruncated
	for _, scoped := range unresolved {
		// An unresolved target that matches a declaration is already reported
		// as that declaration's uncertainty; reporting it twice would count the
		// same evidence as two findings.
		if declaredNames[scoped.Node.QualifiedName] || declaredNames[scoped.Node.Name] {
			continue
		}
		event, eventTruncated, err := c.event(ctx, scoped, limit)
		if err != nil {
			return OrphanedEventList{}, err
		}
		result.Truncated = result.Truncated || eventTruncated
		orphan, ok := classifyOrphan(event)
		if !ok {
			continue
		}
		// No declaration bounds an unresolved event name, so one-sided
		// evidence can never confirm that the counterpart is absent.
		orphan.Status = OrphanUnknown
		result.Events = append(result.Events, orphan)
	}
	sort.Slice(result.Events, func(i, j int) bool {
		if result.Events[i].Event.QualifiedName != result.Events[j].Event.QualifiedName {
			return result.Events[i].Event.QualifiedName < result.Events[j].Event.QualifiedName
		}
		return result.Events[i].Event.ID < result.Events[j].Event.ID
	})
	return result, nil
}

// classifyOrphan reports the one-sided category of an event, or false when both
// a producer and a consumer are present.
func classifyOrphan(event Event) (OrphanedEvent, bool) {
	produced := len(event.Producers) > 0
	consumed := len(event.Consumers) > 0 || len(event.Handlers) > 0
	if produced && consumed {
		return OrphanedEvent{}, false
	}
	orphan := OrphanedEvent{Event: event, Status: OrphanConfirmed}
	switch {
	case produced:
		orphan.Category = PublishedWithoutConsumer
	case consumed:
		orphan.Category = ConsumedWithoutProducer
	default:
		orphan.Category = DeclaredWithNeither
	}
	return orphan, true
}

// declaredEventNames collects every name an indexed event declaration answers
// to, so unresolved targets are never double-reported. Like the counterpart
// index it stays unbounded, because a partial set would duplicate findings.
func (c *Catalog) declaredEventNames(ctx context.Context, options CatalogOptions) (map[string]bool, error) {
	scopedNodes, err := c.repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: []graph.NodeKind{graph.KindEvent}, Repository: options.Repository,
		Visibility: graph.LocalNodes,
	})
	if err != nil {
		return nil, err
	}
	result := map[string]bool{}
	for _, scoped := range scopedNodes {
		result[scoped.Node.QualifiedName] = true
		result[scoped.Node.Name] = true
	}
	delete(result, "")
	return result, nil
}

func (c *Catalog) bounds(ctx context.Context, options CatalogOptions) (int, error) {
	if options.Limit < 0 {
		return 0, fmt.Errorf("limit must not be negative")
	}
	limit := options.Limit
	if limit == 0 {
		limit = DefaultCatalogLimit
	}
	if limit > MaxCatalogLimit {
		limit = MaxCatalogLimit
	}
	if options.Repository == "" {
		return limit, nil
	}
	known, err := c.repository.Repositories(ctx)
	if err != nil {
		return 0, err
	}
	for _, name := range known {
		if name == options.Repository {
			return limit, nil
		}
	}
	return 0, fmt.Errorf("unknown repository %q; indexed repositories are %s",
		options.Repository, strings.Join(known, ", "))
}

// list enumerates one visibility class, bounds the merged result, and reports
// whether the graph held more entries than the bound allows.
func (c *Catalog) list(ctx context.Context, kinds []graph.NodeKind, visibility graph.NodeVisibility,
	options CatalogOptions, limit int) ([]graph.ScopedNode, bool, error) {
	scopedNodes, err := c.repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: kinds, Name: options.Name, Repository: options.Repository,
		Visibility: visibility, Limit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	scopedNodes = uniqueScopedNodes(scopedNodes)
	sortScopedNodes(scopedNodes)
	if len(scopedNodes) > limit {
		return scopedNodes[:limit], true, nil
	}
	return scopedNodes, false, nil
}

func (c *Catalog) resolveDataResource(ctx context.Context, selector string, options CatalogOptions) (graph.ScopedNode, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return graph.ScopedNode{}, fmt.Errorf("a data resource name or node ID is required")
	}
	if strings.HasPrefix(selector, "n:") {
		node, err := c.repository.Node(ctx, selector)
		if err != nil {
			return graph.ScopedNode{}, fmt.Errorf("%w: %s", ErrNotFound, selector)
		}
		if !graph.IsDataResourceKind(node.Kind) {
			return graph.ScopedNode{}, fmt.Errorf("node %s is a %s, not a data resource", selector, node.Kind)
		}
		return graph.ScopedNode{Node: node}, nil
	}
	// Resolution is deliberately unbounded: a bound applied before exact
	// matching could hide the one exact match, or hide a second one and report
	// an ambiguous name as unambiguous.
	candidates, err := c.repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: graph.DataResourceKinds(), Name: selector, Repository: options.Repository,
		Visibility: graph.AllNodes,
	})
	if err != nil {
		return graph.ScopedNode{}, err
	}
	var qualified, named []graph.ScopedNode
	for _, scoped := range candidates {
		if strings.EqualFold(scoped.Node.QualifiedName, selector) {
			qualified = append(qualified, scoped)
		} else if strings.EqualFold(scoped.Node.Name, selector) {
			named = append(named, scoped)
		}
	}
	matched := qualified
	if len(matched) == 0 {
		matched = named
	}
	// An unresolved external target never shadows an indexed declaration of
	// the same name; both remain reachable by node ID.
	if declared := localOnly(matched); len(declared) > 0 {
		matched = declared
	}
	sortScopedNodes(matched)
	switch len(matched) {
	case 0:
		return graph.ScopedNode{}, fmt.Errorf("%w: %s", ErrNotFound, selector)
	case 1:
		return matched[0], nil
	default:
		nodes := make([]graph.Node, 0, len(matched))
		for _, scoped := range matched {
			nodes = append(nodes, scoped.Node)
		}
		return graph.ScopedNode{}, &AmbiguousError{Term: selector, Candidates: nodes}
	}
}

func (c *Catalog) configKey(ctx context.Context, scoped graph.ScopedNode, limit int) (ConfigKey, bool, error) {
	resource := newResource(scoped)
	properties, withheld := classifyConfigProperties(scoped.Node.Properties)
	resource.Properties = properties
	key := ConfigKey{Resource: resource, WithheldProperties: withheld,
		Format:      scoped.Node.Properties["format"],
		Section:     scoped.Node.Properties["section"],
		Defined:     scoped.Node.Properties["defined"] == "true",
		Definitions: []UsageSite{}, Readers: []UsageSite{}, References: []UsageSite{}}
	sites, truncated, err := c.incoming(ctx, scoped.Node.ID, limit,
		graph.EdgeDefines, graph.EdgeReadsConfig, graph.EdgeReferences)
	if err != nil {
		return ConfigKey{}, false, err
	}
	for _, site := range sites {
		switch site.Relation {
		case graph.EdgeDefines:
			key.Definitions = append(key.Definitions, site)
		case graph.EdgeReadsConfig:
			key.Readers = append(key.Readers, site)
		default:
			key.References = append(key.References, site)
		}
	}
	return key, truncated, nil
}

func (c *Catalog) event(ctx context.Context, scoped graph.ScopedNode, limit int) (Event, bool, error) {
	event := Event{Resource: newResource(scoped),
		Form:         scoped.Node.Properties["form"],
		Parameters:   scoped.Node.Properties["parameters"],
		Declarations: []UsageSite{}, Producers: []UsageSite{},
		Consumers: []UsageSite{}, Handlers: []UsageSite{}}
	incoming, truncated, err := c.incoming(ctx, scoped.Node.ID, limit,
		graph.EdgeDeclares, graph.EdgePublishes, graph.EdgeSubscribes)
	if err != nil {
		return Event{}, false, err
	}
	for _, site := range incoming {
		switch site.Relation {
		case graph.EdgeDeclares:
			event.Declarations = append(event.Declarations, site)
		case graph.EdgePublishes:
			event.Producers = append(event.Producers, site)
		default:
			event.Consumers = append(event.Consumers, site)
		}
	}
	handlers, handlersTruncated, err := c.outgoing(ctx, scoped.Node.ID, limit, graph.EdgeHandledBy)
	if err != nil {
		return Event{}, false, err
	}
	event.Handlers = handlers
	return event, truncated || handlersTruncated, nil
}

// unresolvedEventIndex groups unresolved external event targets by the names a
// declaration could match, using the same name equivalence as edge resolution.
// It is deliberately unbounded and ignores the caller's name filter: a partial
// index would report a confirmed orphan where uncertainty exists.
func (c *Catalog) unresolvedEventIndex(ctx context.Context, options CatalogOptions) (map[string][]graph.Node, error) {
	scopedNodes, err := c.repository.ListNodesByKind(ctx, graph.NodeListQuery{
		Kinds: []graph.NodeKind{graph.KindEvent}, Repository: options.Repository,
		Visibility: graph.ExternalNodes,
	})
	if err != nil {
		return nil, err
	}
	index := map[string][]graph.Node{}
	for _, scoped := range scopedNodes {
		for _, key := range []string{scoped.Node.QualifiedName, scoped.Node.Name} {
			if key == "" {
				continue
			}
			index[key] = append(index[key], scoped.Node)
		}
	}
	return index, nil
}

func (c *Catalog) unresolvedCounterparts(ctx context.Context, event Event, index map[string][]graph.Node) ([]UsageSite, bool, error) {
	resolved := map[string]bool{}
	for _, group := range [][]UsageSite{event.Producers, event.Consumers, event.Handlers} {
		for _, site := range group {
			if site.FactID != "" {
				resolved[site.FactID] = true
			}
		}
	}
	seen := map[string]bool{}
	var candidates []graph.Node
	for _, key := range []string{event.QualifiedName, event.Name} {
		for _, node := range index[key] {
			if seen[node.ID] {
				continue
			}
			seen[node.ID] = true
			candidates = append(candidates, node)
		}
	}
	var result []UsageSite
	truncated := false
	for _, candidate := range candidates {
		sites, candidateTruncated, err := c.incoming(ctx, candidate.ID, MaxCatalogLimit, graph.EdgePublishes, graph.EdgeSubscribes)
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || candidateTruncated
		for _, site := range sites {
			// A federated edge carries the same fact identity as the
			// unresolved edge it replaced, so counting it again would report
			// resolved evidence as uncertainty.
			if site.FactID != "" && resolved[site.FactID] {
				continue
			}
			result = append(result, site)
		}
	}
	sortUsageSites(result)
	return result, truncated, nil
}

func (c *Catalog) incoming(ctx context.Context, id string, limit int, relations ...graph.EdgeKind) ([]UsageSite, bool, error) {
	return c.sites(ctx, graph.RelationEdgeQuery{SubjectID: id, Direction: graph.IncomingRelations,
		Relations: relations, Limit: limit})
}

func (c *Catalog) outgoing(ctx context.Context, id string, limit int, relations ...graph.EdgeKind) ([]UsageSite, bool, error) {
	return c.sites(ctx, graph.RelationEdgeQuery{SubjectID: id, Direction: graph.OutgoingRelations,
		Relations: relations, Limit: limit})
}

// sites converts already bounded, deduplicated, and hydrated relation edges
// into public evidence without any traversal adjacency or per-edge node reads.
func (c *Catalog) sites(ctx context.Context, request graph.RelationEdgeQuery) ([]UsageSite, bool, error) {
	page, err := c.repository.RelationEdges(ctx, request)
	if err != nil {
		return nil, false, err
	}
	result := make([]UsageSite, 0, len(page.Items))
	for _, item := range page.Items {
		edge := item.Edge
		site := UsageSite{Relation: edge.Kind, EdgeID: edge.ID, FactID: edge.FactID, Node: item.Counterpart,
			Location: edge.Location, Federated: edge.Properties["federated"] == "true"}
		if len(edge.Properties) > 0 {
			site.Evidence = edge.Properties
		}
		result = append(result, site)
	}
	sortUsageSites(result)
	return result, page.Truncated, nil
}

func newResource(scoped graph.ScopedNode) Resource {
	node := scoped.Node
	return Resource{Repository: scoped.Repository, ID: node.ID, Kind: node.Kind, Name: node.Name,
		QualifiedName: node.QualifiedName, ObjectKind: node.Properties["object_kind"],
		Dialect: node.Properties["dialect"], Persistence: node.Properties["persistence"],
		Language: node.Language, Location: node.Location, Unresolved: node.External,
		Properties: node.Properties}
}

// classifyConfigProperties keeps only properties classified as non-secret
// configuration metadata and reports the names of the rest, so a caller learns
// that something was withheld without learning its value.
func classifyConfigProperties(properties map[string]string) (map[string]string, []string) {
	if len(properties) == 0 {
		return nil, nil
	}
	kept := map[string]string{}
	var withheld []string
	for _, name := range graph.SortedPropertyKeys(properties) {
		if configMetadataProperties[name] {
			kept[name] = properties[name]
			continue
		}
		withheld = append(withheld, name)
	}
	if len(kept) == 0 {
		kept = nil
	}
	return kept, withheld
}

func uniqueScopedNodes(scopedNodes []graph.ScopedNode) []graph.ScopedNode {
	seen := make(map[string]bool, len(scopedNodes))
	result := make([]graph.ScopedNode, 0, len(scopedNodes))
	for _, scoped := range scopedNodes {
		if seen[scoped.Node.ID] {
			continue
		}
		seen[scoped.Node.ID] = true
		result = append(result, scoped)
	}
	return result
}

func localOnly(scopedNodes []graph.ScopedNode) []graph.ScopedNode {
	var result []graph.ScopedNode
	for _, scoped := range scopedNodes {
		if !scoped.Node.External {
			result = append(result, scoped)
		}
	}
	return result
}

func sortScopedNodes(scopedNodes []graph.ScopedNode) {
	sort.Slice(scopedNodes, func(i, j int) bool {
		left, right := scopedNodes[i], scopedNodes[j]
		if left.Node.QualifiedName != right.Node.QualifiedName {
			return left.Node.QualifiedName < right.Node.QualifiedName
		}
		if left.Node.Kind != right.Node.Kind {
			return left.Node.Kind < right.Node.Kind
		}
		if left.Repository != right.Repository {
			return left.Repository < right.Repository
		}
		return left.Node.ID < right.Node.ID
	})
}

func sortUsageSites(sites []UsageSite) {
	sort.Slice(sites, func(i, j int) bool {
		left, right := sites[i], sites[j]
		if left.Relation != right.Relation {
			return left.Relation < right.Relation
		}
		if left.Node.QualifiedName != right.Node.QualifiedName {
			return left.Node.QualifiedName < right.Node.QualifiedName
		}
		if left.Node.ID != right.Node.ID {
			return left.Node.ID < right.Node.ID
		}
		return left.EdgeID < right.EdgeID
	})
}

func joinKinds(kinds []graph.NodeKind) string {
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		values = append(values, string(kind))
	}
	return strings.Join(values, ", ")
}
