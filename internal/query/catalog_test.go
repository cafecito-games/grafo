package query_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestDataResourcesCatalogsTablesAndViews(t *testing.T) {
	repository := newCatalogFixture()
	catalog := query.NewCatalog(repository)
	result, err := catalog.DataResources(context.Background(), nil, query.CatalogOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 2 {
		t.Fatalf("expected the table and the view, got %#v", result.Resources)
	}
	if result.Resources[0].QualifiedName != "order_summary" || result.Resources[1].QualifiedName != "orders" {
		t.Fatalf("catalog is not name ordered: %#v", result.Resources)
	}
	orders := result.Resources[1]
	if orders.Kind != graph.KindTable || orders.ObjectKind != "table" || orders.Dialect != "sqlite" {
		t.Fatalf("normalized metadata missing: %#v", orders)
	}
	if orders.Repository != "checkout" {
		t.Fatalf("repository attribution missing: %#v", orders)
	}
	if len(result.Unresolved) != 1 || result.Unresolved[0].QualifiedName != "archived_orders" {
		t.Fatalf("unresolved resources are not explicit: %#v", result.Unresolved)
	}
	if !result.Unresolved[0].Unresolved {
		t.Fatalf("unresolved resource is not marked: %#v", result.Unresolved[0])
	}
	if result.Truncated {
		t.Fatalf("unexpected truncation: %#v", result)
	}

	repeated, err := catalog.DataResources(context.Background(), nil, query.CatalogOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, repeated) {
		t.Fatalf("catalog is not deterministic:\n%#v\n%#v", result, repeated)
	}
}

func TestDataResourcesRejectsUnsupportedKindAndRepository(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	if _, err := catalog.DataResources(context.Background(), []graph.NodeKind{graph.KindFunction}, query.CatalogOptions{}); err == nil {
		t.Fatal("an unsupported node kind must be rejected, not answered with an empty catalog")
	}
	if _, err := catalog.DataResources(context.Background(), nil, query.CatalogOptions{Repository: "billing"}); err == nil {
		t.Fatal("an unknown repository filter must be rejected")
	}
}

func TestDataResourcesReportsTruncation(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	result, err := catalog.DataResources(context.Background(), nil, query.CatalogOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 || !result.Truncated {
		t.Fatalf("bounded catalog did not report truncation: %#v", result)
	}
}

func TestDataResourceUsageSeparatesReadersAndWriters(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	usage, err := catalog.DataResourceUsage(context.Background(), "orders", query.CatalogOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if usage.Resource.QualifiedName != "orders" {
		t.Fatalf("unexpected resource: %#v", usage.Resource)
	}
	if len(usage.Readers) != 1 || usage.Readers[0].Node.QualifiedName != "shop.ListOrders" {
		t.Fatalf("unexpected readers: %#v", usage.Readers)
	}
	if usage.Readers[0].Location.Path != "shop/list.go" || usage.Readers[0].Location.Line != 12 {
		t.Fatalf("reader evidence is missing its source site: %#v", usage.Readers[0])
	}
	if len(usage.Writers) != 1 || usage.Writers[0].Node.QualifiedName != "shop.CreateOrder" {
		t.Fatalf("unexpected writers: %#v", usage.Writers)
	}
	if len(usage.References) != 1 || usage.References[0].Node.QualifiedName != "orders_by_customer" {
		t.Fatalf("unexpected references: %#v", usage.References)
	}
}

func TestDataResourceUsageRequiresAnUnambiguousName(t *testing.T) {
	repository := newCatalogFixture()
	repository.add("checkout", graph.Node{ID: "n:stock-billing", Kind: graph.KindTable, Name: "stock",
		QualifiedName: "billing.stock", Properties: map[string]string{"object_kind": "table"}})
	repository.add("checkout", graph.Node{ID: "n:stock-warehouse", Kind: graph.KindTable, Name: "stock",
		QualifiedName: "warehouse.stock", Properties: map[string]string{"object_kind": "table"}})
	catalog := query.NewCatalog(repository)
	_, err := catalog.DataResourceUsage(context.Background(), "stock", query.CatalogOptions{})
	var ambiguous *query.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected an ambiguity error, got %v", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Fatalf("expected both candidates, got %#v", ambiguous.Candidates)
	}
	usage, err := catalog.DataResourceUsage(context.Background(), "billing.stock", query.CatalogOptions{})
	if err != nil {
		t.Fatalf("a qualified name must resolve: %v", err)
	}
	if usage.Resource.ID != "n:stock-billing" {
		t.Fatalf("qualified name resolved to the wrong candidate: %#v", usage.Resource)
	}
	byID, err := catalog.DataResourceUsage(context.Background(), "n:stock-warehouse", query.CatalogOptions{})
	if err != nil {
		t.Fatalf("a node ID must resolve: %v", err)
	}
	if byID.Resource.QualifiedName != "warehouse.stock" {
		t.Fatalf("node ID resolved to the wrong candidate: %#v", byID.Resource)
	}
}

func TestDataResourceUsageRejectsAnInertNameFilter(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	_, err := catalog.DataResourceUsage(context.Background(), "orders",
		query.CatalogOptions{Name: "nomatch"})
	if err == nil {
		t.Fatal("a name filter that cannot narrow one resource must be rejected, not ignored")
	}
}

func TestBlankNameFilterMeansTheSameAtEverySurface(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	blank, err := catalog.DataResources(context.Background(), nil, query.CatalogOptions{Name: "   ", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	absent, err := catalog.DataResources(context.Background(), nil, query.CatalogOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(blank, absent) {
		t.Fatalf("a blank name filter narrowed the catalog:\n%#v\n%#v", blank, absent)
	}
	if _, err := catalog.DataResourceUsage(context.Background(), "orders",
		query.CatalogOptions{Name: "   "}); err != nil {
		t.Fatalf("a blank name filter must be treated as absent: %v", err)
	}
}

func TestDataResourceUsageRejectsNonResourceSelectors(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	if _, err := catalog.DataResourceUsage(context.Background(), "shop.ListOrders", query.CatalogOptions{}); err == nil {
		t.Fatal("a function selector must be rejected as a data resource")
	}
	if _, err := catalog.DataResourceUsage(context.Background(), "missing_table", query.CatalogOptions{}); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

func TestConfigKeysReportDefinitionsAndReadersWithoutValues(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	result, err := catalog.ConfigKeys(context.Background(), query.CatalogOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Keys) != 1 {
		t.Fatalf("expected one defined config key, got %#v", result.Keys)
	}
	key := result.Keys[0]
	if key.Name != "DATABASE_URL" || key.Format != "env" || !key.Defined {
		t.Fatalf("normalized config metadata missing: %#v", key)
	}
	if len(key.Definitions) != 1 || key.Definitions[0].Node.QualifiedName != ".env" {
		t.Fatalf("unexpected definitions: %#v", key.Definitions)
	}
	if len(key.Readers) != 1 || key.Readers[0].Node.QualifiedName != "shop.Connect" {
		t.Fatalf("unexpected readers: %#v", key.Readers)
	}
	for name, value := range key.Properties {
		if name == "value" || value == "postgres://user:secret@host/db" {
			t.Fatalf("config catalog leaked a stored value: %#v", key.Properties)
		}
	}
	if !reflect.DeepEqual(key.WithheldProperties, []string{"value"}) {
		t.Fatalf("withheld properties are not reported: %#v", key.WithheldProperties)
	}
	if len(result.Unresolved) != 1 || result.Unresolved[0].QualifiedName != "FEATURE_FLAG_URL" {
		t.Fatalf("unresolved config references are not explicit: %#v", result.Unresolved)
	}
	if len(result.Unresolved[0].Readers) != 1 {
		t.Fatalf("unresolved config reference lost its reader evidence: %#v", result.Unresolved[0])
	}
}

func TestEventsReportProducersConsumersHandlersAndDeclarations(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	result, err := catalog.Events(context.Background(), query.CatalogOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]query.Event{}
	for _, event := range result.Events {
		byName[event.QualifiedName] = event
	}
	balanced, ok := byName["shop.OrderPlaced"]
	if !ok {
		t.Fatalf("missing event: %#v", result.Events)
	}
	if len(balanced.Declarations) != 1 || balanced.Declarations[0].Node.QualifiedName != "shop.Cart" {
		t.Fatalf("unexpected declarations: %#v", balanced.Declarations)
	}
	if len(balanced.Producers) != 1 || balanced.Producers[0].Node.QualifiedName != "shop.Checkout" {
		t.Fatalf("unexpected producers: %#v", balanced.Producers)
	}
	if len(balanced.Consumers) != 1 || balanced.Consumers[0].Node.QualifiedName != "shop.Mailer" {
		t.Fatalf("unexpected consumers: %#v", balanced.Consumers)
	}
	if len(balanced.Handlers) != 1 || balanced.Handlers[0].Node.QualifiedName != "shop.Mailer.Send" {
		t.Fatalf("unexpected handlers: %#v", balanced.Handlers)
	}
	if balanced.Form != "signal" {
		t.Fatalf("normalized event metadata missing: %#v", balanced)
	}
	if len(result.Unresolved) == 0 {
		t.Fatalf("unresolved events are not explicit: %#v", result)
	}
}

func TestOrphanedEventsClassifiesEveryCategory(t *testing.T) {
	catalog := query.NewCatalog(newCatalogFixture())
	result, err := catalog.OrphanedEvents(context.Background(), query.CatalogOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]query.OrphanStatus{}
	category := map[string]query.OrphanCategory{}
	for _, orphan := range result.Events {
		status[orphan.Event.QualifiedName] = orphan.Status
		category[orphan.Event.QualifiedName] = orphan.Category
	}
	if _, present := status["shop.OrderPlaced"]; present {
		t.Fatalf("a produced and consumed event is not an orphan: %#v", result.Events)
	}
	expected := map[string]query.OrphanCategory{
		"shop.CartCleared":   query.PublishedWithoutConsumer,
		"shop.StockChecked":  query.ConsumedWithoutProducer,
		"shop.LegacyPruned":  query.DeclaredWithNeither,
		"shop.PaymentFailed": query.PublishedWithoutConsumer,
	}
	for name, want := range expected {
		if category[name] != want {
			t.Fatalf("event %s category = %q, want %q", name, category[name], want)
		}
	}
	for name, want := range map[string]query.OrphanStatus{
		"shop.CartCleared":   query.OrphanConfirmed,
		"shop.StockChecked":  query.OrphanConfirmed,
		"shop.LegacyPruned":  query.OrphanConfirmed,
		"shop.PaymentFailed": query.OrphanUnknown,
	} {
		if status[name] != want {
			t.Fatalf("event %s status = %q, want %q", name, status[name], want)
		}
	}
	for _, orphan := range result.Events {
		if orphan.Event.QualifiedName != "shop.PaymentFailed" {
			continue
		}
		if orphan.UnresolvedConsumers != 1 || len(orphan.UnresolvedCounterparts) != 1 {
			t.Fatalf("unresolved counterpart evidence is missing: %#v", orphan)
		}
		if orphan.UnresolvedCounterparts[0].Node.QualifiedName != "shop.Auditor" {
			t.Fatalf("unexpected counterpart evidence: %#v", orphan.UnresolvedCounterparts)
		}
	}
}

func TestOrphanedEventsNeverConfirmAnUnresolvedName(t *testing.T) {
	repository := newCatalogFixture()
	repository.add("checkout", graph.Node{ID: "n:external-shipped", Kind: graph.KindEvent,
		Name: "order.shipped", QualifiedName: "order.shipped", External: true,
		Properties: map[string]string{"unresolved": "true"}})
	repository.edges = append(repository.edges, graph.Edge{ID: "e:publish-shipped",
		FactID: "f:publish-shipped", FromID: "n:checkout", ToID: "n:external-shipped",
		Kind: graph.EdgePublishes})
	result, err := query.NewCatalog(repository).OrphanedEvents(context.Background(), query.CatalogOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, orphan := range result.Events {
		if orphan.Event.QualifiedName != "order.shipped" {
			continue
		}
		found = true
		if !orphan.Event.Unresolved {
			t.Fatalf("unresolved event is not marked: %#v", orphan.Event)
		}
		if orphan.Category != query.PublishedWithoutConsumer || orphan.Status != query.OrphanUnknown {
			t.Fatalf("an unresolved name must never be a confirmed orphan: %#v", orphan)
		}
	}
	if !found {
		t.Fatalf("unresolved event name is missing from the report: %#v", result.Events)
	}
	for _, orphan := range result.Events {
		if orphan.Event.QualifiedName == "PaymentFailed" {
			t.Fatalf("an unresolved target of a declared event was reported twice: %#v", result.Events)
		}
	}
}

func TestOrphanedEventsIgnoreResolvedFederatedCounterparts(t *testing.T) {
	repository := newCatalogFixture()
	// A federated edge repoints the unresolved subscriber at the local event
	// while keeping its fact identity, so the same evidence must not also be
	// counted as an unresolved counterpart.
	repository.edges = append(repository.edges, graph.Edge{ID: "e:federated", FactID: "f:audit-subscribe",
		FromID: "n:auditor", ToID: "n:payment-failed", Kind: graph.EdgeSubscribes,
		Properties: map[string]string{"federated": "true"}})
	result, err := query.NewCatalog(repository).OrphanedEvents(context.Background(), query.CatalogOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, orphan := range result.Events {
		if orphan.Event.QualifiedName == "shop.PaymentFailed" {
			t.Fatalf("a federated consumer must clear the orphan: %#v", orphan)
		}
	}
}

func TestBoundedEvidenceNeverStarvesAnotherRelation(t *testing.T) {
	repository := newCatalogFixture()
	// Edges arrive in kind order, so a shared budget spent on reads would hide
	// every writer behind a truncation flag.
	for index := 0; index < 5; index++ {
		reader := graph.Node{ID: fmt.Sprintf("n:reader-%d", index), Kind: graph.KindFunction,
			Name: fmt.Sprintf("Read%d", index), QualifiedName: fmt.Sprintf("shop.Read%d", index)}
		repository.add("checkout", reader)
		repository.edges = append(repository.edges, graph.Edge{ID: fmt.Sprintf("e:read-%d", index),
			FactID: fmt.Sprintf("f:read-%d", index), FromID: reader.ID, ToID: "n:orders", Kind: graph.EdgeReads})
	}
	sortEdgesByKind(repository)
	usage, err := query.NewCatalog(repository).DataResourceUsage(context.Background(), "orders",
		query.CatalogOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Readers) != 2 || !usage.Truncated {
		t.Fatalf("reader bound was not applied: %#v", usage)
	}
	if len(usage.Writers) != 1 {
		t.Fatalf("readers starved the writer list: %#v", usage)
	}
	if len(usage.References) != 1 {
		t.Fatalf("readers starved the reference list: %#v", usage)
	}
}

func TestTruncatedEvidenceNeverConfirmsAnOrphan(t *testing.T) {
	repository := newCatalogFixture()
	for index := 0; index < 3; index++ {
		publisher := graph.Node{ID: fmt.Sprintf("n:publisher-%d", index), Kind: graph.KindFunction,
			Name: fmt.Sprintf("Emit%d", index), QualifiedName: fmt.Sprintf("shop.Emit%d", index)}
		repository.add("checkout", publisher)
		repository.edges = append(repository.edges, graph.Edge{ID: fmt.Sprintf("e:publish-cleared-%d", index),
			FactID: fmt.Sprintf("f:publish-cleared-%d", index), FromID: publisher.ID,
			ToID: "n:cart-cleared", Kind: graph.EdgePublishes})
	}
	sortEdgesByKind(repository)
	result, err := query.NewCatalog(repository).OrphanedEvents(context.Background(), query.CatalogOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, orphan := range result.Events {
		if orphan.Event.QualifiedName != "shop.CartCleared" {
			continue
		}
		if orphan.Status != query.OrphanUnknown {
			t.Fatalf("truncated evidence produced a confirmed orphan: %#v", orphan)
		}
	}
	if !result.Truncated {
		t.Fatalf("truncation was not reported: %#v", result)
	}
}

func TestDataResourcesDeduplicateRepeatedKinds(t *testing.T) {
	result, err := query.NewCatalog(newCatalogFixture()).DataResources(context.Background(),
		[]graph.NodeKind{graph.KindTable, graph.KindTable}, query.CatalogOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 || result.Resources[0].QualifiedName != "orders" {
		t.Fatalf("a repeated kind duplicated the catalog: %#v", result.Resources)
	}
}

// sortEdgesByKind mirrors the storage adapter's edge ordering so bound-related
// behavior is exercised the way the repository returns edges.
func sortEdgesByKind(repository *catalogRepository) {
	sort.Slice(repository.edges, func(i, j int) bool {
		left, right := repository.edges[i], repository.edges[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.FromID != right.FromID {
			return left.FromID < right.FromID
		}
		return left.ID < right.ID
	})
}

func newCatalogFixture() *catalogRepository {
	repository := &catalogRepository{nodes: map[string]graph.Node{}, owners: map[string]string{}}
	repository.repositories = []string{"checkout"}

	repository.add("checkout", graph.Node{ID: "n:orders", Kind: graph.KindTable, Name: "orders",
		QualifiedName: "orders", Language: "sql",
		Properties: map[string]string{"object_kind": "table", "dialect": "sqlite"},
		Location:   graph.Location{Path: "schema.sql", Line: 1}})
	repository.add("checkout", graph.Node{ID: "n:summary", Kind: graph.KindView, Name: "order_summary",
		QualifiedName: "order_summary", Language: "sql",
		Properties: map[string]string{"object_kind": "view", "dialect": "sqlite"}})
	repository.add("checkout", graph.Node{ID: "n:archived", Kind: graph.KindTable, Name: "archived_orders",
		QualifiedName: "archived_orders", External: true,
		Properties: map[string]string{"unresolved": "true"}})
	repository.add("checkout", graph.Node{ID: "n:index", Kind: graph.KindIndex, Name: "orders_by_customer",
		QualifiedName: "orders_by_customer"})
	repository.add("checkout", graph.Node{ID: "n:list", Kind: graph.KindFunction, Name: "ListOrders",
		QualifiedName: "shop.ListOrders"})
	repository.add("checkout", graph.Node{ID: "n:create", Kind: graph.KindFunction, Name: "CreateOrder",
		QualifiedName: "shop.CreateOrder"})
	repository.edges = append(repository.edges,
		graph.Edge{ID: "e:read", FactID: "f:read", FromID: "n:list", ToID: "n:orders", Kind: graph.EdgeReads,
			Location: graph.Location{Path: "shop/list.go", Line: 12}},
		graph.Edge{ID: "e:write", FactID: "f:write", FromID: "n:create", ToID: "n:orders", Kind: graph.EdgeWrites,
			Location: graph.Location{Path: "shop/create.go", Line: 30}},
		graph.Edge{ID: "e:index", FactID: "f:index", FromID: "n:index", ToID: "n:orders", Kind: graph.EdgeReferences},
	)

	repository.add("checkout", graph.Node{ID: "n:env", Kind: graph.KindFile, Name: ".env", QualifiedName: ".env"})
	repository.add("checkout", graph.Node{ID: "n:database-url", Kind: graph.KindConfigKey, Name: "DATABASE_URL",
		QualifiedName: "config:.env:DATABASE_URL",
		Properties: map[string]string{"defined": "true", "format": "env",
			"value": "postgres://user:secret@host/db"}})
	repository.add("checkout", graph.Node{ID: "n:flag-url", Kind: graph.KindConfigKey, Name: "FEATURE_FLAG_URL",
		QualifiedName: "FEATURE_FLAG_URL", External: true,
		Properties: map[string]string{"unresolved": "true"}})
	repository.add("checkout", graph.Node{ID: "n:connect", Kind: graph.KindFunction, Name: "Connect",
		QualifiedName: "shop.Connect"})
	repository.edges = append(repository.edges,
		graph.Edge{ID: "e:defines", FactID: "f:defines", FromID: "n:env", ToID: "n:database-url", Kind: graph.EdgeDefines},
		graph.Edge{ID: "e:reads-config", FactID: "f:reads-config", FromID: "n:connect", ToID: "n:database-url",
			Kind: graph.EdgeReadsConfig, Location: graph.Location{Path: "shop/connect.go", Line: 8}},
		graph.Edge{ID: "e:reads-flag", FactID: "f:reads-flag", FromID: "n:connect", ToID: "n:flag-url",
			Kind: graph.EdgeReadsConfig},
	)

	repository.add("checkout", graph.Node{ID: "n:cart", Kind: graph.KindClass, Name: "Cart", QualifiedName: "shop.Cart"})
	repository.add("checkout", graph.Node{ID: "n:checkout", Kind: graph.KindFunction, Name: "Checkout",
		QualifiedName: "shop.Checkout"})
	repository.add("checkout", graph.Node{ID: "n:mailer", Kind: graph.KindClass, Name: "Mailer",
		QualifiedName: "shop.Mailer"})
	repository.add("checkout", graph.Node{ID: "n:send", Kind: graph.KindMethod, Name: "Send",
		QualifiedName: "shop.Mailer.Send"})
	repository.add("checkout", graph.Node{ID: "n:auditor", Kind: graph.KindClass, Name: "Auditor",
		QualifiedName: "shop.Auditor"})
	repository.add("checkout", graph.Node{ID: "n:stock", Kind: graph.KindClass, Name: "Stock",
		QualifiedName: "shop.Stock"})

	for id, qualified := range map[string]string{"n:order-placed": "shop.OrderPlaced",
		"n:cart-cleared": "shop.CartCleared", "n:stock-checked": "shop.StockChecked",
		"n:legacy-pruned": "shop.LegacyPruned", "n:payment-failed": "shop.PaymentFailed"} {
		repository.add("checkout", graph.Node{ID: id, Kind: graph.KindEvent,
			Name: qualified[strings.LastIndexByte(qualified, '.')+1:], QualifiedName: qualified,
			Properties: map[string]string{"form": "signal"}})
	}
	repository.add("checkout", graph.Node{ID: "n:external-payment-failed", Kind: graph.KindEvent,
		Name: "PaymentFailed", QualifiedName: "PaymentFailed", External: true,
		Properties: map[string]string{"unresolved": "true"}})
	repository.edges = append(repository.edges,
		graph.Edge{ID: "e:declare-placed", FactID: "f:declare-placed", FromID: "n:cart",
			ToID: "n:order-placed", Kind: graph.EdgeDeclares},
		graph.Edge{ID: "e:publish-placed", FactID: "f:publish-placed", FromID: "n:checkout",
			ToID: "n:order-placed", Kind: graph.EdgePublishes},
		graph.Edge{ID: "e:subscribe-placed", FactID: "f:subscribe-placed", FromID: "n:mailer",
			ToID: "n:order-placed", Kind: graph.EdgeSubscribes},
		graph.Edge{ID: "e:handle-placed", FactID: "f:handle-placed", FromID: "n:order-placed",
			ToID: "n:send", Kind: graph.EdgeHandledBy},
		graph.Edge{ID: "e:publish-cleared", FactID: "f:publish-cleared", FromID: "n:checkout",
			ToID: "n:cart-cleared", Kind: graph.EdgePublishes},
		graph.Edge{ID: "e:subscribe-stock", FactID: "f:subscribe-stock", FromID: "n:stock",
			ToID: "n:stock-checked", Kind: graph.EdgeSubscribes},
		graph.Edge{ID: "e:publish-payment", FactID: "f:publish-payment", FromID: "n:checkout",
			ToID: "n:payment-failed", Kind: graph.EdgePublishes},
		graph.Edge{ID: "e:audit-subscribe", FactID: "f:audit-subscribe", FromID: "n:auditor",
			ToID: "n:external-payment-failed", Kind: graph.EdgeSubscribes},
	)
	return repository
}

type catalogRepository struct {
	nodes        map[string]graph.Node
	owners       map[string]string
	edges        []graph.Edge
	repositories []string
}

func (c *catalogRepository) add(repository string, node graph.Node) {
	c.nodes[node.ID] = node
	c.owners[node.ID] = repository
}

func (c *catalogRepository) Repositories(context.Context) ([]string, error) {
	return append([]string(nil), c.repositories...), nil
}

func (c *catalogRepository) ListNodesByKind(_ context.Context, request graph.NodeListQuery) ([]graph.ScopedNode, error) {
	var result []graph.ScopedNode
	for _, kind := range request.Kinds {
		var matched []graph.ScopedNode
		for _, node := range c.nodes {
			if node.Kind != kind {
				continue
			}
			switch request.Visibility {
			case graph.ExternalNodes:
				if !node.External {
					continue
				}
			case graph.AllNodes:
			default:
				if node.External {
					continue
				}
			}
			if request.Repository != "" && c.owners[node.ID] != request.Repository {
				continue
			}
			if request.Name != "" && !strings.Contains(strings.ToLower(node.Name), strings.ToLower(request.Name)) &&
				!strings.Contains(strings.ToLower(node.QualifiedName), strings.ToLower(request.Name)) {
				continue
			}
			matched = append(matched, graph.ScopedNode{Repository: c.owners[node.ID], Node: node})
		}
		sort.Slice(matched, func(i, j int) bool {
			if matched[i].Node.QualifiedName != matched[j].Node.QualifiedName {
				return matched[i].Node.QualifiedName < matched[j].Node.QualifiedName
			}
			return matched[i].Node.ID < matched[j].Node.ID
		})
		if request.Limit > 0 && len(matched) > request.Limit {
			matched = matched[:request.Limit]
		}
		result = append(result, matched...)
	}
	return result, nil
}

func (c *catalogRepository) SearchNodes(_ context.Context, term string, limit int) ([]graph.Node, error) {
	var result []graph.Node
	for _, node := range c.nodes {
		if strings.Contains(strings.ToLower(node.QualifiedName), strings.ToLower(term)) {
			result = append(result, node)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (c *catalogRepository) Node(_ context.Context, id string) (graph.Node, error) {
	node, ok := c.nodes[id]
	if !ok {
		return graph.Node{}, errors.New("not found")
	}
	return node, nil
}

func (c *catalogRepository) EdgesFrom(_ context.Context, id string) ([]graph.Edge, error) {
	var result []graph.Edge
	for _, edge := range c.edges {
		if edge.FromID == id {
			result = append(result, edge)
		}
	}
	return result, nil
}

func (c *catalogRepository) EdgesTo(_ context.Context, id string) ([]graph.Edge, error) {
	var result []graph.Edge
	for _, edge := range c.edges {
		if edge.ToID == id {
			result = append(result, edge)
		}
	}
	return result, nil
}
