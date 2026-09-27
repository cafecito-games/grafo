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
)

var _ graph.CatalogRepository = (*federation.Repository)(nil)

func TestCatalogSharesOneContractAcrossRepositories(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
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
