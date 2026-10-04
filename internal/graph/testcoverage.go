package graph

// IsTestSupportNode reports whether a declaration belongs to test support code
// rather than production code. Parsers set test_role on helpers and lifecycle
// hooks; storage and query use the same predicate so direct and derived
// structural coverage cannot disagree about what is production.
func IsTestSupportNode(node Node) bool {
	if node.Kind == KindTest {
		return true
	}
	switch node.Properties["test_role"] {
	case "helper", "lifecycle", "scope":
		return true
	default:
		return false
	}
}

// DirectTestEdge derives a persisted direct tests relation from one resolved
// call/reference edge. External, ambiguous, helper, lifecycle, and test targets
// are excluded: the original evidence edge remains the authority and unresolved
// evidence remains visible there without becoming coverage.
func DirectTestEdge(source, target Node, evidence Edge) (Edge, bool) {
	if source.External || source.Kind != KindTest || target.External || IsTestSupportNode(target) {
		return Edge{}, false
	}
	if evidence.Kind != EdgeCalls && evidence.Kind != EdgeReferences {
		return Edge{}, false
	}
	properties := make(map[string]string, len(evidence.Properties)+3)
	for key, value := range evidence.Properties {
		properties[key] = value
	}
	properties["coverage"] = "structural"
	properties["evidence_relation"] = string(evidence.Kind)
	properties["evidence_edge_id"] = evidence.ID
	return Edge{
		ID: DerivedEdgeID(evidence.FactID, target.ID, EdgeTests), FactID: evidence.FactID,
		FromID: source.ID, ToID: target.ID, Kind: EdgeTests, Producer: evidence.Producer,
		Location: evidence.Location, Properties: properties,
	}, true
}
