package identity

import (
	"bytes"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
)

func TestEncodeDecodeRoundTripsEveryShape(t *testing.T) {
	for _, testCase := range []struct {
		name string
		id   string
	}{
		{name: "empty is the no-target sentinel", id: ""},
		{name: "node", id: graph.NodeID(graph.KindFunction, "pkg.Thing")},
		{name: "fact", id: graph.FactID("a.go", "n:0123456789abcdef0123", graph.EdgeCalls, "t", 1, 0)},
		{name: "edge", id: graph.EdgeID("f:0123456789abcdef0123", "n:0123456789abcdef0123")},
		// The repository node's prefix is four characters, so nothing may assume
		// a one-letter prefix.
		{name: "repository", id: graph.StableID("repo", "/src/project")},
		// Fixtures use identities that are not StableID output at all.
		{name: "bare word", id: "a"},
		{name: "hyphenated", id: "a-b"},
		{name: "digest that is not hex", id: "n:zzzzzzzzzzzzzzzzzzzz"},
		{name: "ten characters that are not hex", id: "x:abcdefghij"},
		{name: "two separators", id: "a:b:c"},
		{name: "separator only", id: ":"},
		{name: "trailing separator", id: "n:"},
		{name: "unicode", id: "ünïcode"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Decode(Encode(testCase.id)); got != testCase.id {
				t.Fatalf("round trip of %q produced %q", testCase.id, got)
			}
		})
	}
}

func TestEncodeShrinksAStableIdentity(t *testing.T) {
	id := graph.NodeID(graph.KindFunction, "pkg.Thing")
	if len(id) != 22 {
		t.Fatalf("a node identity is %d characters, so this test's premise is stale", len(id))
	}
	// One tag, one prefix letter, one separator, ten digest bytes.
	if encoded := Encode(id); len(encoded) != 13 {
		t.Fatalf("encoded %q to %d bytes, want 13", id, len(encoded))
	}
}

func TestEncodeKeepsTheEmptyIdentityEmpty(t *testing.T) {
	// A fact with no target stores the empty identity, and reconciliation tells
	// that apart from an unresolved one. An encoding that gave it a tag byte
	// would make it sort after present identities and stop being empty.
	if encoded := Encode(""); len(encoded) != 0 {
		t.Fatalf("the empty identity encoded to %v, want no bytes", encoded)
	}
}

func TestCompactIdentitiesKeepTheirTextOrdering(t *testing.T) {
	// Identity columns are ORDER BY tiebreaks and a pagination cursor, so the
	// stored bytes have to sort the way the text did for every identity a real
	// index holds.
	ordered := []string{
		"",
		"f:00000000000000000000",
		"f:0123456789abcdef0123",
		"f:ffffffffffffffffffff",
		"n:00000000000000000000",
		"n:fedcba98765432100000",
		"repo:0123456789abcdef0123",
	}
	for index := 1; index < len(ordered); index++ {
		previous, current := Encode(ordered[index-1]), Encode(ordered[index])
		if bytes.Compare(previous, current) >= 0 {
			t.Errorf("%q encodes to bytes that do not sort before %q",
				ordered[index-1], ordered[index])
		}
	}
}

func TestKeyScansTextAndBlob(t *testing.T) {
	id := graph.NodeID(graph.KindFunction, "pkg.Thing")
	var fromBlob, fromText, fromNil Key
	if err := fromBlob.Scan(Encode(id)); err != nil {
		t.Fatal(err)
	}
	if string(fromBlob) != id {
		t.Errorf("scanning a blob gave %q, want %q", fromBlob, id)
	}
	// A pre-migration index still holds text in the column, and SQLite reports a
	// value's own storage class rather than the column's affinity.
	if err := fromText.Scan(id); err != nil {
		t.Fatal(err)
	}
	if string(fromText) != id {
		t.Errorf("scanning text gave %q, want %q", fromText, id)
	}
	if err := fromNil.Scan(nil); err != nil {
		t.Fatal(err)
	}
	if fromNil != "" {
		t.Errorf("scanning NULL gave %q, want the empty identity", fromNil)
	}
	if err := fromNil.Scan(42); err == nil {
		t.Error("scanning an integer was accepted")
	}
}

func TestKeyValueEncodes(t *testing.T) {
	id := graph.NodeID(graph.KindFunction, "pkg.Thing")
	value, err := Key(id).Value()
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := value.([]byte)
	if !ok {
		t.Fatalf("Value returned %T, want a blob", value)
	}
	if !bytes.Equal(raw, Encode(id)) {
		t.Errorf("Value gave %v, want %v", raw, Encode(id))
	}
}
