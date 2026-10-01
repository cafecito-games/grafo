package federation_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

var _ graph.CatalogRepository = (*federation.Repository)(nil)
var _ graph.CanonicalMessageRepository = (*federation.Repository)(nil)

func TestFederatedCanonicalMessagesPreserveRepositoryQualifiedCollisions(t *testing.T) {
	ctx := context.Background()
	workspace := testtemp.Dir(t)
	roots := []string{filepath.Join(workspace, "alpha"), filepath.Join(workspace, "beta")}
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		seedCatalogIndex(t, ctx, root, "schema.proto", graph.ParseResult{Nodes: []graph.Node{
			{ID: "shared-message-id", Kind: graph.KindType, Name: "Envelope", QualifiedName: "acme.v1.Envelope",
				OwnerFile: "schema.proto", Location: graph.Location{Path: "proto/schema.proto", Line: 1},
				Properties: map[string]string{"declaration": "message"}},
			{ID: "ordinary-" + filepath.Base(root), Kind: graph.KindType, Name: "Ordinary", QualifiedName: "aaa.Ordinary",
				OwnerFile: "schema.proto", Properties: map[string]string{"declaration": "struct"}},
			{ID: "shared-component-id", Kind: graph.KindComponent, Name: filepath.Base(root),
				QualifiedName: "component:" + filepath.Base(root), OwnerFile: "__workspace__"},
		}})
	}
	repository, err := federation.Open(ctx, roots)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })

	page, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{Package: "acme.v1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Truncated || len(page.Items) != 2 || page.Items[0].Repository != "alpha" || page.Items[1].Repository != "beta" {
		t.Fatalf("repository-qualified messages = %#v", page)
	}
	pathPage, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{PathPrefixes: []string{"proto"}, Limit: 10})
	if err != nil || pathPage.Truncated || len(pathPage.Items) != 2 {
		t.Fatalf("repository-relative path-filtered messages = %#v, %v", pathPage, err)
	}
	unmatched, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{PathPrefixes: []string{"protocol"}, Limit: 10})
	if err != nil || unmatched.Truncated || len(unmatched.Items) != 0 {
		t.Fatalf("unmatched canonical path page = %#v, %v", unmatched, err)
	}
	bounded, err := repository.CanonicalMessages(ctx, graph.CanonicalMessageQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated || len(bounded.Items) != 1 || bounded.Items[0].Repository != "alpha" {
		t.Fatalf("globally bounded messages = %#v", bounded)
	}
	components, err := repository.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindComponent}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 || components[0].Repository != "alpha" || components[1].Repository != "beta" {
		t.Fatalf("repository-qualified component IDs = %#v", components)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.CanonicalMessages(cancelled, graph.CanonicalMessageQuery{Limit: 10}); err == nil {
		t.Fatal("cancelled canonical message enumeration succeeded")
	}
}

func TestCatalogSharesOneContractAcrossRepositories(t *testing.T) {
	ctx := context.Background()
	workspace := testtemp.Dir(t)
	producerRoot := filepath.Join(workspace, "producer")
	consumerRoot := filepath.Join(workspace, "consumer")
	for _, root := range []string{producerRoot, consumerRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  default_dialect: sqlite\n")
	}
	write(t, filepath.Join(producerRoot, "schema.sql"), "CREATE TABLE orders (id INTEGER PRIMARY KEY);\n")
	write(t, filepath.Join(producerRoot, "producer.go"), `package producer
func Ship(bus Bus) { bus.Publish("order.shipped") }
type Bus interface{ Publish(string) }
`)
	write(t, filepath.Join(consumerRoot, "consumer.go"), `package consumer
func Watch(bus Bus) { bus.Subscribe("order.shipped") }
type Bus interface{ Subscribe(string) }
`)
	indexWithDefaults(t, ctx, producerRoot)
	indexWithDefaults(t, ctx, consumerRoot)

	single, err := sqlite.Open(ctx, indexPath(t, ctx, producerRoot))
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := query.NewCatalog(single).OrphanedEvents(ctx, query.CatalogOptions{})
	if err != nil {
		_ = single.Close()
		t.Fatal(err)
	}
	if err := single.Close(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, orphan := range isolated.Events {
		if orphan.Event.QualifiedName != "order.shipped" {
			continue
		}
		found = true
		if orphan.Category != query.PublishedWithoutConsumer || orphan.Status != query.OrphanUnknown {
			t.Fatalf("a single repository must report an unresolved name as unknown: %#v", orphan)
		}
	}
	if !found {
		t.Fatalf("the published event is missing from the single-repository report: %#v", isolated.Events)
	}

	repository, err := federation.Open(ctx, []string{producerRoot, consumerRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	catalog := query.NewCatalog(repository)

	names, err := repository.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "consumer" || names[1] != "producer" {
		t.Fatalf("unexpected federated repositories: %#v", names)
	}

	resources, err := catalog.DataResources(ctx, nil, query.CatalogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].Repository != "producer" {
		t.Fatalf("federated resource attribution is wrong: %#v", resources.Resources)
	}
	filtered, err := catalog.DataResources(ctx, nil, query.CatalogOptions{Repository: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Resources) != 0 {
		t.Fatalf("repository filter leaked a peer's resources: %#v", filtered.Resources)
	}
	pathFiltered, err := catalog.DataResources(ctx, nil, query.CatalogOptions{PathPrefixes: []string{"schema.sql"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pathFiltered.Resources) != 1 || pathFiltered.Resources[0].Repository != "producer" {
		t.Fatalf("federated relative path filter = %#v", pathFiltered)
	}

	events, err := catalog.Events(ctx, query.CatalogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var shipped *query.Event
	for index, event := range events.Unresolved {
		if event.QualifiedName == "order.shipped" {
			shipped = &events.Unresolved[index]
		}
	}
	if shipped == nil {
		t.Fatalf("the federated event is missing: %#v", events)
	}
	if shipped.Repository != "" {
		t.Fatalf("an unresolved name must not be attributed to one repository: %#v", shipped)
	}
	if len(shipped.Producers) != 1 || len(shipped.Consumers) != 1 {
		t.Fatalf("federation did not join the producer and the consumer: %#v", shipped)
	}

	orphans, err := catalog.OrphanedEvents(ctx, query.CatalogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, orphan := range orphans.Events {
		if orphan.Event.QualifiedName == "order.shipped" {
			t.Fatalf("a federated consumer must clear the orphan: %#v", orphan)
		}
	}
}

func TestFederatedRelationEdgesProjectThenApplyGlobalBounds(t *testing.T) {
	ctx := context.Background()
	workspace := testtemp.Dir(t)
	producerRoot := filepath.Join(workspace, "producer")
	consumerRoot := filepath.Join(workspace, "consumer")
	for _, root := range []string{producerRoot, consumerRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := graph.Node{ID: graph.NodeID(graph.KindTable, "public.orders"), Kind: graph.KindTable, Name: "orders",
		QualifiedName: "public.orders", OwnerFile: "schema.sql"}
	source := graph.Node{ID: "n:consumer", Kind: graph.KindFunction, Name: "Consume",
		QualifiedName: "consumer.Consume", OwnerFile: "consumer.go"}
	seedCatalogIndex(t, ctx, producerRoot, "schema.sql", graph.ParseResult{
		Nodes: []graph.Node{target, source},
		Facts: []graph.Fact{{ID: "read-a", FromID: source.ID, Kind: graph.EdgeReads,
			TargetID: target.ID, OwnerFile: "schema.sql"}},
	})
	facts := []graph.Fact{
		{ID: "read-c", FromID: source.ID, Kind: graph.EdgeReads, Target: target.QualifiedName,
			TargetKind: graph.KindTable, OwnerFile: "consumer.go"},
		{ID: "read-a", FromID: source.ID, Kind: graph.EdgeReads, Target: target.QualifiedName,
			TargetKind: graph.KindTable, OwnerFile: "consumer.go"},
		{ID: "read-b", FromID: source.ID, Kind: graph.EdgeReads, Target: target.QualifiedName,
			TargetKind: graph.KindTable, OwnerFile: "consumer.go"},
		{ID: "write", FromID: source.ID, Kind: graph.EdgeWrites, Target: target.QualifiedName,
			OwnerFile: "consumer.go"},
	}
	seedCatalogIndex(t, ctx, consumerRoot, "consumer.go", graph.ParseResult{Nodes: []graph.Node{source}, Facts: facts})

	repository, err := federation.Open(ctx, []string{producerRoot, consumerRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	incoming, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: target.ID,
		Direction: graph.IncomingRelations,
		Relations: []graph.EdgeKind{graph.EdgeReads, graph.EdgeReads, graph.EdgeWrites}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !incoming.Truncated || len(incoming.Items) != 3 {
		t.Fatalf("incoming = %#v", incoming)
	}
	seenEdges := map[string]bool{}
	for _, item := range incoming.Items {
		if seenEdges[item.Edge.ID] {
			t.Fatalf("duplicate final edge = %#v", item.Edge)
		}
		seenEdges[item.Edge.ID] = true
		if item.Edge.ToID != target.ID || item.Counterpart.ID != source.ID ||
			item.Edge.Properties["federated"] != "true" {
			t.Fatalf("incoming projection = %#v", item)
		}
	}
	outgoing, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: source.ID,
		Direction: graph.OutgoingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !outgoing.Truncated || len(outgoing.Items) != 2 {
		t.Fatalf("outgoing = %#v", outgoing)
	}
	federated := 0
	for _, item := range outgoing.Items {
		if item.Edge.ToID != target.ID || item.Counterpart.ID != target.ID {
			t.Fatalf("outgoing projection = %#v", item)
		}
		if item.Edge.Properties["federated"] == "true" {
			federated++
		}
	}
	if federated == 0 {
		t.Fatalf("outgoing evidence lost its federation markers: %#v", outgoing)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.RelationEdges(cancelled, graph.RelationEdgeQuery{SubjectID: target.ID,
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeReads}, Limit: 1}); err == nil {
		t.Fatal("member cancellation returned a partial page")
	}
}

func TestFederatedGodotInteractionsPreserveAndEnforceProducer(t *testing.T) {
	ctx := context.Background()
	workspace := testtemp.Dir(t)
	declarationsRoot := filepath.Join(workspace, "declarations")
	clientRoot := filepath.Join(workspace, "client")
	for _, root := range []string{declarationsRoot, clientRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	action := graph.Node{ID: graph.NodeID(graph.KindGodotInputAction, "godot:input_action:project.godot:jump"),
		Kind: graph.KindGodotInputAction, Name: "jump",
		QualifiedName: "godot:input_action:project.godot:jump", OwnerFile: "project.godot"}
	seedCatalogIndex(t, ctx, declarationsRoot, "project.godot", graph.ParseResult{Nodes: []graph.Node{action}})
	accepted := graph.Node{ID: "n:accepted", Kind: graph.KindMethod, Name: "poll",
		QualifiedName: "Player.poll", OwnerFile: "player.gd"}
	rejected := graph.Node{ID: "n:rejected", Kind: graph.KindMethod, Name: "poll",
		QualifiedName: "Forged.poll", OwnerFile: "forged.go"}
	seedCatalogIndex(t, ctx, clientRoot, "client", graph.ParseResult{
		Nodes: []graph.Node{accepted, rejected},
		Facts: []graph.Fact{
			{ID: "f:accepted", FromID: accepted.ID, Kind: graph.EdgeUsesInputAction,
				Producer: graph.ProducerGDScript, Target: action.QualifiedName, TargetKind: action.Kind,
				Properties: map[string]string{"form": "query"}, OwnerFile: "player.gd"},
			{ID: "f:rejected", FromID: rejected.ID, Kind: graph.EdgeUsesInputAction,
				Producer: "go", Target: action.QualifiedName, TargetKind: action.Kind,
				Properties: map[string]string{"form": "query"}, OwnerFile: "forged.go"},
		},
	})

	repository, err := federation.Open(ctx, []string{declarationsRoot, clientRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	acceptedReport, err := query.NewService(repository).GodotInteractions(ctx, accepted.QualifiedName,
		query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(acceptedReport.Outbound) != 1 || !acceptedReport.Outbound[0].Federated ||
		acceptedReport.Outbound[0].Edge.Producer != graph.ProducerGDScript ||
		acceptedReport.Outbound[0].Node.ID != action.ID {
		t.Fatalf("accepted federated interaction = %#v", acceptedReport.Outbound)
	}
	rejectedReport, err := query.NewService(repository).GodotInteractions(ctx, rejected.QualifiedName,
		query.GodotInteractionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rejectedReport.Outbound) != 0 || len(rejectedReport.Inbound) != 0 {
		t.Fatalf("non-Godot producer crossed federated gate: %#v", rejectedReport)
	}
}

func seedCatalogIndex(t *testing.T, ctx context.Context, root, owner string, result graph.ParseResult) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	if err := repository.ReplaceOwner(ctx, owner, result); err != nil {
		t.Fatal(err)
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
}

func indexWithDefaults(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{}); err != nil {
		_ = repository.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
}

func indexPath(t *testing.T, ctx context.Context, root string) string {
	t.Helper()
	project, err := indexer.DiscoverProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	return project.IndexPath
}
