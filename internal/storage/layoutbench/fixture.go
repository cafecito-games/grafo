package layoutbench

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

// DefaultFixtureScale is the scale of the default spike profile: roughly
// 50,000 nodes and 60,000 facts. The deep-ID population, the hub adjacency,
// and the fan populations all derive from it, so a smaller scale reproduces
// every fixture shape at proportionally lower row counts.
const DefaultFixtureScale = 5_000

const (
	// minimumFileCount keeps the repeated long owner path requirement intact
	// at the smallest scales, where the population alone would fit one file.
	minimumFileCount   = 32
	ambiguousPlainName = "ResolveAmbiguous"
	externalPoolSize   = 64
)

// Fixture is a deterministic synthetic repository image. Every file renders
// as a FileRecord plus its ParseResult so the fixture flows through the real
// ReplaceFile and Reconcile path of any storage adapter, and so candidate
// layouts and the production schema see byte-for-byte comparable input.
type Fixture struct {
	Seed  int           `json:"seed"`
	Scale int           `json:"scale"`
	Files []FixtureFile `json:"files"`
}

// FixtureFile pairs one file record with the parse result owned by that file.
type FixtureFile struct {
	Record graph.FileRecord  `json:"record"`
	Parsed graph.ParseResult `json:"parsed"`
}

// splitmix64 is a small deterministic generator. It drives only property
// payload variation; every identifier and location is derived from structural
// indices, so two seeds describe the same logical graph row for row and the
// seed influences bytes, never identity.
type splitmix64 struct {
	state uint64
}

func newSplitmix64(seed int) *splitmix64 {
	return &splitmix64{state: uint64(seed)}
}

func (generator *splitmix64) next() uint64 {
	generator.state += 0x9e3779b97f4a7c15
	mixed := generator.state
	mixed = (mixed ^ (mixed >> 30)) * 0xbf58476d1ce4e5b9
	mixed = (mixed ^ (mixed >> 27)) * 0x94d049bb133111eb
	return mixed ^ (mixed >> 31)
}

// GenerateFixture builds the layout spike corpus deterministically from seed
// and scale. Scale bounds the row counts: the population is 8*scale deep-ID
// nodes, the hub gains scale incoming and scale outgoing calls facts, and the
// default profile of DefaultFixtureScale lands near 50,000 nodes and 60,000
// facts. Scale values below 1 clamp to 1. Seed plays the sample-number role
// from the storage comparison runner: an odd seed emits files, nodes, and
// facts in reverse construction order, so adapters are measured against both
// insertion orders of one logical graph whose identifiers are identical.
func GenerateFixture(seed, scale int) Fixture {
	if scale < 1 {
		scale = 1
	}
	generator := newSplitmix64(seed)
	populationCount := 8 * scale
	hubFan := scale
	populationFileCount := max(minimumFileCount, scale/10)
	hubID := longNodeID("hub", 0)

	fixture := Fixture{Seed: seed, Scale: scale, Files: make([]FixtureFile, 0, populationFileCount+5)}
	for fileOrdinal := range populationFileCount {
		first := fileOrdinal * populationCount / populationFileCount
		last := (fileOrdinal + 1) * populationCount / populationFileCount
		if first == last {
			// At scales below 4 the population cannot fill every file; a file
			// that would own no nodes is not emitted, so no record is empty.
			continue
		}
		owner := populationOwnerPath(fileOrdinal)
		nodes := make([]graph.Node, 0, last-first)
		var facts []graph.Fact
		for index := first; index < last; index++ {
			node := fixtureNode(longNodeID("population", index), graph.KindFunction,
				fmt.Sprintf("PopulationFunction%08d", index), fmt.Sprintf("pkg.PopulationFunction%08d", index),
				owner, index, generator)
			nodes = append(nodes, node)
			// The chain target is the next node of the same file, wrapping so
			// every population node emits exactly one resolvable calls fact.
			nextIndex := index + 1
			if nextIndex == last {
				nextIndex = first
			}
			facts = append(facts, fixtureFact(owner, node.ID, graph.EdgeCalls,
				longNodeID("population", nextIndex), "", index, generator))
			if index%4 == 0 {
				// The external name is structural, not generator-derived: the
				// insertion-order variants must materialize the same external
				// node set, and the seed may only vary payload bytes. Every
				// fourth node references one pool entry, so index/4 walks the
				// whole pool instead of a fixed residue of it.
				facts = append(facts, fixtureFact(owner, node.ID, graph.EdgeReferences, "",
					externalSymbolName(index/4%externalPoolSize), index, generator))
			}
		}
		fixture.Files = append(fixture.Files, fixtureFile(owner, fileOrdinal, nodes, facts, seed, scale))
	}

	// The five structural files follow the population files in both append
	// order and path order, so an even seed always emits ascending paths.
	fixture.Files = append(fixture.Files,
		fanFile(populationFileCount, hubFan, "HubCaller", "callers", hubID, true, seed, scale, generator),
		fanFile(populationFileCount+1, hubFan, "HubCallee", "callees", hubID, false, seed, scale, generator),
		sharedFile(populationFileCount+2, seed, scale, generator),
		hubFile(populationFileCount+3, hubID, seed, scale, generator),
		testFile(populationFileCount+4, seed, scale, generator),
	)

	if seed%2 == 1 {
		slices.Reverse(fixture.Files)
		for index := range fixture.Files {
			slices.Reverse(fixture.Files[index].Parsed.Nodes)
			slices.Reverse(fixture.Files[index].Parsed.Facts)
		}
	}
	return fixture
}

// longNodeID renders a textual stable ID far past the compact-hash length of
// graph.NodeID, because one axis of the layout spike is how a schema carries
// long path-shaped identifiers. label names the population segment the ID
// belongs to; ordinal keeps IDs unique within one label.
func longNodeID(label string, ordinal int) string {
	return "n:pkg/very/deep/path/" + label + strings.Repeat("/very/deep/component", 9) +
		fmt.Sprintf("/leaf%08d:symbol%08d", ordinal, ordinal)
}

// sharedDirectoryPrefix is the directory prefix every owner path shares, so
// the owner_file column stores a heavily repeated long string across at least
// minimumFileCount files.
var sharedDirectoryPrefix = "src/very/deep/path" + strings.Repeat("/very/deep/directory", 6)

func populationOwnerPath(fileOrdinal int) string {
	return sharedDirectoryPrefix + fmt.Sprintf("/module%04d/generated.go", fileOrdinal)
}

func callersOwnerPath(moduleOrdinal int) string {
	return sharedDirectoryPrefix + fmt.Sprintf("/module%04d/callers.go", moduleOrdinal)
}

func calleesOwnerPath(moduleOrdinal int) string {
	return sharedDirectoryPrefix + fmt.Sprintf("/module%04d/callees.go", moduleOrdinal)
}

func sharedOwnerPath(moduleOrdinal int) string {
	return sharedDirectoryPrefix + fmt.Sprintf("/module%04d/shared.go", moduleOrdinal)
}

func hubOwnerPath(moduleOrdinal int) string {
	return sharedDirectoryPrefix + fmt.Sprintf("/module%04d/hub.go", moduleOrdinal)
}

func testOwnerPath(moduleOrdinal int) string {
	return sharedDirectoryPrefix + fmt.Sprintf("/module%04d/hub_test.go", moduleOrdinal)
}

// externalSymbolName is a textual fact target that exists nowhere in the
// fixture, so reconciliation resolves it to a materialized external node. The
// small pool means many facts share few external targets, mirroring real
// cross-repository references.
func externalSymbolName(index int) string {
	return fmt.Sprintf("vendor.example.org/external%03d/repository/pkg%02d.Dispatch%03d",
		index%8, index%externalPoolSize, index)
}

// fixtureFact and fixtureNode derive locations from structural ordinals so
// identifiers stay stable across seeds; only the variation property differs.
func fixtureNode(id string, kind graph.NodeKind, name, qualifiedName, owner string, locationOrdinal int, generator *splitmix64) graph.Node {
	return graph.Node{ID: id, Kind: kind, Name: name, QualifiedName: qualifiedName, Language: "go",
		Location: graph.Location{Path: owner, Line: locationOrdinal%997 + 1,
			Column: locationOrdinal%79 + 1, EndLine: locationOrdinal%997 + 1 + locationOrdinal%9},
		Properties: map[string]string{"fixture": "layoutbench",
			"variation": fmt.Sprintf("%016x", generator.next())},
		OwnerFile: owner}
}

// fixtureFact builds one fact with an exact source locator; targetID and
// target are mutually exclusive so exact and textual resolution are both
// exercised. ordinal drives both the location and the FactID disambiguator,
// and the fact kind and target keep FactIDs unique when two facts of one
// source share an ordinal.
func fixtureFact(owner, fromID string, kind graph.EdgeKind, targetID, target string, ordinal int, generator *splitmix64) graph.Fact {
	line := ordinal%997 + 1
	return graph.Fact{ID: graph.FactID(owner, fromID, kind, targetID+target, line, ordinal),
		FromID: fromID, Kind: kind, Producer: "layoutbench", TargetID: targetID, Target: target,
		Location: graph.Location{Path: owner, Line: line, Column: ordinal%79 + 1,
			EndLine: line + ordinal%9},
		Properties: map[string]string{"fixture": "layoutbench",
			"variation": fmt.Sprintf("%016x", generator.next())},
		OwnerFile: owner}
}

func fixtureFile(owner string, fileOrdinal int, nodes []graph.Node, facts []graph.Fact, seed, scale int) FixtureFile {
	rows := len(nodes) + len(facts)
	return FixtureFile{Record: graph.FileRecord{Path: owner,
		Hash:     graph.StableID("fixture", owner, strconv.Itoa(seed), strconv.Itoa(scale)),
		Language: "go", Size: int64(rows), ModifiedNS: int64(fileOrdinal) + 1,
		IndexedAt: "2026-01-01T00:00:00Z"},
		Parsed: graph.ParseResult{Nodes: nodes, Facts: facts}}
}

// fanFile renders one side of the hub adjacency: either every node calls the
// hub (incoming) or the hub calls every node (outgoing). The exact target IDs
// resolve without name lookup, giving the hub exactly hubFan edges per side.
func fanFile(moduleOrdinal, hubFan int, namePrefix, label, hubID string, incoming bool, seed, scale int, generator *splitmix64) FixtureFile {
	owner := callersOwnerPath(moduleOrdinal)
	if !incoming {
		owner = calleesOwnerPath(moduleOrdinal)
	}
	nodes := make([]graph.Node, 0, hubFan)
	facts := make([]graph.Fact, 0, hubFan)
	for index := range hubFan {
		node := fixtureNode(longNodeID(label, index), graph.KindFunction,
			fmt.Sprintf("%s%08d", namePrefix, index), fmt.Sprintf("pkg.%s%08d", namePrefix, index),
			owner, index, generator)
		nodes = append(nodes, node)
		fromID, targetID := hubID, node.ID
		if incoming {
			fromID, targetID = node.ID, hubID
		}
		facts = append(facts, fixtureFact(owner, fromID, graph.EdgeCalls, targetID, "", index, generator))
	}
	return fixtureFile(owner, moduleOrdinal, nodes, facts, seed, scale)
}

// sharedFile carries the ambiguity stress: one plain name declared both here
// and in the hub file, plus a probe whose textual facts cannot resolve that
// name uniquely and therefore materialize an external node at reconcile time.
// It also declares the production target that the test source calls.
func sharedFile(moduleOrdinal, seed, scale int, generator *splitmix64) FixtureFile {
	owner := sharedOwnerPath(moduleOrdinal)
	ambiguous := fixtureNode(longNodeID("ambiguity", 1), graph.KindFunction, ambiguousPlainName,
		"pkgShared."+ambiguousPlainName, owner, 0, generator)
	probe := fixtureNode(longNodeID("ambiguity", 2), graph.KindFunction, "AmbiguityProbe",
		"pkgShared.AmbiguityProbe", owner, 1, generator)
	productionTarget := fixtureNode(longNodeID("production", 0), graph.KindFunction, "ProductionTarget",
		"pkgShared.ProductionTarget", owner, 2, generator)
	nodes := []graph.Node{ambiguous, probe, productionTarget}
	facts := []graph.Fact{
		fixtureFact(owner, probe.ID, graph.EdgeCalls, "", ambiguousPlainName, 0, generator),
		fixtureFact(owner, probe.ID, graph.EdgeCalls, "", ambiguousPlainName, 1, generator),
	}
	return fixtureFile(owner, moduleOrdinal, nodes, facts, seed, scale)
}

// hubFile owns the hub node itself. A second declaration of the ambiguous
// plain name lives here, in a different file than the shared declaration, so
// the name is genuinely ambiguous rather than merely duplicated evidence.
func hubFile(moduleOrdinal int, hubID string, seed, scale int, generator *splitmix64) FixtureFile {
	owner := hubOwnerPath(moduleOrdinal)
	hub := fixtureNode(hubID, graph.KindFunction, "HubDispatch", "pkg.HubDispatch", owner, 0, generator)
	ambiguous := fixtureNode(longNodeID("ambiguity", 0), graph.KindFunction, ambiguousPlainName,
		"pkgHub."+ambiguousPlainName, owner, 1, generator)
	return fixtureFile(owner, moduleOrdinal, []graph.Node{hub, ambiguous}, nil, seed, scale)
}

// testFile declares a KindTest source with one exact calls fact to a
// production symbol, which is the shape reconciliation needs to derive a
// persisted direct tests edge through graph.DirectTestEdge.
func testFile(moduleOrdinal, seed, scale int, generator *splitmix64) FixtureFile {
	owner := testOwnerPath(moduleOrdinal)
	source := fixtureNode(longNodeID("test", 0), graph.KindTest, "TestHubDispatch",
		"pkg.TestHubDispatch", owner, 0, generator)
	nodes := []graph.Node{source}
	facts := []graph.Fact{fixtureFact(owner, source.ID, graph.EdgeCalls,
		longNodeID("production", 0), "", 0, generator)}
	return fixtureFile(owner, moduleOrdinal, nodes, facts, seed, scale)
}
