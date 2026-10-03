package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// evidenceDigestVersion tags the digest definition. A stored digest carrying a
// different tag describes evidence under rules this build does not share, so it
// can only be treated as a mismatch. Bump it whenever the fields below change.
const evidenceDigestVersion = "evidence-v1"

// EvidenceDigest fingerprints the evidence one file contributes to the graph:
// its nodes and facts, which are exactly what persistence writes for it.
// Diagnostics are deliberately absent because they are reported rather than
// stored.
//
// Two properties make it safe to decide a write on:
//
// Order insensitivity. Each row is digested on its own and the row digests are
// sorted before being combined, so a parser that emits the same rows in a
// different order digests the same. A parser whose emission order is unstable
// therefore disables elision rather than producing a digest that changes from
// run to run, which would be a stale graph.
//
// Field completeness. Every field persistence writes is covered. A field left
// out would make two different graphs digest alike, and the write that carried
// the difference could then be skipped. The field-count assertions in the tests
// are what keep a newly added field from bypassing this quietly.
func EvidenceDigest(parsed ParseResult) string {
	rows := make([]string, 0, len(parsed.Nodes)+len(parsed.Facts))
	for _, node := range parsed.Nodes {
		rows = append(rows, nodeEvidenceDigest(node))
	}
	for _, fact := range parsed.Facts {
		rows = append(rows, factEvidenceDigest(fact))
	}
	sort.Strings(rows)
	combined := sha256.New()
	writeEvidenceField(combined, evidenceDigestVersion)
	writeEvidenceField(combined, strconv.Itoa(len(parsed.Nodes)))
	writeEvidenceField(combined, strconv.Itoa(len(parsed.Facts)))
	for _, row := range rows {
		writeEvidenceField(combined, row)
	}
	return evidenceDigestVersion + ":" + hex.EncodeToString(combined.Sum(nil))[:32]
}

func nodeEvidenceDigest(node Node) string {
	digest := sha256.New()
	writeEvidenceField(digest, "node")
	writeEvidenceField(digest, node.ID)
	writeEvidenceField(digest, string(node.Kind))
	writeEvidenceField(digest, node.Name)
	writeEvidenceField(digest, node.QualifiedName)
	writeEvidenceField(digest, node.Language)
	writeEvidenceLocation(digest, node.Location)
	writeEvidenceProperties(digest, node.Properties)
	writeEvidenceField(digest, node.OwnerFile)
	writeEvidenceField(digest, strconv.FormatBool(node.External))
	return hex.EncodeToString(digest.Sum(nil))
}

func factEvidenceDigest(fact Fact) string {
	digest := sha256.New()
	writeEvidenceField(digest, "fact")
	writeEvidenceField(digest, fact.ID)
	writeEvidenceField(digest, fact.FromID)
	writeEvidenceField(digest, fact.Source)
	writeEvidenceField(digest, string(fact.SourceKind))
	writeEvidenceField(digest, string(fact.Kind))
	writeEvidenceField(digest, fact.Producer)
	writeEvidenceField(digest, fact.TargetID)
	writeEvidenceField(digest, fact.Target)
	writeEvidenceField(digest, string(fact.TargetKind))
	writeEvidenceLocation(digest, fact.Location)
	writeEvidenceProperties(digest, fact.Properties)
	writeEvidenceField(digest, fact.OwnerFile)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeEvidenceLocation(digest interface{ Write([]byte) (int, error) }, location Location) {
	writeEvidenceField(digest, location.Path)
	writeEvidenceField(digest, strconv.Itoa(location.Line))
	writeEvidenceField(digest, strconv.Itoa(location.Column))
	writeEvidenceField(digest, strconv.Itoa(location.EndLine))
}

// writeEvidenceProperties digests a property map by sorted key, so a map's
// iteration order cannot change the result.
func writeEvidenceProperties(digest interface{ Write([]byte) (int, error) }, properties map[string]string) {
	writeEvidenceField(digest, strconv.Itoa(len(properties)))
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		writeEvidenceField(digest, key)
		writeEvidenceField(digest, properties[key])
	}
}

// writeEvidenceField appends a length-prefixed field, so no combination of
// values can be rearranged into the same byte stream.
func writeEvidenceField(digest interface{ Write([]byte) (int, error) }, value string) {
	_, _ = digest.Write([]byte(strconv.Itoa(len(value))))
	_, _ = digest.Write([]byte{':'})
	_, _ = digest.Write([]byte(value))
	_, _ = digest.Write([]byte{0})
}
