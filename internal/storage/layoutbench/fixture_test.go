package layoutbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

// fixtureTotals counts nodes, facts, and distinct owner files across a whole
// fixture so structure assertions stay independent of the per-file layout.
type fixtureTotals struct {
	nodes       int
	facts       int
	ownerFiles  map[string]int
	minIDLength int
}

func measureFixture(fixture Fixture) fixtureTotals {
	totals := fixtureTotals{ownerFiles: map[string]int{}, minIDLength: -1}
	for _, file := range fixture.Files {
		totals.ownerFiles[file.Record.Path] += len(file.Parsed.Nodes) + len(file.Parsed.Facts)
		totals.nodes += len(file.Parsed.Nodes)
		totals.facts += len(file.Parsed.Facts)
		for _, node := range file.Parsed.Nodes {
			if totals.minIDLength < 0 || len(node.ID) < totals.minIDLength {
				totals.minIDLength = len(node.ID)
			}
		}
	}
	return totals
}

func fixtureDigest(t *testing.T, fixture Fixture) string {
	t.Helper()
	content, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// exactDegree counts, per node ID, how many facts reference it as an exact
// source (FromID) and as an exact target (TargetID).
type exactDegree struct {
	fromByID map[string]int
	toByID   map[string]int
}

func measureExactDegree(fixture Fixture) exactDegree {
	degree := exactDegree{fromByID: map[string]int{}, toByID: map[string]int{}}
	for _, file := range fixture.Files {
		for _, fact := range file.Parsed.Facts {
			degree.fromByID[fact.FromID]++
			if fact.TargetID != "" {
				degree.toByID[fact.TargetID]++
			}
		}
	}
	return degree
}

func maxMapValue(values map[string]int) int {
	highest := 0
	for _, value := range values {
		if value > highest {
			highest = value
		}
	}
	return highest
}

// sharedPathPrefix returns the longest prefix every fixture path shares, so
// the repeated long owner path requirement is measured instead of assumed.
func sharedPathPrefix(fixture Fixture) string {
	if len(fixture.Files) == 0 {
		return ""
	}
	first := fixture.Files[0].Record.Path
	last := fixture.Files[len(fixture.Files)-1].Record.Path
	if first == last {
		return first
	}
	limit := min(len(first), len(last))
	for index := range limit {
		if first[index] != last[index] {
			return first[:index]
		}
	}
	return first[:limit]
}

func TestFixtureDeterminismByJSONDigest(t *testing.T) {
	first := fixtureDigest(t, GenerateFixture(7, 250))
	second := fixtureDigest(t, GenerateFixture(7, 250))
	if first != second {
		t.Fatalf("same seed produced different digests: %s vs %s", first, second)
	}
	otherSeed := fixtureDigest(t, GenerateFixture(8, 250))
	if first == otherSeed {
		t.Fatalf("seed 7 and seed 8 produced identical digests: %s", first)
	}
	otherScale := fixtureDigest(t, GenerateFixture(7, 251))
	if first == otherScale {
		t.Fatalf("scale 250 and scale 251 produced identical digests: %s", first)
	}
}

func TestFixtureOddSeedReversesInsertionOrder(t *testing.T) {
	even := GenerateFixture(4, 40)
	paths := make([]string, 0, len(even.Files))
	for _, file := range even.Files {
		paths = append(paths, file.Record.Path)
	}
	if !slices.IsSorted(paths) {
		t.Fatalf("even seed must emit files in ascending path order: %v", paths[:min(5, len(paths))])
	}
	odd := GenerateFixture(5, 40)
	reversedPaths := make([]string, 0, len(odd.Files))
	for _, file := range odd.Files {
		reversedPaths = append(reversedPaths, file.Record.Path)
	}
	sortedPaths := append([]string(nil), reversedPaths...)
	slices.Reverse(sortedPaths)
	if !reflect.DeepEqual(sortedPaths, paths) {
		t.Fatalf("odd seed files are not the even seed files reversed: %v vs %v", reversedPaths[:min(5, len(reversedPaths))], paths[:min(5, len(paths))])
	}
	firstEven := even.Files[0].Parsed.Nodes
	firstOdd := odd.Files[len(odd.Files)-1].Parsed.Nodes
	if len(firstEven) != len(firstOdd) || firstEven[0].ID != firstOdd[len(firstOdd)-1].ID {
		t.Fatalf("odd seed must reverse nodes within each file: %s vs %s", firstEven[0].ID, firstOdd[len(firstOdd)-1].ID)
	}
}

func TestFixtureDefaultScaleStructure(t *testing.T) {
	fixture := GenerateFixture(7, DefaultFixtureScale)
	totals := measureFixture(fixture)

	if totals.nodes < 49_000 || totals.nodes > 51_000 {
		t.Errorf("default scale node count = %d, want approximately 50000", totals.nodes)
	}
	if totals.facts < 59_000 || totals.facts > 61_000 {
		t.Errorf("default scale fact count = %d, want approximately 60000", totals.facts)
	}
	if totals.minIDLength < 200 {
		t.Errorf("shortest node ID = %d characters, want at least 200", totals.minIDLength)
	}
	if len(fixture.Files) < 30 || len(totals.ownerFiles) < 30 {
		t.Errorf("fixture files = %d distinct owners = %d, want at least 30", len(fixture.Files), len(totals.ownerFiles))
	}
	prefix := sharedPathPrefix(fixture)
	if len(prefix) < 100 {
		t.Errorf("shared owner path prefix = %d characters (%q), want at least 100", len(prefix), prefix)
	}

	degree := measureExactDegree(fixture)
	if incoming := maxMapValue(degree.toByID); incoming < 5_000 {
		t.Errorf("highest exact incoming fact degree = %d, want at least 5000", incoming)
	}
	if outgoing := maxMapValue(degree.fromByID); outgoing < 5_000 {
		t.Errorf("highest exact outgoing fact degree = %d, want at least 5000", outgoing)
	}
}

func TestFixtureContentInvariants(t *testing.T) {
	fixture := GenerateFixture(3, 60)

	ambiguousOwners := map[string]bool{}
	textualTargetFacts := 0
	nodeIDs := map[string]bool{}
	factIDs := map[string]bool{}
	testCallsProduction := false
	for _, file := range fixture.Files {
		if file.Record.Hash == "" || file.Record.Language == "" || file.Record.Size <= 0 ||
			file.Record.ModifiedNS <= 0 || file.Record.IndexedAt == "" {
			t.Fatalf("file record incomplete: %+v", file.Record)
		}
		for _, node := range file.Parsed.Nodes {
			if node.OwnerFile != file.Record.Path {
				t.Fatalf("node %s owner %q does not match owning file %q", node.ID, node.OwnerFile, file.Record.Path)
			}
			if node.External {
				t.Fatalf("fixture node %s must not be external", node.ID)
			}
			if !strings.HasPrefix(node.ID, "n:pkg/very/deep/path") {
				t.Fatalf("node ID %q is not a long stable path ID", node.ID)
			}
			if nodeIDs[node.ID] {
				t.Fatalf("duplicate node ID %s", node.ID)
			}
			nodeIDs[node.ID] = true
			if node.Name == ambiguousPlainName {
				ambiguousOwners[node.OwnerFile] = true
			}
		}
		for _, fact := range file.Parsed.Facts {
			if fact.OwnerFile != file.Record.Path {
				t.Fatalf("fact %s owner %q does not match owning file %q", fact.ID, fact.OwnerFile, file.Record.Path)
			}
			if err := fact.ValidateSourceLocator(); err != nil {
				t.Fatalf("fact %s violates the source locator contract: %v", fact.ID, err)
			}
			if factIDs[fact.ID] {
				t.Fatalf("duplicate fact ID %s", fact.ID)
			}
			factIDs[fact.ID] = true
			if fact.Target != "" && fact.TargetID == "" {
				textualTargetFacts++
			}
		}
	}
	if len(ambiguousOwners) < 2 {
		t.Errorf("ambiguous plain name %q declared in %d files, want at least 2", ambiguousPlainName, len(ambiguousOwners))
	}
	if textualTargetFacts == 0 {
		t.Error("fixture contains no facts with textual targets")
	}

	var testNode graph.Node
	var productionIDs []string
	for _, file := range fixture.Files {
		for _, node := range file.Parsed.Nodes {
			if node.Kind == graph.KindTest {
				testNode = node
			}
			if node.Kind != graph.KindTest && !graph.IsTestSupportNode(node) {
				productionIDs = append(productionIDs, node.ID)
			}
		}
	}
	if testNode.ID == "" {
		t.Fatal("fixture contains no test node")
	}
	for _, file := range fixture.Files {
		for _, fact := range file.Parsed.Facts {
			if fact.FromID == testNode.ID && fact.Kind == graph.EdgeCalls &&
				slices.Contains(productionIDs, fact.TargetID) {
				testCallsProduction = true
			}
		}
	}
	if !testCallsProduction {
		t.Errorf("test node %s has no calls fact to a production node", testNode.ID)
	}
}

func TestFixtureScaleBoundsRows(t *testing.T) {
	small := GenerateFixture(2, 40)
	totals := measureFixture(small)
	upperBound := 10*40 + 16
	if totals.nodes > upperBound {
		t.Errorf("scale 40 produced %d nodes, want at most %d", totals.nodes, upperBound)
	}
	degree := measureExactDegree(small)
	if incoming := maxMapValue(degree.toByID); incoming != 40 {
		t.Errorf("highest exact incoming degree = %d, want the hub fan of exactly 40", incoming)
	}
	if outgoing := maxMapValue(degree.fromByID); outgoing != 40 {
		t.Errorf("highest exact outgoing degree = %d, want the hub fan of exactly 40", outgoing)
	}
	if len(small.Files) < 30 {
		t.Errorf("small fixture has %d files, want at least 30", len(small.Files))
	}
	clamped := GenerateFixture(2, 0)
	if clampedScale := measureFixture(clamped); clampedScale.nodes == 0 {
		t.Error("scale 0 must clamp to a non-empty fixture")
	}
}

func TestFixtureReconcileMatchesAcrossInsertionOrder(t *testing.T) {
	ctx := context.Background()
	evenCounts := indexFixtureThroughProductionAdapter(t, ctx, GenerateFixture(4, 40))
	oddCounts := indexFixtureThroughProductionAdapter(t, ctx, GenerateFixture(5, 40))

	if !reflect.DeepEqual(evenCounts, oddCounts) {
		t.Fatalf("insertion order changed reconciled counts:\neven: %+v\nodd: %+v", evenCounts, oddCounts)
	}
	if evenCounts.ByEdge[string(graph.EdgeTests)] == 0 {
		t.Errorf("reconciled graph derived no %s edges: %+v", graph.EdgeTests, evenCounts.ByEdge)
	}
	if evenCounts.External == 0 {
		t.Error("reconciled graph materialized no external nodes")
	}
	if evenCounts.Facts == 0 || evenCounts.Edges == 0 || evenCounts.Nodes == 0 {
		t.Errorf("reconciled graph is empty: %+v", evenCounts)
	}
}

// indexFixtureThroughProductionAdapter pushes a fixture through the real
// ReplaceFile and Reconcile path of the production SQLite adapter, mirroring
// the sequence the storage benchmark uses, and returns the reconciled counts.
func indexFixtureThroughProductionAdapter(t *testing.T, ctx context.Context, fixture Fixture) graph.Counts {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layout.sqlite")
	repository, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("open production adapter: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = repository.Close()
		}
	}()
	for _, file := range fixture.Files {
		if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
			t.Fatalf("replace file %s: %v", file.Record.Path, err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pending, err := repository.ReconciliationPending(ctx)
	if err != nil {
		t.Fatalf("check reconciliation pending: %v", err)
	}
	if pending {
		t.Fatal("reconciliation left resolver work pending")
	}
	counts, err := repository.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	closed = true
	return counts
}

func TestFixtureRecordsItsInputs(t *testing.T) {
	fixture := GenerateFixture(1, 8)
	if fixture.Seed != 1 || fixture.Scale != 8 {
		t.Fatalf("fixture must record its inputs: seed=%d scale=%d", fixture.Seed, fixture.Scale)
	}
	if len(fixture.Files) == 0 {
		t.Fatal("fixture has no files")
	}
}
