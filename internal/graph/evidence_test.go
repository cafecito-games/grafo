package graph

import (
	"reflect"
	"strings"
	"testing"
)

func sampleNode() Node {
	return Node{
		ID: "node:abc", Kind: NodeKind("function"), Name: "Run", QualifiedName: "app.Run",
		Language: "go", Location: Location{Path: "app/app.go", Line: 3, Column: 1, EndLine: 5},
		Properties: map[string]string{"receiver": "App", "exported": "true"},
		OwnerFile:  "app/app.go", External: false,
	}
}

func sampleFact() Fact {
	return Fact{
		ID: "fact:abc", FromID: "node:abc", Source: "", SourceKind: NodeKind(""),
		Kind: EdgeKind("calls"), Producer: "go", TargetID: "node:def",
		Target: "store.Get", TargetKind: NodeKind("method"),
		Location:   Location{Path: "app/app.go", Line: 4, Column: 2, EndLine: 4},
		Properties: map[string]string{"arity": "1"},
		OwnerFile:  "app/app.go",
	}
}

// TestEvidenceDigestCoversEveryPersistedField is the guard that keeps the
// digest honest. A field persistence writes but the digest ignores makes two
// different graphs digest alike, and the write carrying the difference is then
// skipped — a stale graph, not a slow one.
//
// The field counts are asserted deliberately: a field added to Node or Fact
// fails here until it is either digested or consciously excluded.
func TestEvidenceDigestCoversEveryPersistedField(t *testing.T) {
	const nodeFields, factFields = 9, 12
	if actual := reflect.TypeOf(Node{}).NumField(); actual != nodeFields {
		t.Fatalf("Node has %d fields, this test knows %d; digest the new field in "+
			"nodeEvidenceDigest and add a case below", actual, nodeFields)
	}
	if actual := reflect.TypeOf(Fact{}).NumField(); actual != factFields {
		t.Fatalf("Fact has %d fields, this test knows %d; digest the new field in "+
			"factEvidenceDigest and add a case below", actual, factFields)
	}

	baseline := EvidenceDigest(ParseResult{Nodes: []Node{sampleNode()}, Facts: []Fact{sampleFact()}})

	nodeCases := map[string]func(*Node){
		"ID":            func(n *Node) { n.ID = "node:other" },
		"Kind":          func(n *Node) { n.Kind = NodeKind("method") },
		"Name":          func(n *Node) { n.Name = "Walk" },
		"QualifiedName": func(n *Node) { n.QualifiedName = "app.Walk" },
		"Language":      func(n *Node) { n.Language = "java" },
		"Location":      func(n *Node) { n.Location.Line = 99 },
		"Location.End":  func(n *Node) { n.Location.EndLine = 99 },
		"Properties":    func(n *Node) { n.Properties["receiver"] = "Other" },
		"PropertyKey":   func(n *Node) { n.Properties["added"] = "x" },
		"OwnerFile":     func(n *Node) { n.OwnerFile = "app/other.go" },
		"External":      func(n *Node) { n.External = true },
	}
	for name, mutate := range nodeCases {
		node := sampleNode()
		node.Properties = map[string]string{"receiver": "App", "exported": "true"}
		mutate(&node)
		if EvidenceDigest(ParseResult{Nodes: []Node{node}, Facts: []Fact{sampleFact()}}) == baseline {
			t.Errorf("changing node %s did not change the digest", name)
		}
	}

	factCases := map[string]func(*Fact){
		"ID":         func(f *Fact) { f.ID = "fact:other" },
		"FromID":     func(f *Fact) { f.FromID = "node:other" },
		"Source":     func(f *Fact) { f.FromID = ""; f.Source = "named" },
		"SourceKind": func(f *Fact) { f.FromID = ""; f.Source = "named"; f.SourceKind = NodeKind("function") },
		"Kind":       func(f *Fact) { f.Kind = EdgeKind("declares") },
		"Producer":   func(f *Fact) { f.Producer = "java" },
		"TargetID":   func(f *Fact) { f.TargetID = "node:zzz" },
		"Target":     func(f *Fact) { f.Target = "store.Put" },
		"TargetKind": func(f *Fact) { f.TargetKind = NodeKind("function") },
		"Location":   func(f *Fact) { f.Location.Column = 42 },
		"Properties": func(f *Fact) { f.Properties["arity"] = "2" },
		"OwnerFile":  func(f *Fact) { f.OwnerFile = "app/other.go" },
	}
	for name, mutate := range factCases {
		fact := sampleFact()
		fact.Properties = map[string]string{"arity": "1"}
		mutate(&fact)
		if EvidenceDigest(ParseResult{Nodes: []Node{sampleNode()}, Facts: []Fact{fact}}) == baseline {
			t.Errorf("changing fact %s did not change the digest", name)
		}
	}
}

// TestEvidenceDigestIgnoresEmissionOrder is what makes the digest safe against
// a parser that reorders its output: the same rows must digest the same, or an
// unchanged file would be rewritten forever.
func TestEvidenceDigestIgnoresEmissionOrder(t *testing.T) {
	first := sampleNode()
	second := sampleNode()
	second.ID, second.Name, second.QualifiedName = "node:def", "Walk", "app.Walk"
	factOne := sampleFact()
	factTwo := sampleFact()
	factTwo.ID, factTwo.Kind = "fact:def", EdgeKind("declares")

	forward := EvidenceDigest(ParseResult{Nodes: []Node{first, second}, Facts: []Fact{factOne, factTwo}})
	reversed := EvidenceDigest(ParseResult{Nodes: []Node{second, first}, Facts: []Fact{factTwo, factOne}})
	if forward != reversed {
		t.Fatalf("digest depends on emission order: %q then %q", forward, reversed)
	}
}

// TestEvidenceDigestSeparatesRowCounts covers the case a plain set digest would
// miss: the same row repeated is not the same evidence as the row once, because
// persistence writes both.
func TestEvidenceDigestSeparatesRowCounts(t *testing.T) {
	once := EvidenceDigest(ParseResult{Nodes: []Node{sampleNode()}})
	twice := EvidenceDigest(ParseResult{Nodes: []Node{sampleNode(), sampleNode()}})
	if once == twice {
		t.Fatal("a repeated row digests as a single row")
	}
}

// TestEvidenceDigestIgnoresDiagnostics pins assumption 2 of the issue:
// diagnostics are reported, never persisted, so they must not take part in a
// decision about whether persisted rows changed.
func TestEvidenceDigestIgnoresDiagnostics(t *testing.T) {
	without := EvidenceDigest(ParseResult{Nodes: []Node{sampleNode()}})
	with := EvidenceDigest(ParseResult{
		Nodes:       []Node{sampleNode()},
		Diagnostics: []Diagnostic{{Path: "app/app.go", Level: "warning", Message: "unresolved"}},
	})
	if without != with {
		t.Fatal("a diagnostic changed the evidence digest, so an unchanged file would be rewritten")
	}
}

// TestEvidenceDigestCarriesItsVersion lets a stored digest be recognised as
// belonging to another definition without decoding it.
func TestEvidenceDigestCarriesItsVersion(t *testing.T) {
	digest := EvidenceDigest(ParseResult{Nodes: []Node{sampleNode()}})
	if !strings.HasPrefix(digest, evidenceDigestVersion+":") {
		t.Fatalf("digest %q does not carry its version tag", digest)
	}
}

// TestEmptyEvidenceDigestsStably matters because a file can legitimately
// contribute nothing, and that has to be distinguishable from an absent digest
// rather than colliding with it.
func TestEmptyEvidenceDigestsStably(t *testing.T) {
	first := EvidenceDigest(ParseResult{})
	second := EvidenceDigest(ParseResult{Nodes: []Node{}, Facts: []Fact{}})
	if first != second {
		t.Fatalf("empty evidence digested two ways: %q and %q", first, second)
	}
	if first == "" {
		t.Fatal("empty evidence produced an empty digest, which cannot be told from no digest at all")
	}
}
