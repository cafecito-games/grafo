package graph

import "testing"

// TestDerivedEdgeIDDistinguishesOnlyTheTestRelation pins the rule storage now
// depends on. Since migration 13 an edge's identity is not stored, so this
// function and the unique key (fact_id, to_id, kind) have to agree: every kind
// but EdgeTests derives from the fact and the target alone, and EdgeTests adds
// its own kind because DirectTestEdge builds it from a calls or references fact
// that already owns that pair.
func TestDerivedEdgeIDDistinguishesOnlyTheTestRelation(t *testing.T) {
	const factID, toID = "f:fact", "n:target"
	plain := DerivedEdgeID(factID, toID, EdgeCalls)

	for _, kind := range []EdgeKind{EdgeCalls, EdgeReferences, EdgeReads, EdgeDeclares} {
		if got := DerivedEdgeID(factID, toID, kind); got != plain {
			t.Errorf("DerivedEdgeID for %q = %q, want the fact-and-target identity %q", kind, got, plain)
		}
	}
	if got := DerivedEdgeID(factID, toID, EdgeTests); got == plain {
		t.Fatalf("a structural test edge shares the identity %q of the edge it was derived from", got)
	}
	// EdgeID is what writers call when they know they are not building a test
	// edge, so the two have to produce the same bytes for every other kind.
	if got, want := DerivedEdgeID(factID, toID, EdgeCalls), EdgeID(factID, toID); got != want {
		t.Errorf("DerivedEdgeID = %q but EdgeID = %q", got, want)
	}
}

// TestDirectTestEdgeIdentityIsDerivable is the pairing that matters: the writer
// and the reader have to arrive at the same identity, because the reader now
// computes it from columns rather than reading what the writer stored.
func TestDirectTestEdgeIdentityIsDerivable(t *testing.T) {
	source := Node{ID: "n:test", Kind: KindTest, Name: "TestThing", QualifiedName: "pkg.TestThing"}
	target := Node{ID: "n:target", Kind: KindFunction, Name: "Thing", QualifiedName: "pkg.Thing"}
	evidence := Edge{ID: EdgeID("f:fact", target.ID), FactID: "f:fact", FromID: source.ID,
		ToID: target.ID, Kind: EdgeCalls}

	derived, ok := DirectTestEdge(source, target, evidence)
	if !ok {
		t.Fatal("DirectTestEdge declined evidence it should have accepted")
	}
	if want := DerivedEdgeID(derived.FactID, derived.ToID, derived.Kind); derived.ID != want {
		t.Fatalf("the written identity %q is not what a reader derives from its own columns, %q",
			derived.ID, want)
	}
	// And it differs from the evidence edge, which shares both other columns.
	if derived.ID == evidence.ID {
		t.Fatal("the test edge and its evidence edge share an identity")
	}
	if derived.FactID != evidence.FactID || derived.ToID != evidence.ToID {
		t.Fatal("the fixture no longer shares a fact and a target, so it proves nothing")
	}
}
