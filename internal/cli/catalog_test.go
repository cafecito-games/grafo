package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/cli"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestCatalogCommandsReportUsageAndOrphans(t *testing.T) {
	root := catalogFixture(t)
	run(t, "index", root)

	var resources query.DataResourceList
	runJSON(t, &resources, "data-resources", "--repo", root, "--json")
	byName := map[string]query.Resource{}
	for _, resource := range resources.Resources {
		byName[resource.QualifiedName] = resource
	}
	orders, ok := byName["orders"]
	if !ok {
		t.Fatalf("orders table missing from the catalog: %#v", resources)
	}
	if orders.ObjectKind != "table" || orders.Dialect != "sqlite" {
		t.Fatalf("normalized resource metadata missing: %#v", orders)
	}
	if _, ok := byName["order_summary"]; !ok {
		t.Fatalf("order_summary view missing from the catalog: %#v", resources)
	}

	var usage query.DataResourceUsage
	runJSON(t, &usage, "data-usage", "orders", "--repo", root, "--json")
	if len(usage.Writers) == 0 {
		t.Fatalf("the insert statement was not reported as a writer: %#v", usage)
	}
	if len(usage.Readers) == 0 {
		t.Fatalf("the view was not reported as a reader: %#v", usage)
	}
	for _, writer := range usage.Writers {
		if writer.Location.Path == "" {
			t.Fatalf("writer evidence lost its source site: %#v", writer)
		}
	}

	var keys query.ConfigKeyList
	runJSON(t, &keys, "config-keys", "--repo", root, "--name", "DATABASE_URL", "--json")
	if len(keys.Keys) != 1 {
		t.Fatalf("expected one config key, got %#v", keys.Keys)
	}
	key := keys.Keys[0]
	if !key.Defined || key.Format != "env" {
		t.Fatalf("normalized config metadata missing: %#v", key)
	}
	if len(key.Definitions) == 0 {
		t.Fatalf("config key lost its definition site: %#v", key)
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "s3cr3t") {
		t.Fatalf("config catalog leaked a stored value: %s", encoded)
	}

	var events query.EventList
	runJSON(t, &events, "events", "--repo", root, "--json")
	var placed *query.Event
	for index, event := range events.Events {
		if event.Name == "order_placed" {
			placed = &events.Events[index]
		}
	}
	if placed == nil {
		t.Fatalf("declared signal missing from the event catalog: %#v", events)
	}
	if len(placed.Producers) == 0 || len(placed.Consumers) == 0 {
		t.Fatalf("signal producers and consumers were not partitioned: %#v", placed)
	}

	var orphans query.OrphanedEventList
	runJSON(t, &orphans, "orphaned-events", "--repo", root, "--json")
	categories := map[string]query.OrphanCategory{}
	statuses := map[string]query.OrphanStatus{}
	for _, orphan := range orphans.Events {
		categories[orphan.Event.Name] = orphan.Category
		statuses[orphan.Event.Name] = orphan.Status
	}
	if _, present := categories["order_placed"]; present {
		t.Fatalf("a produced and consumed signal is not an orphan: %#v", orphans.Events)
	}
	if categories["cart_cleared"] != query.PublishedWithoutConsumer || statuses["cart_cleared"] != query.OrphanConfirmed {
		t.Fatalf("published-without-consumer was not classified: %#v", orphans.Events)
	}
	if categories["stock_low"] != query.ConsumedWithoutProducer || statuses["stock_low"] != query.OrphanConfirmed {
		t.Fatalf("consumed-without-producer was not classified: %#v", orphans.Events)
	}
	if categories["legacy_pruned"] != query.DeclaredWithNeither || statuses["legacy_pruned"] != query.OrphanConfirmed {
		t.Fatalf("declared-with-neither was not classified: %#v", orphans.Events)
	}
}

func TestCatalogResultsSurviveIncrementalIndexing(t *testing.T) {
	root := catalogFixture(t)
	run(t, "index", root)
	write(t, filepath.Join(root, "schema.sql"), `CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT);
CREATE TABLE shipments (id INTEGER PRIMARY KEY, order_id INTEGER REFERENCES orders(id));
CREATE VIEW order_summary AS SELECT customer FROM orders;
INSERT INTO orders (customer) VALUES ('ada');
`)
	run(t, "index", root)

	commands := [][]string{
		{"data-resources"}, {"config-keys"}, {"events"}, {"orphaned-events"},
		{"data-usage", "orders"},
	}
	incremental := make([]string, 0, len(commands))
	for _, command := range commands {
		incremental = append(incremental, run(t, append(append([]string{}, command...), "--repo", root, "--json")...))
	}

	if err := os.RemoveAll(filepath.Join(root, ".grafo")); err != nil {
		t.Fatal(err)
	}
	run(t, "index", root)
	for index, command := range commands {
		rebuilt := run(t, append(append([]string{}, command...), "--repo", root, "--json")...)
		if rebuilt != incremental[index] {
			t.Fatalf("grafo %s differs between incremental and clean indexing:\n%s\n%s",
				strings.Join(command, " "), incremental[index], rebuilt)
		}
	}
}

func TestCatalogCommandsRejectUnsupportedFilters(t *testing.T) {
	root := catalogFixture(t)
	run(t, "index", root)
	if code, _, stderr := execute(t, "data-resources", "--repo", root, "--kind", "function"); code == 0 {
		t.Fatalf("an unsupported kind must fail: %s", stderr)
	}
	if code, _, stderr := execute(t, "data-resources", "--repo", root, "--repository", "missing"); code == 0 {
		t.Fatalf("an unknown repository filter must fail: %s", stderr)
	}
	if code, _, stderr := execute(t, "data-usage", "missing_table", "--repo", root); code == 0 {
		t.Fatalf("an unknown resource must fail: %s", stderr)
	}
	if code, _, stderr := execute(t, "data-resources", "--repo", root, "--kind", ","); code == 0 {
		t.Fatalf("a kind list naming no kind must fail: %s", stderr)
	}
	if code, stdout, _ := execute(t, "data-resources", "--repo", root, "--name", "%", "--json"); code != 0 {
		t.Fatalf("a literal name fragment must be accepted")
	} else if !strings.Contains(stdout, `"resources": []`) {
		t.Fatalf("a name fragment acted as a wildcard: %s", stdout)
	}
}

func TestCatalogRefusesAMixedFreshnessFederation(t *testing.T) {
	indexed := catalogFixture(t)
	run(t, "index", indexed)
	unindexed := catalogFixture(t)
	code, stdout, stderr := execute(t, "data-resources", "--repos", indexed+","+unindexed, "--json")
	if code == 0 {
		t.Fatalf("a federation that cannot be refreshed must not answer: %s", stdout)
	}
	if stdout != "" {
		t.Fatalf("a partial catalog was written before the failure: %s", stdout)
	}
	if !strings.Contains(stderr, "has no index") {
		t.Fatalf("unexpected federation failure: %s", stderr)
	}
}

func catalogFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "grafo.yaml"), "sql:\n  default_dialect: sqlite\n")
	write(t, filepath.Join(root, "schema.sql"), `CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT);
CREATE VIEW order_summary AS SELECT customer FROM orders;
INSERT INTO orders (customer) VALUES ('ada');
`)
	write(t, filepath.Join(root, ".env"), "DATABASE_URL=postgres://ada:s3cr3t@localhost/shop\n")
	write(t, filepath.Join(root, "cart.gd"), `class_name Cart

signal order_placed(total: int)
signal cart_cleared
signal stock_low
signal legacy_pruned

func checkout(total: int) -> void:
	order_placed.emit(total)
	order_placed.connect(on_order_placed)
	cart_cleared.emit()
	stock_low.connect(on_stock_low)

func on_order_placed(total: int) -> void:
	pass

func on_stock_low() -> void:
	pass
`)
	return root
}

func execute(t *testing.T, arguments ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.New(&stdout, &stderr).Run(context.Background(), arguments)
	return code, stdout.String(), stderr.String()
}

func run(t *testing.T, arguments ...string) string {
	t.Helper()
	code, stdout, stderr := execute(t, arguments...)
	if code != 0 {
		t.Fatalf("grafo %s failed: %s", strings.Join(arguments, " "), stderr)
	}
	return stdout
}

func runJSON(t *testing.T, target any, arguments ...string) {
	t.Helper()
	stdout := run(t, arguments...)
	if err := json.Unmarshal([]byte(stdout), target); err != nil {
		t.Fatalf("grafo %s produced invalid JSON: %v\n%s", strings.Join(arguments, " "), err, stdout)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
