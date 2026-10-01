// Package detailprofile contains benchmark- and design-spike-only fidelity
// models. Production CLI, MCP, indexer configuration, and storage composition
// deliberately do not import this package.
package detailprofile

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

const MatrixSchema = "grafo.detail-profile-matrix/v1"

type Capability string

const (
	CapabilityStructural       Capability = "structural_traversal"
	CapabilityImpactDataflow   Capability = "impact_dataflow"
	CapabilityFailureFlow      Capability = "failure_flow"
	CapabilityMessageFieldFlow Capability = "message_field_flow"
	CapabilityTransportFlow    Capability = "transport_flow"
	CapabilityTestCoverage     Capability = "test_coverage"
	CapabilityGodotComposition Capability = "godot_composition"
	CapabilityGodotInteraction Capability = "godot_interactions"
	CapabilityCatalogTopology  Capability = "catalogs_topology"
	CapabilitySemanticReuse    Capability = "semantic_reuse"
	CapabilitySourceLookup     Capability = "source_lookup"
)

var capabilities = []Capability{
	CapabilityStructural, CapabilityImpactDataflow, CapabilityFailureFlow,
	CapabilityMessageFieldFlow, CapabilityTransportFlow, CapabilityTestCoverage,
	CapabilityGodotComposition, CapabilityGodotInteraction,
	CapabilityCatalogTopology, CapabilitySemanticReuse, CapabilitySourceLookup,
}

type EvidenceRole string

const (
	RoleRequired EvidenceRole = "required"
	RoleEnriches EvidenceRole = "enriches"
)

type Anchor struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

type Evidence struct {
	Name         string       `json:"name"`
	Role         EvidenceRole `json:"role"`
	Capabilities []Capability `json:"capabilities"`
	Producers    []string     `json:"producers"`
	Consumers    []string     `json:"consumers"`
	Anchors      []Anchor     `json:"anchors"`
	Notes        string       `json:"notes,omitempty"`
}

type Operation struct {
	Name         string       `json:"name"`
	Capabilities []Capability `json:"capabilities"`
	Adapters     []string     `json:"adapters"`
	Anchor       Anchor       `json:"anchor"`
}

type EvidenceMatrix struct {
	Schema     string      `json:"schema"`
	Nodes      []Evidence  `json:"nodes"`
	Edges      []Evidence  `json:"edges"`
	Properties []Evidence  `json:"properties_and_forms"`
	Producers  []Evidence  `json:"producers"`
	Operations []Operation `json:"operations"`
}

func Matrix() EvidenceMatrix {
	return EvidenceMatrix{
		Schema: MatrixSchema,
		Nodes:  nodeEvidence(), Edges: edgeEvidence(), Properties: propertyEvidence(),
		Producers: producerEvidence(), Operations: operationEvidence(),
	}
}

func (m EvidenceMatrix) NodeNames() []string     { return evidenceNames(m.Nodes) }
func (m EvidenceMatrix) EdgeNames() []string     { return evidenceNames(m.Edges) }
func (m EvidenceMatrix) ProducerNames() []string { return evidenceNames(m.Producers) }

func evidenceNames(items []Evidence) []string {
	result := make([]string, len(items))
	for index, item := range items {
		result[index] = item.Name
	}
	return result
}

func (m EvidenceMatrix) Validate() error {
	if m.Schema != MatrixSchema {
		return fmt.Errorf("matrix schema %q, want %q", m.Schema, MatrixSchema)
	}
	known := map[Capability]bool{}
	anchorFiles := map[string][]string{}
	for _, capability := range capabilities {
		known[capability] = true
	}
	validate := func(category string, items []Evidence) error {
		seen := map[string]bool{}
		for _, item := range items {
			if strings.TrimSpace(item.Name) == "" || seen[item.Name] {
				return fmt.Errorf("%s has empty or duplicate evidence %q", category, item.Name)
			}
			seen[item.Name] = true
			if item.Role != RoleRequired && item.Role != RoleEnriches {
				return fmt.Errorf("%s %q has unknown role %q", category, item.Name, item.Role)
			}
			if len(item.Capabilities) == 0 || len(item.Producers) == 0 || len(item.Consumers) == 0 || len(item.Anchors) == 0 {
				return fmt.Errorf("%s %q lacks capability, producer, consumer, or anchor classification", category, item.Name)
			}
			for _, capability := range item.Capabilities {
				if !known[capability] {
					return fmt.Errorf("%s %q uses unknown capability %q", category, item.Name, capability)
				}
			}
			for _, anchor := range item.Anchors {
				if anchor.Path == "" || anchor.Line <= 0 {
					return fmt.Errorf("%s %q has invalid anchor %#v", category, item.Name, anchor)
				}
				if err := validateAnchor(anchor, anchorFiles); err != nil {
					return fmt.Errorf("%s %q: %w", category, item.Name, err)
				}
			}
		}
		return nil
	}
	for category, items := range map[string][]Evidence{
		"node": m.Nodes, "edge": m.Edges, "property": m.Properties, "producer": m.Producers,
	} {
		if err := validate(category, items); err != nil {
			return err
		}
	}
	if got, want := strings.Join(m.NodeNames(), "\x00"), joinNodeKinds(graph.NodeKinds()); got != want {
		return fmt.Errorf("node vocabulary classification drift")
	}
	if got, want := strings.Join(m.EdgeNames(), "\x00"), joinEdgeKinds(graph.EdgeKinds()); got != want {
		return fmt.Errorf("edge vocabulary classification drift")
	}
	seenOperations := map[string]bool{}
	for _, operation := range m.Operations {
		if operation.Name == "" || seenOperations[operation.Name] || len(operation.Capabilities) == 0 || len(operation.Adapters) == 0 || operation.Anchor.Path == "" || operation.Anchor.Line <= 0 {
			return fmt.Errorf("invalid or duplicate operation classification %q", operation.Name)
		}
		seenOperations[operation.Name] = true
		if err := validateOperationAnchor(operation.Anchor, anchorFiles); err != nil {
			return fmt.Errorf("operation %q: %w", operation.Name, err)
		}
		for _, capability := range operation.Capabilities {
			if !known[capability] {
				return fmt.Errorf("operation %q uses unknown capability %q", operation.Name, capability)
			}
		}
	}
	return nil
}

func validateAnchor(anchor Anchor, cache map[string][]string) error {
	cleaned := filepath.ToSlash(filepath.Clean(anchor.Path))
	if cleaned != anchor.Path || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(cleaned) {
		return fmt.Errorf("anchor path %q is not a canonical repository-relative path", anchor.Path)
	}
	lines, ok := cache[cleaned]
	if !ok {
		_, source, _, callerOK := runtime.Caller(0)
		if !callerOK {
			return fmt.Errorf("cannot resolve matrix source root")
		}
		root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(cleaned)))
		if err != nil {
			return fmt.Errorf("anchor %s:%d is unreadable: %w", anchor.Path, anchor.Line, err)
		}
		lines = strings.Split(string(content), "\n")
		cache[cleaned] = lines
	}
	if anchor.Line > len(lines) {
		return fmt.Errorf("anchor %s:%d exceeds %d source lines", anchor.Path, anchor.Line, len(lines))
	}
	if strings.TrimSpace(lines[anchor.Line-1]) == "" {
		return fmt.Errorf("anchor %s:%d points at a blank line", anchor.Path, anchor.Line)
	}
	return nil
}

func validateOperationAnchor(anchor Anchor, cache map[string][]string) error {
	if err := validateAnchor(anchor, cache); err != nil {
		return err
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("cannot resolve matrix source root")
	}
	path := filepath.Join(filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..")), filepath.FromSlash(anchor.Path))
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, path, nil, 0)
	if err != nil {
		return fmt.Errorf("operation anchor %s:%d is not parseable Go: %w", anchor.Path, anchor.Line, err)
	}
	for _, declaration := range parsed.Decls {
		function, functionOK := declaration.(*ast.FuncDecl)
		if functionOK && files.Position(function.Pos()).Line == anchor.Line {
			return nil
		}
	}
	return fmt.Errorf("operation anchor %s:%d does not identify a function declaration", anchor.Path, anchor.Line)
}

func joinNodeKinds(values []graph.NodeKind) string {
	names := make([]string, len(values))
	for index, value := range values {
		names[index] = string(value)
	}
	return strings.Join(names, "\x00")
}

func joinEdgeKinds(values []graph.EdgeKind) string {
	names := make([]string, len(values))
	for index, value := range values {
		names[index] = string(value)
	}
	return strings.Join(names, "\x00")
}

func nodeEvidence() []Evidence {
	all := []string{"config", "gdscript", "go", "godot", "indexer", "java", "manifest", "markdown", "protobuf", "protobufbinding", "python", "sql", "swift", "typescript"}
	items := make([]Evidence, 0, len(graph.NodeKinds()))
	for _, kind := range graph.NodeKinds() {
		caps := []Capability{CapabilityStructural, CapabilitySemanticReuse, CapabilitySourceLookup}
		role := RoleRequired
		notes := "declaration identity and stable selector evidence"
		switch kind {
		case graph.KindVariable, graph.KindParameter:
			caps = []Capability{CapabilityImpactDataflow, CapabilityFailureFlow, CapabilityMessageFieldFlow}
			notes = "local propagation identity; removable only with every dependent fact and capability"
		case graph.KindField:
			caps = append(caps, CapabilityMessageFieldFlow, CapabilityCatalogTopology)
		case graph.KindTest:
			caps = append(caps, CapabilityTestCoverage)
		case graph.KindTable, graph.KindView, graph.KindColumn, graph.KindIndex, graph.KindConfigKey, graph.KindEndpoint, graph.KindEvent, graph.KindComponent:
			caps = append(caps, CapabilityCatalogTopology)
		case graph.KindGodotScene, graph.KindGodotResource, graph.KindGodotSceneNode, graph.KindGodotAutoload:
			caps = append(caps, CapabilityGodotComposition, CapabilityGodotInteraction)
		case graph.KindGodotInputAction, graph.KindGodotNodeGroup:
			caps = append(caps, CapabilityGodotInteraction)
		case graph.KindTransportOperation:
			caps = append(caps, CapabilityTransportFlow)
		case graph.KindExternal:
			caps = append(caps, CapabilityCatalogTopology, CapabilityTransportFlow)
			role = RoleEnriches
			notes = "explicit unresolved boundary; omission must never become confident absence"
		}
		items = append(items, Evidence{Name: string(kind), Role: role, Capabilities: uniqueCaps(caps), Producers: all,
			Consumers: []string{"resolution", "source", "semantic", "query", "federation"},
			Anchors:   []Anchor{{Path: "internal/graph/model.go", Line: 26}, {Path: "internal/query/service.go", Line: 112}}, Notes: notes})
	}
	return items
}

func edgeEvidence() []Evidence {
	all := []string{"config", "gdscript", "go", "godot", "indexer", "java", "manifest", "markdown", "protobuf", "protobufbinding", "python", "sql", "swift", "typescript"}
	items := make([]Evidence, 0, len(graph.EdgeKinds()))
	for _, kind := range graph.EdgeKinds() {
		caps := []Capability{CapabilityStructural}
		consumer, line := "traversal", 214
		notes := "retained by the structural/topology candidate"
		switch kind {
		case graph.EdgeAssigns, graph.EdgeReturns, graph.EdgePasses:
			caps, consumer, line = []Capability{CapabilityImpactDataflow, CapabilityMessageFieldFlow, CapabilityFailureFlow}, "impact and propagation", 45
			notes = "omitted with local propagation chains"
		case graph.EdgeReads, graph.EdgeWrites:
			caps, consumer, line = []Capability{CapabilityImpactDataflow, CapabilityMessageFieldFlow, CapabilityCatalogTopology}, "data catalog and message field flow", 246
			notes = "target kind/properties distinguish retained data-resource facts from omitted local/protocol propagation"
		case graph.EdgeReturnsError, graph.EdgePropagatesError, graph.EdgeHandlesError, graph.EdgeWrapsError, graph.EdgePanics, graph.EdgeRecovers, graph.EdgeDefers:
			caps, consumer, line = []Capability{CapabilityFailureFlow}, "failure flow", 78
			notes = "omitted as one fail-closed failure-flow capability"
		case graph.EdgeEncodes, graph.EdgeDecodes:
			caps, consumer, line = []Capability{CapabilityMessageFieldFlow, CapabilityTransportFlow}, "message and codec flow", 151
		case graph.EdgeSends, graph.EdgeReceives, graph.EdgeCarries:
			caps, consumer, line = []Capability{CapabilityTransportFlow}, "transport flow", 210
		case graph.EdgeTests:
			caps, consumer, line = []Capability{CapabilityTestCoverage}, "test coverage", 55
		case graph.EdgeInstantiates, graph.EdgeAttachesScript, graph.EdgeAutoloads:
			caps, consumer, line = []Capability{CapabilityGodotComposition}, "Godot composition", 98
		case graph.EdgeUsesInputAction, graph.EdgeInGroup, graph.EdgeUsesGroup:
			caps, consumer, line = []Capability{CapabilityGodotInteraction}, "Godot interactions", 225
		case graph.EdgeReadsConfig, graph.EdgeDefines, graph.EdgeExposes, graph.EdgeHandledBy, graph.EdgeUsesMiddleware, graph.EdgePublishes, graph.EdgeSubscribes, graph.EdgeRequests:
			caps = append(caps, CapabilityCatalogTopology)
			consumer, line = "catalogs and topology", 501
		case graph.EdgeGeneratedFrom, graph.EdgeHasField:
			caps = append(caps, CapabilityMessageFieldFlow)
			consumer, line = "canonical message flow", 151
		case graph.EdgeDocuments:
			caps = append(caps, CapabilitySourceLookup)
		}
		items = append(items, Evidence{Name: string(kind), Role: RoleRequired, Capabilities: uniqueCaps(caps), Producers: all,
			Consumers: []string{consumer, "reconciliation", "federation"},
			Anchors:   []Anchor{{Path: "internal/graph/model.go", Line: 120}, {Path: consumerPath(consumer), Line: line}}, Notes: notes})
	}
	return items
}

func consumerPath(consumer string) string {
	switch {
	case strings.Contains(consumer, "failure"):
		return "internal/query/failureflow.go"
	case strings.Contains(consumer, "message"), strings.Contains(consumer, "codec"), strings.Contains(consumer, "transport"):
		return "internal/query/messageflow.go"
	case strings.Contains(consumer, "test"):
		return "internal/query/testcoverage.go"
	case strings.Contains(consumer, "Godot composition"):
		return "internal/query/godot.go"
	case strings.Contains(consumer, "Godot interactions"):
		return "internal/query/godotinteractions.go"
	case strings.Contains(consumer, "catalog"):
		return "internal/query/catalog.go"
	case strings.Contains(consumer, "impact"), strings.Contains(consumer, "propagation"):
		return "internal/query/impact.go"
	default:
		return "internal/query/service.go"
	}
}

func propertyEvidence() []Evidence {
	type item struct {
		name, notes string
		caps        []Capability
		path        string
		line        int
	}
	values := []item{
		{"node.signature/type/receiver", "selector display and semantic candidate context", []Capability{CapabilitySemanticReuse, CapabilityStructural}, "internal/parser/golang/golang.go", 260},
		{"node.method/route", "canonical HTTP endpoint identity", []Capability{CapabilityCatalogTopology}, "internal/query/topology.go", 503},
		{"node.protocol_package/protocol_message/protocol_field/oneof", "canonical message and field identity", []Capability{CapabilityMessageFieldFlow}, "internal/query/messageflow.go", 151},
		{"node.generated_binding/generator/canonical", "generated projection authority", []Capability{CapabilityMessageFieldFlow}, "internal/parser/protobufbinding/binding.go", 732},
		{"node.test_framework/test_subtype/test_package/test_role", "static test declaration semantics", []Capability{CapabilityTestCoverage}, "internal/query/testcoverage.go", 55},
		{"node.root/branch", "repository and component ownership", []Capability{CapabilityCatalogTopology}, "internal/indexer/components.go", 15},
		{"node.declaration/object_kind", "domain-specific type and SQL resource meaning", []Capability{CapabilityCatalogTopology, CapabilityMessageFieldFlow}, "internal/parser/protobuf/protobuf.go", 184},
		{"edge.form", "operation form for failure, Godot, codec, and propagation evidence", []Capability{CapabilityFailureFlow, CapabilityGodotInteraction, CapabilityMessageFieldFlow}, "internal/query/failureflow.go", 132},
		{"edge.argument/parameter/result", "call propagation position", []Capability{CapabilityImpactDataflow, CapabilityMessageFieldFlow}, "internal/parser/python/python.go", 463},
		{"edge.protocol/field_path/payload_field", "field and payload provenance", []Capability{CapabilityMessageFieldFlow, CapabilityTransportFlow}, "internal/query/messageflow.go", 267},
		{"edge.method/route", "HTTP request and endpoint evidence", []Capability{CapabilityCatalogTopology}, "internal/query/topology.go", 748},
		{"edge.order/pattern/framework", "effective handler and middleware chain", []Capability{CapabilityCatalogTopology}, "internal/query/topology.go", 594},
		{"edge.role/streaming", "Protobuf field, RPC, and oneof role", []Capability{CapabilityMessageFieldFlow}, "internal/parser/protobuf/protobuf.go", 293},
		{"edge.projection/accessor", "generated binding projection form", []Capability{CapabilityMessageFieldFlow}, "internal/parser/protobufbinding/parser.go", 64},
		{"edge.specifier/type_only/binding_kind/exported/reexport", "module resolution semantics", []Capability{CapabilityStructural}, "internal/parser/typescript/typescript.go", 310},
		{"edge.resolution/federated", "resolution authority and cross-repository attribution", []Capability{CapabilityStructural, CapabilityCatalogTopology}, "internal/federation/repository.go", 820},
	}
	result := make([]Evidence, 0, len(values))
	for _, value := range values {
		result = append(result, Evidence{Name: value.name, Role: RoleRequired, Capabilities: value.caps,
			Producers: []string{"parser or indexer named by the persisted fact"}, Consumers: []string{"property-aware query or reconciliation"},
			Anchors: []Anchor{{Path: value.path, Line: value.line}}, Notes: value.notes})
	}
	return result
}

func producerEvidence() []Evidence {
	values := []struct {
		name, path string
		line       int
	}{
		{"config", "internal/parser/config/config.go", 23}, {"gdscript", "internal/parser/gdscript/gdscript.go", 33},
		{"go", "internal/parser/golang/golang.go", 53}, {"godot", "internal/parser/godot/godot.go", 25},
		{"java", "internal/parser/java/java.go", 21}, {"manifest", "internal/parser/manifest/manifest.go", 22},
		{"markdown", "internal/parser/markdown/markdown.go", 32}, {"protobuf", "internal/parser/protobuf/protobuf.go", 24},
		{"protobufbinding", "internal/parser/protobufbinding/parser.go", 21}, {"python", "internal/parser/python/python.go", 20},
		{"sql", "internal/parser/sql/router.go", 76}, {"swift", "internal/parser/swift/swift.go", 21},
		{"typescript", "internal/parser/typescript/typescript.go", 40}, {"indexer", "internal/indexer/service.go", 465},
	}
	result := make([]Evidence, 0, len(values))
	for _, value := range values {
		result = append(result, Evidence{Name: value.name, Role: RoleRequired, Capabilities: append([]Capability(nil), capabilities...),
			Producers: []string{value.name}, Consumers: []string{"reconciliation and producer-attributed benchmark counts"},
			Anchors: []Anchor{{Path: value.path, Line: value.line}}})
	}
	return result
}

func operationEvidence() []Operation {
	adapters := []string{"cli", "mcp"}
	values := []struct {
		name string
		caps []Capability
		path string
		line int
	}{
		{"find", []Capability{CapabilityStructural}, "internal/query/service.go", 110},
		{"show", []Capability{CapabilityStructural}, "internal/query/service.go", 124},
		{"source", []Capability{CapabilityStructural, CapabilitySourceLookup}, "internal/source/service.go", 63},
		{"neighbors", []Capability{CapabilityStructural}, "internal/query/service.go", 323},
		{"callers", []Capability{CapabilityStructural}, "internal/query/service.go", 323},
		{"callees", []Capability{CapabilityStructural}, "internal/query/service.go", 323},
		{"path", []Capability{CapabilityStructural}, "internal/query/service.go", 398},
		{"impact", []Capability{CapabilityImpactDataflow}, "internal/query/impact.go", 193},
		{"failure-flow", []Capability{CapabilityFailureFlow}, "internal/query/failureflow.go", 57},
		{"godot-composition", []Capability{CapabilityGodotComposition}, "internal/query/godot.go", 69},
		{"godot-interactions", []Capability{CapabilityGodotInteraction}, "internal/query/godotinteractions.go", 130},
		{"search", []Capability{CapabilitySourceLookup}, "internal/search/service.go", 170},
		{"data-resources", []Capability{CapabilityCatalogTopology}, "internal/query/catalog.go", 194},
		{"data-usage", []Capability{CapabilityCatalogTopology}, "internal/query/catalog.go", 243},
		{"config-keys", []Capability{CapabilityCatalogTopology}, "internal/query/catalog.go", 283},
		{"events", []Capability{CapabilityCatalogTopology}, "internal/query/catalog.go", 319},
		{"orphaned-events", []Capability{CapabilityCatalogTopology}, "internal/query/catalog.go", 356},
		{"endpoints", []Capability{CapabilityCatalogTopology}, "internal/query/topology.go", 503},
		{"outbound-requests", []Capability{CapabilityCatalogTopology}, "internal/query/topology.go", 947},
		{"find-handler", []Capability{CapabilityCatalogTopology}, "internal/query/topology.go", 594},
		{"service-topology", []Capability{CapabilityCatalogTopology}, "internal/query/topology.go", 1262},
		{"message-flow", []Capability{CapabilityMessageFieldFlow, CapabilityTransportFlow}, "internal/query/messageflow.go", 155},
		{"message-coverage", []Capability{CapabilityMessageFieldFlow, CapabilityTransportFlow}, "internal/query/messageflow.go", 358},
		{"find-tests", []Capability{CapabilityTestCoverage}, "internal/query/testcoverage.go", 87},
		{"test-coverage", []Capability{CapabilityTestCoverage}, "internal/query/testcoverage.go", 74},
		{"semantic-reuse", []Capability{CapabilitySemanticReuse}, "internal/semantic/search.go", 156},
	}
	result := make([]Operation, 0, len(values))
	for _, value := range values {
		result = append(result, Operation{Name: value.name, Capabilities: value.caps, Adapters: adapters, Anchor: Anchor{Path: value.path, Line: value.line}})
	}
	return result
}

func uniqueCaps(values []Capability) []Capability {
	seen := map[Capability]bool{}
	result := make([]Capability, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

// AnchorString is convenient when emitting the matrix as a compact table.
func AnchorString(anchor Anchor) string { return anchor.Path + ":" + strconv.Itoa(anchor.Line) }

func sortedCapabilities(set CapabilitySet) []Capability {
	result := make([]Capability, 0, len(set))
	for capability := range set {
		result = append(result, capability)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
